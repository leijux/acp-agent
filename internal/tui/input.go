package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// inputLine 是 REPL 的输入行：按 UTF-8 边界编辑，渲染时按**显示列**横向滚动。
//
// 为什么自己写而不是用 bubbles/textinput：bubbles 的 textinput 假设单行 ASCII
// 友好、光标定位按 rune 计宽；CJK/emoji 在这里必须按列算，否则删一个字会残留
// 半个字形（与 cmd/acp-agent/keyinput.go 里修过的是同一个坑）。
type inputLine struct {
	buf []byte
	cur int // 插入点（buf 的字节下标）

	// pastes 保存被折叠的粘贴内容：buf 里只留一个 [txt×N行] 占位符，
	// 真实文本在这里，提交（text）时才展开。否则多行粘贴会把单行输入框
	// 顶碎（内嵌换行会突破「一行」的前提）。
	pastes []pasteRef
}

// pasteRef 是一个占位符与它代替的原文。
type pasteRef struct {
	token string
	text  string
}

// takeTail 把光标前面刚插入的 n 个 rune 抽回去，返回抽出的文本。
//
// 用在“没括号粘贴标记”的落粘贴上：第一个分块已经按普通打字插进去了，
// 等下一个事件确认这是粘贴时再抽回来，跟后面的内容一起折成占位符
// （否则输入行会变成「第一行[txt×2行]」这种半折半不折的样子）。
func (in *inputLine) takeTail(n int) string {
	if n <= 0 || in.cur <= 0 {
		return ""
	}
	start := in.cur
	for i := 0; i < n && start > 0; i++ {
		_, size := utf8.DecodeLastRune(in.buf[:start])
		start -= size
	}
	s := string(in.buf[start:in.cur])
	in.buf = append(in.buf[:start], in.buf[in.cur:]...)
	in.cur = start
	return s
}

// 粘贴折叠阀值：多行必然折叠（单行输入框装不下），单行超过这个长度也折叠。
const pasteFoldRunes = 300

func (in *inputLine) text() string { return in.expand(string(in.buf)) }

func (in *inputLine) empty() bool { return len(in.buf) == 0 }

// setText 整体替换输入内容，光标落到末尾（输入历史导航用）。
//
// 走 paste() 而不是直接写 buf：历史项可能是多行（Alt+Enter 敲出来的）或超长，
// 直接塞进去会把「单行输入框」的前提破掉；paste 会按既有规则折叠成占位符，
// 提交时再展开，与用户自己粘贴同一条路径。
func (in *inputLine) setText(s string) {
	in.reset()
	in.paste(s)
	in.cur = len(in.buf)
}

func (in *inputLine) reset() {
	in.buf = in.buf[:0]
	in.cur = 0
	in.pastes = nil
}

// insert 追加一个字节。UTF-8 多字节字符会被逐个字节喂进来，也是对的。
func (in *inputLine) insert(b byte) {
	in.buf = append(in.buf, 0)
	copy(in.buf[in.cur+1:], in.buf[in.cur:])
	in.buf[in.cur] = b
	in.cur++
}

// insertString 在插入点整段插入（粘贴/补全用）。
func (in *inputLine) insertString(s string) {
	if s == "" {
		return
	}
	tail := append([]byte(nil), in.buf[in.cur:]...)
	in.buf = append(in.buf[:in.cur], s...)
	in.buf = append(in.buf, tail...)
	in.cur += len(s)
}

func (in *inputLine) backspace() {
	if in.cur == 0 {
		return
	}
	// 占位符当一个整体删：只删掉半个 [txt×12行] 既难看，也没法再展开。
	if sp, ok := in.tokenAt(in.cur - 1); ok {
		in.cut(sp.start, sp.end)
		return
	}
	start := runeStartBefore(in.buf, in.cur)
	in.buf = append(in.buf[:start], in.buf[in.cur:]...)
	in.cur = start
	in.prune()
}

func (in *inputLine) deleteForward() {
	if in.cur >= len(in.buf) {
		return
	}
	if sp, ok := in.tokenAt(in.cur); ok {
		in.cut(sp.start, sp.end)
		return
	}
	end := runeStartAfter(in.buf, in.cur)
	in.buf = append(in.buf[:in.cur], in.buf[end:]...)
	in.prune()
}

func (in *inputLine) left() {
	if in.cur <= 0 {
		return
	}
	if sp, ok := in.tokenAt(in.cur - 1); ok {
		in.cur = sp.start // 光标跳到占位符前面，整体跳过
		return
	}
	in.cur = runeStartBefore(in.buf, in.cur)
}

func (in *inputLine) right() {
	if in.cur >= len(in.buf) {
		return
	}
	if sp, ok := in.tokenAt(in.cur); ok {
		in.cur = sp.end
		return
	}
	in.cur = runeStartAfter(in.buf, in.cur)
}

func (in *inputLine) home() { in.cur = 0 }
func (in *inputLine) end()  { in.cur = len(in.buf) }

func (in *inputLine) killToStart() {
	in.buf = append(in.buf[:0], in.buf[in.cur:]...)
	in.cur = 0
	in.prune()
}

func (in *inputLine) killToEnd() {
	in.buf = in.buf[:in.cur]
	in.prune()
}

// view 渲染「提示符 + 输入 + 光标」。可用宽度不够时只保留光标附近的一段，
// 保证光标永远可见（长输入下很重要的手感）。
func (in *inputLine) view(prompt string, cols int) string {
	if cols <= 0 {
		cols = 80
	}
	pw := ansi.StringWidth(prompt)
	avail := cols - pw
	if avail < 4 {
		avail = 4
	}

	before := displayText(string(in.buf[:in.cur]))
	after := displayText(string(in.buf[in.cur:]))

	// 光标前的部分从左侧裁剪，给光标至少留 1 列。
	// 逐 rune 丢弃而不是一次算差值：宽字符占 2 列，按列差裁剪会留在半格上。
	maxBefore := avail - 1
	for maxBefore >= 0 && ansi.StringWidth(before) > maxBefore {
		_, size := utf8.DecodeRuneInString(before)
		if size <= 0 {
			break
		}
		before = before[size:]
	}
	beforeW := ansi.StringWidth(before)

	caret := " "
	afterW := 0
	if r, rest := firstRune(after); r != 0 {
		caret = string(r)
		afterW = ansi.StringWidth(after) - ansi.StringWidth(rest)
		after = rest
	} else {
		after = ""
	}

	// 光标后还能放多少列（放不下的部分直接截断，避免顶破输入行）。
	rest := avail - beforeW - afterW
	if rest < 0 {
		rest = 0
	}
	after = ansi.Truncate(after, rest, "")

	return prompt + before + "\x1b[7m" + caret + escReset + after
}

// displayText 把存着的换行画成 ⏎。
//
// 输入框是固定一行的：Alt+Enter 手动换行后，如果直接把 \n 拼进输出，
// 终端会把这一行拆成两行，底部行数对不上 → 整屏错位。
func displayText(s string) string { return strings.ReplaceAll(s, "\n", "⏎") }

// firstRune 返回 s 的第一个 rune 与剩余部分。
func firstRune(s string) (rune, string) {
	if s == "" {
		return 0, ""
	}
	for i := 1; i <= len(s); i++ {
		if i == len(s) || (i < len(s) && s[i]&0xC0 != 0x80) {
			rs := []rune(s[:i])
			if len(rs) == 0 {
				return 0, s[i:]
			}
			return rs[0], s[i:]
		}
	}
	return 0, ""
}

// runeStartBefore 返回 i 之前那个字符的起始字节下标。
func runeStartBefore(b []byte, i int) int {
	if i <= 0 {
		return 0
	}
	j := i - 1
	for j > 0 && b[j]&0xC0 == 0x80 { // UTF-8 续字节
		j--
	}
	return j
}

// runeStartAfter 返回 i 处字符之后的字节下标。
func runeStartAfter(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	j := i + 1
	for j < len(b) && b[j]&0xC0 == 0x80 {
		j++
	}
	return j
}

// cleanLine 去掉行尾空白，用于把粘贴内容整段塞进输入行。
func cleanLine(s string) string { return strings.TrimRight(s, "\r\n") }

// ---------- 粘贴折叠 ----------

// paste 处理一段粘贴内容（括号粘贴模式下一次带来一大段）。
//
// 规则：
//   - 多行：一律折叠成 [txt×N行]（单行输入框装不下内嵌换行）；
//   - 单行但超过 pasteFoldRunes：折叠成 [txt×N字]（避免一屏看不见头尾）；
//   - 其余情况就跟普通插入一样。
//
// 注意先做 CRLF 归一：换个系统复制来的文本常带 \r\n，
// 行数按 \n 数，留着 \r 会把行数和渲染都搞乱。
func (in *inputLine) paste(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = cleanLine(s)
	if s == "" {
		return
	}

	lines := strings.Count(s, "\n") + 1
	var unit string
	switch {
	case lines > 1:
		unit = fmt.Sprintf("%d行", lines)
	case len([]rune(s)) > pasteFoldRunes:
		unit = fmt.Sprintf("%d字", len([]rune(s)))
	default:
		in.insertString(s)
		return
	}

	// 占位符必须唯一：两个内容不同但行数相同的粘贴不能共用一个 token，
	// 否则展开时两处都会变成后一个。序号放在方括号**里面** —— 放外面会
	// 变成 [txt×2行]2，它把 [txt×2行] 当成前缀，展开时会互相干扰。
	token := "[txt×" + unit + "]"
	for n := 2; in.hasPaste(token); n++ {
		token = fmt.Sprintf("[txt×%s·%d]", unit, n)
	}
	in.pastes = append(in.pastes, pasteRef{token: token, text: s})
	in.insertString(token)
}

// expand 把 buf 里的占位符换回原文（提交时用）。
func (in *inputLine) expand(s string) string {
	for _, p := range in.pastes {
		s = strings.ReplaceAll(s, p.token, p.text)
	}
	return s
}

// hasPaste 报告 token 是否已被用着。
func (in *inputLine) hasPaste(token string) bool {
	for _, p := range in.pastes {
		if p.token == token {
			return true
		}
	}
	return false
}

// spans 返回 buf 里所有占位符的区间（按位置升序），供编辑时整体跳过/删除。
type tokenSpan struct{ start, end int }

func (in *inputLine) spans() []tokenSpan {
	buf := string(in.buf)
	var out []tokenSpan
	for _, p := range in.pastes {
		for off := 0; ; {
			i := strings.Index(buf[off:], p.token)
			if i < 0 {
				break
			}
			start := off + i
			out = append(out, tokenSpan{start: start, end: start + len(p.token)})
			off = start + len(p.token)
		}
	}
	return out
}

// tokenAt 返回覆盖字节下标 i 的占位符。
func (in *inputLine) tokenAt(i int) (tokenSpan, bool) {
	for _, sp := range in.spans() {
		if i >= sp.start && i < sp.end {
			return sp, true
		}
	}
	return tokenSpan{}, false
}

// cut 删掉 [start,end)，并把插入点放到 start。
func (in *inputLine) cut(start, end int) {
	if start < 0 {
		start = 0
	}
	if end > len(in.buf) {
		end = len(in.buf)
	}
	if start >= end {
		return
	}
	in.buf = append(in.buf[:start], in.buf[end:]...)
	in.cur = start
	in.prune()
}

// prune 丢掉 buf 里已经找不到的占位符（被手动销毁的不再参与展开）。
func (in *inputLine) prune() {
	if len(in.pastes) == 0 {
		return
	}
	buf := string(in.buf)
	kept := in.pastes[:0]
	for _, p := range in.pastes {
		if strings.Contains(buf, p.token) {
			kept = append(kept, p)
		}
	}
	in.pastes = kept
}
