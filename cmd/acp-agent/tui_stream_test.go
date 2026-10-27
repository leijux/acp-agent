package main

import (
	"io"
	"reflect"
	"testing"

	"github.com/leijux/acp-agent/internal/tui"
)

// 一轮正文流结束时的宿主动作（markdown Flush + closeDelta 补换行）只能让
// 未换行的最后一行产生「一条 partial + 一条收尾 line」，否则界面上最后一行会
// 出现两遍（Block.appendLine 见尾行是半成品时会替换，但前提是宿主别发两遍 line）。
func TestStreamingTailEmitsSingleClosingLine(t *testing.T) {
	ui := tui.New(tui.Options{Header: "h"})
	term := &terminal{out: ui.Sink(), errOut: io.Discard}
	term.md = newMDRenderer(term)

	term.markdown().Write("第一行\n最后一行")
	term.inDelta = true
	term.closeDelta()

	var got []string
	for _, msg := range ui.Pending() {
		switch m := msg.(type) {
		case tui.LineMsg:
			got = append(got, "line:"+m.Text)
		case tui.PartialMsg:
			got = append(got, "partial:"+m.Text)
		}
	}
	want := []string{"line:第一行", "partial:最后一行", "line:最后一行"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("消息序列 = %q, 期望 %q", got, want)
	}
}
