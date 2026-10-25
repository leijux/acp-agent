// Package proc 管理子进程树。
//
// 启动 MCP server（`npx -y ...`、`uvx ...`）或 shell 命令时，直接 Kill 顶层进程
// 会留下孙进程（node.exe、python.exe）。这里用作业对象/进程组一次性杀整棵树：
//
//   - Windows：Job Object（TerminateJobObject），对 `npx` 包装出的孙进程有效；
//     taskkill /T 依赖 ParentProcessId 链，Git Bash fork 出来的 coreutils 杀不掉。
//   - Unix：Setpgid + kill(-pid, SIGKILL)。
package proc

import (
	"os"
	"os/exec"
)

// Job 是作业对象（Windows）或进程组（Unix）的句柄。
// 各平台的实现见 job_windows.go / job_unix.go。
//
// Windows 上创建失败时 NewJob 返回 nil，此时 Terminate 是空操作，
// 调用方应回退到 KillTree。
func NewJob() *Job { return newJob() }

// Setup 在 cmd.Start 之前配置进程属性（Unix 上开启独立进程组）。
func Setup(cmd *exec.Cmd) { setup(cmd) }

// KillTree 杀掉 cmd 启动的整棵进程树。cmd 未启动时是空操作。
func KillTree(cmd *exec.Cmd) { killTree(cmd) }

// Terminate 尽力结束进程：先关标准输入（让 server 自己退出），再杀进程树。
// 这是 MCP stdio server 的标准关闭顺序。
func Terminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	KillTree(cmd)
}

// Alive 判断进程是否仍在运行。
//
// 两个平台的探测方式不同（见 alive 的各自实现）：Unix 用 signal 0，
// Windows 上 os.Process.Signal 对 Signal(0) 一律返回错误，必须走 OpenProcess。
func Alive(p *os.Process) bool {
	if p == nil {
		return false
	}
	return alive(p)
}
