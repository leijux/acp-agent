//go:build !shellnative

package tools

// 内建 cp 的端到端测试：PATH 指空目录强制走内建实现。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellBuiltinCp(t *testing.T) {
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
	run(t, "mkdir -p src/sub && printf 'hello\\n' > src/a.txt && printf 'deep\\n' > src/sub/b.txt")

	t.Run("file", func(t *testing.T) {
		run(t, "cp src/a.txt copy.txt")
		data, err := os.ReadFile(filepath.Join(root, "copy.txt"))
		if err != nil || string(data) != "hello\n" {
			t.Fatalf("拷贝内容不对: %q %v", data, err)
		}
	})

	t.Run("dir_requires_recursive", func(t *testing.T) {
		if got := run(t, "cp src no-r || echo FAILED"); !strings.Contains(got, "FAILED") || !strings.Contains(got, "omitting directory") {
			t.Fatalf("目录不带 -r 应报错: %q", got)
		}
	})

	t.Run("dir_recursive", func(t *testing.T) {
		run(t, "cp -r src dst")
		data, err := os.ReadFile(filepath.Join(root, "dst", "sub", "b.txt"))
		if err != nil || string(data) != "deep\n" {
			t.Fatalf("递归拷贝结果不对: %q %v", data, err)
		}
	})

	t.Run("into_existing_dir", func(t *testing.T) {
		run(t, "mkdir -p out && cp src/a.txt out")
		if _, err := os.Stat(filepath.Join(root, "out", "a.txt")); err != nil {
			t.Fatalf("拷进已存在目录时应保留 basename: %v", err)
		}
	})

	t.Run("multiple_sources", func(t *testing.T) {
		run(t, "mkdir -p multi && cp src/a.txt src/sub/b.txt multi")
		for _, name := range []string{"a.txt", "b.txt"} {
			if _, err := os.Stat(filepath.Join(root, "multi", name)); err != nil {
				t.Fatalf("多源拷贝缺 %s: %v", name, err)
			}
		}
		// 目标不是目录时多源应报错
		if got := run(t, "cp src/a.txt src/sub/b.txt notadir || echo FAILED"); !strings.Contains(got, "FAILED") {
			t.Fatalf("多源目标不是目录应失败: %q", got)
		}
	})

	t.Run("no_clobber", func(t *testing.T) {
		run(t, "printf 'keep\\n' > n.txt && cp -n src/a.txt n.txt")
		data, _ := os.ReadFile(filepath.Join(root, "n.txt"))
		if string(data) != "keep\n" {
			t.Fatalf("-n 不该覆盖: %q", data)
		}
	})

	t.Run("missing_source", func(t *testing.T) {
		if got := run(t, "cp does-not-exist x || echo FAILED"); !strings.Contains(got, "FAILED") || !strings.Contains(got, "No such file") {
			t.Fatalf("缺源文件应报错: %q", got)
		}
	})

	t.Run("into_itself", func(t *testing.T) {
		if got := run(t, "cp -r src src/inner || echo FAILED"); !strings.Contains(got, "FAILED") {
			t.Fatalf("拷贝进自身应被拒绝: %q", got)
		}
	})
}
