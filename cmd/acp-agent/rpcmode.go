package main

// Pi 的常驻 JSON-RPC 面（--mode rpc）。
//
// 同一个模式承担两件事，靠「有没有收到 prompt」自然分流：
//
//  1. **探测**（编排器模型目录）：拉一个短命进程，写两行命令、读两行 response，
//     然后关 stdin 等退出（Multica 的 discoverPiModelsRPC）。只需要
//     get_state / get_available_models 两条只读命令。
//  2. **任务执行 + steering**（Pi 1.4 起的 task supplement）：先用
//     `{"type":"prompt","message":…}` 投递任务，运行期间可以再发
//     `{"type":"steer","message":…}` 插话；事件流与 `--mode json` 完全相同，
//     另外要求最终发一条 `agent_settled`（否则调用方判 "RPC exited before
//     agent_settled"）。
//
// 所以本文件 = 命令循环 + 一轮 run 的桥接。真正的 agent loop 在 internal/agent
// （早已有 Steer/FollowUp/Abort），这里只负责协议形状：
//
//	stdin  一行一条命令：{"id","type","message"}
//	stdout 一行一条 JSON：response / queue_update / message_start / 事件流 / agent_settled
//
// 关键约定（调用方按这些判断成败）：
//   - 非 JSON 行忽略，不中断会话；
//   - 未知命令回 success:false，进程继续；
//   - prompt 的 disposition 用 "queued" 而非 "handled"：handled + 尚未开始会被
//     调用方当成「整轮已结束」，它会立刻关 stdin 并停掉 supplement（steering 就废了）；
//   - steering 被真正吃进上下文时（下一轮 turn_start）补一条 role=user 的
//     message_start——调用方用「精确文本匹配」当送达凭证，只看 queued 回执不算数；
//   - stdin EOF：没跑过 prompt（探测）直接退；跑到一半则等这一轮结束再退。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

// rpcWriter 让事件流与 RPC 响应共用一个 stdout。
//
// 两边都可能来自不同 goroutine（事件来自 agent loop，响应来自命令循环），而
// 每一行必须整行落盘：json.Encoder 一次 Encode 就是一次 Write，所以在这里加
// 一把锁就够，不需要在两边各自缓冲。
type rpcWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (r *rpcWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.w.Write(p)
}

// piRPCModelJSON 是模型描述，字段名与 Pi 的 RPC 契约一致：编排器按
// reasoning / thinkingLevelMap 决定要不要显示思考档位选择器、显示哪几档。
type piRPCModelJSON struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	Provider         string            `json:"provider"`
	Reasoning        bool              `json:"reasoning"`
	ThinkingLevelMap map[string]string `json:"thinkingLevelMap,omitempty"`
}

// piRPCResponseJSON 是回给调用方的一条响应。
type piRPCResponseJSON struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Command string `json:"command,omitempty"`
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// piRPCRequest 是一条命令。message 只对 prompt / steer 有意义。
type piRPCRequest struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

// rpcTaskRunner 抽象「跑一轮」，让命令循环可以脱离 provider/app 测试。
type rpcTaskRunner interface {
	// Run 阻塞到这一轮结束（与 app.runOnce 一致）。
	Run(ctx context.Context, prompt string) error
	// Steer 把一条插话排进当前轮（agent.Steer 语义：本轮 assistant 结束后进入上下文）。
	Steer(text string)
	// On 注册事件观察者（agent.EventHandler）。
	On(agent.EventHandler)
}

// rpcSession 是一次 --mode rpc 会话的全部状态。
type rpcSession struct {
	cfg         Config
	apiKey      string
	providerArg string
	stderr      io.Writer

	out    *rpcWriter
	sink   *piEventSink
	models []piRPCModelJSON

	// newRunner 惰性构造 runner：探测场景永不调用，所以不会去要 API key、
	// 也不会连 MCP。
	newRunner func(ctx context.Context) (rpcTaskRunner, error)

	mu      sync.Mutex
	queue   []string // 已收下、还没进入上下文的 steering（queue_update 的快照来源）
	runner  rpcTaskRunner
	started bool
	runErr  error
	settled chan struct{}
}

// runPiRPC 服务一个 RPC 会话，stdin EOF（探测）或一轮跑完（任务）时返回。
func runPiRPC(stdin io.Reader, stdout, stderr io.Writer, providerArg, apiKey string, cfg Config) error {
	s := newRPCSession(stdout, stderr, providerArg, apiKey, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := s.loop(ctx, stdin)
	// 事件流没写完（磁盘满、管道坏）时不能还给调用方一个「成功」。
	if serr := s.sink.Err(); serr != nil {
		if err != nil {
			return fmt.Errorf("事件流写入失败: %w（run 同时失败: %v）", serr, err)
		}
		return fmt.Errorf("事件流写入失败: %w", serr)
	}
	return err
}

// newRPCSession 组装一次会话（不开始读 stdin，方便测试注入 newRunner）。
func newRPCSession(stdout, stderr io.Writer, providerArg, apiKey string, cfg Config) *rpcSession {
	out := &rpcWriter{w: stdout}
	s := &rpcSession{
		cfg: cfg, apiKey: apiKey, providerArg: providerArg, stderr: stderr,
		out:     out,
		sink:    newPiEventSink(out),
		models:  piRPCCatalog(providerArg),
		settled: make(chan struct{}),
	}
	s.newRunner = s.buildRunner
	return s
}

// loop 读命令、分发，直到进程该退出。
func (s *rpcSession) loop(ctx context.Context, stdin io.Reader) error {
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		readErr <- sc.Err()
	}()

	for {
		select {
		case <-s.settled:
			return s.result()
		case line, ok := <-lines:
			if !ok {
				if err := <-readErr; err != nil {
					return fmt.Errorf("--mode rpc 读 stdin: %w", err)
				}
				// stdin 关了：探测（没投过 prompt）直接退出；任务跑到一半则等
				// 这一轮结束——编排器可能先关 stdin，再继续读事件直到 settled。
				if !s.isStarted() {
					return nil
				}
				<-s.settled
				return s.result()
			}
			if err := s.handle(ctx, strings.TrimSpace(line)); err != nil {
				return err
			}
		}
	}
}

func (s *rpcSession) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (s *rpcSession) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// handle 处理一行命令。
func (s *rpcSession) handle(ctx context.Context, line string) error {
	if line == "" {
		return nil
	}
	var req piRPCRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		// 不是 JSON：调用方可能在写注释/日志，忽略而不是把会话打断。
		if s.stderr != nil {
			fmt.Fprintf(s.stderr, "警告: --mode rpc 收到非 JSON 行，已忽略: %s\n", truncateLine(line))
		}
		return nil
	}
	switch req.Type {
	case "get_state":
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "get_state", Success: true,
			Data: map[string]any{
				"model":         piRPCStateModel(s.cfg, s.models),
				"thinkingLevel": s.cfg.ThinkingLevel,
			},
		})
	case "get_available_models":
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "get_available_models", Success: true,
			Data: map[string]any{"models": s.models},
		})
	case "prompt":
		s.startPrompt(ctx, req)
	case "steer":
		s.steer(req)
	case "":
		// 没有 type：回一条失败，别让调用方一直等到超时。
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Success: false, Error: "缺少命令类型（type）",
		})
	default:
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: req.Type, Success: false,
			Error: fmt.Sprintf("%s 未实现（只支持 get_state / get_available_models / prompt / steer）", req.Type),
		})
	}
	return nil
}

// startPrompt 投递初始任务并开始跑一轮。本进程只跑一轮（与 Pi 的 -p 语义一致）。
func (s *rpcSession) startPrompt(ctx context.Context, req piRPCRequest) {
	if s.isStarted() {
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "prompt", Success: false,
			Error: "本进程已经跑过一轮 prompt（每次执行只接受一条初始任务）",
		})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "prompt", Success: false,
			Error: "prompt 不能为空",
		})
		return
	}
	runner, err := s.newRunner(ctx)
	if err != nil {
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "prompt", Success: false, Error: err.Error(),
		})
		return
	}
	// 插话被吃进上下文时（下一轮开始）补一条 user message_start：调用方用它当
	// 「送达凭证」，只看 queued 回执不算数。
	runner.On(func(ev agent.Event) {
		if _, ok := ev.(agent.TurnStartEvent); ok {
			s.flushSteeringReceipts()
		}
	})
	s.mu.Lock()
	s.runner = runner
	s.started = true
	s.mu.Unlock()

	// disposition 必须是 "queued"：调用方把「handled 且尚未开始」当成整轮已结束，
	// 会立刻关 stdin 并停掉 supplement —— 那样 steering 就再也送不进来了。
	s.sink.emit(piRPCResponseJSON{
		Type: "response", ID: req.ID, Command: "prompt", Success: true,
		Data: map[string]any{"disposition": "queued"},
	})

	go func() {
		err := runner.Run(ctx, req.Message)
		// 收尾信号：调用方拿不到它会把整轮判成失败（
		// "pi RPC exited before agent_settled"）。
		s.sink.emit(map[string]any{"type": "agent_settled"})
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
		close(s.settled)
	}()
}

// steer 把一条插话排进当前轮。
func (s *rpcSession) steer(req piRPCRequest) {
	s.mu.Lock()
	runner, started := s.runner, s.started
	s.mu.Unlock()
	if !started || runner == nil || s.isSettled() {
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "steer", Success: false,
			Error: "没有正在执行的轮次（steer 只能在 prompt 之后、轮次结束之前发）",
		})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.sink.emit(piRPCResponseJSON{
			Type: "response", ID: req.ID, Command: "steer", Success: false,
			Error: "steer 的 message 不能为空",
		})
		return
	}
	runner.Steer(req.Message)
	s.mu.Lock()
	s.queue = append(s.queue, req.Message)
	queued := append([]string(nil), s.queue...)
	s.mu.Unlock()

	s.sink.emit(piRPCResponseJSON{
		Type: "response", ID: req.ID, Command: "steer", Success: true,
		Data: map[string]any{"disposition": "queued"},
	})
	s.emitQueueUpdate(queued)
}

func (s *rpcSession) isSettled() bool {
	select {
	case <-s.settled:
		return true
	default:
		return false
	}
}

// flushSteeringReceipts 在「新的一轮开始」时把排队的插话标记为已送达。
//
// agent 的语义是：本轮 assistant 结束后 takeSteering() 全量取走、Push 进上下文，
// 然后 continue 到下一轮 —— 所以 turn_start 就是「这些消息已经进了上下文」的
// 唯一可靠信号。
func (s *rpcSession) flushSteeringReceipts() {
	s.mu.Lock()
	pending := s.queue
	s.queue = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	for _, text := range pending {
		s.sink.emit(map[string]any{
			"type":    "message_start",
			"message": map[string]any{"role": "user", "content": text},
		})
	}
	s.emitQueueUpdate(nil)
}

// emitQueueUpdate 发出当前 steering 队列快照（Pi 用 queue_update 通告队列变化；
// 我们不改写用户文本，所以它只是让调用方的「队列增量」逻辑有数据可对）。
func (s *rpcSession) emitQueueUpdate(steering []string) {
	if steering == nil {
		steering = []string{}
	}
	s.sink.emit(map[string]any{"type": "queue_update", "steering": steering})
}

// buildRunner 生产实现：真的构造 provider + app 并跑一轮。
func (s *rpcSession) buildRunner(ctx context.Context) (rpcTaskRunner, error) {
	// rpc 的任务执行复用 json 模式的事件流接线（stdout 只放 JSON、ask 提成 auto
	// 已在 applyRunMode 里做过），只是多了一层 RPC 命令通道。
	cfg := s.cfg
	cfg.JSONMode = true
	cfg.RPCMode = false
	cfg.JSONOut = s.out

	installSegmenter(cfg)
	apiKey := resolveAPIKey(cfg, s.apiKey)
	prov, model, err := buildProvider(cfg, apiKey)
	if err != nil {
		return nil, err
	}
	termOut := newTerminal(os.Stderr, true)
	termOut.quiet = true
	// stdin 属于 RPC 命令通道：权限询问绝不能从它读（ask 已提成 auto）。
	in := bufio.NewReader(strings.NewReader(""))
	ap, err := newApp(ctx, cfg, prov, model, in, termOut, false, false)
	if err != nil {
		return nil, err
	}
	return &appRunner{ap: ap}, nil
}

// appRunner 把 app 适配成 rpcTaskRunner。
type appRunner struct{ ap *app }

func (r *appRunner) Run(ctx context.Context, prompt string) error {
	defer r.ap.close()
	return r.ap.runOnce(ctx, prompt)
}

func (r *appRunner) Steer(text string) { r.ap.agent.Steer(text) }

func (r *appRunner) On(h agent.EventHandler) { r.ap.agent.On(h) }

// truncateLine 把回显的坏行截短，避免日志被整段乱码刷屏。
func truncateLine(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// piRPCCatalog 把模型目录摊平成 RPC 形状，与 --list-models 同源。
func piRPCCatalog(providerArg string) []piRPCModelJSON {
	rows := piCatalogRows(piCatalogEndpoints(providerArg))
	models := make([]piRPCModelJSON, 0, len(rows))
	for _, r := range rows {
		info, _ := provider.LookupModel(r.ID)
		models = append(models, piRPCModelJSON{
			ID:               r.ID,
			Provider:         r.Provider,
			Reasoning:        info.SupportsThinking,
			ThinkingLevelMap: piRPCThinkingLevelMap(info.SupportsThinking),
		})
	}
	return models
}

// piRPCStateModel 找出「不指定 --model 时我们会用哪个模型」。
//
// 先在目录里按 provider+id 精确匹配（编排器就是拿 provider + "/" + id 去比
// get_state 的返回值来标记 Default）；匹配不到（手填的模型、端点没配 key）
// 也照样报一份，免得调用方以为我们没模型。
func piRPCStateModel(cfg Config, models []piRPCModelJSON) piRPCModelJSON {
	for _, m := range models {
		if m.Provider == cfg.Provider && m.ID == cfg.Model {
			return m
		}
	}
	info, _ := provider.LookupModel(cfg.Model)
	return piRPCModelJSON{
		ID:               cfg.Model,
		Provider:         cfg.Provider,
		Reasoning:        info.SupportsThinking,
		ThinkingLevelMap: piRPCThinkingLevelMap(info.SupportsThinking),
	}
}

// piRPCThinkingLevelMap 如实报出我们对每一档的处理方式。
//
// Pi 的语义：off..high 默认可用（除非显式映射成 null）；xhigh/max 必须给出
// 非空映射才会出现在选择器里。我们的映射（见 provider.ThinkingParams）：
//
//	off                      → thinking=disabled
//	minimal / low            → reasoning_effort=low
//	medium / high / xhigh    → reasoning_effort=high
//	max                      → reasoning_effort=max
//
// 不支持思考的模型回 nil：编排器据此隐藏档位选择器（比给它一个调了也没用的
// 选择器诚实）。
func piRPCThinkingLevelMap(thinking bool) map[string]string {
	if !thinking {
		return nil
	}
	return map[string]string{
		"minimal": "low",
		"low":     "low",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "high",
		"max":     "max",
	}
}
