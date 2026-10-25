package acp

import (
	"context"
	"testing"
)

// loadSession 能力必须如实反映 AgentConfig.Dir：客户端会据此决定是否尝试恢复会话。
func TestInitializeDeclaresLoadSessionOnlyWithDir(t *testing.T) {
	ctx := context.Background()

	noDir := &AgentHandler{}
	caps, err := noDir.Initialize(ctx, InitializeParams{ProtocolVersion: ProtocolVersion})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if caps.LoadSession {
		t.Fatal("Dir 为空时不应声明 loadSession（LoadSession 调用必然失败）")
	}
	if !caps.PromptCapabilities.Image {
		t.Fatal("应始终声明图片输入")
	}

	withDir := &AgentHandler{cfg: AgentConfig{Dir: t.TempDir()}}
	caps, err = withDir.Initialize(ctx, InitializeParams{ProtocolVersion: ProtocolVersion})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !caps.LoadSession {
		t.Fatal("Dir 非空时应声明 loadSession")
	}
}

// 能力声明与实际行为必须一致：没有 Dir 时 session/load 报错，有 Dir 时走文件。
func TestLoadSessionMatchesDeclaredCapability(t *testing.T) {
	ctx := context.Background()

	caps, _ := (&AgentHandler{}).Initialize(ctx, InitializeParams{})
	if caps.LoadSession {
		t.Fatal("前提不成立：Dir 为空却声明了 loadSession")
	}
	_, err := (&AgentHandler{}).LoadSession(ctx, &ServerSession{}, SessionLoadParams{SessionID: "s"})
	if err == nil {
		t.Fatal("Dir 为空时 LoadSession 应报错")
	}

	dir := t.TempDir()
	h := &AgentHandler{cfg: AgentConfig{Dir: dir}}
	caps, _ = h.Initialize(ctx, InitializeParams{})
	if !caps.LoadSession {
		t.Fatal("前提不成立：Dir 非空却未声明 loadSession")
	}
	// 会话文件不存在 → 资源不存在，而不是「未配置目录」。
	_, err = h.LoadSession(ctx, &ServerSession{}, SessionLoadParams{SessionID: "nope"})
	if err == nil {
		t.Fatal("不存在的会话应报错")
	}
	if rpc, ok := err.(*RPCError); ok {
		if rpc.Code != CodeResourceNotFound {
			t.Fatalf("错误码 = %d, 期望 %d（%s）", rpc.Code, CodeResourceNotFound, rpc.Message)
		}
		return
	}
	t.Fatalf("错误类型 = %T, 期望 *RPCError", err)
}
