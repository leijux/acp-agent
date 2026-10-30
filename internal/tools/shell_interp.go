//go:build !shellnative

package tools

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// shellEngineHint 说明当前执行引擎。
//
// 这里的命令名是给模型看的「能用什么」的唯一入口（工具描述不列内建清单，
// 模型也不会去猜）：新加内建命令时记得同步这一行，否则它就是个存在但
// 永远不会被想起来的命令。
func shellEngineHint() string {
	return "（内置解释器 POSIX/bash 语义；" +
		"PATH 里没有 ls/cat/head/tail/sleep/pwd/mkdir/rm/cp/mv/grep 时自动用内建实现，" +
		"自定义工具 goja 可在管道里执行 JS 做纯计算）"
}

// newShellProcess 用 mvdan/sh 的纯 Go 解释器执行脚本（默认引擎）。
//
// 不加任何 tag 就是它：Windows 上不需要安装 bash。想回到系统 shell 用
// -tags shellnative（shell_exec.go）。
//
// 支持 POSIX sh + 大部分 bash 语法（[[ ]]、(( )、数组、heredoc、管道、重定向、
// 函数、set -e/-u/-o pipefail）。内建命令（cd/echo/export/printf/test/…）由
// 解释器自己实现；awk/sed/git 这类外部命令仍然走 os/exec 调用系统可执行文件。
// PATH 里找不到的常用命令（sleep/ls/cat/head/tail/pwd/mkdir/rm/grep）由
// shell_builtin.go 的 ExecHandler 中间件用纯 Go 实现回退，因此纯 Windows 环境
// （一个 coreutils 都没装）也能跑通。
func newShellProcess(ctx context.Context, script, dir string, env map[string]string, stdout, stderr io.Writer, policy Policy) (shellProc, error) {
	p := &interpProc{ctx: ctx, script: script, dir: dir, env: env, stdout: stdout, stderr: stderr, policy: policy}
	if stdout == nil {
		pr, pw := io.Pipe()
		p.pipeR, p.pipeW = pr, pw
		p.stdout, p.stderr = pw, pw
	}
	return p, nil
}

type interpProc struct {
	ctx     context.Context
	script  string
	dir     string
	env     map[string]string
	stdout  io.Writer
	stderr  io.Writer
	policy  Policy
	pipeR   *io.PipeReader
	pipeW   *io.PipeWriter
	runner  *interp.Runner
	done    chan struct{}
	mu      sync.Mutex
	cancel  context.CancelFunc
	status  int
	runErr  error
	killed  bool
	pipeOne sync.Once
}

func (p *interpProc) Start() error {
	p.done = make(chan struct{})
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(p.script), "")
	if err != nil {
		p.closePipe()
		close(p.done)
		return err
	}
	runner, err := interp.New(
		interp.StdIO(nil, p.stdout, p.stderr),
		interp.Env(expand.ListEnviron(withEnv(p.env)...)),
		interp.Dir(p.dir),
		// 内建命令回退：PATH 里没有 sleep/ls/cat 之类可执行文件时用纯 Go 实现顶上。
		interp.ExecHandlers(builtinExecHandler(p.policy)),
	)
	if err != nil {
		p.closePipe()
		close(p.done)
		return err
	}
	p.runner = runner

	go func() {
		defer close(p.done)
		// mvdan/sh v3 没有「中断整个脚本」的 API：唯一的取消通道是传给 Run 的 ctx。
		// DefaultExecHandler 用 exec.CommandContext 执行外部命令，cancel 会连带杀掉它们；
		// 纯内建死循环（while true; do :; done）无法被中断——解释器本身的限制，
		// 已在 docs/tools.md 记录。
		ctx, cancel := context.WithCancel(p.ctx)
		p.mu.Lock()
		p.cancel = cancel
		p.mu.Unlock()
		defer cancel()

		err := runner.Run(ctx, file)

		p.mu.Lock()
		p.status, p.runErr = 0, nil
		if err != nil {
			if st, ok := func() (status uint8, ok bool) {
				if es, ok := errors.AsType[interp.ExitStatus](err); ok {
					return uint8(es), true
				}
				return 0, false
			}(); ok {
				p.status = int(st)
			} else {
				p.status, p.runErr = 1, err
			}
		}
		p.mu.Unlock()
		p.closePipe()
	}()
	return nil
}

func (p *interpProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return -1, nil
	}
	return p.status, p.runErr
}

func (p *interpProc) Kill() {
	p.mu.Lock()
	p.killed = true
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *interpProc) Output() (io.ReadCloser, error) {
	if p.pipeR == nil {
		return nil, nil
	}
	return p.pipeR, nil
}

func (p *interpProc) closePipe() {
	p.pipeOne.Do(func() {
		if p.pipeW != nil {
			_ = p.pipeW.Close()
		}
	})
}
