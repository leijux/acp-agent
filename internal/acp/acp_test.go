package acp

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tools"
)

// ---------- 测试脚手架 ----------

// fakeClient 是测试用的 ACP 客户端，跑在 net.Pipe 的另一端。
type fakeClient struct {
	t    *testing.T
	conn *Conn

	mu       sync.Mutex
	updates  []map[string]any
	perms    []RequestPermissionParams
	permOut  func(RequestPermissionParams) RequestPermissionOutcome
	notices  int
	resolved chan struct{}
}

func newPipe(t *testing.T) (*fakeClient, *Server, func()) {
	t.Helper()
	cr, sr := net.Pipe()

	clientConn := NewConn(cr, cr, nil)
	fc := &fakeClient{
		t:        t,
		conn:     clientConn,
		resolved: make(chan struct{}, 64),
	}
	clientConn.HandleNotify(MethodSessionUpdate, func(_ context.Context, params json.RawMessage) {
		var p SessionUpdateParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("client 解析 session/update 失败: %v", err)
			return
		}
		raw, _ := json.Marshal(p.Update)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		fc.mu.Lock()
		fc.updates = append(fc.updates, m)
		fc.mu.Unlock()
		select {
		case fc.resolved <- struct{}{}:
		default:
		}
	})
	clientConn.Handle(MethodSessionRequestPr, func(_ context.Context, params json.RawMessage) (any, error) {
		var p RequestPermissionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		fc.mu.Lock()
		fc.perms = append(fc.perms, p)
		out := fc.permOut
		fc.mu.Unlock()
		o := RequestPermissionOutcome{Outcome: "selected", OptionID: "allow-once"}
		if out != nil {
			o = out(p)
		}
		return RequestPermissionResult{Outcome: o}, nil
	})

	go clientConn.Serve()

	// server 端：注意 net.Pipe 是全双工的，一对读写即可。
	serverConn := NewConn(sr, sr, nil)
	srv, err := NewServer(serverConn, ServerOptions{
		Handler: mustHandler(t),
		Info:    Implementation{Name: "test-agent", Version: "0.0.1"},
		Capabilities: AgentCapabilities{
			LoadSession:        true,
			PromptCapabilities: PromptCapabilities{Image: true},
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()

	cleanup := func() {
		_ = srv.Close()
		_ = clientConn.Close()
	}
	return fc, srv, cleanup
}

func mustHandler(t *testing.T) Handler {
	t.Helper()
	h, err := NewAgentHandler(AgentConfig{
		Provider: agent.NewScriptedProvider(
			&agent.AssistantMessage{
				Content:    []agent.Block{agent.TextBlock{Text: "你好，我是测试 agent"}},
				StopReason: agent.StopEnd,
			},
		),
		Model: agent.Model{Provider: "test", ID: "test-model", ContextWindow: 200_000},
	})
	if err != nil {
		t.Fatalf("NewAgentHandler: %v", err)
	}
	return h
}

func (fc *fakeClient) initialize(t *testing.T) InitializeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out InitializeResult
	err := fc.conn.CallInto(ctx, MethodInitialize, InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: ClientCapabilities{
			FS:       FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
		ClientInfo: &Implementation{Name: "test-client", Version: "1.0"},
	}, &out)
	if err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	return out
}

func (fc *fakeClient) newSession(t *testing.T) SessionNewResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out SessionNewResult
	if err := fc.conn.CallInto(ctx, MethodSessionNew, SessionNewParams{Cwd: "/tmp"}, &out); err != nil {
		t.Fatalf("session/new 失败: %v", err)
	}
	return out
}

func (fc *fakeClient) prompt(t *testing.T, sid, text string) (SessionPromptResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out SessionPromptResult
	err := fc.conn.CallInto(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID: sid,
		Prompt:    []ContentBlock{TextBlock(text)},
	}, &out)
	return out, err
}

func (fc *fakeClient) kinds() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	out := make([]string, 0, len(fc.updates))
	for _, u := range fc.updates {
		if k, ok := u["sessionUpdate"].(string); ok {
			out = append(out, k)
		}
	}
	return out
}

func (fc *fakeClient) waitKind(t *testing.T, kind string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		fc.mu.Lock()
		for _, u := range fc.updates {
			if u["sessionUpdate"] == kind {
				fc.mu.Unlock()
				return u
			}
		}
		fc.mu.Unlock()
		select {
		case <-fc.resolved:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("等待 %q 超时，已收到: %v", kind, fc.kinds())
	return nil
}

// ---------- 测试 ----------

func TestInitializeNegotiatesCapabilities(t *testing.T) {
	fc, _, cleanup := newPipe(t)
	defer cleanup()

	out := fc.initialize(t)
	if out.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocolVersion = %d", out.ProtocolVersion)
	}
	if out.AgentInfo == nil || out.AgentInfo.Name != "test-agent" {
		t.Fatalf("agentInfo = %+v", out.AgentInfo)
	}
	if !out.AgentCapabilities.LoadSession {
		t.Fatal("未声明 loadSession")
	}
	if !out.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("未声明图片输入")
	}
}

func TestInitializeRejectsBadVersion(t *testing.T) {
	fc, _, cleanup := newPipe(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out InitializeResult
	err := fc.conn.CallInto(ctx, MethodInitialize, InitializeParams{ProtocolVersion: 99}, &out)
	if err == nil {
		t.Fatal("期望版本不匹配报错")
	}
}

func TestPromptStreamsUserAndAgentChunks(t *testing.T) {
	fc, _, cleanup := newPipe(t)
	defer cleanup()

	fc.initialize(t)
	sess := fc.newSession(t)
	if sess.SessionID == "" {
		t.Fatal("sessionId 为空")
	}

	res, err := fc.prompt(t, sess.SessionID, "hi")
	if err != nil {
		t.Fatalf("prompt 失败: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}

	kinds := fc.kinds()
	if !contains(kinds, SessionUpdateUserMessage) {
		t.Fatalf("缺少 user_message_chunk: %v", kinds)
	}
	chunk := fc.waitKind(t, SessionUpdateAgentMessage, time.Second)
	content, ok := chunk["content"].(map[string]any)
	if !ok || content["text"] != "你好，我是测试 agent" {
		t.Fatalf("agent_message_chunk 内容不对: %v", chunk)
	}
}

func TestPromptUnknownSession(t *testing.T) {
	fc, _, cleanup := newPipe(t)
	defer cleanup()

	fc.initialize(t)
	_, err := fc.prompt(t, "nope", "hi")
	if err == nil {
		t.Fatal("期望会话不存在报错")
	}
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) || rpcErr.Code != CodeResourceNotFound {
		t.Fatalf("错误码不对: %v", err)
	}
}

func TestToolCallEmitsUpdatesAndAsksPermission(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)

	var (
		mu      sync.Mutex
		updates []map[string]any
		perms   []RequestPermissionParams
	)
	clientConn.HandleNotify(MethodSessionUpdate, func(_ context.Context, params json.RawMessage) {
		var p SessionUpdateParams
		_ = json.Unmarshal(params, &p)
		raw, _ := json.Marshal(p.Update)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		updates = append(updates, m)
		mu.Unlock()
	})
	clientConn.Handle(MethodSessionRequestPr, func(_ context.Context, params json.RawMessage) (any, error) {
		var p RequestPermissionParams
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		perms = append(perms, p)
		mu.Unlock()
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{
			Outcome: "selected", OptionID: "allow-once",
		}}, nil
	})
	go clientConn.Serve()
	defer clientConn.Close()

	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "echo", mutates: false}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handler, err := NewAgentHandler(AgentConfig{
		Provider: agent.NewScriptedProvider(
			&agent.AssistantMessage{
				Content: []agent.Block{
					agent.TextBlock{Text: "我来调用工具"},
					agent.ToolCallBlock{ID: "call_1", Name: "echo", Arguments: map[string]any{"text": "x"}},
				},
				StopReason: agent.StopToolUse,
			},
			&agent.AssistantMessage{
				Content:    []agent.Block{agent.TextBlock{Text: "工具返回了"}},
				StopReason: agent.StopEnd,
			},
		),
		Model:       agent.Model{Provider: "test", ID: "test-model", ContextWindow: 200_000},
		Registry:    reg,
		Executor:    tools.NewExecutor(reg, 4),
		Permission:  PermissionAsk,
		AutoApprove: nil,
	})
	if err != nil {
		t.Fatalf("NewAgentHandler: %v", err)
	}

	serverConn := NewConn(sr, sr, nil)
	srv, err := NewServer(serverConn, ServerOptions{Handler: handler})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var init InitializeResult
	if err := clientConn.CallInto(ctx, MethodInitialize, InitializeParams{ProtocolVersion: ProtocolVersion}, &init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var sess SessionNewResult
	if err := clientConn.CallInto(ctx, MethodSessionNew, SessionNewParams{Cwd: "/tmp"}, &sess); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var res SessionPromptResult
	if err := clientConn.CallInto(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID: sess.SessionID,
		Prompt:    []ContentBlock{TextBlock("跑一下 echo")},
	}, &res); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(perms) != 1 {
		t.Fatalf("期望 1 次权限请求，实际 %d", len(perms))
	}
	if perms[0].ToolCall.ToolCallID != "call_1" {
		t.Fatalf("权限请求的 toolCallId = %q，期望 call_1", perms[0].ToolCall.ToolCallID)
	}
	if perms[0].ToolCall.Kind != ToolKindOther {
		t.Logf("kind = %q（stubTool 未声明 kind，走默认）", perms[0].ToolCall.Kind)
	}

	var sawCall, sawProgress, sawDone bool
	for _, u := range updates {
		switch u["sessionUpdate"] {
		case SessionUpdateToolCall:
			if u["toolCallId"] == "call_1" && u["status"] == ToolStatusPending {
				sawCall = true
			}
		case SessionUpdateToolCallUpdate:
			if u["toolCallId"] != "call_1" {
				continue
			}
			switch u["status"] {
			case ToolStatusInProgress:
				sawProgress = true
			case ToolStatusCompleted:
				sawDone = true
			}
		}
	}
	if !sawCall {
		t.Errorf("缺少 tool_call(pending) 更新: %v", updates)
	}
	if !sawProgress {
		t.Errorf("缺少 tool_call_update(in_progress)")
	}
	if !sawDone {
		t.Errorf("缺少 tool_call_update(completed)")
	}
}

func TestPermissionDeniedBecomesToolResult(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)
	clientConn.HandleNotify(MethodSessionUpdate, func(context.Context, json.RawMessage) {})
	clientConn.Handle(MethodSessionRequestPr, func(context.Context, json.RawMessage) (any, error) {
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{
			Outcome: "selected", OptionID: "reject-once",
		}}, nil
	})
	go clientConn.Serve()
	defer clientConn.Close()

	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	handler, _ := NewAgentHandler(AgentConfig{
		Provider: agent.NewScriptedProvider(
			&agent.AssistantMessage{
				Content: []agent.Block{
					agent.ToolCallBlock{ID: "c1", Name: "echo", Arguments: map[string]any{"text": "x"}},
				},
				StopReason: agent.StopToolUse,
			},
			&agent.AssistantMessage{
				Content:    []agent.Block{agent.TextBlock{Text: "好的，我不执行"}},
				StopReason: agent.StopEnd,
			},
		),
		Model:      agent.Model{Provider: "test", ID: "m", ContextWindow: 200_000},
		Registry:   reg,
		Executor:   tools.NewExecutor(reg, 4),
		Permission: PermissionAsk,
	})
	serverConn := NewConn(sr, sr, nil)
	srv, _ := NewServer(serverConn, ServerOptions{Handler: handler})
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var init InitializeResult
	_ = clientConn.CallInto(ctx, MethodInitialize, InitializeParams{ProtocolVersion: ProtocolVersion}, &init)
	var sess SessionNewResult
	if err := clientConn.CallInto(ctx, MethodSessionNew, SessionNewParams{Cwd: "/tmp"}, &sess); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var res SessionPromptResult
	// 拒绝后 turn 必须继续（模型还能看到拒绝原因并换个做法），不能整体失败。
	if err := clientConn.CallInto(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID: sess.SessionID, Prompt: []ContentBlock{TextBlock("跑")},
	}, &res); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}
}

func TestCancelStopsPrompt(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)
	clientConn.HandleNotify(MethodSessionUpdate, func(context.Context, json.RawMessage) {})
	go clientConn.Serve()
	defer clientConn.Close()

	// provider 阻塞到 ctx 取消，模拟长任务。
	blocking := agent.ProviderFunc(func(ctx context.Context, _ agent.Request, _ agent.Sink) (*agent.AssistantMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	handler, _ := NewAgentHandler(AgentConfig{
		Provider: blocking,
		Model:    agent.Model{Provider: "test", ID: "m", ContextWindow: 200_000},
	})
	serverConn := NewConn(sr, sr, nil)
	srv, _ := NewServer(serverConn, ServerOptions{Handler: handler})
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var init InitializeResult
	_ = clientConn.CallInto(ctx, MethodInitialize, InitializeParams{ProtocolVersion: ProtocolVersion}, &init)
	var sess SessionNewResult
	if err := clientConn.CallInto(ctx, MethodSessionNew, SessionNewParams{Cwd: "/tmp"}, &sess); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	done := make(chan SessionPromptResult, 1)
	go func() {
		var res SessionPromptResult
		_ = clientConn.CallInto(context.Background(), MethodSessionPrompt, SessionPromptParams{
			SessionID: sess.SessionID, Prompt: []ContentBlock{TextBlock("长任务")},
		}, &res)
		done <- res
	}()

	time.Sleep(50 * time.Millisecond)
	if err := clientConn.Notify(ctx, MethodSessionCancel, SessionCancelParams{SessionID: sess.SessionID}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case res := <-done:
		if res.StopReason != StopCancelled {
			t.Fatalf("stopReason = %q", res.StopReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel 之后 prompt 没有返回")
	}
}

func TestConcurrentPromptRejected(t *testing.T) {
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)
	clientConn.HandleNotify(MethodSessionUpdate, func(context.Context, json.RawMessage) {})
	go clientConn.Serve()
	defer clientConn.Close()

	blocking := agent.ProviderFunc(func(ctx context.Context, _ agent.Request, _ agent.Sink) (*agent.AssistantMessage, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return &agent.AssistantMessage{
				Content:    []agent.Block{agent.TextBlock{Text: "ok"}},
				StopReason: agent.StopEnd,
			}, nil
		}
	})
	handler, _ := NewAgentHandler(AgentConfig{
		Provider: blocking,
		Model:    agent.Model{Provider: "test", ID: "m", ContextWindow: 200_000},
	})
	serverConn := NewConn(sr, sr, nil)
	srv, _ := NewServer(serverConn, ServerOptions{Handler: handler})
	go srv.Serve()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var init InitializeResult
	_ = clientConn.CallInto(ctx, MethodInitialize, InitializeParams{ProtocolVersion: ProtocolVersion}, &init)
	var sess SessionNewResult
	if err := clientConn.CallInto(ctx, MethodSessionNew, SessionNewParams{Cwd: "/tmp"}, &sess); err != nil {
		t.Fatalf("session/new: %v", err)
	}

	go func() {
		var res SessionPromptResult
		_ = clientConn.CallInto(context.Background(), MethodSessionPrompt, SessionPromptParams{
			SessionID: sess.SessionID, Prompt: []ContentBlock{TextBlock("first")},
		}, &res)
	}()
	time.Sleep(50 * time.Millisecond)

	var res SessionPromptResult
	err := clientConn.CallInto(ctx, MethodSessionPrompt, SessionPromptParams{
		SessionID: sess.SessionID, Prompt: []ContentBlock{TextBlock("second")},
	}, &res)
	if err == nil {
		t.Fatal("期望并发 prompt 被拒绝")
	}
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) || rpcErr.Code != CodeInvalidRequest {
		t.Fatalf("错误码不对: %v", err)
	}
}

func TestUnknownMethodAndSessionClose(t *testing.T) {
	fc, _, cleanup := newPipe(t)
	defer cleanup()

	fc.initialize(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out map[string]any
	err := fc.conn.CallInto(ctx, "no/such_method", struct{}{}, &out)
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("未知方法错误码不对: %v", err)
	}

	sess := fc.newSession(t)
	if err := fc.conn.CallInto(ctx, MethodSessionClose, SessionCloseParams{SessionID: sess.SessionID}, nil); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	_, err = fc.prompt(t, sess.SessionID, "after close")
	if !asRPCError(err, &rpcErr) || rpcErr.Code != CodeResourceNotFound {
		t.Fatalf("关闭后 prompt 应报会话不存在: %v", err)
	}
}

// ---------- 辅助 ----------

type stubTool struct {
	name    string
	mutates bool
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "stub" }
func (s stubTool) Parameters() map[string]any {
	return tools.Schema(map[string]any{"text": tools.Prop("string", "文本")}, "text")
}
func (s stubTool) ConcurrencySafe() bool { return !s.mutates }
func (s stubTool) Mutates() bool         { return s.mutates }
func (s stubTool) Execute(_ context.Context, args tools.Args) (tools.Result, error) {
	return tools.TextResult("echo: %s", args.Str("text", "")), nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func asRPCError(err error, target **RPCError) bool {
	for err != nil {
		if r, ok := err.(*RPCError); ok {
			*target = r
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestNotificationOrderPreserved 锁定「通知必须串行处理」这一契约。
//
// 之前 jsonrpc.go 用 goroutine 处理每条通知，session/update 到达客户端的
// 顺序会乱（实测 tool_call_update completed 偶尔晚于 agent_message_chunk），
// 客户端 UI 就会出现错序的流式渲染。
func TestNotificationOrderPreserved(t *testing.T) {
	fc, srv, cleanup := newPipe(t)
	defer cleanup()
	fc.initialize(t)
	sess := fc.newSession(t)

	s, ok := srv.Session(sess.SessionID)
	if !ok {
		t.Fatal("server 侧会话不存在")
	}

	const n = 60
	for i := 0; i < n; i++ {
		s.Sink.Send(ToolCallUpdate{
			SessionUpdate: SessionUpdateToolCall,
			ToolCallID:    "seq",
			Status:        ToolStatusInProgress,
			Title:         time.Unix(0, int64(i)).Format("05.000"),
		})
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fc.mu.Lock()
		got := len(fc.updates)
		fc.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.updates) != n {
		t.Fatalf("收到 %d 条 update，期望 %d", len(fc.updates), n)
	}
	for i, u := range fc.updates {
		want := time.Unix(0, int64(i)).Format("05.000")
		if u["title"] != want {
			t.Fatalf("第 %d 条乱序：title=%v，期望 %v", i, u["title"], want)
		}
	}
}
