package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/leijux/acp-agent/internal/acp"
)

// 子代理通过 ACP 协议驱动本程序自身：
//
//	主进程(acp-agent REPL)  ──spawn──▶  acp-agent --acp（stdio JSON-RPC）
//	     │  client 侧                          │  agent 侧
//	     └──── session/prompt ────────────────▶│
//	     ◀──── session/update 通知 ────────────┘
//	     ◀──── session/request_permission ─────┘   （子代理的工具确认回传到主进程）
//
// 走 ACP 而不是进程内直接调 agent 的好处：
//   - 上下文、kernel 会话、工具状态天然隔离，子代理看不到主会话历史
//   - 子代理崩了不会带走主进程
//   - 同一套协议可以换成任意第三方 ACP agent（改 Command 即可）
//
// 代价是每次 delegate 要起一个进程（kernel 的 goja VM 初始化 ~35ms），
// 相对一次 LLM 往返可以忽略。

// SubagentConfig 描述如何启动子代理。为零值时用「自身 + --acp」。
type SubagentConfig struct {
	// Command 是可执行文件。留空或 "self" 表示用当前可执行文件。
	Command string `json:"command,omitempty"`
	// Args 追加在自动生成的参数之后。
	Args []string `json:"args,omitempty"`
	// Model 非空时用 -m 覆盖子代理模型（角色差异化最省事的做法）。
	Model string `json:"model,omitempty"`
	// Permissions 是子代理自己的权限档位，缺省继承主进程。
	//
	// 无论取什么值，子进程都会以 ask 启动，从而把改动型工具上报回主进程，
	// 由主进程的权限守卫最终裁决（否则 delegate 就成了绕过确认的后门）。
	Permissions string `json:"permissions,omitempty"`
	// Env 追加环境变量，格式 KEY=VALUE。
	Env []string `json:"env,omitempty"`
}

// isSelf 报告是否要启动自身。
func (c SubagentConfig) isSelf() bool {
	return c.Command == "" || c.Command == "self" || c.Command == "self:"+execSelfName()
}

func execSelfName() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "acp-agent"
}

// buildSpec 组装子代理启动参数。
//
// 关键约束：
//   - --acp：以 ACP agent 模式运行（stdio JSON-RPC）
//   - --permissions ask：改动型工具必须上报回主进程裁决
//   - --disable-tools delegate：禁止子代理再委派，防无限递归
//   - --root：与主进程同一个工作目录根，避免子代理在别处动手
func (a *app) buildSpec(role string) (acp.SubagentSpec, error) {
	cfg := SubagentConfig{}
	if a.cfg.Subagent != nil {
		cfg = *a.cfg.Subagent
	}
	self := cfg.isSelf()

	command := cfg.Command
	if self {
		exe, err := os.Executable()
		if err != nil {
			return acp.SubagentSpec{}, fmt.Errorf("无法定位自身可执行文件: %w", err)
		}
		command = exe
	}

	args := []string{"--acp", "--no-color", "--root", a.cfg.Root}
	// 子代理只用 kernel 的压缩与 provider，不落主会话文件。
	if a.cfg.Provider != "" {
		args = append(args, "--provider", a.cfg.Provider)
	}
	model := cfg.Model
	if model == "" {
		model = a.resolveModelID()
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	perm := cfg.Permissions
	if perm == "" {
		perm = permAsk // 必须 ask：让子代理把改动上报回来
	}
	args = append(args, "--permissions", perm)
	args = append(args, "--disable-tools", "delegate")
	// 子代理不加载中文分词词典：它只负责干活，不做 BM25 检索（压缩决策由
	// 主进程的 kernel 做），而词典要占 100+MB 内存 + 450ms 启动。
	// 主进程的 config.json 里即使设了 segmenter=gse，也会被子代理这里的显式值盖掉。
	args = append(args, "--segmenter", "rune")
	// 子代理不背主进程的 MCP server 集合：启动更快、工具表更小、
	// 权限面更窄。需要时给 SubagentConfig.Args 传 --mcp-config 单独开。
	args = append(args, "--no-mcp")
	// 子代理不继承主进程的会话文件，但共享配置目录（API key 在那里）。
	if cc := os.Getenv("ACP_AGENT_CONFIG"); cc != "" {
		args = append(args, "--config", cc)
	}
	args = append(args, cfg.Args...)

	return acp.SubagentSpec{
		Command: command,
		Args:    args,
		Env:     cfg.Env,
		Dir:     a.cfg.Root,
	}, nil
}

// resolveModelID 返回子代理默认模型：优先 Subagent.Model，其次当前会话模型。
// agent 为 nil 时（纯配置场景、测试）回退到 cfg.Model。
func (a *app) resolveModelID() string {
	if a.agent != nil {
		return a.agent.Model().ID
	}
	return a.cfg.Model
}

// runSubagent 实现 tools.SubagentRunner：起一个 ACP 子代理，跑一轮，返回文本。
func (a *app) runSubagent(ctx context.Context, role, task string) (string, error) {
	spec, err := a.buildSpec(role)
	if err != nil {
		return "", err
	}

	if a.term != nil {
		a.term.logf("↳ 子代理 %s 启动（%s）\n", role, describeSubagentCommand(spec))
	}

	stderr := a.subagentStderr(role)
	// 子代理退出时把最后一行（可能没有换行）刷出来。
	defer stderr.Flush()
	sub, err := acp.SpawnSubagent(ctx, acp.SubagentOptions{
		Spec:         spec,
		ClientInfo:   acp.Implementation{Name: "acp-agent", Version: version},
		OnPermission: a.subagentPermission(role),
		OnUpdate:     a.subagentUpdate(role),
		Stderr:       stderr,
	})
	if err != nil {
		return "", fmt.Errorf("子代理启动失败: %w", err)
	}
	defer sub.Close()

	res, err := sub.Prompt(ctx, task)
	if err != nil {
		return "", err
	}
	return formatSubagentResult(role, res), nil
}

// describeSubagentCommand 把启动命令压缩成一行日志。
func describeSubagentCommand(spec acp.SubagentSpec) string {
	name := spec.Command
	if exe, err := os.Executable(); err == nil && exe == name {
		name = "self"
	}
	parts := append([]string{name}, spec.Args...)
	return strings.Join(parts, " ")
}

// subagentPermission 把子代理的授权请求交给主进程的权限守卫裁决。
//
// 语义与内置工具一致：readonly 拒绝改动、auto 放行、ask 询问用户。
// 工具名加 [role] 前缀，既让用户看清来源，也让「总是允许」只对这一类请求生效。
//
// ACP 模式下本地 guard 没有终端可问（stdin 是 JSON-RPC），所以直接转给
// 客户端（session/request_permission）；只有非 ACP 模式才落回本地 guard。
func (a *app) subagentPermission(role string) func(context.Context, acp.RequestPermissionParams) (string, error) {
	return func(ctx context.Context, p acp.RequestPermissionParams) (string, error) {
		name := p.ToolCall.Name
		if name == "" {
			name = p.ToolCall.Title
		}
		if name == "" {
			name = "未知工具"
		}
		label := fmt.Sprintf("%s: %s", role, name)
		mutates := subagentKindMutates(p.ToolCall.Kind)

		// 先按主进程自己的权限档位裁决：auto 全放行、readonly 拒改动。
		// 只有 ask 才需要麻烦用户 —— 否则 permissions=auto 的会话里
		// 子代理每动一下都要弹窗，甚至不弹（客户端不支持）就一直等到超时。
		if a.guard != nil {
			switch a.guard.mode {
			case modeAuto:
				return pickOption(p.Options, "allow_once", "allow"), nil
			case modeReadonly:
				if mutates {
					return pickOption(p.Options, "reject_once", "reject"), nil
				}
			}
		}

		allow, handled, err := a.acpAuthorize(ctx, label, mutates)
		if handled {
			if err != nil {
				return "", err
			}
			if allow {
				if id := pickOption(p.Options, "allow_once", "allow"); id != "" {
					return id, nil
				}
				return "", nil
			}
			// 拒绝：优先选 reject_once，找不到就直接 cancelled。
			if id := pickOption(p.Options, "reject_once", "reject"); id != "" {
				return id, nil
			}
			return "", nil
		}

		if err := a.guard.authorize(ctx, label, mutates); err != nil {
			// 拒绝：优先选 reject_once，找不到就直接 cancelled。
			if id := pickOption(p.Options, "reject_once", "reject"); id != "" {
				return id, nil
			}
			return "", nil
		}
		// 放行：allow_once 语义最窄，避免子代理在后续轮次悄悄改动。
		if id := pickOption(p.Options, "allow_once", "allow"); id != "" {
			return id, nil
		}
		return "", nil
	}
}

// subagentKindMutates 依据 ACP toolKind 判断是否会改动状态。
// 未知 kind 一律按「会改动」处理 —— 权限判断宁可多问一次。
func subagentKindMutates(kind string) bool {
	switch strings.ToLower(kind) {
	case "read", "search", "think", "fetch":
		return false
	default:
		return true
	}
}

// pickOption 按 kind 的优先级挑选选项；都匹配不到时用前缀兜底。
func pickOption(opts []acp.PermissionOption, kinds ...string) string {
	for _, want := range kinds {
		for _, o := range opts {
			if strings.EqualFold(o.Kind, want) {
				return o.OptionID
			}
		}
	}
	return ""
}

// subagentUpdate 把子代理的活动以低调方式回显到 stderr，
// 让用户能看出「现在跑到子代理里了」而不是主代理卡住。
func (a *app) subagentUpdate(role string) func(acp.SubagentUpdate) {
	return func(u acp.SubagentUpdate) {
		if a.term == nil {
			return
		}
		switch u.Kind {
		case acp.SessionUpdateToolCall:
			if u.ToolCall != nil {
				a.term.logf("  [%s] ⚙ %s\n", role, firstNonBlank(u.ToolCall.Title, u.ToolCall.Name))
			}
		}
	}
}

// subagentStderr 把子代理的 stderr 转发出来，便于定位它为什么失败。
func (a *app) subagentStderr(role string) *prefixWriter {
	return &prefixWriter{w: os.Stderr, prefix: "[" + role + "] "}
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// formatSubagentResult 把子代理结论组装成给主模型看的文本。
// 异常终止原因必须显式说明，否则主模型会误以为任务已完成。
func formatSubagentResult(role string, res *acp.SubagentResult) string {
	var b strings.Builder
	if strings.TrimSpace(res.Text) == "" {
		b.WriteString(fmt.Sprintf("（子代理 %s 没有返回任何文本）\n", role))
	} else {
		b.WriteString(strings.TrimSpace(res.Text))
		b.WriteString("\n")
	}

	var notes []string
	switch res.StopReason {
	case acp.StopEndTurn, "":
	case acp.StopMaxTokens:
		notes = append(notes, "输出被 max_tokens 截断，结论可能不完整")
	case acp.StopMaxTurnReqs:
		notes = append(notes, "达到最大轮数上限，任务可能未完成")
	case acp.StopRefusal:
		notes = append(notes, "子代理拒绝了该任务")
	case acp.StopCancelled:
		notes = append(notes, "被取消")
	default:
		notes = append(notes, "异常结束: "+res.StopReason)
	}
	if res.Cancelled {
		notes = append(notes, "已中断")
	}
	if res.ToolCalls > 0 {
		notes = append(notes, fmt.Sprintf("工具调用 %d 次", res.ToolCalls))
	}
	if res.Usage != nil && res.Usage.Total > 0 {
		notes = append(notes, fmt.Sprintf("tokens %d", res.Usage.Total))
	}
	if len(notes) > 0 {
		b.WriteString("\n— 子代理 ")
		b.WriteString(role)
		b.WriteString("：")
		b.WriteString(strings.Join(notes, "，"))
		b.WriteString(" —\n")
	}
	return b.String()
}

// prefixWriter 给每行加前缀后写到 w（子代理的 stderr 归并到主进程）。
type prefixWriter struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
	line   []byte // 按字节累积，不转 rune
}

// prefixMaxLine 是单行缓冲上限。子代理可能输出一整行超大 JSON；不设上限会无界增长。
// 超过后强制断行输出，保证后续内容仍可见。
const prefixMaxLine = 4 << 10

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range b {
		if c == '\n' {
			p.flushLocked()
			continue
		}
		// 关键：按**字节**累积。管道读取会在任意字节处切断多字节字符，
		// 早先的实现把它转成 string 再按 rune 遍历，边界的半个 UTF-8 序列
		// 会变成 U+FFFD —— 子代理的中文输出会零星变成 “�”。
		p.line = append(p.line, c)
		if len(p.line) >= prefixMaxLine {
			p.flushLocked()
		}
	}
	return len(b), nil
}

// flushLocked 输出当前行（调用方需持 p.mu）。
func (p *prefixWriter) flushLocked() {
	if len(p.line) == 0 {
		return
	}
	_, _ = fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.line)
	p.line = p.line[:0]
}

// Flush 输出尚未换行的尾部（子代理退出时调用）。
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
}
