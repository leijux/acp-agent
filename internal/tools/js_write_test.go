package tools

// js 的 fs.writeFile：只在 permissions=auto（AllowScriptWrite）下可用，
// 且路径同样受 Root 约束、写盘复用 write 的路径锁。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runJSWrite(t *testing.T, p Policy, script string) (string, bool) {
	t.Helper()
	tool := NewJSToolWithPolicy(p)
	res, err := tool.Execute(ctx_(), Args{"script": script})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text(), res.IsError
}

func TestJSWriteFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("requires_auto", func(t *testing.T) {
		p := Policy{Root: dir} // ask 模式：没开 AllowScriptWrite
		text, isErr := runJSWrite(t, p, `fs.writeFile("a.txt", "hi")`)
		if !isErr {
			t.Fatalf("未开 AllowScriptWrite 时应报错，实际 %q", text)
		}
		if !strings.Contains(text, "permissions=auto") || !strings.Contains(text, "write/edit") {
			t.Fatalf("错误信息应把模型引到 write 工具: %q", text)
		}
		if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
			t.Fatal("被拒绝的写入不该落盘")
		}
	})

	t.Run("writes_when_allowed", func(t *testing.T) {
		p := Policy{Root: dir, AllowScriptWrite: true}
		text, isErr := runJSWrite(t, p, `fs.writeFile("sub/b.txt", "内容")`)
		if isErr {
			t.Fatalf("应允许写入: %q", text)
		}
		b, err := os.ReadFile(filepath.Join(dir, "sub", "b.txt"))
		if err != nil || string(b) != "内容" {
			t.Fatalf("内容 = %q, %v", b, err)
		}
	})

	t.Run("readonly_blocks", func(t *testing.T) {
		p := Policy{Root: dir, AllowScriptWrite: true, ReadOnly: true}
		text, isErr := runJSWrite(t, p, `fs.writeFile("c.txt", "x")`)
		if !isErr || !strings.Contains(text, "只读") {
			t.Fatalf("只读模式应拒绝: %q", text)
		}
	})

	t.Run("outside_root_rejected", func(t *testing.T) {
		p := Policy{Root: dir, AllowScriptWrite: true}
		text, isErr := runJSWrite(t, p, `fs.writeFile("../escape.txt", "x")`)
		if !isErr {
			t.Fatalf("越界路径应被拒: %q", text)
		}
	})

	t.Run("returns_bytes", func(t *testing.T) {
		p := Policy{Root: dir, AllowScriptWrite: true}
		text, _ := runJSWrite(t, p, `fs.writeFile("d.txt", "12345"); "ok"`)
		if !strings.Contains(text, "ok") {
			t.Fatalf("脚本应继续执行: %q", text)
		}
	})
}
