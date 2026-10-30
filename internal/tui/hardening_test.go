package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// /freeze 把内容交给 bubbletea Println 写进 scrollback，所以视口必须同时清空：
// 否则同一份内容在 scrollback 和屏上各一份，再按一次再叠一份。
func TestFreezeClearsViewport(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindText, Text: "内容一"})
	m.rebuild()
	if m.flat == "" {
		t.Fatal("前置条件失败：视口没有内容")
	}

	if _, cmd := m.Update(FreezeMsg{}); cmd == nil {
		t.Fatal("freeze 应把内容交给 bubbletea Println")
	}
	if m.flat != "" || len(m.blocks) != 0 {
		t.Fatalf("freeze 后视口没清空: flat=%q blocks=%d", m.flat, len(m.blocks))
	}
	if got := ansi.Strip(m.viewString()); strings.Contains(got, "内容一") {
		t.Fatalf("freeze 后屏幕上还留着正文（会和 scrollback 重复）:\n%q", got)
	}
}

// 窄终端下右侧状态优先，左侧快捷键提示截断而不是整段消失。
func TestStatusViewKeepsLeftHintWhenNarrow(t *testing.T) {
	m, _ := newTestModel(34, 10)
	send(m, RunningMsg{On: true})

	out := ansi.Strip(m.statusView())
	if !strings.Contains(out, "运行中") {
		t.Fatalf("窄终端下右侧状态被丢了: %q", out)
	}
	if !strings.Contains(out, "↑/↓") {
		t.Fatalf("左侧提示应截断保留，而不是整段消失: %q", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("截断处应有省略号: %q", out)
	}
	if w := ansi.StringWidth(out); w > 34 {
		t.Fatalf("状态行宽 %d 超过终端宽度 34: %q", w, out)
	}
}

// 新的权限询问到来时，旧的那个必须被回答（否则 AskPermission 一直挂着）。
func TestAskMsgAnswersPrevious(t *testing.T) {
	m, _ := newTestModel(60, 20)
	first := make(chan string, 1)
	second := make(chan string, 1)

	send(m, AskMsg{Tool: "edit", Reply: first})
	send(m, AskMsg{Tool: "write", Reply: second})

	select {
	case got := <-first:
		if got != "n" {
			t.Fatalf("旧询问应被当成“否”回掉，实际 %q", got)
		}
	default:
		t.Fatal("旧询问的 Reply 没收到回答：调用方会一直等到 ctx 取消")
	}
	if m.perm == nil || m.perm.tool != "write" {
		t.Fatalf("当前询问 = %+v，期望 write", m.perm)
	}
}

var _ tea.Msg = FreezeMsg{}
