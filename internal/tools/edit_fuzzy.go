package tools

// edit 的文本容错层：编码/行尾归一 + 模糊匹配。
//
// 动机（对齐上游 Pi 的 edit-diff.ts）：
//   - 从网页/文档粘来的代码常带智能引号（“” ‘’）、各种破折号（– — −）、
//     NBSP/全角空格、行尾空白 —— 逐字符精确匹配会直接报「找不到」，
//     模型只能猜着重试；
//   - Windows 项目的文件往往是 CRLF，甚至带 UTF-8 BOM。模型给的 oldText
//     通常是 LF，精确匹配同样会失败。
//
// 处理顺序：先剥 BOM、把全文归一成 LF 匹配，写入时再还原原始行尾与 BOM；
// 精确匹配失败才降级到模糊匹配（NFKC + 去行尾空白 + 标点归一）。
//
// 模糊匹配的一个取舍：**被触及的行会按归一后的形态重写**，未触及的行逐字节
// 保留原始内容。这样不会把整个文件格式化掉，也能容忍行尾空白差异。

import (
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// utf8BOM 是 UTF-8 的 BOM 前缀（模型不会在 oldText 里写出这个不可见字符）。
const utf8BOM = "\ufeff"

// splitBOM 把 BOM 与正文拆开。
func splitBOM(s string) (bom, text string) {
	if strings.HasPrefix(s, utf8BOM) {
		return utf8BOM, s[len(utf8BOM):]
	}
	return "", s
}

// detectLineEnding 判断文件主行尾：第一个换行是 CRLF 就算 CRLF。
func detectLineEnding(s string) string {
	crlf := strings.Index(s, "\r\n")
	lf := strings.Index(s, "\n")
	if lf < 0 || crlf < 0 {
		return "\n"
	}
	if crlf < lf {
		return "\r\n"
	}
	return "\n"
}

// normalizeToLF 把所有行尾归一成 LF。
func normalizeToLF(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// restoreLineEndings 把 LF 还原成指定行尾。
func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// normalizeForFuzzyMatch 做模糊匹配前的归一：
//
//   - NFKC（全角 ASCII → 半角、兼容汉字等）
//   - 每行去行尾空白（模型极难复现行尾空格）
//   - 智能引号 → ASCII 引号
//   - 各式破折号/连字符/减号 → '-'
//   - 各类特殊空格（NBSP、en/em space、表意空格等）→ 普通空格
//
// 注意：行首缩进**不动**——缩进是语义信息，抹掉会让匹配落到错误的层级上。
func normalizeForFuzzyMatch(s string) string {
	s = norm.NFKC.String(s)
	if strings.ContainsRune(s, '\n') {
		lines := strings.Split(s, "\n")
		for i, ln := range lines {
			lines[i] = strings.TrimRight(ln, " \t")
		}
		s = strings.Join(lines, "\n")
	} else {
		s = strings.TrimRight(s, " \t")
	}
	s = strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'",
		"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`,
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-",
		"\u2014", "-", "\u2015", "-", "\u2212", "-",
		"\u00a0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ",
		"\u2005", " ", "\u2006", " ", "\u2007", " ", "\u2008", " ",
		"\u2009", " ", "\u200a", " ", "\u202f", " ", "\u205f", " ", "\u3000", " ",
	).Replace(s)
	return s
}

// splitLinesWithEndings 按行切开但保留行尾（"a\nb" → ["a\n", "b"]）。
func splitLinesWithEndings(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, strings.Count(s, "\n")+1)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// lineSpans 返回每行在 s 中的字节区间。
func lineSpans(s string) [][2]int {
	lines := splitLinesWithEndings(s)
	spans := make([][2]int, len(lines))
	off := 0
	for i, ln := range lines {
		spans[i] = [2]int{off, off + len(ln)}
		off += len(ln)
	}
	return spans
}

// applyFuzzyReplace 把 base（归一后的内容）上 [idx, idx+length) 的匹配
// 应用回 original，未触及的行保留 original 的原始字节。
//
// 为什么要走「按行」这一步：模糊归一改变了字符偏移（去行尾空白、引号替换），
// base 里的下标不能直接用到 original 上。
func applyFuzzyReplace(original, base string, idx, length int, newText string) (string, error) {
	if idx < 0 || idx+length > len(base) {
		return "", fmt.Errorf("内部错误：模糊匹配区间超出内容范围")
	}
	origLines := splitLinesWithEndings(original)
	spans := lineSpans(base)
	if len(origLines) != len(spans) {
		// 归一化不该改变行数；真变了就退回整体替换（安全但会丢原始字节）。
		return base[:idx] + newText + base[idx+length:], nil
	}

	// 找出匹配覆盖的行区间 [startLine, endLine)。
	startLine := -1
	for i, sp := range spans {
		if idx >= sp[0] && idx < sp[1] {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		return "", fmt.Errorf("内部错误：无法定位模糊匹配所在行")
	}
	endLine := startLine
	for endLine < len(spans) && spans[endLine][1] < idx+length {
		endLine++
	}
	if endLine >= len(spans) {
		endLine = len(spans) - 1
	}

	var b strings.Builder
	for _, ln := range origLines[:startLine] {
		b.WriteString(ln)
	}
	// 触及的行：以 base 的内容为准（可能被归一过），只替换命中区间。
	groupStart := spans[startLine][0]
	groupEnd := spans[endLine][1]
	touched := base[groupStart:groupEnd]
	relStart := idx - groupStart
	relEnd := relStart + length
	b.WriteString(touched[:relStart])
	b.WriteString(newText)
	b.WriteString(touched[relEnd:])
	for _, ln := range origLines[endLine+1:] {
		b.WriteString(ln)
	}
	return b.String(), nil
}

// ---------- 重叠预检 ----------

// locateOriginalRange 在**原文**里给 oldText 定位，返回字节区间。
//
// 精确优先；失败再走与模糊替换同一套归一，命中后按行映射回原文偏移
// （归一改变了字符偏移，不能直接拿 fuzzy 空间的下标）。
// ok=false 表示原文里根本没有这段文本 —— 那多半是「链式」写法
// （第二个 edit 依赖第一个 edit 的结果），交给增量匹配处理。
func locateOriginalRange(original, oldText string) (start, end int, ok bool) {
	if idx := strings.Index(original, oldText); idx >= 0 {
		return idx, idx + len(oldText), true
	}
	fuzzyOrig := normalizeForFuzzyMatch(original)
	fuzzyOld := normalizeForFuzzyMatch(oldText)
	if strings.TrimSpace(fuzzyOld) == "" || strings.Count(fuzzyOrig, fuzzyOld) != 1 {
		return 0, 0, false
	}
	idx := strings.Index(fuzzyOrig, fuzzyOld)
	spans := lineSpans(fuzzyOrig)
	origLines := splitLinesWithEndings(original)
	if len(spans) != len(origLines) {
		return 0, 0, false
	}

	startLine := -1
	for i, sp := range spans {
		if idx >= sp[0] && idx < sp[1] {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		return 0, 0, false
	}
	endLine := startLine
	matchEnd := idx + len(fuzzyOld)
	for endLine < len(spans) && spans[endLine][1] < matchEnd {
		endLine++
	}
	if endLine >= len(spans) {
		endLine = len(spans) - 1
	}

	origSpans := lineSpans(original)
	return origSpans[startLine][0], origSpans[endLine][1], true
}

// checkEditOverlap 用原文定位每个 oldText，两两比较是否重叠。
//
// 为什么要这一步：增量匹配下，「第二个 oldText 落在第一个 newText 内部」会被
// 静默命中，产出模型根本没想要的结果。典型例子：
//
//	原文  "value = 10"
//	edits [ {old:"value = 10", new:"value = 100"}, {old:"= 10", new:"= 20"} ]
//
//	增量执行 → "value = 200"（模型想要的是两处独立修改），且不报错。
//
// 上游 Pi 是「全部对原文匹配」，这种情况会直接报 overlap。我们保留增量语义
// （链式改写仍然可用），只在「两个 oldText 在原文里就重叠」时报错 —— 那几乎
// 一定是模型写错了。
func checkEditOverlap(original string, olds []string) error {
	type span struct {
		idx  int
		from int
		to   int
	}
	var spans []span
	for i, oldText := range olds {
		from, to, ok := locateOriginalRange(original, oldText)
		if !ok {
			continue // 原文里没有：链式写法，交给增量匹配
		}
		spans = append(spans, span{idx: i, from: from, to: to})
	}
	if len(spans) < 2 {
		return nil
	}
	// 按起点排序后比较相邻区间（相交必然在相邻项上暴露）。
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].from < spans[j-1].from; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	for i := 1; i < len(spans); i++ {
		if spans[i-1].to > spans[i].from {
			prev, cur := spans[i-1], spans[i]
			return fmt.Errorf(
				"edits[%d] 与 edits[%d] 在原文中重叠，结果会取决于执行顺序。请合并成一个 edit，或改成互不相交的区域",
				prev.idx, cur.idx)
		}
	}
	return nil
}
