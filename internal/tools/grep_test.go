package tools

// grep 工具的行为测试（并发与优化之后）。
//
// 这些用例盯的是「优化不能改变语义」：输出格式、顺序稳定性、limit 截断、
// 上下文去重、字面量预筛不改匹配结果。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func grepText(t *testing.T, tool *GrepTool, args Args) (string, Result) {
	t.Helper()
	res, err := tool.Execute(ctx_(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("grep 返回错误: %s", res.Text())
	}
	return res.Text(), res
}

// 上下文行用 "-" 分隔、匹配行用 ":"（GNU grep 的格式），
// 相邻命中的上下文重叠部分只输出一次。
func TestGrepContextFormatAndDedup(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	body := "l1\ntarget A\nl3\nl4\ntarget B\nl6\n"
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	text, _ := grepText(t, NewGrepTool(p), Args{"pattern": "target", "context": 1})
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	want := []string{
		"f.txt-1-l1",
		"f.txt:2:target A",
		"f.txt-3-l3",
		"f.txt-4-l4",
		"f.txt:5:target B",
		"f.txt-6-l6",
	}
	if len(lines) != len(want) {
		t.Fatalf("行数 = %d, want %d\n实际:\n%s", len(lines), len(want), text)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("第 %d 行 = %q, want %q\n完整输出:\n%s", i+1, lines[i], want[i], text)
		}
	}

	// 两个命中挨得很近时，重叠的上下文行不能重复出现。
	closeBody := "target A\ntarget B\n"
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte(closeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	text, _ = grepText(t, NewGrepTool(p), Args{"pattern": "target", "context": 2, "glob": "g.txt"})
	if n := strings.Count(text, "target B"); n != 1 {
		t.Fatalf("重叠上下文重复输出（%d 次）:\n%s", n, text)
	}
	if n := strings.Count(text, "target A"); n != 1 {
		t.Fatalf("重叠上下文重复输出（%d 次）:\n%s", n, text)
	}
}

// limit 截断：最多返回 limit 条命中，并标记 Truncated。
func TestGrepLimitTruncates(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("NEEDLE line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	text, res := grepText(t, NewGrepTool(p), Args{"pattern": "NEEDLE", "limit": 5})
	if got := strings.Count(text, "NEEDLE"); got != 5 {
		t.Fatalf("命中行数 = %d, want 5:\n%s", got, text)
	}
	if !res.Truncated {
		t.Fatal("达到 limit 时应标记 Truncated")
	}
	// limit 刚好等于命中总数时不算截断
	text, res = grepText(t, NewGrepTool(p), Args{"pattern": "NEEDLE", "limit": 50})
	if got := strings.Count(text, "NEEDLE"); got != 50 {
		t.Fatalf("命中行数 = %d, want 50", got)
	}
	if res.Truncated {
		t.Fatalf("刚好取完不算截断，实际 Truncated=true:\n%s", text)
	}
}

// 并发扫描必须给出确定的结果：同一查询跑多次，输出逐字节相同、且按路径序。
func TestGrepResultOrderIsStable(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	// 文件数要超过 grepSerialBelow，才会走并发分支
	for i := 0; i < grepSerialBelow+20; i++ {
		sub := filepath.Join(dir, "d"+itoa(i%7))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(sub, "f"+itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("alpha\ntarget here\nomega\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewGrepTool(p)
	first, _ := grepText(t, tool, Args{"pattern": "target", "limit": 1000})
	for i := 0; i < 3; i++ {
		got, _ := grepText(t, tool, Args{"pattern": "target", "limit": 1000})
		if got != first {
			t.Fatalf("第 %d 次结果与首次不一致（并发导致不确定）", i+2)
		}
	}

	// 顺序必须按遍历序（WalkDir 是字典序），而不是完成序
	var files []string
	for _, ln := range strings.Split(strings.TrimRight(first, "\n"), "\n") {
		if i := strings.Index(ln, ":"); i > 0 {
			f := ln[:i]
			if len(files) == 0 || files[len(files)-1] != f {
				files = append(files, f)
			}
		}
	}
	for i := 1; i < len(files); i++ {
		if files[i-1] > files[i] {
			t.Fatalf("文件顺序不是字典序：%q 出现在 %q 之前", files[i-1], files[i])
		}
	}
}

// 字面量预筛（LiteralPrefix）不能改变匹配结果。
func TestGrepLiteralPrefixPresieve(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	files := map[string]string{
		"has_prefix.txt": "foo then bar\n",
		"only_bar.txt":   "only bar here\n",   // 只有后缀，预筛应当整文件跳过
		"only_foo.txt":   "only foo here\n",   // 有前缀但不匹配整体
		"neither.txt":    "nothing relevant\n", // 两者都没有
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	text, _ := grepText(t, NewGrepTool(p), Args{"pattern": "foo.*bar"})
	if !strings.Contains(text, "has_prefix.txt") {
		t.Fatalf("漏掉了真正匹配的文件:\n%s", text)
	}
	for _, name := range []string{"only_bar.txt", "only_foo.txt", "neither.txt"} {
		if strings.Contains(text, name) {
			t.Fatalf("预筛后误报 %s:\n%s", name, text)
		}
	}

	// ignoreCase 时 LiteralPrefix 为空（(?i) 不是字面量），结果仍要正确
	text, _ = grepText(t, NewGrepTool(p), Args{"pattern": "FOO.*BAR", "ignoreCase": true})
	if !strings.Contains(text, "has_prefix.txt") {
		t.Fatalf("ignoreCase 下漏匹配:\n%s", text)
	}
}

// 超大文件跳过并提示，不读进内存。
func TestGrepSkipsHugeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("需要写一个 64MB 以上的文件")
	}
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	huge := filepath.Join(dir, "huge.log")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	// 只写够大小：稀疏内容，命中行放在开头，若真被扫描一定命中
	if _, err := f.WriteString("NEEDLE at top\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(grepMaxFileBytes + 1024); err != nil {
		f.Close()
		t.Skipf("无法创建超大文件（%v），跳过", err)
	}
	f.Close()
	defer os.Remove(huge)

	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("NEEDLE small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, _ := grepText(t, NewGrepTool(p), Args{"pattern": "NEEDLE"})
	if !strings.Contains(text, "small.txt") {
		t.Fatalf("小文件应正常命中:\n%s", text)
	}
	if strings.Contains(text, "huge.log") {
		t.Fatalf("超大文件不该被扫描:\n%s", text)
	}
	if !strings.Contains(text, "跳过 1 个超过") {
		t.Fatalf("缺少超大文件提示:\n%s", text)
	}
}

// 二进制文件与空文件不影响结果（旧行为保持不变）。
func TestGrepSkipsBinaryAndEmpty(t *testing.T) {
	p := testPolicy(t)
	dir := t.TempDir()
	p.Root = dir
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("NEEDLE\x00\x01\x02"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("NEEDLE text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, _ := grepText(t, NewGrepTool(p), Args{"pattern": "NEEDLE"})
	if !strings.Contains(text, "text.txt") {
		t.Fatalf("文本文件应命中:\n%s", text)
	}
	if strings.Contains(text, "bin.dat") || strings.Contains(text, "empty.txt") {
		t.Fatalf("二进制/空文件不该命中:\n%s", text)
	}
}
