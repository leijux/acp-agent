package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
	return a.buildSpecIn(a.cfg.Root, role)
}

// buildSpecIn 用指定的工作目录构造子代理启动参数。
//
// ACP 模式下会话的 cwd 不一定是进程启动目录（多项目客户端会切项目），
// 子代理必须落在「这个会话」的目录里，否则它会在另一个项目里动手。
func (a *app) buildSpecIn(root, role string) (acp.SubagentSpec, error) {
	if strings.TrimSpace(root) == "" {
		root = a.cfg.Root
	}
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

	args := []string{"--acp", "--no-color", "--root", root}
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
	// 主进程的 kernel 做），而词典要占 ~30MB 内存 + ~50ms 启动。
	// 分词器已经没有命令行开关，所以用环境变量关：优先级是 flag > env > 配置文件，
	// 所以即使是子代理自己的 config.json 写了 segmenter=gse，也会被这里盖掉。
	// 子进程会继承主进程环境，重复的 key 以追加在后面的为准（os/exec 语义）。
	env := append(append([]string{}, cfg.Env...), "ACP_AGENT_SEGMENTER=rune")
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
		Env:     env,
		Dir:     root,
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
	return a.runSubagentIn(ctx, a.cfg.Root, role, task)
}

// runSubagentIn 让子代理在指定工作目录里跑（ACP 会话用会话自己的 cwd）。
func (a *app) runSubagentIn(ctx context.Context, root, role, task string) (string, error) {
	spec, err := a.buildSpecIn(root, role)
	if err != nil {
		return "", err
	}

	started := time.Now()
	if a.term != nil {
		// 不再重复 task：上面那行「→ delegate agent=… task=…」已经打过了。
		line := "启动"
		if hint := subagentCmdHint(spec); hint != "" {
			line += " · " + hint
		}
		a.subagentLine("%s %s %s",
			a.term.paint(cDim, "↳ 子代理"), a.term.paint(cBold, role),
			a.term.paint(cDim, line))
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
		a.subagentDone(role, started, 0, "", fmt.Errorf("启动失败: %w", err))
		return "", fmt.Errorf("子代理启动失败: %w", err)
	}
	defer sub.Close()

	res, err := sub.Prompt(ctx, task)
	if err != nil {
		a.subagentDone(role, started, 0, "", err)
		return "", err
	}
	a.subagentDone(role, started, res.ToolCalls, res.Text, nil)
	return formatSubagentResult(role, res), nil
}

// subagentDone 收尾一行：成功报工具次数/耗时并预览结论，失败报原因。
func (a *app) subagentDone(role string, started time.Time, tools int, text string, err error) {
	if a.term == nil {
		return
	}
	took := time.Since(started).Round(100 * time.Millisecond)
	if err != nil {
		a.subagentLine("%s %s %s", a.term.paint(cRed, "↳ 子代理"),
			a.term.paint(cBold, role), a.term.paint(cRed, err.Error()))
		return
	}
	a.subagentLine("%s %s %s", a.term.paint(cDim, "↳ 子代理"),
		a.term.paint(cBold, role),
		a.term.paint(cDim, fmt.Sprintf("完成 · %d 次工具 · %s", tools, fmtDuration(took))))
	// 结论文本会作为工具结果交给主模型，这里只给一眼预览，
	// 否则用户要等主模型复述才知道子代理干了什么。
	if p := firstLine(strings.TrimSpace(text), 96); p != "" {
		a.subagentLine("    %s %s", a.term.paint(cDim, "⋯"), a.term.paint(cDim, p))
	}
}

// subagentCmdHint 返回一行启动命令提示。
//
// 常见情形是「自身 + --acp」那一串，全是噪声，直接不显示；
// 只有配置了第三方 ACP agent（SubagentConfig.Command）时才值得看一眼。
func subagentCmdHint(spec acp.SubagentSpec) string {
	if exe, err := os.Executable(); err == nil && exe == spec.Command {
		return ""
	}
	return truncRunes(strings.Join(append([]string{spec.Command}, spec.Args...), " "), 60)
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

// subagentLine 输出一行子代理活动。
//
// 为什么不能直接写 stderr：TUI 下屏幕归 bubbletea 独占，往 stderr 写会
// 把正在重绘的界面搅成乱码（而且这些行不进块视图，滚一下就没了）。
// 所以交互界面里走 term.printf → 行缓冲 → 工具块；-p / --no-tui 仍走 stderr，
// 因为 -p 的 stdout 是「结果」，不能掺日志。
func (a *app) subagentLine(format string, args ...any) {
	if a.term == nil {
		return
	}
	if a.ui == nil {
		a.term.logf(format+"\n", args...)
		return
	}
	a.setKind(kindTool)
	a.endPendingLine() // 别粘到流式半行上
	a.term.printf(format+"\n", args...)
}

// subagentUpdate 把子代理的活动回显出来，让用户看出「现在跑到子代理里了」，
// 而不是以为主代理卡死。
//
// 只画两层：启动/结束各一行，中间的工具调用缩进一级；子代理自己的回复
// 不往主视图里灌（那是给主模型看的，刷屏只会盖掉主对话）。
func (a *app) subagentUpdate(role string) func(acp.SubagentUpdate) {
	return func(u acp.SubagentUpdate) {
		if a.term == nil {
			return
		}
		switch u.Kind {
		case acp.SessionUpdateToolCall:
			if u.ToolCall == nil {
				return
			}
			title := firstNonBlank(u.ToolCall.Title, u.ToolCall.Name)
			if strings.TrimSpace(title) == "" {
				title = u.ToolCall.Kind
			}
			a.subagentLine("    %s %s", a.term.paint(cDim, "⚙"),
				a.term.paint(cDim, truncRunes(firstLine(title, 78), 78)))
		case acp.SessionUpdateToolCallUpdate:
			// 失败的工具调用单独报一行；成功的不报，避免一步两行。
			if u.ToolCall == nil || !isFailedStatus(u.ToolCall.Status) {
				return
			}
			a.subagentLine("    %s %s", a.term.paint(cRed, "✗"),
				a.term.paint(cRed, truncRunes(firstLine(firstNonBlank(u.ToolCall.Title, u.ToolCall.Name), 78), 78)))
		}
	}
}

// isFailedStatus 判断 ACP 工具状态是否是失败终态。
func isFailedStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "failed", "error", "rejected", "cancelled":
		return true
	}
	return false
}

// subagentStderr 把子代理的 stderr 转发出来，便于定位它为什么失败。
//
// 不再直接写 os.Stderr：TUI 下那会把界面搅乱。交汇一行由 subagentLine 决定去向
// （界面块 or stderr）。上限两个：单行长度 + 总行数 —— 子代理吐一个超大 JSON 或
// panic 堆栈时，不兜住就会把主对话刷没。
func (a *app) subagentStderr(role string) *prefixWriter {
	return &prefixWriter{
		prefix:   "[" + role + "] ",
		maxLines: subagentStderrMaxLines,
		emit: func(line string) {
			a.subagentLine("    %s %s", a.term.paint(cYellow, "!"),
				a.term.paint(cDim, truncRunes(line, 160)))
		},
		emitDropped: func(n int) {
			a.subagentLine("    %s", a.term.paint(cDim,
				fmt.Sprintf("…（子代理还有 %d 行输出未显示）", n)))
		},
	}
}

// subagentStderrMaxLines 是单个子代理最多回显的 stderr 行数。
const subagentStderrMaxLines = 24

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// fmtDuration 把耗时写成 3s / 1m05s（与状态行计时同一个口径）。
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
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

// prefixWriter 给每行加前缀后交给 emit（子代理的 stderr 归并到主视图）。
type prefixWriter struct {
	prefix string
	emit   func(string)
	// emitDropped 在超过 maxLines 时被调一次，说明还有多少行没显示。
	emitDropped func(int)
	maxLines    int

	mu      sync.Mutex
	line    []byte // 按字节累积，不转 rune
	written int
	dropped int
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
	if p.maxLines > 0 && p.written >= p.maxLines {
		p.dropped++
		p.line = p.line[:0]
		return
	}
	p.written++
	p.emit(p.prefix + string(p.line))
	p.line = p.line[:0]
}

// Flush 输出尚未换行的尾部（子代理退出时调用）。
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
	if p.dropped > 0 && p.emitDropped != nil {
		n := p.dropped
		p.dropped = 0
		p.emitDropped(n)
	}
}
