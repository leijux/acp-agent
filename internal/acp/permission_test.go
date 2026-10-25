package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tools"
)

// TestWrapRegistryPreservesMutation 是并发调度的正确性前提：
// 包装后仍必须实现 tools.Mutation，否则 Executor 会把会改状态的工具
// 当成只读工具排进同一个并发批次。
func TestWrapRegistryPreservesMutation(t *testing.T) {
	base := tools.NewRegistry()
	if err := base.Register(
		stubTool{name: "reader", mutates: false},
		stubTool{name: "writer", mutates: true},
	); err != nil {
		t.Fatalf("Register: %v", err)
	}
	guard := GuardFunc(func(context.Context, string, map[string]any) error { return nil })
	wrapped, err := WrapRegistry(base, guard)
	if err != nil {
		t.Fatalf("WrapRegistry: %v", err)
	}
	if wrapped.Len() != 2 {
		t.Fatalf("工具数量 = %d", wrapped.Len())
	}
	w, ok := wrapped.Get("writer")
	if !ok {
		t.Fatal("writer 丢失")
	}
	m, ok := w.(tools.Mutation)
	if !ok {
		t.Fatal("包装后丢失 Mutation 接口 —— 会被并行调度")
	}
	if !m.Mutates() {
		t.Fatal("Mutates() 返回 false")
	}
	if w.ConcurrencySafe() {
		t.Fatal("mutating 工具不应标记为并发安全")
	}

	r, _ := wrapped.Get("reader")
	if r.ConcurrencySafe() != true {
		t.Fatal("只读工具应并发安全")
	}
}

// TestGuardBlocksExecution 确认门禁在工具真正执行之前生效。
func TestGuardBlocksExecution(t *testing.T) {
	base := tools.NewRegistry()
	spy := &spyTool{name: "spy"}
	if err := base.Register(spy); err != nil {
		t.Fatalf("Register: %v", err)
	}
	wrapped, err := WrapRegistry(base, GuardFunc(
		func(context.Context, string, map[string]any) error {
			return ErrPermissionDenied
		}))
	if err != nil {
		t.Fatalf("WrapRegistry: %v", err)
	}
	tool, _ := wrapped.Get("spy")
	res, err := tool.Execute(context.Background(), tools.Args{"text": "x"})
	if err != nil {
		t.Fatalf("拒绝应以工具结果的形式返回，而不是 Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("拒绝后结果应标记为错误，让模型看到原因")
	}
	if spy.calls != 0 {
		t.Fatal("被拒绝的工具仍然执行了")
	}
}

// TestDenyMutatingAllowsReadOnly 覆盖只读审查模式。
func TestDenyMutatingAllowsReadOnly(t *testing.T) {
	base := tools.NewRegistry()
	if err := base.Register(
		stubTool{name: "reader"},
		stubTool{name: "writer", mutates: true},
	); err != nil {
		t.Fatalf("Register: %v", err)
	}
	g := DenyMutating(base)
	if err := g.Authorize(context.Background(), "reader", nil); err != nil {
		t.Fatalf("只读工具被误拒: %v", err)
	}
	if err := g.Authorize(context.Background(), "writer", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("写工具应被拒: %v", err)
	}
}

// TestPermissionGuardRemembersAlwaysAllow 验证 allow-always 只弹一次。
func TestPermissionGuardRemembersAlwaysAllow(t *testing.T) {
	cr, sr := net.Pipe()
	// client 侧：对 server 发来的 request_permission 一律 allow-always。
	clientConn := NewConn(cr, cr, nil)
	var mu sync.Mutex
	asked := 0
	clientConn.Handle(MethodSessionRequestPr, func(context.Context, json.RawMessage) (any, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{
			Outcome: "selected", OptionID: "allow-always",
		}}, nil
	})
	go clientConn.Serve()
	defer clientConn.Close()

	// server（Agent）侧：CallInto 需要自己的读循环来收响应。
	serverConn := NewConn(sr, sr, nil)
	go serverConn.Serve()
	defer serverConn.Close()

	g := NewPermissionGuard(
		NewClient(serverConn, ClientCapabilities{}, nil),
		"s1",
		newUpdates(serverConn, "s1", ClientCapabilities{}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Authorize(ctx, "bash", nil); err != nil {
		t.Fatalf("第一次授权失败: %v", err)
	}
	if err := g.Authorize(ctx, "bash", nil); err != nil {
		t.Fatalf("第二次授权失败: %v", err)
	}
	mu.Lock()
	got := asked
	mu.Unlock()
	if got != 1 {
		t.Fatalf("allow-always 后仍弹了 %d 次权限请求", got)
	}
}

// TestPermissionGuardTimesOutAndStopsAsking 覆盖「客户端收下请求但永不回复」。
//
// 这是 --permissions ask 下最常见的卡死形态：请求发出去没人应答，
// 没有超时整个 turn 就挂在那里。除了超时，还要保证不会每一步都重试 ——
// 否则用户每调一个工具就得等一个完整超时。
func TestPermissionGuardTimesOutAndStopsAsking(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)
	var mu sync.Mutex
	asked := 0
	clientConn.Handle(MethodSessionRequestPr, func(ctx context.Context, _ json.RawMessage) (any, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		<-ctx.Done() // 收下，永不回复
		return nil, ctx.Err()
	})
	go clientConn.Serve()
	defer clientConn.Close()

	serverConn := NewConn(sr, sr, nil)
	go serverConn.Serve()
	defer serverConn.Close()

	g := NewPermissionGuard(
		NewClient(serverConn, ClientCapabilities{}, nil),
		"s1",
		newUpdates(serverConn, "s1", ClientCapabilities{}),
	).SetTimeout(150 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := g.Authorize(ctx, "bash", nil)
	if !errors.Is(err, ErrPermissionTimeout) {
		t.Fatalf("期望 ErrPermissionTimeout，得到 %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("超时耗时 %v，没有生效", d)
	}

	// 第二次必须立刻失败，且不再打扰客户端。
	mu.Lock()
	before := asked
	mu.Unlock()
	start = time.Now()
	if err := g.Authorize(ctx, "write", nil); !errors.Is(err, ErrPermissionTimeout) {
		t.Fatalf("第二次期望 ErrPermissionTimeout，得到 %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("已判定客户端不响应后仍等待了 %v", d)
	}
	mu.Lock()
	after := asked
	mu.Unlock()
	if after != before {
		t.Fatalf("已判定不响应后仍在请求客户端（%d → %d）", before, after)
	}
}

// TestPermissionGuardCancelIsNotTimeout 验证 turn 被取消时仍然是 cancelled。
func TestPermissionGuardCancelIsNotTimeout(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)
	clientConn.Handle(MethodSessionRequestPr, func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go clientConn.Serve()
	defer clientConn.Close()

	serverConn := NewConn(sr, sr, nil)
	go serverConn.Serve()
	defer serverConn.Close()

	g := NewPermissionGuard(
		NewClient(serverConn, ClientCapabilities{}, nil),
		"s1",
		newUpdates(serverConn, "s1", ClientCapabilities{}),
	).SetTimeout(10 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := g.Authorize(ctx, "bash", nil); !errors.Is(err, ErrPermissionCancelled) {
		t.Fatalf("期望 ErrPermissionCancelled，得到 %v", err)
	}
	if g.unresponsive {
		t.Fatal("取消不应被当成「客户端不响应」")
	}
}

// ---------- update.go 的映射 ----------

func TestToolKindAndTitle(t *testing.T) {
	cases := map[string]string{
		"read": ToolKindRead, "write": ToolKindEdit, "edit": ToolKindEdit,
		"glob": ToolKindSearch, "grep": ToolKindSearch,
		"bash": ToolKindExecute, "web_fetch": ToolKindFetch, "unknown_tool": ToolKindOther,
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
	if got := toolTitle("read", map[string]any{"path": "/a/b.go"}); got != "读取 /a/b.go" {
		t.Errorf("toolTitle(read) = %q", got)
	}
	long := toolTitle("bash", map[string]any{"command": "echo 1234567890123456789012345678901234567890123456789012345678901234567890"})
	if len([]rune(long)) > 70 {
		t.Errorf("标题未截断: %q", long)
	}
}

func TestMapStopReason(t *testing.T) {
	if got := mapStopReason(nil); got != StopEndTurn {
		t.Errorf("nil -> %q", got)
	}
	if got := mapStopReason(&agent.RunResult{Aborted: true}); got != StopCancelled {
		t.Errorf("aborted -> %q", got)
	}
}

// spyTool 记录被执行的次数，用于断言门禁先于执行。
type spyTool struct {
	name  string
	calls int
}

func (s *spyTool) Name() string        { return s.name }
func (s *spyTool) Description() string { return "spy" }
func (s *spyTool) Parameters() map[string]any {
	return tools.Schema(map[string]any{"text": tools.Prop("string", "文本")})
}
func (s *spyTool) ConcurrencySafe() bool { return true }
func (s *spyTool) Execute(context.Context, tools.Args) (tools.Result, error) {
	s.calls++
	return tools.TextResult("ran"), nil
}
