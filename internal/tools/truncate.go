package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 默认截断预算（对齐 Pi：约 2000 行 / 50KB 取先到者）。
const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 << 10
)

// Truncate 按行数与字节数截断文本，保留头尾，中间插入省略标记。
//
// 返回 truncated 表示是否发生截断；用于给模型明确的「还有更多内容」信号，
// 而不是静默丢数据。
func Truncate(text string, maxLines, maxBytes int) (out string, truncated bool) {
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if len(text) <= maxBytes && strings.Count(text, "\n")+1 <= maxLines {
		return text, false
	}

	lines := strings.Split(text, "\n")
	truncated = true

	if len(lines) > maxLines {
		head := maxLines / 2
		tail := maxLines - head
		if head < 1 {
			head = 1
		}
		if tail < 1 {
			tail = 1
		}
		if head+tail < len(lines) {
			omitted := len(lines) - head - tail
			lines = append(
				append(append([]string{}, lines[:head]...),
					fmt.Sprintf("\n... [%d 行已省略] ...\n", omitted)),
				lines[len(lines)-tail:]...)
			truncated = true
		}
	}

	text = strings.Join(lines, "\n")

	if len(text) > maxBytes {
		// 按字节切：头 2/3，尾 1/3，切点回退到合法 UTF-8 边界。
		headBytes := maxBytes * 2 / 3
		tailBytes := maxBytes - headBytes
		headEnd := safeCutFromEnd(text, headBytes)
		tailStart := safeCutFromStart(text, len(text)-tailBytes)
		if tailStart <= headEnd {
			headEnd = safeCutFromEnd(text, maxBytes)
			out = text[:headEnd]
		} else {
			out = text[:headEnd] +
				fmt.Sprintf("\n... [约 %d 字节已省略] ...\n", tailStart-headEnd) +
				text[tailStart:]
		}
		if len(out) > maxBytes && headEnd >= maxBytes {
			out = out[:safeCutFromEnd(out, maxBytes)]
		}
	} else {
		out = text
	}
	return out, truncated
}

// safeCutFromEnd 返回 <= n 的合法 UTF-8 边界。
func safeCutFromEnd(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	// 若切点在 rune 中间（s[n] 是起始字节但 n>0 且前一 rune 未结束），
	// 上面的判断已保证 n 是 rune 起始位。
	return n
}

// safeCutFromStart 返回 >= n 的合法 UTF-8 边界。
func safeCutFromStart(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for n < len(s) && !utf8.RuneStart(s[n]) {
		n++
	}
	return n
}

// Note 追加截断提示。
func Note(truncated bool, hint string) string {
	if !truncated {
		return ""
	}
	if hint == "" {
		return "\n[输出已截断]"
	}
	return "\n[输出已截断：" + hint + "]"
}

// 数字格式化辅助（行号、字节数）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
