package tools

// glob 工具的行为测试。
//
// 重点盯两件事：
//   1. 按修改时间取「最新的 N 个」不能在截断时退化成「遍历序前 N 个」；
//   2. pattern 含 / 与不含 / 的匹配语义（相对路径 vs basename）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGlobTruncationReturnsNewest 修的是这个 bug：
// 旧实现在匹配数超过 limit*4 时直接 SkipAll，再在那一小撮里按 mtime 排序，
// 于是「找最近改动」返回的是遍历序靠前的旧文件（实测返回 f0020 而不是 f0999）。
func TestGlobTruncationReturnsNewest(t *testing.T) {
	dir := t.TempDir()
	p := Policy{Root: dir}
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 1000; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%04d.go", i))
		if err := os.WriteFile(name, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 序号越大越新
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	res, err := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "*.go", "limit": 5})
	if err != nil || res.IsError {
		t.Fatalf("glob: %v %s", err, res.Text())
	}
	text := res.Text()
	want := []string{"f0999.go", "f0998.go", "f0997.go", "f0996.go", "f0995.go"}
	for i, name := range want {
		if !strings.Contains(text, name) {
			t.Fatalf("缺少第 %d 新的文件 %s:\n%s", i+1, name, text)
		}
	}
	if !res.Truncated {
		t.Fatal("匹配数超过 limit 时应标记 Truncated")
	}
	// 顺序必须是 mtime 倒序
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := 1; i < len(want); i++ {
		if lines[i-1] != want[i-1] || lines[i] != want[i] {
			t.Fatalf("顺序不对，实际前几行:\n%s", text)
		}
	}
}

// 匹配数不超过 limit 时不标截断，且全部返回。
func TestGlobNoTruncationWhenFits(t *testing.T) {
	dir := t.TempDir()
	p := Policy{Root: dir}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "*.go", "limit": 10})
	if res.Truncated {
		t.Fatalf("没超限却标了截断:\n%s", res.Text())
	}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if !strings.Contains(res.Text(), name) {
			t.Fatalf("缺少 %s:\n%s", name, res.Text())
		}
	}
}

// 含 / 的 pattern 匹配相对路径；不含 / 的只匹配 basename。
func TestGlobPatternModes(t *testing.T) {
	dir := t.TempDir()
	p := Policy{Root: dir}
	files := []string{
		"pkg/a.go",
		"pkg/sub/b.go",
		"pkg/sub/c_test.go",
		"top_test.go",
	}
	for _, rel := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 不含 /：basename 匹配，递归全树
	res, _ := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "*_test.go"})
	got := res.Text()
	if !strings.Contains(got, "c_test.go") || !strings.Contains(got, "top_test.go") || strings.Contains(got, "a.go") {
		t.Fatalf("basename 匹配不对:\n%s", got)
	}

	// 含 /：相对路径匹配，* 不跨目录（输出路径分隔符随平台，只查文件名）
	res, _ = NewGlobTool(p).Execute(ctx_(), Args{"pattern": "pkg/*.go"})
	got = res.Text()
	if !strings.Contains(got, "a.go") || strings.Contains(got, "b.go") {
		t.Fatalf("相对路径匹配不对（* 不该跨目录）:\n%s", got)
	}

	// ** 跨层级
	res, _ = NewGlobTool(p).Execute(ctx_(), Args{"pattern": "pkg/**/*.go"})
	got = res.Text()
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.go") {
		t.Fatalf("** 匹配不对:\n%s", got)
	}
}

// 候选超过硬上限时要如实标注，而不是静默返回「遍历序前 N 个」。
func TestGlobCappedIsReported(t *testing.T) {
	dir := t.TempDir()
	p := Policy{Root: dir}
	for i := 0; i < 30; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := globMaxCandidates
	globMaxCandidates = 10
	t.Cleanup(func() { globMaxCandidates = saved })

	res, _ := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "*.go", "limit": 5})
	if !res.Truncated {
		t.Fatal("候选被截断时应标记 Truncated")
	}
	if !strings.Contains(res.Text(), "不保证是全库最新") {
		t.Fatalf("缺少「结果可能不完整」的提示:\n%s", res.Text())
	}
}
