package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/tools"
)

// reply 构造一条纯文本 assistant 响应。
func reply(text string) *AssistantMessage {
	return &AssistantMessage{
		Content:    []Block{TextBlock{Text: text}},
		StopReason: StopEnd,
	}
}

// toolReply 构造一条带工具调用的 assistant 响应。
func toolReply(calls ...ToolCallBlock) *AssistantMessage {
	blocks := make([]Block, 0, len(calls))
	for _, c := range calls {
		blocks = append(blocks, c)
	}
	return &AssistantMessage{Content: blocks, StopReason: StopToolUse}
}

func newTestAgent(t *testing.T, p Provider, opts ...func(*Options)) *Agent {
	t.Helper()
	o := Options{
		Provider: p,
		Model:    Model{Provider: "test", ID: "test-model", ContextWindow: 1_000_000},
	}
	for _, f := range opts {
		f(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// ---------- 1. 基本 turn loop ----------

func TestRunSingleTurnNoTools(t *testing.T) {
	p := NewScriptedProvider(reply("你好"))
	a := newTestAgent(t, p)

	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 1 {
		t.Fatalf("Turns = %d, want 1", res.Turns)
	}
	if len(res.Messages) != 2 { // user + assistant
		t.Fatalf("Messages = %d, want 2", len(res.Messages))
	}
	if got := res.LastAssistant().Text(); got != "你好" {
		t.Fatalf("assistant text = %q", got)
	}
	// 会话树里应有 2 个 message entry
	if n := countMessageEntries(a.Session()); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
}

func countMessageEntries(s *Session) int {
	n := 0
	for _, e := range s.Entries() {
		if e.Type == EntryMessage {
			n++
		}
	}
	return n
}

// TestRunToolLoop 验证 pi 的核心语义：有工具结果就要再来一轮，
// 直到模型不再发起工具调用。
func TestRunToolLoop(t *testing.T) {
	p := NewScriptedProvider(
		toolReply(
			ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			ToolCallBlock{ID: "c2", Name: "read", Arguments: map[string]any{"path": "b.txt"}},
		),
		reply("读完了"),
	)
	reg := tools.NewRegistry()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("AAA"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("BBB"), 0o644)
	reg.MustRegister(&fakeReadTool{root: dir})

	ex := tools.NewExecutor(reg, 4)
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.Executor = ex
	})

	res, err := a.Run(context.Background(), "读两个文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2（工具结果必须再触发一轮）", res.Turns)
	}
	// 消息顺序：user, assistant(tool calls), toolResult, toolResult, assistant
	roles := make([]Role, len(res.Messages))
	for i, m := range res.Messages {
		roles[i] = m.Role()
	}
	want := []Role{RoleUser, RoleAssistant, RoleToolResult, RoleToolResult, RoleAssistant}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	// 工具结果顺序必须与 calls 一致（并发完成顺序无关）
	if tr := res.Messages[2].(*ToolResultMessage); !strings.Contains(tr.Text(), "AAA") {
		t.Fatalf("第一个工具结果应为 a.txt，实际 %q", tr.Text())
	}
	if tr := res.Messages[3].(*ToolResultMessage); !strings.Contains(tr.Text(), "BBB") {
		t.Fatalf("第二个工具结果应为 b.txt，实际 %q", tr.Text())
	}
	// 第二次请求的上下文里必须带上工具结果
	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(reqs))
	}
	if len(reqs[1].Messages) != 4 {
		t.Fatalf("第二轮上下文消息数 = %d, want 4", len(reqs[1].Messages))
	}
	if len(reqs[0].Tools) == 0 {
		t.Fatal("请求没有携带工具定义")
	}
}

// fakeReadTool 是最小只读工具，避免依赖具体内建实现的行为。
type fakeReadTool struct{ root string }

func (*fakeReadTool) Name() string        { return "read" }
func (*fakeReadTool) Description() string { return "读文件" }
func (*fakeReadTool) Parameters() map[string]any {
	return tools.Schema(map[string]any{"path": tools.Prop("string", "路径")}, "path")
}
func (*fakeReadTool) ConcurrencySafe() bool { return true }
func (f *fakeReadTool) Execute(ctx context.Context, a tools.Args) (tools.Result, error) {
	p, err := a.Required("path")
	if err != nil {
		return tools.ErrorResult("%v", err), nil
	}
	data, err := os.ReadFile(filepath.Join(f.root, p))
	if err != nil {
		return tools.ErrorResult("%v", err), nil
	}
	return tools.TextResult("%s", string(data)), nil
}

// ---------- 2. steering / follow-up / abort ----------

func TestSteeringEntersAfterCurrentTurn(t *testing.T) {
	var a *Agent
	p := NewScriptedProvider(
		func(req Request) (*AssistantMessage, error) {
			// 第一轮期间排队一条 steering 消息
			a.Steer("顺便看一下 b.txt")
			return toolReply(ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}), nil
		},
		reply("好的"),
	)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644)
	reg := tools.NewRegistry()
	reg.MustRegister(&fakeReadTool{root: dir})

	var err error
	a, err = New(Options{
		Provider: p,
		Model:    Model{Provider: "test", ID: "m", ContextWindow: 1_000_000},
		Registry: reg,
		Executor: tools.NewExecutor(reg, 4),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), "开始"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(reqs))
	}
	// 第二轮上下文倒数第二条应是 steering 的 user 消息（在 tool result 之后）
	msgs := reqs[1].Messages
	last := msgs[len(msgs)-1]
	um, ok := last.(*UserMessage)
	if !ok {
		t.Fatalf("最后一条应为 steering user 消息，实际 %T", last)
	}
	if !strings.Contains(blocksText(um.Content), "顺便看一下") {
		t.Fatalf("steering 内容缺失: %q", blocksText(um.Content))
	}
}

func TestFollowUpWaitsForIdle(t *testing.T) {
	var a *Agent
	p := NewScriptedProvider(
		func(req Request) (*AssistantMessage, error) {
			a.FollowUp("再做一件事")
			return reply("完成了"), nil
		},
		reply("做完了第二件事"),
	)
	var err error
	a, err = New(Options{
		Provider: p,
		Model:    Model{Provider: "test", ID: "m", ContextWindow: 1_000_000},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := a.Run(context.Background(), "做事")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2（follow-up 应触发新的一轮）", res.Turns)
	}
	// follow-up 在第一轮没有工具调用之后才进入
	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(reqs))
	}
}

func TestAbortReturnsQueuedMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := NewScriptedProvider(func(req Request) (*AssistantMessage, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	a := newTestAgent(t, p)
	a.Steer("排队1")
	a.FollowUp("排队2")

	res, err := a.Run(ctx, "跑")
	if err == nil && !res.Aborted {
		t.Fatal("应当被 abort")
	}
	if !res.Aborted {
		t.Fatalf("Aborted = false, err = %v", err)
	}
	if len(res.Abandoned) != 2 {
		t.Fatalf("Abandoned = %d, want 2", len(res.Abandoned))
	}
}

func TestAbortMethodStopsRun(t *testing.T) {
	started := make(chan struct{})
	p := NewScriptedProvider(func(req Request) (*AssistantMessage, error) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		return reply("太晚了"), nil
	}, reply("不该到这里"))
	a := newTestAgent(t, p)

	go func() {
		<-started
		a.Abort()
	}()
	res, err := a.Run(context.Background(), "x")
	if !res.Aborted {
		t.Fatalf("Aborted = false (err=%v)", err)
	}
}

// ---------- 3. 并发工具调用 ----------

func TestToolCallsRunConcurrently(t *testing.T) {
	dir := t.TempDir()
	reg := tools.NewRegistry()
	slow := &slowTool{delay: 120 * time.Millisecond, key: "s"}
	reg.MustRegister(&fakeReadTool{root: dir}, slow)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644)

	p := NewScriptedProvider(
		toolReply(
			ToolCallBlock{ID: "1", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			ToolCallBlock{ID: "2", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			ToolCallBlock{ID: "3", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			ToolCallBlock{ID: "4", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
		),
		reply("ok"),
	)
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.Executor = tools.NewExecutor(reg, 4)
	})

	var mu sync.Mutex
	var parallel []bool
	a.On(func(ev Event) {
		if e, ok := ev.(ToolExecutionEndEvent); ok {
			mu.Lock()
			parallel = append(parallel, e.Parallel)
			mu.Unlock()
		}
	})

	if _, err := a.Run(context.Background(), "并发读"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(parallel) != 4 {
		t.Fatalf("工具事件数 = %d, want 4", len(parallel))
	}
	allParallel := true
	for _, p := range parallel {
		if !p {
			allParallel = false
		}
	}
	if !allParallel {
		t.Fatalf("只读工具应全部并发执行，实际 %v", parallel)
	}
}

type slowTool struct {
	delay time.Duration
	key   string
}

func (s *slowTool) Name() string               { return "slow" }
func (s *slowTool) Description() string        { return "慢工具" }
func (s *slowTool) Parameters() map[string]any { return tools.Schema(map[string]any{}) }
func (*slowTool) ConcurrencySafe() bool        { return true }
func (s *slowTool) Execute(ctx context.Context, a tools.Args) (tools.Result, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return tools.ErrorResult("取消"), nil
	}
	return tools.TextResult("%s", s.key), nil
}

// ---------- 4. 压缩 ----------

func TestCompactionReducesContext(t *testing.T) {
	p := NewScriptedProvider(
		// 第一次调用是摘要请求（Purpose=summarize）
		func(req Request) (*AssistantMessage, error) {
			if req.Purpose == PurposeSummarize {
				return reply("## Goal\n测试压缩"), nil
			}
			return reply("不应到这里"), nil
		},
	)
	a := newTestAgent(t, p, func(o *Options) {
		o.ContextWindow = 40_000
		o.ReserveTokens = 1_000
	})
	// 造一段够长的历史（一直加到超阈值）
	for i := 0; i < 400 && !a.shouldCompact(); i++ {
		a.Push(TextMessage(strings.Repeat("这是一段比较长的中文内容用于撑大上下文。", 20)))
		a.Push(reply(strings.Repeat("模型回复内容。", 20)))
	}
	before := a.ContextTokens()
	if !a.shouldCompact() {
		t.Fatalf("应触发压缩（tokens=%d, window=%d）", before, a.opts.ContextWindow)
	}

	res, err := a.Compact(context.Background(), "", "manual")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !strings.Contains(res.Summary, "测试压缩") {
		t.Fatalf("summary = %q", res.Summary)
	}
	if res.TokensAfter >= res.TokensBefore {
		t.Fatalf("压缩后 token 未减少: %d → %d", res.TokensBefore, res.TokensAfter)
	}
	// 投影后第一条应是压缩摘要
	msgs := a.Session().Messages()
	if _, ok := msgs[0].(*CompactionSummaryMessage); !ok {
		t.Fatalf("投影首条应为 CompactionSummaryMessage，实际 %T", msgs[0])
	}
	// 关键：被块吃掉的 entry 必须从投影里真的消失（不只是 token 估算变小）。
	entryCount, blocked := 0, 0
	for _, e := range a.Session().ActiveBranch() {
		if e.Type == EntryMessage {
			entryCount++
		}
	}
	for _, e := range a.Session().Entries() {
		if e.Type == EntryCompaction {
			blocked += len(e.BlockedIDs)
		}
	}
	if blocked == 0 {
		t.Fatal("压缩块没有记录被吃掉的 entry（BlockedIDs 为空）")
	}
	if want := entryCount - blocked + 1; len(msgs) > want+2 {
		t.Fatalf("投影 %d 条，期望 ≈%d（entry %d - blocked %d + 摘要）",
			len(msgs), want, entryCount, blocked)
	}
	// 原始 entry 仍在会话树里（pi 语义：原文不删）
	if n := countMessageEntries(a.Session()); n == 0 || n%2 != 0 {
		t.Fatalf("原始 entry 计数异常: %d", n)
	}
}

// TestKernelBlockedMessagesHidden 验证 kernel 块投影：被块吃掉的 entry 不再进入上下文。
func TestKernelBlockedMessagesHidden(t *testing.T) {
	s := NewSession()
	e1, _ := s.Append(TextMessage("第一条"))
	s.Append(reply("回复一"))
	e3, _ := s.Append(TextMessage("第三条"))
	s.Append(reply("回复三"))

	// 块吃掉前两条 entry。
	if _, err := s.AppendCompactionBlock("b1", "块摘要", []string{e1.ID, e3.ID}, 100, 20); err != nil {
		t.Fatalf("AppendCompactionBlock: %v", err)
	}

	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("投影应为 摘要+两条回复，实际 %d 条：%#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(*CompactionSummaryMessage); !ok {
		t.Fatalf("首条应为压缩摘要，实际 %T", msgs[0])
	}
	if sm, ok := msgs[0].(*CompactionSummaryMessage); ok && sm.Summary != "块摘要" {
		t.Fatalf("摘要 = %q", sm.Summary)
	}
}

// TestKernelStateRoundTrip 验证 kernel state 随 JSONL 落盘并恢复。
func TestKernelStateRoundTrip(t *testing.T) {
	s := NewSession()
	s.Append(TextMessage("你好"))
	s.AppendCompactionBlock("b1", "摘要", []string{s.Entries()[0].ID}, 10, 1)
	s.SetKernelState([]byte(`{"keep":1}`))

	f := filepath.Join(t.TempDir(), "s.jsonl")
	if err := s.Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSession(f)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got := string(loaded.KernelState()); got != `{"keep":1}` {
		t.Fatalf("KernelState = %q", got)
	}
	ce := loaded.Entries()[1]
	if ce.BlockID != "b1" || len(ce.BlockedIDs) != 1 {
		t.Fatalf("块字段未持久化: %+v", ce)
	}
}

// TestAutoCompactionDuringToolLoop 验证 pi 的触发时机：
// 压缩发生在「工具结果追加后、下一次 assistant 响应前」。
func TestAutoCompactionDuringToolLoop(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644)
	reg := tools.NewRegistry()
	reg.MustRegister(&fakeReadTool{root: dir})

	var summarized bool
	p := NewScriptedProvider(
		toolReply(ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}),
		func(req Request) (*AssistantMessage, error) {
			if req.Purpose == PurposeSummarize {
				summarized = true
				return reply("## Goal\n自动压缩"), nil
			}
			return reply("完成"), nil
		},
		reply("最终完成"),
	)
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.Executor = tools.NewExecutor(reg, 4)
		o.ContextWindow = 30_000
		o.ReserveTokens = 1_000
	})
	for i := 0; i < 400 && !a.shouldCompact(); i++ {
		a.Push(TextMessage(strings.Repeat("填充历史内容。", 30)))
		a.Push(reply(strings.Repeat("填充回复内容。", 30)))
	}
	if !a.shouldCompact() {
		t.Fatalf("前置条件不满足，tokens=%d", a.ContextTokens())
	}
	if _, err := a.Run(context.Background(), "现在读文件"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !summarized {
		t.Fatal("工具轮之后没有触发自动压缩")
	}
}

// ---------- 5. 持久化与分支 ----------

func TestSessionSaveLoadRoundTrip(t *testing.T) {
	s := NewSession()
	s.Append(TextMessage("你好"))
	s.Append(&AssistantMessage{
		Content:    []Block{TextBlock{Text: "在的"}, ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a"}}},
		StopReason: StopToolUse,
		Usage:      Usage{Input: 10, Output: 5, TotalTokens: 15},
	})
	s.Append(&ToolResultMessage{ToolCallID: "c1", ToolName: "read", Content: []Block{TextBlock{Text: "内容"}}})
	s.Append(&BashExecutionMessage{Command: "ls", Output: "a.txt", ExitCode: intp(0)})
	s.Append(&CustomMessage{CustomType: "note", Content: []Block{TextBlock{Text: "注入"}}, Display: true})
	s.AppendCompaction("旧摘要", "e2", 123)

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(loaded.Entries()) != len(s.Entries()) {
		t.Fatalf("entry 数不一致: %d vs %d", len(loaded.Entries()), len(s.Entries()))
	}
	if loaded.LeafID() != s.LeafID() {
		t.Fatalf("leaf 不一致: %q vs %q", loaded.LeafID(), s.LeafID())
	}
	// 消息往返保真：assistant 的 tool call 与 usage 都要还原
	msgs := loaded.Messages()
	var foundAssistant bool
	for _, m := range msgs {
		if am, ok := m.(*AssistantMessage); ok {
			foundAssistant = true
			if len(am.ToolCalls()) != 1 {
				t.Fatalf("tool call 未还原: %+v", am.Content)
			}
			if am.Usage.TotalTokens != 15 {
				t.Fatalf("usage 未还原: %+v", am.Usage)
			}
		}
	}
	if !foundAssistant {
		t.Fatal("没有还原出 assistant 消息")
	}
}

func intp(v int) *int { return &v }

func TestBranchForkChangesProjection(t *testing.T) {
	s := NewSession()
	e1, _ := s.Append(TextMessage("第一问"))
	s.Append(reply("第一答"))

	if err := s.Fork(e1.ID); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	s.Append(TextMessage("第二问（新分支）"))

	msgs := s.Messages()
	if len(msgs) != 2 {
		t.Fatalf("分支投影长度 = %d, want 2（第一答不应出现）", len(msgs))
	}
	if got := TextOf(msgs[1]); !strings.Contains(got, "新分支") {
		t.Fatalf("msgs[1] = %q", got)
	}
	// 旧分支的 entry 仍在
	if len(s.Entries()) != 3 {
		t.Fatalf("entries = %d, want 3", len(s.Entries()))
	}
}

// ---------- 6. 错误与边界 ----------

func TestProviderErrorRecordsAssistantMessage(t *testing.T) {
	boom := errors.New("网络断了")
	p := NewScriptedProvider(func(req Request) (*AssistantMessage, error) { return nil, boom })
	a := newTestAgent(t, p)

	res, err := a.Run(context.Background(), "x")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	last := res.LastAssistant()
	if last == nil || last.StopReason != StopError {
		t.Fatalf("应记录 error 状态的 assistant 消息，实际 %+v", last)
	}
}

func TestMaxTurnsLimit(t *testing.T) {
	// 永远返回工具调用，触发轮数上限
	p := NewScriptedProvider()
	for i := 0; i < 10; i++ {
		p.steps = append(p.steps, toolReply(ToolCallBlock{ID: fmt.Sprint(i), Name: "read", Arguments: map[string]any{"path": "a"}}))
	}
	reg := tools.NewRegistry()
	reg.MustRegister(&fakeReadTool{root: t.TempDir()})
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.Executor = tools.NewExecutor(reg, 2)
		o.MaxTurns = 3
	})
	res, err := a.Run(context.Background(), "无限循环")
	if err == nil {
		t.Fatal("应因轮数上限报错")
	}
	if res.Turns != 3 {
		t.Fatalf("Turns = %d, want 3", res.Turns)
	}
}

// TestMaxTurnsZeroMeansUnlimited 默认（0）= 不限制。
//
// 上游 Pi 的 agent loop 本来就没有轮数上限，只靠「模型不再调工具」自然终止；
// 这里不允许再把 0 静默改写成某个默认值（以前先是 100，后来是 500），
// 否则「不限制」根本没有表达方式。
func TestMaxTurnsZeroMeansUnlimited(t *testing.T) {
	a := newTestAgent(t, NewScriptedProvider(reply("ok")))
	if a.opts.MaxTurns != 0 || a.opts.MaxSteps != 0 {
		t.Fatalf("未指定时不该补默认上限：MaxTurns=%d MaxSteps=%d", a.opts.MaxTurns, a.opts.MaxSteps)
	}
	// 0 不能触发「达到最大轮数限制」。
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 1 {
		t.Fatalf("Turns = %d, want 1", res.Turns)
	}

	// 负数同样归零（不限制），不能变成个负的上限让第一轮就撞线。
	neg := newTestAgent(t, NewScriptedProvider(reply("ok")), func(o *Options) {
		o.MaxTurns = -1
		o.MaxSteps = -2
	})
	if neg.opts.MaxTurns != 0 || neg.opts.MaxSteps != 0 {
		t.Fatalf("负数应规整为 0：MaxTurns=%d MaxSteps=%d", neg.opts.MaxTurns, neg.opts.MaxSteps)
	}
	if _, err := neg.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("负数上限不应报错: %v", err)
	}
}

func TestNoProviderError(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
}
// ---------- 7. 上下文组装 ----------

func TestBuildSystemPromptOrder(t *testing.T) {
	got := BuildSystemPrompt(ContextOptions{
		BasePrompt: "BASE",
		WorkingDir: "/repo",
		Sections:   map[string]string{"Coding": "SECT", "Alpha": "A"},
		ContextFiles: []ContextFile{
			{Path: "AGENTS.md", Content: "RULES"},
		},
		ExtraInstructions: "EXTRA",
	})
	idx := func(s string) int { return strings.Index(got, s) }
	if idx("BASE") < 0 || idx("SECT") < 0 || idx("RULES") < 0 || idx("EXTRA") < 0 {
		t.Fatalf("缺少片段:\n%s", got)
	}
	// section 按 key 排序：Alpha 在 Coding 之前
	if !(idx("Alpha") < idx("Coding")) {
		t.Fatalf("section 未排序:\n%s", got)
	}
	if !(idx("BASE") < idx("RULES") && idx("RULES") < idx("EXTRA")) {
		t.Fatalf("顺序错误:\n%s", got)
	}
}

func TestProjectMessagesConvertsBashAndCustom(t *testing.T) {
	msgs := []Message{
		&BashExecutionMessage{Command: "ls", Output: "a.txt"},
		&BashExecutionMessage{Command: "secret", Output: "x", ExcludeFromContext: true},
		&CustomMessage{CustomType: "note", Content: []Block{TextBlock{Text: "注入"}}},
	}
	out := ProjectMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	for _, m := range out {
		if m.Role() != RoleUser {
			t.Fatalf("应转成 user 消息，实际 %v", m.Role())
		}
	}
	if !strings.Contains(TextOf(out[0]), "ls") {
		t.Fatalf("bash 命令未保留: %q", TextOf(out[0]))
	}
}

func TestDiscoverContextFiles(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT"), 0o644)
	os.WriteFile(filepath.Join(root, "a", "AGENTS.md"), []byte("MID"), 0o644)

	files := DiscoverContextFiles(sub)
	if len(files) == 0 {
		t.Fatal("未发现上下文文件")
	}
	// 最近的一份优先
	if !strings.Contains(files[0].Content, "MID") {
		t.Fatalf("应优先最近的文件，实际 %q", files[0].Content)
	}
}

// TestUsageAccumulates 验证多轮 usage 累加。
func TestUsageAccumulates(t *testing.T) {
	p := NewScriptedProvider(
		&AssistantMessage{Content: []Block{TextBlock{Text: "a"}}, StopReason: StopEnd,
			Usage: Usage{Input: 100, Output: 10, TotalTokens: 110}},
		reply("b"),
	)
	a := newTestAgent(t, p)
	a.FollowUp("再来")
	res, err := a.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.Input != 100 {
		t.Fatalf("Usage.Input = %d, want 100", res.Usage.Input)
	}
}

// 思考等级与温度必须从 Options 透传到 Request —— 否则 provider 侧
// 的 DeepSeek thinking 参数与采样温度永远收不到（真实曾漏过）。
func TestAgentForwardsThinkingAndTemperature(t *testing.T) {
	temp := 0.3
	var got Request
	a := newTestAgent(t, ProviderFunc(func(ctx context.Context, req Request, _ Sink) (*AssistantMessage, error) {
		got = req
		return reply("ok"), nil
	}), func(o *Options) {
		o.ThinkingLevel = "high"
		o.Temperature = &temp
	})
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.ThinkingLevel != "high" {
		t.Errorf("ThinkingLevel = %q, want high", got.ThinkingLevel)
	}
	if got.Temperature == nil || *got.Temperature != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", got.Temperature)
	}
}
