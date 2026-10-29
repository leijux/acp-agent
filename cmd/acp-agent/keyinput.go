package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/leijux/acp-agent/internal/tui"
)

// REPL 的按键层：在 cbreak 模式下独占 stdin，把字节流变成「行」与「中断」。
//
// 为什么需要它：行模式（cooked）下终端自己攒行，Esc 永远不会被进程看到 ——
// 用户按 Esc 只是往行缓冲里塞了一个不可见字节，要等回车才送达。想中途打断
// 一个正在跑的 turn，就必须关掉行缓冲。
//
// 关掉行缓冲后终端不再回显，所以行编辑自己做：
//   - 字符回显、退格、回车提交；
//   - ←/→ Home/End Delete Ctrl-A/E/U/K；
//   - Esc 打断当前 turn（空闲时清空当前行）。
//
// 方向键必须在这里解析：终端发的 ←/→ 是 ESC [ D / ESC [ C，如果「见到
// 0x1b 就当 Esc」，按一下方向键就会把正在跑的任务打断、或者把输入行清空。
//
// 单一读者是关键：整个 REPL 生命周期只由这个 watcher 读 os.Stdin，
// 权限询问（guard.ask）也从这里取行，避免两个 reader 抢字节。

// errQuitRequested 表示用户按了 Ctrl+C / Ctrl+D，要求退出 REPL。
var errQuitRequested = errors.New("用户请求退出")

type keyWatcher struct {
	reader *bufio.Reader
	out    io.Writer
	// outMu 非空时每次写屏先拿它：回显和 REPL 的流式渲染共用同一个 stdout，
	// 不串行化会把整行重画的 CSI 序列切进正文里。
	outMu sync.Locker

	lines chan string
	quit  chan struct{}
	eof   chan struct{}

	// cancelTurn 非空时，Esc 取消当前 turn（清空行）；为空表示空闲。
	cancelTurn atomic.Value // context.CancelFunc
	// reading 表示有人正阻塞在 ReadLine（权限询问也走它）：此时即使 turn
	// 在跑，也必须接收输入。
	reading atomic.Bool

	mu     sync.Mutex
	buf    []byte // 当前行内容
	cur    int    // 插入点（buf 的字节下标）
	prompt string // 当前提示符（可能含 ANSI），重绘用
	cols   int    // 终端列数；0 表示未知（按单行处理）
	closed bool

	// 输入历史（纯内存，与 TUI 模式共用 tui.History 的语义）：
	// histPos = -1 表示没在导航；histDraft 是第一次按 ↑ 之前的内容，
	// ↓ 到最新一条时原样放回去。
	hist       tui.History
	histPos    int
	histDraft  string
	histPrefix string

	// tailUntil：刚把单个 ESC 当 Esc 处理后的一个短窗口。终端偶尔会把
	// ESC 与 "[D" 拆成两次送达（慢链路 / ssh），这时剩下的 "[D" 不能被
	// 当成用户输入写进行里。只由 loop goroutine 读写。
	tailUntil time.Time

	stopOnce sync.Once
	restore  func()
}

// startKeyWatcher 把终端切到 cbreak 并在后台读键。
//
// outMu / cols 在 loop 启动前写入（启动后再写就是数据竞争）：
// outMu 让回显与 REPL 的流式渲染共用同一个 stdout；cols 用重画时算折行。
//
// fd 不是终端（管道、重定向）时返回错误，调用方应退回行模式读取。
func startKeyWatcher(fd int, out io.Writer, outMu sync.Locker, cols int) (*keyWatcher, error) {
	restore, err := enterCbreak(fd)
	if err != nil {
		return nil, err
	}
	w := newKeyWatcher(os.Stdin, out, restore)
	w.outMu = outMu
	w.cols = cols
	go w.loop()
	return w, nil
}

// newKeyWatcher 构造按键层（不切终端模式）；测试直接注入 reader。
func newKeyWatcher(r io.Reader, out io.Writer, restore func()) *keyWatcher {
	// histPos=-1：初始不在历史导航中。
	return &keyWatcher{
		reader:  bufio.NewReader(r),
		out:     out,
		lines:   make(chan string, 8),
		quit:    make(chan struct{}),
		eof:     make(chan struct{}),
		restore: restore,
		histPos: -1, // -1：不在历史导航中
	}
}

// SetCancelTurn 设置 Esc 的中断目标；传 nil 表示回到空闲态。
func (w *keyWatcher) SetCancelTurn(cancel context.CancelFunc) {
	if cancel == nil {
		w.cancelTurn.Store(context.CancelFunc(nil))
		return
	}
	w.cancelTurn.Store(cancel)
}

// SetPrompt 记住提示符（含 ANSI 颜色）。整行重画时要把它一起写回去，
// 否则擦干净之后光标前就没有 "› " 了。
func (w *keyWatcher) SetPrompt(p string) {
	w.mu.Lock()
	w.prompt = p
	w.mu.Unlock()
}

// SetCols 设置终端列数（0 = 未知）。折行后要擦掉多行，必须知道列数。
func (w *keyWatcher) SetCols(n int) {
	w.mu.Lock()
	w.cols = n
	w.mu.Unlock()
}

// ReadLine 读取一行（不含换行）。ctx 取消或用户要求退出时返回错误。
func (w *keyWatcher) ReadLine(ctx context.Context) (string, error) {
	w.reading.Store(true)
	defer w.reading.Store(false)

	// 退出信号优先于残留行：Ctrl+C / Ctrl+D 就该立刻走。
	select {
	case <-w.quit:
		return "", errQuitRequested
	default:
	}

	// 再看有没有已经入队的行。
	//
	// 必须这么判：输入流读完会立即 EOF（管道、重定向、strings.Reader），
	// 而最后一行在 EOF 之前就已入队，两个 case 同时就绪时 select 随机选，
	// 有几率把用户最后一条输入丢掉（老 bug：echo “任务” | acp-agent --no-tui 偶发不执行）。
	if line, ok := w.takeLine(); ok {
		return line, nil
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.quit:
		return "", errQuitRequested
	case line := <-w.lines:
		return line, nil
	case <-w.eof:
		// submit() 在 loop 读下一次前就把行入了队，所以 eof 就绪时行已在队列里。
		if line, ok := w.takeLine(); ok {
			return line, nil
		}
		return "", io.EOF
	}
}

// takeLine 非阻塞取一行。
func (w *keyWatcher) takeLine() (string, bool) {
	select {
	case line := <-w.lines:
		return line, true
	default:
		return "", false
	}
}

// Close 恢复终端模式。可重复调用。
func (w *keyWatcher) Close() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		if w.restore != nil {
			w.restore()
		}
	})
}

func (w *keyWatcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *keyWatcher) loop() {
	for {
		b, err := w.reader.ReadByte()
		if err != nil {
			// 输入流结束（管道关闭、真实 stdin EOF）。注意 Ctrl+D 已经在上面的
			// 字节分支里当成退出处理，不会走到这里。
			w.requestEOF()
			return
		}
		if w.isClosed() {
			return
		}

		switch b {
		case 0x1b: // Esc 或控制序列的前缀
			w.onEscape()
		case 0x03: // Ctrl+C
			// turn 正在跑时先打断它，再退出；否则主循环会先读回退出信号。
			if cancel, _ := w.cancelTurn.Load().(context.CancelFunc); cancel != nil {
				cancel()
			}
			w.requestQuit()
			return
		case 0x04: // Ctrl+D
			w.requestQuit()
			return
		case '\r', '\n':
			w.submit()
		case 0x7f, 0x08: // 退格
			w.backspace()
		case 0x15: // Ctrl+U 删到行首
			w.killToStart()
		case 0x0b: // Ctrl+K 删到行尾
			w.killToEnd()
		case 0x01: // Ctrl+A
			w.moveHome()
		case 0x05: // Ctrl+E
			w.moveEnd()
		default:
			if w.inEscTail() && (b == '[' || b == 'O') {
				// 拆成两次送达的序列尾巴：b 已经是 '['/'O'，继续把剩下的一半吃掉。
				w.tailUntil = time.Time{}
				w.readSequenceFrom(b)
				continue
			}
			if b >= 0x20 {
				w.appendByte(b)
			}
		}
	}
}

// editable 报告当前按键是否该进入行编辑。
//
// turn 运行期间默认不接收输入：这时屏幕上是流式正文，回显/重绘会把它搅乱，
// 提前敲的字也不该算进下一个 prompt。有人在等 ReadLine（权限询问）时例外。
func (w *keyWatcher) editable() bool {
	if w.reading.Load() {
		return true
	}
	cancel, _ := w.cancelTurn.Load().(context.CancelFunc)
	return cancel == nil
}

// onEscape 区分「用户按了 Esc」和「控制序列」。
//
// 方向键/Home/End/Delete 都以 ESC 开头，而 Esc 键本身只发一个字节。这里靠
// 「ESC 后面是否紧跟 [ 或 O」来判断 —— 终端会把一个按键的字节一次性写进
// 管道，bufio 一次 Read 就全拿到了，所以 Buffered() 在这一刻是可靠的。
func (w *keyWatcher) onEscape() {
	if w.reader.Buffered() > 0 {
		if nb, err := w.reader.Peek(1); err == nil && (nb[0] == '[' || nb[0] == 'O') {
			w.readSequence()
			return
		}
	}

	cancel, _ := w.cancelTurn.Load().(context.CancelFunc)
	if cancel != nil {
		cancel()
		return
	}
	if !w.editable() {
		return
	}
	// 单个 Esc：设一个短窗口，防止序列被拆成两次送达时 "[D" 漏进行里。
	w.tailUntil = time.Now().Add(20 * time.Millisecond)

	// 空闲：丢掉半截输入，并把提示符重画出来（旧实现只换行，提示符就没了）。
	w.mu.Lock()
	w.buf = w.buf[:0]
	w.cur = 0
	w.renderLocked()
	w.mu.Unlock()
}

// readSequence 吃掉 ESC 后的控制序列并分发。
//
// CSI：ESC [ 参数字节 (0x30-0x3f) 中间字节 (0x20-0x2f) 终止字节 (0x40-0x7e)
// SS3：ESC O 终止字节（应用模式下的方向键/Home/End）
func (w *keyWatcher) readSequence() {
	b, err := w.reader.ReadByte() // '[' 或 'O'
	if err != nil {
		return
	}
	w.readSequenceFrom(b)
}

// readSequenceFrom 处理序列首字节（'[' 或 'O'）之后的部分。
func (w *keyWatcher) readSequenceFrom(b byte) {
	if b == 'O' {
		f, err := w.reader.ReadByte()
		if err != nil {
			return
		}
		w.onSequenceKey(f, "")
		return
	}

	var params []byte
	for i := 0; i < 16; i++ { // 参数长度设上限，别被垃圾字节拖住
		c, err := w.reader.ReadByte()
		if err != nil {
			return
		}
		switch {
		case c >= 0x40 && c <= 0x7e: // 终止字节
			w.onSequenceKey(c, string(params))
			return
		case c >= 0x20 && c < 0x40: // 参数 / 中间字节
			params = append(params, c)
		default: // 非法序列：丢弃
			return
		}
	}
}

func (w *keyWatcher) onSequenceKey(final byte, params string) {
	if !w.editable() {
		return
	}
	switch final {
	case 'C': // →
		w.moveRight()
	case 'D': // ←
		w.moveLeft()
	case 'A': // ↑ 输入历史
		w.historyUp()
	case 'B': // ↓ 输入历史
		w.historyDown()
	case 'H': // Home
		w.moveHome()
	case 'F': // End
		w.moveEnd()
	case '~':
		switch params {
		case "1", "7":
			w.moveHome()
		case "4", "8":
			w.moveEnd()
		case "3": // Delete
			w.deleteAt()
		}
	}
	// PgUp/PgDn 等一律忽略。
}

// inEscTail 报告是否处在「刚把单个 ESC 当 Esc 处理」的短窗口内。
// 窗口内紧跟的 '['/'O' 视为序列拆分送达的尾巴，不能当输入。
func (w *keyWatcher) inEscTail() bool {
	return !w.tailUntil.IsZero() && time.Now().Before(w.tailUntil)
}

func (w *keyWatcher) requestQuit() {
	select {
	case <-w.quit:
	default:
		close(w.quit)
	}
}

// requestEOF 标记输入流结束。
func (w *keyWatcher) requestEOF() {
	select {
	case <-w.eof:
	default:
		close(w.eof)
	}
}

// ---------- 行编辑 ----------

func (w *keyWatcher) appendByte(b byte) {
	if !w.editable() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	atEnd := w.cur == len(w.buf)
	w.buf = append(w.buf, 0)
	copy(w.buf[w.cur+1:], w.buf[w.cur:])
	w.buf[w.cur] = b
	w.cur++
	if atEnd { // 行尾追加直接回显，省一次整行重画
		w.writeString(string([]byte{b}))
		return
	}
	w.renderLocked()
}

func (w *keyWatcher) backspace() {
	if !w.editable() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == 0 {
		return
	}
	start := runeStartBefore(w.buf, w.cur)
	w.buf = append(w.buf[:start], w.buf[w.cur:]...)
	w.cur = start
	w.renderLocked()
}

func (w *keyWatcher) deleteAt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur >= len(w.buf) {
		return
	}
	end := runeStartAfter(w.buf, w.cur)
	w.buf = append(w.buf[:w.cur], w.buf[end:]...)
	w.renderLocked()
}

func (w *keyWatcher) moveLeft() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == 0 {
		return
	}
	w.cur = runeStartBefore(w.buf, w.cur)
	w.renderLocked()
}

func (w *keyWatcher) moveRight() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur >= len(w.buf) {
		return
	}
	w.cur = runeStartAfter(w.buf, w.cur)
	w.renderLocked()
}

func (w *keyWatcher) moveHome() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = 0
	w.renderLocked()
}

func (w *keyWatcher) moveEnd() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = len(w.buf)
	w.renderLocked()
}

func (w *keyWatcher) killToStart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf[:0], w.buf[w.cur:]...)
	w.cur = 0
	w.renderLocked()
}

func (w *keyWatcher) killToEnd() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = w.buf[:w.cur]
	w.renderLocked()
}

// historyUp 把输入行换成上一条历史（↑）。
//
// 语义与 TUI 模式一致（共用 tui.History）：第一次按 ↑ 时记住当前行与它作为
// 前缀；前缀没有匹配时放宽成全量，免得按键看起来像坏的。
func (w *keyWatcher) historyUp() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hist.Len() == 0 {
		return
	}
	if w.histPos < 0 {
		w.histDraft = string(w.buf)
		w.histPrefix = w.histDraft
	}
	text, pos, ok := w.hist.Prev(w.histPos, w.histPrefix)
	if !ok && w.histPrefix != "" {
		w.histPrefix = ""
		text, pos, ok = w.hist.Prev(w.histPos, "")
	}
	if !ok {
		return
	}
	w.histPos = pos
	w.setBufLocked(text)
	w.renderLocked()
}

// historyDown 把输入行换成下一条历史（↓）；已经在最新一条时回到原草稿。
func (w *keyWatcher) historyDown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.histPos < 0 {
		return
	}
	if text, pos, ok := w.hist.Next(w.histPos, w.histPrefix); ok {
		w.histPos = pos
		w.setBufLocked(text)
		w.renderLocked()
		return
	}
	w.histPos = -1
	w.setBufLocked(w.histDraft)
	w.histDraft = ""
	w.histPrefix = ""
	w.renderLocked()
}

// setBufLocked 整体替换当前行。调用方必须持有 w.mu。
func (w *keyWatcher) setBufLocked(s string) {
	w.buf = append(w.buf[:0], s...)
	w.cur = len(w.buf)
}

func (w *keyWatcher) submit() {
	if !w.editable() {
		return
	}
	w.mu.Lock()
	line := string(w.buf)
	if w.cur != len(w.buf) {
		// 折行/光标在中间时先把光标挪到行尾，否则 \r\n 会插在屏幕中间。
		w.cur = len(w.buf)
		w.renderLocked()
	}
	// 记录输入历史，并退出导航（下次 ↑ 从最新一条重新开始）。
	w.hist.Push(line)
	w.histPos = -1
	w.histDraft = ""
	w.histPrefix = ""
	w.buf = w.buf[:0]
	w.cur = 0
	w.mu.Unlock()

	w.writeString("\r\n")
	select {
	case w.lines <- line:
	default:
		// 没人取行（turn 正在跑且没有权限询问）：丢弃，不让 goroutine 卡死。
	}
}

// renderLocked 重画「提示符 + 输入」这一整块，把光标放回插入点。
// 调用方必须持有 w.mu。
//
// 为什么整行重画而不是就地 "\b \b"：
//   - CJK / emoji 占 2 列，删掉一个字符要擦 2 列，否则残留半个字形；
//   - 输入超过一屏宽时终端会折行，"退一格"越不过行首，只有知道提示符
//     宽度和行数才能把多出来的行一起擦掉；
//   - \x1b[J（擦到屏幕末尾）顺带清掉任何历史残留，输入行永远是最后一行。
func (w *keyWatcher) renderLocked() {
	pw := promptWidth(w.prompt)
	bw := displayWidth(string(w.buf))
	total := pw + bw

	endLine, endCol, rows := 0, total, 1
	if w.cols > 0 {
		endLine, endCol = total/w.cols, total%w.cols
		rows = endLine + 1
	}

	var b strings.Builder
	b.WriteString("\r")
	if rows > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", rows-1)
	}
	b.WriteString("\x1b[J")
	b.WriteString(w.prompt)
	b.Write(w.buf)

	// 光标回到插入点：先算插入点在第几行第几列。
	line, col := endLine, endCol
	cw := pw + displayWidth(string(w.buf[:w.cur]))
	if w.cols > 0 {
		line, col = cw/w.cols, cw%w.cols
	} else if w.cur < len(w.buf) {
		// 列数未知（拿不到终端宽度）：按「从行尾左移」估算，单行场景够用。
		col = endCol - displayWidth(string(w.buf[w.cur:]))
		if col < 0 {
			col = 0
		}
	}
	if d := endLine - line; d > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", d)
	}
	b.WriteString("\r")
	if col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", col)
	}
	w.writeString(b.String())
}

func (w *keyWatcher) writeString(s string) {
	if w.out == nil {
		return
	}
	if w.outMu != nil {
		w.outMu.Lock()
		defer w.outMu.Unlock()
	}
	_, _ = io.WriteString(w.out, s)
}

// ---------- 小工具 ----------

// runeStartBefore 返回 i 之前那个字符的起始字节下标。
func runeStartBefore(b []byte, i int) int {
	if i <= 0 {
		return 0
	}
	j := i - 1
	for j > 0 && b[j]&0xC0 == 0x80 { // UTF-8 续字节
		j--
	}
	return j
}

// runeStartAfter 返回 i 处字符之后的字节下标。
func runeStartAfter(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	j := i + 1
	for j < len(b) && b[j]&0xC0 == 0x80 {
		j++
	}
	return j
}

// promptWidth 返回提示符最后一行的显示宽度（跳过 ANSI 转义序列）。
//
// 只算最后一行：多行提示符（权限询问前面那个 \n）重画时只回退到它自己的行首。
func promptWidth(s string) int {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // CSI / OSC：跳到终止字节
			i++
			if i < len(s) && (s[i] == '[' || s[i] == 'O') {
				i++
				for i < len(s) && !(s[i] >= 0x40 && s[i] <= 0x7e) {
					i++
				}
				i++
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}
