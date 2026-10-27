package main

import (
	"fmt"
	"strings"

	"github.com/leijux/acp-agent/internal/agent"
)

// 复制助手回复。
//
// 为什么从活动分支取、而不是 a.sess.Messages()：后者是发给模型的投影，
// 压缩过的前缀已被摘要替代，复制到的是摘要而不是原文。
// 只取 TextBlock：思考和工具调用用户一般不想要，混进来反而碍事。

// lastAssistantText 返回倒数第 n 条助手回复的正文（n=1 即最近一条），
// 以及遍历到的助手回复总数（用于「超出范围」提示）。
func (a *app) lastAssistantText(n int) (string, int) {
	if a.sess == nil || n <= 0 {
		return "", 0
	}
	branch := a.sess.ActiveBranch()
	total := 0
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		if e == nil || e.Type != agent.EntryMessage {
			continue
		}
		msg, ok := e.Message.(*agent.AssistantMessage)
		if !ok {
			continue
		}
		text := assistantTextOf(msg)
		if text == "" {
			continue // 纯工具调用的中间步骤不算「回复」
		}
		total++
		if total == n {
			return text, total
		}
	}
	return "", total
}

// assistantTextOf 把一条助手消息里的正文块拼起来（跳过思考/工具调用）。
func assistantTextOf(m *agent.AssistantMessage) string {
	var parts []string
	for _, blk := range m.Content {
		if b, ok := blk.(agent.TextBlock); ok {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// copyReply 复制倒数第 n 条回复，并就地给一行结果提示。
func (a *app) copyReply(n int) error {
	text, total := a.lastAssistantText(n)
	if text == "" {
		if total == 0 {
			return fmt.Errorf("还没有可复制的回复")
		}
		return fmt.Errorf("只有 %d 条回复（/copy %d 超出范围）", total, n)
	}
	label := "最近一条回复"
	if n > 1 {
		label = fmt.Sprintf("倒数第 %d 条回复", n)
	}
	return a.copyWithNotice(label, text)
}

// copyWithNotice 写剪贴板并报一行结果（TUI 的拖选复制也走这里）。
func (a *app) copyWithNotice(label, text string) error {
	// 先把耗时操作做掉（起子进程 / PowerShell），不占任何锁。
	how, err := a.copyText(text)
	if err != nil {
		return err
	}
	a.notice("已复制%s（%d 行 / %s 字符）via %s",
		label, lineCount(text), fmtTokens(len([]rune(text))), how)
	return nil
}

// copyFailure 报复制失败（可能从界面 goroutine 调到）。
func (a *app) copyFailure(err error) {
	a.endPendingLine()
	a.setKind(kindSystem)
	a.term.printf("%s %v\n", a.term.paint(cRed, "复制失败:"), err)
}

// copyText 调底层剪贴板实现。
//
// OSC 52 只在非交互界面下才允许：TUI 里 stdout 归渲染器独占，
// 而且终端不支持时无法回读确认，宁可报错也不假报成功。
func (a *app) copyText(text string) (string, error) {
	return copyToClipboard(text, a.ui == nil)
}

// notice 打一行系统提示。
//
// 必须先 endPendingLine：TUI 下所有 printf 共用同一个行缓冲，而流式输出
// （尤其是逐 token 的思考）经常停在「未换行」的半行上，直接写会把提示文字
// 拼在那半行后面，还带着旧的块类型。
func (a *app) notice(format string, args ...any) {
	a.endPendingLine()
	a.setKind(kindSystem)
	a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf(format, args...)))
}

// endPendingLine 收尾当前未换行的输出行。
//
// 只对 TUI 有意义：那里 printf 写的是行缓冲，换行才成行；非 TUI 直写终端，
// 多补一个换行只会在中间凭空多一个空行。
func (a *app) endPendingLine() {
	if a.ui == nil {
		return
	}
	// 渲染器里可能还握着几个字符（md 按行渲染，没有换行就不吐）。
	a.term.mu.Lock()
	if a.term.md != nil {
		a.term.md.Flush()
	}
	a.term.mu.Unlock()
	a.ui.FlushLine() // 行缓冲里真有半行才补换行，避免凭空多空行
}

// lineCount 数行数（末尾没有换行的最后一行也算一行）。
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
