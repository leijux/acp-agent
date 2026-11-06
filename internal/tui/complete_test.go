package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// completer 造一个固定返回候选的补全回调。
func completer(cands []string) func(string, int) ([]string, int, int) {
	return func(line string, cur int) ([]string, int, int) {
		// 约定：@ 之后到光标就是替换区间（与 mentions.Complete 一致）。
		start := strings.LastIndexByte(line[:cur], '@')
		if start < 0 {
			return nil, 0, 0
		}
		return cands, start + 1, cur
	}
}

func newCompleteModel(cands []string) *Model {
	ui := New(Options{Header: "h", Complete: completer(cands)})
	m := ui.model
	m.width, m.height = 80, 20
	return m
}

// 唯一候选：直接替换，不留提示。
func TestCompleteInputUnique(t *testing.T) {
	m := newCompleteModel([]string{"src/main.go"})
	m.in.setText("@src/ma")
	m.completeInput()
	if got := string(m.in.buf); got != "@src/main.go" {
		t.Fatalf("buf = %q", got)
	}
	if m.in.cur != len("@src/main.go") {
		t.Errorf("光标 = %d, want 末尾", m.in.cur)
	}
	if m.status != "" {
		t.Errorf("唯一候选不该留提示: %q", m.status)
	}
}

// 多候选：补到最长公共前缀，并把候选数写在状态行。
func TestCompleteInputCommonPrefix(t *testing.T) {
	m := newCompleteModel([]string{"src/a.go", "src/app/"})
	m.in.setText("@src/")
	m.completeInput()
	// "src/a.go" 与 "src/app/" 的公共前缀是 "src/a"
	if got := string(m.in.buf); got != "@src/a" {
		t.Fatalf("buf = %q, want @src/a", got)
	}
	if !strings.Contains(m.status, "2 个候选") {
		t.Errorf("status = %q", m.status)
	}
}

// 没有候选：输入行不动，只提示。
func TestCompleteInputNoMatch(t *testing.T) {
	m := newCompleteModel(nil)
	m.in.setText("@nope")
	m.completeInput()
	if got := string(m.in.buf); got != "@nope" {
		t.Fatalf("buf 被改动: %q", got)
	}
	if !strings.Contains(m.status, "没有匹配") {
		t.Errorf("status = %q", m.status)
	}
}

// 没有补全能力（未注入回调）时 Tab 不该崩、也不该改输入。
func TestCompleteInputDisabled(t *testing.T) {
	ui := New(Options{Header: "h"})
	m := ui.model
	m.width, m.height = 80, 20
	m.in.setText("@x")
	m.completeInput()
	if got := string(m.in.buf); got != "@x" {
		t.Fatalf("buf = %q", got)
	}
}

// Tab 键要走到补全上（以前是往输入行插一个 \t）。
func TestTabTriggersCompletion(t *testing.T) {
	m := newCompleteModel([]string{"docs/readme.md"})
	m.in.setText("@docs/re")
	press(m, tea.KeyTab)
	if got := string(m.in.buf); got != "@docs/readme.md" {
		t.Fatalf("Tab 没触发补全: %q", got)
	}
}

// 补全区间碰到粘贴占位符时不动它（占位符切开就展不开了）。
func TestReplaceRangeRefusesPasteToken(t *testing.T) {
	var in inputLine
	in.paste(strings.Repeat("x", 400)) // 超长 → 折叠成占位符
	if len(in.buf) == 0 {
		t.Skip("没有折叠出占位符")
	}
	end := len(in.buf)
	if in.replaceRange(0, end, "y") {
		t.Fatal("不该允许替换占位符区间")
	}
}
