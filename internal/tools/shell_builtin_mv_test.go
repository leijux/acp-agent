//go:build !shellnative

package tools

// 内建 mv 的端到端测试（PATH 指空目录强制走内建）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellBuiltinMv(t *testing.T) {
	bt, _ := newBash(t)
	empty := t.TempDir()
	run := func(t *testing.T, script string) string {
		t.Helper()
		res, err := bt.Execute(ctx_(), Args{"command": script, "env": map[string]any{"PATH": empty}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res.Text()
	}

	root := bt.Policy.Root
	mustWrite(t, filepath.Join(root, "a.txt"), "AAA", 0o644)

	t.Run("rename", func(t *testing.T) {
		run(t, "mv a.txt b.txt")
		if _, err := os.Stat(filepath.Join(root, "a.txt")); err == nil {
			t.Fatal("源文件应已不存在")
		}
		if b, err := os.ReadFile(filepath.Join(root, "b.txt")); err != nil || string(b) != "AAA" {
			t.Fatalf("目标内容 = %q, %v", b, err)
		}
	})

	t.Run("into_dir", func(t *testing.T) {
		run(t, "mkdir -p sub && mv b.txt sub")
		if b, err := os.ReadFile(filepath.Join(root, "sub", "b.txt")); err != nil || string(b) != "AAA" {
			t.Fatalf("移进目录失败: %q %v", b, err)
		}
	})

	t.Run("overwrite_and_noclobber", func(t *testing.T) {
		run(t, "printf OLD > x.txt && printf NEW > y.txt && mv y.txt x.txt")
		if b, _ := os.ReadFile(filepath.Join(root, "x.txt")); string(b) != "NEW" {
			t.Fatalf("默认应覆盖，实际 %q", b)
		}
		run(t, "printf KEEP > k1 && printf NOPE > k2 && mv -n k2 k1")
		if b, _ := os.ReadFile(filepath.Join(root, "k1")); string(b) != "KEEP" {
			t.Fatalf("-n 不该覆盖，实际 %q", b)
		}
	})

	t.Run("dir_rename", func(t *testing.T) {
		run(t, "mkdir -p d1/x && printf F > d1/x/f.txt && mv d1 d2")
		if b, err := os.ReadFile(filepath.Join(root, "d2", "x", "f.txt")); err != nil || string(b) != "F" {
			t.Fatalf("目录移动失败: %q %v", b, err)
		}
	})

	t.Run("multi_source_to_dir", func(t *testing.T) {
		run(t, "mkdir -p md && printf 1 > m1 && printf 2 > m2 && mv m1 m2 md")
		for _, name := range []string{"m1", "m2"} {
			if _, err := os.Stat(filepath.Join(root, "md", name)); err != nil {
				t.Fatalf("%s 未移进 md", name)
			}
		}
		// 多源但目标不是目录 → 失败
		if got := run(t, "printf 3 > m3 && mv m3 m4 notadir || echo FAILED"); !strings.Contains(got, "FAILED") {
			t.Fatalf("多源目标非目录应失败: %q", got)
		}
	})

	t.Run("into_itself_rejected", func(t *testing.T) {
		run(t, "mkdir -p self/sub")
		if got := run(t, "mv self self/sub || echo REJECTED"); !strings.Contains(got, "REJECTED") {
			t.Fatalf("移进自身应失败: %q", got)
		}
	})

	t.Run("missing_source", func(t *testing.T) {
		if got := run(t, "mv nope.txt somewhere || echo FAILED"); !strings.Contains(got, "FAILED") {
			t.Fatalf("不存在的源应失败: %q", got)
		}
	})
}
