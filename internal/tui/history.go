package tui

// 输入历史：本进程内提交过的 prompt，纯内存。
//
// 刻意不落盘（与 bash/readline 不同）：输入历史里经常出现一次性的敏感命令，
// 攒成文件就要额外管权限、清理与跨项目串味；重启后丢历史是可以接受的代价。
// 需要找回上次会话发过什么时，用 `-c` 恢复会话（那是对话历史，不是输入历史）。
//
// 语义对齐 readline / fish：
//
//   - 空白行与**以空格开头的行**不记录（bash 惯例：临时敏感命令不进历史）；
//   - 同一条已经在历史里就移到最新位置，不留重复副本（连续按 ↑ 不会反复看到同一条）；
//   - 上限 historyMax 条，超出丢最旧的；
//   - 前缀非空时只在「以该前缀开头」的历史里跳（fish 的向上搜索）——
//     敲了 `git ` 再按 ↑ 只翻 git 命令，比逐条翻五百条实用得多。

import "strings"

// historyMax 是内存里保留的输入条数上限。

const historyMax = 500

// History 保存输入历史（旧 → 新）。
type History struct {
	items []string
}

// Len 返回条数。
func (h *History) Len() int { return len(h.items) }

// At 取第 i 条。
func (h *History) At(i int) (string, bool) {
	if i < 0 || i >= len(h.items) {
		return "", false
	}
	return h.items[i], true
}

// Push 记录一条输入。
func (h *History) Push(s string) {
	if strings.TrimSpace(s) == "" || strings.HasPrefix(s, " ") {
		return
	}
	// 已存在则先摘掉，再追加到末尾 —— 保持「最近的永远在最新」。
	for i, old := range h.items {
		if old == s {
			h.items = append(h.items[:i], h.items[i+1:]...)
			break
		}
	}
	h.items = append(h.items, s)
	if len(h.items) > historyMax {
		h.items = append([]string(nil), h.items[len(h.items)-historyMax:]...)
	}
}

// Prev 从 pos 往前找第一个匹配 prefix 的条目。
//
// pos == -1 表示「还没进历史」，从最新一条开始找；找不到时返回 ok=false 且
// pos 不变（调用方保持当前输入不动）。
func (h *History) Prev(pos int, prefix string) (text string, newPos int, ok bool) {
	start := len(h.items) - 1
	if pos >= 0 {
		start = pos - 1
	}
	for i := start; i >= 0; i-- {
		if prefix == "" || strings.HasPrefix(h.items[i], prefix) {
			return h.items[i], i, true
		}
	}
	return "", pos, false
}

// Next 从 pos 往后找第一个匹配 prefix 的条目。
//
// 已经是最后一条（或后面没有匹配项）时返回 ok=false —— 调用方这时应该把
// 用户按 ↑ 之前正在编辑的草稿放回去（readline 行为），而不是留在一个旧条目上。
func (h *History) Next(pos int, prefix string) (text string, newPos int, ok bool) {
	if pos < 0 {
		return "", pos, false
	}
	for i := pos + 1; i < len(h.items); i++ {
		if prefix == "" || strings.HasPrefix(h.items[i], prefix) {
			return h.items[i], i, true
		}
	}
	return "", pos, false
}
