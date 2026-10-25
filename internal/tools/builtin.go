package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

// ReadTool 读取文件内容（带行号），支持行范围与图片。
type ReadTool struct {
	Policy Policy
}

func NewReadTool(p Policy) *ReadTool { return &ReadTool{Policy: p} }

func (*ReadTool) Name() string { return "read" }
func (*ReadTool) Description() string {
	return "读取文件内容。优先用它查看代码；支持 offset/limit 分页。" +
		"图片文件返回图片内容。大文件会被截断，需要时用 offset 继续读。"
}
func (*ReadTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"path":    Prop("string", "文件路径（绝对或相对于工作目录）"),
		"offset":  PropInt("起始行号，1 起（用于大文件分页）", 0),
		"limit":   PropInt("最多读取行数", 2000),
		"noLines": map[string]any{"type": "boolean", "description": "不带行号，返回原始内容"},
	}, "path")
}
func (*ReadTool) ConcurrencySafe() bool { return true }
func (*ReadTool) Mutates() bool         { return false }

// MaxReadBytesFor 计算单次读取上限。
func (t *ReadTool) maxBytes() int64 {
	if t.Policy.MaxReadBytes > 0 {
		return t.Policy.MaxReadBytes
	}
	return 2 << 20
}

var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}

func (t *ReadTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	raw, err := a.Required("path")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	abs, err := t.Policy.Resolve(raw)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return ErrorResult("读取失败：%v", err), nil
	}
	if st.IsDir() {
		return ErrorResult("%s 是目录，请用 ls", t.Policy.Rel(abs)), nil
	}
	if ext := strings.ToLower(filepath.Ext(abs)); imageExts[ext] != "" {
		if st.Size() > 10<<20 {
			return ErrorResult("图片过大（%s），上限 10MB", humanBytes(st.Size())), nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return ErrorResult("读取失败：%v", err), nil
		}
		return Result{
			Content: []Block{{
				Kind:     KindImage,
				Data:     base64.StdEncoding.EncodeToString(data),
				MIMEType: imageExts[ext],
			}},
			Details: map[string]any{"path": t.Policy.Rel(abs), "bytes": len(data)},
		}, nil
	}
	if ext_(abs) == ".pdf" {
		// 不做 PDF 解析，明确告知而不是吐二进制。
		return ErrorResult("%s 是 PDF，当前工具不支持解析，请用其它方式提取文本", t.Policy.Rel(abs)), nil
	}

	data, err := readCapped(abs, t.maxBytes())
	if err != nil {
		return ErrorResult("读取失败：%v", err), nil
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	// 末尾换行会产生一个空元素，去掉以免显示多余行号。
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	offset := int(a.Int("offset", 0))
	if offset < 0 {
		offset = 0
	}
	limit := int(a.Int("limit", 2000))
	if limit <= 0 {
		limit = 2000
	}
	start := offset
	if start > 0 {
		start-- // 1-based → 0-based
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	view := lines[start:end]

	noLines := a.Bool("noLines", false)
	var b strings.Builder
	for i, ln := range view {
		if noLines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(ln)
			continue
		}
		fmt.Fprintf(&b, "%5d| %s\n", start+i+1, ln)
	}
	out := b.String()
	truncated := false
	if end < len(lines) {
		truncated = true
		out += fmt.Sprintf("\n[还有 %d 行未显示，用 offset=%d 继续]\n", len(lines)-end, end+1)
	}
	if noLines {
		if cut, t2 := Truncate(out, 0, 0); t2 {
			out, truncated = cut, true
		}
	}
	return Result{
		Content:   []Block{{Kind: KindText, Text: out, Start: start + 1, End: end}},
		Truncated: truncated,
		Details: map[string]any{
			"path":  t.Policy.Rel(abs),
			"lines": len(lines),
			"start": start + 1,
			"end":   end,
		},
	}, nil
}

// ext_ 小写扩展名（含点）。
func ext_(p string) string { return strings.ToLower(filepath.Ext(p)) }

// readCapped 读取至多 max 字节，多读 1 字节用于判断是否截断。
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, max+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	if int64(n) > max {
		return buf[:max], nil
	}
	return buf[:n], nil
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// WriteTool 写入/创建文件。
type WriteTool struct {
	Policy Policy
}

func NewWriteTool(p Policy) *WriteTool { return &WriteTool{Policy: p} }

func (*WriteTool) Name() string { return "write" }
func (*WriteTool) Description() string {
	return "创建或覆盖文件。会自动创建父目录。完整重写用 write；局部修改优先用 edit。"
}
func (*WriteTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"path":    Prop("string", "文件路径"),
		"content": Prop("string", "完整文件内容"),
	}, "path", "content")
}

// ConcurrencySafe 为 true：本工具用 lockFile 把同一路径的写串行化，
// 不同路径的 write 可以真正并行。
func (*WriteTool) ConcurrencySafe() bool { return true }
func (*WriteTool) Mutates() bool         { return true }
func (*WriteTool) PathScoped() bool      { return true }

func (t *WriteTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	if t.Policy.ReadOnly {
		return ErrorResult("当前策略为只读，禁止写文件"), nil
	}
	raw, err := a.Required("path")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	content, ok := a.String("content")
	if !ok {
		// 允许 content 为空字符串（创建空文件）。
		if v, exists := a["content"]; !exists || v == nil {
			return ErrorResult("缺少参数 content"), nil
		}
	}
	abs, err := t.Policy.Resolve(raw)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	// Stat 与 WriteFile 必须在同一把锁里，否则两个并发 write 的
	// existed/oldSize 会互相污染，模型看到的 created/overwrote 就是错的。
	unlock := LockFile(abs)
	defer unlock()
	existed := false
	oldSize := int64(0)
	if st, err := os.Stat(abs); err == nil {
		if st.IsDir() {
			return ErrorResult("%s 是目录", t.Policy.Rel(abs)), nil
		}
		existed, oldSize = true, st.Size()
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return ErrorResult("创建目录失败：%v", err), nil
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return ErrorResult("写入失败：%v", err), nil
	}
	action := "created"
	if existed {
		action = "overwrote"
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: fmt.Sprintf("%s %s (%d bytes)", action, t.Policy.Rel(abs), len(content))}},
		Details: map[string]any{
			"path": t.Policy.Rel(abs), "action": action,
			"bytes": len(content), "oldBytes": oldSize, "existed": existed,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// Edit 是一次替换。
type Edit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// EditTool 精确文本替换（要求 oldText 在文件中唯一，或提供 occurrence）。
type EditTool struct {
	Policy Policy
}

func NewEditTool(p Policy) *EditTool { return &EditTool{Policy: p} }

func (*EditTool) Name() string { return "edit" }
func (*EditTool) Description() string {
	return "精确文本替换。oldText 必须与文件内容逐字符匹配（含缩进），且默认要求唯一出现。" +
		"多个互不重叠的改动放在一次调用的 edits 数组里，比多次调用更安全。" +
		"不要用大段未改动内容去「连接」相距很远的修改。" +
		"容错：CRLF/LF 与 UTF-8 BOM 自动归一；精确匹配失败时会忽略行尾空白、" +
		"智能引号、各式破折号与特殊空格再试一次（会提示为模糊匹配）。" +
		"两个 oldText 在原文中重叠会直接报错（结果会取决于执行顺序），" +
		"但『后一个 oldText 只在前一个改完之后才出现』的链式写法仍然允许。"
}
func (*EditTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"path": Prop("string", "文件路径"),
		"edits": map[string]any{
			"type":        "array",
			"description": "替换列表，按顺序应用，互不重叠",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"oldText": Prop("string", "要被替换的精确原文"),
					"newText": Prop("string", "替换后的文本"),
				},
				"required":             []string{"oldText", "newText"},
				"additionalProperties": false,
			},
		},
	}, "path", "edits")
}
func (*EditTool) ConcurrencySafe() bool { return true }
func (*EditTool) Mutates() bool         { return true }
func (*EditTool) PathScoped() bool      { return true }

func (t *EditTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	if t.Policy.ReadOnly {
		return ErrorResult("当前策略为只读，禁止修改文件"), nil
	}
	raw, err := a.Required("path")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	abs, err := t.Policy.Resolve(raw)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	edits, err := parseEdits(a)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	// 读-改-写整体持锁：两个并发 edit 同一文件时，后一个必须看到
	// 前一个的结果，否则后写者会用它读到的旧内容覆盖掉前者的修改。
	unlock := LockFile(abs)
	defer unlock()
	data, err := os.ReadFile(abs)
	if err != nil {
		return ErrorResult("读取失败：%v", err), nil
	}
	// 编码/行尾归一：BOM 先剥掉（模型不可能把不可见 BOM 写进 oldText），
	// 全文归一成 LF 再匹配，写回时还原原始行尾与 BOM。
	original := string(data)
	bom, body := splitBOM(original)
	ending := detectLineEnding(body)
	cur := normalizeToLF(body)

	// 先归一 + 校验，再做重叠预检（预检需要干净的 oldText 列表）。
	norm := make([][2]string, len(edits))
	for i, e := range edits {
		o, nw := normalizeToLF(e.OldText), normalizeToLF(e.NewText)
		if o == "" {
			return ErrorResult("edits[%d]: oldText 不能为空", i), nil
		}
		if o == nw {
			return ErrorResult("edits[%d]: oldText 与 newText 相同，无意义", i), nil
		}
		norm[i] = [2]string{o, nw}
	}
	olds := make([]string, len(norm))
	for i := range norm {
		olds[i] = norm[i][0]
	}
	if err := checkEditOverlap(cur, olds); err != nil {
		return ErrorResult("%v", err), nil
	}

	applied, fuzzyUsed := 0, 0
	for i := range norm {
		oldText, newText := norm[i][0], norm[i][1]
		switch n := strings.Count(cur, oldText); {
		case n == 1:
			cur = strings.Replace(cur, oldText, newText, 1)
			applied++

		case n > 1:
			return ErrorResult("edits[%d]: oldText 出现 %d 次，不唯一。请扩大上下文使其唯一", i, n), nil

		default:
			// 精确匹配失败 → 模糊匹配（NFKC、去行尾空白、智能引号/破折号/特殊空格归一）。
			next, matched, err := fuzzyReplace(cur, oldText, newText)
			if err != nil {
				if errors.Is(err, errFuzzyNotFound) {
					return ErrorResult(
						"edits[%d]: 文件中找不到该 oldText（已尝试忽略行尾空白/智能引号/破折号/特殊空格/CRLF 差异）\n---\n%s",
						i, clip(oldText, 300)), nil
				}
				if errors.Is(err, errFuzzyAmbiguous) {
					return ErrorResult("edits[%d]: oldText 模糊匹配到多处，不唯一。请扩大上下文使其唯一", i), nil
				}
				return ErrorResult("edits[%d]: %v", i, err), nil
			}
			if matched {
				cur = next
				applied++
				fuzzyUsed++
			} else {
				return ErrorResult("edits[%d]: 文件中找不到该 oldText（注意缩进/换行需完全一致）\n---\n%s", i, clip(oldText, 300)), nil
			}
		}
	}

	final := bom + restoreLineEndings(cur, ending)
	if final == original {
		return ErrorResult("没有产生任何修改"), nil
	}
	if err := os.WriteFile(abs, []byte(final), 0o644); err != nil {
		return ErrorResult("写入失败：%v", err), nil
	}
	msg := fmt.Sprintf("edited %s (%d edits, %d → %d bytes)", t.Policy.Rel(abs), applied, len(original), len(final))
	if fuzzyUsed > 0 {
		msg += fmt.Sprintf("，其中 %d 处为模糊匹配（已忽略行尾空白/引号/破折号差异）", fuzzyUsed)
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: msg}},
		Details: map[string]any{
			"path": t.Policy.Rel(abs), "edits": applied, "fuzzy": fuzzyUsed,
			"lineEnding": map[string]string{"\n": "LF", "\r\n": "CRLF"}[ending],
			"bom":        bom != "",
			"oldBytes":   len(raw), "newBytes": len(final),
		},
	}, nil
}

// fuzzyReplace 在 cur 上做一次模糊匹配替换。
//
// 返回值 matched=false 且 err=nil 表示「精确匹配也没命中」——
// 调用方据此给出更贴切的错误文案。
func fuzzyReplace(cur, oldText, newText string) (string, bool, error) {
	fuzzyCur := normalizeForFuzzyMatch(cur)
	fuzzyOld := normalizeForFuzzyMatch(oldText)
	if strings.TrimSpace(fuzzyOld) == "" {
		return cur, false, nil
	}
	switch n := strings.Count(fuzzyCur, fuzzyOld); {
	case n == 0:
		return cur, false, errFuzzyNotFound
	case n > 1:
		return cur, false, errFuzzyAmbiguous
	}
	idx := strings.Index(fuzzyCur, fuzzyOld)
	out, err := applyFuzzyReplace(cur, fuzzyCur, idx, len(fuzzyOld), newText)
	if err != nil {
		return cur, false, err
	}
	return out, true, nil
}

func parseEdits(a Args) ([]Edit, error) {
	var edits []Edit

	// 各种模型/各家 provider 的写法差异都要吃下来：
	//   edits: [{...}]           正常形态
	//   edits: "{...}" / "[...]" 把数组/对象序列化成了 JSON 字符串（Opus 4.6、GLM 见过）
	//   edits: {...}             单个对象没包成数组
	//   顶层 oldText/newText     旧版扁平参数
	appendRaw := func(raw any) error {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("edit 项必须是对象")
		}
		e := Args(m)
		oldText, ok := e.String("oldText")
		if !ok {
			return fmt.Errorf("缺少 oldText")
		}
		newText, ok := e.String("newText")
		if !ok {
			return fmt.Errorf("缺少 newText")
		}
		edits = append(edits, Edit{OldText: oldText, NewText: newText})
		return nil
	}

	switch v := a["edits"].(type) {
	case nil:
		// 只有扁平参数的情况，下面统一处理。
	case []any:
		for i, item := range v {
			if err := appendRaw(item); err != nil {
				return nil, fmt.Errorf("edits[%d]: %v", i, err)
			}
		}
	case map[string]any:
		if err := appendRaw(v); err != nil {
			return nil, err
		}
	case string:
		var arr []any
		if err := json.Unmarshal([]byte(v), &arr); err == nil {
			for i, item := range arr {
				if err := appendRaw(item); err != nil {
					return nil, fmt.Errorf("edits[%d]: %v", i, err)
				}
			}
			break
		}
		var one map[string]any
		if err := json.Unmarshal([]byte(v), &one); err != nil {
			return nil, fmt.Errorf("edits 是字符串但无法解析成数组或对象: %v", err)
		}
		if err := appendRaw(one); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("edits 必须是数组（收到 %T）", a["edits"])
	}

	// 旧版扁平参数：与 Pi 一样并入数组末尾，而不是二选一。
	if oldText, ok1 := a.String("oldText"); ok1 {
		if newText, ok2 := a.String("newText"); ok2 {
			edits = append(edits, Edit{OldText: oldText, NewText: newText})
		}
	}

	if len(edits) == 0 {
		return nil, errors.New("缺少参数 edits（或 oldText/newText）")
	}
	return edits, nil
}

// 模糊匹配的结果分类，供调用方给出更准确的提示。
var (
	errFuzzyNotFound  = errors.New("edit: 模糊匹配未命中")
	errFuzzyAmbiguous = errors.New("edit: 模糊匹配不唯一")
)

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:safeCutFromEnd(s, n)] + "…"
}

// ---------------------------------------------------------------------------
// ls
// ---------------------------------------------------------------------------

// LSTool 列目录。
type LSTool struct{ Policy Policy }

func NewLSTool(p Policy) *LSTool { return &LSTool{Policy: p} }

func (*LSTool) Name() string { return "ls" }
func (*LSTool) Description() string {
	return "列出目录内容（目录以 / 结尾）。用 glob 做模式匹配，用 grep 搜内容。"
}
func (*LSTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"path":  Prop("string", "目录路径，默认当前目录"),
		"all":   map[string]any{"type": "boolean", "description": "包含隐藏文件（. 开头）"},
		"long":  map[string]any{"type": "boolean", "description": "显示大小与修改时间"},
		"depth": PropInt("递归深度，默认 1", 1),
	})
}
func (*LSTool) ConcurrencySafe() bool { return true }
func (*LSTool) Mutates() bool         { return false }

func (t *LSTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	raw := a.Str("path", ".")
	abs, err := t.Policy.Resolve(raw)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return ErrorResult("列目录失败：%v", err), nil
	}
	_ = st
	all := a.Bool("all", false)
	long := a.Bool("long", false)
	depth := int(a.Int("depth", 1))
	if depth <= 0 {
		depth = 1
	}
	if depth > 5 {
		depth = 5
	}

	var b strings.Builder
	var count int
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过权限错误等
		}
		if p == abs {
			return nil
		}
		rel, _ := filepath.Rel(abs, p)
		lvl := strings.Count(rel, string(filepath.Separator)) + 1
		if lvl > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !all && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && (name == "node_modules" || name == ".git") && !all {
			return filepath.SkipDir
		}
		count++
		indent := strings.Repeat("  ", lvl-1)
		suffix := ""
		if d.IsDir() {
			suffix = "/"
		}
		if long {
			info, ierr := d.Info()
			if ierr == nil {
				fmt.Fprintf(&b, "%s%s%s  %10d  %s\n", indent, name, suffix, info.Size(), info.ModTime().Format("2006-01-02 15:04"))
			} else {
				fmt.Fprintf(&b, "%s%s%s\n", indent, name, suffix)
			}
		} else {
			fmt.Fprintf(&b, "%s%s%s\n", indent, name, suffix)
		}
		if count > 5000 {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return ErrorResult("列目录失败：%v", err), nil
	}
	out := b.String()
	if out == "" {
		out = "(空目录)"
	}
	if count > 5000 {
		out += "\n[条目过多已截断，请缩小范围]"
	}
	out = t.Policy.Rel(abs) + ":\n" + out
	return Result{
		Content: []Block{{Kind: KindText, Text: out}},
		Details: map[string]any{"path": t.Policy.Rel(abs), "count": count},
	}, nil
}

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

// GlobTool 按通配符查找文件。
type GlobTool struct{ Policy Policy }

func NewGlobTool(p Policy) *GlobTool { return &GlobTool{Policy: p} }

func (*GlobTool) Name() string { return "glob" }
func (*GlobTool) Description() string {
	return "按 glob 模式查找文件（如 **/*.go、src/**/*_test.go）。按修改时间倒序返回，便于找最近改动。"
}
func (*GlobTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"pattern": Prop("string", "glob 模式，** 匹配任意层级"),
		"path":    Prop("string", "搜索根目录，默认工作目录"),
		"limit":   PropInt("最多返回条数", 200),
	}, "pattern")
}
func (*GlobTool) ConcurrencySafe() bool { return true }
func (*GlobTool) Mutates() bool         { return false }

func (t *GlobTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	pattern, err := a.Required("pattern")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	root, err := t.Policy.Resolve(a.Str("path", "."))
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	limit := int(a.Int("limit", 200))
	if limit <= 0 {
		limit = 200
	}
	matches, truncated, err := globFiles(ctx, root, pattern, limit)
	if err != nil {
		return ErrorResult("查找失败：%v", err), nil
	}
	if len(matches) == 0 {
		return Result{Content: []Block{{Kind: KindText, Text: "没有匹配的文件: " + pattern}}}, nil
	}
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(t.Policy.Rel(m) + "\n")
	}
	if truncated {
		fmt.Fprintf(&b, "[结果超过 %d 条，已截断]\n", limit)
	}
	return Result{
		Content:   []Block{{Kind: KindText, Text: b.String()}},
		Truncated: truncated,
		Details:   map[string]any{"count": len(matches), "pattern": pattern},
	}, nil
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

// GrepTool 在文件内容中搜索正则。
type GrepTool struct{ Policy Policy }

func NewGrepTool(p Policy) *GrepTool { return &GrepTool{Policy: p} }

func (*GrepTool) Name() string { return "grep" }
func (*GrepTool) Description() string {
	return "在文件内容中搜索正则表达式，输出 文件:行号: 内容。用于定位符号定义/引用。" +
		"只需要文件名时用 glob；需要读全文时用 read。"
}
func (*GrepTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"pattern":    Prop("string", "Go 正则表达式"),
		"path":       Prop("string", "搜索路径（文件或目录），默认工作目录"),
		"glob":       Prop("string", "只搜匹配该 glob 的文件，如 *.go"),
		"ignoreCase": map[string]any{"type": "boolean", "description": "忽略大小写"},
		"context":    PropInt("输出的上下文行数", 0),
		"limit":      PropInt("最多返回匹配数", 200),
	}, "pattern")
}
func (*GrepTool) ConcurrencySafe() bool { return true }
func (*GrepTool) Mutates() bool         { return false }

func (t *GrepTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	pattern, err := a.Required("pattern")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	expr := pattern
	if a.Bool("ignoreCase", false) {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return ErrorResult("正则无效：%v", err), nil
	}
	root, err := t.Policy.Resolve(a.Str("path", "."))
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	limit := int(a.Int("limit", 200))
	if limit <= 0 {
		limit = 200
	}
	ctxLines := int(a.Int("context", 0))
	globPat := a.Str("glob", "")

	files, err := collectFiles(ctx, root, globPat)
	if err != nil {
		return ErrorResult("遍历失败：%v", err), nil
	}

	var b strings.Builder
	hits, fileHits := 0, 0
	truncated := false
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		data, err := os.ReadFile(f)
		if err != nil || len(data) == 0 {
			continue
		}
		if isBinary(data) {
			continue
		}
		lines := strings.Split(string(data), "\n")
		matchedInFile := false
		for i, ln := range lines {
			if !re.MatchString(ln) {
				continue
			}
			if hits >= limit {
				truncated = true
				break
			}
			if !matchedInFile {
				matchedInFile = true
				fileHits++
				if fileHits > 1 {
					b.WriteString("\n")
				}
			}
			rel := t.Policy.Rel(f)
			lo, hi := i-ctxLines, i+ctxLines
			if lo < 0 {
				lo = 0
			}
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for j := lo; j <= hi; j++ {
				mark := ":"
				if j == i {
					mark = ":"
				}
				fmt.Fprintf(&b, "%s%s%d:%s\n", rel, mark, j+1, clip(lines[j], 400))
			}
			hits++
		}
		if truncated {
			break
		}
	}
	if hits == 0 {
		return Result{
			Content: []Block{{Kind: KindText, Text: fmt.Sprintf("没有匹配: %s（扫描 %d 个文件）", pattern, len(files))}},
			Details: map[string]any{"files": len(files)},
		}, nil
	}
	out := b.String()
	out, t2 := Truncate(out, 0, 0)
	return Result{
		Content:   []Block{{Kind: KindText, Text: out}},
		Truncated: truncated || t2,
		Details:   map[string]any{"hits": hits, "files": fileHits},
	}, nil
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// skipDirs 是遍历时默认跳过的目录。
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "target": true, "__pycache__": true,
	".venv": true, ".idea": true, ".cache": true,
}

// collectFiles 收集 root 下的文件（可按 glob 过滤）。
func collectFiles(ctx context.Context, root, globPat string) ([]string, error) {
	var out []string
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{root}, nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if globPat != "" {
			ok, _ := filepath.Match(globPat, d.Name())
			if !ok {
				// 支持 ** 形式：对相对路径再匹配一次
				rel, _ := filepath.Rel(root, p)
				ok2, _ := matchDoubleStar(globPat, rel)
				if !ok2 {
					return nil
				}
			}
		}
		out = append(out, p)
		return nil
	})
	return out, err
}
