package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// 两行内容的模型（第一行故意带 ANSI 颜色，确认切列不会切断转义序列）。
func selModel(t *testing.T, lines ...string) *Model {
	t.Helper()
	m, _ := newTestModel(40, 20)
	for _, ln := range lines {
		send(m, LineMsg{Kind: KindText, Text: ln})
	}
	m.rebuild()
	return m
}

func mousePress(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y}
}

func mouseMotion(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: x, Y: y}
}

func mouseRelease(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: x, Y: y}
}

// 同一行内拖选：列是显示列，不是 rune 下标。
func TestSelectionSpanSameRow(t *testing.T) {
	s := selection{active: true, anchor: selPoint{row: 2, col: 5}, focus: selPoint{row: 2, col: 2}}
	from, to, ok := s.span(2)
	if !ok || from != 2 || to != 5 {
		t.Fatalf("span = %d,%d,%v，期望 2,5,true（反向拖选也要规范化）", from, to, ok)
	}
	if _, _, ok := s.span(1); ok {
		t.Fatal("选区外的行不该有 span")
	}
}

// 跨行：首行从锚点列到行尾，中间整行，末行到焦点列。
func TestSelectionSpanMultiRow(t *testing.T) {
	s := selection{active: true, anchor: selPoint{row: 1, col: 3}, focus: selPoint{row: 3, col: 2}}
	if from, to, ok := s.span(1); !ok || from != 3 || to != -1 {
		t.Fatalf("首行 span = %d,%d,%v，期望 3,-1", from, to, ok)
	}
	if from, to, ok := s.span(2); !ok || from != 0 || to != -1 {
		t.Fatalf("中间行 span = %d,%d,%v，期望 0,-1", from, to, ok)
	}
	if from, to, ok := s.span(3); !ok || from != 0 || to != 2 {
		t.Fatalf("末行 span = %d,%d,%v，期望 0,2", from, to, ok)
	}
}

// 按列取字符：CJK 占两列，切在字中间也不能切出半个字。
func TestSelectionTextCJKAndANSI(t *testing.T) {
	m := selModel(t, "\x1b[36mabc中文def\x1b[0m")
	m.sel = selection{armed: true, active: true,
		anchor: selPoint{row: 0, col: 3}, focus: selPoint{row: 0, col: 7}}

	if got := m.selectionText(); got != "中文" {
		t.Fatalf("选区文本 = %q，期望 中文（且不含 ANSI）", got)
	}
}

// 多行选区按行拼接，行尾空格被去掉。
func TestSelectionTextMultiRow(t *testing.T) {
	m := selModel(t, "hello   ", "world")
	m.sel = selection{armed: true, active: true,
		anchor: selPoint{row: 0, col: 2}, focus: selPoint{row: 1, col: 3}}

	if got := m.selectionText(); got != "llo\nwor" {
		t.Fatalf("选区文本 = %q，期望 \"llo\\nwor\"", got)
	}
}

// 左键拖选松开即复制：不需要 Shift，也不用终端原生选择。
func TestDragReleaseCopiesSelection(t *testing.T) {
	m := selModel(t, "abcdefgh", "第二行")
	var copied []string
	m.onCopyText = func(s string) { copied = append(copied, s) }

	send(m, mousePress(0, 1))
	send(m, mouseMotion(3, 1))
	send(m, mouseRelease(3, 1))

	if len(copied) != 1 || copied[0] != "abc" {
		t.Fatalf("复制内容 = %q，期望 [\"abc\"]", copied)
	}
	// 松开后高亮保留，让人看清复制了哪段。
	if !m.sel.active || m.sel.dragging {
		t.Fatalf("松开后选区状态 = %+v，期望 active 且非 dragging", m.sel)
	}
}

// 单击（按下就松开，没拖动）仍然保留「点思考块折叠」的老行为，不触发复制。
func TestClickWithoutDragStillTogglesThinking(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindThinking, Text: "思考内容"})
	m.rebuild()
	copied := 0
	m.onCopyText = func(string) { copied++ }

	send(m, mousePress(0, 1))
	send(m, mouseRelease(0, 1))

	if copied != 0 {
		t.Fatalf("单击不该复制，实际复制 %d 次", copied)
	}
	if !m.blocks[0].Collapsed {
		t.Fatal("单击思考块应该折叠它")
	}
}

// 从 header / 输入行开始的手势整段忽略（别把 header 上的拖动当选区）。
func TestDragFromHeaderIgnored(t *testing.T) {
	m := selModel(t, "abcdefgh")
	copied := 0
	m.onCopyText = func(string) { copied++ }

	send(m, mousePress(0, 0)) // 第 0 行是 header
	send(m, mouseMotion(4, 1))
	send(m, mouseRelease(4, 1))

	if copied != 0 {
		t.Fatalf("header 上开始的手势不该复制，实际 %d 次", copied)
	}
	if m.sel.active {
		t.Fatal("header 上开始的手势不该留下选区")
	}
}

// 拖选过程中界面上要有反显高亮。
func TestSelectionHighlightedInView(t *testing.T) {
	m := selModel(t, "abcdefgh")
	send(m, mousePress(1, 1))
	send(m, mouseMotion(4, 1))

	if !strings.Contains(m.View(), escReverse) {
		t.Fatalf("视口里没有反显高亮:\n%q", m.View())
	}
}

// 追加内容不该把选区整段废掉（流式输出一直在追加，否则流式时根本拖不动）。
// 宽度变（resize）才代表折行位置全变了，那时才清空。
func TestSelectionSurvivesAppendButNotResize(t *testing.T) {
	m := selModel(t, "abcdefgh")
	m.sel = selection{armed: true, active: true, anchor: selPoint{0, 0}, focus: selPoint{0, 3}}

	send(m, LineMsg{Kind: KindText, Text: "新的一行"})
	m.rebuild()
	if !m.sel.active {
		t.Fatal("末尾追加内容不该清掉选区")
	}
	if got := m.selectionText(); got != "abc" {
		t.Fatalf("选区内容 = %q，期望 abc（追加不影响锚点以上的行）", got)
	}

	// 窗口宽度变化：折行位置全变，选区必须作废。
	m.lastCols = 999
	m.rebuild()
	if m.sel.active {
		t.Fatal("宽度变化后选区应清空")
	}
}

// 视口滚动后 pointAt 必须把屏幕行换算成带 YOffset 的 flat 行，
// 否则滚下去以后拖选会选到别的内容。
func TestPointAtWithScrollOffset(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ { // 远多于 vpH，视口会停在底部
		lines = append(lines, fmt.Sprintf("第%d行内容", i))
	}
	m := selModel(t, lines...)
	if m.vp.YOffset == 0 {
		t.Fatal("前置条件失败：视口没有滚动")
	}

	p, ok := m.pointAt(0, 1) // 屏幕第一行正文
	if !ok {
		t.Fatal("pointAt 说这里不在正文区")
	}
	if want := m.vp.YOffset; p.row != want {
		t.Fatalf("flat 行 = %d，期望 %d（YOffset=%d）", p.row, want, m.vp.YOffset)
	}
	// 选出来的内容必须真的是那一行
	m.sel = selection{armed: true, active: true, anchor: p, focus: selPoint{row: p.row, col: 99}}
	if got := m.selectionText(); !strings.HasSuffix(got, "行内容") ||
		!strings.Contains(got, fmt.Sprintf("第%d行内容", p.row)) {
		t.Fatalf("选区内容 = %q，期望包含第%d行内容", got, p.row)
	}
}

// 列落在宽字符中间时不能切出半个字（CJK 占两列）。
func TestCutColsOnWideCharBoundary(t *testing.T) {
	m := selModel(t, "abc中文def")
	for _, c := range []struct {
		from, to int
		want     string
	}{
		{3, 5, "中"},
		{4, 6, "中"}, // 从“中”的第二列开始：ansi.Cut 向前对齐到字边界，不会切半个字
		{3, 7, "中文"},
		{5, 99, "文def"},
	} {
		m.sel = selection{armed: true, active: true,
			anchor: selPoint{row: 0, col: c.from}, focus: selPoint{row: 0, col: c.to}}
		if got := m.selectionText(); got != c.want {
			t.Errorf("列 [%d,%d) = %q，期望 %q", c.from, c.to, got, c.want)
		}
	}
}

// 输出还在流的时候也要能拖选：PartialMsg 不该把正在进行的选区清掉。
func TestDragSurvivesStreamingPartial(t *testing.T) {
	ui := New(Options{Header: "h"})
	s := ui.Sink()
	write(ui, s, "已经写完的一行\n")

	send(ui.model, mousePress(0, 1))
	send(ui.model, mouseMotion(2, 1))
	// 真实时序：流式输出每几毫秒来一条半成品
	write(ui, s, "正在输出")

	if !ui.model.sel.dragging {
		t.Fatal("流式输出把拖动状态清掉了：拖选在流式时失效")
	}
	copied := ""
	ui.OnCopyText(func(s string) { copied = s })
	send(ui.model, mouseRelease(2, 1))
	if copied == "" {
		t.Fatal("流式期间拖选没有复制任何东西")
	}
}
