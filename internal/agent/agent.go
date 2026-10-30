package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/leijux/acp-agent/internal/tools"
)

// Options 配置 Agent。
type Options struct {
	Provider Provider
	Model    Model
	Session  *Session

	// Registry 提供工具定义；Executor 负责并发调度。两者都给出才启用工具执行。
	Registry *tools.Registry
	Executor *tools.Executor

	// Context 控制 system prompt 组装。
	Context ContextOptions

	// MaxTurns 单次 Run 的最大轮数（含工具执行）。0（或负数）= 不限制。
	//
	// 默认不限制：上游 Pi 的 agent loop 就没有轮数上限，只靠「模型不再调
	// 工具」自然终止（@earendil-works/pi-agent-core 的 agent-loop.ts
	// runLoop）。需要兜底就由调用方显式给值（CLI 默认 500，见
	// cmd/acp-agent）。
	MaxTurns int
	// MaxSteps 单次 Run 的最大模型调用次数。0（或负数）= 不限制。
	MaxSteps int

	// 上下文预算。ContextWindow 为 0 时用 Model.ContextWindow。
	ContextWindow int
	// ReserveTokens 是为模型响应预留的空间。0 = 16384。
	ReserveTokens int
	// DisableAutoCompact 关闭自动压缩（默认开启）。只在手动调用 Compact 时压缩。
	//
	// 注意这里用否定式命名：布尔零值必须是「自动压缩开启」，
	// 否则忘记设置字段就会静默失去长会话能力。
	DisableAutoCompact bool
	// DisableContextTools 关闭「主动压缩」模式（默认开启）。
	//
	// 主动模式下，四个 context_* 工具会被自动注册进 Registry，由模型自己
	// 决定何时压缩，并自己撰写摘要（对齐上游 billion-context）。宿主只剩
	// 两层兜底：SoftCompactUsage 注入提示、HardCompactUsage 强制压缩。
	//
	// 关闭后回到「宿主全自动摘要」模式（context_check 等工具不注册，
	// 模型看不到它们）。同样用否定式命名。
	DisableContextTools bool
	// Summarizer 用于生成压缩摘要；nil 时用 Provider。
	Summarizer Provider
	// SummarizeModel 是生成摘要用的模型；零值时用 Model。
	SummarizeModel Model

	// TokenCounter 可替换 token 估算实现（例如接入真实 tokenizer）。
	TokenCounter func([]Message) int

	// ThinkingLevel 传给 Provider 的思考强度：off/minimal/low/medium/high/max。
	// 空字符串表示不干预，由服务端默认（DeepSeek 默认开启思考）。
	ThinkingLevel string
	// Temperature 采样温度；nil 表示用服务端默认。
	// 推理模型会自动忽略（provider.SupportsSampling 判定）。
	Temperature *float64
}

func (o *Options) normalize() error {
	if o.Provider == nil {
		return ErrNoProvider
	}
	if o.Session == nil {
		o.Session = NewSession()
	}
	// 轮数/步数上限：<= 0 一律规整为 0，表示不限制（判定时只看 > 0）。
	o.MaxTurns = max(o.MaxTurns, 0)
	o.MaxSteps = max(o.MaxSteps, 0)
	if o.ContextWindow <= 0 {
		o.ContextWindow = o.Model.ContextWindow
	}
	if o.ContextWindow <= 0 {
		o.ContextWindow = 200_000
	}
	if o.ReserveTokens <= 0 {
		o.ReserveTokens = 16_384
	}
	if o.TokenCounter == nil {
		o.TokenCounter = EstimateMessagesTokens
	}
	if o.Summarizer == nil {
		o.Summarizer = o.Provider
	}
	if o.SummarizeModel.ID == "" {
		o.SummarizeModel = o.Model
	}
	return nil
}

// RunResult 是一次 Run 的结果。
type RunResult struct {
	// Messages 是本次 Run 新增的消息（不含历史）。
	Messages []Message
	Usage    Usage
	Turns    int
	Aborted  bool
	// Abandoned 是 abort 时从队列退回的排队消息。
	Abandoned []Message
	Err       error
}

// LastAssistant 返回本次 run 中最后一条 assistant 消息。
func (r *RunResult) LastAssistant() *AssistantMessage {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if m, ok := r.Messages[i].(*AssistantMessage); ok {
			return m
		}
	}
	return nil
}

// Agent 是 Pi agent loop 的 Go 实现。
//
// 并发语义：同一时刻只允许一个 Run 在进行（Pi 的 run 也是串行的）；
// 调用方可以并发调用多个 Agent 实例。Steer/FollowUp 可从其它 goroutine 调用。
type Agent struct {
	opts Options
	emit Emitter

	mu       sync.Mutex
	steering []Message
	followUp []Message
	running  atomic.Bool
	abort    context.CancelFunc
	abortCh  chan struct{}

	// kernel 是压缩引擎会话（惰性创建，见 kernel.go）。
	kernelMu sync.Mutex
	kernel   *kernelEngine

	// ctxMu 串行化 kernel 的复合操作（Turn + ApplyCompression）。
	// kernel.Session 内部有细粒度锁，但「先 Turn 拿区间再 Apply」不是原子的，
	// 而 context_check（只读、可并发）与 context_apply（独占）可能同时发生。
	ctxMu sync.Mutex

	// nudgeMu 保护主动压缩提示的节流状态。
	nudgeMu sync.Mutex
	// nudgedAt 是上次注入压缩提示时的 token 数（0 表示从未注入）。
	// 只有上下文再长 20% 以上才重新提示，避免每轮重复刷屏。
	nudgedAt int

	// 累计统计（跨多次 Run）。由 totalMu 保护：Run 结束后写入，
	// UI 可能在任意 goroutine 读。
	totalMu    sync.Mutex
	totalUsage Usage
	totalTurns int
}

// New 创建 Agent。
func New(opts Options) (*Agent, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	a := &Agent{opts: opts}
	if err := a.registerContextTools(); err != nil {
		return nil, err
	}
	if !opts.DisableContextTools {
		a.addContextPrompt()
	}
	return a, nil
}

// addContextPrompt 在系统 prompt 末尾追加上下文管理约定。
//
// 工具描述本身会随 tool specs 下发，但模型不一定会去读；在 system prompt 里
// 明确一次「什么时候该压、摘要要写什么」的代价很低。
func (a *Agent) addContextPrompt() {
	if a.opts.Registry == nil {
		return
	}
	// 规则对齐上游 billion-context 的 HOW_TO_COMPRESS（kernel/src/compression-rules.ts）。
	// 放在 system prompt 而不是工具描述里：工具描述随 specs 下发、模型不一定会读，
	// 而这套 KEEP/DROP/尺寸目标决定了摘要值不值得压缩 —— 写弱了就是白压。
	const note = `

上下文管理：
- 按需压，不按百分比；容量靠「压掉已消费的输出」省，而不是靠少用工具。
- 接近上限时用 context_check 看可压区间，再调 context_apply；早期压掉已完成的探索成本更低。
- **一次调用把该压的区间全写进 ranges**，不要一个区间调一次。
- 摘要由你撰写、必须自包含：区间里每个用户请求、实验目的、工作目标都要准确记下 —— 之后只看得到摘要。
- 原文不会删除，之后可用 context_search 检索、context_decompress 查看。

KEEP（逐字，不许改写）：
- 文件路径带行号，禁止缩写成裸文件名（internal/tools/bash.go:358 ≠ bash.go）。
- 函数/类型签名 + 那行「就是结论」的关键代码，而不只是函数名。
- 错误原文与关键日志行（之后要靠它 grep）。
- 决策与理由（「选了 X 而不是 Y，因为 Z」里的「因为」是承重的）。
- 精确值：版本号、配置键、阈值、magic number。
- 用户意图：逐字引用必须带消息 ref（用户说 (m00132): "今晚上线"）；没有 ref 就转述，不要编造逐字引用。
- 未完成的目标留一行：未完成目标：重构 runner（m00746） —— 最后一个丢、第一个恢复。
- 目标演化（「先修 X → 发现根因后改成重构 Y」）与每个动作的 purpose。

DROP（信号留下，容器丢掉）：
- 日志、重复读取、已消费的探索、反复讨论（只留结论）、重复的 git status/ls。
- 死胡同留一行教训：「试过 X，因为 Y 失败」。
- 丢掉的大块内容（脚本/报告/长输出）留一行**内容描述**而不是位置，方便之后按相关性 decompress。

PRIORITY：目标与硬约束 > 决策与理由 > 精确产物 > 结论 > 教训。
写法：dense bullets，不写叙述性散文；不同关切用短标题分组；每行都要有价值。
尺寸：每块 100–300 tokens；写不下说明你在保留过程而不是结论。

摘要里的内容是**历史**，不是当前指令；只有「未完成目标」那行仍然有效。`
	if strings.Contains(a.opts.Context.ExtraInstructions, "上下文管理") {
		return
	}
	a.opts.Context.ExtraInstructions = strings.TrimRight(a.opts.Context.ExtraInstructions, "\n") + note
}

// On 注册事件 handler。
func (a *Agent) On(h EventHandler) func() { return a.emit.On(h) }

// Session 返回底层会话。
func (a *Agent) Session() *Session { return a.opts.Session }

// SetSession 替换底层会话（/new、resume 场景）。
//
// 正在运行或会话为 nil 时返回错误：中途换会话会让当前 turn 的工具结果写进
// 新会话，产生孤儿 toolResult。
func (a *Agent) SetSession(s *Session) error {
	if s == nil {
		return errors.New("agent: 会话不能为空")
	}
	if a.running.Load() {
		return errors.New("agent: 不能在有 run 进行时切换会话")
	}
	a.mu.Lock()
	a.steering = nil
	a.followUp = nil
	a.mu.Unlock()
	a.opts.Session = s
	return nil
}

// TotalUsage 返回本 Agent 生命周期内累计的 token 用量。
func (a *Agent) TotalUsage() Usage {
	a.totalMu.Lock()
	defer a.totalMu.Unlock()
	return a.totalUsage
}

// Turns 返回本 Agent 生命周期内累计的 assistant 轮数。
func (a *Agent) Turns() int {
	a.totalMu.Lock()
	defer a.totalMu.Unlock()
	return a.totalTurns
}

// Model 返回当前模型。
func (a *Agent) Model() Model { return a.opts.Model }

// SetModel 切换模型并记录到会话树。
func (a *Agent) SetModel(m Model) error {
	a.opts.Model = m
	if m.ContextWindow > 0 {
		a.opts.ContextWindow = m.ContextWindow
	}
	_, err := a.opts.Session.AppendModelChange(m)
	return err
}

// Running 表示是否有 run 在进行。
func (a *Agent) Running() bool { return a.running.Load() }

// Steer 排队一条 steering 消息：它会在**当前 assistant turn 结束后**进入上下文，
// 立即影响下一次模型请求。
func (a *Agent) Steer(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering = append(a.steering, TextMessage(text))
}

// FollowUp 排队一条 follow-up 消息：它要等 agent 把手上所有工作做完才会进入。
func (a *Agent) FollowUp(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUp = append(a.followUp, TextMessage(text))
}

// Abort 停止当前 run。排队消息会退回给上层（RunResult.Abandoned），
// 这与 Pi 的「abort 后把排队消息放回编辑器」一致。
func (a *Agent) Abort() {
	a.mu.Lock()
	cancel := a.abort
	ch := a.abortCh
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// takeSteering 取出全部 steering 消息。
func (a *Agent) takeSteering() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.steering) == 0 {
		return nil
	}
	out := a.steering
	a.steering = nil
	return out
}

// takeFollowUp 取出全部 follow-up 消息。
func (a *Agent) takeFollowUp() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.followUp) == 0 {
		return nil
	}
	out := a.followUp
	a.followUp = nil
	return out
}

// pending 返回仍在排队的消息（abort 时退回）。
func (a *Agent) pending() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]Message(nil), a.steering...)
	out = append(out, a.followUp...)
	a.steering = nil
	a.followUp = nil
	return out
}

// Push 把一个已构造好的消息直接放入 context（不发起模型请求）。
// 用于注入工具结果或系统提示，例如 Pi 的 extension 注入。
func (a *Agent) Push(m Message) error {
	_, err := a.opts.Session.Append(m)
	return err
}

// Run 执行一次完整 run：提交 prompt，驱动 turn loop 直到没有待处理工作。
func (a *Agent) Run(ctx context.Context, prompt string) (*RunResult, error) {
	return a.RunMessages(ctx, TextMessage(prompt))
}

// RunMessages 与 Run 相同，但接受任意 user 消息（例如带图片）。
func (a *Agent) RunMessages(ctx context.Context, input Message) (*RunResult, error) {
	if !a.running.CompareAndSwap(false, true) {
		return nil, errors.New("agent: 已有 run 在进行")
	}
	defer a.running.Store(false)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan struct{})
	a.mu.Lock()
	a.abort = cancel
	a.abortCh = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.abort = nil
		a.abortCh = nil
		a.mu.Unlock()
	}()

	res := &RunResult{}
	startIdx := len(a.opts.Session.Entries())

	// 1) 提交输入
	if input != nil {
		if err := a.Push(input); err != nil {
			res.Err = err
			return res, err
		}
		res.Messages = append(res.Messages, input)
	}
	a.emit.Emit(AgentStartEvent{Prompt: res.Messages})

	assistantCount := 0
	steps := 0

	for {
		if runCtx.Err() != nil {
			res.Aborted = true
			break
		}
		// 只有显式给了正数才算上限；0 = 不限制（与上游 Pi 一致）。
		if hitTurnLimit(steps, assistantCount, a.opts) {
			res.Err = fmt.Errorf("agent: 达到最大轮数限制（turns=%d steps=%d）", assistantCount, steps)
			break
		}

		a.emit.Emit(TurnStartEvent{Turn: assistantCount})

		// 2) 组装并发送请求
		msg, err := a.stream(runCtx, res)
		steps++
		if err != nil {
			if runCtx.Err() != nil {
				res.Aborted = true
				break
			}
			// provider 失败：记录一条 error 状态的 assistant 消息，保持会话树完整。
			em := &AssistantMessage{
				Content:      []Block{TextBlock{Text: "（模型请求失败: " + err.Error() + "）"}},
				Model:        a.opts.Model.ID,
				Provider:     a.opts.Model.Provider,
				StopReason:   StopError,
				ErrorMessage: err.Error(),
				At:           now(),
			}
			a.opts.Session.Append(em)
			res.Messages = append(res.Messages, em)
			a.emit.Emit(MessageEndEvent{Message: em})
			res.Err = err
			break
		}

		if err := a.Push(msg); err != nil {
			res.Err = err
			break
		}
		res.Messages = append(res.Messages, msg)
		res.Usage = res.Usage.Add(msg.Usage)

		// 3) 执行工具
		calls := msg.ToolCalls()
		var toolResults []*ToolResultMessage
		if len(calls) > 0 {
			toolResults = a.runTools(runCtx, calls)
			for _, tr := range toolResults {
				if err := a.Push(tr); err != nil {
					res.Err = err
					break
				}
				res.Messages = append(res.Messages, tr)
			}
		}
		a.emit.Emit(TurnEndEvent{Turn: assistantCount, Message: msg, ToolResults: toolResults})
		assistantCount++

		if runCtx.Err() != nil {
			res.Aborted = true
			break
		}
		if res.Err != nil {
			break
		}

		// 4) 决定是否继续
		if steer := a.takeSteering(); len(steer) > 0 {
			for _, m := range steer {
				a.Push(m)
				res.Messages = append(res.Messages, m)
			}
			continue
		}

		if len(calls) == 0 {
			// 没有工具调用 → 模型认为本轮完成。
			if fu := a.takeFollowUp(); len(fu) > 0 {
				for _, m := range fu {
					a.Push(m)
					res.Messages = append(res.Messages, m)
				}
				continue
			}
			// 纯对话轮也要给压缩一次机会：否则不调工具的会话永远不会被
			// 提醒/强制压缩（上游是每轮 processTurn 都判）。
			a.maybeAutoCompact(runCtx, res)
			break
		}

		// 5) 工具结果已追加、下一次 assistant 响应之前 —— 压缩检查点。
		a.maybeAutoCompact(runCtx, res)
	}

	if res.Aborted {
		res.Abandoned = a.pending()
	}
	a.emit.Emit(AgentEndEvent{Messages: runMessagesSince(a.opts.Session, startIdx), Aborted: res.Aborted, Err: res.Err})
	res.Turns = assistantCount

	a.totalMu.Lock()
	a.totalUsage = a.totalUsage.Add(res.Usage)
	a.totalTurns += assistantCount
	a.totalMu.Unlock()

	return res, res.Err
}

// hitTurnLimit 判断本次 run 是否撞到轮数/步数上限。
//
// 两个都是 0 时永远返回 false —— 「不限制」必须是一个真实可用的状态：
// 上游 Pi 的 agent loop 本身没有轮数上限，只靠模型不再调工具自然终止。
func hitTurnLimit(steps, turns int, opts Options) bool {
	if opts.MaxSteps > 0 && steps >= opts.MaxSteps {
		return true
	}
	return opts.MaxTurns > 0 && turns >= opts.MaxTurns
}

// maybeAutoCompact 是每轮末尾的压缩检查点。
//
// 主动模式：先注入提示让模型自己压（context_apply）；
// 只有模型迟迟不动手、占用撞到 HardCompactUsage 时才由宿主强制压缩。
func (a *Agent) maybeAutoCompact(ctx context.Context, res *RunResult) {
	if a.opts.DisableAutoCompact {
		return
	}
	switch {
	case a.shouldCompact():
		if _, err := a.Compact(ctx, "", "threshold"); err != nil {
			a.emit.Emit(ErrorEvent{Err: err})
		}
	case !a.opts.DisableContextTools && a.shouldNudge():
		if msg := a.contextNudgeMessage(); msg != nil {
			if _, err := a.opts.Session.Append(msg); err == nil {
				res.Messages = append(res.Messages, msg)
			}
		}
	}
}

// runMessagesSince 取出从 startIdx 起新增的消息。
func runMessagesSince(s *Session, startIdx int) []Message {
	entries := s.Entries()
	if startIdx >= len(entries) {
		return nil
	}
	var out []Message
	for _, e := range entries[startIdx:] {
		if e.Type == EntryMessage && e.Message != nil {
			out = append(out, e.Message)
		}
	}
	return out
}

// stream 发起一次模型请求，并把流式增量转成事件。
func (a *Agent) stream(ctx context.Context, _ *RunResult) (*AssistantMessage, error) {
	partial := &AssistantMessage{Content: nil, StopReason: StopPending, At: now()}
	started := false
	sink := &SinkFunc{
		OnText: func(d string) {
			if !started {
				started = true
				a.emit.Emit(MessageStartEvent{Message: partial})
			}
			partial.Content = appendText(partial.Content, d)
			a.emit.Emit(MessageUpdateEvent{Message: partial, Kind: "text", Delta: d})
		},
		OnThinking: func(d string) {
			if !started {
				started = true
				a.emit.Emit(MessageStartEvent{Message: partial})
			}
			partial.Content = appendThinking(partial.Content, d)
			a.emit.Emit(MessageUpdateEvent{Message: partial, Kind: "thinking", Delta: d})
		},
		OnToolCall: func(c ToolCallBlock) {
			partial.Content = append(partial.Content, c)
		},
	}

	req := a.buildRequest()
	msg, err := a.opts.Provider.Stream(ctx, req, sink)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, errors.New("agent: provider 返回 nil 消息")
	}
	if msg.At == 0 {
		msg.At = now()
	}
	if msg.StopReason == "" || msg.StopReason == StopPending {
		if len(msg.ToolCalls()) > 0 {
			msg.StopReason = StopToolUse
		} else {
			msg.StopReason = StopEnd
		}
	}
	if msg.Model == "" {
		msg.Model = req.Model.ID
	}
	if msg.Provider == "" {
		msg.Provider = req.Model.Provider
	}
	a.emit.Emit(MessageEndEvent{Message: msg})
	return msg, nil
}

func appendText(blocks []Block, delta string) []Block {
	if n := len(blocks); n > 0 {
		if t, ok := blocks[n-1].(TextBlock); ok {
			t.Text += delta
			blocks[n-1] = t
			return blocks
		}
	}
	return append(blocks, TextBlock{Text: delta})
}

func appendThinking(blocks []Block, delta string) []Block {
	if n := len(blocks); n > 0 {
		if t, ok := blocks[n-1].(ThinkingBlock); ok {
			t.Thinking += delta
			blocks[n-1] = t
			return blocks
		}
	}
	return append(blocks, ThinkingBlock{Thinking: delta})
}

// buildRequest 组装模型请求：system prompt + 活动分支投影 + 工具定义。
func (a *Agent) buildRequest() Request {
	sys := BuildSystemPrompt(a.opts.Context)
	msgs := ProjectMessages(a.opts.Session.Messages())

	ctxWin := a.opts.ContextWindow
	maxOut := a.opts.Model.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = ctxWin / 4
	}
	return Request{
		SystemPrompt:  sys,
		Messages:      msgs,
		Tools:         toolSpecs(a.opts.Registry),
		Model:         a.opts.Model,
		MaxTokens:     maxOut,
		Purpose:       "chat",
		ThinkingLevel: a.opts.ThinkingLevel,
		Temperature:   a.opts.Temperature,
	}
}

// runTools 执行一轮里的全部工具调用。返回顺序与 calls 一致。
func (a *Agent) runTools(ctx context.Context, calls []ToolCallBlock) []*ToolResultMessage {
	out := make([]*ToolResultMessage, len(calls))
	if a.opts.Executor == nil || a.opts.Registry == nil {
		for i, c := range calls {
			out[i] = &ToolResultMessage{
				ToolCallID: c.ID, ToolName: c.Name,
				Content: []Block{TextBlock{Text: "工具执行未启用（未配置 Registry/Executor）"}},
				IsError: true, At: now(),
			}
		}
		return out
	}

	// 把模型调用转成 tools.Call。转换保序。
	tcalls := make([]tools.Call, len(calls))
	for i, c := range calls {
		tcalls[i] = tools.Call{ID: c.ID, Name: c.Name, Args: tools.Args(c.Arguments)}
		if tcalls[i].ID == "" {
			tcalls[i].ID = fmt.Sprintf("call_%d_%d", now(), i)
		}
	}

	// 事件：每个调用开始/结束时上报，携带是否并发。
	prevStart := a.opts.Executor.OnStart
	prevDone := a.opts.Executor.OnDone
	a.opts.Executor.OnStart = func(c tools.Call) {
		a.emit.Emit(ToolExecutionStartEvent{Call: ToolCallBlock{ID: c.ID, Name: c.Name, Arguments: c.Args}})
		if prevStart != nil {
			prevStart(c)
		}
	}
	a.opts.Executor.OnDone = func(r tools.ExecResult) {
		if prevDone != nil {
			prevDone(r)
		}
	}
	defer func() {
		a.opts.Executor.OnStart = prevStart
		a.opts.Executor.OnDone = prevDone
	}()

	results := a.opts.Executor.Execute(ctx, tcalls)
	for i, r := range results {
		msg := &ToolResultMessage{
			ToolCallID: tcalls[i].ID,
			ToolName:   tcalls[i].Name,
			Content:    ToolResultToBlocks(r.Result),
			Details:    r.Result.Details,
			IsError:    r.Result.IsError || r.Err != nil,
			At:         now(),
		}
		if r.Err != nil {
			msg.Content = append([]Block{TextBlock{Text: "工具执行失败: " + r.Err.Error()}}, msg.Content...)
		}
		out[i] = msg
		a.emit.Emit(ToolExecutionEndEvent{
			Call: ToolCallBlock{
				ID:        tcalls[i].ID,
				Name:      tcalls[i].Name,
				Arguments: map[string]any(tcalls[i].Args),
			},
			Result:   msg,
			Duration: r.Duration,
			Parallel: r.Parallel,
		})
	}
	return out
}
