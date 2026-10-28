package tools

// glob 工具的基准测试。与 grep 一样，先把「基线」钉住，再谈优化。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// seedGlobTree 造一棵 20 目录 × files/20 文件的树。
func seedGlobTree(b *testing.B, root string, files int) {
	b.Helper()
	for i := 0; i < files; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%20), fmt.Sprintf("sub%d", i%3))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		name := fmt.Sprintf("file%05d.go", i)
		if i%10 == 0 {
			name = fmt.Sprintf("file%05d_test.go", i)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGlob(b *testing.B, files int, pattern string, limit int64) {
	b.Helper()
	root := b.TempDir()
	seedGlobTree(b, root, files)
	tool := NewGlobTool(Policy{Root: root})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := tool.Execute(context.Background(), Args{"pattern": pattern, "limit": limit})
		if err != nil || res.IsError {
			b.Fatalf("glob: %v %s", err, res.Text())
		}
	}
}

// BenchmarkGlobAll 全命中（limit 截断生效，遍历会被提前终止）。
func BenchmarkGlobAll(b *testing.B) { benchGlob(b, 2000, "**/*.go", 200) }

// BenchmarkGlobSparse 1/10 命中。
func BenchmarkGlobSparse(b *testing.B) { benchGlob(b, 2000, "**/*_test.go", 200) }

// BenchmarkGlobBasename 只按文件名匹配（无 / 的 pattern）。
func BenchmarkGlobBasename(b *testing.B) { benchGlob(b, 2000, "*_test.go", 200) }
