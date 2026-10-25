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
	w, _ := newTestKeys("\x03")

	var cancelled atomic.Bool
	w.SetCancelTurn(func() { cancelled.Store(true) })

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
