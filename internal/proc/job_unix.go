//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// setup 让子进程成为独立进程组，便于整组杀掉。
func setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree 杀掉整个进程组（含 shell 派生的子进程）。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// 负 pid 表示进程组。
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// alive 用 signal 0 探测进程是否存活：进程在则成功，不存在则 ESRCH。
func alive(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}

// Job 在 Unix 上没有额外能力：Setpgid 已经足够，NewJob 返回 nil。
type Job struct{}

func newJob() *Job { return nil }

func (*Job) Assign(*os.Process) error { return nil }
func (*Job) Terminate()               {}
func (*Job) Close()                   {}
