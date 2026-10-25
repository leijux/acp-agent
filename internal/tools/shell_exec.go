//go:build shellnative

package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// shellEngineHint 说明当前执行引擎。
func shellEngineHint() string {
	return "（引擎：系统 shell）"
}

// newShellProcess 用系统 shell 执行：Windows 优先 bash，其次 sh，最后 cmd.exe。
//
// 这是可选引擎：默认用 mvdan/sh 内置解释器（shell_interp.go），
// 加 -tags shellnative 才编译本文件。用到 Git Bash 特有的补全/别名/内建时选它。
func newShellProcess(_ context.Context, script, dir string, env map[string]string, stdout, stderr io.Writer, _ Policy) (shellProc, error) {
	prog, args := shellCmd(script)
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	cmd.Env = withEnv(env)
	setupProcAttr(cmd)
	// Kill 后孙进程可能还持着输出管道（Windows 上 taskkill /T 杀不到 Git Bash 的
	// coreutils 子进程），没有这个上限的话 Wait 会一直等到孙进程自然退出。
	cmd.WaitDelay = 300 * time.Millisecond
	p := &execProc{cmd: cmd}
	if stdout == nil {
		// 后台：父子进程共用一根管道，Start 之后父进程关掉写端才能读到 EOF。
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = pw, pw
		p.pipe, p.writeEnd = pr, pw
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}
	return p, nil
}

type execProc struct {
	cmd      *exec.Cmd
	pipe     *os.File
	writeEnd *os.File
	job      *processJob
}

func (p *execProc) Start() error {
	err := p.cmd.Start()
	if p.writeEnd != nil {
		_ = p.writeEnd.Close()
	}
	if err != nil {
		if p.pipe != nil {
			_ = p.pipe.Close()
		}
		return err
	}
	// Windows：把进程放进 Job Object，Kill 时才能连带杀掉孙进程。
	// 必须在 Start 之后才能拿到进程句柄。
	if p.job = newProcessJob(); p.job != nil {
		_ = p.job.Assign(p.cmd.Process)
	}
	return nil
}

func (p *execProc) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.job != nil {
		p.job.Close()
		p.job = nil
	}
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	// WaitDelay 到期但进程已退出：管道被孙进程拖住，不算执行失败。
	if errors.Is(err, exec.ErrWaitDelay) {
		return 0, nil
	}
	return -1, err
}

// Kill 用 Job Object 一次性杀掉整棵树；没有作业对象时才回退到 killTree。
func (p *execProc) Kill() {
	if p.job != nil {
		// 作业对象已覆盖整棵树（含孙进程），不必再 fork 一个 taskkill。
		p.job.Terminate()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		return
	}
	killTree(p.cmd)
}

func (p *execProc) Output() (io.ReadCloser, error) {
	if p.pipe == nil {
		return nil, nil
	}
	return p.pipe, nil
}

// shellCmd 返回平台默认 shell。
//
// Windows 上用 `bash -c`（非 login shell）而不是 `-lc`：Git Bash 的 login shell
// 会 source /etc/profile 与 ~/.bash_profile，单次启动多花 100–200ms，
// 在「多个只读命令并发」的场景下这个开销会被放大。
func shellCmd(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("bash"); err == nil {
			return p, []string{"-c", command}
		}
		if p, err := exec.LookPath("sh"); err == nil {
			return p, []string{"-c", command}
		}
		return "cmd.exe", []string{"/c", command}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-lc", command}
	}
	return "/bin/sh", []string{"-c", command}
}
