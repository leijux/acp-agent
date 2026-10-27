package tui

import (
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
}

func (in *inputLine) text() string { return string(in.buf) }

func (in *inputLine) empty() bool { return len(in.buf) == 0 }

func (in *inputLine) reset() {
	in.buf = in.buf[:0]
	in.cur = 0
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
	start := runeStartBefore(in.buf, in.cur)
	in.buf = append(in.buf[:start], in.buf[in.cur:]...)
	in.cur = start
}

func (in *inputLine) deleteForward() {
	if in.cur >= len(in.buf) {
		return
	}
	end := runeStartAfter(in.buf, in.cur)
	in.buf = append(in.buf[:in.cur], in.buf[end:]...)
}

func (in *inputLine) left() {
	if in.cur > 0 {
		in.cur = runeStartBefore(in.buf, in.cur)
	}
}

func (in *inputLine) right() {
	if in.cur < len(in.buf) {
		in.cur = runeStartAfter(in.buf, in.cur)
	}
}

func (in *inputLine) home() { in.cur = 0 }
func (in *inputLine) end()  { in.cur = len(in.buf) }

func (in *inputLine) killToStart() {
	in.buf = append(in.buf[:0], in.buf[in.cur:]...)
	in.cur = 0
}

func (in *inputLine) killToEnd() {
	in.buf = in.buf[:in.cur]
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

	before := string(in.buf[:in.cur])
	after := string(in.buf[in.cur:])

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
