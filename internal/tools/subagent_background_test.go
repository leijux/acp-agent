package tools

// 后台子代理（delegate / subagent_output / subagent_kill）的测试。
//
// 核心约束：delegate 必须**立刻返回**（不阻塞主代理的 turn），结论靠
// subagent_output 取；任务的生命周期不绑定调用方的 ctx。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// blockingRunner 返回一个可手动放行的 runner，并记录被取消的次数。
func blockingRunner() (*SubagentManager, chan struct{}, *int32) {
	release := make(chan struct{})
	var cancelled int32
	mgr := NewSubagentManager(func(ctx context.Context, role, task string) (string, error) {
		select {
		case <-release:
			return "结论：" + role + "/" + task, nil
		case <-ctx.Done():
			atomic.AddInt32(&cancelled, 1)
			return "", ctx.Err()
		}
	})
	return mgr, release, &cancelled
}

// TestDelegateReturnsImmediately delegate 默认后台执行，不等子代理结束。
func TestDelegateReturnsImmediately(t *testing.T) {
	mgr, release, _ := blockingRunner()
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}}

	start := time.Now()
	res, err := tool.Execute(ctx_(), Args{"agent": "worker", "task": "写个测试"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("delegate 阻塞了 %v（应立刻返回）", d)
	}
	if res.IsError {
		t.Fatalf("delegate 失败: %s", res.Text())
	}
	ids, _ := res.Details.(map[string]any)["ids"].([]string)
	if len(ids) != 1 {
		t.Fatalf("应返回 1 个 jobId，实际 %v", res.Details)
	}
	// 子代理还没结束：任务仍在跑。
	if job, ok := mgr.Get(ids[0]); !ok || !job.Running {
		t.Fatalf("任务应处于 running: %+v", job)
	}

	close(release)
	out := &SubagentOutputTool{Manager: mgr}
	ores, err := out.Execute(ctx_(), Args{"id": ids[0], "wait": "exit", "timeoutMs": 2000})
	if err != nil {
		t.Fatalf("subagent_output: %v", err)
	}
	if !strings.Contains(ores.Text(), "结论：worker/写个测试") {
		t.Fatalf("结论未取回: %q", ores.Text())
	}
	if !strings.Contains(ores.Text(), "done") {
		t.Fatalf("状态应为 done: %q", ores.Text())
	}
}

// TestDelegateWaitBlocks wait=true 保持旧的阻塞语义。
func TestDelegateWaitBlocks(t *testing.T) {
	mgr := NewSubagentManager(func(ctx context.Context, role, task string) (string, error) {
		return "立即返回的结论", nil
	})
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}}
	res, err := tool.Execute(ctx_(), Args{"agent": "worker", "task": "t", "wait": true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError || !strings.Contains(res.Text(), "立即返回的结论") {
		t.Fatalf("wait=true 应直接带回结论: %s", res.Text())
	}
}

// TestDelegateSurvivesTurnCancel 任务不能被「本轮 ctx 取消」连坐杀掉。
func TestDelegateSurvivesTurnCancel(t *testing.T) {
	mgr, release, cancelled := blockingRunner()
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}}

	ctx, cancel := context.WithCancel(ctx_())
	res, err := tool.Execute(ctx, Args{"agent": "worker", "task": "慢任务"})
	if err != nil || res.IsError {
		t.Fatalf("delegate: %v %s", err, res.Text())
	}
	cancel() // 模拟 turn 结束 / 用户 Esc
	time.Sleep(50 * time.Millisecond)

	id := res.Details.(map[string]any)["ids"].([]string)[0]
	if job, _ := mgr.Get(id); !job.Running {
		t.Fatalf("turn 取消不应杀掉后台任务: %+v", job)
	}
	if got := atomic.LoadInt32(cancelled); got != 0 {
		t.Fatalf("runner 被取消了 %d 次（不该发生）", got)
	}
	close(release)
}

// TestSubagentKill 终止后状态是 killed，且不会当成失败。
func TestSubagentKill(t *testing.T) {
	mgr, _, cancelled := blockingRunner()
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}}
	res, _ := tool.Execute(ctx_(), Args{"agent": "worker", "task": "长任务"})
	id := res.Details.(map[string]any)["ids"].([]string)[0]

	kill := &SubagentKillTool{Manager: mgr}
	if kres, err := kill.Execute(ctx_(), Args{"id": id}); err != nil || kres.IsError {
		t.Fatalf("subagent_kill: %v %s", err, kres.Text())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, _ := mgr.Get(id); !job.Running {
			if !job.Cancelled {
				t.Fatalf("终止后应标记 cancelled: %+v", job)
			}
			if atomic.LoadInt32(cancelled) != 1 {
				t.Fatalf("runner 应收到 1 次取消，实际 %d", atomic.LoadInt32(cancelled))
			}
			out := &SubagentOutputTool{Manager: mgr}
			ores, _ := out.Execute(ctx_(), Args{"id": id})
			if !strings.Contains(ores.Text(), "killed") {
				t.Fatalf("状态应显示 killed: %q", ores.Text())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("kill 后任务仍未结束")
}

// TestSubagentOutputList 不给 id 时列出全部任务。
func TestSubagentOutputList(t *testing.T) {
	mgr, release, _ := blockingRunner()
	defer close(release)
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}}
	if _, err := tool.Execute(ctx_(), Args{"tasks": []any{
		map[string]any{"agent": "worker", "task": "a"},
		map[string]any{"agent": "worker", "task": "b"},
	}}); err != nil {
		t.Fatal(err)
	}
	out := &SubagentOutputTool{Manager: mgr}
	res, err := out.Execute(ctx_(), Args{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	text := res.Text()
	if !strings.Contains(text, "sub-1") || !strings.Contains(text, "sub-2") {
		t.Fatalf("应列出两个任务: %q", text)
	}
}

// TestDelegateBatchLimits 一次提交超过 MaxParallel 要报错而不是起一堆进程。
func TestDelegateBatchLimits(t *testing.T) {
	mgr, release, _ := blockingRunner()
	defer close(release)
	tool := &SubagentTool{Manager: mgr, Agents: []string{"worker"}, MaxParallel: 2}
	items := make([]any, 3)
	for i := range items {
		items[i] = map[string]any{"agent": "worker", "task": "t"}
	}
	res, err := tool.Execute(ctx_(), Args{"tasks": items})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("超限应报错: %s", res.Text())
	}
}

// TestSubagentToolsDisabledWithDelegate delegate 被禁用时 output/kill 一起关，
// 否则子代理（--disable-tools delegate）会看到一对永远没任务的工具。
func TestSubagentToolsDisabledWithDelegate(t *testing.T) {
	p := testPolicy(t)
	r, _ := DefaultRegistry(Options{Policy: p, Disable: []string{"delegate"}})
	for _, name := range []string{"delegate", "subagent_output", "subagent_kill"} {
		if _, ok := r.Get(name); ok {
			t.Fatalf("禁用 delegate 后不应注册 %q", name)
		}
	}
}

// TestDefaultRegistryHasSubagentTools 正常情况下三个工具都在。
func TestDefaultRegistryHasSubagentTools(t *testing.T) {
	p := testPolicy(t)
	r, _ := DefaultRegistry(Options{Policy: p})
	for _, name := range []string{"delegate", "subagent_output", "subagent_kill"} {
		if _, ok := r.Get(name); !ok {
			t.Fatalf("缺少工具 %q", name)
		}
	}
}

// TestRegistryCloseRunsCleanups 回收函数：逆序执行、只跑一次、连带 base。
//
// “连带 base”是关键：ACP/agent 拿到的往往是包过权限门禁的视图（UseBase），
// manager 登记在底层 registry 上，不转发就会留下孤儿子进程。
func TestRegistryCloseRunsCleanups(t *testing.T) {
	base := NewRegistry()
	var order []string
	base.AddCleanup(func() { order = append(order, "bash") })
	base.AddCleanup(func() { order = append(order, "subagent") })

	view := NewRegistry()
	view.UseBase(base, nil)
	view.AddCleanup(func() { order = append(order, "view") })

	view.Close()
	view.Close() // 幂等：cleanupOnce 挡住

	want := []string{"view", "subagent", "bash"}
	if len(order) != len(want) {
		t.Fatalf("cleanup 次数/内容不对: %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cleanup 顺序 = %v, want %v", order, want)
		}
	}
}
