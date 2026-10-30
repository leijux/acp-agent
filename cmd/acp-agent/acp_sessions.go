package main

// ACP 会话级资源：每个 session/new 都带着自己的 cwd 进来，工作目录必须跟着
// 会话走，而不是进程启动目录。
//
// 背景：acp-chat 这类客户端一个 agent 进程服务多个项目，切项目只发新的
// session/new（带新的 cwd），不会重启进程。如果工具集仍然用进程级 registry，
// read/write/glob/grep 的相对路径、bash 的默认 cwd、系统 prompt 里的
// 「当前工作目录」就会永远停在进程启动目录 —— 模型看到的工作区是错的。
//
// 这里为每个会话按 cwd 重建：工具集（Policy.Root）、上下文文件、持久化目录、
// 子代理工作目录。Provider / MCP 连接仍是进程级共享。

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/leijux/acp-agent/internal/acp"
	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/skills"
	"github.com/leijux/acp-agent/internal/tools"
)

// newACPSession 为 ACP 会话按其工作目录构造独立的工具集与上下文。
func (a *app) newACPSession(cwd string) (acp.SessionConfig, error) {
	root := normalizeSessionRoot(cwd, a.cfg.Root)

	allowOutside := a.guard != nil && a.guard.mode == modeAuto
	// 技能按会话 cwd 重新发现：一个进程服务多个项目时，技能集也得跟着走。
	skillSet := skills.Discover(skills.Options{
		Root:       root,
		ExtraPaths: a.cfg.Skills,
		Disabled:   a.cfg.DisableSkills,
	})
	reg, bashMgr := tools.DefaultRegistry(tools.Options{
		Policy: tools.Policy{
			Root: root, MaxReadBytes: 2 << 20,
			AllowNetworkWrite: allowOutside, // 与 auto 同步：只有全放行模式才能写网络
			AllowScriptWrite:  allowOutside, // 同上
		},
		AllowOutside: allowOutside,
		MaxParallel:  8,
		Disable:      registryDisable(a.cfg, a.webUsable),
		Subagent:     a.sessionSubagentRunner(root),
		Search:       a.search,
		Fetch:        a.fetch,
		Skills:       skillSet.Skills,
	})
	a.inheritMCPTools(reg)

	return acp.SessionConfig{
		Registry: reg,
		Executor: tools.NewExecutor(reg, 8),
		Context: agent.ContextOptions{
			WorkingDir:   root,
			ContextFiles: agent.DiscoverContextFiles(root),
			Skills:       skillSet.Skills,
		},
		Dir: acpSessionDir(root),
		// 会话独占的 bash 后台进程跟着会话一起回收。
		Close: bashMgr.KillAll,
	}, nil
}

// sessionSubagentRunner 让 delegate 子代理跑在会话自己的工作目录里。
func (a *app) sessionSubagentRunner(root string) tools.SubagentRunner {
	return func(ctx context.Context, role, task string) (string, error) {
		return a.runSubagentIn(ctx, root, role, task)
	}
}

// inheritMCPTools 把进程级 MCP 工具复制进会话 registry。
//
// MCP 连接是进程级成本（子进程 + 握手），不适合每个会话重连；工具对象本身
// 无状态（每次调用发一条 JSON-RPC），多个 registry 共享同一实例是安全的。
// 内置工具已经在会话 registry 里按本会话的 Policy 建好，跳过它们。
func (a *app) inheritMCPTools(reg *tools.Registry) {
	if a.rawReg == nil || reg == nil {
		return
	}
	for _, name := range a.rawReg.Names() {
		if _, ok := reg.Get(name); ok {
			continue
		}
		if t, ok := a.rawReg.Get(name); ok {
			_ = reg.Register(t)
		}
	}
}

// normalizeSessionRoot 把客户端给的 cwd 规整成绝对路径；空值回退进程根。
func normalizeSessionRoot(cwd, fallback string) string {
	root := strings.TrimSpace(cwd)
	if root == "" {
		root = fallback
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return filepath.Clean(root)
}

// acpSessionDir 是 ACP 会话的持久化目录：按工作目录分区，并且和 CLI 的
// session-*.jsonl 隔开放 —— 否则 -c（继续最近一次）会把 ACP 会话当成
// 「最近一次」接着聊，那是两份不同的历史格式。
func acpSessionDir(root string) string {
	return filepath.Join(newSessionStore(root).dir(), "acp")
}
