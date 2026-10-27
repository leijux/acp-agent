package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tui"
)

// 块类型别名：repl.go 的事件渲染器用它标记「后续行属于哪类块」，
// 这样 cmd 包里不必到处 import internal/tui 的常量名。
const (
	kindSystem   = tui.KindSystem
	kindThinking = tui.KindThinking
	kindText     = tui.KindText
	kindTool     = tui.KindTool
)

// setKind 声明后续输出归属的块类型；非 TUI 模式下是 no-op。
func (a *app) setKind(k tui.BlockKind) {
	if a.ui != nil {
		a.ui.SetBlockKind(k)
	}
}

// turnCtl 保存当前 turn 的取消函数，供 Esc / Ctrl+C（界面 goroutine）
// 与提交循环（宿主 goroutine）安全共享。
type turnCtl struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (c *turnCtl) set(fn context.CancelFunc) {
	c.mu.Lock()
	c.cancel = fn
	c.mu.Unlock()
}

func (c *turnCtl) stop() {
	c.mu.Lock()
	fn := c.cancel
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// tuiRepl 是新的交互界面：Bubble Tea + 块视图。
//
// 与老 REPL 的分工：老 repl() 仍是 --no-tui / 非终端时的实现，两者共用
// 斜杠命令、agent、会话与权限层；差别只在「谁负责渲染与读键」。
func (a *app) tuiRepl(ctx context.Context, cont bool) error {
	a.ensureSession(cont)

	ui := tui.New(tui.Options{Header: a.headerLine()})
	a.ui = ui
	// 先取宽度再接管 out：t.width() 靠 out 是不是 *os.File 来判断能不能 ioctl。
	if w := a.term.width(); w > 0 {
		a.term.setCols(w)
	}
	// 共用同一个 sink：两个 sink 各自带 partial 行缓冲，交替写会互相覆盖。
	sink := ui.Sink()
	a.term.out = sink // 所有 printf/writeRaw 从此走界面
	ui.OnResize(func(w, _ int) { a.term.setCols(w) })

	ctl := &turnCtl{}
	ui.OnInterrupt(ctl.stop)
	// Ctrl-Y：复制最近一条回复。写剪贴板要起子进程（Windows 上还带 PowerShell），
	// 放到 goroutine 里跑，不能卡住界面的事件循环。
	ui.OnCopy(func() {
		go func() {
			if err := a.copyReply(1); err != nil {
				a.copyFailure(err)
			}
		}()
	})
	// 鼠标拖选：松开左键就把选中的文本写剪贴板。同样丢到 goroutine，
	// 别让 PowerShell 的启动时间堵住界面。
	ui.OnCopyText(func(text string) {
		go func() {
			if err := a.copyWithNotice("选区", text); err != nil {
				a.copyFailure(err)
			}
		}()
	})

	// 权限询问改成界面里的一行确认；Esc 等价于「否」。
	a.guard.askTUI = func(ctx context.Context, name string) (string, error) {
		return ui.AskPermission(ctx, name)
	}
	a.guard.out = sink

	// 欢迎信息：走 sink，落成系统块。
	// 名称+版本不在这里再打一遍 —— 顶部 header 行已经常驻显示，重复会看着像两遍。
	a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf(
		"模型 %s/%s  上下文 %d  权限 %s  工作目录 %s",
		a.agent.Model().Provider, a.agent.Model().ID,
		a.agent.Model().ContextWindow, a.cfg.Permissions, a.cfg.Root)))
	// 会话路径早点显示：-c 接错项目时一眼能看出来。
	a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf("会话 %s（%d 条消息）",
		a.sessPath, len(a.sess.Messages()))))
	a.term.printf("%s\n\n", a.term.paint(cDim, "输入 /help 查看命令，Ctrl-D 退出"))

	// 接着走旧会话时把历史画出来（必须在 Run 之前：消息先排队）。
	a.replayHistory(ui)

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	go a.tuiSubmitLoop(subCtx, ui, ctl)

	err := ui.Run(ctx)

	// 界面退出：先停掉正在跑的 turn（否则权限询问会永远等一个没人看的确认），
	// 再把 stdout 还回去，收尾信息才看得见。
	ctl.stop()
	stopSub()
	a.guard.askTUI = nil
	a.guard.out = os.Stdout
	a.ui = nil
	a.term.out = os.Stdout
	a.saveSession()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// tuiSubmitLoop 消费用户提交的输入：斜杠命令就地处理，其余交给 agent。
func (a *app) tuiSubmitLoop(ctx context.Context, ui *tui.UI, ctl *turnCtl) {
	for {
		select {
		case <-ctx.Done():
			return
		case input, ok := <-ui.Submit():
			if !ok {
				return
			}
			input = strings.TrimSpace(input)
			if input == "" {
				continue
			}
			if strings.HasPrefix(input, "/") {
				quit, err := a.command(ctx, input)
				if err != nil {
					a.setKind(kindSystem)
					a.term.printf("%s %v\n", a.term.paint(cRed, "错误:"), err)
				}
				ui.SetHeader(a.headerLine())
				if quit {
					ui.Quit()
					return
				}
				continue
			}
			a.tuiTurn(ctx, ui, ctl, input)
		}
	}
}

// tuiTurn 跑一轮对话。阻塞在提交 goroutine 上，界面另用一条 goroutine 刷新。
func (a *app) tuiTurn(ctx context.Context, ui *tui.UI, ctl *turnCtl, input string) {
	start := time.Now()
	turnCtx, cancel := context.WithCancel(ctx)
	ctl.set(cancel)
	ui.Running(true, "")
	a.setKind(kindText)

	res, err := a.agent.Run(turnCtx, input)

	ctl.set(nil)
	cancel()
	a.term.closeDelta()
	a.term.resetMarkdown()
	a.setKind(kindSystem)

	switch {
	case err != nil && !errors.Is(err, context.Canceled):
		a.term.printf("%s %v\n", a.term.paint(cRed, "错误:"), err)
	case errors.Is(err, context.Canceled) || (res != nil && res.Aborted):
		a.term.printf("%s\n", a.term.paint(cYellow, "已中断"))
	case res != nil:
		a.term.printUsage(res.Usage, time.Since(start))
	}
	a.term.printf("\n")
	a.saveSession()
	ui.Running(false, "")
	ui.SetHeader(a.headerLine())
}

// headerLine 是界面顶部那一行：模型 / 上下文占用 / 权限 / 工作目录。
func (a *app) headerLine() string {
	m := a.agent.Model()
	// 上下文用百分比：绝对值每秒都在变，宽度还会顶来顶去。
	ctx := "--"
	if m.ContextWindow > 0 {
		ctx = fmt.Sprintf("%.0f%%",
			float64(a.agent.ContextTokens())/float64(m.ContextWindow)*100)
	}
	return fmt.Sprintf("%s %s   %s/%s   ctx %s   %s   %s",
		a.term.paint(cBold, "acp-agent"),
		a.term.paint(cDim, version),
		m.Provider, m.ID,
		ctx,
		a.cfg.Permissions,
		a.term.paint(cDim, a.cfg.Root))
}

// ensureSession 处理 -c 续接与本次会话文件路径的分配（TUI 与老 REPL 共用）。
func (a *app) ensureSession(cont bool) {
	if cont && a.sessPath == "" {
		if p := a.store.latest(); p != "" {
			if s, err := agent.LoadSession(p); err == nil {
				a.sess = s
				a.sessPath = p
				a.agent.SetSession(s)
			}
		}
	}
	if a.sessPath == "" {
		a.sessPath = a.store.newPath()
	}
}
