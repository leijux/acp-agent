package main

import (
	"fmt"
	"strings"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tui"
)

// 会话历史回放：`-c` / `--session` 接着旧会话时，把已有对话重新画进界面。
//
// 为什么用 ActiveBranch 而不是 Session.Messages()：后者是**发送给模型的投影**
// （压缩掉的前缀已被摘要替代），回放时会把被折叠的历史整段丢掉。用户想看的
// 是「这个会话里都聊过什么」，所以取完整活动分支，压缩处只留一个标记行。

const (
	// historyMaxItems 是一次回放最多画多少段（块）。超出部分只给一行提示——
	// 几百条消息全塞进视口既慢又没人看，需要全文的去开会话文件。
	historyMaxItems = 160
	// historyMaxLines / historyMaxChars 限制单块内容的体量（工具输出可能上万行）。
	// 唯一的例外是「最后一条回复」：-c 接着聊时用户往往是照着那段结尾继续干活，
	// 所以它整段保留（见 clipItems）。
	historyMaxLines = 24
	historyMaxChars = 4000
)

// historyItem 是回放中的一段内容。Text 保持原始文本（正文是 markdown 源，
// 思考是纯文本），怎么渲染由调用方按 Kind 决定。
type historyItem struct {
	Kind tui.BlockKind
	Text string
}

// historyItems 把活动分支翻译成界面块序列。
func historyItems(entries []*agent.Entry, limit int) []historyItem {
	var items []historyItem
	for _, e := range entries {
		if e == nil {
			continue
		}
		switch e.Type {
		case agent.EntryMessage:
			items = append(items, messageItems(e.Message)...)
		case agent.EntryCompaction:
			text := "⚕ 上下文已压缩"
			if e.TokensBefore > 0 && e.TokensAfter > 0 {
				text += fmt.Sprintf("（%d → %d token，更早的内容已折叠）", e.TokensBefore, e.TokensAfter)
			}
			items = append(items, historyItem{Kind: tui.KindSystem, Text: text})
		case agent.EntryBranchSummary:
			items = append(items, historyItem{Kind: tui.KindSystem, Text: "⚕ 分支摘要"})
		case agent.EntryModelChange:
			if e.Model != nil {
				items = append(items, historyItem{
					Kind: tui.KindSystem,
					Text: fmt.Sprintf("模型切换到 %s/%s", e.Model.Provider, e.Model.ID),
				})
			}
		}
	}
	items = clipItems(items)
	if limit > 0 && len(items) > limit {
		cut := len(items) - limit
		items = append([]historyItem{{
			Kind: tui.KindSystem,
			Text: fmt.Sprintf("…（更早的 %d 段已省略，/session 查看会话文件）", cut),
		}}, items[cut:]...)
	}
	return items
}

// messageItems 把一条消息拆成若干段（不裁剪：裁剪交给 clipItems 统一做，
// 因为它要看得到整份序列才知道哪一段是最后一条回复）。
func messageItems(m agent.Message) []historyItem {
	switch msg := m.(type) {
	case *agent.UserMessage:
		text := strings.TrimSpace(blocksToText(msg.Content))
		if text == "" {
			return nil
		}
		return []historyItem{{Kind: tui.KindUser, Text: text}}

	case *agent.AssistantMessage:
		var out []historyItem
		// 按块出现的顺序还原：思考 → 正文 → 工具调用，与实时渲染一致。
		for _, blk := range msg.Content {
			switch b := blk.(type) {
			case agent.ThinkingBlock:
				if t := strings.TrimSpace(b.Thinking); t != "" {
					out = append(out, historyItem{Kind: tui.KindThinking, Text: t})
				}
			case agent.TextBlock:
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, historyItem{Kind: tui.KindText, Text: t})
				}
			case agent.ToolCallBlock:
				out = append(out, historyItem{
					Kind: tui.KindTool,
					Text: strings.TrimSpace("→ " + b.Name + " " + summarizeArgs(b.Arguments)),
				})
			}
		}
		return out

	case *agent.ToolResultMessage:
		mark, detail := "✓", ""
		if msg.IsError {
			mark = "✗"
			detail = " " + firstLine(msg.Text(), 160)
		}
		return []historyItem{{Kind: tui.KindTool, Text: strings.TrimSpace(mark + " " + msg.ToolName + detail)}}

	case *agent.BashExecutionMessage:
		cmd := strings.TrimSpace(msg.Command)
		if cmd == "" {
			return nil
		}
		return []historyItem{{Kind: tui.KindTool, Text: "$ " + cmd}}

	default:
		// SystemMessage（本来就不进分支）/ CustomMessage（扩展注入）不参与回放。
		return nil
	}
}

// clipItems 裁掉每段里的超长内容，但放过最后一条回复（最后一段 assistant 正文）。
//
// 为什么留这个例外：接着旧会话时用户常常就是照着上一轮的结尾（待办清单、
// 下一步计划、结论）继续干活，截断后那段正好被切掉，回放的意义就没了。
// 前面的历史照旧截断——那部分只是用来回忆上下文，不需要逐字全文。
// 顺手丢掉裁剪后变空的段。
func clipItems(items []historyItem) []historyItem {
	keep := -1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Kind == tui.KindText {
			keep = i
			break
		}
	}
	out := items[:0]
	for i, it := range items {
		if i != keep {
			it.Text = clipHistory(it.Text)
		}
		if it.Text == "" {
			continue
		}
		out = append(out, it)
	}
	return out
}

// clipHistory 裁掉过长的块内容：按行数 + 字符数双上限，超了加省略提示。
func clipHistory(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	truncated := false
	lines := strings.Split(s, "\n")
	if len(lines) > historyMaxLines {
		lines = lines[:historyMaxLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > historyMaxChars {
		out = string(r[:historyMaxChars])
		truncated = true
	}
	if truncated {
		out += "\n…（已截断）"
	}
	return out
}

// replayHistory 把历史灌进界面。必须在 ui.Run 之前调用：消息先排队，
// 界面起来后一次性按序消费（UI.Send 在程序启动前不会阻塞）。
func (a *app) replayHistory(ui *tui.UI) {
	if a.sess == nil {
		return
	}
	items := historyItems(a.sess.ActiveBranch(), historyMaxItems)
	if len(items) == 0 {
		return
	}
	// 回放只灌内容，不加「历史记录 / 历史结束」之类的分隔行：
	// 标题已经在顶部 header 上，分隔行只是噪声。
	for _, it := range items {
		a.emitHistoryItem(ui, it)
	}
	// 历史里的思考默认收起：几屏旧思考会把“刚才聊到哪”遮住；
	// 想看内容点击/⌃T 即可。
	ui.CollapseThinking()
}

// emitHistoryItem 按块类型选择渲染方式：正文走 markdown，思考走暗色纯文本。
func (a *app) emitHistoryItem(ui *tui.UI, it historyItem) {
	switch it.Kind {
	case tui.KindUser:
		ui.UserLine(it.Text)
	case tui.KindText:
		a.setKind(kindText)
		a.renderMarkdownText(it.Text)
	case tui.KindThinking:
		a.setKind(kindThinking)
		for _, ln := range strings.Split(it.Text, "\n") {
			a.term.printf("%s\n", a.term.paint(cDim, ln))
		}
	default:
		a.setKind(it.Kind)
		for _, ln := range strings.Split(it.Text, "\n") {
			a.term.printf("%s\n", a.term.paint(cDim, ln))
		}
	}
}

// renderMarkdownText 用现有流式渲染器把一段 markdown 一次性落成行。
// 渲染器本身不区分 live/回放，输出照样走 sink，所以这里只负责喂完再清状态。
func (a *app) renderMarkdownText(text string) {
	t := a.term
	t.mu.Lock()
	md := t.markdown()
	md.Write(text)
	// 没以换行结尾的残余：Flush 不会补换行，而行缓冲要靠换行才肯成行。
	// 不补的话，下一个人 emitHistoryItem（工具行/下一段正文）的字节会追加到
	// 这条尾巴上，两段内容被拼成一行、块类型也跟着错。
	tail := md.pending()
	md.Flush()
	if tail {
		fmt.Fprintln(t.out)
	}
	t.inDelta = false
	t.mu.Unlock()
	t.resetMarkdown()
}
