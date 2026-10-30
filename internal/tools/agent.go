package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/leijux/acp-agent/internal/skills"
)

// SubagentRunner 由宿实现：在一个干净的上下文里跑子任务，返回结果文本。
//
// 与主 Agent 共享的文件系统操作仍受 Policy 约束；实现方通常把 worker/oracle
// 等角色映射到不同模型与工具子集。
type SubagentRunner func(ctx context.Context, agent, task string) (string, error)

// SubagentTool 把子任务委派给独立 agent。
type SubagentTool struct {
	Runner SubagentRunner
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
	return &SubagentTool{Runner: r, Agents: agents, MaxParallel: 4}
}

func (*SubagentTool) Name() string { return "delegate" }
func (t *SubagentTool) Description() string {
	return "把自包含的子任务交给独立 agent 执行，返回其结论（不污染当前上下文）。" +
		"子代理看不到本会话历史，任务描述必须写全（路径、目标、约束）。" +
		"多个互不依赖的子任务用 tasks 数组一次提交，可并行执行。" +
		"可用角色：" + strings.Join(t.Agents, ", ")
}
func (t *SubagentTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"agent": PropEnum("string", "子代理角色", t.Agents...),
		"task":  Prop("string", "完整、自包含的任务描述"),
		"tasks": map[string]any{
			"type":        "array",
			"description": "并行执行的多个任务（与 task 二选一）",
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
	})
}

// ConcurrencySafe=false：委派本身会消耗大量资源，独占一批由内部再并行。
func (*SubagentTool) ConcurrencySafe() bool { return false }
func (*SubagentTool) Mutates() bool         { return false }

func (t *SubagentTool) Execute(ctx context.Context, a Args) (Result, error) {
	if t.Runner == nil {
		return ErrorResult("delegate 未配置 Runner（宿主需注入 tools.SubagentRunner）"), nil
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
	type out struct {
		agent, task, result string
		err                 error
	}
	results := make([]out, len(jobs))
	sem := make(chan struct{}, maxP)
	var wg sync.WaitGroup
	for i := range jobs {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			text, err := t.Runner(ctx, jobs[i].agent, jobs[i].task)
			results[i] = out{agent: jobs[i].agent, task: jobs[i].task, result: text, err: err}
		}()
	}
	wg.Wait()

	var b strings.Builder
	failed := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		status := "ok"
		if r.err != nil {
			status = "error"
			failed++
		}
		fmt.Fprintf(&b, "=== [%d] %s (%s) ===\ntask: %s\n", i, r.agent, status, clip(r.task, 200))
		if r.err != nil {
			fmt.Fprintf(&b, "失败: %v\n", r.err)
			continue
		}
		body, _ := Truncate(r.result, 0, 24<<10)
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: b.String()}},
		Details: map[string]any{"jobs": len(jobs), "failed": failed},
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
