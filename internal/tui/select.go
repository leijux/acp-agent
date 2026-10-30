package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// 屏上拖选：左键按下记锚点，拖动扩展，松开就交给宿主写剪贴板。
//
// 坐标有两套，不能混：
//   - 行用 flat 行号（m.flat 的下标，已经折好行，一行＝屏幕一行）；
//   - 列用「显示列」（CJK / emoji 算 2 列），因为一个 rune 不等于一列，
//     而终端里用户看到的是列。
//
// 选中的内容从 m.flat 里按列切出（ansi.Cut 不破坏转义序列），
// 复制时再 Strip 掉颜色。

type selPoint struct{ row, col int }

type selection struct {
	// armed 表示这段手势从正文区开始；从 header/输入行开始的手势整段不管。
	armed    bool
	active   bool // 有选区（松开后仍留着高亮，让用户看清复制了什么）
	dragging bool
	anchor   selPoint
	focus    selPoint
}

func (s *selection) reset() { *s = selection{} }

// clamp 把选区行夹到现有内容范围内。
//
// 流式输出只在末尾追加，锚点以上的行号不会变，所以不该整段作废；
// 先把行夹到范围内（行数还不够时），拖动才能跨着新输出继续做。
func (s *selection) clamp(rows int) {
	if !s.active {
		return
	}
	if rows <= 0 {
		s.reset()
		return
	}
	last := rows - 1
	if s.anchor.row > last {
		s.anchor.row = last
	}
	if s.focus.row > last {
		s.focus.row = last
	}
}

// empty 报告是否没选中任何字符（同一格 = 一次单击）。
func (s *selection) empty() bool { return s.anchor == s.focus }

// bounds 返回按行/列排序后的起止点。
func (s *selection) bounds() (selPoint, selPoint) {
	a, b := s.anchor, s.focus
	if b.row < a.row || (b.row == a.row && b.col < a.col) {
		a, b = b, a
	}
	return a, b
}

// span 返回第 row 行被选中的显示列区间 [from, to)；
// to 为 -1 表示到行尾（多行选区里除最后一行之外都这样）。
func (s *selection) span(row int) (from, to int, ok bool) {
	if !s.active {
		return 0, 0, false
	}
	a, b := s.bounds()
	if row < a.row || row > b.row {
		return 0, 0, false
	}
	from, to = 0, -1
	if row == a.row {
		from = a.col
	}
	if row == b.row {
		to = b.col
	}
	return from, to, true
}

// pointAt 把屏幕坐标换成 (flat 行, 显示列)。
//
// 屏幕是 0-based：第 0 行是 header，视口占第 1..vpH 行，
// 所以落在 header / 输入行 / 状态行上的鼠标事件一律不算选区。
func (m *Model) pointAt(x, y int) (selPoint, bool) {
	if y < 1 || y > m.vpH {
		return selPoint{}, false
	}
	row := m.vp.YOffset() + (y - 1)
	if row < 0 || row >= len(m.owners) {
		return selPoint{}, false
	}
	if x < 0 {
		x = 0
	}
	return selPoint{row: row, col: x}, true
}

// selectionText 把当前选区切成纯文本：按显示列切、去掉 ANSI、去掉行尾空格。
func (m *Model) selectionText() string {
	if !m.sel.active || m.sel.empty() {
		return ""
	}
	if m.needRebuild {
		m.rebuild() // 保证 m.flat 是最新的（只会把行号 clamp 到范围内）
	}
	lines := strings.Split(m.flat, "\n")
	a, b := m.sel.bounds()
	var out []string
	for row := a.row; row <= b.row && row < len(lines); row++ {
		from, to, ok := m.sel.span(row)
		if !ok {
			continue
		}
		out = append(out, strings.TrimRight(ansi.Strip(cutCols(lines[row], from, to)), " "))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// cutCols 按显示列取子串（保留 ANSI）。to < 0 表示到行尾。
func cutCols(line string, from, to int) string {
	w := ansi.StringWidth(line)
	if from < 0 {
		from = 0
	}
	if to < 0 || to > w {
		to = w
	}
	if from >= to {
		return ""
	}
	return ansi.Cut(line, from, to)
}

// highlight 给一行加上选区反显。head/tail 保持原样，只有中段反显。
func highlight(line string, from, to int) string {
	w := ansi.StringWidth(line)
	if from < 0 {
		from = 0
	}
	if to < 0 || to > w {
		to = w
	}
	if from >= to {
		return line
	}
	head := ansi.Cut(line, 0, from)
	mid := ansi.Cut(line, from, to)
	tail := ansi.Cut(line, to, w)
	// 用 27m（只关掉反显）而不是 0m：后者会把这一行原有的 dim/cyan 一起清掉，
	// 选区右侧的文字会突然变成默认颜色。
	return head + escReverse + mid + escUnreverse + tail
}

// highlightSelection 给视口里的可见行加选区高亮（rows 是屏幕顺序）。
func (m *Model) highlightSelection(rows []string) []string {
	if !m.sel.active {
		return rows
	}
	out := make([]string, len(rows))
	copy(out, rows)
	for i := range out {
		row := m.vp.YOffset() + i
		if row >= len(m.owners) {
			break
		}
		from, to, ok := m.sel.span(row)
		if !ok {
			continue
		}
		out[i] = highlight(out[i], from, to)
	}
	return out
}
