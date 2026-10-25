package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/acp"
)

// TestGuard_NonInteractiveDeniesWithoutTouchingStdin 是这次卡死的回归测试。
//
// ACP 模式下 stdin 是 JSON-RPC 帧流：老实现会在 ask 里 ReadString('\n')，
// 永远等不到一行 y/n，delegate 就挂在那里（只能 Esc 取消）。现在必须
// 立刻拒绝，且连一次读都不能发起 —— 否则还是会吃掉协议字节。
func TestGuard_NonInteractiveDeniesWithoutTouchingStdin(t *testing.T) {
	pr, pw := io.Pipe() // 永不写入：任何读取都会永久阻塞
	defer pw.Close()

	g := newGuard(modeAsk, bufio.NewReader(pr), io.Discard)
	g.interactive = false

	done := make(chan error, 1)
	go func() { done <- g.authorize(context.Background(), "worker: write", true) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("非交互模式下改动型工具必须被拒绝")
		}
		var denied errDenied
		if !errors.As(err, &denied) {
			t.Fatalf("期望 errDenied，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("非交互模式下 authorize 不得阻塞（它在等 stdin）")
	}

	// 只读工具不受影响。
	if err := g.authorize(context.Background(), "read", false); err != nil {
		t.Fatalf("只读工具应放行: %v", err)
	}
}

// TestSubagentPermissionFallsBackWhenNoACPSession 验证非 ACP 场景下
// 子代理授权仍走本地 guard，且不会挂住。
func TestSubagentPermissionFallsBackWhenNoACPSession(t *testing.T) {
	g := newGuard(modeAsk, bufio.NewReader(&blockingReader{}), io.Discard)
	g.interactive = false
	a := &app{guard: g}

	done := make(chan string, 1)
	go func() {
		id, err := a.subagentPermission("worker")(context.Background(), acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{Name: "write", Kind: "edit"},
			Options:  acp.DefaultPermissionOptions(),
		})
		if err != nil {
			t.Errorf("OnPermission 不应返回错误（拒绝应走 option）: %v", err)
		}
		done <- id
	}()

	select {
	case id := <-done:
		if id != "reject-once" {
			t.Fatalf("期望 reject-once，得到 %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("子代理授权在无 ACP 会话时不得阻塞")
	}
}

// TestACPAuthorizeTimesOutWhenClientIgnores 验证「客户端收下请求但不回复」
// 不会把整个 delegate 永久挂住（以前只能靠用户 Esc）。
func TestACPAuthorizeTimesOutWhenClientIgnores(t *testing.T) {
	old := subagentPermTimeout
	subagentPermTimeout = 200 * time.Millisecond
	t.Cleanup(func() { subagentPermTimeout = old })

	cr, sr := net.Pipe()
	clientConn := acp.NewConn(cr, cr, nil)
	defer clientConn.Close()

	// 收到了，但永远不应答：模拟不支持权限弹窗的客户端。
	var asked atomic.Int32
	clientConn.Handle(acp.MethodSessionRequestPr, func(ctx context.Context, _ json.RawMessage) (any, error) {
		asked.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go clientConn.Serve()

	a := &app{}
	tracker := &acpSessionTracker{Handler: stubHandler{}, onSession: a.setACPSession}
	srv, err := acp.NewServer(acp.NewConn(sr, sr, nil), acp.ServerOptions{
		Handler: tracker,
		Info:    acp.Implementation{Name: "test-agent", Version: "0"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var initRes acp.InitializeResult
	if err := clientConn.CallInto(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: acp.ProtocolVersion}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var newRes acp.SessionNewResult
	if err := clientConn.CallInto(ctx, acp.MethodSessionNew, acp.SessionNewParams{Cwd: t.TempDir()}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	start := time.Now()
	_, handled, err := a.acpAuthorize(ctx, "worker: write", true)
	if !handled {
		t.Fatal("应走 ACP 通道")
	}
	if err == nil {
		t.Fatal("客户端不应答时必须报超时")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("期望超时错误，得到 %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("超时耗时 %v，说明没有生效", d)
	}

	// 第二次必须立失败，不能再等一个超时。
	askedBefore := asked.Load()
	start = time.Now()
	if _, handled, err := a.acpAuthorize(ctx, "worker: write", true); !handled || err == nil {
		t.Fatalf("第二次应快速失败，handled=%v err=%v", handled, err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("已判定客户端不响应后仍等待了 %v", d)
	}
	if got := asked.Load(); got != askedBefore {
		t.Fatalf("已判定不响应后仍在请求客户端（%d → %d）", askedBefore, got)
	}
}

// TestACPAuthorizeParentCancelIsNotTimeout 验证父 ctx 取消报 cancelled，
// 而不是被误判成「客户端不响应」并污染后续请求。
func TestACPAuthorizeParentCancelIsNotTimeout(t *testing.T) {
	old := subagentPermTimeout
	subagentPermTimeout = 5 * time.Second
	t.Cleanup(func() { subagentPermTimeout = old })

	cr, sr := net.Pipe()
	clientConn := acp.NewConn(cr, cr, nil)
	defer clientConn.Close()
	clientConn.Handle(acp.MethodSessionRequestPr, func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go clientConn.Serve()

	a := &app{}
	tracker := &acpSessionTracker{Handler: stubHandler{}, onSession: a.setACPSession}
	srv, err := acp.NewServer(acp.NewConn(sr, sr, nil), acp.ServerOptions{
		Handler: tracker,
		Info:    acp.Implementation{Name: "test-agent", Version: "0"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var initRes acp.InitializeResult
	if err := clientConn.CallInto(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: acp.ProtocolVersion}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var newRes acp.SessionNewResult
	if err := clientConn.CallInto(ctx, acp.MethodSessionNew, acp.SessionNewParams{Cwd: t.TempDir()}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelRun()
	}()

	_, handled, err := a.acpAuthorize(runCtx, "worker: write", true)
	if !handled {
		t.Fatal("应走 ACP 通道")
	}
	if !errors.Is(err, acp.ErrPermissionCancelled) {
		t.Fatalf("期望 ErrPermissionCancelled，得到 %v", err)
	}
	if a.subPerm.unsupported() {
		t.Fatal("取消不应被当成「客户端不响应」")
	}
}

// TestACPAuthorizeRemembersAlways 验证客户端选「总是允许」后不再重复弹窗，
// 且记忆按 label 隔离（不会把 worker 的授权顺带开给 reviewer）。
func TestACPAuthorizeRemembersAlways(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := acp.NewConn(cr, cr, nil)
	defer clientConn.Close()

	var asked atomic.Int32
	clientConn.Handle(acp.MethodSessionRequestPr, func(_ context.Context, _ json.RawMessage) (any, error) {
		asked.Add(1)
		return acp.RequestPermissionResult{Outcome: acp.RequestPermissionOutcome{
			Outcome:  "selected",
			OptionID: "allow-always",
		}}, nil
	})
	go clientConn.Serve()

	a := &app{}
	tracker := &acpSessionTracker{Handler: stubHandler{}, onSession: a.setACPSession}
	srv, err := acp.NewServer(acp.NewConn(sr, sr, nil), acp.ServerOptions{
		Handler: tracker,
		Info:    acp.Implementation{Name: "test-agent", Version: "0"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var initRes acp.InitializeResult
	if err := clientConn.CallInto(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: acp.ProtocolVersion}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var newRes acp.SessionNewResult
	if err := clientConn.CallInto(ctx, acp.MethodSessionNew, acp.SessionNewParams{Cwd: t.TempDir()}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	for i := 0; i < 3; i++ {
		allow, handled, err := a.acpAuthorize(ctx, "worker: write", true)
		if err != nil || !handled || !allow {
			t.Fatalf("第 %d 次: handled=%v allow=%v err=%v", i+1, handled, allow, err)
		}
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("allow-always 后客户端被问 %d 次，期望 1", got)
	}

	// 不同 label 必须重新问。
	if _, _, err := a.acpAuthorize(ctx, "reviewer: write", true); err != nil {
		t.Fatalf("reviewer 授权失败: %v", err)
	}
	if got := asked.Load(); got != 2 {
		t.Fatalf("新 label 应重新请求，客户端共被问 %d 次，期望 2", got)
	}
}

// TestSubagentPermissionCache 验证缓存本身的基本语义（含总是拒绝）。
func TestSubagentPermissionCache(t *testing.T) {
	var c subagentPermCache
	if _, ok := c.verdict("worker: write"); ok {
		t.Fatal("空缓存不应有记忆")
	}
	c.remember("worker: write", true)
	if allow, ok := c.verdict("worker: write"); !ok || !allow {
		t.Fatal("应记住总是允许")
	}
	// 后来的「总是拒绝」应覆盖之前的「总是允许」。
	c.remember("worker: write", false)
	if allow, ok := c.verdict("worker: write"); !ok || allow {
		t.Fatal("总是拒绝应覆盖总是允许")
	}
}

// blockingReader 永远阻塞，用来证明「没有读 stdin」。
type blockingReader struct{}

func (*blockingReader) Read([]byte) (int, error) {
	select {} // 永久阻塞（测试超时会先触发）
}

// TestACPAuthorizeRoundTripsThroughClient 是「子代理改动上报回客户端」的
// 端到端回归：ACP 模式下 delegate 的授权应该走 session/request_permission
// 弹给客户端，而不是去读 stdin。
func TestACPAuthorizeRoundTripsThroughClient(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := acp.NewConn(cr, cr, nil)
	defer clientConn.Close()

	var answer atomic.Value
	answer.Store("allow-once")
	var asked atomic.Int32
	clientConn.Handle(acp.MethodSessionRequestPr, func(_ context.Context, params json.RawMessage) (any, error) {
		asked.Add(1)
		var p acp.RequestPermissionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.ToolCall.Name != "worker: write" {
			t.Errorf("客户端收到的工具名 = %q", p.ToolCall.Name)
		}
		return acp.RequestPermissionResult{Outcome: acp.RequestPermissionOutcome{
			Outcome:  "selected",
			OptionID: answer.Load().(string),
		}}, nil
	})
	go clientConn.Serve()

	a := &app{}
	tracker := &acpSessionTracker{Handler: stubHandler{}, onSession: a.setACPSession}
	serverConn := acp.NewConn(sr, sr, nil)
	srv, err := acp.NewServer(serverConn, acp.ServerOptions{
		Handler: tracker,
		Info:    acp.Implementation{Name: "test-agent", Version: "0"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var initRes acp.InitializeResult
	if err := clientConn.CallInto(ctx, acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
	}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var newRes acp.SessionNewResult
	if err := clientConn.CallInto(ctx, acp.MethodSessionNew, acp.SessionNewParams{Cwd: t.TempDir()}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	allow, handled, err := a.acpAuthorize(ctx, "worker: write", true)
	if err != nil {
		t.Fatalf("acpAuthorize: %v", err)
	}
	if !handled || !allow {
		t.Fatalf("应经客户端放行，得到 handled=%v allow=%v", handled, allow)
	}

	answer.Store("reject-once")
	allow, handled, err = a.acpAuthorize(ctx, "worker: write", true)
	if err != nil {
		t.Fatalf("acpAuthorize: %v", err)
	}
	if !handled || allow {
		t.Fatalf("应经客户端拒绝，得到 handled=%v allow=%v", handled, allow)
	}
	if asked.Load() != 2 {
		t.Fatalf("客户端被问 %d 次，期望 2", asked.Load())
	}
}

// TestACPSessionTracker 验证会话记录的建立与清理。
func TestACPSessionTracker(t *testing.T) {
	var got []*acp.ServerSession
	tr := &acpSessionTracker{
		Handler:   stubHandler{},
		onSession: func(s *acp.ServerSession) { got = append(got, s) },
	}

	first := &acp.ServerSession{ID: "s1"}
	second := &acp.ServerSession{ID: "s2"}

	if _, err := tr.NewSession(context.Background(), first, acp.SessionNewParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.NewSession(context.Background(), second, acp.SessionNewParams{}); err != nil {
		t.Fatal(err)
	}
	// 关闭旧会话不能把新会话的授权通道拔掉。
	if err := tr.Close(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if tr.cur != second {
		t.Fatalf("关闭 s1 后当前会话应仍是 s2，得到 %v", tr.cur)
	}
	if err := tr.Close(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if tr.cur != nil {
		t.Fatalf("关闭当前会话后应为 nil，得到 %v", tr.cur)
	}
	if len(got) != 3 || got[0] != first || got[1] != second || got[2] != nil {
		t.Fatalf("onSession 调用序列不对: %v", got)
	}
}

// stubHandler 是只满足接口的空 Handler。
type stubHandler struct{}

func (stubHandler) Initialize(context.Context, acp.InitializeParams) (acp.AgentCapabilities, error) {
	return acp.AgentCapabilities{}, nil
}
func (stubHandler) NewSession(context.Context, *acp.ServerSession, acp.SessionNewParams) (acp.SessionSetup, error) {
	return acp.SessionSetup{}, nil
}
func (stubHandler) LoadSession(context.Context, *acp.ServerSession, acp.SessionLoadParams) (acp.SessionSetup, error) {
	return acp.SessionSetup{}, nil
}
func (stubHandler) Prompt(context.Context, *acp.ServerSession, []acp.ContentBlock) (acp.PromptResult, error) {
	return acp.PromptResult{}, nil
}
func (stubHandler) Cancel(context.Context, *acp.ServerSession) {}
func (stubHandler) Close(context.Context, *acp.ServerSession) error {
	return nil
}
