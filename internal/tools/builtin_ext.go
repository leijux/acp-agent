package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// parallel：一次发起多个只读工具调用（并发执行）
// ---------------------------------------------------------------------------

// ParallelTool 是元工具：模型可以一次性提交多个独立调用，由本地并发执行。
//
// 相比让模型串行调 N 次，它省掉 N-1 轮往返；语义上要求各调用互不依赖，
// 因此只允许并发安全工具（read/grep/glob/ls/web_search 等）。
type ParallelTool struct {
	Registry    *Registry
	MaxParallel int
}

// NewParallelTool 创建并行元工具。
func NewParallelTool(r *Registry, maxParallel int) *ParallelTool {
	return &ParallelTool{Registry: r, MaxParallel: maxParallel}
}

func (*ParallelTool) Name() string { return "parallel" }
func (*ParallelTool) Description() string {
	return "一次性并行执行多个只读工具调用（read/grep/glob/ls/web_search 等），比逐个调用省往返时间。" +
		"每个调用必须互不依赖；写文件/edit/bash 修改类操作不要放进来（会被拒绝）。"
}
func (*ParallelTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"calls": map[string]any{
			"type":        "array",
			"description": "要并发执行的调用列表",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool": Prop("string", "工具名"),
					"args": map[string]any{"type": "object", "description": "该工具的参数对象"},
				},
				"required":             []string{"tool", "args"},
				"additionalProperties": false,
			},
		},
	}, "calls")
}

// ConcurrencySafe=false：元工具自身独占一批，避免与其它调用交叉调度。
func (*ParallelTool) ConcurrencySafe() bool { return false }
func (*ParallelTool) Mutates() bool         { return false }

func (t *ParallelTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	raw, ok := a["calls"].([]any)
	if !ok || len(raw) == 0 {
		return ErrorResult("calls 必须是非空数组"), nil
	}
	if len(raw) > 32 {
		return ErrorResult("calls 最多 32 个，当前 %d", len(raw)), nil
	}

	calls := make([]Call, 0, len(raw))
	var rejected []string
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return ErrorResult("calls[%d] 必须是对象", i), nil
		}
		ca := Args(m)
		name, err := ca.Required("tool")
		if err != nil {
			return ErrorResult("calls[%d]: %v", i, err), nil
		}
		if name == t.Name() {
			return ErrorResult("calls[%d]: 不允许嵌套 parallel", i), nil
		}
		tool, found := t.Registry.Get(name)
		if !found {
			return ErrorResult("calls[%d]: 未知工具 %q", i, name), nil
		}
		if !tool.ConcurrencySafe() {
			rejected = append(rejected, name)
			continue
		}
		if mut, ok := tool.(Mutation); ok && mut.Mutates() {
			rejected = append(rejected, name)
			continue
		}
		args, _ := ca["args"].(map[string]any)
		calls = append(calls, Call{ID: fmt.Sprintf("p%d", i), Name: name, Args: args})
	}
	if len(rejected) > 0 {
		return ErrorResult("以下工具不能并行执行（会修改状态）：%s。请单独调用它们。", strings.Join(rejected, ", ")), nil
	}

	ex := &Executor{Registry: t.Registry, MaxParallel: t.MaxParallel, Mode: ModeParallel}
	results := ex.Execute(ctx, calls)

	var b strings.Builder
	failed := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		status := "ok"
		if r.Err != nil || r.Result.IsError {
			status = "error"
			failed++
		}
		fmt.Fprintf(&b, "=== [%d] %s (%s, %s) ===\n", i, r.Call.Name, status, r.Duration.Round(1e6))
		body := r.Result.Text()
		body, _ = Truncate(body, 400, 16<<10) // 单项预算，避免聚合爆炸
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: b.String()}},
		Details: map[string]any{"calls": len(results), "failed": failed},
		IsError: failed == len(results),
	}, nil
}

// ---------------------------------------------------------------------------
// todo
// ---------------------------------------------------------------------------

// Todo 是一条任务项。
type Todo struct {
	Content  string `json:"content"`
	Status   string `json:"status"` // pending | in_progress | completed
	Priority string `json:"priority,omitempty"`
}

// TodoTool 管理本会话的任务清单（内存态）。
type TodoTool struct {
	mu    sync.Mutex
	todos []Todo
}

// NewTodoTool 创建 todo 工具。
func NewTodoTool() *TodoTool { return &TodoTool{} }

func (*TodoTool) Name() string { return "todo" }
func (*TodoTool) Description() string {
	return "维护任务清单，帮助跟踪多步工作的进度。action=replace 覆盖整份清单，add 追加，update 改状态，list 查看。" +
		"同时最多一个 in_progress。"
}
func (*TodoTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"action": PropEnum("string", "replace | add | update | list", "replace", "add", "update", "list"),
		"todos": map[string]any{
			"type":        "array",
			"description": "replace/add 时的任务列表",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content":  Prop("string", "任务描述"),
					"status":   PropEnum("string", "状态", "pending", "in_progress", "completed"),
					"priority": PropEnum("string", "优先级", "high", "medium", "low"),
				},
				"required":             []string{"content"},
				"additionalProperties": false,
			},
		},
		"index":  PropInt("update 时的下标（0 起）", -1),
		"status": PropEnum("string", "update 时的新状态", "pending", "in_progress", "completed"),
	}, "action")
}
func (*TodoTool) ConcurrencySafe() bool { return false }
func (*TodoTool) Mutates() bool         { return false }

func (t *TodoTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	action := a.Str("action", "list")
	t.mu.Lock()
	defer t.mu.Unlock()

	switch action {
	case "replace":
		items, err := parseTodos(a["todos"])
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		t.todos = items
	case "add":
		items, err := parseTodos(a["todos"])
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		t.todos = append(t.todos, items...)
	case "update":
		idx := int(a.Int("index", -1))
		if idx < 0 || idx >= len(t.todos) {
			return ErrorResult("index 越界（当前 %d 项）", len(t.todos)), nil
		}
		if s, ok := a.String("status"); ok && s != "" {
			switch s {
			case "pending", "in_progress", "completed":
				t.todos[idx].Status = s
			default:
				return ErrorResult("status 只能是 pending/in_progress/completed"), nil
			}
		}
	case "list":
	}

	// 输出
	if len(t.todos) == 0 {
		return Result{Content: []Block{{Kind: KindText, Text: "(任务清单为空)"}}}, nil
	}
	var b strings.Builder
	pending, done, doing := 0, 0, 0
	for i, td := range t.todos {
		mark := "[ ]"
		switch td.Status {
		case "completed":
			mark = "[x]"
			done++
		case "in_progress":
			mark = "[~]"
			doing++
		default:
			pending++
		}
		fmt.Fprintf(&b, "%d. %s %s", i+1, mark, td.Content)
		if td.Priority != "" {
			fmt.Fprintf(&b, " (%s)", td.Priority)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n进度: %d 完成 / %d 进行中 / %d 待办", done, doing, pending)
	return Result{
		Content: []Block{{Kind: KindText, Text: b.String()}},
		Details: map[string]any{"todos": t.todos},
	}, nil
}

func parseTodos(v any) ([]Todo, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("todos 必须是数组")
	}
	out := make([]Todo, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("todos[%d] 必须是对象", i)
		}
		aa := Args(m)
		content, err := aa.Required("content")
		if err != nil {
			return nil, fmt.Errorf("todos[%d]: %v", i, err)
		}
		status := aa.Str("status", "pending")
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return nil, fmt.Errorf("todos[%d]: status 非法 %q", i, status)
		}
		out = append(out, Todo{Content: content, Status: status, Priority: aa.Str("priority", "")})
	}
	return out, nil
}
