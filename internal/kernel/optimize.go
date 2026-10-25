package kernel

import "sort"

// Go 侧 token 计数（② 的核心优化）。
//
// acp-kernel 的默认计数是 JS 里的：
//
//	function defaultCountTokens(text) {
//	  const cjk = text.match(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g);
//	  const cjkCount = cjk?.length ?? 0;
//	  return cjkCount + Math.ceil((text.length - cjkCount) / 4);
//	}
//
// 这条正则对每条消息、每个压缩块都会跑一次，而 goja 的 String.prototype.match
// 走的是 regexp2 回溯引擎 —— pprof 显示它占 Turn 的 78% 分配、33% CPU。
// kernel 允许从宿主注入 countTokens，这里用 Go 精确复刻同一算法，
// 与 JS 版逐字节等价，但快两个数量级。

// CountTokens 与 acp-kernel 的 defaultCountTokens 完全等价：
// CJK 字符各计 1，其余按 UTF-16 code unit 每 4 个计 1（向上取整）。
func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk := 0
	units := 0
	for _, r := range text {
		// JS 的 text.length 是 UTF-16 code unit 数：增补平面字符算 2。
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		// 与上游正则字符类一致（不含增补平面 CJK 扩展 B+）。
		if r >= 0x4E00 && r <= 0x9FFF ||
			r >= 0x3040 && r <= 0x30FF ||
			r >= 0xAC00 && r <= 0xD7AF {
			cjk++
		}
	}
	rest := units - cjk
	return cjk + (rest+3)/4
}

// EstimateTokens 估算一组消息的 token 用量，用于调用 Turn 前填充 tokenCount。
//
// 算法是 kernel 的 countMessageTokens：sum(countTokens(text)) + sum(thinkingTokens)。
// 因为 CountTokens 与上游等价，这个估算与 kernel 自己算出来的口径一致；
// 差别只在于 kernel 会把压缩块的摘要也算进来。
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += CountTokens(m.Text)
		n += m.ThinkingTokens
	}
	return n
}

// FindRange 把 nudge 报的 ref 区间映射回具体的 Message 切片，
// 让调用方知道这段压缩到底吃掉了哪些消息（便于自己写摘要）。
func FindRange(msgs []Message, r NudgeRange) []Message {
	startIdx, endIdx := -1, -1
	for i, m := range msgs {
		if m.Ref == r.StartRef && startIdx < 0 {
			startIdx = i
		}
		if m.Ref == r.EndRef {
			endIdx = i
		}
	}
	if startIdx < 0 || endIdx < startIdx {
		return nil
	}
	return msgs[startIdx : endIdx+1]
}

// SortedRefs 返回消息的稳定 ref 列表（去空、升序）。
func SortedRefs(msgs []Message) []string {
	refs := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Ref != "" {
			refs = append(refs, m.Ref)
		}
	}
	sort.Strings(refs)
	return refs
}
