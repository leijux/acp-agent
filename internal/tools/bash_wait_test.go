package tools

// bash_output 的增量读（cursor）与等待（wait）语义。
//
// 这两件事一起解决「模型死等」：以前只能反复轮询拿全量快照，一次两分钟的
// 任务要耗掉好几轮模型往返和重复输出。

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func startBg(t *testing.T, m *BashManager, cmd string) string {
	t.Helper()
	dir := t.TempDir()
	id, err := m.Start(cmd, dir, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}

// cursor 只拿新增内容，不重复发整块。
func TestBashReadCursorIncremental(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "printf 'a\\nb\\nc\\n'")

	res, err := m.Read(ctx_(), id, 0, "", WaitExit, 5*time.Second)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Done {
		t.Fatalf("应已结束: %+v", res)
	}
	if got := strings.TrimSpace(res.Text); got != "a\nb\nc" {
		t.Fatalf("首次读取 = %q", got)
	}
	if res.Next == 0 {
		t.Fatal("Next 应指向末尾")
	}

	// 用 cursor 再读：没有新内容
	again, err := m.Read(ctx_(), id, res.Next, "", WaitNone, 0)
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if again.Text != "" {
		t.Fatalf("cursor 之后不该有新内容，实际 %q", again.Text)
	}
	if again.Next != res.Next {
		t.Fatalf("Next 不该变：%d → %d", res.Next, again.Next)
	}

	// cursor=0 仍能拿全量（向后兼容）
	all, _ := m.Read(ctx_(), id, 0, "", WaitNone, 0)
	if !strings.Contains(all.Text, "a") || !strings.Contains(all.Text, "c") {
		t.Fatalf("cursor=0 应返回全量，实际 %q", all.Text)
	}

	// filter 与 cursor 组合
	f, _ := m.Read(ctx_(), id, 0, "^b$", WaitNone, 0)
	if got := strings.TrimSpace(f.Text); got != "b" {
		t.Fatalf("filter 结果 = %q", got)
	}
}

// wait=exit 会阻塞到命令结束，而不是立刻返回 running。
func TestBashReadWaitExit(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "sleep 0.3 && echo done-marker")

	start := time.Now()
	res, err := m.Read(ctx_(), id, 0, "", WaitExit, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Done {
		t.Fatalf("wait=exit 应等到结束: %+v", res)
	}
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0", res.Exit)
	}
	if !strings.Contains(res.Text, "done-marker") {
		t.Fatalf("缺少输出: %q", res.Text)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("只等了 %v，没有真的阻塞到命令结束", elapsed)
	}
}

// wait=output 等到有新输出就返回（不必等进程结束）。
func TestBashReadWaitOutput(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	// 先输出一行，再睡一会儿才结束：wait=output 应该在第一行出来时就返回。
	id := startBg(t, m, "echo early && sleep 1")

	start := time.Now()
	res, err := m.Read(ctx_(), id, 0, "", WaitOutput, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Text == "" {
		t.Fatal("wait=output 应拿到新输出")
	}
	if res.Done {
		t.Log("命令恰好已结束（时序），不影响本用例结论")
	}
	if elapsed > 800*time.Millisecond {
		t.Fatalf("等了 %v 才返回，wait=output 不该等进程结束", elapsed)
	}
}

// 等待超时要如实返回（进程还在跑），而不是伪装成结束。
func TestBashReadWaitTimeout(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "sleep 5")

	start := time.Now()
	res, err := m.Read(ctx_(), id, 0, "", WaitExit, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("应标记 TimedOut: %+v", res)
	}
	if res.Done {
		t.Fatal("超时时不该报 done")
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("超时时长异常: %v", elapsed)
	}
	_ = m.Kill(id)
}

// ctx 取消必须立刻打断等待（用户 Esc / turn 中断）。
func TestBashReadWaitCanceled(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "sleep 5")
	defer m.Kill(id)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := m.Read(ctx, id, 0, "", WaitExit, 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Canceled {
		t.Fatalf("应标记 Canceled: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx 取消后 %v 才返回", elapsed)
	}
}

// 同一会话只允许一个等待者：第二个立刻拿快照返回，不跟着一起挂。
func TestBashReadOnlyOneWaiter(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "sleep 1")

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = m.Read(context.Background(), id, 0, "", WaitExit, 5*time.Second)
	}()
	// 等第一个真正进入等待（waiting 置位）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.sessions[id].isWaiting() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	res, err := m.Read(context.Background(), id, 0, "", WaitExit, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("第二个 Read: %v", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("第二个等待者不该阻塞，实际等了 %v", elapsed)
	}
	if res.Done {
		t.Fatal("命令还在跑，第二个读取不该报 done")
	}
	<-firstDone
}

// isWaiting 是测试用的探针。
func (s *bashSession) isWaiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiting
}

// 进程结束后 kill 仍可调用；kill 也要唤醒等待者。
func TestBashKillWakesWaiter(t *testing.T) {
	m := NewBashManager(DefaultPolicy())
	id := startBg(t, m, "sleep 5")

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = m.Kill(id)
	}()

	start := time.Now()
	res, err := m.Read(ctx_(), id, 0, "", WaitExit, 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Done {
		t.Fatalf("kill 后应唤醒并报 done: %+v", res)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("kill 后 %v 才醒", elapsed)
	}
}

// 前台超时被接管的会话（src=sharedOutput）也要能被等待唤醒。
func TestBashAttachedSessionWait(t *testing.T) {
	bt, _ := newBash(t)
	// 前台跑一条会超时的命令，超时后接管为后台会话
	res, err := bt.Execute(ctx_(), Args{"command": "sleep 1; echo attached-done", "timeout": 200})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := res.Text()
	if !strings.Contains(text, "sessionId=") {
		t.Fatalf("未接管为后台: %s", text)
	}
	id := parseSessionID(text)
	if id == "" {
		t.Fatalf("解析不到 sessionId: %s", text)
	}
	wait, err := bt.Manager.Read(ctx_(), id, 0, "", WaitExit, 10*time.Second)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !wait.Done || !strings.Contains(wait.Text, "attached-done") {
		t.Fatalf("接管会话的等待结果不对: done=%v text=%q", wait.Done, wait.Text)
	}
}

// parseSessionID 从工具输出里抠出 sessionId（文本里可能紧跟中文标点）。
func parseSessionID(text string) string {
	re := regexp.MustCompile("sessionId=([A-Za-z0-9-]+)")
	if m := re.FindStringSubmatch(text); len(m) == 2 {
		return m[1]
	}
	return ""
}
