package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEditFile(t *testing.T, content string) (Policy, string) {
	t.Helper()
	p := testPolicy(t)
	path := filepath.Join(p.Root, "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p, path
}

func editOnce(t *testing.T, p Policy, oldText, newText string) Result {
	t.Helper()
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path":  "f.txt",
		"edits": []any{map[string]any{"oldText": oldText, "newText": newText}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// ---------- CRLF / BOM 归一 ----------

// TestEditCRLFFileWithLFOldText Windows 文件是 CRLF、模型给的是 LF，
// 必须能改中，且写回后仍是 CRLF。
func TestEditCRLFFileWithLFOldText(t *testing.T) {
	p, path := writeEditFile(t, "a\r\nb\r\nc\r\n")
	res := editOnce(t, p, "b\n", "B\n")
	if res.IsError {
		t.Fatalf("CRLF 文件匹配失败: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if got := string(data); got != "a\r\nB\r\nc\r\n" {
		t.Fatalf("写回内容 = %q，期望全 CRLF 且 b→B", got)
	}
}

// TestEditPreservesBOM 带 BOM 的文件：匹配时剥掉，写回时补上。
func TestEditPreservesBOM(t *testing.T) {
	p, path := writeEditFile(t, utf8BOM+"hello\n")
	res := editOnce(t, p, "hello", "world")
	if res.IsError {
		t.Fatalf("BOM 文件编辑失败: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), utf8BOM) {
		t.Fatalf("BOM 丢了: %q", data)
	}
	if !strings.Contains(string(data), "world") {
		t.Fatalf("内容没改: %q", data)
	}
}

// TestEditOldTextWithCRLF 反过来：文件是 LF、模型给 CRLF 的 oldText 也要能中。
func TestEditOldTextWithCRLF(t *testing.T) {
	p, path := writeEditFile(t, "x\ny\n")
	res := editOnce(t, p, "x\r\ny\r\n", "X\nY\n")
	if res.IsError {
		t.Fatalf("CRLF oldText 在 LF 文件上失败: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if got := string(data); got != "X\nY\n" {
		t.Fatalf("内容 = %q", got)
	}
}

// ---------- 模糊匹配 ----------

// TestEditFuzzyTrailingWhitespace 行尾空白差异。
func TestEditFuzzyTrailingWhitespace(t *testing.T) {
	p, path := writeEditFile(t, "func a() {}   \nfunc b() {}\n")
	res := editOnce(t, p, "func a() {}\n", "func a() { println(1) }\n")
	if res.IsError {
		t.Fatalf("行尾空白容错失败: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "模糊匹配") {
		t.Errorf("结果里应说明用了模糊匹配: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "println(1)") {
		t.Fatalf("内容 = %q", data)
	}
	// 未触及的行必须原样保留（包括行尾空白）
	if !strings.Contains(string(data), "func b() {}\n") {
		t.Fatalf("未触及行被改动了: %q", data)
	}
}

// TestEditFuzzySmartQuotesAndDashes 智能引号 / en-dash / NBSP / 全角空格：
// 文件里是「排版过的」字符，模型给的是 ASCII，仍然要能改中。
func TestEditFuzzySmartQuotesAndDashes(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		oldText string
		newText string
		want    string
	}{
		{"智能双引号", "s := \u201chello\u201d\n", `s := "hello"`, `s := "hi"`, "hi"},
		{"en-dash", "x := a\u2013b\n", "x := a-b", "x := a-b-c", "a-b-c"},
		{"NBSP", "k := \u00a0value\n", "k :=  value", "k :=  VALUE", "VALUE"},
		{"全角空格", "n := \u3000wide\n", "n :=  wide", "n :=  WIDE", "WIDE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, path := writeEditFile(t, c.file)
			res := editOnce(t, p, c.oldText, c.newText)
			if res.IsError {
				t.Fatalf("模糊匹配失败: %s", res.Text())
			}
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), c.want) {
				t.Fatalf("内容 = %q，期望含 %q", data, c.want)
			}
		})
	}
}

// TestEditFuzzyMultiline 跨行模糊匹配。
func TestEditFuzzyMultiline(t *testing.T) {
	p, path := writeEditFile(t, "if x {   \n  y()   \n}\n")
	res := editOnce(t, p, "if x {\n  y()\n}", "if x {\n  z()\n}")
	if res.IsError {
		t.Fatalf("跨行模糊匹配失败: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "z()") || strings.Contains(string(data), "y()") {
		t.Fatalf("内容 = %q", data)
	}
}

// TestEditFuzzyAmbiguous 模糊匹配到多处时必须拒绝。
//
// 文件里两行的差异只在行尾空白，oldText 精确匹配不中、模糊匹配中两处。
func TestEditFuzzyAmbiguous(t *testing.T) {
	p, _ := writeEditFile(t, "a   \na\t\n")
	res := editOnce(t, p, "a\n", "b\n")
	if !res.IsError {
		t.Fatalf("多处模糊匹配应报错: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "不唯一") {
		t.Fatalf("错误信息不对: %s", res.Text())
	}
}

// TestEditStillExactWhenPossible 能精确匹配时不要走模糊路径（不打标记）。
func TestEditStillExactWhenPossible(t *testing.T) {
	p, _ := writeEditFile(t, "abc\n")
	res := editOnce(t, p, "abc", "abcd")
	if res.IsError {
		t.Fatalf("精确匹配失败: %s", res.Text())
	}
	if strings.Contains(res.Text(), "模糊匹配") {
		t.Fatalf("精确匹配不该标成模糊: %s", res.Text())
	}
}

// TestEditFuzzyNormalizationUnit 归一函数的单元断言。
func TestEditFuzzyNormalizationUnit(t *testing.T) {
	got := normalizeForFuzzyMatch("\u201cquoted\u201d \u2014 a\u00a0b   \n\tx  ")
	want := `"quoted" - a b` + "\n\tx"
	if got != want {
		t.Fatalf("normalizeForFuzzyMatch = %q，期望 %q", got, want)
	}
	// 行首缩进不能被吃掉
	if !strings.Contains(got, "\tx") {
		t.Fatalf("行首缩进被改了: %q", got)
	}
}

func TestEditLineEndingDetect(t *testing.T) {
	cases := map[string]string{
		"a\nb\n":     "\n",
		"a\r\nb\r\n": "\r\n",
		"a\nb\r\n":   "\n",
		"none":       "\n",
	}
	for in, want := range cases {
		if got := detectLineEnding(in); got != want {
			t.Errorf("detectLineEnding(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------- 参数容错 ----------

// TestEditAcceptsJSONStringEdits 有的模型把 edits 序列化成字符串。
func TestEditAcceptsJSONStringEdits(t *testing.T) {
	p, path := writeEditFile(t, "abc\n")
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path":  "f.txt",
		"edits": `[{"oldText":"abc","newText":"XYZ"}]`,
	})
	if err != nil || res.IsError {
		t.Fatalf("JSON 字符串 edits 失败: %v %s", err, res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "XYZ") {
		t.Fatalf("内容 = %q", data)
	}
}

// TestEditAcceptsSingleObjectEdits edits 是单个对象（没包成数组）。
func TestEditAcceptsSingleObjectEdits(t *testing.T) {
	p, path := writeEditFile(t, "abc\n")
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path":  "f.txt",
		"edits": map[string]any{"oldText": "abc", "newText": "OBJ"},
	})
	if err != nil || res.IsError {
		t.Fatalf("单对象 edits 失败: %v %s", err, res.Text())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "OBJ") {
		t.Fatalf("内容 = %q", data)
	}
}

// TestEditMergesLegacyFlatWithEdits 扁平参数与 edits 数组同时给出时并入，而不是二选一。
func TestEditMergesLegacyFlatWithEdits(t *testing.T) {
	p, path := writeEditFile(t, "a\nb\n")
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path":    "f.txt",
		"edits":   []any{map[string]any{"oldText": "a", "newText": "A"}},
		"oldText": "b",
		"newText": "B",
	})
	if err != nil || res.IsError {
		t.Fatalf("混合参数失败: %v %s", err, res.Text())
	}
	data, _ := os.ReadFile(path)
	if got := string(data); got != "A\nB\n" {
		t.Fatalf("内容 = %q，期望 A\\nB\\n", got)
	}
}

// TestEditRejectsBadEditsType 真正非法的类型仍要报错，不能静默吞掉。
func TestEditRejectsBadEditsType(t *testing.T) {
	p, _ := writeEditFile(t, "abc\n")
	res, _ := NewEditTool(p).Execute(ctx_(), Args{
		"path":  "f.txt",
		"edits": 12345,
	})
	if !res.IsError {
		t.Fatalf("非法 edits 类型应报错: %s", res.Text())
	}
}

// ---------- 重叠预检 ----------

// TestEditRejectsOverlappingEdits 回归：「第二个 oldText 落在第一个 newText 内部」
// 这种情况增量匹配会静默命中并产出模型没想要的结果，必须在预检阶段拒绝。
func TestEditRejectsOverlappingEdits(t *testing.T) {
	p, path := writeEditFile(t, "value = 10\n")
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "value = 10", "newText": "value = 100"},
			map[string]any{"oldText": "= 10", "newText": "= 20"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("重叠 edits 应报错，实际: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "重叠") {
		t.Fatalf("错误信息应提到重叠: %s", res.Text())
	}
	// 文件不能被改半个字
	data, _ := os.ReadFile(path)
	if string(data) != "value = 10\n" {
		t.Fatalf("报错时不应写盘: %q", data)
	}
}

// TestEditRejectsDuplicateOldText 两个 edit 用同一段 oldText 也属于重叠。
func TestEditRejectsDuplicateOldText(t *testing.T) {
	p, _ := writeEditFile(t, "abc\n")
	res, _ := NewEditTool(p).Execute(ctx_(), Args{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "abc", "newText": "x"},
			map[string]any{"oldText": "abc", "newText": "y"},
		},
	})
	if !res.IsError || !strings.Contains(res.Text(), "重叠") {
		t.Fatalf("重复 oldText 应判为重叠: %s", res.Text())
	}
}

// TestEditFuzzyOverlapDetected 模糊定位到的区间同样参与重叠判定。
func TestEditFuzzyOverlapDetected(t *testing.T) {
	// 文件里是智能引号 + 行尾空白，两个 oldText 用 ASCII 写法但互相重叠
	p, _ := writeEditFile(t, "msg := \u201chello world\u201d   \n")
	res, _ := NewEditTool(p).Execute(ctx_(), Args{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": `msg := "hello world"`, "newText": `msg := "hi"`},
			map[string]any{"oldText": `"hello world"`, "newText": `"HELLO"`},
		},
	})
	if !res.IsError || !strings.Contains(res.Text(), "重叠") {
		t.Fatalf("模糊定位的重叠未被检出: %s", res.Text())
	}
}

// TestEditAllowsChainEdits 链式写法（第二个 oldText 只在第一个改完之后出现）必须继续可用。
func TestEditAllowsChainEdits(t *testing.T) {
	p, path := writeEditFile(t, "x = 1\n")
	res, err := NewEditTool(p).Execute(ctx_(), Args{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "x = 1", "newText": "x = 2"},
			map[string]any{"oldText": "x = 2", "newText": "x = 3"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("链式 edits 不该被重叠检测挡住: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "x = 3\n" {
		t.Fatalf("内容 = %q", data)
	}
}

// TestEditAllowsAdjacentEdits 相邻但不相交的 edits 正常。
func TestEditAllowsAdjacentEdits(t *testing.T) {
	p, path := writeEditFile(t, "aaa bbb\n")
	res, _ := NewEditTool(p).Execute(ctx_(), Args{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"oldText": "aaa", "newText": "AAA"},
			map[string]any{"oldText": " bbb", "newText": " BBB"},
		},
	})
	if res.IsError {
		t.Fatalf("相邻 edits 被误判重叠: %s", res.Text())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "AAA BBB\n" {
		t.Fatalf("内容 = %q", data)
	}
}

// TestCheckEditOverlapUnit 预检函数本身：交错区间也要检出（排序后相邻比较）。
func TestCheckEditOverlapUnit(t *testing.T) {
	orig := "0123456789"
	// [0,4) 与 [2,6) 相交
	if err := checkEditOverlap(orig, []string{"0123", "2345"}); err == nil {
		t.Fatal("相交区间未报错")
	}
	// [0,3) 与 [5,8) 不相交
	if err := checkEditOverlap(orig, []string{"012", "567"}); err != nil {
		t.Fatalf("不相交区间误报: %v", err)
	}
	// 原文里找不到的（链式）不参与判定
	if err := checkEditOverlap(orig, []string{"012", "zzz"}); err != nil {
		t.Fatalf("找不到的 oldText 不该参与重叠判定: %v", err)
	}
	// 首尾相邻（[0,3) 与 [3,6)）不算重叠
	if err := checkEditOverlap(orig, []string{"012", "345"}); err != nil {
		t.Fatalf("首尾相邻被误判重叠: %v", err)
	}
}
