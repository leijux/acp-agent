package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// 「运行中」右侧要带计时：1s 这种。
func TestRunningShowsElapsed(t *testing.T) {
	m, _ := newTestModel(120, 20)
	send(m, RunningMsg{On: true})

	m.turnStart = time.Now().Add(-3 * time.Second)
	if got := m.statusView(); !strings.Contains(got, "运行中 3s") {
		t.Fatalf("状态行 = %q, 期望含「运行中 3s」", got)
	}

	// turn 结束：计时消失，别把耗时留在状态行上。
	send(m, RunningMsg{On: false})
	if got := m.statusView(); strings.Contains(got, "运行中") {
		t.Fatalf("结束后状态行 = %q, 不该再有「运行中」", got)
	}
}

// 计时靠自己的心跳推进：界面只在收到消息时重绘，没心跳就停在 0s。
func TestRunningTicksDriveRefresh(t *testing.T) {
	m, _ := newTestModel(120, 20)
	_, cmd := m.Update(RunningMsg{On: true})
	if cmd == nil {
		t.Fatal("开跑时应该安排一次心跳")
	}

	// 心跳会继续排队；turn 结束后必须停下（否则界面每秒空转重绘）。
	_, cmd = m.Update(tickMsg{seq: m.turnSeq})
	if cmd == nil {
		t.Fatal("运行中收到心跳后应继续安排下一次")
	}
	send(m, RunningMsg{On: false})
	if _, cmd = m.Update(tickMsg{seq: m.turnSeq}); cmd != nil {
		t.Fatal("结束后心跳应停止")
	}

	// 上一轮遗留的心跳（seq 过期）不能复活计时。
	m2, _ := newTestModel(120, 20)
	send(m2, RunningMsg{On: true})
	send(m2, RunningMsg{On: false})
	send(m2, RunningMsg{On: true})
	if _, cmd = m2.Update(tickMsg{seq: m2.turnSeq - 1}); cmd != nil {
		t.Fatal("过期 seq 的心跳应被丢弃")
	}
}

func TestFmtElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                     "0s",
		3 * time.Second:                       "3s",
		59*time.Second + 900*time.Millisecond: "59s",
		time.Minute + 5*time.Second:           "1m05s",
		2*time.Hour + 3*time.Minute:           "2h03m",
		-time.Second:                          "0s",
	}
	for in, want := range cases {
		if got := fmtElapsed(in); got != want {
			t.Errorf("fmtElapsed(%v) = %q, 期望 %q", in, got, want)
		}
	}
}

// 计时链只能有一条：RunningMsg 重复到达时 seq 递增，旧链自然失效。
func TestRunningSeqAdvances(t *testing.T) {
	m, _ := newTestModel(120, 20)
	send(m, RunningMsg{On: true})
	first := m.turnSeq
	send(m, RunningMsg{On: true})
	if m.turnSeq == first {
		t.Fatal("重复开跑应推进 seq，避免两条计时链并发")
	}
	var _ tea.Msg = tickMsg{seq: first}
}
