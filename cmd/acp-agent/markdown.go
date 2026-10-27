package main

import (
	"strings"
)

// mdRenderer 把流式 Markdown 增量渲染成终端输出。
//
// 设计要点：
//   - 按行渲染：只有遇到 '\n' 的完整行才做行内解析，所以 `**粗体**`、行内代码
//     即使被 chunk 从中间切开也能正确渲染；
//   - 代码围栏（``` ）用状态机跨行跟踪，围栏内的内容原样保留（仅缩进 + 变暗），
//     不误判里面的 `*` 或 `_`；
//   - 超长无换行段落（模型偶尔整段一行）超过阈值时原样输出，避免一直不显示。
type mdRenderer struct {
	t   *terminal
	buf strings.Builder

	inFence   bool
	fenceLang string

	// tableRows 是攒起来待排版的表格行。表格必须先拿到整块才能算列宽，
	// 所以不能像其它块级元素那样逐行输出。
	tableRows []string

	// wrotePartial 表示当前逻辑行已经吐出了一部分（超长段落提前刷出）。
	wrotePartial bool
}

const mdPartialFlush = 400

func newMDRenderer(t *terminal) *mdRenderer { return &mdRenderer{t: t} }

// Write 追加流式片段，遇到换行就渲染整行。
func (m *mdRenderer) Write(delta string) {
	if delta == "" {
		return
	}
	m.buf.WriteString(delta)
	for {
		s := m.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		line := s[:i]
		m.buf.Reset()
		m.buf.WriteString(s[i+1:])
		m.renderLine(line)
		m.wrotePartial = false
	}
	// 无换行的长段落：先原样吐出来，避免看起来卡住。
	// 表格行例外：提前吐出半张表会打乱后续排版。
	if !m.inFence && m.buf.Len() > mdPartialFlush &&
		!strings.HasPrefix(strings.TrimSpace(m.buf.String()), "|") {
		m.t.writeRaw(m.buf.String())
		m.buf.Reset()
		m.wrotePartial = true
	}
}

// Flush 输出尚未换行的残余（turn 结束时调用）。
func (m *mdRenderer) Flush() {
	// 表格块不靠换行结束，靠“不再是表格行”结束，所以这里也要落盘。
	m.flushTable()
	if m.buf.Len() == 0 {
		return
	}
	line := m.buf.String()
	m.buf.Reset()
	if m.inFence {
		m.t.writeRaw(m.codeLine(line))
		return
	}
	if m.wrotePartial {
		// 前半段已经原样输出过，剩下的直接接上（不再做行内解析）。
		m.t.writeRaw(line)
	} else {
		m.t.writeRaw(m.inline(line))
	}
	m.wrotePartial = false
}

// pending 报告是否还有「未以换行结尾」的残余。
// 围栏内的残余不在此列：Flush 会用 codeLine 给它补上换行。
func (m *mdRenderer) pending() bool { return !m.inFence && m.buf.Len() > 0 }

// Reset 为下一轮对话清状态。
func (m *mdRenderer) Reset() {
	m.buf.Reset()
	m.inFence = false
	m.fenceLang = ""
	m.wrotePartial = false
	m.tableRows = nil
}

func (m *mdRenderer) emit(s string) { m.t.writeRaw(s) }

// codeLine 渲染代码块里的一行：缩进两格 + 变暗，不做任何行内解析。
func (m *mdRenderer) codeLine(line string) string {
	return m.t.paint(cDim, "  "+line) + "\n"
}

func (m *mdRenderer) renderLine(line string) {
	trimmed := strings.TrimSpace(line)

	if m.inFence {
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			m.inFence = false
			m.fenceLang = ""
			return
		}
		m.emit(m.codeLine(line))
		return
	}
	// 表格行：连续以 | 开头的行属于同一张表，先攒起来，
	// 等块结束（遇到非表格行或 Flush）再一次性排版。
	if strings.HasPrefix(trimmed, "|") {
		m.tableRows = append(m.tableRows, trimmed)
		return
	}
	if len(m.tableRows) > 0 {
		m.flushTable()
	}

	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		m.inFence = true
		m.fenceLang = strings.TrimSpace(strings.TrimLeft(trimmed, "`~"))
		if m.fenceLang != "" {
			m.emit(m.t.paint(cDim, "  ── "+m.fenceLang+" ──") + "\n")
		}
		return
	}

	// 空行
	if trimmed == "" {
		m.emit("\n")
		return
	}

	// 标题 # ~ ######
	if level, text, ok := markdownHeading(trimmed); ok {
		code := cBold + cCyan
		if level >= 3 {
			code = cBold
		}
		m.emit(m.t.paint(code, text) + "\n")
		return
	}

	// 分隔线 --- / *** / ___
	if isThematicBreak(trimmed) {
		m.emit(m.t.paint(cDim, strings.Repeat("─", 48)) + "\n")
		return
	}

	// 引用 >
	if rest, ok := cutPrefix(trimmed, ">"); ok {
		rest = strings.TrimSpace(rest)
		m.emit(m.t.paint(cDim, "│ ") + m.t.paint(cDim, m.inline(rest)) + "\n")
		return
	}

	// 列表项 - * +（保留缩进层级）
	if indent, rest, ok := markdownBullet(line); ok {
		m.emit(indent + m.t.paint(cCyan, "•") + " " + m.inline(rest) + "\n")
		return
	}
	if indent, num, rest, ok := markdownOrdered(line); ok {
		m.emit(indent + m.t.paint(cCyan, num+".") + " " + m.inline(rest) + "\n")
		return
	}

	// 缩进代码块（4 空格或 tab）
	if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
		m.emit(m.codeLine(line))
		return
	}

	m.emit(m.inline(line) + "\n")
}

// inline 渲染行内标记：`代码`、**粗体**、__粗体__、*斜体*、~~删除线~~、[文本](url)。
func (m *mdRenderer) inline(s string) string {
	if !m.t.color {
		return stripInlineMarkers(s)
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		// 行内代码
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString(m.t.paint(cYellow, s[i+1:i+1+j]))
				i += j + 2
				continue
			}
		}
		// 粗体 **text** / __text__
		for _, mark := range []string{"**", "__"} {
			if strings.HasPrefix(s[i:], mark) {
				if j := strings.Index(s[i+2:], mark); j > 0 {
					b.WriteString(m.t.paint(cBold, s[i+2:i+2+j]))
					i += j + 4
					goto next
				}
			}
		}
		// 删除线 ~~text~~
		if strings.HasPrefix(s[i:], "~~") {
			if j := strings.Index(s[i+2:], "~~"); j > 0 {
				b.WriteString(m.t.paint(cStrike, s[i+2:i+2+j]))
				i += j + 4
				continue
			}
		}
		// 斜体 *text*
		if s[i] == '*' && !strings.HasPrefix(s[i:], "**") {
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString(m.t.paint(cItalic, s[i+1:i+1+j]))
				i += j + 2
				continue
			}
		}
		// 链接 [文本](url)
		if s[i] == '[' {
			close := strings.IndexByte(s[i:], ']')
			if close > 0 && i+close+1 < len(s) && s[i+close+1] == '(' {
				if end := strings.IndexByte(s[i+close+2:], ')'); end > 0 {
					text := s[i+1 : i+close]
					url := s[i+close+2 : i+close+2+end]
					b.WriteString(m.t.paint(cBlue, text))
					b.WriteString(m.t.paint(cDim, " ("+url+")"))
					i += close + 2 + end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	next:
	}
	return b.String()
}

// stripInlineMarkers 在无色环境下把行内标记去掉，保留文字（便于重定向到文件后仍可读）。
func stripInlineMarkers(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString(s[i+1 : i+1+j])
				i += j + 2
				continue
			}
		}
		matched := false
		for _, mark := range []string{"**", "__", "~~"} {
			if strings.HasPrefix(s[i:], mark) {
				if j := strings.Index(s[i+2:], mark); j > 0 {
					b.WriteString(s[i+2 : i+2+j])
					i += j + 4
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		// 单个 *斜体*：必须和 inline() 一样处理，否则表格的「量宽」与
		// 「渲染」会差两个星号的宽度，整列错位。
		if s[i] == '*' && !strings.HasPrefix(s[i:], "**") {
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString(s[i+1 : i+1+j])
				i += j + 2
				continue
			}
		}
		if s[i] == '[' {
			close := strings.IndexByte(s[i:], ']')
			if close > 0 && i+close+1 < len(s) && s[i+close+1] == '(' {
				if end := strings.IndexByte(s[i+close+2:], ')'); end > 0 {
					b.WriteString(s[i+1 : i+close])
					b.WriteString(" (")
					b.WriteString(s[i+close+2 : i+close+2+end])
					b.WriteString(")")
					i += close + 2 + end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// ---------- 块级识别 ----------

func markdownHeading(s string) (level int, text string, ok bool) {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	rest := s[n:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return 0, "", false
	}
	return n, strings.TrimSpace(strings.TrimRight(strings.TrimSpace(rest), "#")), true
}

func isThematicBreak(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c && s[i] != ' ' && s[i] != '\t' {
			return false
		}
	}
	return true
}

func markdownBullet(line string) (indent, rest string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return "", "", false
	}
	indent = line[:len(line)-len(trimmed)]
	if len(trimmed) < 2 {
		return "", "", false
	}
	switch trimmed[0] {
	case '-', '*', '+':
	default:
		return "", "", false
	}
	if trimmed[1] != ' ' && trimmed[1] != '\t' {
		return "", "", false
	}
	return indent, strings.TrimSpace(trimmed[2:]), true
}

func markdownOrdered(line string) (indent, num, rest string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return "", "", "", false
	}
	indent = line[:len(line)-len(trimmed)]
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(trimmed)-1 {
		return "", "", "", false
	}
	if (trimmed[i] != '.' && trimmed[i] != ')') || trimmed[i+1] != ' ' {
		return "", "", "", false
	}
	return indent, trimmed[:i], strings.TrimSpace(trimmed[i+2:]), true
}

func cutPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// ---------- 表格 ----------

// mdAlign 是一列的对齐方式（由分隔行的 :---: 语法决定）。
type mdAlign int

const (
	alignLeft mdAlign = iota
	alignRight
	alignCenter
)

// 表格排版的尺寸约束。
const (
	// tableMinCol 是单列被压缩时的下限（含省略号）。
	tableMinCol = 6
	// tableDefaultWidth 是拿不到终端宽度时的兜底上限。
	tableDefaultWidth = 120
)

// flushTable 把攒下的表格行一次性排版输出。
//
// 为什么不逐行输出：列宽要靠整块才能算出来。终端里没有浏览器的自动布局，
// 只能自己量宽度（CJK 按 2 列）、自己补空格、自己决定要不要截断。
//
// 样式：完整边框（┌┬┐ ├┼┤ └┴┘）、`│` 分列、表头加粗、超宽列按需截断。
// 非 TTY（`-p` 模式、重定向、管道）时原样输出，避免给下游程序塞满对齐空格。
func (m *mdRenderer) flushTable() {
	rows := m.tableRows
	m.tableRows = nil
	if len(rows) == 0 {
		return
	}
	if !m.t.color {
		for _, r := range rows {
			m.t.writeRaw(r + "\n")
		}
		return
	}

	type row struct {
		cells  []string // 原始单元格（带行内标记）
		plain  []string // 去标记后的可见文本
		header bool
	}
	var (
		parsed []row
		aligns []mdAlign
	)
	for _, raw := range rows {
		cells := splitTableRow(raw)
		if a, ok := parseTableAligns(cells); ok {
			if aligns == nil {
				aligns = a
			}
			// 分隔行紧跟在表头之后：把上一行标成表头。
			if n := len(parsed); n > 0 {
				parsed[n-1].header = true
			}
			continue
		}
		plain := make([]string, len(cells))
		for i, c := range cells {
			plain[i] = stripInlineMarkers(c)
		}
		parsed = append(parsed, row{cells: cells, plain: plain})
	}
	if len(parsed) == 0 {
		return
	}
	// 没有分隔行时也把首行当表头：渲染出来总比一坨平平的表格好看。
	if aligns == nil {
		aligns = []mdAlign{alignLeft}
		parsed[0].header = true
	}

	cols := len(aligns)
	for _, r := range parsed {
		if len(r.cells) > cols {
			cols = len(r.cells)
		}
	}
	for len(aligns) < cols {
		aligns = append(aligns, alignLeft)
	}

	// 列宽按「去掉行内标记后的可见宽度」算，否则 `**粗体**` 会多占 4 列。
	widths := make([]int, cols)
	for _, r := range parsed {
		for i := 0; i < cols; i++ {
			if i >= len(r.plain) {
				continue
			}
			if w := displayWidth(r.plain[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	limit := m.t.width()
	if limit <= 0 {
		limit = tableDefaultWidth
	}
	widths = fitTableWidth(widths, limit)
	if tableTotalWidth(widths) > limit {
		// 终端窄到连最小列宽都摆不下：原样输出，别画一张会折行的破表。
		for _, r := range rows {
			m.t.writeRaw(r + "\n")
		}
		return
	}
	// 截断后列宽可能比内容窄：逐行标记哪些单元格要退化成纯文本。
	shrunk := make([]bool, cols)
	for i, w := range widths {
		for _, r := range parsed {
			if i < len(r.plain) && displayWidth(r.plain[i]) > w {
				shrunk[i] = true
				break
			}
		}
	}

	rule := func(left, mid, right string) string {
		var b strings.Builder
		b.WriteString(m.t.paint(cDim, left))
		for i, w := range widths {
			b.WriteString(m.t.paint(cDim, strings.Repeat("─", w+2)))
			if i < len(widths)-1 {
				b.WriteString(m.t.paint(cDim, mid))
			}
		}
		b.WriteString(m.t.paint(cDim, right))
		return b.String()
	}

	m.t.writeRaw(rule("┌", "┬", "┐") + "\n")
	for _, r := range parsed {
		var b strings.Builder
		b.WriteString(m.t.paint(cDim, "│"))
		for i := 0; i < cols; i++ {
			var text string
			if i < len(r.plain) {
				text = r.plain[i]
			}
			if displayWidth(text) > widths[i] {
				text = truncDisplay(text, widths[i])
			}
			pad := widths[i] - displayWidth(text)
			if pad < 0 {
				pad = 0
			}
			left, right := 0, pad
			switch aligns[i] {
			case alignRight:
				left, right = pad, 0
			case alignCenter:
				left = pad / 2
				right = pad - left
			}
			var cell string
			switch {
			case r.header:
				// 表头加粗：走纯文本，避免 inline() 的 reset 序列把粗体打断。
				cell = m.t.paint(cBold, strings.Repeat(" ", left)+text+strings.Repeat(" ", right))
			case shrunk[i] && i < len(r.cells):
				// 这一列被压缩过：截断会切碎行内标记，直接输出纯文本。
				cell = strings.Repeat(" ", left) + text + strings.Repeat(" ", right)
			case i < len(r.cells):
				cell = strings.Repeat(" ", left) + m.inline(r.cells[i]) + strings.Repeat(" ", right)
			default:
				cell = strings.Repeat(" ", left) + text + strings.Repeat(" ", right)
			}
			b.WriteString(" ")
			b.WriteString(cell)
			b.WriteString(" ")
			b.WriteString(m.t.paint(cDim, "│"))
		}
		m.t.writeRaw(b.String() + "\n")
		if r.header {
			m.t.writeRaw(rule("├", "┼", "┤") + "\n")
		}
	}
	m.t.writeRaw(rule("└", "┴", "┘") + "\n")
}

// tableTotalWidth 返回给定列宽下整张表的显示宽度（含边框与格内留白）。
func tableTotalWidth(widths []int) int {
	n := 1 // 最右边那根 │
	for _, w := range widths {
		n += w + 3 // "│" + " " + cell + " "
	}
	return n
}

// fitTableWidth 在终端放不下时按「削最宽列」的策略收窄，返回新列宽。
//
// limit<=0 表示拿不到终端宽度（非 *os.File、CI、测试缓冲），用兜底上限。
// 削到 tableMinCol 仍放不下时原样返回（调用方会退回不排版）。
func fitTableWidth(widths []int, limit int) []int {
	if limit <= 0 {
		limit = tableDefaultWidth
	}
	out := append([]int(nil), widths...)
	total := func() int { return tableTotalWidth(out) }
	for total() > limit {
		// 找当前最宽且还能削的列。
		idx, best := -1, tableMinCol
		for i, w := range out {
			if w > best {
				idx, best = i, w
			}
		}
		if idx < 0 {
			break // 每列都到下限了，再削也没意义
		}
		out[idx]--
	}
	return out
}

// truncDisplay 按显示宽度截断，末尾补省略号。
func truncDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 { // 留 1 列给省略号
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteString("…")
	return b.String()
}

// splitTableRow 把 `| a | b |` 拆成 [a, b]。
// 单元格里的 `\|` 转义暂不支持（模型几乎不写，真遇到会多切一列）。
func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseTableAligns 判断一行是不是分隔行（|---|:--:|--:|），是则返回各列对齐。
func parseTableAligns(cells []string) ([]mdAlign, bool) {
	if len(cells) == 0 {
		return nil, false
	}
	out := make([]mdAlign, len(cells))
	for i, c := range cells {
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		body := strings.Trim(c, ":")
		if body == "" || strings.Trim(body, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			out[i] = alignCenter
		case right:
			out[i] = alignRight
		default:
			out[i] = alignLeft
		}
	}
	return out, true
}

// displayWidth 返回终端显示宽度：CJK / 全角 / emoji 算 2 列，其余 1 列。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r == 0x200D, r == 0xFE0F, r == 0xFE0E: // ZWJ / 变体选择符
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115F, // Hangul Jamo 初声
		r >= 0x2600 && r <= 0x27BF,   // 杂项符号与 Dingbats（✅ ❌ 等按 emoji 呈现）
		r >= 0x2B00 && r <= 0x2BFF,   // ⬛ ⭐
		r >= 0x2E80 && r <= 0xA4CF,   // CJK 部首、汉字、假名、韩文兼容
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul 音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F,   // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60,   // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B+
		return 2
	}
	return 1
}
