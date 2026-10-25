package tools

import "testing"

// TestRegistryViewSeesBaseChanges 回归：MCP 的 tools/list_changed 必须穿透到视图。
//
// 早先 wrapRegistry 把 base 逐项拷贝成快照，于是新增的 MCP 工具模型永远看不到，
// 服务端撤回的工具仍留在视图里可被调用（调用后必然失败）。
func TestRegistryViewSeesBaseChanges(t *testing.T) {
	base := NewRegistry()
	mustRegister(t, base, "read")

	view := NewRegistry()
	view.UseBase(base, func(t Tool) Tool { return t }) // 这里不解包，只验证可见性

	if _, ok := view.Get("late"); ok {
		t.Fatal("未注册的工具不应可见")
	}
	if got := len(view.Names()); got != 1 {
		t.Fatalf("视图工具数 %d，期望 1", got)
	}

	// 动态新增（模拟 tools/list_changed）
	mustRegister(t, base, "late")
	if _, ok := view.Get("late"); !ok {
		t.Fatal("base 上新增的工具对视图不可见（快照语义）")
	}
	if got := len(view.Specs()); got != 2 {
		t.Fatalf("Specs 数量 %d，期望 2", got)
	}

	// 动态撤回
	base.Unregister("late")
	if _, ok := view.Get("late"); ok {
		t.Fatal("base 上撤回的工具仍可从视图调用（快照语义）")
	}
	if got := len(view.Names()); got != 1 {
		t.Fatalf("撤回后视图工具数 %d，期望 1", got)
	}
}

// TestRegistryViewOwnToolsTakePrecedence 自己注册的工具优先于 base。
func TestRegistryViewOwnToolsTakePrecedence(t *testing.T) {
	base := NewRegistry()
	mustRegister(t, base, "read", "grep")

	view := NewRegistry()
	view.UseBase(base, func(t Tool) Tool { return t })
	mustRegister(t, view, "context_check") // agent 在门禁之后注册的工具

	names := view.Names()
	if len(names) != 3 {
		t.Fatalf("工具数 %d，期望 3: %v", len(names), names)
	}
	if names[0] != "context_check" {
		t.Errorf("自己的工具应排在前面: %v", names)
	}
	// 自己注册的不应被 base 覆盖
	if _, ok := view.Get("context_check"); !ok {
		t.Error("自己注册的工具查不到")
	}
}

// TestRegistryViewWrapsOnLookup 每次 Get 都要经过 wrap（门禁不能只在拷贝时生效一次）。
func TestRegistryViewWrapsOnLookup(t *testing.T) {
	base := NewRegistry()
	mustRegister(t, base, "read")

	var calls int
	view := NewRegistry()
	view.UseBase(base, func(t Tool) Tool {
		calls++
		return t
	})
	view.Get("read")
	view.Get("read")
	if calls != 2 {
		t.Fatalf("wrap 被调用 %d 次，期望 2（每次查找都包装）", calls)
	}
}

func mustRegister(t *testing.T, r *Registry, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := r.Register(&fakeTool{name: n, safe: true}); err != nil {
			t.Fatalf("Register(%s): %v", n, err)
		}
	}
}
