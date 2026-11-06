// Package mentions 实现提示词里的 `@文件` / `@文件夹`。
//
// 分工：
//   - Parse 只做语法扫描（拿到 token 的字节区间与路径），不碰磁盘；
//   - Expand 把 token 变成注入块（文件内容 / 目录清单）+ 预算与安全检查；
//   - Complete 给输入框用，只列 token 所在的那一层目录，不做全仓库搜索。
//
// 为什么展开而不是让模型自己 read：用户写 @ 是显式授权，不该降级成建议；
// 而且编排器（Multica）会把 argv 里的 @file 丢掉，正文里的 @ 反而会原样到
// stdin —— 展开正好补回这个语义。详见 docs/mentions.md。
package mentions

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/leijux/acp-agent/internal/fsutil"
)

// 默认预算。改这里等于改行为，别在调用点另写一套。
const (
	DefaultMaxFileBytes  int64 = 256 << 10 // 单文件注入上限
	DefaultMaxFiles            = 20        // 一次提交最多注入几个文件
	DefaultMaxTotalBytes int64 = 512 << 10 // 一次提交总注入上限
	DefaultMaxDirEntries       = 200       // 目录清单条目上限
	// minBudgetSlice 是「剩余预算少到这个程度就不值得再塞一个文件」的阈值。
	// 半行代码比没有更糟糕：白占一个 slot 还把预算吃干。
	minBudgetSlice = 4 << 10
	binarySniff    = 8 << 10 // 二进制判定只看前 8KB
)

// Token 是一个 `@path` 在原文里的位置与解析结果。
type Token struct {
	Start, End int    // 原文字节区间（含 @ 与引号）
	Raw        string // 原文片段
	Path       string // 去引号、去行号后的路径
	LineRange  string // 行号后缀，如 "10-40"（v1 只透传，不裁剪内容）
	Quoted     bool
}

// Parse 扫描出所有 `@path`。`\@` 视为转义，不算 token（由 Expand 还原成 `@`）。
//
// 起点判定：@ 必须在行首、空白之后，或 `(` `[` `{` `"` `'` “ ` “ 之后。
// 这条规则是为了不误伤 `a@b.com` 这类文本。
func Parse(text string) []Token {
	var out []Token
	for i := 0; i < len(text); {
		if text[i] == '\\' && i+1 < len(text) && text[i+1] == '@' {
			i += 2
			continue
		}
		if text[i] != '@' || !tokenStart(text, i) {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
			continue
		}
		start := i
		j := i + 1
		quoted := false
		if j < len(text) && (text[j] == '"' || text[j] == '\'') {
			quoted = true
			quote := text[j]
			j++
			for j < len(text) && text[j] != quote {
				if text[j] == '\\' && j+1 < len(text) {
					j++
				}
				j++
			}
			if j < len(text) {
				j++ // 收尾引号
			}
		} else {
			for j < len(text) {
				r, size := utf8.DecodeRuneInString(text[j:])
				if isTerminator(r) {
					break
				}
				j += size
			}
		}
		raw := text[start:j]
		inner := raw[1:]
		if quoted && len(inner) >= 2 {
			inner = unquote(inner)
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			i = j
			continue
		}
		path, lineRange := splitLineRange(inner)
		out = append(out, Token{
			Start: start, End: j, Raw: raw,
			Path: path, LineRange: lineRange, Quoted: quoted,
		})
		i = j
	}
	return out
}

// tokenStart 判断 text[i]（一个 @）是否位于 token 起点。
func tokenStart(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune("([{\"'`", r)
}

// isTerminator 是路径的结束字符。
//
// 冒号**不在**表里：`@a.go:10-40` 的行号后缀要能被一并读进 token，再由
// splitLineRange 剥出来。
func isTerminator(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune("\"'`()[]{}<>,;!?，。；：、！？）】」』", r)
}

func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q != '"' && q != '\'') || s[len(s)-1] != q {
		return s
	}
	body := s[1 : len(s)-1]
	// 只处理自己产生的转义：\" 与 \'
	body = strings.ReplaceAll(body, `\"`, `"`)
	body = strings.ReplaceAll(body, `\'`, `'`)
	return body
}

// splitLineRange 把路径尾部的 `:10` / `:10-40` 剥出来。
//
// 盘符（`C:\a\b`）不能被当行号：冒号前只有一个字母时直接放行。
func splitLineRange(s string) (string, string) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return s, ""
	}
	suffix := s[i+1:]
	if !isDigitRange(suffix) {
		return s, ""
	}
	if i == 1 && isASCIILetter(s[0]) {
		return s, ""
	}
	return s[:i], suffix
}

func isDigitRange(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	seenDash := false
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '-' && !seenDash:
			seenDash = true
		default:
			return false
		}
	}
	return digits > 0
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Options 控制一次展开。零值不会崩：所有上限都有默认值。
type Options struct {
	Root          string
	MaxFileBytes  int64
	MaxFiles      int
	MaxTotalBytes int64
	MaxDirEntries int
	AllowOutside  bool
}

func (o Options) withDefaults() Options {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if o.MaxDirEntries <= 0 {
		o.MaxDirEntries = DefaultMaxDirEntries
	}
	return o
}

// Attachment 是一个 token 的处理结果（成功或跳过）。
type Attachment struct {
	Path      string // 展示用路径（相对 Root 时就是用户写的那个）
	Kind      string // "file" | "dir"
	Bytes     int64  // 文件大小 / 清单体积未知时为 0
	Truncated int64  // 实际注入的字节数（被截断时小于 Bytes）
	Entries   int    // 目录条目数
	LineRange string
	Skipped   string // "" | "budget" | "binary" | "missing" | "outside"
}

// Result 是展开结果。
type Result struct {
	Text        string // 展开后的完整 prompt
	Attachments []Attachment
	Warnings    []string
}

// Expand 把 `@token` 展开成 `<attachments>` 注入块，附在正文之前。
//
// @token 原文保留在正文里：删掉会让句子读不通（"看下 里的用法"），模型也
// 需要建立「文中提到 ↔ 附件」的对应。
func Expand(opts Options, text string) Result {
	opts = opts.withDefaults()
	tokens := Parse(text)
	res := Result{Text: unescape(text)}
	if len(tokens) == 0 {
		return res
	}

	var block strings.Builder
	seen := map[string]bool{}
	var files int
	var total int64

	add := func(a Attachment) {
		res.Attachments = append(res.Attachments, a)
	}

	for _, tok := range tokens {
		abs, err := fsutil.Resolve(opts.Root, opts.AllowOutside, tok.Path)
		if err != nil {
			add(Attachment{Path: tok.Path, LineRange: tok.LineRange, Skipped: "outside"})
			res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 已跳过：%v", tok.Path, err))
			continue
		}
		if seen[abs] {
			continue // 同一个文件 @ 两次只注入一份
		}
		seen[abs] = true

		info, err := os.Stat(abs)
		if err != nil {
			add(Attachment{Path: tok.Path, LineRange: tok.LineRange, Skipped: "missing"})
			res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 不存在，已按原文保留", tok.Path))
			continue
		}

		if info.IsDir() {
			before := block.Len()
			entries, more := listDir(abs, opts.MaxDirEntries)
			block.WriteString(fmt.Sprintf("<dir path=%q entries=%q>\n", tok.Path, itoa(len(entries))))
			for _, e := range entries {
				block.WriteString(e)
				block.WriteByte('\n')
			}
			if more > 0 {
				block.WriteString(fmt.Sprintf("…（还有 %d 项）\n", more))
			}
			block.WriteString("</dir>\n")
			// 清单也算总预算：200 个长文件名同样是实打实的上下文。
			total += int64(block.Len() - before)
			add(Attachment{Path: tok.Path, Kind: "dir", Entries: len(entries), LineRange: tok.LineRange})
			continue
		}

		// 预算：文件数与总字节都算上，超了就留占位符而不是静默丢掉。
		if files >= opts.MaxFiles || total >= opts.MaxTotalBytes {
			block.WriteString(fmt.Sprintf("<file path=%q bytes=%q skipped=\"budget\"/>\n", tok.Path, i64toa(info.Size())))
			add(Attachment{Path: tok.Path, Kind: "file", Bytes: info.Size(), LineRange: tok.LineRange, Skipped: "budget"})
			res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 未注入：超出本次预算（%d 个文件 / %dKB）",
				tok.Path, opts.MaxFiles, opts.MaxTotalBytes>>10))
			continue
		}

		limit := opts.MaxFileBytes
		if remain := opts.MaxTotalBytes - total; remain < limit {
			// 配额只剩个位数时不要再塞半行代码：那只是噪音，不如留占位符。
			if remain < minBudgetSlice {
				block.WriteString(fmt.Sprintf("<file path=%q bytes=%q skipped=\"budget\"/>\n", tok.Path, i64toa(info.Size())))
				add(Attachment{Path: tok.Path, Kind: "file", Bytes: info.Size(), LineRange: tok.LineRange, Skipped: "budget"})
				res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 未注入：本次预算只剩 %dB", tok.Path, remain))
				continue
			}
			limit = remain
		}
		data, err := readLimited(abs, limit)
		if err != nil {
			add(Attachment{Path: tok.Path, Kind: "file", LineRange: tok.LineRange, Skipped: "missing"})
			res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 读取失败：%v", tok.Path, err))
			continue
		}
		if isBinary(data) {
			block.WriteString(fmt.Sprintf("<file path=%q bytes=%q skipped=\"binary\"/>\n", tok.Path, i64toa(info.Size())))
			add(Attachment{Path: tok.Path, Kind: "file", Bytes: info.Size(), LineRange: tok.LineRange, Skipped: "binary"})
			res.Warnings = append(res.Warnings, fmt.Sprintf("@%s 是二进制文件（%s），已跳过", tok.Path, humanBytes(info.Size())))
			continue
		}

		files++
		total += int64(len(data))
		if info.Size() > int64(len(data)) {
			block.WriteString(fmt.Sprintf("<file path=%q bytes=%q truncated=%q>\n",
				tok.Path, i64toa(info.Size()), i64toa(int64(len(data)))))
		} else {
			block.WriteString(fmt.Sprintf("<file path=%q bytes=%q>\n", tok.Path, i64toa(info.Size())))
		}
		block.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			block.WriteByte('\n')
		}
		if info.Size() > int64(len(data)) {
			block.WriteString(fmt.Sprintf("…（已截断：共 %s，只注入前 %s）\n",
				humanBytes(info.Size()), humanBytes(int64(len(data)))))
		}
		block.WriteString("</file>\n")
		add(Attachment{
			Path: tok.Path, Kind: "file", Bytes: info.Size(),
			Truncated: int64(len(data)), LineRange: tok.LineRange,
		})
	}

	if block.Len() > 0 {
		res.Text = "<attachments>\n" + block.String() + "</attachments>\n\n" + res.Text
	}
	return res
}

// listDir 列出一层目录（目录名带 / 结尾），返回条目与「还有多少没列」。
func listDir(abs string, max int) ([]string, int) {
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, 0
	}
	out := make([]string, 0, len(des))
	skipped := 0
	for _, d := range des {
		name := d.Name()
		if d.IsDir() {
			if fsutil.Skip(name) {
				continue
			}
			name += "/"
		}
		if len(out) >= max {
			skipped++
			continue
		}
		out = append(out, name)
	}
	return out, skipped
}

// readLimited 最多读 limit 字节。
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if limit <= 0 {
		return nil, nil
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	// 读满、文件比 limit 短（ErrUnexpectedEOF）、空文件（EOF）都是正常结果。
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// isBinary 用 git 的启发式：前面没有 NUL 就当文本；同时非 UTF-8 也当二进制。
func isBinary(data []byte) bool {
	sniff := data
	if len(sniff) > binarySniff {
		sniff = sniff[:binarySniff]
	}
	for _, b := range sniff {
		if b == 0 {
			return true
		}
	}
	if len(data) > len(sniff) {
		return false // 只嗅前 8KB，后面坏了也照注入（截断会在块里标出来）
	}
	return !utf8.Valid(data)
}

// Complete 返回 `@token` 的补全候选与替换区间（区间**不含** @ 本身）。
//
// 只列 token 所在的父目录：`@src/to` 只扫 src/ 下以 to 开头的项。大仓库里
// 全仓搜索会让每次 Tab 都卡一下，得不偿失。
func Complete(root, line string, cur, max int) (cands []string, start, end int) {
	if max <= 0 {
		max = DefaultMaxDirEntries
	}
	if cur <= 0 || cur > len(line) {
		return nil, 0, 0
	}
	tokens := Parse(line[:cur])
	if len(tokens) == 0 {
		return nil, 0, 0
	}
	tok := tokens[len(tokens)-1]
	if tok.End != cur || tok.Quoted {
		return nil, 0, 0
	}
	typed := tok.Path
	dir, base := "", typed
	if i := strings.LastIndexAny(typed, `/\`); i >= 0 {
		dir, base = typed[:i], typed[i+1:]
	}

	lookup := dir
	if lookup == "" {
		lookup = "."
	}
	absDir, err := fsutil.Resolve(root, false, lookup)
	if err != nil {
		return nil, 0, 0
	}
	des, err := os.ReadDir(absDir)
	if err != nil {
		return nil, 0, 0
	}
	prefix := strings.ToLower(base)
	for _, d := range des {
		name := d.Name()
		if d.IsDir() && fsutil.Skip(name) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		p := name
		if d.IsDir() {
			p += "/"
		}
		if dir != "" {
			p = joinDisplay(dir, p)
		}
		cands = append(cands, p)
	}
	sort.Strings(cands)
	if len(cands) > max {
		cands = cands[:max]
	}
	return cands, tok.Start + 1, cur
}

func joinDisplay(dir, name string) string {
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + "/" + name
}

// CommonPrefix 返回候选的最长公共前缀（补全用）。
func CommonPrefix(cands []string) string {
	if len(cands) == 0 {
		return ""
	}
	p := cands[0]
	for _, c := range cands[1:] {
		for !strings.HasPrefix(c, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// unescape 把 `\@` 还原成 `@`（其余反斜杠原样保留）。
func unescape(s string) string {
	if !strings.Contains(s, `\@`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '@' {
			b.WriteByte('@')
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func itoa(n int) string     { return fmt.Sprint(n) }
func i64toa(n int64) string { return fmt.Sprint(n) }

// humanBytes 是按 1024 进制的人读大小（与终端里其它地方的显示口径一致）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
