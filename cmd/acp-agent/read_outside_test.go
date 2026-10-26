package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// TestAutoModeIgnoresRoot 回归：--permissions auto 下内置工具不再受 --root 约束。
//
// 依据：auto 已经放行一切改动型工具，守着 root 只会让模型绕道 bash
// （bash 的命令本身从来不受 Root 约束）。ask/readonly 必须保留边界。
func TestAutoModeIgnoresRoot(t *testing.T) {
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(outsideDir, "created.txt")

	appFor := func(mode string) *app {
		root := t.TempDir()
		a, _ := newTestApp(t, Config{
			Provider:    "deepseek",
			Model:       "deepseek-chat",
			Root:        root,
			Permissions: mode,
			MaxTurns:    1,
			Session:     filepath.Join(root, "s.jsonl"),
		}, "http://127.0.0.1:1")
		return a
	}
	exec := func(t *testing.T, a *app, name string, args tools.Args) tools.Result {
		t.Helper()
		tool, ok := a.reg.Get(name)
		if !ok {
			t.Fatalf("缺少 %s 工具", name)
		}
		res, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("%s Execute: %v", name, err)
		}
		return res
	}

	auto := appFor(permAuto)
	res := exec(t, auto, "read", tools.Args{"path": secret})
	if res.IsError {
		t.Fatalf("auto 模式应允许越界读: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "outside-content") {
		t.Fatalf("未读到内容: %q", res.Text())
	}
	res = exec(t, auto, "write", tools.Args{"path": fresh, "content": "hi\n"})
	if res.IsError {
		t.Fatalf("auto 模式应允许越界写: %s", res.Text())
	}
	if b, err := os.ReadFile(fresh); err != nil || string(b) != "hi\n" {
		t.Fatalf("越界写未生效: %q, %v", b, err)
	}

	// ask 模式保持原边界。注意到不了路径检查：改动型工具先被守卫拦下
	// （非交互下直接拒绝），read 才是由 Policy.Resolve 拒的。
	ask := appFor(permAsk)
	res = exec(t, ask, "read", tools.Args{"path": secret})
	if !res.IsError || !strings.Contains(res.Text(), "超出允许目录") {
		t.Fatalf("ask 模式应拒绝越界 read，得到: %s", res.Text())
	}
	res = exec(t, ask, "write", tools.Args{"path": fresh, "content": "no\n"})
	if !res.IsError {
		t.Fatalf("ask 模式应拒绝越界 write，得到: %s", res.Text())
	}
	if b, err := os.ReadFile(fresh); err == nil && string(b) == "no\n" {
		t.Fatal("ask 模式下越界写不应落盘")
	}
}
