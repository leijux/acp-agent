// Package tui 是 REPL 的终端界面层：可滚动、可折叠、可用鼠标点击展开的块视图。
//
// 设计边界：本包只认「块」与「行」，不认识 markdown / agent / 工具。
// 宿主（cmd/acp-agent）负责把**已经渲染好的行**（可带 ANSI）灌进来，并用
// LineMsg/PartialMsg 上的 Kind 声明行归属。这样命令输出、工具提示、流式
// markdown 正文共用同一条通路，本包也不会反向依赖 cmd 里的渲染器。
//
// 为什么不用 alt-screen：REPL 的产出（对话）是要能被终端原生滚动、复制、
// 滚出屏幕后仍能回看的。alt-screen 会把这些内容关在一个临时屏里，退出即失。
// 所以这里保持 inline 渲染：视口是我们自己维护的一段，退出后最后一屏仍留在
// 终端里；需要整屏回看时用 /freeze（把当前视口一次性写入 scrollback）。
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// 颜色常量：与 cmd/acp-agent 的 terminal 保持同一套 SGR，避免两处观感不一致。
const (
	escReset   = "\x1b[0m"
	escDim     = "\x1b[2m"
	escBold    = "\x1b[1m"
	escBlue    = "\x1b[34m"
	escYellow  = "\x1b[33m"
	escCyan    = "\x1b[36m"
	escRed     = "\x1b[31m"
	escReverse = "\x1b[7m"
	// escUnreverse 只关反显，保留这一行原本的 dim/bold 等属性。
	escUnreverse = "\x1b[27m"
)

// BlockKind 是块的语义类型，只影响标题、能否折叠与是否作为跳转锚点。
type BlockKind int

const (
	KindSystem   BlockKind = iota // 提示、错误、usage、命令输出
	KindUser                      // 用户提交的输入
	KindThinking                  // 思考（可折叠）
	KindText                      // 助手正文
	KindTool                      // 工具调用提示
)

// ---------- 宿主 → 界面 的消息 ----------

// LineMsg 提交一整行（不含换行）。Kind 变化处自动切块。
type LineMsg struct {
	Kind BlockKind
	Text string
}

// PartialMsg 提交「尚未换行」的当前行（流式逐字输出会反复刷新同一行）。
type PartialMsg struct {
	Kind BlockKind
	Text string
}

// KindMsg 强制断开当前块：下一次追加即使 Kind 相同也另起一块。
type KindMsg struct{ Kind BlockKind }

// RunningMsg 告知是否有 turn 在跑（影响状态行与 Ctrl+C 语义）。
type RunningMsg struct {
	On   bool
	Note string
}

// ResetMsg 清空所有块（/new 用）。
type ResetMsg struct{}

// HeaderMsg 更新顶部固定行（切模型、上下文变化时）。
type HeaderMsg struct{ Text string }

// StatusMsg 更新底部状态行右侧文字。
type StatusMsg struct{ Text string }

// AskMsg 请求一次权限确认；界面按键后把 "y"/"a"/"n"/"d" 写回 Reply。
type AskMsg struct {
	Tool  string
	Reply chan string
}

// MouseToggleMsg 开关鼠标上报。开着会抢走终端拖选复制（需 Shift 绕过），
// 所以留了 /mouse off 这个口子。
type MouseToggleMsg struct{ On bool }

// FreezeMsg 把当前全部内容一次性写进终端 scrollback（/freeze）：
// 之后就能用终端自己的滚动/搜索，代价是这些行不再可折叠。
type FreezeMsg struct{}

// CollapseThinkingMsg 折叠已有全部思考块（回放历史后调用：
// 历史里的思考默认收起，界面才不会被几屏旧思考塔满）。
type CollapseThinkingMsg struct{}

// ---------- 块 ----------

// Block 是一个语义块。Lines 里存的是宿主渲染好的行（可能带 ANSI）。
type Block struct {
	Kind      BlockKind
	Lines     []string
	Collapsed bool
	Started   time.Time
	Duration  time.Duration

	// wrapCols/dirty 是折行缓存：宽度变化或内容变化后重排，避免每帧重算。
	wrapCols int
	dirty    bool
	wrapped  []string

	// partial 表示尾行还是「尚未换行的半成品」（PartialMsg 写下的）。
	// 下一条同 Kind 的 LineMsg 是它的收尾，要替换而非追加，
	// 否则流式输出的最后一行会在屏上出现两遍。
	partial bool
}

func newBlock(kind BlockKind) *Block {
	return &Block{Kind: kind, Started: time.Now(), dirty: true}
}

// collapsible 只有思考块可折叠（正文折叠会让人以为回答没了）。
func (b *Block) collapsible() bool { return b.Kind == KindThinking && len(b.Lines) > 0 }

// anchor 报告该块是否作为 Ctrl+↑/↓ 的跳转目标（一问一答）。
func (b *Block) anchor() bool { return b.Kind == KindUser || b.Kind == KindText }

func (b *Block) appendLine(s string) {
	b.Lines = append(b.Lines, s)
	b.dirty = true
}

// setTail 替换最后一行（流式 partial 行刷新用）；没有行时等价于追加。
func (b *Block) setTail(s string) {
	if n := len(b.Lines); n > 0 {
		b.Lines[n-1] = s
	} else {
		b.Lines = append(b.Lines, s)
	}
	b.dirty = true
}

func (b *Block) finish() {
	if !b.Started.IsZero() {
		b.Duration = time.Since(b.Started)
	}
}

// title 是折叠态的标题行：折叠时必须一眼看出「这里有东西被收起来了」。
func (b *Block) title() string {
	arrow := "▾"
	if b.Collapsed {
		arrow = "▸"
	}
	parts := []string{fmt.Sprintf("%d 行", len(b.Lines))}
	if b.Duration > 0 {
		parts = append(parts, b.Duration.Round(100*time.Millisecond).String())
	}
	if b.Collapsed {
		parts = append(parts, "点击/⌃T 展开")
	}
	return fmt.Sprintf("%s%s 思考 · %s%s", escDim, arrow, strings.Join(parts, " · "), escReset)
}

// render 返回该块在 cols 宽度下的屏幕行（含标题行）。
func (b *Block) render(cols int) []string {
	if b.collapsible() && b.Collapsed {
		return []string{b.title()}
	}
	body := b.body(cols)
	switch {
	case b.collapsible():
		return append([]string{b.title()}, body...)
	case b.Kind == KindUser:
		// 用户消息加一个明显的标签行：否则一问一答混在一起分不清。
		return append([]string{escBold + escBlue + "▌ 你" + escReset}, body...)
	}
	return body
}

func (b *Block) body(cols int) []string {
	if !b.dirty && b.wrapCols == cols {
		return b.wrapped
	}
	out := make([]string, 0, len(b.Lines))
	for _, ln := range b.Lines {
		out = append(out, wrapLine(ln, cols)...)
	}
	b.wrapped, b.wrapCols, b.dirty = out, cols, false
	return out
}

// wrapLine 按显示宽度硬折行，保留 ANSI（CJK 宽字符、颜色跨行都不破）。
//
// 必须在界面层折行：viewport 不会替我们折，超宽行会被直接截断。
func wrapLine(s string, cols int) []string {
	if cols <= 0 || ansi.StringWidth(s) <= cols {
		return []string{s}
	}
	return strings.Split(ansi.Hardwrap(s, cols, true), "\n")
}

// ---------- UI 句柄 ----------

// Options 是界面初始化参数。
type Options struct {
	Header  string // 顶部固定行（模型/权限/工作目录）
	Status  string // 底部状态行右侧
	NoMouse bool   // 关掉鼠标上报（/mouse off 也能关）
	FPS     int    // 渲染帧率上限，默认 30
}

// UI 是 TUI 的对外句柄，所有方法都可以从任意 goroutine 调用。
type UI struct {
	opts   Options
	model  *Model
	prog   *tea.Program
	submit chan string

	mu      sync.Mutex
	kind    BlockKind
	started bool
	closed  bool
	pending []tea.Msg
	// sink 是最近一次 Sink() 创建的实例（FlushLine 需要它的行缓冲状态）。
	sink *lineSink
}

// New 构造界面（此时还不会碰终端，Run 才接管）。
func New(opts Options) *UI {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	u := &UI{
		opts:   opts,
		submit: make(chan string, 8),
	}
	u.model = newModel(u, opts)
	return u
}

// Submit 返回用户提交的输入（已 TrimSpace 且非空）。
func (u *UI) Submit() <-chan string { return u.submit }

// OnInterrupt 注册 Esc / Ctrl+C 打断当前 turn 的回调（由宿主接上取消函数）。
func (u *UI) OnInterrupt(fn func()) { u.model.onInterrupt = fn }

// OnResize 注册终端尺寸变化回调（宿主用它同步渲染宽度）。
func (u *UI) OnResize(fn func(w, h int)) { u.model.onResize = fn }

// OnCopyText 注册拖选复制回调：鼠标松开时把选中的纯文本交给它。
// 界面不碰剪贴板（那是平台相关的事，属于宿主）。
func (u *UI) OnCopyText(fn func(string)) { u.model.onCopyText = fn }

// Run 接管终端并阻塞到退出。
func (u *UI) Run(ctx context.Context) error {
	popts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithFPS(u.opts.FPS),
	}
	u.prog = tea.NewProgram(u.model, popts...)

	u.mu.Lock()
	u.started = true
	pending := u.pending
	u.pending = nil
	u.mu.Unlock()
	go func() {
		for _, msg := range pending {
			u.prog.Send(msg)
		}
	}()

	_, err := u.prog.Run()

	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()

	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

// Quit 退出界面（/exit 或 Ctrl+D）。
func (u *UI) Quit() {
	u.mu.Lock()
	started := u.started
	prog := u.prog
	u.mu.Unlock()
	if started && prog != nil {
		prog.Quit()
		return
	}
	u.Send(tea.Quit())
}

// Send 投递消息。程序未启动时排队（Send 在 Run 之前会阻塞，会把调用方吊死），
// 退出后丢弃（bubbletea 的 Send 在退出后是 no-op，这里提前避开）。
func (u *UI) Send(msg tea.Msg) {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	if !u.started {
		u.pending = append(u.pending, msg)
		u.mu.Unlock()
		return
	}
	prog := u.prog
	u.mu.Unlock()
	prog.Send(msg)
}

// SetBlockKind 声明后续行归属的块类型。
func (u *UI) SetBlockKind(k BlockKind) {
	u.mu.Lock()
	changed := u.kind != k
	u.kind = k
	u.mu.Unlock()
	if changed {
		u.Send(KindMsg{Kind: k})
	}
}

// CurrentKind 返回当前的块类型（sink 给行打标签用）。
func (u *UI) CurrentKind() BlockKind {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.kind
}

// Pending 返回尚未投递给界面的消息副本（诊断与测试用；界面启动后为空）。
func (u *UI) Pending() []tea.Msg {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]tea.Msg(nil), u.pending...)
}

// Sink 返回一个 io.Writer：写进去的字节流会被切成行送进界面。
// 宿主的 terminal 直接把 out 指向它，于是所有 printf/writeRaw 自动上屏。
func (u *UI) Sink() io.Writer {
	s := &lineSink{ui: u}
	u.mu.Lock()
	u.sink = s // 记住当前 sink：FlushLine 要用它
	u.mu.Unlock()
	return s
}

// FlushLine 把行缓冲里未换行的残余收尾成一行（没有残余则什么也不做）。
//
// 留给宿主在「快边插自己的输出」时用：行缓冲是共用的，不收尾的话
// 提示文字会粘到流式半行的尾巴上。
func (u *UI) FlushLine() {
	u.mu.Lock()
	s := u.sink
	u.mu.Unlock()
	if s != nil {
		s.flushLine()
	}
}

// Running 更新「是否有 turn 在跑」。
func (u *UI) Running(on bool, note string) {
	u.Send(RunningMsg{On: on, Note: note})
}

// Reset 清空会话块。
func (u *UI) Reset() { u.Send(ResetMsg{}) }

// SetHeader 更新顶部固定行。
func (u *UI) SetHeader(s string) { u.Send(HeaderMsg{Text: s}) }

// SetStatus 更新底部状态行。
func (u *UI) SetStatus(s string) { u.Send(StatusMsg{Text: s}) }

// SetMouse 开关鼠标上报。
func (u *UI) SetMouse(on bool) { u.Send(MouseToggleMsg{On: on}) }

// Freeze 把当前内容写进终端 scrollback。
func (u *UI) Freeze() { u.Send(FreezeMsg{}) }

// CollapseThinking 折叠当前已有全部思考块。
func (u *UI) CollapseThinking() { u.Send(CollapseThinkingMsg{}) }

// UserLine 记录一条用户输入（作为独立的 user 块）。
func (u *UI) UserLine(text string) {
	u.Send(LineMsg{Kind: KindUser, Text: text})
	u.Send(KindMsg{Kind: KindUser})
}

// AskPermission 请求一次权限确认。ctx 取消或界面退出时返回错误（调用方按拒绝处理）。
func (u *UI) AskPermission(ctx context.Context, tool string) (string, error) {
	reply := make(chan string, 1)
	u.Send(AskMsg{Tool: tool, Reply: reply})
	select {
	case c := <-reply:
		return c, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ---------- sink ----------

// lineSink 把字节流切成行。未完成的尾巴作为 partial 行反复刷新——
// 思考内容是逐 token 写进来的，不刷新就一个字都看不到。
type lineSink struct {
	ui  *UI
	mu  sync.Mutex
	buf []byte
	// kind 是 buf 里那些字节写入时的块类型，不等于 ui.CurrentKind()：
	// 宿主常常「先切 types、再补换行」（如思考→正文），若按当前 kind 打标签，
	// 思考的尾行会被当成正文第一行发到新块里，多出一行。
	kind BlockKind
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		s.kind = s.ui.CurrentKind()
	}
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(s.buf[:i]), "\r")
		rest := len(s.buf) - i - 1
		copy(s.buf, s.buf[i+1:])
		s.buf = s.buf[:rest]
		s.ui.Send(LineMsg{Kind: s.kind, Text: line})
		// 剩下的字节属于下一行：按此刻的 kind 归属。
		s.kind = s.ui.CurrentKind()
	}
	if len(s.buf) > 0 {
		s.ui.Send(PartialMsg{Kind: s.kind, Text: string(s.buf)})
	}
	return len(p), nil
}

// flushLine 把未换行的残余收尾成一行；没有残余时什么也不做。
//
// 宿主在跨 goroutine 插自己的输出（如复制成功提示）前调用：
// 行缓冲是共用的，不收尾就会把两个来源的文字拼成同一行。
func (s *lineSink) flushLine() {
	s.mu.Lock()
	n := len(s.buf)
	s.mu.Unlock()
	if n == 0 {
		return
	}
	_, _ = s.Write([]byte("\n"))
}

// viewport 在本包内部组装，不对宿主暴露。
func newViewport() viewport.Model {
	return viewport.New(viewport.WithWidth(0), viewport.WithHeight(0))
}
