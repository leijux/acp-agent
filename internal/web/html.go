package web

import (
	"fmt"
	"html"
	"strings"
)

// blockTags 是产生段落的标签：进入/离开时都要断行。
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "thead": true, "tbody": true, "section": true, "article": true,
	"header": true, "main": true, "aside": true, "blockquote": true,
	"figure": true, "figcaption": true, "dl": true, "dt": true, "dd": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"form": true, "fieldset": true, "address": true, "details": true, "summary": true,
}

// skipTags 的内容整体丢弃。
//
// nav / footer 也丢：里面是菜单和版权声明，对模型没有信息量，
// 却常占页面正文的一半。
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"iframe": true, "canvas": true, "math": true, "object": true, "embed": true,
	"nav": true, "footer": true,
}

// cellTags 输出制表符，让表格在纯文本里还能看出列。
var cellTags = map[string]bool{"td": true, "th": true}

// ExtractText 把 HTML 转成给模型读的纯文本。
//
// 不是精确的解析器，只保证三件事：script/style 不泄漏、块级标签断行、
// 实体被还原。对模型来说这比保留原始标签更省 token 也更好读。
func ExtractText(src string) string {
	src = stripBOM(src)
	var title string
	// codes 收集 <pre> 内容。它们必须先被抽走，否则 normalizeSpace
	// 会把代码的缩进和换行全折叠掉。
	var codes []string

	var b strings.Builder
	b.Grow(len(src) / 2)

	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		if c != '<' {
			j := strings.IndexByte(src[i:], '<')
			if j < 0 {
				b.WriteString(src[i:])
				break
			}
			b.WriteString(src[i : i+j])
			i += j
			continue
		}
		// 注释
		if strings.HasPrefix(src[i:], "<!--") {
			if j := strings.Index(src[i+4:], "-->"); j >= 0 {
				i += 4 + j + 3
			} else {
				break
			}
			continue
		}
		// doctype / CDATA / 处理指令
		if strings.HasPrefix(src[i:], "<!") || strings.HasPrefix(src[i:], "<?") {
			if j := strings.IndexByte(src[i:], '>'); j >= 0 {
				i += j + 1
			} else {
				break
			}
			continue
		}

		end := tagEnd(src, i)
		if end < 0 {
			// 不闭合的 `<`：当成普通字符。
			b.WriteByte(c)
			i++
			continue
		}
		name, closing, selfClosing := parseTagName(src[i+1 : end])
		i = end + 1

		switch {
		case skipTags[name]:
			if !closing && !selfClosing {
				i = skipElement(src, i, name)
			}
		case name == "title":
			if !closing {
				if j := strings.Index(strings.ToLower(src[i:]), "</title>"); j >= 0 {
					title = strings.TrimSpace(html.UnescapeString(src[i : i+j]))
					i += j + len("</title>")
				}
			}
		case cellTags[name]:
			b.WriteByte('\t')
		case name == "pre":
			if closing || selfClosing {
				b.WriteByte('\n')
				break
			}
			inner, next, ok := elementInner(src, i, "pre")
			if !ok {
				b.WriteByte('\n')
				break
			}
			code := strings.TrimSpace(stripInnerTags(inner))
			if code != "" {
				codes = append(codes, code)
				fmt.Fprintf(&b, "\n\x00%d\x00\n", len(codes)-1)
			}
			i = next
		case name == "li":
			// 列表项加项目符号，否则连续的 li 会被拼成一行。
			if !closing && !selfClosing {
				b.WriteString("\n- ")
			}
		case blockTags[name]:
			b.WriteByte('\n')
		default:
			// 行内标签（a/span/strong/em/code…）直接丢掉，文字本身保留。
		}
	}

	body := normalizeSpace(html.UnescapeString(b.String()))
	for i, code := range codes {
		body = strings.ReplaceAll(body, fmt.Sprintf("\x00%d\x00", i), "```\n"+code+"\n```")
	}
	if title != "" {
		title = normalizeSpace(title)
		if title != "" && !strings.HasPrefix(body, title) {
			return title + "\n\n" + body
		}
	}
	return body
}

// elementInner 返回 name 元素的内容与 </name> 之后的下标。
// pre 不允许嵌套，所以取第一个闭合标签即可。
func elementInner(src string, from int, name string) (inner string, next int, ok bool) {
	lower := strings.ToLower(src[from:])
	j := strings.Index(lower, "</"+name)
	if j < 0 {
		return "", from, false
	}
	k := from + j
	gt := strings.IndexByte(src[k:], '>')
	if gt < 0 {
		return "", from, false
	}
	return src[from:k], k + gt + 1, true
}

// stripInnerTags 去掉代码块内部的标签（通常是 <code>）。
func stripInnerTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '<' {
			if j := strings.IndexByte(s[i:], '>'); j >= 0 {
				i += j + 1
				continue
			}
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// tagEnd 返回从 i 开始的标签的 '>' 下标；引号内的 '>' 不算结束。
func tagEnd(src string, i int) int {
	var quote byte
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j
		}
	}
	return -1
}

// parseTagName 从标签内部文本（不含 &lt;&gt;）解析出标签名与闭合/自闭合标记。
func parseTagName(inner string) (name string, closing, selfClosing bool) {
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(inner[1:])
	}
	if strings.HasSuffix(inner, "/") {
		selfClosing = true
	}
	var sb strings.Builder
	for k := 0; k < len(inner); k++ {
		c := inner[k]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' || c == '>' {
			break
		}
		sb.WriteByte(c)
	}
	return strings.ToLower(sb.String()), closing, selfClosing
}

// skipElement 跳过 name 元素的整个内容，返回 `</name>` 之后的下标。
func skipElement(src string, from int, name string) int {
	lower := strings.ToLower(src[from:])
	j := strings.Index(lower, "</"+name)
	if j < 0 {
		return len(src)
	}
	k := from + j
	if gt := strings.IndexByte(src[k:], '>'); gt >= 0 {
		return k + gt + 1
	}
	return len(src)
}

// normalizeSpace 折叠空白：行内连续空白变一个空格，空行最多保留一个。
func normalizeSpace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			blank++
			continue
		}
		if blank > 0 && len(out) > 0 {
			out = append(out, "")
		}
		blank = 0
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
