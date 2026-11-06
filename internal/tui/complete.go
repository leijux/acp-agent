package tui

import (
	"fmt"
	"strings"
)

// completeInput 处理 Tab：补 @路径。
//
// 只有一条规则值得记住：**唯一候选直接替换，多候选补到最长公共前缀**。
// 不上浮层（V1）：候选清单放在状态行里，既不用抢视口行（viewString 必须严格
// 等于终端高度），也不会跟 ↑/↓ 的历史导航抢键。
func (m *Model) completeInput() {
	if m.complete == nil || m.perm != nil {
		return
	}
	line := string(m.in.buf)
	cands, start, end := m.complete(line, m.in.cur)
	if len(cands) == 0 {
		m.status = "没有匹配的 @路径"
		return
	}
	if len(cands) == 1 {
		if m.in.replaceRange(start, end, cands[0]) {
			m.status = ""
		}
		return
	}
	prefix := commonPrefix(cands)
	if typed := line[start:end]; len(prefix) > len(typed) {
		m.in.replaceRange(start, end, prefix)
	}
	m.status = fmt.Sprintf("%d 个候选：%s", len(cands), strings.Join(headN(cands, 4), "  "))
}

// commonPrefix 返回候选的最长公共前缀。
func commonPrefix(cands []string) string {
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

func headN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
