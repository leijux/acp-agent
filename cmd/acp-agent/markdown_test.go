package main

import (
	"bytes"
	"strings"
	"testing"
)

func renderMD(t *testing.T, chunks ...string) string {
	t.Helper()
	var buf bytes.Buffer
	term := &terminal{out: &buf, errOut: &buf}
	md := newMDRenderer(term)
	for _, c := range chunks {
		md.Write(c)
	}
	md.Flush()
	return buf.String()
}

func TestMDHeading(t *testing.T) {
	got := renderMD(t, "# 标题\n### 三级 (带说明)\n")
	if got != "标题\n三级 (带说明)\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMDBulletAndOrdered(t *testing.T) {
	got := renderMD(t, "- 一\n  - 二\n1. 第一\n  2) 第二\n")
	want := "• 一\n  • 二\n1. 第一\n  2. 第二\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMDInlineMarkersStrippedWithoutColor(t *testing.T) {
	got := renderMD(t, "这是 **粗体** 与 `code` 还有 [链接](https://x.y) 和 ~~删~~\n")
	want := "这是 粗体 与 code 还有 链接 (https://x.y) 和 删\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMDFenceKeepsContentVerbatim(t *testing.T) {
	src := "说明\n```go\nfunc f(*int) { /* a_b_ */ }\n```\n结尾\n"
	got := renderMD(t, src)
	if !strings.Contains(got, "  func f(*int) { /* a_b_ */ }\n") {
		t.Fatalf("代码块内容被改写: %q", got)
	}
	if strings.Contains(got, "```") {
		t.Fatalf("围栏没有被吃掉: %q", got)
	}
	// 代码块里的 * 不能被当成斜体
	if strings.Contains(got, "func f(") == false || !strings.Contains(got, "*int") {
		t.Fatalf("代码块内容丢失: %q", got)
	}
}

func TestMDFenceSurvivesChunkBoundary(t *testing.T) {
	// 逐字节喂入，围栏状态必须跨 chunk 正确。
	got := renderMD(t, "```\n", "*a*\n", "```\n", "**b**\n")
	if !strings.Contains(got, "  *a*") {
		t.Fatalf("围栏内被解析: %q", got)
	}
	if !strings.Contains(got, "b\n") || strings.Contains(got, "**b**") {
		t.Fatalf("围栏外没有解析: %q", got)
	}
}

// 流式分片不能改变最终文本。
func TestMDStreamingEquivalence(t *testing.T) {
	src := "# 标题\n\n段落有 **粗体**、`代码` 和 [链接](u)。\n\n- 项目 a\n- 项目 b\n\n```sh\necho hi\n```\n"
	whole := renderMD(t, src)

	for _, step := range []int{1, 2, 3, 7, 16} {
		var chunks []string
		for i := 0; i < len(src); i += step {
			end := i + step
			if end > len(src) {
				end = len(src)
			}
			chunks = append(chunks, src[i:end])
		}
		if got := renderMD(t, chunks...); got != whole {
			t.Fatalf("step=%d 流式结果与整块不一致\nwhole=%q\ngot  =%q", step, whole, got)
		}
	}
}

func TestMDLongParagraphFlushes(t *testing.T) {
	long := strings.Repeat("字", mdPartialFlush+50)
	got := renderMD(t, long) // 没有换行
	if len([]rune(got)) < mdPartialFlush {
		t.Fatalf("长段落没有提前刷出，获得 %d 字", len([]rune(got)))
	}
	if !strings.Contains(got, long) {
		t.Fatal("长段落内容被截断")
	}
}

func TestMDQuoteAndTable(t *testing.T) {
	got := renderMD(t, "> 引用内容\n| a | b |\n")
	if !strings.Contains(got, "引用内容") || !strings.Contains(got, "| a | b |") {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(got, "│ ") {
		t.Fatalf("引用缺少提示符: %q", got)
	}
}

func TestMDResetClearsFence(t *testing.T) {
	var buf bytes.Buffer
	term := &terminal{out: &buf, errOut: &buf}
	md := newMDRenderer(term)
	md.Write("```\ncode\n") // 围栏未闭合
	md.Reset()
	md.Write("**b**\n") // 重置后应正常解析
	if got := buf.String(); strings.Contains(got, "**b**") {
		t.Fatalf("Reset 后仍处于围栏状态: %q", got)
	}
}

// renderMDColor 用带颜色的终端渲染，才能看到表格排版。
func renderMDColor(t *testing.T, chunks ...string) string {
	t.Helper()
	var buf bytes.Buffer
	term := &terminal{out: &buf, errOut: &buf, color: true}
	md := newMDRenderer(term)
	for _, c := range chunks {
		md.Write(c)
	}
	md.Flush()
	return buf.String()
}

func renderMDColorCols(t *testing.T, cols int, chunks ...string) string {
	t.Helper()
	var buf bytes.Buffer
	term := &terminal{out: &buf, errOut: &buf, color: true, cols: cols}
	md := newMDRenderer(term)
	for _, c := range chunks {
		md.Write(c)
	}
	md.Flush()
	return buf.String()
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 'A' || s[j] > 'Z') && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestMDTableRenders(t *testing.T) {
	src := "| 工具 | 状态 |\n|---|---|\n| read | ✅ |\n| bash_kill | ❌ |\n后记\n"
	got := stripANSI(renderMDColor(t, src))
	want := "┌───────────┬──────┐\n" +
		"│ 工具      │ 状态 │\n" +
		"├───────────┼──────┤\n" +
		"│ read      │ ✅   │\n" +
		"│ bash_kill │ ❌   │\n" +
		"└───────────┴──────┘\n" +
		"后记\n"
	if got != want {
		t.Fatalf("表格排版不对\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "---|") {
		t.Fatalf("分隔行没有被吃掉: %q", got)
	}
}

func TestMDTableCellInline(t *testing.T) {
	got := stripANSI(renderMDColor(t, "| a | b |\n|---|---|\n| `code` | **粗** |\n"))
	if strings.Contains(got, "`") || strings.Contains(got, "**") {
		t.Fatalf("单元格内行内标记没渲染: %q", got)
	}
	if !strings.Contains(got, "code") || !strings.Contains(got, "粗") {
		t.Fatalf("单元格内容丢失: %q", got)
	}
}

func TestMDTableAlignRight(t *testing.T) {
	got := stripANSI(renderMDColor(t, "| l | r |\n|:--|--:|\n| a | 1 |\n| bb | 22 |\n"))
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// 右对齐：'1' 要贴在右边界，和 '22' 的个位对齐
	// 右对齐：个位要贴在同一列上，所以 "1" 后面只应有一个空格（再加边界空格）。
	// lines[0]=┌边框 lines[1]=表头 lines[2]=├分隔 lines[3..]=数据行
	if !strings.HasSuffix(lines[3], " 1 │") {
		t.Fatalf("右对齐列未靠右: %q", lines[3])
	}
	if !strings.HasSuffix(lines[4], "22 │") {
		t.Fatalf("右对齐列未靠右: %q", lines[4])
	}
}

func TestMDTableNonTTYPassesThrough(t *testing.T) {
	// 非 TTY：原样输出，方便管道消费。
	got := renderMD(t, "| a | b |\n|---|---|\n| 1 | 2 |\n")
	want := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	if got != want {
		t.Fatalf("非 TTY 不应排版\ngot %q\nwant %q", got, want)
	}
}

func TestMDTableStreamingEquivalence(t *testing.T) {
	src := "前言\n\n| 列一 | 列二 |\n|:--|--:|\n| 中文 | 1 |\n| x | 22 |\n\n结尾 **粗**\n"
	whole := renderMDColor(t, src)
	for _, step := range []int{1, 3, 8, 64} {
		var chunks []string
		for i := 0; i < len(src); i += step {
			end := i + step
			if end > len(src) {
				end = len(src)
			}
			chunks = append(chunks, src[i:end])
		}
		if got := renderMDColor(t, chunks...); got != whole {
			t.Fatalf("step=%d 流式表格与整块不一致\ngot  %q\nwant %q", step, got, whole)
		}
	}
}

func TestDisplayWidthCJK(t *testing.T) {
	cases := map[string]int{"abc": 3, "中文": 4, "a中": 3, "✅": 2, "": 0}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, 期望 %d", s, got, want)
		}
	}
}

// TestMDTableFitsTerminalWidth 表格不能超过终端宽度（否则边框折行全乱）。
func TestMDTableFitsTerminalWidth(t *testing.T) {
	src := "| 工具 | 说明 | 何时用 |\n|---|---|---|\n" +
		"| `grep` | Go 正则搜文件内容，输出 文件:行号，支持 context 上下文行与 glob 限定文件类型 | 按内容找代码 |\n" +
		"| `glob` | 按通配模式找文件，按修改时间倒序 | 按名字找文件 |\n"
	for _, cols := range []int{120, 100, 80, 60, 40} {
		out := stripANSI(renderMDColorCols(t, cols, src))
		for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if w := displayWidth(ln); w > cols {
				t.Fatalf("cols=%d 时表格宽 %d 溢出:\n%s", cols, w, out)
			}
		}
		if !strings.Contains(out, "┌") {
			t.Fatalf("cols=%d 应正常排版", cols)
		}
	}
}

// TestMDTableTruncatesLongCell 超宽列用省略号截断，且截断后不残留行内标记。
func TestMDTableTruncatesLongCell(t *testing.T) {
	long := strings.Repeat("很长的说明文字", 12)
	src := "| 列 | 说明 |\n|---|---|\n| a | " + long + " |\n"
	out := stripANSI(renderMDColorCols(t, 60, src))
	if !strings.Contains(out, "…") {
		t.Fatalf("超宽内容应被截断: %s", out)
	}
	if strings.Contains(out, long) {
		t.Fatalf("长内容没有被截断: %s", out)
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := displayWidth(ln); w > 60 {
			t.Fatalf("截断后仍溢出(%d): %q", w, ln)
		}
	}
}

// TestMDTableTooNarrowFallsBack 窄到摆不下时退回原样，而不是画折行的表。
func TestMDTableTooNarrowFallsBack(t *testing.T) {
	src := "| 工具 | 说明 | 何时用 |\n|---|---|---|\n| grep | 搜内容 | 找代码 |\n"
	got := stripANSI(renderMDColorCols(t, 12, src))
	if strings.Contains(got, "┌") {
		t.Fatalf("太窄时不应画表: %q", got)
	}
	if !strings.Contains(got, "| grep | 搜内容 | 找代码 |") {
		t.Fatalf("应原样输出: %q", got)
	}
}

// TestFitTableWidth 收窄策略：削最宽列，直到放下或全部到下限。
func TestFitTableWidth(t *testing.T) {
	got := fitTableWidth([]int{4, 40, 4}, 40)
	if tableTotalWidth(got) > 40 {
		t.Fatalf("没有收窄到 40 以内: %v (总宽 %d)", got, tableTotalWidth(got))
	}
	if got[0] != 4 || got[2] != 4 {
		t.Errorf("不该削窄列: %v", got)
	}
	if got[1] >= 40 {
		t.Errorf("宽列没有被削: %v", got)
	}
	// 每条都到下限就停手
	all := fitTableWidth([]int{tableMinCol, tableMinCol}, 5)
	if tableTotalWidth(all) <= 5 {
		t.Fatalf("不该无限削下去: %v", all)
	}
	// limit<=0 用兜底上限
	if w := fitTableWidth([]int{10}, 0); w[0] != 10 {
		t.Errorf("limit=0 时不应削窄: %v", w)
	}
}

// TestMDMeasureMatchesRender 量宽函数与渲染函数必须给出同样的可见宽度。
//
// 否则表格补空格会算错，整列错位（踩过：stripInlineMarkers 漏了 *斜体*，
// 比 inline() 多算两个星号）。
func TestMDMeasureMatchesRender(t *testing.T) {
	var buf bytes.Buffer
	term := &terminal{out: &buf, errOut: &buf, color: true}
	for _, s := range []string{
		"*斜体*", "**粗**", "__粗__", "`code`", "[文本](https://x.y)", "~~删~~", "普通",
		"a **粗** *斜* `c` 混排",
	} {
		measure := displayWidth(stripInlineMarkers(s))
		buf.Reset()
		md := newMDRenderer(term)
		md.Write(s + "\n")
		md.Flush()
		render := displayWidth(strings.TrimSuffix(stripANSI(buf.String()), "\n"))
		if measure != render {
			t.Errorf("%q 量宽 %d ≠ 渲染 %d", s, measure, render)
		}
	}
}
