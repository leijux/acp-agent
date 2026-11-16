package main

import (
	"io"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tui"
)

// 网络中断重连：宿主先给界面打「消息起点」标记，撤销时截到那里，
// 并把行缓冲/markdown 里的半截内容丢掉 —— 重发的内容不能拼在半截后面。
func TestRenderHandlerRewindOnReconnect(t *testing.T) {
	ui := tui.New(tui.Options{Header: "h"})
	term := &terminal{out: ui.Sink(), errOut: io.Discard}
	term.md = newMDRenderer(term)
	a := &app{term: term, ui: ui}
	h := a.renderHandler()

	h(agent.MessageStartEvent{Message: &agent.AssistantMessage{}})
	h(agent.MessageUpdateEvent{Kind: "text", Delta: "半截答\n"})
	// 未换行的残余：重连前必须扔掉，否则重发的内容会跟它拼成同一行。
	h(agent.MessageUpdateEvent{Kind: "text", Delta: "还没写完"})
	if !term.md.pending() {
		t.Fatal("前提不成立：这里应有一段未换行的残余")
	}

	h(agent.MessageRewindEvent{})

	if term.md.pending() {
		t.Error("重发前 markdown 缓冲必须清空，否则新内容会拼在半截回复后面")
	}
	h(agent.MessageStartEvent{Message: &agent.AssistantMessage{}})
	h(agent.MessageUpdateEvent{Kind: "text", Delta: "答复\n"})

	var (
		marks   int
		rewound bool
		lines   []string
	)
	for _, msg := range ui.Pending() {
		switch m := msg.(type) {
		case tui.MarkStreamMsg:
			marks++
		case tui.RewindMsg:
			rewound = true
		case tui.LineMsg:
			lines = append(lines, m.Text)
		}
	}
	if !rewound {
		t.Fatal("应给界面发 RewindMsg：否则屏上的半截内容撤不掉")
	}
	if marks != 2 {
		t.Errorf("每条消息都应打一次消息起点标记，实际 %d 次", marks)
	}
	if len(lines) < 2 || lines[1] != "答复" {
		t.Fatalf("重发后的行 = %q，应单独成行且不含半截内容", lines)
	}
}
