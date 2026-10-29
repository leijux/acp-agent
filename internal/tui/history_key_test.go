package tui

// 输入历史在 TUI 层的按键语义：
//   - ↑ 取上一条（空历史时无反应，不会把输入行弄乱）
//   - ↓ 到下一条，到最新一条后回到按 ↑ 之前的草稿
//   - 前缀过滤：敲了内容再按 ↑ 只在匹配的历史里翻
//   - 提交后才进历史；提交会退出导航

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// press 用现成的 key()/send() helper 送一个功能键。
func press(m *Model, t tea.KeyType) { send(m, key(t)) }

func TestTUIHistoryUpDown(t *testing.T) {
	m, _ := newTestModel(100, 20)

	// 没有历史时按 ↑：输入行保持原样
	typeText(m, "draft in progress")
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "draft in progress" {
		t.Fatalf("无历史时 ↑ 不该改动输入，实际 %q", got)
	}

	// 提交两条，历史里就有它们
	m.in.reset()
	typeText(m, "first command")
	press(m, tea.KeyEnter)
	typeText(m, "second command")
	press(m, tea.KeyEnter)
	if m.hist.Len() != 2 {
		t.Fatalf("提交后历史应有 2 条，实际 %d", m.hist.Len())
	}

	// 敲一半再按 ↑：先回到最近一条，草稿被记住
	typeText(m, "thi")
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "second command" {
		t.Fatalf("↑ 应取最近一条，实际 %q", got)
	}
	if m.histPos != 1 {
		t.Fatalf("histPos = %d, want 1", m.histPos)
	}
	// 再 ↑ 到更早一条
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "first command" {
		t.Fatalf("再 ↑ 应取更早一条，实际 %q", got)
	}
	// 到顶后继续 ↑：不动
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "first command" {
		t.Fatalf("到顶后 ↑ 不该改动，实际 %q", got)
	}

	// ↓ 回下来，最后回到草稿
	press(m, tea.KeyDown)
	if got := m.in.text(); got != "second command" {
		t.Fatalf("↓ 应回下一条，实际 %q", got)
	}
	press(m, tea.KeyDown)
	if got := m.in.text(); got != "thi" {
		t.Fatalf("↓ 到最新一条后应回到草稿，实际 %q", got)
	}
	if m.histPos != -1 {
		t.Fatalf("回草稿后 histPos 应为 -1，实际 %d", m.histPos)
	}
	// 已在草稿时再 ↓：不动（滚动交给 PgDn / 滚轮）
	press(m, tea.KeyDown)
	if got := m.in.text(); got != "thi" {
		t.Fatalf("草稿状态 ↓ 不该改动，实际 %q", got)
	}
}

func TestTUIHistoryPrefixFilter(t *testing.T) {
	m, _ := newTestModel(100, 20)
	for _, s := range []string{"git status", "ls -la", "git commit -m x"} {
		m.in.reset()
		typeText(m, s)
		press(m, tea.KeyEnter)
	}

	// 敲 "git " 再按 ↑：只翻 git 命令
	typeText(m, "git ")
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "git commit -m x" {
		t.Fatalf("前缀过滤第一条 = %q", got)
	}
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "git status" {
		t.Fatalf("前缀过滤第二条 = %q", got)
	}
	// 没有更多匹配：不动
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "git status" {
		t.Fatalf("无更多匹配时不该改动，实际 %q", got)
	}
}

func TestTUIHistorySubmitResetsNav(t *testing.T) {
	m, _ := newTestModel(100, 20)
	typeText(m, "one")
	press(m, tea.KeyEnter)

	// 进历史取回 "one"，编辑后提交
	press(m, tea.KeyUp)
	if got := m.in.text(); got != "one" {
		t.Fatalf("↑ = %q", got)
	}
	typeText(m, "!")
	press(m, tea.KeyEnter)
	if m.histPos != -1 {
		t.Fatalf("提交后应退出导航，histPos = %d", m.histPos)
	}
	if _, ok := m.hist.At(m.hist.Len() - 1); !ok {
		t.Fatal("历史应有内容")
	}
	if got, _ := m.hist.At(m.hist.Len() - 1); got != "one!" {
		t.Fatalf("提交的编辑后内容应进历史，实际 %q", got)
	}
	// "one" 与 "one!" 是两条不同输入，都该留着
	if m.hist.Len() != 2 {
		t.Fatalf("应有 2 条，实际 %d: %+v", m.hist.Len(), m.hist.items)
	}
}

func TestTUIHistoryMultilineFolds(t *testing.T) {
	m, _ := newTestModel(100, 20)
	// 用 Alt+Enter 造一条多行输入
	typeText(m, "line1")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	typeText(m, "line2")
	press(m, tea.KeyEnter)

	if m.hist.Len() != 1 {
		t.Fatalf("多行输入也应进历史，实际 %d", m.hist.Len())
	}
	// 取回时折叠成占位符，不把输入框顶碎
	press(m, tea.KeyUp)
	got := m.in.text()
	if got != "line1\nline2" {
		t.Fatalf("取回的多行历史展开后应为原文，实际 %q", got)
	}
	if n := len(m.in.buf); n > 64 {
		t.Fatalf("输入行里应只剩折叠占位符，实际 %d 字节: %q", n, string(m.in.buf))
	}
}
