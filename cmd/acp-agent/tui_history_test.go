package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tui"
)

func histEntry(m agent.Message) *agent.Entry {
	return &agent.Entry{Type: agent.EntryMessage, Message: m}
}

func userMsg(text string) *agent.UserMessage {
	return &agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: text}}}
}

// TestHistoryItemsShape 验证回放把一条 assistant 响应按「思考 → 正文 → 工具」
// 的原始顺序拆开，工具结果单独成段。
func TestHistoryItemsShape(t *testing.T) {
	entries := []*agent.Entry{
		histEntry(userMsg("看看目录")),
		histEntry(&agent.AssistantMessage{Content: []agent.Block{
			agent.ThinkingBlock{Thinking: "先 ls"},
			agent.TextBlock{Text: "我看一下"},
			agent.ToolCallBlock{Name: "ls", Arguments: map[string]any{"path": "internal"}},
		}}),
		histEntry(&agent.ToolResultMessage{ToolName: "ls", Content: []agent.Block{
			agent.TextBlock{Text: "tui.go\nmodel.go"},
		}}),
	}
	items := historyItems(entries, 0)

	wantKinds := []tui.BlockKind{tui.KindUser, tui.KindThinking, tui.KindText, tui.KindTool, tui.KindTool}
	if len(items) != len(wantKinds) {
		t.Fatalf("段数 = %d，期望 %d：%+v", len(items), len(wantKinds), items)
	}
	for i, k := range wantKinds {
		if items[i].Kind != k {
			t.Fatalf("第 %d 段类型 = %v，期望 %v", i, items[i].Kind, k)
		}
	}
	if items[0].Text != "看看目录" {
		t.Fatalf("用户文本 = %q", items[0].Text)
	}
	if items[1].Text != "先 ls" || items[2].Text != "我看一下" {
		t.Fatalf("思考/正文文本不对: %q / %q", items[1].Text, items[2].Text)
	}
	if !strings.Contains(items[3].Text, "→ ls") || !strings.Contains(items[3].Text, "internal") {
		t.Fatalf("工具调用行 = %q", items[3].Text)
	}
	if !strings.HasPrefix(items[4].Text, "✓ ls") {
		t.Fatalf("工具结果行 = %q", items[4].Text)
	}
}

// TestHistoryItemsToolError 工具失败要留 ✗ 与错误首行。
func TestHistoryItemsToolError(t *testing.T) {
	items := historyItems([]*agent.Entry{
		histEntry(&agent.ToolResultMessage{
			ToolName: "bash", IsError: true,
			Content: []agent.Block{agent.TextBlock{Text: "exit status 1\n更多细节"}},
		}),
	}, 0)
	if len(items) != 1 || !strings.HasPrefix(items[0].Text, "✗ bash") {
		t.Fatalf("失败结果 = %+v", items)
	}
	if !strings.Contains(items[0].Text, "exit status 1") {
		t.Fatalf("应带错误首行: %q", items[0].Text)
	}
}

// TestHistoryItemsSkipsNoise 空内容 / system / custom 不参与回放。
func TestHistoryItemsSkipsNoise(t *testing.T) {
	entries := []*agent.Entry{
		histEntry(userMsg("   ")),
		histEntry(&agent.AssistantMessage{Content: []agent.Block{
			agent.ThinkingBlock{Thinking: "  "},
			agent.TextBlock{Text: ""},
			agent.ImageBlock{Data: "xxx"},
		}}),
		histEntry(&agent.SystemMessage{Content: []agent.Block{agent.TextBlock{Text: "SYS"}}}),
	}
	if items := historyItems(entries, 0); len(items) != 0 {
		t.Fatalf("应全部跳过，实际 %+v", items)
	}
}

// TestHistoryItemsCompactionAndModel 压缩/换模型留一行标记，便于知道上下文被折叠过。
func TestHistoryItemsCompactionAndModel(t *testing.T) {
	entries := []*agent.Entry{
		{Type: agent.EntryCompaction, TokensBefore: 9000, TokensAfter: 1200},
		{Type: agent.EntryModelChange, Model: &agent.Model{Provider: "deepseek", ID: "deepseek-flash"}},
	}
	items := historyItems(entries, 0)
	if len(items) != 2 {
		t.Fatalf("段数 = %d", len(items))
	}
	if items[0].Kind != tui.KindSystem || !strings.Contains(items[0].Text, "9000 → 1200") {
		t.Fatalf("压缩标记 = %+v", items[0])
	}
	if !strings.Contains(items[1].Text, "deepseek/deepseek-flash") {
		t.Fatalf("模型切换标记 = %+v", items[1])
	}
}

// TestHistoryItemsLimit 只回放最近 N 段，并在最前面给省略提示。
func TestHistoryItemsLimit(t *testing.T) {
	var entries []*agent.Entry
	for i := 0; i < 30; i++ {
		entries = append(entries, histEntry(userMsg("问题")))
	}
	items := historyItems(entries, 10)
	if len(items) != 11 {
		t.Fatalf("段数 = %d，期望 10+1 条提示", len(items))
	}
	if !strings.Contains(items[0].Text, "更早的 20 段") {
		t.Fatalf("省略提示 = %q", items[0].Text)
	}
}

// TestClipHistory 超长内容按行/字符截断并标注。
func TestClipHistory(t *testing.T) {
	var lines []string
	for i := 0; i < historyMaxLines+5; i++ {
		lines = append(lines, "行")
	}
	got := clipHistory(strings.Join(lines, "\n"))
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Fatalf("没有截断提示: %q", got)
	}
	if n := strings.Count(got, "\n") + 1; n > historyMaxLines+1 {
		t.Fatalf("行数没截住: %d", n)
	}
	long := clipHistory(strings.Repeat("字", historyMaxChars+100))
	if r := []rune(strings.TrimSuffix(long, "\n…（已截断）")); len(r) > historyMaxChars {
		t.Fatalf("字符数没截住: %d", len(r))
	}
	if clipHistory("   ") != "" {
		t.Fatal("空白内容应归零")
	}
}

// TestHistoryItemsFromLoadedSession 回放必须跑在“从 JSONL 读回来的”会话上：
// 反序列化后的消息/块是值类型还是指针，决定了 type switch 会不会静默落空。
func TestHistoryItemsFromLoadedSession(t *testing.T) {
	s := agent.NewSession()
	s.Append(userMsg("问题"))
	s.Append(&agent.AssistantMessage{Content: []agent.Block{
		agent.ThinkingBlock{Thinking: "想一想"},
		agent.TextBlock{Text: "回答"},
		agent.ToolCallBlock{Name: "ls", Arguments: map[string]any{"path": "x"}},
	}})
	s.Append(&agent.ToolResultMessage{ToolName: "ls", Content: []agent.Block{
		agent.TextBlock{Text: "x.go"},
	}})

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := agent.LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	items := historyItems(loaded.ActiveBranch(), 0)
	if len(items) != 5 {
		t.Fatalf("段数 = %d，期望 5（user/thinking/text/tool/tool）: %+v", len(items), items)
	}
}

// TestReplayHistoryIntoUI `-c` 启动时的回放：内容要真的排进界面队列，
// 且正文走 markdown、块类型正确（折叠与跳转依赖它）。
func TestReplayHistoryIntoUI(t *testing.T) {
	var out bytes.Buffer
	a := &app{
		term: newTerminal(&out, true),
		sess: agent.NewSession(),
	}
	a.sess.Append(userMsg("帮我改 bug"))
	a.sess.Append(&agent.AssistantMessage{Content: []agent.Block{
		agent.ThinkingBlock{Thinking: "先读代码"},
		agent.TextBlock{Text: "**改好了**"},
		agent.ToolCallBlock{Name: "edit", Arguments: map[string]any{"path": "a.go"}},
	}})

	ui := tui.New(tui.Options{Header: "h"})
	a.ui = ui
	a.term.out = ui.Sink()

	a.replayHistory(ui)

	var kinds []tui.BlockKind
	var texts []string
	for _, msg := range ui.Pending() {
		if lm, ok := msg.(tui.LineMsg); ok {
			kinds = append(kinds, lm.Kind)
			texts = append(texts, lm.Text)
		}
	}
	if len(kinds) == 0 {
		t.Fatal("回放没有产生任何行")
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"帮我改 bug", "先读代码", "改好了", "→ edit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("回放缺少 %q:\n%s", want, joined)
		}
	}
	// 回放不加任何分隔/提示行，只灌内容。
	for _, unwanted := range []string{"历史记录", "历史结束"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("回放不应再有 %q 提示行:\n%s", unwanted, joined)
		}
	}
	// 用户消息必须是 user 块（否则在界面里没有「▌ 你」标签，锚点也不对）
	found := false
	for i, k := range kinds {
		if k == tui.KindUser && texts[i] == "帮我改 bug" {
			found = true
		}
	}
	if !found {
		t.Fatalf("用户消息块类型不对: %v / %v", kinds, texts)
	}
	// markdown 渲染不该把 ** 原样留下
	if strings.Contains(joined, "**改好了**") {
		t.Fatalf("正文没有走 markdown 渲染:\n%s", joined)
	}
	// 分段边界：每段必须落在自己的块里。
	// renderMarkdownText 不补收尾换行时，紧接着的工具行会被 append 到正文那半行上，
	// 结果是 [text]"改好了→ edit a.go"（工具行丢了、块类型也错）—— contains 断言抓不到。
	for i, txt := range texts {
		if strings.Contains(txt, "改好了") && strings.Contains(txt, "→ edit") {
			t.Fatalf("正文与工具行被拼成一行了（kinds=%v texts=%v）", kinds, texts)
		}
		if strings.Contains(txt, "→ edit") && kinds[i] != tui.KindTool {
			t.Fatalf("工具行块类型 = %v，期望 KindTool", kinds[i])
		}
	}
}
