package acp

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件测试 SpawnSubagent 的真实跨进程链路：
//
//	测试进程 ──spawn──▶ 测试二进制自身（带 ACP_FAKE_AGENT=1，内嵌 ACP agent）
//
// 用「测试二进制当子进程」而不是预编译 acp-agent 二进制，是为了让测试
// 不依赖构建产物；协议行为与真实子代理完全一致（都是 ACP over stdio）。

func TestMain(m *testing.M) {
	if os.Getenv("ACP_FAKE_AGENT") == "1" {
		os.Exit(runFakeAgent())
	}
	os.Exit(m.Run())
}

// runFakeAgent 在 stdin/stdout 上跑一个最小的 ACP agent。
//
// 行为由 prompt 文本里的指令控制，便于测试不同分支：
//   - 默认：回显 "echo:<prompt>"
//   - "perm"：先发一个 edit 类 tool_call 并请求权限，按结果决定回什么
//   - "slow"：持续推送直到被取消
//   - "fail"：返回 -32603 错误
func runFakeAgent() int {
	conn := NewConn(os.Stdin, os.Stdout, nil)
	srv, err := NewServer(conn, ServerOptions{Handler: fakeHandler{}, Info: Implementation{Name: "fake"}})
	if err != nil {
		return 1
	}
	if err := srv.Serve(); err != nil {
		return 1
	}
	return 0
}

type fakeHandler struct{}

func (fakeHandler) Initialize(context.Context, InitializeParams) (AgentCapabilities, error) {
	return AgentCapabilities{}, nil
}

func (fakeHandler) NewSession(_ context.Context, s *ServerSession, p SessionNewParams) (SessionSetup, error) {
	return SessionSetup{SessionID: "fake-" + s.ID}, nil
}

func (fakeHandler) LoadSession(ctx context.Context, s *ServerSession, p SessionLoadParams) (SessionSetup, error) {
	return SessionSetup{}, nil
}

func (fakeHandler) Cancel(context.Context, *ServerSession) {}

func (fakeHandler) Close(context.Context, *ServerSession) error { return nil }

func (fakeHandler) Prompt(ctx context.Context, s *ServerSession, prompt []ContentBlock) (PromptResult, error) {
	text := promptText(prompt)
	switch {
	case strings.Contains(text, "fail"):
		return PromptResult{}, fmt.Errorf("假 agent 故意失败")

	case strings.Contains(text, "perm"):
		// 模拟子代理要改动文件：先登记 tool_call，再请求权限。
		s.Sink.Send(ToolCallUpdate{
			SessionUpdate: "tool_call", ToolCallID: "tc-1", Title: "write a.txt", Kind: ToolKindEdit,
			Status: ToolStatusPending,
		})
		out, err := s.Client().RequestPermission(ctx, s.ID, ToolCallUpdate{
			ToolCallID: "tc-1", Title: "write a.txt", Kind: ToolKindEdit,
		}, []PermissionOption{
			{OptionID: "allow-once", Name: "允许", Kind: "allow_once"},
			{OptionID: "reject-once", Name: "拒绝", Kind: "reject_once"},
		})
		if err != nil {
			return PromptResult{}, err
		}
		switch out.Outcome {
		case "selected":
			s.Sink.Send(AgentMessageChunk{SessionUpdate: "agent_message_chunk", Content: TextBlock("perm answer: " + out.OptionID)})
		default:
			s.Sink.Send(AgentMessageChunk{SessionUpdate: "agent_message_chunk", Content: TextBlock("perm outcome: " + out.Outcome)})
		}
		return PromptResult{StopReason: StopEndTurn}, nil

	case strings.Contains(text, "slow"):
		// 每秒推一条，直到 ctx 被 cancel。
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return PromptResult{}, ctx.Err()
			case <-time.After(30 * time.Millisecond):
				s.Sink.Send(AgentMessageChunk{SessionUpdate: "agent_message_chunk", Content: TextBlock(fmt.Sprintf("tick%d ", i))})
			}
		}

	default:
		s.Sink.Send(AgentMessageChunk{SessionUpdate: "agent_message_chunk", Content: TextBlock("echo:" + text)})
		return PromptResult{StopReason: StopEndTurn}, nil
	}
}

func promptText(blocks []ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" || c.Text != "" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// selfSpec 构造「测试二进制自身当子进程」的启动参数。
func selfSpec(t *testing.T, extraEnv ...string) SubagentSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制: %v", err)
	}
	return SubagentSpec{
		Command: exe,
		Env:     append([]string{"ACP_FAKE_AGENT=1"}, extraEnv...),
	}
}

func TestSpawnSubagentEcho(t *testing.T) {
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec:       selfSpec(t),
		ClientInfo: Implementation{Name: "test", Version: "0"},
	})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	res, err := sub.Prompt(context.Background(), "你好")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got, want := res.Text, "echo:你好"; got != want {
		t.Errorf("文本 = %q，期望 %q", got, want)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("stopReason = %q，期望 %q", res.StopReason, StopEndTurn)
	}
	if sub.SessionID() == "" {
		t.Error("会话 ID 为空")
	}
}

func TestSpawnSubagentPermissionAllow(t *testing.T) {
	var asked atomic.Int32
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec: selfSpec(t),
		OnPermission: func(_ context.Context, p RequestPermissionParams) (string, error) {
			asked.Add(1)
			if p.ToolCall.Kind != ToolKindEdit {
				t.Errorf("kind = %q，期望 %q", p.ToolCall.Kind, ToolKindEdit)
			}
			for _, o := range p.Options {
				if o.Kind == "allow_once" {
					return o.OptionID, nil
				}
			}
			return "", fmt.Errorf("选项里没有 allow_once")
		},
	})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	res, err := sub.Prompt(context.Background(), "perm please")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if asked.Load() != 1 {
		t.Errorf("权限回调次数 = %d，期望 1", asked.Load())
	}
	if !strings.Contains(res.Text, "perm answer: allow-once") {
		t.Errorf("文本 = %q，期望包含 allow-once 的答复", res.Text)
	}
}

// 拒绝授权时，子代理必须拿到 outcome=cancelled（拿不到 selected），
// 而不是让 Prompt 整体失败 —— 否则模型会以为工具坏了。
func TestSpawnSubagentPermissionReject(t *testing.T) {
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec: selfSpec(t),
		OnPermission: func(_ context.Context, p RequestPermissionParams) (string, error) {
			for _, o := range p.Options {
				if o.Kind == "reject_once" {
					return o.OptionID, nil
				}
			}
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	res, err := sub.Prompt(context.Background(), "perm")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(res.Text, "perm answer: reject-once") {
		t.Errorf("文本 = %q，期望包含 reject-once", res.Text)
	}
}

func TestSpawnSubagentUpdates(t *testing.T) {
	var chunks atomic.Int32
	var sawToolCall atomic.Bool
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec: selfSpec(t),
		OnUpdate: func(u SubagentUpdate) {
			switch u.Kind {
			case SessionUpdateAgentMessage:
				chunks.Add(1)
			case SessionUpdateToolCall:
				sawToolCall.Store(true)
			}
		},
		OnPermission: func(_ context.Context, p RequestPermissionParams) (string, error) {
			return "allow-once", nil
		},
	})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	if _, err := sub.Prompt(context.Background(), "perm"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if chunks.Load() == 0 {
		t.Error("没有收到任何 agent_message_chunk")
	}
	if !sawToolCall.Load() {
		t.Error("没有收到 tool_call 更新")
	}
}

func TestSpawnSubagentError(t *testing.T) {
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{Spec: selfSpec(t)})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	if _, err := sub.Prompt(context.Background(), "fail"); err == nil {
		t.Fatal("期望报错，实际 nil")
	}
}

// 取消必须真的打断子代理，而不是只丢下它继续跑。
func TestSpawnSubagentCancel(t *testing.T) {
	sub, err := SpawnSubagent(context.Background(), SubagentOptions{Spec: selfSpec(t)})
	if err != nil {
		t.Fatalf("启动子代理: %v", err)
	}
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := sub.Prompt(ctx, "slow")
		errCh <- err
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("取消后期望返回错误，实际 nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Prompt 没有返回")
	}
}

// 子进程不存在时必须给出可读错误，而不是挂住。
func TestSpawnSubagentBadCommand(t *testing.T) {
	_, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec: SubagentSpec{Command: "acp-agent-does-not-exist-xyz", Dir: os.TempDir()},
	})
	if err == nil {
		t.Fatal("期望报错，实际 nil")
	}
}

func TestSubagentSpecEmptyCommand(t *testing.T) {
	if _, err := SpawnSubagent(context.Background(), SubagentOptions{}); err == nil {
		t.Fatal("Command 为空时期望报错，实际 nil")
	}
}

// TestSpawnSubagentHandshakeTimeout 回归：子代理不响应握手时不能无限挂住。
//
// helper 进程起来后什么都不做（不读 stdin、不回 initialize），
// 只能靠 HandshakeTimeout 兜住。
func TestSpawnSubagentHandshakeTimeout(t *testing.T) {
	cmd, args := silentHelper(t)

	start := time.Now()
	_, err := SpawnSubagent(context.Background(), SubagentOptions{
		Spec:             SubagentSpec{Command: cmd, Args: args, Dir: os.TempDir()},
		HandshakeTimeout: 300 * time.Millisecond,
		CloseTimeout:     500 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("握手应超时报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "握手超时") {
		t.Fatalf("错误信息应提到握手超时: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("握手超时没生效，耗时 %s", elapsed)
	}
}

// silentHelper 返回一个「启动后不读 stdin、不写 stdout」的命令，用于制造握手卡死。
func silentHelper(t *testing.T) (string, []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "ping", []string{"-n", "30", "127.0.0.1"}
	}
	return "sleep", []string{"30"}
}
