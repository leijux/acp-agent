package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// 网络中断重连：截断到 MarkStream 处，只撤销这条消息渲染出的块。
func TestRewindMsgDropsStreamBlocks(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindUser, Text: "看看这个"})
	send(m, MarkStreamMsg{})
	send(m, KindMsg{Kind: KindThinking})
	send(m, PartialMsg{Kind: KindThinking, Text: "想一"})
	send(m, KindMsg{Kind: KindText})
	send(m, PartialMsg{Kind: KindText, Text: "半截答"})

	send(m, RewindMsg{})

	if len(m.blocks) != 1 {
		t.Fatalf("应只剩用户块，实际 %d 块: %+v", len(m.blocks), m.blocks)
	}
	view := ansi.Strip(m.viewString())
	if strings.Contains(view, "半截答") || strings.Contains(view, "想一") {
		t.Errorf("半截内容应被擦掉: %q", view)
	}
	if !strings.Contains(view, "看看这个") {
		t.Errorf("用户块不该被动: %q", view)
	}
}

// 上一条回复的正文必须保住：本轮内容还在行缓冲里（一个块都没产生）时，
// 「按尾部 Kind 连续性删」会一路删到上一条回复 —— 那是用户已经看到的内容。
func TestRewindMsgKeepsPreviousReply(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindUser, Text: "第一问"})
	send(m, LineMsg{Kind: KindText, Text: "上一轮答复"})
	send(m, LineMsg{Kind: KindUser, Text: "第二问"})
	send(m, MarkStreamMsg{}) // 新消息开始，但还没吐出任何块

	send(m, RewindMsg{})

	if len(m.blocks) != 3 {
		t.Fatalf("不该删掉已完成的块，实际 %d 块: %+v", len(m.blocks), m.blocks)
	}
	if got := ansi.Strip(m.viewString()); !strings.Contains(got, "上一轮答复") {
		t.Errorf("上一条回复被误删: %q", got)
	}
}

// 没有 MarkStream 标记时退化成删尾部连续的正文/思考块（老宿主/单元测试）。
func TestRewindMsgFallsBackToTrailingBlocks(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindUser, Text: "问题"})
	send(m, LineMsg{Kind: KindTool, Text: "→ read"})
	send(m, PartialMsg{Kind: KindText, Text: "半截"})

	send(m, RewindMsg{})

	if len(m.blocks) != 2 {
		t.Fatalf("应删掉尾部正文块，实际 %d 块: %+v", len(m.blocks), m.blocks)
	}
}

// 截断后尾块可能还是正文（上一条回复）：重发的增量必须新起一块，
// 否则会 append 进上一条回复里。
func TestRewriteAfterRewindStartsNewBlock(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindText, Text: "上一条答复"})
	send(m, MarkStreamMsg{})
	send(m, PartialMsg{Kind: KindText, Text: "半截"})
	send(m, RewindMsg{})

	send(m, LineMsg{Kind: KindText, Text: "重发答复"})

	if len(m.blocks) != 2 {
		t.Fatalf("重发内容应新起一块，实际 %d 块: %+v", len(m.blocks), m.blocks)
	}
	if m.blocks[0].Lines[0] != "上一条答复" {
		t.Errorf("上一条答复被改写: %+v", m.blocks[0].Lines)
	}
	if m.blocks[1].Lines[0] != "重发答复" {
		t.Errorf("重发内容 = %+v", m.blocks[1].Lines)
	}
}
