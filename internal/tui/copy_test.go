package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Ctrl-Y 只负责转达按键：界面不认识「回复」，复制由宿主决定。
func TestCtrlYTriggersCopy(t *testing.T) {
	m, _ := newTestModel(120, 20)
	called := 0
	m.onCopy = func() { called++ }

	send(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if called != 1 {
		t.Fatalf("Ctrl-Y 应触发一次复制回调，实际 %d 次", called)
	}

	// 没注册回调时按 Ctrl-Y 不能崩（--no-tui 或不关心复制的宿主）。
	m2, _ := newTestModel(120, 20)
	send(m2, tea.KeyMsg{Type: tea.KeyCtrlY})
}
