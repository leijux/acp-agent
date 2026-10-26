package tools

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sleepCmd 返回一个跨平台的「睡 ms 毫秒」命令。
func sleepCmd(ms int) string {
	if runtime.GOOS != "windows" {
		return "sleep " + fmtSeconds(ms)
	}
	// 默认引擎是 mvdan/sh 内置解释器：sleep 不是内建命令，必须真的存在于 PATH。
	// 只看 bash 是否存在会误判（Windows 自带 WSL 的 bash.exe 在 PATH 里，sleep 却不在）。
	if _, err := exec.LookPath("sleep"); err == nil {
		return "sleep " + fmtSeconds(ms)
	}
	// 没有 sleep 时用 ping 近似（-n 次数受网络栈影响，粗粒度够用）
	n := ms/1000 + 2
	return "ping -n " + itoa(n) + " 127.0.0.1 > NUL"
}

func fmtSeconds(ms int) string {
	return itoa(ms/1000) + "." + itoa(ms%1000/100) + "0"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func newBash(t *testing.T) (*BashTool, func()) {
	t.Helper()
	p := testPolicy(t)
	bt := NewBashTool(p)
	return bt, func() {}
}

func TestBashEcho(t *testing.T) {
	bt, cleanup := newBash(t)
	defer cleanup()
	res, err := bt.Execute(ctx_(), Args{"command": "echo hello-world"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("命令失败: %s", res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "hello-world") {
		t.Fatalf("缺少输出: %q", text)
	}
	if !strings.Contains(text, "[exit 0") {
		t.Fatalf("缺少退出码标注: %q", text)
	}
}

func TestBashExitCode(t *testing.T) {
	bt, _ := newBash(t)
	res, err := bt.Execute(ctx_(), Args{"command": "exit 3"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "[exit 3") {
		t.Fatalf("退出码未捕获: %q", res.Text())
	}
	if d, ok := res.Details.(map[string]any); ok {
		if code, ok := d["exitCode"].(int); ok && code != 3 {
			t.Fatalf("details exitCode = %d, want 3", code)
		}
	}
}

func TestBashRejectsDangerous(t *testing.T) {
	bt, _ := newBash(t)
	res, _ := bt.Execute(ctx_(), Args{"command": "rm -rf /"})
	if !res.IsError {
		t.Fatal("危险命令应被拒绝")
	}
}

func TestBashWorkingDir(t *testing.T) {
	p := testPolicy(t)
	bt := NewBashTool(p)
	res, err := bt.Execute(ctx_(), Args{"command": "pwd", "cwd": "."})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		// Windows cmd 没有 pwd，跳过
		t.Skipf("跳过：%s", res.Text())
	}
}

// TestBashTimeoutGoesBackground 前台超时后转后台，输出可在 bash_output 取回。
func TestBashTimeoutGoesBackground(t *testing.T) {
	bt, _ := newBash(t)
	cmd := "echo start && " + sleepCmd(1500) + " && echo end"
	res, err := bt.Execute(ctx_(), Args{"command": cmd, "timeout": 300})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("超时不应算错误: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "已接管为后台") {
		t.Fatalf("未接管为后台: %q", res.Text())
	}
	d, _ := res.Details.(map[string]any)
	id, _ := d["sessionId"].(string)
	if id == "" {
		t.Fatal("缺少 sessionId")
	}
	out := NewBashOutputTool(bt.Manager)
	// 轮询直到结束
	deadline := time.Now().Add(10 * time.Second)
	var last Result
	for time.Now().Before(deadline) {
		last, _ = out.Execute(ctx_(), Args{"sessionId": id})
		if strings.Contains(last.Text(), "done(exit 0)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	text := last.Text()
	if !strings.Contains(text, "start") {
		t.Fatalf("后台输出缺失前段: %q", text)
	}
	if !strings.Contains(text, "end") {
		t.Fatalf("后台输出缺失后段: %q", text)
	}
	if !strings.Contains(text, "done(exit 0)") {
		t.Fatalf("未观察到进程结束: %q", text)
	}
}

func TestBashBackgroundAndKill(t *testing.T) {
	bt, _ := newBash(t)
	res, err := bt.Execute(ctx_(), Args{"command": "echo bg && " + sleepCmd(5000), "background": true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	d, _ := res.Details.(map[string]any)
	id, _ := d["sessionId"].(string)
	if id == "" {
		t.Fatalf("缺少 sessionId: %s", res.Text())
	}
	time.Sleep(400 * time.Millisecond)
	out, _ := NewBashOutputTool(bt.Manager).Execute(ctx_(), Args{"sessionId": id})
	if !strings.Contains(out.Text(), "bg") {
		t.Fatalf("未取到后台输出: %q", out.Text())
	}
	kill, err := NewBashKillTool(bt.Manager).Execute(ctx_(), Args{"sessionId": id})
	if err != nil || kill.IsError {
		t.Fatalf("kill 失败: %v %s", err, kill.Text())
	}
	// 杀掉后应最终变为 done
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ = NewBashOutputTool(bt.Manager).Execute(ctx_(), Args{"sessionId": id})
		if strings.Contains(out.Text(), "done(") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("kill 后进程未结束: %q", out.Text())
}

func TestBashOutputFilter(t *testing.T) {
	bt, _ := newBash(t)
	res, _ := bt.Execute(ctx_(), Args{
		"command":    "echo keep-me && echo drop-me && echo keep-also",
		"background": false,
	})
	if !strings.Contains(res.Text(), "keep-me") {
		t.Fatalf("前台输出异常: %q", res.Text())
	}
	// 后台 + filter
	bg, _ := bt.Execute(ctx_(), Args{"command": "echo keep-me && echo drop-me", "background": true})
	d, _ := bg.Details.(map[string]any)
	id, _ := d["sessionId"].(string)
	time.Sleep(400 * time.Millisecond)
	out, _ := NewBashOutputTool(bt.Manager).Execute(ctx_(), Args{"sessionId": id, "filter": "keep"})
	text := out.Text()
	if !strings.Contains(text, "keep-me") || strings.Contains(text, "drop-me") {
		t.Fatalf("filter 未生效: %q", text)
	}
}

// TestBashConcurrent 多个只读 bash 调用并发执行（Executor 层面验证）。
func TestBashConcurrent(t *testing.T) {
	p := testPolicy(t)
	r, _ := DefaultRegistry(Options{Policy: p, MaxParallel: 8})
	ex := NewExecutor(r, 8)
	sleep := sleepCmd(400)

	var calls []Call
	for i := 0; i < 4; i++ {
		calls = append(calls, Call{
			ID:   itoa(i),
			Name: "bash",
			Args: map[string]any{"command": "echo job" + itoa(i) + " && " + sleep},
		})
	}
	start := time.Now()
	results := ex.Execute(context.Background(), calls)
	elapsed := time.Since(start)

	for i, res := range results {
		if res.Err != nil || res.Result.IsError {
			t.Fatalf("job%d 失败: %v %s", i, res.Err, res.Result.Text())
		}
		if !strings.Contains(res.Result.Text(), "job"+itoa(i)) {
			t.Fatalf("job%d 输出错误: %q", i, res.Result.Text())
		}
	}

	// 串行基线现场测，不用写死的绝对阈值：早先写死 1.3s，而 Windows 上
	// 一次 sleep 400ms 的命令实际耗时 1.25~1.30s（进程启动 + 内置 shell），
	// 整套测试并行跑时很容易越过阈值 —— 报「未并发」但代码没问题。
	// MaxParallel=1 跑同一批调用，把机器负载/进程启动开销都吸收进基线。
	serialStart := time.Now()
	if res := NewExecutor(r, 1).Execute(context.Background(), calls); len(res) != len(calls) {
		t.Fatalf("串行基线返回 %d 个结果", len(res))
	}
	serial := time.Since(serialStart)

	if elapsed > serial*3/5 {
		t.Fatalf("4 个 bash 调用耗时 %v，串行基线 %v，未并发", elapsed, serial)
	}
	t.Logf("4 个 bash 并发耗时 %v（串行基线 %v）", elapsed, serial)
}

func TestBashUnknownSession(t *testing.T) {
	bt, _ := newBash(t)
	res, _ := NewBashOutputTool(bt.Manager).Execute(ctx_(), Args{"sessionId": "nope"})
	if !res.IsError {
		t.Fatal("未知 session 应报错")
	}
}
