package tools

import "io"

// shellProc 抽象「执行一段脚本」，让执行引擎在“内置解释器”与“系统 shell”之间切换。
//
// Policy 会传给内置解释器：`goja` 等由 Go 实现的内建命令要按同一套 Root 约束
// 访问文件（和 read/write 工具看到的范围一致）。
//
// 默认实现（shell_interp.go）用 mvdan/sh 纯 Go 执行，不需要外部 shell；
// 加构建标签 shellnative 后（shell_exec.go）改为 os/exec 调 bash / sh / cmd.exe。
//
// 两者的差别只在内建语义与启动开销：外部命令（git、ls、rg…）都是各自 exec 出来的。
type shellProc interface {
	// Start 启动执行。语法错误、找不到 shell 可执行文件等在这里返回。
	Start() error
	// Wait 等待结束并返回退出码。宿主错误（被信号杀死、无法启动）返回 -1。
	Wait() (int, error)
	// Kill 立即终止（前台超时、请求取消、bash_kill）。
	Kill()
	// Output 返回 stdout+stderr 合并的读端，仅后台模式使用；前台模式返回 nil。
	Output() (io.ReadCloser, error)
}

// withEnv 在进程环境上追加工具传入的 env（同名的后者生效）。
func withEnv(extra map[string]string) []string {
	env := append([]string{}, getEnv()...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
