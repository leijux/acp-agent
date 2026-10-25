//go:build !windows

package tools

import (
	"os/exec"

	"github.com/leijux/acp-agent/internal/proc"
)

// 进程树管理已抽到 internal/proc（MCP stdio server 也要用）。
// 这里保留旧名字，避免改动 shell_exec.go。

func setupProcAttr(cmd *exec.Cmd) { proc.Setup(cmd) }

func killTree(cmd *exec.Cmd) { proc.KillTree(cmd) }

type processJob = proc.Job

func newProcessJob() *processJob { return proc.NewJob() }
