//go:build !shellnative

package tools

import (
	"runtime"
	"strings"
	"testing"
)

// Windows 的进程退出码是 32 位，256 的倍数（256/512/0xC0000000…）都合法。
// mvdan/sh 的 DefaultExecHandler 用 ExitStatus(err.ExitCode()) 把它截成 uint8，
// 于是变成 interp.ExitStatus(0) —— 一个「非 nil 但码为 0」的错误。
// Runner.Run 收尾时据此 panic（api.go:1030
// "ended up with a non-nil exitStatus.err but a zero exitStatus.code"），
// 而这个 panic 发生在解释器 goroutine 里，会带走整个进程。
func TestBashExitCodeTruncatesToZero(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只有 Windows 会有低 8 位为 0 的非零退出码")
	}
	bt, cleanup := newBash(t)
	defer cleanup()

	res, err := bt.Execute(ctx_(), Args{"command": `cmd /c exit 256`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 原码已被上游截断，只能保住「失败」这一个事实：不能当成功。
	details, _ := res.Details.(map[string]any)
	if code, _ := details["exitCode"].(int); code == 0 {
		t.Fatalf("退出码 256 被当成成功了: %+v", res.Details)
	}
	// 不能再出现「shell 内部错误」——那说明刚才 panic 了（本用例存在时会被 recover
	// 兜住，但没有 recover 的旧版本会直接带走整个进程）。
	if s := res.Text(); strings.Contains(s, "内部错误") {
		t.Fatalf("退出码 256 触发了内部 panic: %s", s)
	}
}

func TestExitStatusZeroIsNil(t *testing.T) {
	// interp.ExitStatus(0) 是一个非 nil 的 error，等价于「失败但码为 0」，
	// 会让 mvdan/sh 的 Runner.Run panic。helper 必须把 0 收敛成 nil。
	if err := exitStatus(0); err != nil {
		t.Fatalf("exitStatus(0) = %v（%T），必须是 nil", err, err)
	}
	for _, code := range []int{-1, 256, 257, 999} {
		err := exitStatus(code)
		if err == nil {
			t.Fatalf("exitStatus(%d) = nil，越界码必须收敛成 1", code)
		}
	}
	if err := exitStatus(3); err == nil {
		t.Fatal("exitStatus(3) 不该是 nil")
	}
}
