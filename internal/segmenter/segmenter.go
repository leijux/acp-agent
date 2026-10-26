// Package segmenter 给 internal/kernel 提供基于真实词典的中文分词器。
//
// 背景：kernel 上游用 `new Intl.Segmenter("zh", {granularity:"word"})` 切分中文
// 检索词，goja 没有 Intl，默认回退到 kernel.RuneSegmenter（单个汉字一段 +
// CJK run 内 bigram 回退）。单字粒度能保证检索召回，但拿不到"词"，
// 中文 BM25 的精度不如 Node + CLDR。
//
// 这里的实现通过 kernel.WithSegmenter 注入，让 kernel 拿到真正的词：
//
//	k, err := kernel.New(kernel.WithSegmenter(seg))
//
// 与 cmd/acp-agent 的接线见 agent.SetKernelOptions。
//
// 代价（选之前先看清楚）：
//   - 二进制体积：自带高频简体词表被 go:embed，+1.34MB。
//     **构建必须同时带 gse 的 `ne` tag**（`task build:full`），否则 gse 自己的
//     embed（简/繁/日词典 + idf，共 ~31MB）仍会被 linker 保留，二进制会是
//     52MB 而不是 22MB；
//   - 启动开销：简体词表约 50ms / 29MB 分配（只加载一次，进程级共享）；
//   - 结果依赖词表版本：分词结果会随 gse 升级或词表重新裁剪而变，进而影响
//     检索召回，但**不影响上下文压缩的正确性**（压缩切点/渲染视图不依赖分词）；
//   - 召回陷阱：kernel 的 cjkRunTokens 只要 run 内有多字词就丢弃全部单字段，
//     直通分词结果会丢跨词边界/繁体的召回。用 WithExpandBigram() 补 bigram
//     （cmd 默认接线就是这么做的），详见 gse.go。
//
// Index 语义：kernel.Segment.Index 是 **rune 偏移**（与 kernel.RuneSegmenter 一致），
// 不是 JS Intl.Segmenter 的 UTF-16 码元偏移。kernel 自身只用 Segment.Text，
// 不用 Index；自定义实现时请保持 rune 偏移且非递减，否则星平面字符（emoji、
// CJK Ext B）上会与 RuneSegmenter 不一致。
package segmenter

import (
	"unicode/utf8"

	"github.com/leijux/acp-agent/internal/kernel"
)

// runeIndex 把「字节偏移」换成「rune 偏移」，按调用顺序增量推进。
//
// 分词库返回的都是字节位置（UTF-8），而 kernel 的契约是 rune 索引。
// 直接对每个段做 utf8.RuneCountInString(text[:pos]) 是 O(n²)，这里只数
// 相邻两次位置之间的一小段。
type runeIndex struct {
	text    string
	bytePos int
	runePos int
}

// at 返回字节位置 bytePos 对应的 rune 偏移。
func (r *runeIndex) at(bytePos int) int {
	if bytePos < r.bytePos { // 分词器理论上不会回退，保险起见重置
		r.bytePos, r.runePos = 0, 0
	}
	if bytePos > len(r.text) {
		bytePos = len(r.text)
	}
	r.runePos += utf8.RuneCountInString(r.text[r.bytePos:bytePos])
	r.bytePos = bytePos
	return r.runePos
}

// runeLen 返回文本的 rune 总数（用于边界断言/调试）。
func runeLen(s string) int { return utf8.RuneCountInString(s) }

var _ kernel.Segmenter = (*GSE)(nil)
