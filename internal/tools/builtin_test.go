package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func testPolicy(t *testing.T) Policy {
	t.Helper()
	return Policy{Root: t.TempDir(), MaxReadBytes: 64 << 10}
}

func ctx_() context.Context { return context.Background() }

// ---------------------------------------------------------------------------
// Truncate
// ---------------------------------------------------------------------------

func TestTruncateByLines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	out, truncated := Truncate(b.String(), 10, 0)
	if !truncated {
		t.Fatal("应发生截断")
	}
	if !strings.Contains(out, "已省略") {
		t.Fatalf("缺少省略标记: %q", out)
	}
	if !strings.Contains(out, "line 1") || !strings.Contains(out, "line 100") {
		t.Fatal("应保留头尾")
	}
}

func TestTruncateByBytes(t *testing.T) {
	s := strings.Repeat("中文字符", 5000) // 多字节，切点必须回退到 rune 边界
	out, truncated := Truncate(s, 0, 1000)
	if !truncated {
		t.Fatal("应发生截断")
	}
	if len(out) > 1200 {
		t.Fatalf("输出仍过大: %d bytes", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("截断后不是合法 UTF-8")
	}
}

func TestTruncateNoop(t *testing.T) {
	s := "short\ntext"
	out, truncated := Truncate(s, 100, 1000)
	if truncated || out != s {
		t.Fatalf("短内容不应截断: %q %v", out, truncated)
	}
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

func TestPolicyRejectsEscape(t *testing.T) {
	p := testPolicy(t)
	cases := []string{"../../etc/passwd", "../outside.txt", "a/../../b"}
	for _, c := range cases {
		if _, err := p.Resolve(c); err == nil {
			t.Fatalf("应拒绝越界路径 %q", c)
		}
	}
}

func TestPolicyAllowsInside(t *testing.T) {
	p := testPolicy(t)
	abs, err := p.Resolve("sub/dir/file.go")
	if err != nil {
		t.Fatalf("合法路径被拒: %v", err)
	}
	if !strings.HasPrefix(abs, filepath.Clean(p.Root)) {
		t.Fatalf("解析结果不在 root 内: %s", abs)
	}
}

func TestPolicyReadOnly(t *testing.T) {
	p := testPolicy(t)
	p.ReadOnly = true
	w := NewWriteTool(p)
	res, err := w.Execute(ctx_(), Args{"path": "x.txt", "content": "hi"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("只读策略下 write 应失败")
	}
}

func TestPolicyCheckCommand(t *testing.T) {
	p := testPolicy(t)
	if err := p.CheckCommand("rm -rf /"); err == nil {
		t.Fatal("危险命令未被拦截")
	}
	if err := p.CheckCommand("ls -la"); err != nil {
		t.Fatalf("正常命令被拦截: %v", err)
	}
}

// ---------------------------------------------------------------------------
// read / write / edit
// ---------------------------------------------------------------------------

func TestWriteReadRoundTrip(t *testing.T) {
	p := testPolicy(t)
	w, r := NewWriteTool(p), NewReadTool(p)

	res, err := w.Execute(ctx_(), Args{"path": "a/b/c.txt", "content": "hello\nworld\n"})
	if err != nil || res.IsError {
		t.Fatalf("write 失败: %v %s", err, res.Text())
	}
	if _, err := os.Stat(filepath.Join(p.Root, "a/b/c.txt")); err != nil {
		t.Fatalf("文件未创建: %v", err)
	}

	out, err := r.Execute(ctx_(), Args{"path": "a/b/c.txt"})
	if err != nil || out.IsError {
		t.Fatalf("read 失败: %v %s", err, out.Text())
	}
	text := out.Text()
	if !strings.Contains(text, "hello") || !strings.Contains(text, "world") {
		t.Fatalf("内容缺失: %q", text)
	}
	if !strings.Contains(text, "1|") {
		t.Fatalf("缺少行号: %q", text)
	}
}

func TestReadImage(t *testing.T) {
	p := testPolicy(t)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	path := filepath.Join(p.Root, "x.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewReadTool(p).Execute(ctx_(), Args{"path": "x.png"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Kind != KindImage {
		t.Fatalf("图片未按 image 块返回: %+v", res.Content)
	}
	if res.Content[0].MIMEType != "image/png" {
		t.Fatalf("mime 错误: %s", res.Content[0].MIMEType)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	p := testPolicy(t)
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	os.WriteFile(filepath.Join(p.Root, "n.txt"), []byte(b.String()), 0o644)

	res, _ := NewReadTool(p).Execute(ctx_(), Args{"path": "n.txt", "offset": 10, "limit": 5})
	text := res.Text()
	if !strings.Contains(text, "L10") {
		t.Fatalf("offset 未生效: %q", text)
	}
	if strings.Contains(text, "L16") {
		t.Fatalf("limit 未生效: %q", text)
	}
	if !res.Truncated {
		t.Fatal("还有剩余行时应标记 truncated")
	}
}

func TestEditUniqueAndApplies(t *testing.T) {
	p := testPolicy(t)
	path := filepath.Join(p.Root, "e.go")
	os.WriteFile(path, []byte("package x\n\nfunc a() {}\n\nfunc b() {}\n"), 0o644)

	e := NewEditTool(p)
	res, err := e.Execute(ctx_(), Args{
		"path": "e.go",
		"edits": []any{
			map[string]any{"oldText": "func a() {}", "newText": "func a() { println(1) }"},
			map[string]any{"oldText": "func b() {}", "newText": "func b() { println(2) }"},
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("edit 失败: %v %s", err, res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "println(1)") || !strings.Contains(string(data), "println(2)") {
		t.Fatalf("两处修改未都生效:\n%s", data)
	}
}

func TestEditRejectsAmbiguous(t *testing.T) {
	p := testPolicy(t)
	path := filepath.Join(p.Root, "dup.txt")
	os.WriteFile(path, []byte("x\nx\n"), 0o644)
	res, _ := NewEditTool(p).Execute(ctx_(), Args{"path": "dup.txt", "edits": []any{
		map[string]any{"oldText": "x", "newText": "y"},
	}})
	if !res.IsError {
		t.Fatal("不唯一的 oldText 应被拒绝")
	}
	if !strings.Contains(res.Text(), "2 次") {
		t.Fatalf("错误信息应说明出现次数: %s", res.Text())
	}
}

func TestEditRejectsMissing(t *testing.T) {
	p := testPolicy(t)
	os.WriteFile(filepath.Join(p.Root, "m.txt"), []byte("abc\n"), 0o644)
	res, _ := NewEditTool(p).Execute(ctx_(), Args{"path": "m.txt", "edits": []any{
		map[string]any{"oldText": "zzz", "newText": "y"},
	}})
	if !res.IsError {
		t.Fatal("找不到 oldText 应报错")
	}
}

// ---------------------------------------------------------------------------
// ls / glob / grep
// ---------------------------------------------------------------------------

func seedTree(t *testing.T, p Policy) {
	t.Helper()
	files := map[string]string{
		"main.go":                     "package main\nfunc main() {}\n",
		"pkg/util.go":                 "package pkg\n// TODO: 优化\nfunc Util() {}\n",
		"pkg/util_test.go":            "package pkg\nfunc TestUtil(t *testing.T) {}\n",
		"docs/readme.md":              "# 标题\nTODO: 写文档\n",
		"node_modules/skip/should.go": "package skip\n",
	}
	for rel, content := range files {
		full := filepath.Join(p.Root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLSTool(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)
	res, err := NewLSTool(p).Execute(ctx_(), Args{"path": "."})
	if err != nil || res.IsError {
		t.Fatalf("ls 失败: %v %s", err, res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "main.go") || !strings.Contains(text, "pkg/") {
		t.Fatalf("缺条目: %q", text)
	}
	if strings.Contains(text, "node_modules") {
		t.Fatal("默认应跳过 node_modules")
	}
}

func TestGlobDoubleStar(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)

	res, err := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "**/*.go"})
	if err != nil || res.IsError {
		t.Fatalf("glob 失败: %v %s", err, res.Text())
	}
	text := res.Text()
	for _, want := range []string{"main.go", "util.go", "util_test.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺少 %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "node_modules") {
		t.Fatal("应跳过 node_modules")
	}
	if strings.Contains(text, "readme.md") {
		t.Fatal("模式 **/*.go 不应匹配 .md")
	}
}

func TestGlobSpecificDir(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)
	res, _ := NewGlobTool(p).Execute(ctx_(), Args{"pattern": "pkg/*_test.go"})
	if !strings.Contains(res.Text(), "util_test.go") {
		t.Fatalf("未匹配到: %s", res.Text())
	}
	if strings.Contains(res.Text(), "util.go") {
		t.Fatalf("误匹配: %s", res.Text())
	}
}

func TestGrepFindsWithLineNumbers(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)
	res, err := NewGrepTool(p).Execute(ctx_(), Args{"pattern": "TODO"})
	if err != nil || res.IsError {
		t.Fatalf("grep 失败: %v %s", err, res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "util.go") || !strings.Contains(text, "readme.md") {
		t.Fatalf("应匹配多个文件:\n%s", text)
	}
	if !strings.Contains(text, ":2:") {
		t.Fatalf("缺少行号:\n%s", text)
	}
}

func TestGrepGlobFilterAndCase(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)
	res, _ := NewGrepTool(p).Execute(ctx_(), Args{"pattern": "todo", "glob": "*.md", "ignoreCase": true})
	text := res.Text()
	if !strings.Contains(text, "readme.md") {
		t.Fatalf("应匹配 md:\n%s", text)
	}
	if strings.Contains(text, "util.go") {
		t.Fatalf("glob 过滤失效:\n%s", text)
	}
}

func TestGrepInvalidRegex(t *testing.T) {
	p := testPolicy(t)
	res, _ := NewGrepTool(p).Execute(ctx_(), Args{"pattern": "([unclosed"})
	if !res.IsError {
		t.Fatal("非法正则应报错而不是 panic")
	}
}

func TestGrepNoMatch(t *testing.T) {
	p := testPolicy(t)
	seedTree(t, p)
	res, _ := NewGrepTool(p).Execute(ctx_(), Args{"pattern": "不存在的字符串XYZ"})
	if res.IsError {
		t.Fatal("无匹配不应是错误")
	}
	if !strings.Contains(res.Text(), "没有匹配") {
		t.Fatalf("提示缺失: %s", res.Text())
	}
}

// ---------------------------------------------------------------------------
// todo
// ---------------------------------------------------------------------------

func TestTodoLifecycle(t *testing.T) {
	td := NewTodoTool()
	res, _ := td.Execute(ctx_(), Args{
		"action": "replace",
		"todos": []any{
			map[string]any{"content": "写代码", "status": "in_progress", "priority": "high"},
			map[string]any{"content": "写测试"},
		},
	})
	if !strings.Contains(res.Text(), "写代码") {
		t.Fatalf("清单未建立: %s", res.Text())
	}
	res, _ = td.Execute(ctx_(), Args{"action": "update", "index": 1, "status": "completed"})
	if !strings.Contains(res.Text(), "[x] 写测试") {
		t.Fatalf("状态未更新: %s", res.Text())
	}
	res, _ = td.Execute(ctx_(), Args{"action": "update", "index": 9, "status": "completed"})
	if !res.IsError {
		t.Fatal("越界 index 应报错")
	}
}

// ---------------------------------------------------------------------------
// DefaultRegistry
// ---------------------------------------------------------------------------

func TestDefaultRegistry(t *testing.T) {
	p := testPolicy(t)
	r, mgr := DefaultRegistry(Options{Policy: p, MaxParallel: 4})
	if mgr == nil {
		t.Fatal("BashManager 为空")
	}
	names := r.Names()
	for _, want := range []string{"read", "write", "edit", "ls", "glob", "grep", "bash", "bash_output", "bash_kill", "parallel", "todo", "web_search", "web_fetch", "delegate"} {
		if _, ok := r.Get(want); !ok {
			t.Fatalf("缺少工具 %q（现有 %v）", want, names)
		}
	}
	// web 与 delegate 未注入 provider 时应优雅报错，而不是 panic。
	for _, name := range []string{"web_search", "web_fetch", "delegate"} {
		tool, _ := r.Get(name)
		res, err := tool.Execute(ctx_(), Args{"query": "x", "url": "https://example.com", "task": "x"})
		if err != nil {
			t.Fatalf("%s 返回 Go error: %v", name, err)
		}
		if !res.IsError {
			t.Fatalf("%s 未配置时应返回错误结果", name)
		}
	}
	// specs 可用于喂给 LLM
	specs := r.Specs()
	if len(specs) != r.Len() {
		t.Fatalf("specs %d != tools %d", len(specs), r.Len())
	}
	for _, s := range specs {
		if s.Name == "" || s.Description == "" || s.Parameters == nil {
			t.Fatalf("spec 不完整: %+v", s)
		}
		if s.Parameters["type"] != "object" {
			t.Fatalf("%s 的 schema 不是 object", s.Name)
		}
	}
}

func TestDefaultRegistryDisable(t *testing.T) {
	p := testPolicy(t)
	r, _ := DefaultRegistry(Options{Policy: p, Disable: []string{"bash", "delegate"}})
	if _, ok := r.Get("bash"); ok {
		t.Fatal("bash 应被禁用")
	}
	if _, ok := r.Get("delegate"); ok {
		t.Fatal("delegate 应被禁用")
	}
}
