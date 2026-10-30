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
	// 鼠标拖选：松开左键就把选中的文本写剪贴板。丢到 goroutine，
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

// tuiSubmitLoop 消费用户提交的输入。
//
// 两种语义：
//   - 空闲时提交 = 开一轮新对话；
//   - 运行中提交 = 作为 steering 插话（agent.Steer）：当前这轮工具调用结束后、
//     下一次模型请求前进入上下文，而不是排到整轮彻底结束之后。想立刻停下用 Esc。
//
// turn 必须放在 goroutine 里跑：主循环要一直能收输入，否则运行中的提交只能堆在
// UI 的 8 条缓冲里，等轮次结束才被取走 —— 那时 agent 已空闲，插话就退化成排队了。
// submitAction 是「这条输入该怎么处理」的决策结果。
type submitAction int

const (
	submitCommand submitAction = iota // 斜杠命令：就地执行
	submitTurn                        // 空闲：开一轮新对话
	submitSteer                       // 运行中：作为 steering 插话
)

// decideSubmit 是 tuiSubmitLoop 的唯一分支判断，单独抽出来是为了能测：
// 「运行中的普通输入必须变成 steering，而不是排到下一轮」这条语义一旦坏了，
// 表现只是行为变了（没人报错），没有测试守不住。
func decideSubmit(input string, busy bool) submitAction {
	if strings.HasPrefix(input, "/") {
		return submitCommand
	}
	if busy {
		return submitSteer
	}
	return submitTurn
}

func (a *app) tuiSubmitLoop(ctx context.Context, ui *tui.UI, ctl *turnCtl) {
	turnDone := make(chan struct{}, 1)
	var pending []string // 运行中收到的斜杠命令：命令不能插话，攒到轮次结束
	busy := false

	// handle 处理一条输入，返回是否退出。
	handle := func(input string) bool {
		switch decideSubmit(input, busy) {
		case submitCommand:
			quit, err := a.command(ctx, input)
			if err != nil {
				a.setKind(kindSystem)
				a.term.printf("%s %v\n", a.term.paint(cRed, "错误:"), err)
			}
			ui.SetHeader(a.headerLine())
			return quit
		case submitSteer:
			a.agent.Steer(input)
			a.setKind(kindSystem)
			a.term.printf("%s\n", a.term.paint(cDim,
				"↳ 已插话：这轮工具调用结束后生效（要立刻中断按 Esc）"))
			return false
		}
		busy = true
		go func(in string) {
			defer func() { turnDone <- struct{}{} }()
			a.tuiTurn(ctx, ui, ctl, in)
		}(input)
		return false
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-turnDone:
			busy = false
			q := pending
			pending = nil
			for _, in := range q {
				if handle(in) {
					ui.Quit()
					return
				}
			}
		case input, ok := <-ui.Submit():
			if !ok {
				return
			}
			// 先把「turn 刚结束」这个通知排掉：两个 case 同时就绪时 select 是随机的，
			// 先取输入会把一条本该开新一轮的消息误当成插话（Steer 后要等下一轮的
			// 第一个 assistant 结束才生效，用户会看到「发了但没反应」）。
			select {
			case <-turnDone:
				busy = false
			default:
			}
			input = strings.TrimSpace(input)
			if input == "" {
				continue
			}
			// 运行中的命令不能插话（它改的是会话/工具/模型状态），先攒着。
			if busy && strings.HasPrefix(input, "/") {
				pending = append(pending, input)
				a.setKind(kindSystem)
				a.term.printf("%s\n", a.term.paint(cDim, "↳ 命令将在当前任务结束后执行"))
				continue
			}
			if handle(input) {
				ui.Quit()
				return
			}
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
	// abort 会把还在排队的 steering / follow-up 消息退回来（Pi 语义：放回编辑器）。
	// 不处理就等于把用户刚打的插话静默丢掉。
	if res != nil && len(res.Abandoned) > 0 {
		var texts []string
		for _, m := range res.Abandoned {
			if t := strings.TrimSpace(agent.TextOf(m)); t != "" {
				texts = append(texts, t)
			}
		}
		if len(texts) > 0 {
			ui.Prefill(strings.Join(texts, "\n"))
			a.setKind(kindSystem)
			a.term.printf("%s\n", a.term.paint(cDim, "↳ 中断时排队的消息已放回输入框"))
		}
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
