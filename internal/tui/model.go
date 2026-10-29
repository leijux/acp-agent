package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	selMarker = "\x1b[33m◆\x1b[0m"
	prompt    = "\x1b[1;34m›\x1b[0m "
)

// anchor 是 Ctrl+↑/↓ 的跳转目标：一条用户消息或一段助手正文的起始行。
type anchor struct {
	line  int // flat 行号
	block int
}

// permState 是一次待确认的权限询问。
type permState struct {
	tool  string
	reply chan string
}

// Model 是 bubbletea 的界面状态。
//
// 并发约定：Model 的所有字段只在 Update/View 里被读写（bubbletea 单 goroutine
// 保证），宿主只通过 UI.Send 投递消息 —— 不要在别处直接改它。
type Model struct {
	ui   *UI
	opts Options

	width, height int
	vpH           int // 视口高度（行）

	vp      viewport.Model
	blocks  []*Block
	owners  []int // flat 行 → block 下标（鼠标点击命中测试用）
	anchors []anchor

	in      inputLine
	running bool
	note    string
	header  string
	status  string
	perm    *permState

	// 输入历史（纯内存，本进程内提交过的 prompt）。
	//
	// histPos = -1 表示「没在导航历史」（输入行里是用户自己的内容）；
	// histDraft / histPrefix 在第一次按 ↑ 时拍下，↓ 回到最新一条时靠它们
	// 把用户原本的草稿放回去（readline 行为）。
	hist       History
	histPos    int
	histDraft  string
	histPrefix string

	// turnStart / turnSeq 给「运行中 3s」那个秒表用：
	// turnSeq 让上一轮残留的 tick 作废，不会和新的计时链并发跑。
	turnStart time.Time
	turnSeq   int

	selBlock int // 当前高亮块（-1 = 无）；只用于视觉提示

	// sel 是鼠标拖选产生的文本选区（与 selBlock 的块选中是两件事）。
	sel      selection
	breakBlk bool
	mouse    bool
	turnFrom int // 本次 turn 起始的块下标（结束后自动折叠本轮思考）

	// flat 是当前完整内容（/freeze 用它写进 scrollback）。
	flat string

	// needRebuild 把重排推迟到 View：流式 partial 行每秒能来上百次，
	// 每来一次就重建全部内容（O(总行数)）会白垆 CPU。
	needRebuild bool
	lastCols    int

	// lastKeyAt / loose / looseSeq / looseNL 识别「终端没启用括号粘贴」时的落粘贴。
	// 那种情况下一次多行粘贴会被拆成一串 runes+Enter+runes…，若不拦，
	// 每个换行都变成一次提交，一条粘贴就变成好几条消息。
	// 所以先攒进 loose，停下（或到点）再整体走 inputLine.paste() 折叠。
	lastKeyAt time.Time
	loose     strings.Builder
	looseSeq  int
	looseNL   bool
	tailRunes int // 上一次直接插入的字数（落粘贴要拿它把这一段抽回去重折）

	// now 只在测试里替换，nil 用 time.Now。
	now func() time.Time

	onInterrupt func()
	onResize    func(w, h int)
	onCopyText  func(string)
}

func newModel(ui *UI, opts Options) *Model {
	m := &Model{
		ui:       ui,
		opts:     opts,
		vp:       newViewport(),
		header:   opts.Header,
		status:   opts.Status,
		selBlock: -1,
		mouse:    !opts.NoMouse,
		turnFrom: -1,
		histPos:  -1,
	}
	m.vp.MouseWheelEnabled = true
	return m
}

func (m *Model) Init() tea.Cmd { return nil }

// ---------- Update ----------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.rebuild()
		if m.onResize != nil {
			m.onResize(msg.Width, msg.Height)
		}
		return m, nil

	case RunningMsg:
		if msg.On {
			m.turnFrom = len(m.blocks)
			m.running = true
			m.turnStart = time.Now()
			m.turnSeq++
			if msg.Note != "" {
				m.note = msg.Note
			}
			// 界面只在有消息时重绘：不自己打秒表，计时会停在 0s。
			return m, tickCmd(m.turnSeq)
		}
		m.running = false
		m.note = ""
		m.turnStart = time.Time{}
		// turn 结束：收尾最后一块（思考块据此显示耗时），并自动收起本轮思考。
		m.finishOpen()
		if m.turnFrom >= 0 {
			for i := m.turnFrom; i < len(m.blocks); i++ {
				blk := m.blocks[i]
				if blk.Kind != KindThinking {
					continue
				}
				blk.finish()
				if blk.collapsible() {
					blk.Collapsed = true
				}
			}
			m.turnFrom = -1
		}
		m.needRebuild = true
		return m, nil

	case LineMsg:
		m.appendLine(msg.Kind, msg.Text)
		return m, nil

	case PartialMsg:
		m.appendPartial(msg.Kind, msg.Text)
		return m, nil

	case KindMsg:
		m.breakBlk = true
		return m, nil

	case ResetMsg:
		m.clear()
		return m, nil

	case tickMsg:
		// 只跑计时，不动内容：计时器每秒把「运行中」那行刷新一次。
		if !m.running || msg.seq != m.turnSeq {
			return m, nil
		}
		return m, tickCmd(m.turnSeq)

	case HeaderMsg:
		m.header = msg.Text
		return m, nil

	case StatusMsg:
		m.status = msg.Text
		return m, nil

	case FreezeMsg:
		if m.flat == "" {
			return m, nil
		}
		// 交给 bubbletea 的 Println：它会把这些行永久写进终端 scrollback，
		// 然后把视口重绘一遍。重绘前必须先把视口清空 —— 否则同一份内容
		// 在 scrollback 和屏上各留一份（再 /freeze 一次再叠一份），
		// 而且“写进 scrollback 之后不再可折叠”也做不到。
		flat := m.flat
		m.clear()
		return m, tea.Println(flat)

	case MouseToggleMsg:
		m.mouse = msg.On
		if msg.On {
			return m, func() tea.Msg { return tea.EnableMouseCellMotion() }
		}
		return m, func() tea.Msg { return tea.DisableMouse() }

	case CollapseThinkingMsg:
		m.sel.reset() // 折叠会把行数改一大截，旧选区行列全失效
		for _, b := range m.blocks {
			if b.collapsible() {
				b.Collapsed = true
			}
		}
		m.needRebuild = true
		return m, nil

	case AskMsg:
		// 上一个询问还没被回答：先当“否”回掉，否则它的 AskPermission
		// 会一直等到 ctx 取消（工具调用就这样悬着）。
		if m.perm != nil {
			m.answer("n")
		}
		m.perm = &permState{tool: msg.Tool, reply: msg.Reply}
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)

	case loosePasteMsg:
		// 落粘贴停止输入后的收尾：把攒的整段折叠成 [txt×N行]。
		if msg.seq == m.looseSeq {
			m.flushLoose()
		}
		return m, nil

	case tea.MouseMsg:
		return m, m.onMouse(msg)
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) onKey(msg tea.KeyMsg) tea.Cmd {
	// 权限确认优先：这时只认 y/a/n/d 与 Esc，别把按键灌进输入行。
	if m.perm != nil {
		switch msg.Type {
		case tea.KeyEsc:
			m.answer("n")
		case tea.KeyRunes:
			switch strings.ToLower(string(msg.Runes)) {
			case "y":
				m.answer("y")
			case "a":
				m.answer("a")
			case "n":
				m.answer("n")
			case "d":
				m.answer("d")
			}
		}
		return nil
	}

	rapid := m.touchKey()
	tail := m.tailRunes // 上一个键直接插进输入行的字数（落粘贴要抽回去重折）
	m.tailRunes = 0
	// 突发结束（或来了个不属于粘贴的按键）：先把攒着的落粘贴折进输入行，
	// 再让这个键按正常语义处理（回车就是要提交、方向键就是要移光标）。
	if !rapid || (msg.Type != tea.KeyRunes && msg.Type != tea.KeyEnter) {
		m.flushLoose()
	}

	switch msg.Type {
	case tea.KeyEnter:
		if msg.Alt { // Alt+Enter 不提交（便于敲多行）
			m.in.insert('\n')
			return nil
		}
		if rapid {
			// 上一次按键就在几毫秒内，这是粘贴里的换行，不是用户在提交。
			m.adoptTail(tail)
			// Windows 的行尾是 \r\n，两个字节都解析成 KeyEnter 且成对出现，
			// 吞掉第二个，否则每行之间会多出一个空行。
			if m.looseNL {
				m.looseNL = false
				return nil
			}
			m.loose.WriteByte('\n')
			m.looseNL = true
			return m.looseTick()
		}
		m.submit()
		return nil

	case tea.KeyEsc:
		if m.running {
			m.interrupt()
			return nil
		}
		m.in.reset()
		return nil

	case tea.KeyCtrlC:
		if m.running {
			m.interrupt()
			return nil
		}
		if !m.in.empty() { // 先清输入，再按一次才退出
			m.in.reset()
			return nil
		}
		return tea.Quit

	case tea.KeyCtrlD:
		if m.in.empty() {
			return tea.Quit
		}
		m.in.deleteForward()
		return nil

	case tea.KeyCtrlT:
		m.toggleRecent()
		return nil

	case tea.KeyCtrlUp:
		m.jump(-1)
		return nil

	case tea.KeyCtrlDown:
		m.jump(1)
		return nil

	case tea.KeyPgUp:
		m.vp.PageUp()
		return nil

	case tea.KeyPgDown:
		m.vp.PageDown()
		return nil

	case tea.KeyUp:
		m.historyUp()
		return nil

	case tea.KeyDown:
		m.historyDown()
		return nil

	case tea.KeyLeft:
		m.in.left()
		return nil

	case tea.KeyRight:
		m.in.right()
		return nil

	case tea.KeyHome, tea.KeyCtrlA:
		m.in.home()
		return nil

	case tea.KeyEnd, tea.KeyCtrlE:
		m.in.end()
		return nil

	case tea.KeyBackspace:
		m.in.backspace()
		return nil

	case tea.KeyDelete:
		m.in.deleteForward()
		return nil

	case tea.KeyCtrlU:
		m.in.killToStart()
		return nil

	case tea.KeyCtrlK:
		m.in.killToEnd()
		return nil

	case tea.KeySpace:
		if msg.Alt {
			m.in.insert(' ')
			return nil
		}
		m.in.insert(' ')
		return nil

	case tea.KeyRunes:
		if msg.Paste { // 括号粘贴：整段交给输入行（多行/过长会自动折叠成 [txt×N行]）
			m.in.paste(string(msg.Runes))
			m.looseNL = false
			return nil
		}
		// 没带括号粘贴标记：只能靠“紧接上一个键（几毫秒内）”识别。
		// 不能用“一次送来多个字符”当依据：输入法上屏也是一条多字符消息，
		// 那样会把中文输入延迟到收尾定时器才显示。
		if rapid {
			m.adoptTail(tail)
			for _, r := range string(msg.Runes) {
				if r >= 0x20 || r == '\t' {
					m.loose.WriteRune(r)
				}
			}
			m.looseNL = false
			return m.looseTick()
		}
		for _, r := range string(msg.Runes) {
			if r >= 0x20 || r == '\t' {
				m.in.insertString(string(r))
				m.tailRunes++
			}
		}
		return nil
	}
	return nil
}

func (m *Model) onMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.vp.ScrollUp(3)
		return nil
	case tea.MouseButtonWheelDown:
		m.vp.ScrollDown(3)
		return nil
	}
	// 按下前先把 flat/owners 对齐到最新内容：否则行号可能差一行，
	// 选区会从错的位置开始。拖动过程中不能重建（会把选区清掉）。
	if msg.Action == tea.MouseActionPress &&
		(m.needRebuild || m.lastCols != m.contentWidth()) {
		m.rebuild()
	}

	p, inBody := m.pointAt(msg.X, msg.Y)
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return nil
		}
		// 按下只记锚点：是「点一下」还是「拖一段」要等松开才知道。
		// 按在 header/输入行/状态行上的手势整段忽略（armed=false）。
		m.selBlock = -1
		m.sel = selection{armed: inBody, active: inBody, dragging: true, anchor: p, focus: p}
		return nil

	case tea.MouseActionMotion:
		if !m.sel.dragging || !m.sel.armed {
			return nil
		}
		y := msg.Y
		// 拖到视口上/下边界外：贴边继续选，并据此滚动一格，
		// 这样一个手势就能选过一屏以上的内容。
		if y < 1 {
			y = 1
			m.vp.ScrollUp(1)
		} else if y > m.vpH {
			y = m.vpH
			m.vp.ScrollDown(1)
		}
		p, ok := m.pointAt(msg.X, y)
		if !ok {
			return nil
		}
		m.sel.focus = p
		return nil

	case tea.MouseActionRelease:
		if !m.sel.dragging {
			return nil
		}
		m.sel.dragging = false
		if !m.sel.armed {
			m.sel.reset()
			return nil
		}
		if m.sel.empty() {
			// 没拖动：一次单击，保持“点思考块折叠”的老行为。
			m.sel.reset()
			if inBody {
				m.selBlock = m.owners[p.row]
				m.toggleAt(m.selBlock)
			}
			return nil
		}
		// 拖选：松开左键即复制（高亮留着，让人看清复制了哪段）。
		if text := m.selectionText(); text != "" && m.onCopyText != nil {
			m.onCopyText(text)
		}
		return nil
	}
	return nil
}

// ---------- 行为 ----------

// pasteBurstGap 是判断「这几个键是同一批粘贴」的间隔上限。
//
// 真粘贴时所有字节是同时到达终端的，相邻消息的间隔是微秒级；人打字（包括
// 输入法上屏后立刻回车）不会快到这个程度。取 30ms 是为了容下大粘贴里
// 分块读取（bubbletea 每次最多读 256 字节）引入的抖动。
const pasteBurstGap = 30 * time.Millisecond

// pasteSettle 是落粘贴「停止输入多久算粘完了」。到点就把攒的内容折成
// [txt×N行]，用户粘完松手就能看到占位符，不用等下一次按键。
const pasteSettle = 80 * time.Millisecond

// loosePasteMsg 是落粘贴的收尾定时器。seq 用来作废过期的那个。
type loosePasteMsg struct{ seq int }

func (m *Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// touchKey 记录本次按键时间，返回它是否紧接在上一次按键之后（粘贴特征）。
func (m *Model) touchKey() bool {
	now := m.clock()
	rapid := !m.lastKeyAt.IsZero() && now.Sub(m.lastKeyAt) < pasteBurstGap
	m.lastKeyAt = now
	return rapid
}

// looseTick 安排一次收尾：到 pasteSettle 还没新按键就把攒的折叠进输入行。
// 每来一个字符就重新计时，所以只有真停下来才会触发。
func (m *Model) looseTick() tea.Cmd {
	if m.loose.Len() == 0 {
		return nil
	}
	m.looseSeq++
	seq := m.looseSeq
	return tea.Tick(pasteSettle, func(time.Time) tea.Msg { return loosePasteMsg{seq: seq} })
}

// adoptTail 把上一个键刚直接插进输入行的文字抽回 loose，跟着一起折叠。
//
// 只在 loose 还空着时调用（即这个突发里的第一段），已攒了内容就说明
// 前面的分块已经在 loose 里了。tail 为 0 或光标不在那段文字末尾时不动。
func (m *Model) adoptTail(tail int) {
	if tail <= 0 || m.loose.Len() > 0 {
		return
	}
	if s := m.in.takeTail(tail); s != "" {
		m.loose.WriteString(s)
	}
}

// flushLoose 把攒着的落粘贴整段折进输入行（与括号粘贴同一套折叠规则）。
func (m *Model) flushLoose() {
	s := m.loose.String()
	m.loose.Reset()
	m.looseNL = false
	m.looseSeq++ // 作废在途的 settle tick
	m.tailRunes = 0
	if s == "" {
		return
	}
	m.in.paste(s)
}

// historyUp 切到上一条历史。
//
// 第一次进入导航（histPos<0）时拍两个快照：
//   - histDraft：用户当前正在敲的内容，↓ 回到最新一条时要原样放回去；
//   - histPrefix：用它做前缀过滤（fish 的向上搜索），否则在一个几十条历史的
//     会话里要按十几次才能找到想要的命令。没有匹配项时**不动输入行** ——
//     与 readline 的 history-search-backward 一致。
func (m *Model) historyUp() {
	if m.hist.Len() == 0 {
		return
	}
	if m.histPos < 0 {
		m.histDraft = m.in.text()
		m.histPrefix = m.histDraft
	}
	text, pos, ok := m.hist.Prev(m.histPos, m.histPrefix)
	if !ok && m.histPrefix != "" {
		// 前缀一条都不匹配（敲了半句想找相关命令，但历史里没有）：
		// 放宽成全量再找一次 —— 否则按键看起来像坏了。
		// 一旦放宽，本次导航就一直按全量走，不做「有时过滤有时不过滤」的反复。
		m.histPrefix = ""
		text, pos, ok = m.hist.Prev(m.histPos, "")
	}
	if !ok {
		return
	}
	m.histPos = pos
	m.in.setText(text)
}

// historyDown 切到下一条历史；已经在最新一条时回到用户原本的草稿。
func (m *Model) historyDown() {
	if m.histPos < 0 {
		// 没在导航历史时，↓ 不下走（滚动交给 PgDn / 滚轮）。
		return
	}
	if text, pos, ok := m.hist.Next(m.histPos, m.histPrefix); ok {
		m.histPos = pos
		m.in.setText(text)
		return
	}
	m.histPos = -1
	m.in.setText(m.histDraft)
	m.histDraft = ""
	m.histPrefix = ""
}

func (m *Model) submit() {
	m.flushLoose() // 落粘贴可能还没到收尾时间，先折进来再取文本
	text := strings.TrimSpace(m.in.text())
	if text == "" {
		return
	}
	// 记录输入历史；提交后退出历史导航（否则下一次 ↑ 会从一个旧下标继续）。
	m.hist.Push(text)
	m.histPos = -1
	m.histDraft = ""
	m.histPrefix = ""
	m.in.reset()
	// 回显成 user 块：用户需要看到「我发了什么」，尤其是多行输入。
	m.appendLine(KindUser, text)
	m.breakBlk = true
	select {
	case m.ui.submit <- text:
	default: // 宿主还没取走：丢弃，不让界面卡死
	}
}

func (m *Model) interrupt() {
	if m.onInterrupt != nil {
		m.onInterrupt()
	}
	m.note = "已请求中断…"
}

func (m *Model) answer(c string) {
	if m.perm == nil {
		return
	}
	p := m.perm
	m.perm = nil
	m.layout()
	select {
	case p.reply <- c:
	default:
	}
}

// appendLine 追加一整行；Kind 变化或强制断块时另起一块。
func (m *Model) appendLine(kind BlockKind, text string) {
	// 上一行还是半成品：这行是它的收尾（md 渲染器先吐残行、closeDelta 再补换行），
	// 替换尾行而不是另起一行，否则最后一行会出现两遍。
	//
	// 不看 breakBlk：思考→正文时宿主会先切 kind（breakBlk 被置位）、再补换行，
	// 而那个换行仍属于思考那一行，不能因为 break 就把它当新块的首行。
	if n := len(m.blocks); n > 0 {
		if blk := m.blocks[n-1]; blk.Kind == kind && blk.partial {
			blk.setTail(text)
			blk.partial = false
			m.needRebuild = true
			return
		}
	}
	blk := m.curBlock(kind)
	// 多行输入 / 多行用户消息会带着内嵌换行进来：必须拆成多行存。
	// 一条 Lines 里塞多行会让 owners/anchors 里「一行一个下标」的假设失效，
	// 之后鼠标点击命中错的块、Ctrl+↑/↓ 也会跳到错位置。
	for _, ln := range strings.Split(text, "\n") {
		blk.appendLine(ln)
	}
	m.needRebuild = true
}

// appendPartial 刷新尾部未完成行（流式逐字输出走这里）。
func (m *Model) appendPartial(kind BlockKind, text string) {
	// 尾行还不是半成品 → 这是新一行，必须追加。
	// （直接 setTail 会把上一整行吃掉：流式每行开头都会先来一次 Partial。）
	if n := len(m.blocks); n > 0 {
		if blk := m.blocks[n-1]; blk.Kind == kind && blk.partial {
			blk.setTail(text)
			m.needRebuild = true
			return
		}
	}
	blk := m.curBlock(kind)
	blk.appendLine(text)
	blk.partial = true
	m.needRebuild = true
}

func (m *Model) curBlock(kind BlockKind) *Block {
	if m.breakBlk || len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].Kind != kind {
		m.blocks = append(m.blocks, newBlock(kind))
		m.breakBlk = false
	}
	return m.blocks[len(m.blocks)-1]
}

// finishOpen 收尾最后一块（思考耗时据此显示）。
func (m *Model) finishOpen() {
	if n := len(m.blocks); n > 0 {
		m.blocks[n-1].finish()
	}
}

func (m *Model) toggleAt(i int) {
	if i < 0 || i >= len(m.blocks) {
		return
	}
	blk := m.blocks[i]
	if !blk.collapsible() {
		return
	}
	m.sel.reset() // 折叠/展开会让后面的行整体上移下移
	blk.Collapsed = !blk.Collapsed
	m.rebuild()
}

// toggleRecent 折叠/展开最近的思考块；有选中块时优先操作它。
func (m *Model) toggleRecent() {
	if m.selBlock >= 0 && m.selBlock < len(m.blocks) && m.blocks[m.selBlock].collapsible() {
		m.toggleAt(m.selBlock)
		return
	}
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].collapsible() {
			m.toggleAt(i)
			return
		}
	}
}

// jump 在「问/答」锚点之间移动：dir<0 向上，dir>0 向下。
func (m *Model) jump(dir int) {
	if len(m.anchors) == 0 {
		return
	}
	target := -1
	if m.selBlock < 0 {
		if dir < 0 {
			target = len(m.anchors) - 1 // 第一次按上：跳到最近的一条
		} else {
			target = 0
		}
	} else {
		for i := range m.anchors {
			if dir < 0 && m.anchors[i].block < m.selBlock {
				target = i
			}
			if dir > 0 && m.anchors[i].block > m.selBlock {
				target = i
				break
			}
		}
	}
	if target < 0 {
		return // 到头了不回绕：跳转要么到目标，要么什么都不做
	}
	m.selBlock = m.anchors[target].block
	m.rebuild()
	m.vp.SetYOffset(m.anchors[target].line - 1)
}

// ---------- 布局与渲染 ----------

// layout 计算视口高度：header 1 行 + 视口 + [权限行] + 输入 1 行 + 状态 1 行。
func (m *Model) layout() {
	h := m.height
	if h <= 0 {
		h = 24
	}
	extra := 0
	if m.perm != nil {
		extra = 1
	}
	vpH := h - 3 - extra
	if vpH < 1 {
		vpH = 1
	}
	m.vpH = vpH
	m.vp.Width = m.width
	if m.vp.Width <= 0 {
		m.vp.Width = 80
	}
	m.vp.Height = vpH
}

func (m *Model) contentWidth() int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

// clear 清空全部内容（/new 的 ResetMsg 与 /freeze 落盘后都用它）。
func (m *Model) clear() {
	m.blocks = nil
	m.owners = nil
	m.anchors = nil
	m.selBlock = -1
	m.turnFrom = -1
	m.sel.reset()
	m.flat = ""
	m.vp.SetContent("")
	m.vp.GotoTop()
	m.needRebuild = true
}

// rebuild 把块摊平成「视口内容 + 行归属 + 跳转锚点」。
//
// 每帧重建是 O(块数)，很便宜；真正贵的折行由 Block 自己按宽度缓存，
// 所以流式输出时只有正在写的那一块会重新折行。
func (m *Model) rebuild() {
	cols := m.contentWidth()
	// lastCols==0 表示首次布局，不算「宽度变了」。
	widthChanged := m.lastCols != 0 && m.lastCols != cols
	m.needRebuild = false
	m.lastCols = cols
	if m.vpH <= 0 {
		m.layout()
	}
	follow := m.vp.AtBottom()

	m.owners = m.owners[:0]
	m.anchors = m.anchors[:0]

	var sb strings.Builder
	line := 0
	for i, blk := range m.blocks {
		if i > 0 {
			sb.WriteByte('\n')
			m.owners = append(m.owners, i-1)
			line++
		}
		if i == m.selBlock {
			sb.WriteString(selMarker)
			sb.WriteByte('\n')
			m.owners = append(m.owners, i)
			line++
		}
		if blk.anchor() {
			m.anchors = append(m.anchors, anchor{line: line, block: i})
		}
		for _, ln := range blk.render(cols) {
			sb.WriteString(ln)
			sb.WriteByte('\n')
			m.owners = append(m.owners, i)
			line++
		}
	}
	m.flat = strings.TrimSuffix(sb.String(), "\n")
	m.vp.SetContent(m.flat)
	if follow {
		m.vp.GotoBottom()
	}
	// 重排之后同一个行号可能已经对应别的字：按情况处理。
	//   - 宽度变了：折行位置全变，选区一律作废；
	//   - 只是末尾追加（流式）：锚点以上的行号不会变，夹一下行范围就行。
	//     一刀切清空的话，输出还在流的时候根本拖不动选区。
	if widthChanged {
		m.sel.reset()
	} else {
		m.sel.clamp(len(m.owners))
	}
}

func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "正在初始化…"
	}
	m.layout()
	if m.needRebuild || m.lastCols != m.contentWidth() {
		m.rebuild()
	}
	rows := make([]string, 0, m.height)
	rows = append(rows, ansi.Truncate(m.header, m.width, "…"))

	vp := strings.Split(m.vp.View(), "\n")
	if len(vp) < m.vpH {
		vp = append(vp, make([]string, m.vpH-len(vp))...)
	}
	if len(vp) > m.vpH {
		vp = vp[len(vp)-m.vpH:]
	}
	vp = m.highlightSelection(vp)
	rows = append(rows, vp...)

	if m.perm != nil {
		rows = append(rows, ansi.Truncate(m.permView(), m.width, "…"))
	}
	rows = append(rows, ansi.Truncate(m.in.view(prompt, m.width), m.width, ""))
	rows = append(rows, ansi.Truncate(m.statusView(), m.width, ""))

	// 行数必须严格等于终端高度，否则 bubbletea 的行内重绘会错位。
	for len(rows) < m.height {
		rows = append(rows, "")
	}
	if len(rows) > m.height {
		rows = rows[:m.height]
	}
	return strings.Join(rows, "\n")
}

func (m *Model) permView() string {
	return escYellow + "允许执行 " + m.perm.tool + "？[y]是 / [a]总是 / [n]否 / [d]永不" + escReset
}

func (m *Model) statusView() string {
	left := escDim + "↑/↓ 历史 · PgUp/PgDn 滚动 · /help" + escReset
	right := ""
	if m.running {
		parts := []string{"● 运行中"}
		if e := m.elapsed(); e != "" {
			parts = append(parts, e)
		}
		if m.note != "" {
			parts = append(parts, m.note)
		}
		right = escCyan + strings.Join(parts, " ") + escReset
	} else if m.status != "" {
		right = escDim + m.status + escReset
	}
	if right == "" {
		return left
	}
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		// 空间不够：右侧状态优先（运行中/中断提示不能丢），
		// 左侧提示按剩余宽度截断并带省略号，而不是整段消失。
		avail := m.width - ansi.StringWidth(right) - 2 // 留 1 列空格 + 1 列省略号
		if avail < 10 {
			return ansi.Truncate(right, m.width, "…")
		}
		return ansi.Truncate(left, avail, "…") + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

// tickMsg 是秒表心跳；seq 用于丢弃上一轮遗留的计时链。
type tickMsg struct{ seq int }

// tickCmd 安排下一次秒表心跳。
func tickCmd(seq int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{seq: seq} })
}

// elapsed 返回当前 turn 已跑的时长（没在跑时为空）。
func (m *Model) elapsed() string {
	if m.turnStart.IsZero() {
		return ""
	}
	return fmtElapsed(time.Since(m.turnStart))
}

// fmtElapsed 把时长写成 "3s" / "1m05s" / "1h02m"：
// 秒是可以直接感知的量级，保留小数反而要多看一眼。
func fmtElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
