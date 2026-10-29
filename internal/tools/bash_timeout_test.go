package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBashTimeoutDoesNotReexecute 回归：前台超时不能把命令重跑一遍。
//
// 旧实现超时后调 Manager.Start(cmd...) 重新执行整条命令，`>>` 追加、
// git commit、npm install 这类非幂等操作会被执行两次。
func TestBashTimeoutDoesNotReexecute(t *testing.T) {
	p := testPolicy(t)
	bt := NewBashTool(p)
	marker := filepath.Join(p.Root, "marker.txt")
	// 追加一行后长睡，确保超时发生时第一行已经写盘。
	// 路径必须用正斜杠：内置解释器走 bash 语义，反斜杠会被当成转义符吃掉。
	cmd := "echo hit >> " + filepath.ToSlash(marker) + " && " + sleepCmd(1500)

	res, err := bt.Execute(ctx_(), Args{"command": cmd, "timeout": 200})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("超时不应算错误: %s", res.Text())
	}
	d, _ := res.Details.(map[string]any)
	id, _ := d["sessionId"].(string)
	if id == "" {
		t.Fatalf("应接管为后台会话: %s", res.Text())
	}

	// 等后台进程结束，再确认只追加了一次。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := bt.Manager.Read(ctx_(), id, 0, "", WaitNone, 0); err == nil && res.Done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("读标记文件: %v", err)
	}
	if n := strings.Count(string(b), "hit"); n != 1 {
		t.Fatalf("命令被执行了 %d 次（应为 1 次）: %q", n, string(b))
	}
}

// TestBashTimeoutKeepsPartialOutput 接管时不能丢掉前台已经产生的输出。
func TestBashTimeoutKeepsPartialOutput(t *testing.T) {
	bt, _ := newBash(t)
	res, err := bt.Execute(ctx_(), Args{
		"command": "echo before-sleep && " + sleepCmd(1200),
		"timeout": 300,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 被接管的进程会一直在后台跑：必须收尾，否则 TempDir 删不掉（Windows 下
	// 进程还持着 cwd）。
	if d, ok := res.Details.(map[string]any); ok {
		if id, _ := d["sessionId"].(string); id != "" {
			defer killAndWait(t, bt.Manager, id)
		}
	}
	if !strings.Contains(res.Text(), "before-sleep") {
		t.Fatalf("丢失了超时前的输出: %q", res.Text())
	}
}

// killAndWait 终止后台会话并等到 done，避免残留进程干扰后续测试与 TempDir 清理。
func killAndWait(t *testing.T, m *BashManager, id string) {
	t.Helper()
	_ = m.Kill(id)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := m.Read(ctx_(), id, 0, "", WaitNone, 0); err != nil || res.Done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("警告: 后台会话 %s 未被回收", id)
}

// TestBashManagerAttachDrainsOutput 接管后的会话仍能读到后续输出。
func TestBashManagerAttachDrainsOutput(t *testing.T) {
	bt, _ := newBash(t)
	res, err := bt.Execute(ctx_(), Args{
		"command": "echo first && " + sleepCmd(800) + " && echo second",
		"timeout": 200,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	d, _ := res.Details.(map[string]any)
	id, _ := d["sessionId"].(string)
	if id == "" {
		t.Fatalf("缺少 sessionId: %s", res.Text())
	}

	out := NewBashOutputTool(bt.Manager)
	deadline := time.Now().Add(10 * time.Second)
	var last Result
	for time.Now().Before(deadline) {
		last, _ = out.Execute(ctx_(), Args{"sessionId": id})
		if strings.Contains(last.Text(), "done(") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	text := last.Text()
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Fatalf("接管后输出不完整: %q", text)
	}
}

// TestBashCancelDoesNotHang 回归：ctx 取消后不能永久阻塞。
func TestBashCancelDoesNotHang(t *testing.T) {
	bt, _ := newBash(t)
	ctx, cancel := context.WithCancel(ctx_())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = bt.Execute(ctx, Args{"command": sleepCmd(5000), "timeout": 30000})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 bash 工具没返回（Kill 后等待无上限）")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("取消耗时过长: %s", el)
	}
}

// TestSharedOutputLimit sharedOutput 超限后仍要不阻塞写端。
func TestSharedOutputLimit(t *testing.T) {
	o := newSharedOutput(8)
	n, err := o.Write([]byte("1234567890"))
	if err != nil || n != 10 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if _, err := o.Write([]byte("abc")); err != nil {
		t.Fatalf("超限后写入不应报错: %v", err)
	}
	s := o.String()
	if !strings.Contains(s, "12345678") {
		t.Fatalf("应保留前 8 字节: %q", s)
	}
	if !strings.Contains(s, "已截断") {
		t.Fatalf("截断必须可见（旧实现静默丢弃）: %q", s)
	}
}
