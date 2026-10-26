package segmenter

import (
	_ "embed"
	"strings"

	"github.com/go-ego/gse"

	"github.com/leijux/acp-agent/internal/kernel"
)

// 词表由 `go generate` 调用 script/trimdict 从 gse 自带词典裁剪而来：
// 按词频降序取前 N 行、原样保留 `词 频率 词性`。**只带简体**（不编入繁体/日文表）。
//
// 为什么不直接用 gse.LoadDictEmbed：gse 的 go:embed 里同时有简体 4.9MB、
// 繁体 3.4MB、日文 22.6MB 和 idf 5.9MB，只要 LoadDictEmbed 可达，linker
// 就会全部保留 —— 我们只用简体，却要付 +31MB 二进制。改成自带词表 +
// LoadDictStr 后只多 1.34MB，加载从 ~210ms/105MB 降到 ~50ms/29MB。
// 构建时必须同时带 gse 的 `ne` tag 才会真正丢掉它的 embed，见 cmd/acp-agent
// （`task build:full`）。
//
//go:generate go run ../../script/trimdict -lang s -n 100000 -out dict_zh_s.txt
var (
	//go:embed dict_zh_s.txt
	dictZhS string
)

// HansDictWords 是简体词表条数（改这里要同步上面的 //go:generate 参数；
// 词表条数与 golden 测试会盯住切分结果）。
const HansDictWords = 100000

// GSE 用 go-ego/gse + 自带高频简体词表做中文分词，纯 Go、无 cgo、零外部文件。
//
// 只认简体：繁体文本会退化成单字，由 kernel 的 cjkRunTokens 走 bigram 回退
// （开启 WithExpandBigram 时还能保住跨字召回，但拿不到繁体词）。
//
// 并发：Segment 只读 gse 的词典（jumper/tokens 都是局部变量），
// 16 goroutine × 200 次 `go test -race` 无告警，可以挂在进程级 Kernel 上共享。
type GSE struct {
	seg gse.Segmenter
	// expand 为 true 时，在多字词之外补该 CJK run 的 bigram。见 WithExpandBigram。
	expand bool
}

type gseOptions struct {
	expand bool
}

// GSEOption 调整 NewGSE 的行为。
type GSEOption func(*gseOptions)

// WithExpandBigram 让 Segment 在词之外补上该 CJK run 的 bigram。
//
// 起因：kernel 的 tokenizer 里 cjkRunTokens 只要 run 内有多字词就**只返回词**
// （`segs.filter(w => w.length >= 2)`），单字和跨词边界的 bigram 全被丢掉。
// 于是接真分词器反而丢召回：`壓縮引擎` 只剩 `[引擎]`（`壓 縮` 全丢），
// 查询 `缩引` 命中 0 篇。补 bigram 后 kernel 同时拿到词（高 idf，精度）和
// bigram（召回）：
//
//	壓縮引擎：只有词 → [引擎]；补 bigram → [引擎 壓縮 縮引]
//
// 只在该 run 含多字词时才补，避免与 kernel 自己的 bigram 回退重复计数。
func WithExpandBigram() GSEOption { return func(o *gseOptions) { o.expand = true } }

// NewGSE 加载内置高频简体词表；WithExpandBigram() 打开 bigram 补齐。
//
// 加载是 CPU/内存密集型的（一次性，约 29MB 分配），调用方应复用同一个实例 ——
// kernel 会把它挂在进程级 Kernel 上，所有会话共享。
func NewGSE(opts ...GSEOption) (*GSE, error) {
	var o gseOptions
	for _, fn := range opts {
		fn(&o)
	}

	var seg gse.Segmenter
	// 关掉 gse 打到 stderr 的日志：ACP 模式下 stderr 是给用户看子代理输出用的。
	seg.SkipLog = true
	seg.SkipPos = true
	// SkipSubSeg 不为 searchMode 预先算子词，节省约 1/4 的词典内存，
	// 且对 kernel 用的 Segment（非搜索模式）没有行为差异 —— 实测分词结果逐字符相同。
	seg.SkipSubSeg = true

	// 用 LoadDictStr（字符串路径，纯解析），不用 LoadDict（文件路径走 bufio +
	// 每行一次 Fscanln）：同一份词表实测 50ms/29MB vs 149ms/64MB。
	dict := strings.TrimRight(dictZhS, "\n") + "\n"
	if err := seg.LoadDictStr(dict); err != nil {
		return nil, err
	}
	return &GSE{seg: seg, expand: o.expand}, nil
}

// Segment 实现 kernel.Segmenter。
func (g *GSE) Segment(text string) []kernel.Segment {
	if text == "" {
		return nil
	}
	raw := g.seg.Segment([]byte(text))
	out := make([]kernel.Segment, 0, len(raw))
	idx := runeIndex{text: text}
	for i := range raw {
		seg := &raw[i]
		tok := seg.Token()
		if tok == nil {
			continue
		}
		w := tok.Text()
		if w == "" {
			continue
		}
		out = append(out, kernel.Segment{Text: w, Index: idx.at(seg.Start())})
	}
	if g.expand {
		out = expandCJKRunBigrams(out)
	}
	return out
}

// expandCJKRunBigrams 在含多字词的 CJK run 内补上跨词边界的 bigram。
//
// 输出保持 Index 非递减（与 kernel.Segmenter 契约一致）：每个段后面紧跟以它
// 最后一个字开头的 bigram，Index 就是该段的 Index。
//
// 只在 run 内存在长度 ≥2 的段时才补 —— 否则 kernel 会自己走 bigram 回退，
// 我们再补一遍只会让 tf 翻倍。
func expandCJKRunBigrams(segs []kernel.Segment) []kernel.Segment {
	out := make([]kernel.Segment, 0, len(segs)+len(segs)/2)
	var run []kernel.Segment
	flush := func() {
		if len(run) == 0 {
			return
		}
		multi := false
		for _, s := range run {
			if len([]rune(s.Text)) >= 2 {
				multi = true
				break
			}
		}
		for i, s := range run {
			out = append(out, s)
			if !multi || i+1 >= len(run) {
				continue
			}
			last := []rune(s.Text)
			next := []rune(run[i+1].Text)
			if len(last) == 0 || len(next) == 0 {
				continue
			}
			out = append(out, kernel.Segment{
				Text:  string(last[len(last)-1]) + string(next[0]),
				Index: s.Index,
			})
		}
		run = run[:0]
	}
	for _, s := range segs {
		if isCJKSegment(s.Text) {
			run = append(run, s)
			continue
		}
		flush()
		out = append(out, s)
	}
	flush()
	return out
}

// isCJKSegment 判断段是否含 CJK 字符，口径与 kernel 的 CJK 正则一致
// （汉字/假名/谚文）。kernel 用 CJK.test 决定 run 的边界，这里必须同口径，
// 否则补出来的 bigram 会落到别的 run 里。
func isCJKSegment(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x3400 && r <= 0x4DBF, // CJK Ext A
			r >= 0x4E00 && r <= 0x9FFF, // CJK Unified
			r >= 0xF900 && r <= 0xFAFF, // CJK Compatibility
			r >= 0x3040 && r <= 0x30FF, // Hiragana + Katakana
			r >= 0xAC00 && r <= 0xD7AF: // Hangul
			return true
		}
	}
	return false
}

// String 便于日志/诊断。
func (g *GSE) String() string {
	if g.expand {
		return "gse+bigram"
	}
	return "gse"
}
