package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// REPL 的按键层：在 cbreak 模式下独占 stdin，把字节流变成「行」与「中断」。
//
// 为什么需要它：行模式（cooked）下终端自己攒行，Esc 永远不会被进程看到 ——
// 用户按 Esc 只是往行缓冲里塞了一个不可见字节，要等回车才送达。想中途打断
// 一个正在跑的 turn，就必须关掉行缓冲。
//
// 由于关掉行缓冲后终端不再回显，这里要自己做最小行编辑（字符回显、退格、
// 回车提交）。不做历史、方向键、Ctrl-A 这类 readline 能力：REPL 的输入只是
// 一行命令或一个 prompt，够用。
//
// 单一读者是关键：整个 REPL 生命周期只由这个 watcher 读 os.Stdin，
// 权限询问（guard.ask）也从这里取行，避免两个 reader 抢字节。

// errQuitRequested 表示用户按了 Ctrl+C / Ctrl+D，要求退出 REPL。
var errQuitRequested = errors.New("用户请求退出")

type keyWatcher struct {
	reader *bufio.Reader
	out    io.Writer

	lines chan string
	quit  chan struct{}
	eof   chan struct{}

	// cancelTurn 非空时，Esc 取消当前 turn（清空行）；为空表示空闲。
	cancelTurn atomic.Value // context.CancelFunc

	mu         sync.Mutex
	buf        []byte
	drainUntil time.Time
	closed     bool

	stopOnce sync.Once
	restore  func()
}

// startKeyWatcher 把终端切到 cbreak 并在后台读键。
//
// fd 不是终端（管道、重定向）时返回错误，调用方应退回行模式读取。
func startKeyWatcher(fd int, out io.Writer) (*keyWatcher, error) {
	restore, err := enterCbreak(fd)
	if err != nil {
		return nil, err
	}
	w := newKeyWatcher(os.Stdin, out, restore)
	go w.loop()
	return w, nil
}

// newKeyWatcher 构造按键层（不切终端模式）；测试直接注入 reader。
func newKeyWatcher(r io.Reader, out io.Writer, restore func()) *keyWatcher {
	return &keyWatcher{
		reader:  bufio.NewReader(r),
		out:     out,
		lines:   make(chan string, 8),
		quit:    make(chan struct{}),
		eof:     make(chan struct{}),
		restore: restore,
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

// ReadLine 读取一行（不含换行）。ctx 取消或用户要求退出时返回错误。
func (w *keyWatcher) ReadLine(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.quit:
		return "", errQuitRequested
	case <-w.eof:
		return "", io.EOF
	case line := <-w.lines:
		return line, nil
	}
}

// Close 恢复终端模式。可重复调用。
func (w *keyWatcher) Close() {
	w.stopOnce.Do(func() {
		if w.restore != nil {
			w.restore()
		}
	})
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

		w.mu.Lock()
		closed := w.closed
		draining := time.Now().Before(w.drainUntil)
		w.mu.Unlock()
		if closed {
			return
		}
		if draining {
			// Esc 之后的一小段时间里，把方向键序列等残留字节吞掉，
			// 免得它们混进下一次行输入（`ESC [ A` 会变成 "[A"）。
			continue
		}

		switch b {
		case 0x1b: // Esc
			w.onEsc()
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
		default:
			if b >= 0x20 {
				w.appendByte(b)
			}
		}
	}
}

// onEsc 打断当前 turn；空闲时清空当前行。
func (w *keyWatcher) onEsc() {
	w.mu.Lock()
	w.drainUntil = time.Now().Add(60 * time.Millisecond)
	w.mu.Unlock()

	cancel, _ := w.cancelTurn.Load().(context.CancelFunc)
	if cancel != nil {
		cancel()
		return
	}
	// 空闲：丢掉半截输入并换行，回到提示符。
	w.mu.Lock()
	n := len(w.buf)
	w.buf = w.buf[:0]
	w.mu.Unlock()
	if n > 0 {
		w.writeString("\r\n")
	}
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

func (w *keyWatcher) appendByte(b byte) {
	w.mu.Lock()
	w.buf = append(w.buf, b)
	w.mu.Unlock()
	_, _ = w.out.Write([]byte{b})
}

func (w *keyWatcher) backspace() {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return
	}
	// 按 UTF-8 边界退一个字符，避免把中文退成半个字节。
	i := len(w.buf) - 1
	for i > 0 && w.buf[i]&0xC0 == 0x80 {
		i--
	}
	w.buf = w.buf[:i]
	w.mu.Unlock()
	w.writeString("\b \b")
}

func (w *keyWatcher) submit() {
	w.mu.Lock()
	line := string(w.buf)
	w.buf = w.buf[:0]
	w.mu.Unlock()
	w.writeString("\r\n")
	select {
	case w.lines <- line:
	default:
		// 没人取行（turn 正在跑且没有权限询问）：丢弃，不让 goroutine 卡死。
	}
}

func (w *keyWatcher) writeString(s string) {
	if w.out != nil {
		_, _ = io.WriteString(w.out, s)
	}
}
