package tui

import (
	"io"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drain 取走排队中的消息（界面未 Run 时 Send 只是入队；界面启动后这里为空）。
func drain(ui *UI) []tea.Msg {
	ui.mu.Lock()
	msgs := ui.pending
	ui.pending = nil
	ui.mu.Unlock()
	return msgs
}

// pump 把手上的消息灌进 model 并返回当前块（模拟界面跑起来后的 Update）。
func pump(ui *UI) []*Block {
	for _, msg := range drain(ui) {
		ui.model.Update(msg)
	}
	return ui.model.blocks
}

// write 写一段字节并立刻让界面消费掉（保持与真实时序一致）。
func write(ui *UI, s io.Writer, text string) []*Block {
	if _, err := s.Write([]byte(text)); err != nil {
		panic(err)
	}
	return pump(ui)
}

func blockLines(blocks []*Block) [][]string {
	out := make([][]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Lines
	}
	return out
}

// 多行内容（Alt+Enter 多行输入 / 多行用户消息）必须拆成多行存：
// 一条 Lines 里带着内嵌换行会让 owners/anchors 的「一行一个下标」失效，
// 之后鼠标点击命中错的块。
func TestMultilineLineKeepsOwnersAligned(t *testing.T) {
	m, _ := newTestModel(40, 20)
	send(m, LineMsg{Kind: KindThinking, Text: "第一行\n第二行"})
	send(m, LineMsg{Kind: KindText, Text: "正文"})
	m.rebuild()

	// 思考标题 + 两行内容 + 空行 + 正文 = 5 行 flat
	if len(m.owners) != len(strings.Split(m.flat, "\n")) {
		t.Fatalf("owners(%d) 与 flat 行数(%d) 不一致:\n%s",
			len(m.owners), len(strings.Split(m.flat, "\n")), m.flat)
	}
	if n := len(m.blocks[0].Lines); n != 2 {
		t.Fatalf("思考块行数 = %d，期望 2（内嵌换行要拆开）", n)
	}

	// 点最后一行必须是正文块，不能因为错位折错块。
	selBlock := m.owners[len(m.owners)-1]
	if m.blocks[selBlock].Kind != KindText {
		t.Fatalf("最后一行属于块 %d（kind=%v），期望正文块", selBlock, m.blocks[selBlock].Kind)
	}
}

// 流式正文收尾：md 渲染器先把残余半行 writeRaw 出去（→ Partial），
// closeDelta 再补换行（→ LineMsg）。最后一行不能变成两行，前一行也不能被吃掉。
func TestLastLineNotDuplicated(t *testing.T) {
	ui := New(Options{Header: "h"})
	s := ui.Sink()

	blocks := write(ui, s, "第一行\n")
	blocks = write(ui, s, "第二行\n")
	blocks = write(ui, s, "最后一行没有换行") // mdRenderer.Flush
	blocks = write(ui, s, "\n")       // closeDelta
	blocks = write(ui, s, "\n")       // turn 结束后宿主补的空行

	want := [][]string{{"第一行", "第二行", "最后一行没有换行", ""}}
	if got := blockLines(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("块内容 = %q, 期望 %q", got, want)
	}
}

// 超长段落：先吐半行（Partial），再补齐整行（LineMsg），也应只剩一整行。
func TestPartialFlushThenLineKeepsOneLine(t *testing.T) {
	ui := New(Options{Header: "h"})
	s := ui.Sink()

	blocks := write(ui, s, "前半段")
	blocks = write(ui, s, "后半段\n")

	want := [][]string{{"前半段后半段"}}
	if got := blockLines(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("块内容 = %q, 期望 %q", got, want)
	}
}

// 思考→正文：宿主先切 kind 再补换行（repl.go 里就是这个顺序），
// 思考的尾行必须留在思考块里，不能当正文第一行复制到新块。
func TestThinkingTailStaysInThinkingBlock(t *testing.T) {
	ui := New(Options{Header: "h"})
	s := ui.Sink()

	ui.SetBlockKind(KindThinking)
	pump(ui)
	blocks := write(ui, s, "思考中的最后一行")
	ui.SetBlockKind(KindText) // 模拟 repl.go:214 先 setKind
	pump(ui)
	blocks = write(ui, s, "\x1b[0m\n") // repl.go:217 再补换行
	blocks = write(ui, s, "正文第一行\n")

	want := [][]string{
		{"思考中的最后一行\x1b[0m"},
		{"正文第一行"},
	}
	if got := blockLines(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("块内容 = %q, 期望 %q", got, want)
	}
	if len(blocks) != 2 || blocks[0].Kind != KindThinking || blocks[1].Kind != KindText {
		t.Fatalf("块类型 = %v, 期望 thinking/text", blocks)
	}
}

// 相邻两行内容相同且都不是半成品时，仍然要各占一行（别把替换逻辑做过头）。
func TestIdenticalConsecutiveLinesKeepBoth(t *testing.T) {
	ui := New(Options{Header: "h"})
	s := ui.Sink()

	blocks := write(ui, s, "---\n")
	blocks = write(ui, s, "---\n")

	want := [][]string{{"---", "---"}}
	if got := blockLines(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("块内容 = %q, 期望 %q", got, want)
	}
}
