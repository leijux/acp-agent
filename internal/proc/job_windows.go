//go:build windows

package proc

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// setup 在 Windows 上不需要额外属性；进程树靠 Job Object 管理。
func setup(cmd *exec.Cmd) {}

// killTree 在 Windows 上用 taskkill /T 杀整棵进程树。
//
// 注意：taskkill 依赖 ParentProcessId 链，实测杀不掉 Git Bash 再 fork 出来的
// coreutils 子进程（`bash -c "sleep 5"` 里的 sleep.exe 会活到自然结束）。
// 需要可靠时用 Job（NewJob + Assign + Terminate）。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}

// alive 判断进程是否还活着。
//
// Windows 上 os.Process.Signal(syscall.Signal(0)) 一律返回 "not supported"，
// 直接看它会把「活着的进程」判成死的（早先的实现就是这个 bug，注释写着要改
// 用 OpenProcess 但代码没改）。这里用 SYNCHRONIZE 句柄 + 0 超时等待：
//   - 拿到 WAIT_TIMEOUT → 还在跑
//   - WAIT_OBJECT_0    → 已退出
func alive(p *os.Process) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(p.Pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	r, _, _ := procWaitForSingleObject.Call(uintptr(h), 0)
	return r == waitTimeout
}

const waitTimeout = 0x00000102 // WAIT_TIMEOUT

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procWaitForSingleObject      = kernel32.NewProc("WaitForSingleObject")
)

// Job 包装一个 Windows 作业对象。
type Job struct{ h syscall.Handle }

// newJob 创建作业对象；失败时返回 nil（调用方回退到 killTree）。
func newJob() *Job {
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil
	}
	return &Job{h: syscall.Handle(h)}
}

// Assign 把进程加入作业对象。需要 PROCESS_SET_QUOTA | PROCESS_TERMINATE。
func (j *Job) Assign(p *os.Process) error {
	if j == nil || j.h == 0 || p == nil {
		return nil
	}
	// PROCESS_SET_QUOTA | PROCESS_TERMINATE（syscall 包没导出这两个常量）。
	const access = 0x0100 | 0x0001
	ph, err := syscall.OpenProcess(access, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(ph)
	if r, _, e := procAssignProcessToJobObject.Call(uintptr(j.h), uintptr(ph)); r == 0 {
		return e
	}
	return nil
}

// Terminate 杀掉作业对象里的所有进程（含孙进程）。
func (j *Job) Terminate() {
	if j == nil || j.h == 0 {
		return
	}
	_, _, _ = procTerminateJobObject.Call(uintptr(j.h), 1)
}

// Close 释放句柄。
func (j *Job) Close() {
	if j == nil || j.h == 0 {
		return
	}
	_, _, _ = procCloseHandle.Call(uintptr(j.h))
	j.h = 0
}
