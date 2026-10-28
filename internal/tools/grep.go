package tools

// grep 工具：并发扫描 + 字面量预筛 + 零拷贝行切分。
//
// 为什么单独成文件：这段是热路径（模型最常调的工具之一），逻辑比 ls/read
// 复杂得多，混在 builtin.go 里既难读也难改。
//
// 三个设计约束，改之前先读：
//
//  1. **结果顺序必须稳定**。同一查询两次跑出不同顺序会污染 prompt 缓存，
//     也让「为什么这次没这条」无法解释。所以 worker 只写自己的下标，合并
//     时按文件序号归并，不做「谁先完成谁先输出」。
//  2. **limit 命中不早停**。早停意味着「哪些文件被扫」取决于调度顺序，
//     结果不确定。代价是多扫一些文件，换来可复现。
//  3. **worker 数不与工具层的 MaxParallel 相乘**。8 个并发 grep 调用
//     × 4 worker = 32 个 goroutine 同时读盘，机械盘上会互相拖慢。

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	// grepMaxWorkers 是单次搜索的并发上限。再高在机械盘/网络盘上只会互相拖慢。
	grepMaxWorkers = 4
	// grepSerialBelow：文件数少于此值时串行更快 —— goroutine + channel 的
	// 固定开销大于并行收益（小仓库是常态，别让优化变成倒退）。
	grepSerialBelow = 64
	// grepMaxFileBytes：超过这个大小的文件跳过并计数。全量读入是内存风险，
	// 流式实现又撑不起「上下文行」的语义，明确跳过比 OOM 好。
	grepMaxFileBytes = 64 << 20
	// grepLineClip：单行输出的字符上限（minified 文件、压缩过的日志）。
	grepLineClip = 400
)

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

// grepFileEntry 是遍历阶段收集的一个候选文件。
//
// 带上 size 是为了让扫描阶段免掉一次 os.Stat，并且能在读之前就跳过超大文件。
type grepFileEntry struct {
	path string
	size int64
}

// grepFileResult 是单个文件的扫描结果。
//
// blocks 里每条元素对应「一处命中 + 它的上下文行」，已经渲染成最终文本；
// worker 内部就把它截到 limit 条 —— 单文件的命中数不可能超过全局 limit，
// 这样并发时内存有界（workers × limit 条）。
type grepFileResult struct {
	blocks    []string
	hits      int
	tooBig    bool
	truncated bool // 本文件命中数超过 limit，有内容没输出
}

// grepScan 承载一次搜索的编译结果与参数（只读，可在多个 worker 间共享）。
type grepScan struct {
	re       *regexp.Regexp
	prefix   []byte // LiteralPrefix 非空时的整文件预筛串；nil 表示不预筛
	limit    int
	ctxLines int
	policy   Policy
}

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
	g := &grepScan{
		re:       re,
		limit:    limit,
		ctxLines: int(a.Int("context", 0)),
		policy:   t.Policy,
	}
	// 字面量预筛：正则里必须出现的固定前缀，用它先过一遍整个文件。
	// 命中稀疏时（典型代码库）能省掉绝大部分逐行正则；`(?i)` 开头时
	// LiteralPrefix 返回空，天然不会误判。
	if p, _ := re.LiteralPrefix(); p != "" {
		g.prefix = []byte(p)
	}

	files, err := collectGrepFiles(ctx, root, a.Str("glob", ""))
	if err != nil {
		return ErrorResult("遍历失败：%v", err), nil
	}
	results := g.scanAll(ctx, files)

	var b strings.Builder
	hits, fileHits, tooBig := 0, 0, 0
	truncated := false
	for _, r := range results {
		if r.tooBig {
			tooBig++
		}
		if r.truncated {
			truncated = true
		}
	}
	for _, r := range results {
		if len(r.blocks) == 0 {
			continue
		}
		if hits >= limit {
			truncated = true
			break
		}
		if fileHits > 0 {
			b.WriteString("\n")
		}
		fileHits++
		for _, blk := range r.blocks {
			if hits >= limit {
				truncated = true
				break
			}
			b.WriteString(blk)
			hits++
		}
	}

	var note string
	if tooBig > 0 {
		note = fmt.Sprintf("\n[跳过 %d 个超过 %dMB 的文件；大文件请用 read 分页，或 bash 里的外部 grep]",
			tooBig, grepMaxFileBytes>>20)
	}
	if hits == 0 {
		return Result{
			Content: []Block{{Kind: KindText, Text: fmt.Sprintf("没有匹配: %s（扫描 %d 个文件）%s",
				pattern, len(files), note)}},
			Details: map[string]any{"files": len(files), "skippedBig": tooBig},
		}, nil
	}

	out := b.String()
	if note != "" {
		out = strings.TrimRight(out, "\n") + note + "\n"
	}
	out, t2 := Truncate(out, 0, 0)
	return Result{
		Content:   []Block{{Kind: KindText, Text: out}},
		Truncated: truncated || t2,
		Details:   map[string]any{"hits": hits, "files": fileHits, "skippedBig": tooBig},
	}, nil
}

// scanAll 扫描全部文件，返回与 files 同序的结果。
func (g *grepScan) scanAll(ctx context.Context, files []grepFileEntry) []grepFileResult {
	results := make([]grepFileResult, len(files))
	workers := grepMaxWorkers
	if n := runtime.NumCPU(); n < workers {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	if len(files) < grepSerialBelow || workers == 1 {
		for i, f := range files {
			if ctx.Err() != nil {
				break
			}
			results[i] = g.scanFile(ctx, f)
		}
		return results
	}

	// 每个 worker 只写自己的下标，主 goroutine 等所有人结束后再合并 ——
	// 不需要锁，也不会出现「谁先完成谁先输出」的不确定性。
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = g.scanFile(ctx, files[i])
			}
		}()
	}
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

// scanFile 扫描单个文件。
func (g *grepScan) scanFile(ctx context.Context, f grepFileEntry) grepFileResult {
	var res grepFileResult
	if f.size > grepMaxFileBytes {
		res.tooBig = true
		return res
	}
	if f.size <= 0 {
		return res
	}
	data, err := os.ReadFile(f.path)
	if err != nil || len(data) == 0 {
		return res
	}
	if isBinary(data) {
		return res
	}
	if g.prefix != nil && !bytes.Contains(data, g.prefix) {
		return res
	}
	g.scanLines(ctx, g.policy.Rel(f.path), splitLines(data), &res)
	return res
}

// splitLines 只切切片头，不复制内容（旧实现 strings.Split(string(data)) 会
// 复制两遍内容，内存约为文件的 3 倍）。CRLF 的 \r 在这里剥掉，输出再补。
func splitLines(data []byte) [][]byte {
	out := make([][]byte, 0, len(data)/40+1)
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		out = append(out, line)
		start = i + 1
	}
	if start < len(data) {
		line := data[start:]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		out = append(out, line)
	}
	return out
}

// scanLines 逐行匹配并渲染。
//
// 上下文行用 `-` 分隔（`文件-行号-内容`），匹配行用 `:` —— 与 GNU grep 一致，
// 模型能一眼看出哪行是命中。相邻命中的上下文区段重叠时去重，只输出一次。
func (g *grepScan) scanLines(ctx context.Context, rel string, lines [][]byte, res *grepFileResult) {
	lastEnd := -1
	for i := 0; i < len(lines); i++ {
		if i&1023 == 0 && ctx.Err() != nil {
			return
		}
		if !g.re.Match(lines[i]) {
			continue
		}
		if len(res.blocks) >= g.limit {
			// 已经装满 limit 条：剩下的不再渲染，但要如实标截断。
			res.truncated = true
			return
		}
		lo, hi := i-g.ctxLines, i+g.ctxLines
		if lo < 0 {
			lo = 0
		}
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		start := lo
		if start <= lastEnd {
			start = lastEnd + 1
		}
		if start > hi {
			// 整段（含命中行）已经被上一个命中的上下文输出过了。
			continue
		}
		var b strings.Builder
		for j := start; j <= hi; j++ {
			sep := byte('-')
			if j == i {
				sep = ':'
			}
			b.WriteString(rel)
			b.WriteByte(sep)
			b.WriteString(strconv.Itoa(j + 1))
			b.WriteByte(sep)
			b.WriteString(clip(string(lines[j]), grepLineClip))
			b.WriteByte('\n')
		}
		lastEnd = hi
		res.blocks = append(res.blocks, b.String())
		res.hits++
	}
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

// collectGrepFiles 收集 root 下的候选文件（可按 glob 过滤），带上大小。
func collectGrepFiles(ctx context.Context, root, globPat string) ([]grepFileEntry, error) {
	var out []grepFileEntry
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []grepFileEntry{{path: root, size: st.Size()}}, nil
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
		var size int64
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
		}
		out = append(out, grepFileEntry{path: p, size: size})
		return nil
	})
	return out, err
}
