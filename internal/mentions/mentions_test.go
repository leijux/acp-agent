package mentions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func paths(toks []Token) []string {
	var out []string
	for _, t := range toks {
		out = append(out, t.Path)
	}
	return out
}

func TestParseBasics(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"行首", "@a.go", []string{"a.go"}},
		{"空白后", "看下 @src/a.go 里的问题", []string{"src/a.go"}},
		{"括号后", "(@x.go)", []string{"x.go"}},
		{"行尾", "改一下 @main.go", []string{"main.go"}},
		{"邮箱不误判", "a@b.com 是邮箱", nil},
		{"句柄不误判", "foo@bar", nil},
		{"转义", `\@literal 不是路径`, nil},
		{"中文逗号终止", "@a.go，然后呢", []string{"a.go"}},
		{"目录", "@src/ 里", []string{"src/"}},
		{"连续两个", "@a.go @b.go", []string{"a.go", "b.go"}},
		{"带引号", `@"a b.txt" 看一下`, []string{"a b.txt"}},
		{"windows 路径", `@C:\proj\a.go`, []string{`C:\proj\a.go`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := paths(Parse(c.text))
			if len(got) != len(c.want) {
				t.Fatalf("Parse(%q) = %v, want %v", c.text, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Parse(%q) = %v, want %v", c.text, got, c.want)
				}
			}
		})
	}
}

// 行号后缀要剥出来（v1 只透传，不裁剪内容），但不能把 Windows 盘符当行号。
func TestParseLineRange(t *testing.T) {
	toks := Parse("看 @main.go:10-40 和 @a.go:7")
	if len(toks) != 2 {
		t.Fatalf("token 数 = %d: %+v", len(toks), toks)
	}
	if toks[0].Path != "main.go" || toks[0].LineRange != "10-40" {
		t.Errorf("toks[0] = %+v", toks[0])
	}
	if toks[1].Path != "a.go" || toks[1].LineRange != "7" {
		t.Errorf("toks[1] = %+v", toks[1])
	}
	if got := Parse(`@C:\x\y.go`); len(got) != 1 || got[0].LineRange != "" {
		t.Errorf("盘符被当行号: %+v", got)
	}
}

func TestExpandFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")

	res := Expand(Options{Root: root}, "看下 @main.go 的问题")
	if len(res.Attachments) != 1 {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	a := res.Attachments[0]
	if a.Kind != "file" || a.Path != "main.go" || a.Skipped != "" {
		t.Fatalf("附件字段不对: %+v", a)
	}
	if !strings.HasPrefix(res.Text, "<attachments>\n<file path=\"main.go\"") {
		t.Errorf("注入块形状不对:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "package main") {
		t.Errorf("没注入内容:\n%s", res.Text)
	}
	// 正文原样保留在附件块之后（含 @token 原文）
	if !strings.Contains(res.Text, "</attachments>\n\n看下 @main.go 的问题") {
		t.Errorf("正文被改动:\n%s", res.Text)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("不该有警告: %v", res.Warnings)
	}
}

func TestExpandDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.go", "a")
	writeFile(t, root, "src/sub/b.go", "b")
	writeFile(t, root, "src/node_modules/skip.js", "x")

	res := Expand(Options{Root: root}, "@src/ 里有什么")
	if len(res.Attachments) != 1 || res.Attachments[0].Kind != "dir" {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	if !strings.Contains(res.Text, "sub/") {
		t.Errorf("子目录应以 / 结尾:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "node_modules") {
		t.Errorf("不该列 node_modules:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "skip.js") {
		t.Errorf("目录只列一层，不该进子目录:\n%s", res.Text)
	}
	if res.Attachments[0].Entries != 2 {
		t.Errorf("Entries = %d, want 2", res.Attachments[0].Entries)
	}
}

func TestExpandBinarySkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "img.bin", string([]byte{0x89, 'P', 0x00, 0x01}))
	res := Expand(Options{Root: root}, "@img.bin")
	if len(res.Attachments) != 1 || res.Attachments[0].Skipped != "binary" {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	if !strings.Contains(res.Text, `skipped="binary"`) {
		t.Errorf("缺占位符:\n%s", res.Text)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "二进制") {
		t.Errorf("警告 = %v", res.Warnings)
	}
}

func TestExpandTruncated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "big.txt", strings.Repeat("x", 100))
	res := Expand(Options{Root: root, MaxFileBytes: 8}, "@big.txt")
	if !strings.Contains(res.Text, `truncated="8"`) {
		t.Errorf("缺 truncated 属性:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "已截断") {
		t.Errorf("缺截断说明:\n%s", res.Text)
	}
	if res.Attachments[0].Truncated != 8 {
		t.Errorf("Truncated = %d", res.Attachments[0].Truncated)
	}
}

func TestExpandBudget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "a")
	writeFile(t, root, "b.txt", "b")
	res := Expand(Options{Root: root, MaxFiles: 1}, "@a.txt @b.txt")
	if len(res.Attachments) != 2 {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	if res.Attachments[1].Skipped != "budget" {
		t.Errorf("第二个文件应因预算跳过: %+v", res.Attachments[1])
	}
	if !strings.Contains(res.Text, `skipped="budget"`) {
		t.Errorf("缺预算占位符:\n%s", res.Text)
	}
}

// 剩余配额只剩几字节时不再塞半个文件（那只是噪音）。
func TestExpandBudgetSliceTooSmall(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", strings.Repeat("a", 4000))
	writeFile(t, root, "b.txt", strings.Repeat("b", 60))
	// 预算 5000：第一个文件（4000B）能完整进来，剩下的 1000B 不够第二个。
	res := Expand(Options{Root: root, MaxTotalBytes: 5000}, "@a.txt @b.txt")
	if len(res.Attachments) != 2 {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	if res.Attachments[0].Skipped != "" {
		t.Errorf("第一个文件应注入: %+v", res.Attachments[0])
	}
	if res.Attachments[1].Skipped != "budget" {
		t.Errorf("配额不够时应整体跳过: %+v", res.Attachments[1])
	}
}

// 目录清单也吃总预算。
func TestExpandDirCountsTowardBudget(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFile(t, root, filepath.Join("d", strings.Repeat("x", 40)+string(rune('a'+i))), "x")
	}
	writeFile(t, root, "a.txt", strings.Repeat("a", 50))
	// 预算只够放一个：清单先来，后面那个文件就只能跳过。
	res := Expand(Options{Root: root, MaxTotalBytes: 300}, "@d @a.txt")
	if len(res.Attachments) != 2 {
		t.Fatalf("附件 = %+v", res.Attachments)
	}
	if res.Attachments[1].Skipped != "budget" {
		t.Errorf("清单应吃掉大部分预算，文件应跳过: %+v", res.Attachments)
	}
}

func TestExpandMissingAndOutside(t *testing.T) {
	root := t.TempDir()
	res := Expand(Options{Root: root}, "@nope.go @../escape.go")
	if res.Attachments[0].Skipped != "missing" {
		t.Errorf("missing: %+v", res.Attachments[0])
	}
	if res.Attachments[1].Skipped != "outside" {
		t.Errorf("outside: %+v", res.Attachments[1])
	}
	if len(res.Warnings) != 2 {
		t.Errorf("警告 = %v", res.Warnings)
	}
}

func TestExpandDedupe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello")
	res := Expand(Options{Root: root}, "@a.txt 和 @a.txt")
	if len(res.Attachments) != 1 {
		t.Fatalf("应去重: %+v", res.Attachments)
	}
	if n := strings.Count(res.Text, "<file path="); n != 1 {
		t.Errorf("注入块数 = %d, want 1", n)
	}
}

func TestExpandEscape(t *testing.T) {
	root := t.TempDir()
	res := Expand(Options{Root: root}, `联系 \@someone`)
	if len(res.Attachments) != 0 {
		t.Fatalf("转义不该展开: %+v", res.Attachments)
	}
	if res.Text != "联系 @someone" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestExpandNoToken(t *testing.T) {
	res := Expand(Options{Root: t.TempDir()}, "普通的一句话")
	if res.Text != "普通的一句话" || len(res.Attachments) != 0 {
		t.Errorf("不该改动: %+v", res)
	}
}

func TestComplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.go", "a")
	writeFile(t, root, "src/app/main.go", "m")
	writeFile(t, root, "src2/x.go", "x")
	writeFile(t, root, "node_modules/skip.js", "s")

	line := "@src/ap"
	cands, start, end := Complete(root, line, len(line), 50)
	if len(cands) != 1 || cands[0] != "src/app/" {
		t.Fatalf("cands = %v", cands)
	}
	if start != 1 || end != len(line) {
		t.Errorf("替换区间 = [%d,%d)", start, end)
	}

	line = "@src/"
	cands, _, _ = Complete(root, line, len(line), 50)
	joined := strings.Join(cands, ",")
	if !strings.Contains(joined, "src/a.go") || !strings.Contains(joined, "src/app/") {
		t.Errorf("cands = %v", cands)
	}
	if strings.Contains(joined, "src2") {
		t.Errorf("不该跨目录: %v", cands)
	}

	// @ 后面为空 → 列根目录，且跳过 node_modules
	line = "@"
	cands, _, _ = Complete(root, line, len(line), 50)
	if strings.Contains(strings.Join(cands, ","), "node_modules") {
		t.Errorf("根目录不该列 node_modules: %v", cands)
	}

	// 光标不在 token 末尾 / 没有 token → 不给候选
	if cands, _, _ := Complete(root, "@src/ap x", 8, 10); cands != nil {
		t.Errorf("光标不在 token 末尾时不该补全: %v", cands)
	}
	if cands, _, _ := Complete(root, "hello", 5, 10); cands != nil {
		t.Errorf("没有 token 时不该补全: %v", cands)
	}
}

func TestCommonPrefix(t *testing.T) {
	if got := CommonPrefix([]string{"src/a.go", "src/app/"}); got != "src/a" {
		t.Errorf("CommonPrefix = %q", got)
	}
	if got := CommonPrefix([]string{"a", "b"}); got != "" {
		t.Errorf("CommonPrefix = %q", got)
	}
	if got := CommonPrefix(nil); got != "" {
		t.Errorf("CommonPrefix(nil) = %q", got)
	}
}
