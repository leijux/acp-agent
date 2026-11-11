package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/leijux/acp-agent/internal/skills"
)

// SubagentRunner 由宿实现：在一个干净的上下文里跑子任务，返回结果文本。
//
// 与主 Agent 共享的文件系统操作仍受 Policy 约束；实现方通常把 worker/oracle
// 等角色映射到不同模型与工具子集。
type SubagentRunner func(ctx context.Context, agent, task string) (string, error)

// SubagentTool 把子任务委派给独立 agent。
//
type SubagentTool struct {
	Runner SubagentRunner
	// Manager 是后台任务管理器（为 nil 时由 Runner 自动构造）。
	Manager *SubagentManager
	// Agents 是可用角色名（用于生成描述与参数校验）。
	Agents []string
	// MaxParallel 控制同一批次内并行委派的上限。
	MaxParallel int
}

// NewSubagentTool 创建委派工具。
func NewSubagentTool(r SubagentRunner, agents ...string) *SubagentTool {
	if len(agents) == 0 {
		agents = []string{"worker", "researcher", "reviewer", "oracle", "planner"}
	}
	return &SubagentTool{Runner: r, Manager: NewSubagentManager(r), Agents: agents, MaxParallel: 4}
}

func (*SubagentTool) Name() string { return "delegate" }
func (t *SubagentTool) Description() string {
	return "把自包含的子任务交给独立 agent 在后台执行，立即返回 jobId（不阻塞本轮，也不污染当前上下文）。" +
		"子代理看不到本会话历史，任务描述必须写全（路径、目标、约束）。" +
		"默认不等结果：继续干其它事，需要结论时用 subagent_output（wait=exit 可等到结束），" +
		"不要反复轮询；不再需要的任务用 subagent_kill 终止。" +
		"确实需要本步结果才能继续时，传 wait=true 直接拿结论。" +
		"多个互不依赖的子任务用 tasks 数组一次提交。可用角色：" + strings.Join(t.Agents, ", ")
}
func (t *SubagentTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"agent": PropEnum("string", "子代理角色", t.Agents...),
		"task":  Prop("string", "完整、自包含的任务描述"),
		"tasks": map[string]any{
			"type":        "array",
			"description": "并行启动的多个任务（与 task 二选一）",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": PropEnum("string", "角色", t.Agents...),
					"task":  Prop("string", "任务描述"),
				},
				"required":             []string{"task"},
				"additionalProperties": false,
			},
		},
		"wait":      Prop("boolean", "true = 阻塞到全部结束并返回结论（默认 false = 后台跑）"),
		"timeoutMs": PropInt("wait=true 时的等待上限毫秒（默认 300000）", 0),
	})
}

// ConcurrencySafe=true：后台委派只是起任务，不等结果也不独占文件；
// 与同一批里的只读工具并行无副作用（旧实现会阻塞到结论，所以当时特意独占）。
func (*SubagentTool) ConcurrencySafe() bool { return true }
func (*SubagentTool) Mutates() bool         { return false }

func (t *SubagentTool) Execute(ctx context.Context, a Args) (Result, error) {
	if t.Manager == nil {
		if t.Runner == nil {
			return ErrorResult("delegate 未配置 Runner（宿主需注入 tools.SubagentRunner）"), nil
		}
		t.Manager = NewSubagentManager(t.Runner)
	}
	type job struct{ agent, task string }
	var jobs []job

	if agent, ok := a.String("agent"); ok && strings.TrimSpace(agent) != "" {
		task, err := a.Required("task")
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		jobs = append(jobs, job{agent: agent, task: task})
	} else if raw, ok := a["tasks"].([]any); ok && len(raw) > 0 {
		for i, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				return ErrorResult("tasks[%d] 必须是对象", i), nil
			}
			aa := Args(m)
			task, err := aa.Required("task")
			if err != nil {
				return ErrorResult("tasks[%d]: %v", i, err), nil
			}
			jobs = append(jobs, job{agent: aa.Str("agent", "worker"), task: task})
		}
	} else {
		task, err := a.Required("task")
		if err != nil {
			return ErrorResult("需要 task 或 tasks"), nil
		}
		jobs = append(jobs, job{agent: "worker", task: task})
	}

	for _, j := range jobs {
		if !t.validAgent(j.agent) {
			return ErrorResult("未知子代理角色 %q（可用：%s）", j.agent, strings.Join(t.Agents, ", ")), nil
		}
	}

	maxP := t.MaxParallel
	if maxP <= 0 {
		maxP = 4
	}
	if len(jobs) > maxP {
		return ErrorResult("一次最多提交 %d 个任务（收到 %d）", maxP, len(jobs)), nil
	}

	// 先把所有任务起起来：后台模式立即返回，阻塞模式才逐个等。
	started := make([]SubagentJob, len(jobs))
	startErrs := make([]error, len(jobs))
	for i, j := range jobs {
		started[i], startErrs[i] = t.Manager.Start(j.agent, j.task)
	}

	if !a.Bool("wait", false) {
		var b strings.Builder
		b.WriteString("已转入后台（本轮不等结果，主代理可继续干活）。\n")
		failed := 0
		for i, s := range started {
			if startErrs[i] != nil {
				failed++
				fmt.Fprintf(&b, "- [%s] %s 启动失败: %v\n", jobs[i].agent, clip(jobs[i].task, 80), startErrs[i])
				continue
			}
			fmt.Fprintf(&b, "- %s [%s] %s\n", s.ID, s.Role, clip(jobs[i].task, 120))
		}
		b.WriteString("用 subagent_output 取结论（wait=exit 等到结束，比反复轮询省 token），")
		b.WriteString("不需要了用 subagent_kill 终止。\n")
		ids := make([]string, 0, len(started))
		for i, s := range started {
			if startErrs[i] == nil {
				ids = append(ids, s.ID)
			}
		}
		return Result{
			Content: []Block{{Kind: KindText, Text: b.String()}},
			Details: map[string]any{"jobs": len(jobs), "ids": ids, "background": true, "failed": failed},
			IsError: failed == len(jobs),
		}, nil
	}

	// wait=true：退化成原来的阻塞语义（一次性拿全部结论）。
	timeoutMs := a.Int("timeoutMs", 0)
	if timeoutMs <= 0 {
		timeoutMs = 300000
	}
	if timeoutMs > maxWaitTimeoutMs {
		timeoutMs = maxWaitTimeoutMs
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	var b strings.Builder
	failed := 0
	pending := 0
	for i, s := range started {
		if i > 0 {
			b.WriteString("\n")
		}
		if startErrs[i] != nil {
			failed++
			fmt.Fprintf(&b, "=== [%d] %s (error) ===\ntask: %s\n失败: %v\n", i, jobs[i].agent, clip(jobs[i].task, 200), startErrs[i])
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		got, finished, err := t.Manager.Wait(ctx, s.ID, WaitExit, remaining)
		if err != nil || !finished {
			pending++
			fmt.Fprintf(&b, "=== [%d] %s (running) ===\njob: %s\ntask: %s\n"+
				"仍在后台运行，用 subagent_output id=%s wait=exit 继续等。\n",
				i, jobs[i].agent, s.ID, clip(jobs[i].task, 200), s.ID)
			continue
		}
		status := "ok"
		if got.Err != "" || got.Cancelled {
			status = "error"
			failed++
		}
		fmt.Fprintf(&b, "=== [%d] %s (%s) ===\ntask: %s\n", i, jobs[i].agent, status, clip(jobs[i].task, 200))
		if got.Cancelled {
			fmt.Fprintf(&b, "已终止\n")
			continue
		}
		if got.Err != "" {
			fmt.Fprintf(&b, "失败: %s\n", got.Err)
			continue
		}
		body, _ := Truncate(strings.TrimSpace(got.Result), 0, 24<<10)
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: b.String()}},
		Details: map[string]any{"jobs": len(jobs), "failed": failed, "pending": pending},
		IsError: failed == len(jobs),
	}, nil
}

func (t *SubagentTool) validAgent(name string) bool {
	for _, a := range t.Agents {
		if a == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 默认工具集
// ---------------------------------------------------------------------------

// Options 控制默认工具集的内容。
type Options struct {
	Policy      Policy
	Search      SearchProvider
	Fetch       FetchProvider
	Subagent    SubagentRunner
	Agents      []string
	MaxParallel int
	// AllowOutside 让内置工具允许访问 Policy.Root 之外的路径（置位 Policy.AllowOutside）。
	//
	// 作用于 read/write/edit/ls/glob/grep/js 的路径解析，以及 bash 的 cwd。
	// 用途是 --permissions auto：那里连改动型工具都不用确认，守着 root 只会
	// 让模型绕道 bash（bash 的命令本身从来不受 Root 约束）。ask/readonly 保留边界。
	AllowOutside bool
	// Skills 是发现的技能清单。非空时注册 skill 工具；为空则不注册
	// （没东西可加载时，一段工具描述只是白占 prompt）。
	Skills []skills.Skill
	// Disable 指定不注册的工具名。
	Disable []string
}

// DefaultRegistry 组装一套与 Pi 对齐的基础工具。
//
// 返回的 Registry 已注册：read/write/edit/ls/glob/grep/bash/bash_output/bash_kill/
// parallel/todo/web_search/web_fetch/delegate。
func DefaultRegistry(o Options) (*Registry, *BashManager) {
	if o.MaxParallel <= 0 {
		o.MaxParallel = 8
	}
	if o.Policy.Root == "" {
		o.Policy = DefaultPolicy()
	}
	if o.AllowOutside {
		o.Policy.AllowOutside = true
	}
	disable := make(map[string]bool, len(o.Disable))
	for _, d := range o.Disable {
		disable[d] = true
	}
	// "*" 表示「一个内建工具都不要」（Pi 的 --no-tools / --no-builtin-tools）。
	// MCP 工具不经过这里，由 connectMCP 单独注册 —— 所以 --no-builtin-tools
	// 自然只关内建、保留 MCP。
	disableAll := disable["*"]

	r := NewRegistry()
	bash := NewBashTool(o.Policy)
	subagents := NewSubagentManager(o.Subagent)
	// 会话结束时回收后台资源：bash 进程 + 后台子代理。
	// 放在 Registry 上而不是让每个调用方自己拿 manager，是因为 ACP 一个进程
	// 服务多个会话，漏掉一个就会把子代理进程变成孤儿。
	r.AddCleanup(bash.Manager.KillAll)
	r.AddCleanup(subagents.KillAll)
	// parallel 依赖 Registry 已包含其它工具，最后注册。
	candidates := []Tool{
		NewReadTool(o.Policy),
		NewWriteTool(o.Policy),
		NewEditTool(o.Policy),
		NewLSTool(o.Policy),
		NewGlobTool(o.Policy),
		NewGrepTool(o.Policy),
		bash,
		NewBashOutputTool(bash.Manager),
		NewBashKillTool(bash.Manager),
		NewJSToolWithPolicy(o.Policy),
		NewTodoTool(),
		NewWebSearchTool(o.Search),
		NewWebFetchTool(o.Fetch),
		NewSubagentTool(o.Subagent, o.Agents...),
	}
	// delegate 被禁用时（--disable-tools delegate / 子代理防递归）三个子代理
	// 工具一起关：只留 output/kill 会让模型看到一堆「找不到任务」的工具。
	if disableAll || disable["delegate"] {
		for i := range candidates {
			if candidates[i].Name() == "delegate" {
				candidates = append(candidates[:i], candidates[i+1:]...)
				break
			}
		}
	} else {
		candidates = append(candidates,
			NewSubagentOutputTool(subagents),
			NewSubagentKillTool(subagents),
		)
	}
	for _, t := range candidates {
		if disableAll || disable[t.Name()] {
			continue
		}
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
	// 没有任何技能时不注册 skill 工具：工具描述本身也占 prompt，
	// 一个只会回答「没有技能」的工具不值得。
	if len(o.Skills) > 0 && !disableAll && !disable["skill"] {
		r.MustRegister(NewSkillTool(o.Skills))
	}
	if !disableAll && !disable["parallel"] {
		r.MustRegister(NewParallelTool(r, o.MaxParallel))
	}
	return r, bash.Manager
}

// SortedNames 便于稳定展示。
func SortedNames(r *Registry) []string {
	n := r.Names()
	sort.Strings(n)
	return n
}
