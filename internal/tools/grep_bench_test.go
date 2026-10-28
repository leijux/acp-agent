package tools

// grep 工具的基准测试。
//
// 「优化了多少」必须有数字兜底：这些基准覆盖三条主路径 ——
//   1. 稀疏命中（真实代码库的常态：绝大多数文件不命中）
//   2. 密集命中（渲染/拼接开销占主导）
//   3. 单大文件（读路径 + 行切分的压力）
//
// 跑法：go test ./internal/tools -run XXX -bench Grep -benchtime 5x

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedGrepTree 造一棵文件树。needleEveryNth 为 0 时每个文件都含 NEEDLE
// （密集命中）；否则每 N 个文件里放一行 NEEDLE（稀疏命中）。
func seedGrepTree(b *testing.B, root string, files, lines, needleEveryNth int) {
	b.Helper()
	if needleEveryNth <= 0 {
		needleEveryNth = 1
	}
	for i := 0; i < files; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%20))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		withNeedle := i%needleEveryNth == 0
		var sb strings.Builder
		for l := 0; l < lines; l++ {
			if withNeedle && l == lines/2 {
				fmt.Fprintf(&sb, "func target_%d() error { return nil } // NEEDLE\n", i)
				continue
			}
			fmt.Fprintf(&sb, "var filler_%d_%d = compute(%d, %d) // padding padding padding padding\n", i, l, l, i)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.go", i)), []byte(sb.String()), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGrep(b *testing.B, files, lines, every int, limit int64) {
	b.Helper()
	root := b.TempDir()
	seedGrepTree(b, root, files, lines, every)
	tool := NewGrepTool(Policy{Root: root, MaxReadBytes: 2 << 20})
	execArgs := Args{"pattern": "NEEDLE", "limit": limit}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := tool.Execute(context.Background(), execArgs)
		if err != nil || res.IsError {
			b.Fatalf("grep: %v %s", err, res.Text())
		}
	}
}

// BenchmarkGrepSparse 2000 个文件、每 50 个含一处命中（真实代码库常态）。
func BenchmarkGrepSparse(b *testing.B) {
	benchGrep(b, 2000, 60, 50, 200)
}

// BenchmarkGrepDense 500 个文件、每 10 行一处命中（渲染与 limit 截断开销）。
func BenchmarkGrepDense(b *testing.B) {
	benchGrep(b, 500, 60, 1, 500)
}

// BenchmarkGrepBigFile 单个 ~8MB 文件（流式读路径）。
func BenchmarkGrepBigFile(b *testing.B) {
	b.Helper()
	root := b.TempDir()
	var sb strings.Builder
	for l := 0; l < 80_000; l++ {
		if l%100 == 0 {
			fmt.Fprintf(&sb, "line %d NEEDLE\n", l)
			continue
		}
		sb.WriteString("line filler padding padding padding padding padding padding\n")
	}
	if err := os.WriteFile(filepath.Join(root, "big.log"), []byte(sb.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	tool := NewGrepTool(Policy{Root: root})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := tool.Execute(context.Background(), Args{"pattern": "NEEDLE", "limit": 100})
		if err != nil || res.IsError {
			b.Fatalf("grep: %v %s", err, res.Text())
		}
	}
}
