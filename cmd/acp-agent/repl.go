package main

import (
	"github.com/leijux/acp-agent/internal/tools"
	"github.com/leijux/acp-agent/internal/skills"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
	"golang.org/x/term"
)

// terminal 负责所有面向人的输出：颜色、流式渲染、工具调用提示。
//
// 设计约束：stdout 在 -p 模式下是「结果」，日志必须走 stderr；
// 在 REPL 模式两者合一。
type terminal struct {
	out    io.Writer
	errOut io.Writer
	color  bool
	quiet  bool // -p 模式：正文不流式重复输出，日志走 stderr
	// cols 非 0 时直接当作终端列数（测试或已知宽度）。
	// 用 atomic：界面 goroutine 在 resize 时写，渲染侧算表格宽度时读，
	// 后者只在持 t.mu 时调用，所以不能靠 t.mu 保护（会自锁）。
	cols     atomic.Int64
	mu       sync.Mutex
	thinking bool        // 正在输出思考内容（用于换行收尾）
	inDelta  bool        // 正在流式输出正文
	md       *mdRenderer // 正文 Markdown 渲染器（惰性创建）
}

func newTerminal(out io.Writer, noColor bool) *terminal {
	color := !noColor && isTTY(out)
	t := &terminal{out: out, errOut: os.Stderr, color: color}
	t.md = newMDRenderer(t)
	return t
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// width 返回终端列数；拿不到时返回 0（调用方用兑底上限）。
//
// 表格要靠它决定要不要截断列：80 列的终端里一张 130 列的表格会折行，边框全乱。
func (t *terminal) width() int {
	if w := int(t.cols.Load()); w > 0 {
		return w
	}
	f, ok := t.out.(*os.File)
	if !ok {
		return 0
	}
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cCyan   = "\033[36m"
	cItalic = "\033[3m"
	cStrike = "\033[9m"
)

func (t *terminal) paint(code, s string) string {
	if !t.color {
		return s
	}
	return code + s + cReset
}

// writeRaw 直接写 stdout，不加锁（调用方须已持有 t.mu）。
func (t *terminal) writeRaw(s string) { fmt.Fprint(t.out, s) }

// setCols 记录终端列数（可由任意 goroutine 调用）。
func (t *terminal) setCols(w int) { t.cols.Store(int64(w)) }

// markdown 返回流式 Markdown 渲染器（调用方须已持有 t.mu）。
func (t *terminal) markdown() *mdRenderer {
	if t.md == nil { // 零值构造（测试里常见）
		t.md = newMDRenderer(t)
	}
	return t.md
}

// resetMarkdown 在每轮对话结束后清掉围栏等渲染状态。
func (t *terminal) resetMarkdown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.md != nil {
		t.md.Reset()
	}
}

// humanOut 返回「人读输出」该去的 writer：正常是 stdout，安静模式（-p /
// --mode json，stdout 已经另有用途）改走 stderr。
// 以前各处直接写 t.out，一个 web 配置错误就能把中文塞进事件流。
func (t *terminal) humanOut() io.Writer {
	if t.quiet && t.errOut != nil {
		return t.errOut
	}
	return t.out
}

func (t *terminal) printf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.humanOut(), format, args...)
}

func (t *terminal) logf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.errOut, format, args...)
}

// closeDelta 结束流式行（如果还在行内）。
func (t *terminal) closeDelta() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inDelta {
		if t.md != nil {
			t.md.Flush()
		}
		fmt.Fprintln(t.out)
		t.inDelta = false
	}
	if t.thinking {
		fmt.Fprintln(t.out)
		t.thinking = false
	}
}

// fmtTokens 把 token 数写成人类可读的短形式：567 / 1.2k / 12.3k / 123k / 1.2M。
// 原始数字位数多、秒级变化，看的人反而要多位数才能判断量级。
func fmtTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 999_500:
		if n < 100_000 {
			return trimZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
		}
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	default:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	}
}

// trimZero 去掉 "1.0" 里的 ".0"，避免 "1.0k" 这种冗余写法。
func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func (t *terminal) printUsage(u agent.Usage, elapsed time.Duration) {
	if u.TotalTokens == 0 && u.Input == 0 {
		return
	}
	t.closeDelta()
	// 数字都带人类可读单位（1.2k 而不是 1234），量级一眼看得出。
	parts := []string{
		fmt.Sprintf("tokens  in %s / out %s", fmtTokens(u.Input), fmtTokens(u.Output)),
	}
	if u.CacheRead > 0 {
		parts = append(parts, fmt.Sprintf("cache_read %s", fmtTokens(u.CacheRead)))
	}
	if u.Reasoning > 0 {
		parts = append(parts, fmt.Sprintf("reasoning %s", fmtTokens(u.Reasoning)))
	}
	if u.Cost.Total > 0 {
		sym := "$"
		if u.Cost.Currency == "CNY" {
			sym = "¥"
		} else if u.Cost.Currency != "" && u.Cost.Currency != "USD" {
			sym = u.Cost.Currency + " "
		}
		parts = append(parts, fmt.Sprintf("%s%.4f", sym, u.Cost.Total))
	}
	if elapsed > 0 {
		parts = append(parts, elapsed.Round(100*time.Millisecond).String())
	}
	line := t.paint(cDim, "· "+strings.Join(parts, "  "))
	if t.quiet {
		t.logf("%s\n", line)
		return
	}
	t.printf("%s\n", line)
}

// ---------- 事件渲染 ----------

// renderHandler 把 agent 事件翻成终端输出。
func (a *app) renderHandler() agent.EventHandler {
	t := a.term
	// -p 模式：正文由 runOnce 统一打印，这里只把工具体验写到 stderr。
	quiet := func(format string, args ...any) {
		if t.quiet {
			t.logf(format, args...)
			return
		}
		t.printf(format, args...)
	}
	return func(ev agent.Event) {
		switch e := ev.(type) {
		case agent.MessageUpdateEvent:
			if t.quiet {
				return
			}
			switch e.Kind {
			case "thinking":
				a.setKind(kindThinking)
				t.mu.Lock()
				if !t.thinking {
					if t.md != nil {
						t.md.Flush() // 收掉正文可能的半行
					}
					if a.ui != nil {
						// TUI 已有「▾ 思考」块标题，不要再打 [思考] 前缀。
						fmt.Fprint(t.out, t.paint(cDim, "\n"))
					} else {
						fmt.Fprint(t.out, t.paint(cDim, "\n[思考] "))
					}
					t.thinking = true
				}
				fmt.Fprint(t.out, t.paint(cDim, e.Delta))
				t.mu.Unlock()
			case "text":
				a.setKind(kindText)
				t.mu.Lock()
				if t.thinking {
					fmt.Fprintln(t.out, t.paint(cReset, ""))
					t.thinking = false
				}
				t.markdown().Write(e.Delta)
				t.inDelta = true
				t.mu.Unlock()
			}

		case agent.ToolExecutionStartEvent:
			t.closeDelta()
			a.setKind(kindTool)
			arg := summarizeArgs(e.Call.Arguments)
			quiet("%s %s %s\n", t.paint(cCyan, "→"), t.paint(cBold, e.Call.Name), t.paint(cDim, arg))

		case agent.ToolExecutionEndEvent:
			status := t.paint(cGreen, "✓")
			detail := ""
			if e.Result != nil {
				if e.Result.IsError {
					status = t.paint(cRed, "✗")
					detail = " " + t.paint(cRed, firstLine(blocksToText(e.Result.Content), 120))
				} else {
					detail = " " + t.paint(cDim, fmt.Sprintf("(%d 块, %s)", len(e.Result.Content), e.Duration.Round(time.Millisecond)))
				}
			}
			if e.Parallel {
				detail += t.paint(cDim, " [并行]")
			}
			quiet("%s %s%s\n", status, t.paint(cBold, e.Call.Name), detail)

		case agent.CompactionEndEvent:
			a.setKind(kindSystem)
			quiet("%s\n", t.paint(cYellow,
				fmt.Sprintf("⚕ 上下文已压缩：%d → %d token", e.TokensBefore, e.TokensAfter)))

		case agent.AgentEndEvent:
			if e.Err != nil && !errors.Is(e.Err, context.Canceled) && !t.quiet {
				a.setKind(kindSystem)
				t.closeDelta()
				// 安静模式下不在这里打：-p / --mode json 的失败原因由 main 统一写一次
				// stderr（否则同一条错误会出现两次），stdout 也不能掺人读内容。
				t.printf("%s %v\n", t.paint(cRed, "错误:"), e.Err)
			}
		}
	}
}

func blocksToText(blocks []agent.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(agent.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		s := fmt.Sprint(v)
		s = truncRunes(s, 60)
		if strings.ContainsAny(s, " \t") {
			s = strconv.Quote(s)
		}
		parts = append(parts, k+"="+s)
	}
	// map 迭代无序，排序保证输出稳定。
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, " ")
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncRunes(s, max)
}

// truncRunes 按字符（而非字节）截断，避免切碎 UTF-8 多字节字符。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- REPL ----------

const replHelp = `
可用命令：
  /help            显示帮助
  /new             开一个新会话（清空上下文）
  /compact         立即压缩上下文
  /context         显示上下文占用
  /model [id]      查看或切换模型
  /tools           列出可用工具
  /cost            本次会话累计用量
  /save [path]     保存会话
  /session         显示会话文件路径与消息数
  /init            生成 AGENTS.md 模板
  /mouse [on|off]  开关鼠标（开着才能点折叠与拖选复制，但会抢终端原生拖选）
  /freeze          把当前内容写进终端 scrollback（之后可用终端滚动回看）
  /reload [what]   重新加载 MCP 与技能（what: mcp / skills / all，默认全部）
  /exit 或 Ctrl-D  退出
  其它输入        发送给模型
（运行中按 Esc 中断当前任务，Ctrl-C 退出）

交互界面（默认，Bubble Tea）额外键位：
  鼠标左键拖选     松开即复制选中文本（已带反显高亮）
  Ctrl-↑/Ctrl-↓   在上一条/下一条提问与回答之间跳转
  Ctrl-T          折叠/展开思考（也可鼠标点击思考块）
  PgUp/PgDn 滚轮  滚动查看历史
`

func (a *app) repl(ctx context.Context, cont bool) error {
	a.ensureSession(cont)

	// 按键层：让 Esc 能在 turn 执行期间即时生效。
	//
	// 非终端（管道/重定向）或平台不支持时它拿不到，退回行模式读取 ——
	// 此时 Esc 不可用，其余功能不变。
	if a.keys == nil {
		if w, err := startKeyWatcher(int(os.Stdin.Fd()), a.term.out, &a.term.mu, a.term.width()); err == nil {
			a.keys = w
			a.guard.keys = w
			defer func() {
				a.guard.keys = nil
				a.keys = nil
				w.Close()
			}()
		}
	}

	a.term.printf("%s %s\n", a.term.paint(cBold, "acp-agent"), a.term.paint(cDim, version))
	a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf(
		"模型 %s/%s  上下文 %d  权限 %s  工作目录 %s",
		a.agent.Model().Provider, a.agent.Model().ID,
		a.agent.Model().ContextWindow, a.cfg.Permissions, a.cfg.Root)))
	a.term.printf("%s\n\n", a.term.paint(cDim, "输入 /help 查看命令，Ctrl-D 退出"))

	for {
		a.printPrompt()
		line, err := a.readLine(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, errQuitRequested) {
				a.term.printf("\n")
				a.saveSession()
				return nil
			}
			return err
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			quit, err := a.command(ctx, input)
			if err != nil {
				a.term.printf("%s %v\n", a.term.paint(cRed, "错误:"), err)
			}
			if quit {
				return nil
			}
			continue
		}

		start := time.Now()
		// 每次 turn 一个可取消的 ctx：Esc 只取消当前任务，不影响会话。
		turnCtx, cancelTurn := context.WithCancel(ctx)
		if a.keys != nil {
			a.keys.SetCancelTurn(cancelTurn)
		}
		res, err := a.agent.Run(turnCtx, input)
		if a.keys != nil {
			a.keys.SetCancelTurn(nil)
		}
		cancelTurn()
		a.term.closeDelta()
		a.term.resetMarkdown()
		if err != nil && !errors.Is(err, context.Canceled) {
			a.term.printf("%s %v\n\n", a.term.paint(cRed, "错误:"), err)
			continue
		}
		if errors.Is(err, context.Canceled) || (res != nil && res.Aborted) {
			a.term.printf("%s\n\n", a.term.paint(cYellow, "已中断"))
			continue
		}
		if res != nil {
			a.term.printUsage(res.Usage, time.Since(start))
		}
		a.saveSession()
		a.term.printf("\n")
	}
}

// printPrompt 打印 REPL 提示符，并把它记到按键层（重画整行时要一起写回去）。
func (a *app) printPrompt() {
	p := a.term.paint(cBold+cBlue, "›") + " "
	a.term.printf("%s", p)
	if a.keys != nil {
		a.keys.SetPrompt(p)
	}
}

// readLine 从按键层读一行；没有按键层（非终端/平台不支持）时退回行模式。
func (a *app) readLine(ctx context.Context) (string, error) {
	if a.keys != nil {
		line, err := a.keys.ReadLine(ctx)
		if err != nil {
			return "", err
		}
		return line + "\n", nil
	}
	return a.in.ReadString('\n')
}

// command 处理斜杠命令，返回是否退出。
func (a *app) command(ctx context.Context, input string) (bool, error) {
	fields := strings.Fields(input)
	cmd := strings.ToLower(fields[0])
	arg := ""
	if len(fields) > 1 {
		arg = strings.Join(fields[1:], " ")
	}

	switch cmd {
	case "/exit", "/quit", "/q":
		a.saveSession()
		return true, nil

	case "/help", "/h", "/?":
		a.term.printf("%s", replHelp)
		return false, nil

	case "/new":
		a.saveSession()
		s := agent.NewSession()
		a.sess = s
		a.sessPath = a.store.newPath()
		a.agent.SetSession(s)
		if a.ui != nil {
			a.ui.Reset() // 视图跟着清空，否则旧对话还挂在屏上
		}
		a.term.printf("%s\n", a.term.paint(cDim, "已开始新会话 "+a.sessPath))
		return false, nil

	case "/compact":
		before := a.agent.ContextTokens()
		res, err := a.agent.Compact(ctx, "", "manual")
		if err != nil {
			return false, err
		}
		after := a.agent.ContextTokens()
		note := ""
		switch {
		case res == nil:
			note = "（kernel 判断当前无需压缩）"
		case res.Summary == "":
			note = "（无可压缩内容）"
		}
		a.term.printf("%s\n", a.term.paint(cDim,
			fmt.Sprintf("压缩完成：%d → %d token%s", before, after, note)))
		a.saveSession()
		return false, nil

	case "/context":
		used := a.agent.ContextTokens()
		win := a.agent.Model().ContextWindow
		pct := 0.0
		if win > 0 {
			pct = float64(used) / float64(win) * 100
		}
		a.term.printf("%s\n", a.term.paint(cDim,
			fmt.Sprintf("上下文 %d / %d token（%.1f%%），%d 条消息",
				used, win, pct, len(a.sess.Messages()))))
		return false, nil

	case "/model":
		if arg == "" {
			m := a.agent.Model()
			a.term.printf("%s\n", a.term.paint(cDim,
				fmt.Sprintf("当前 %s/%s  ctx=%d  max_out=%d",
					m.Provider, m.ID, m.ContextWindow, m.MaxOutputTokens)))
			return false, nil
		}
		var m agent.Model
		if ep, ok := provider.LookupEndpoint(a.cfg.Provider); ok {
			m = ep.ModelFor(arg)
		} else {
			m = provider.ResolveModel(a.cfg.Provider, arg)
		}
		if err := a.agent.SetModel(m); err != nil {
			return false, err
		}
		a.term.printf("%s\n", a.term.paint(cDim,
			fmt.Sprintf("已切换到 %s（ctx=%d）", m.ID, m.ContextWindow)))
		return false, nil

	case "/tools":
		names := a.reg.Names()
		a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf("%d 个工具：", len(names))))
		for _, n := range names {
			if t, ok := a.reg.Get(n); ok {
				a.term.printf("  %-14s %s\n", a.term.paint(cBold, n), firstLine(t.Description(), 100))
			}
		}
		return false, nil

	case "/cost":
		u := a.agent.TotalUsage()
		sym := "$"
		if u.Cost.Currency == "CNY" {
			sym = "¥"
		}
		a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf(
			"累计 in=%d out=%d cache_read=%d reasoning=%d  %s%.4f  轮数=%d",
			u.Input, u.Output, u.CacheRead, u.Reasoning, sym, u.Cost.Total, a.agent.Turns())))
		return false, nil

	case "/save":
		path := arg
		if path == "" {
			path = a.sessPath
		}
		if path == "" {
			path = a.store.newPath()
		}
		if err := a.sess.Save(path); err != nil {
			return false, err
		}
		a.sessPath = path
		a.term.printf("%s\n", a.term.paint(cDim, "已保存到 "+path))
		return false, nil

	case "/session":
		a.term.printf("%s\n", a.term.paint(cDim, fmt.Sprintf(
			"文件 %s\n消息 %d 条  leaf=%s",
			a.sessPath, len(a.sess.Messages()), a.sess.LeafID())))
		return false, nil

	case "/init":
		return false, a.writeAgentsTemplate(arg)

	case "/mouse":
		if a.ui == nil {
			return false, fmt.Errorf("鼠标开关只在交互界面（默认）下有效")
		}
		on := strings.ToLower(arg) != "off"
		a.ui.SetMouse(on)
		if on {
			a.term.printf("%s\n", a.term.paint(cDim, "已开启鼠标（点击思考块折叠；左键拖选松开即复制）"))
		} else {
			a.term.printf("%s\n", a.term.paint(cDim, "已关闭鼠标（/mouse on 恢复）"))
		}
		return false, nil

	case "/reload":
		// 支持 /reload（全部）、/reload mcp、/reload skills。
		// 为什么需要：MCP server 与 .agents/skills 都可能在本进程运行期间被改
		// （装了个新 server、加了个新技能），以前只能退出重开。
		target := strings.ToLower(strings.TrimSpace(arg))
		doMCP := target == "" || target == "all" || target == "mcp"
		doSkills := target == "" || target == "all" || target == "skills"
		if !doMCP && !doSkills {
			return false, fmt.Errorf("/reload 支持：mcp / skills / all（当前 %q）", arg)
		}
		if doSkills {
			added, removed, err := a.reloadSkills()
			if err != nil {
				return false, err
			}
			a.term.printf("%s\n", a.term.paint(cDim,
				fmt.Sprintf("技能已重载：%d 个（+%d -%d）", added+removed, added, removed)))
		}
		if doMCP {
			servers, toolsN, err := a.reloadMCP(ctx)
			if err != nil {
				return false, err
			}
			a.term.printf("%s\n", a.term.paint(cDim,
				fmt.Sprintf("MCP 已重载：%d 个 server，%d 个工具", servers, toolsN)))
		}
		return false, nil

	case "/freeze":
		if a.ui == nil {
			return false, fmt.Errorf("/freeze 只在交互界面（默认）下有效")
		}
		a.ui.Freeze()
		return false, nil

	default:
		return false, fmt.Errorf("未知命令 %s（/help 查看）", cmd)
	}
}

func (a *app) writeAgentsTemplate(extra string) error {
	path := a.cfg.Root + string(os.PathSeparator) + "AGENTS.md"
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在", path)
	}
	tmpl := "# AGENTS.md\n\n本文件会被 agent 自动加载为上下文。\n\n" +
		"## 项目概览\n\n- 语言/框架：\n- 目录结构：\n\n" +
		"## 约定\n\n- 代码风格：\n- 测试方式：\n- 禁止事项：\n"
	if extra != "" {
		tmpl += "\n## 补充\n\n" + extra + "\n"
	}
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return err
	}
	a.term.printf("%s\n", a.term.paint(cDim, "已创建 "+path))
	return nil
}

// （会话文件的检索与分配见 sessions.go 的 sessionStore。）

// mcpToolNames 取 manager 工具里属于 MCP 的那些名字（mcp__<server>__<tool>）。
func mcpToolNames(list []tools.Tool) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		if strings.HasPrefix(t.Name(), "mcp__") {
			out = append(out, t.Name())
		}
	}
	return out
}

// reloadSkills 重新发现 .agents/skills，并把结果同步到系统提示与 skill 工具。
//
// 返回 (总数, 变化数)：变化数只用于提示里显示 ±，不参与逻辑。
func (a *app) reloadSkills() (total, changed int, err error) {
	before := map[string]bool{}
	for _, s := range a.agent.Skills() {
		before[s.Name] = true
	}
	skillSet := skills.Discover(skills.Options{
		Root:       a.cfg.Root,
		ExtraPaths: a.cfg.Skills,
		Disabled:   a.cfg.DisableSkills,
	})
	after := map[string]bool{}
	for _, s := range skillSet.Skills {
		after[s.Name] = true
	}
	for name := range after {
		if !before[name] {
			changed++ // 新增
		}
	}
	for name := range before {
		if !after[name] {
			changed++ // 移除
		}
	}

	// 系统提示里的 Skills 段由 BuildSystemPrompt 每次请求现算，这里改数据即可。
	a.agent.SetSkills(skillSet.Skills)

	// 工具表跟着变：没有技能就不该留一个只会回「没有技能」的工具。
	a.rawReg.Unregister("skill")
	if len(skillSet.Skills) > 0 {
		if err := a.rawReg.Register(tools.NewSkillTool(skillSet.Skills)); err != nil {
			return len(skillSet.Skills), changed, fmt.Errorf("注册 skill 工具失败: %w", err)
		}
	}
	return len(skillSet.Skills), changed, nil
}

// reloadMCP 断开现有 MCP 连接，重新加载配置并连接。
//
// 配置来源与启动时完全一致（--mcp-config / --mcp-json / 默认文件链 / --extension），
// 所以改完 .mcp.json 直接 /reload mcp 就能生效。
func (a *app) reloadMCP(ctx context.Context) (servers, toolsN int, err error) {
	if a.cfg.NoMCP {
		return 0, 0, fmt.Errorf("启动时带了 --no-mcp，重载也不会连接 MCP")
	}
	// 先摘掉旧工具：名字可能与新配置无关，留着就是幽灵工具。
	if a.mcp != nil {
		// 名字必须在 Close **之前**取：Close 会把 m.tools 清空（manager.go 的
		// Close 里 m.tools = map[string][]tools.Tool{}）。先 Close 再问 Tools()
		// 拿到的是空列表 → 旧工具一个都没摘 → 新连接注册时全部因同名被跳过，
		// 表现出来就是「/reload mcp 好像没生效」。
		stale := mcpToolNames(a.mcp.Tools())
		a.mcp.Close()
		a.rawReg.Unregister(stale...)
		a.mcp = nil
	}
	mgr, err := connectMCP(ctx, a.cfg, a.rawReg, a.term, true)
	if err != nil {
		return 0, 0, err
	}
	a.mcp = mgr
	if mgr == nil {
		return 0, 0, nil
	}
	connected := 0
	for _, st := range mgr.Status() {
		if st.Connected {
			connected++
		}
	}
	return connected, len(mgr.Tools()), nil
}
