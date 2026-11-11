package tools

// subagent_output / subagent_kill：后台子代理任务的读取与终止。
//
// 与 bash_output / bash_kill 一一对应 —— 模型已经熟悉这套「后台任务 + 轮询/等待」
// 的用法，子代理沿用同一形状，不用重新学。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SubagentOutputTool 读取后台子代理任务的结论。
type SubagentOutputTool struct{ Manager *SubagentManager }

func NewSubagentOutputTool(m *SubagentManager) *SubagentOutputTool {
	return &SubagentOutputTool{Manager: m}
}

func (*SubagentOutputTool) Name() string { return "subagent_output" }
func (*SubagentOutputTool) Description() string {
	return "读取 delegate 起的后台子代理任务。不给 id 时列出全部任务。" +
		"wait=exit 会阻塞到任务结束（比反复轮询省 token），默认立即返回当前状态。" +
		"任务结束后结论只有第一次读时会完整返回，之后仍可反复读（文本不会被消费）。"
}
func (*SubagentOutputTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"id":        Prop("string", "delegate 返回的 jobId（如 sub-1）；留空列出全部任务"),
		"wait":      Prop("string", "none（默认，立即返回）| exit（等到任务结束）"),
		"timeoutMs": PropInt("等待超时毫秒（exit 默认 120000，上限 300000）", 0),
	})
}

// ConcurrencySafe 为 true：只读任务状态，多个子任务可以同时等。
func (*SubagentOutputTool) ConcurrencySafe() bool { return true }
func (*SubagentOutputTool) Mutates() bool         { return false }

func (t *SubagentOutputTool) Execute(ctx context.Context, a Args) (Result, error) {
	if t.Manager == nil {
		return ErrorResult("subagent_output 未启用（宿主未注入子代理管理器）"), nil
	}
	id := strings.TrimSpace(a.Str("id", ""))
	if id == "" {
		jobs := t.Manager.List()
		if len(jobs) == 0 {
			return Result{Content: []Block{{Kind: KindText, Text: "（没有后台子代理任务）"}}}, nil
		}
		var b strings.Builder
		b.WriteString("后台子代理任务：\n")
		for _, j := range jobs {
			fmt.Fprintf(&b, "- %s [%s] %s · %s · %s\n", j.ID, j.Role, statusText(j),
				fmtDuration(j.Elapsed()), clip(j.Task, 100))
		}
		return Result{
			Content: []Block{{Kind: KindText, Text: b.String()}},
			Details: map[string]any{"jobs": len(jobs)},
		}, nil
	}

	until, err := parseWaitUntil(a.Str("wait", ""))
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	timeoutMs := a.Int("timeoutMs", 0)
	if timeoutMs <= 0 {
		timeoutMs = int64(defaultSubagentWait / time.Millisecond)
	}
	if timeoutMs > maxWaitTimeoutMs {
		timeoutMs = maxWaitTimeoutMs
	}
	job, finished, werr := t.Manager.Wait(ctx, id, until, time.Duration(timeoutMs)*time.Millisecond)
	if werr != nil {
		if job.ID == "" {
			return ErrorResult("%v", werr), nil
		}
		// ctx 取消不算失败：把当前快照交回去。
		finished = false
	}
	// 结论给足一点（24KB，与 delegate wait=true 一致），别让模型为了补细节再等一轮。
	text := renderSubagentJob(job, 24<<10)
	if until != WaitNone && !finished {
		text += fmt.Sprintf("（等待超时 %s，任务仍在运行：可再调一次带 wait=exit）\n", time.Duration(timeoutMs)*time.Millisecond)
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: text}},
		Details: map[string]any{
			"id": job.ID, "role": job.Role, "running": job.Running,
			"done": finished, "cancelled": job.Cancelled, "err": job.Err,
		},
	}, nil
}

// SubagentKillTool 终止后台子代理任务。
type SubagentKillTool struct{ Manager *SubagentManager }

func NewSubagentKillTool(m *SubagentManager) *SubagentKillTool {
	return &SubagentKillTool{Manager: m}
}

func (*SubagentKillTool) Name() string { return "subagent_kill" }
func (*SubagentKillTool) Description() string {
	return "终止一个后台子代理任务（不给 id 时终止全部）。已结束的任务不受影响。"
}
func (*SubagentKillTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"id":  Prop("string", "要终止的 jobId；留空终止全部"),
		"all": Prop("boolean", "true = 终止全部任务"),
	})
}

func (*SubagentKillTool) ConcurrencySafe() bool { return false }
func (*SubagentKillTool) Mutates() bool         { return true }

func (t *SubagentKillTool) Execute(_ context.Context, a Args) (Result, error) {
	if t.Manager == nil {
		return ErrorResult("subagent_kill 未启用（宿主未注入子代理管理器）"), nil
	}
	id := strings.TrimSpace(a.Str("id", ""))
	if id == "" || a.Bool("all", false) {
		t.Manager.KillAll()
		return Result{Content: []Block{{Kind: KindText, Text: "已终止全部后台子代理任务。"}}}, nil
	}
	if err := t.Manager.Kill(id); err != nil {
		return ErrorResult("%v", err), nil
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: fmt.Sprintf("已终止 %s。", id)}},
		Details: map[string]any{"id": id},
	}, nil
}
