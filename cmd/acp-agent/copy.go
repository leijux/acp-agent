package main

import (
	"fmt"
	"strings"
)

// 拖选复制：界面把选中的纯文本交过来，这里只负责写剪贴板并报一行结果。

// copyWithNotice 写剪贴板并报一行结果（TUI 的拖选复制走这里）。
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
