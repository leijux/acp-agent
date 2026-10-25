package acp

import (
	"context"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// TestDenyMutatingDeniesBash 回归：只读审查模式必须拦住 bash。
//
// BashTool 没有 Mutation（调度上可并行），旧实现只查 Mutation/ConcurrencySafe，
// 于是 DenyMutating 会放行 shell —— 只读模式形同虚设。
func TestDenyMutatingDeniesBash(t *testing.T) {
	base, _ := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{Root: t.TempDir()},
	})
	g := DenyMutating(base)

	if err := g.Authorize(context.Background(), "bash", nil); err == nil {
		t.Fatal("只读模式应拒绝 bash")
	}
	if err := g.Authorize(context.Background(), "bash_kill", nil); err == nil {
		t.Fatal("只读模式应拒绝 bash_kill")
	}
	if err := g.Authorize(context.Background(), "read", nil); err != nil {
		t.Fatalf("只读工具被误拒: %v", err)
	}
	if err := g.Authorize(context.Background(), "grep", nil); err != nil {
		t.Fatalf("只读工具被误拒: %v", err)
	}
	if err := g.Authorize(context.Background(), "write", nil); err == nil {
		t.Fatal("只读模式应拒绝 write")
	}
}
