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
//   - 二进制体积：gse 把简体/繁体/日文词典 + idf 表 embed 进包，约 +15MB；
//   - 启动开销：首次加载词典约几百毫秒（只加载一次，进程级共享）；
//   - 结果依赖词典版本：分词结果会随库升级变化，进而影响检索召回，
//     但**不影响上下文压缩的正确性**（压缩切点/渲染视图不依赖分词）。
//
// Index 语义：kernel.Segment.Index 是 **rune 偏移**（与 kernel.RuneSegmenter 一致），
// 不是 JS Intl.Segmenter 的 UTF-16 码元偏移。kernel 自身只用 Segment.Text，
// 不用 Index；自定义实现时请保持 rune 偏移，否则星平面字符（emoji、CJK Ext B）
// 上会与 RuneSegmenter 不一致。
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
var _ kernel.Segmenter = (*Sego)(nil)
