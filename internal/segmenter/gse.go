package segmenter

import (
	"fmt"
	"strings"

	"github.com/go-ego/gse"

	"github.com/leijux/acp-agent/internal/kernel"
)

// GSE 用 go-ego/gse 的**内置词典**做中文分词，零外部文件依赖。
//
// 这是推荐选项：纯 Go、无 cgo，交叉编译不受影响，代价是词典被 embed 进
// 二进制（约 +15MB）以及首次加载词典的几百毫秒。
//
// 默认只加载**简体**词典：实测加载 453ms / 192MB 分配，而简 + 繁要 811ms / 295MB。
// 繁体文本在只有简体词典时会退化成单字，kernel 的 cjkRunTokens 发现没有
// 多字词后会自动回到 bigram 回退 —— 召回不丢，只是繁体失去词级精度。
// 需要繁体词时用 NewGSE(DictZhHansHant)。
type GSE struct {
	seg gse.Segmenter
}

// 词典选择（对应 gse.LoadDictEmbed 的参数）。
const (
	// DictZhHans 只要简体词典。
	DictZhHans = "zh_s"
	// DictZhHansHant 简体 + 繁体（加载更慢、内存更多）。
	DictZhHansHant = "zh"
)

// NewGSE 加载 gse 内置词典；dict 为空时用简体（DictZhHans）。
//
// 词典加载是 CPU/内存密集型的（一次性），调用方应复用同一个实例 ——
// kernel 会把它挂在进程级 Kernel 上，所有会话共享。
func NewGSE(dict ...string) (*GSE, error) {
	name := DictZhHans
	if len(dict) > 0 && strings.TrimSpace(dict[0]) != "" {
		name = dict[0]
	}

	var seg gse.Segmenter
	// 关掉 gse 打到 stderr 的日志：ACP 模式下 stderr 是给用户看子代理输出用的。
	seg.SkipLog = true
	seg.SkipPos = true
	// SkipSubSeg 不为 searchMode 预先算子词，节省约 1/4 的词典内存，
	// 且对 kernel 用的 Segment（非搜索模式）没有行为差异 —— 实测分词结果逐字符相同。
	seg.SkipSubSeg = true
	if err := seg.LoadDictEmbed(name); err != nil {
		return nil, fmt.Errorf("segmenter: 加载 gse 内置词典 %q 失败: %w", name, err)
	}
	return &GSE{seg: seg}, nil
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
	return out
}

// String 便于日志/诊断。
func (g *GSE) String() string { return "gse" }
