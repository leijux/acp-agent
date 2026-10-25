package main

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// TestReadonlyModeDeniesBash 回归：只读模式必须拦住 bash。
//
// bash 只实现了 ConcurrencySafe（调度可并行），没有 Mutation，
// 旧的门禁只看 Mutation.Mutates() → readonly 模式下 shell 被放行，
// 模型 `bash: rm -rf` / `python -c 'open(...,"w")'` 就绕过了只读限制。
func TestReadonlyModeDeniesBash(t *testing.T) {
	reg, _ := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{Root: t.TempDir()},
	})
	g := newGuard(modeReadonly, bufio.NewReader(strings.NewReader("")), io.Discard)
	wrapped := wrapRegistry(reg, g)

	bt, ok := wrapped.Get("bash")
	if !ok {
		t.Fatal("缺少 bash 工具")
	}
	// 调度层面必须保持可并行：只读命令并发是刻意保留的能力。
	if !bt.ConcurrencySafe() {
		t.Error("bash 不应因为权限判定而失去 ConcurrencySafe")
	}
	res, err := bt.Execute(context.Background(), tools.Args{"command": "echo hi"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("只读模式应拒绝 bash，得到结果: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "用户拒绝") {
		t.Fatalf("拒绝原因不对: %q", res.Text())
	}
}

// TestReadonlyModeStillAllowsRead 只读工具不受影响。
func TestReadonlyModeStillAllowsRead(t *testing.T) {
	reg, _ := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{Root: t.TempDir()},
	})
	g := newGuard(modeReadonly, bufio.NewReader(strings.NewReader("")), io.Discard)
	wrapped := wrapRegistry(reg, g)

	rt, ok := wrapped.Get("read")
	if !ok {
		t.Fatal("缺少 read 工具")
	}
	res, err := rt.Execute(context.Background(), tools.Args{"path": "不存在.txt"})
	if err != nil {
		t.Fatalf("只读工具不应因权限报错: %v", err)
	}
	if strings.Contains(res.Text(), "用户拒绝") {
		t.Fatalf("read 被误拒: %q", res.Text())
	}
}

// TestAutoModeAllowsBash auto 模式不询问。
func TestAutoModeAllowsBash(t *testing.T) {
	reg, _ := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{Root: t.TempDir()},
	})
	g := newGuard(modeAuto, bufio.NewReader(strings.NewReader("")), io.Discard)
	wrapped := wrapRegistry(reg, g)

	bt, _ := wrapped.Get("bash")
	res, err := bt.Execute(context.Background(), tools.Args{"command": "echo ok-auto"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(res.Text(), "用户拒绝") {
		t.Fatalf("auto 模式误拒: %q", res.Text())
	}
}
