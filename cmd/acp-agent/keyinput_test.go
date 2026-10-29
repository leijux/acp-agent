package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestKeys 用内存输入构造按键层。
func newTestKeys(input string) (*keyWatcher, *bytes.Buffer) {
	var out bytes.Buffer
	w := newKeyWatcher(strings.NewReader(input), &out, nil)
	go w.loop()
	return w, &out
}

// newTestKeysBefore 先构造（不启动 loop），让调用方先装好 cancelTurn 再开跑。
// 否则输入里的 Ctrl+C 可能先于 SetCancelTurn 被消费掉（测试自身的时序竞争）。
func newTestKeysBefore(input string) (*keyWatcher, *bytes.Buffer) {
	var out bytes.Buffer
	return newKeyWatcher(strings.NewReader(input), &out, nil), &out
}

func readLineTimeout(t *testing.T, w *keyWatcher) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return w.ReadLine(ctx)
}

// TestKeyWatcherLine 验证基本行输入：字符回显 + 回车提交。
func TestKeyWatcherLine(t *testing.T) {
	w, out := newTestKeys("hello\r")

	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q", line)
	}
	if got := out.String(); got != "hello\r\n" {
		t.Fatalf("回显 = %q", got)
	}
}

// TestKeyWatcherBackspace 验证退格按 UTF-8 边界删字符。
func TestKeyWatcherBackspace(t *testing.T) {
	w, _ := newTestKeys("ab\x7f中\x7f\r")

	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "a" {
		t.Fatalf("line = %q，期望 %q", line, "a")
	}
}

// TestKeyWatcherEscCancelsTurn 验证 Esc 在 turn 期间触发取消。
func TestKeyWatcherEscCancelsTurn(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newKeyWatcher(pr, io.Discard, nil)
	go w.loop()

	var cancelled atomic.Bool
	w.SetCancelTurn(func() { cancelled.Store(true) })

	if _, err := pw.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(cancelled.Load) {
		t.Fatal("Esc 没有触发 cancelTurn")
	}
}

// TestKeyWatcherEscWhileIdleClearsLine 验证空闲时 Esc 丢掉半截输入。
func TestKeyWatcherEscWhileIdleClearsLine(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newKeyWatcher(pr, io.Discard, nil)
	go w.loop()

	if _, err := pw.Write([]byte("abc\x1b")); err != nil {
		t.Fatal(err)
	}
	// 等过 drain 窗口（60ms），后续输入才是新的一行。
	time.Sleep(120 * time.Millisecond)
	if _, err := pw.Write([]byte("x\r")); err != nil {
		t.Fatal(err)
	}

	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "x" {
		t.Fatalf("line = %q，期望 %q（abc 应被 Esc 清掉）", line, "x")
	}
}

// TestKeyWatcherCtrlCQuits 验证 Ctrl+C 既取消 turn 也发出退出信号。
func TestKeyWatcherCtrlCQuits(t *testing.T) {
	// 先装 cancelTurn 再开跑：否则 loop 可能先把 \x03 吃掉（读到空的 cancelTurn），
	// 变成“只退出、不取消”的假失败。
	w, _ := newTestKeysBefore("\x03")
	var cancelled atomic.Bool
	w.SetCancelTurn(func() { cancelled.Store(true) })
	go w.loop()

	_, err := readLineTimeout(t, w)
	if !errors.Is(err, errQuitRequested) {
		t.Fatalf("期望 errQuitRequested，得到 %v", err)
	}
	if !waitFor(cancelled.Load) {
		t.Fatal("Ctrl+C 应同时取消当前 turn")
	}
}

// TestKeyWatcherEchoWrites 验证控制字符不会被回显成垃圾。
func TestKeyWatcherEchoWrites(t *testing.T) {
	w, out := newTestKeys("ok\r")
	if _, err := readLineTimeout(t, w); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("回显里不应有控制字符: %q", out.String())
	}
}

// TestKeyWatcherArrowDoesNotCancel 验证方向键（ESC [ C / ESC [ D）不会被当成
// Esc 打断正在跑的 turn，也不会把输入行清空。
func TestKeyWatcherArrowDoesNotCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newKeyWatcher(pr, io.Discard, nil)
	go w.loop()

	var cancelled atomic.Bool
	w.SetCancelTurn(func() { cancelled.Store(true) })

	if _, err := pw.Write([]byte("\x1b[D\x1b[C\x1b[A\x1b[B")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("方向键不应该触发 cancelTurn")
	}
}

// TestKeyWatcherArrowKeys 验证 ←/→ 移动插入点后插入/删除的位置正确。
func TestKeyWatcherArrowKeys(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// →← 无历史也不能当 Esc：整行保留
		{"方向键不丢字", "ac\x1b[Db\r", "abc"},
		{"左移后插字符", "ab\x1b[D\x1b[Dc\r", "cab"},
		{"左移后退格", "abc\x1b[D\x7f\r", "ac"},
		{"Home 后插字符", "bc\x1b[Ha\r", "abc"},
		{"Home 键（ESC O H）", "bc\x1bOHa\r", "abc"},
		{"End 后追加", "ab\x1b[Hc\x1b[Fd\r", "cabd"},
		{"Delete 删当前字符", "abc\x1b[D\x1b[D\x1b[3~\r", "ac"},
		{"Ctrl-A / Ctrl-E", "bc\x01a\x05d\r", "abcd"},
		{"Ctrl-U 删到行首", "abcd\x15x\r", "x"},
		{"Ctrl-K 删到行尾", "abcd\x1b[D\x0b\r", "abc"},
		{"上下列不产生垃圾", "a\x1b[Ab\x1b[Bc\r", "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, _ := newTestKeys(c.input)
			line, err := readLineTimeout(t, w)
			if err != nil {
				t.Fatalf("ReadLine: %v", err)
			}
			if line != c.want {
				t.Fatalf("line = %q，期望 %q", line, c.want)
			}
		})
	}
}

// TestKeyWatcherBackspaceErasesCleanly 验证退格走「整行重画 + 擦到屏幕末尾」，
// 而不是固定写 "\b \b"。后者删 CJK 字符（占 2 列）只擦掉半格，留下残缺字形。
func TestKeyWatcherBackspaceErasesCleanly(t *testing.T) {
	w, out := newTestKeys("中\x7f\r")
	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "" {
		t.Fatalf("line = %q，期望空", line)
	}
	got := out.String()
	if strings.Contains(got, "\b \b") {
		t.Fatalf("不应再用 \\b 空格 \\b（宽字符擦不干净）: %q", got)
	}
	if !strings.Contains(got, "\x1b[J") {
		t.Fatalf("退格应擦到屏幕末尾: %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "\r\n"), "中") {
		t.Fatalf("被删字符不应留在屏上: %q", got)
	}
}

// TestKeyWatcherRedrawKeepsPrompt 验证重画时提示符会补回来（含 ANSI 颜色）。
func TestKeyWatcherRedrawKeepsPrompt(t *testing.T) {
	w, out := newTestKeys("ab\x1b[D\r")
	w.SetPrompt("\x1b[1;34m›\x1b[0m ")

	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "ab" {
		t.Fatalf("line = %q", line)
	}
	if n := strings.Count(out.String(), "›"); n < 2 {
		t.Fatalf("重画后提示符应重新出现（至少 2 次），实际 %d: %q", n, out.String())
	}
}

// TestPromptWidth 验证提示符宽度按可见列算（ANSI 不算、多行只看最后一行）。
func TestPromptWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"› ", 2},
		{"\x1b[1;34m›\x1b[0m ", 2},
		{"\n允许执行 bash？ ", 16},
		{"中文 › ", 7},
	}
	for _, c := range cases {
		if got := promptWidth(c.in); got != c.want {
			t.Errorf("promptWidth(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestKeyWatcherSplitSequenceDropped 验证序列被拆成两次送达（ESC / "[D"）时，
// 尾巴不会漏进输入行。
func TestKeyWatcherSplitSequenceDropped(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newKeyWatcher(pr, io.Discard, nil)
	go w.loop()

	if _, err := pw.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := pw.Write([]byte("[D\r")); err != nil {
		t.Fatal(err)
	}

	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "" {
		t.Fatalf("line = %q，期望空（[D 是拆开的序列尾巴）", line)
	}
}

// TestKeyWatcherRenderHandlesWrap 验证输入折行后重画会先回退到输入块的首行
// 再擦除（否则会在屏幕中间留下一串残缺字符）。
func TestKeyWatcherRenderHandlesWrap(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	w := newKeyWatcher(pr, &out, nil)
	w.SetPrompt("› ") // 2 列
	w.SetCols(5)      // 2+8=10 列 → 占 2 行
	go w.loop()

	if _, err := pw.Write([]byte("abcdefgh\x7f\r")); err != nil {
		t.Fatal(err)
	}
	line, err := readLineTimeout(t, w)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "abcdefg" {
		t.Fatalf("line = %q", line)
	}
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Fatalf("折行后重画应先上移一行再擦: %q", out.String())
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// 输入流在最后一行之后立刻 EOF（管道 / 重定向）：ReadLine 必须把行交出去，
// 不能因为 eof 与 lines 同时就绪就随机挑 eof（那会丢掉用户最后一条输入）。
func TestReadLineKeepsLastLineBeforeEOF(t *testing.T) {
	for i := 0; i < 200; i++ {
		w, _ := newTestKeys("最后一行\r")
		line, err := readLineTimeout(t, w)
		if err != nil {
			t.Fatalf("第 %d 次：ReadLine 返回 %v（最后一行被 EOF 抢走了）", i, err)
		}
		if line != "最后一行" {
			t.Fatalf("第 %d 次：line = %q", i, line)
		}
	}
}

// TestKeyWatcherHistory 行模式的 ↑/↓ 取输入历史。
//
// 序列：输入 "first\r" 提交 → 输入 "second\r" 提交 → 输入 "thi" 再按 ↑
// （前缀无匹配，应放宽成全量取到 "second"）→ ↓ 回到草稿 "thi"。
func TestKeyWatcherHistory(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newKeyWatcher(pr, io.Discard, nil)
	go w.loop()

	write := func(s string) {
		t.Helper()
		if _, err := pw.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}

	write("first\r")
	if line, err := readLineTimeout(t, w); err != nil || line != "first" {
		t.Fatalf("第一条 = %q, %v", line, err)
	}
	write("second\r")
	if line, err := readLineTimeout(t, w); err != nil || line != "second" {
		t.Fatalf("第二条 = %q, %v", line, err)
	}
	if w.hist.Len() != 2 {
		t.Fatalf("历史应有 2 条，实际 %d", w.hist.Len())
	}

	// ↑ 取上一条（前缀 "thi" 无匹配 → 放宽成全量）
	write("\x1b[A")
	waitForState(t, func() bool { return w.inHistory() && w.lineText() == "second" })

	// 再 ↑ 到 "first"
	write("\x1b[A")
	waitForState(t, func() bool { return w.lineText() == "first" })

	// ↓ 回 "second"，再 ↓ 回草稿
	write("\x1b[B")
	waitForState(t, func() bool { return w.lineText() == "second" })
	write("\x1b[B")
	waitForState(t, func() bool { return w.lineText() == "" })
}

// inHistory 报告是否在历史导航中（测试用）。
func (w *keyWatcher) inHistory() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.histPos >= 0
}

// lineText 返回当前输入行（测试用）。
func (w *keyWatcher) lineText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// waitForState 等一个按键处理后的状态成立。
func waitForState(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待输入行状态超时")
}
