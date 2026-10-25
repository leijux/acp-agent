package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTool 用于调度测试：可控制耗时、并发安全性与副作用。
type fakeTool struct {
	name    string
	safe    bool
	mutates bool
	delay   time.Duration
	fn      func(ctx context.Context, a Args) (Result, error)
}

func (f *fakeTool) Name() string               { return f.name }
func (f *fakeTool) Description() string        { return "fake" }
func (f *fakeTool) ConcurrencySafe() bool      { return f.safe }
func (f *fakeTool) Mutates() bool              { return f.mutates }
func (f *fakeTool) Parameters() map[string]any { return Schema(nil) }
func (f *fakeTool) Execute(ctx context.Context, a Args) (Result, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if f.fn != nil {
		return f.fn(ctx, a)
	}
	return TextResult("%s:%v", f.name, a["x"]), nil
}

// concurrencyMeter 记录并发峰值与当前并发。
type concurrencyMeter struct {
	cur, peak int32
	mu        sync.Mutex
	order     []string
}

func (m *concurrencyMeter) enter(name string) {
	n := atomic.AddInt32(&m.cur, 1)
	for {
		old := atomic.LoadInt32(&m.peak)
		if n <= old || atomic.CompareAndSwapInt32(&m.peak, old, n) {
			break
		}
	}
	m.mu.Lock()
	m.order = append(m.order, "enter:"+name)
	m.mu.Unlock()
}

func (m *concurrencyMeter) leave(name string) {
	atomic.AddInt32(&m.cur, -1)
	m.mu.Lock()
	m.order = append(m.order, "leave:"+name)
	m.mu.Unlock()
}

func (m *concurrencyMeter) Peak() int32 { return atomic.LoadInt32(&m.peak) }

func newMeteredRegistry(m *concurrencyMeter, tools ...*fakeTool) *Registry {
	r := NewRegistry()
	for _, t := range tools {
		t := t
		inner := t.fn
		t.fn = func(ctx context.Context, a Args) (Result, error) {
			m.enter(t.name)
			defer m.leave(t.name)
			time.Sleep(t.delay)
			if inner != nil {
				return inner(ctx, a)
			}
			return TextResult("%s", t.name), nil
		}
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
	return r
}

// TestExecutorRunsSafeToolsInParallel 验证连续只读工具确实并发。
func TestExecutorRunsSafeToolsInParallel(t *testing.T) {
	m := &concurrencyMeter{}
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c", "d"} {
		r.MustRegister(&fakeTool{name: n, safe: true, delay: 80 * time.Millisecond})
	}
	_ = m
	ex := NewExecutor(r, 8)
	start := time.Now()
	results := ex.Execute(context.Background(), []Call{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
	})
	elapsed := time.Since(start)

	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("call %d 失败: %v", i, res.Err)
		}
		if !res.Parallel {
			t.Fatalf("call %d 未被标记为并行执行", i)
		}
	}
	// 4×80ms 串行需 320ms；并行应接近 80ms。留足余量避免 CI 抖动。
	if elapsed > 250*time.Millisecond {
		t.Fatalf("4 个 80ms 的只读工具耗时 %v，没有并发执行", elapsed)
	}
}

// TestExecutorFencesMutatingTools 验证改动型工具形成栅栏并独占执行。
func TestExecutorFencesMutatingTools(t *testing.T) {
	var running int32
	var maxWhileMutating int32
	mut := &fakeTool{
		name: "write", safe: false, mutates: true,
		fn: func(ctx context.Context, a Args) (Result, error) {
			atomic.StoreInt32(&running, 1)
			time.Sleep(40 * time.Millisecond)
			atomic.StoreInt32(&running, 0)
			return TextResult("ok"), nil
		},
	}
	spy := &fakeTool{
		name: "read", safe: true,
		fn: func(ctx context.Context, a Args) (Result, error) {
			if n := atomic.LoadInt32(&running); n > 0 {
				atomic.StoreInt32(&maxWhileMutating, 1)
			}
			time.Sleep(10 * time.Millisecond)
			return TextResult("ok"), nil
		},
	}
	r := NewRegistry()
	r.MustRegister(mut, spy)

	ex := NewExecutor(r, 8)
	results := ex.Execute(context.Background(), []Call{
		{Name: "write"}, {Name: "read"}, {Name: "read"},
	})
	for _, res := range results {
		if res.Err != nil {
			t.Fatalf("失败: %v", res.Err)
		}
	}
	if atomic.LoadInt32(&maxWhileMutating) != 0 {
		t.Fatal("只读工具与改动型工具发生了重叠执行（栅栏失效）")
	}
	if results[0].Parallel {
		t.Fatal("改动型工具不应被标记为并行")
	}
	if !results[1].Parallel || !results[2].Parallel {
		t.Fatal("write 之后的两个 read 应该并行")
	}
}

// TestExecutorPreservesOrder 结果顺序必须与调用顺序一致。
func TestExecutorPreservesOrder(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 10; i++ {
		i := i
		r.MustRegister(&fakeTool{
			name: fmt.Sprintf("t%d", i), safe: true,
			delay: time.Duration(10-i) * time.Millisecond, // 故意逆序完成
			fn: func(ctx context.Context, a Args) (Result, error) {
				return TextResult("%d", i), nil
			},
		})
	}
	var calls []Call
	for i := 0; i < 10; i++ {
		calls = append(calls, Call{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("t%d", i)})
	}
	ex := NewExecutor(r, 16)
	results := ex.Execute(context.Background(), calls)
	for i, res := range results {
		if res.Call.ID != fmt.Sprintf("c%d", i) {
			t.Fatalf("结果顺序错乱：位置 %d 是 %s", i, res.Call.ID)
		}
		if got := res.Result.Text(); got != fmt.Sprintf("%d", i) {
			t.Fatalf("位置 %d 内容不匹配：%q", i, got)
		}
	}
}

// TestExecutorMaxParallel 并发度不超过上限。
func TestExecutorMaxParallel(t *testing.T) {
	m := &concurrencyMeter{}
	var tools []*fakeTool
	for i := 0; i < 12; i++ {
		tools = append(tools, &fakeTool{name: fmt.Sprintf("p%d", i), safe: true, delay: 30 * time.Millisecond})
	}
	r := newMeteredRegistry(m, tools...)
	ex := NewExecutor(r, 3)
	var calls []Call
	for i := 0; i < 12; i++ {
		calls = append(calls, Call{Name: fmt.Sprintf("p%d", i)})
	}
	ex.Execute(context.Background(), calls)
	if peak := m.Peak(); peak > 3 {
		t.Fatalf("并发峰值 %d 超过上限 3", peak)
	}
	if peak := m.Peak(); peak < 2 {
		t.Fatalf("并发峰值只有 %d，MaxParallel 未生效", peak)
	}
}

// TestModeSequential 串行模式下并发峰值为 1。
func TestModeSequential(t *testing.T) {
	m := &concurrencyMeter{}
	var tools []*fakeTool
	for i := 0; i < 5; i++ {
		tools = append(tools, &fakeTool{name: fmt.Sprintf("s%d", i), safe: true, delay: 10 * time.Millisecond})
	}
	r := newMeteredRegistry(m, tools...)
	ex := NewExecutor(r, 8)
	ex.Mode = ModeSequential
	var calls []Call
	for i := 0; i < 5; i++ {
		calls = append(calls, Call{Name: fmt.Sprintf("s%d", i)})
	}
	ex.Execute(context.Background(), calls)
	if peak := m.Peak(); peak != 1 {
		t.Fatalf("串行模式并发峰值 %d，期望 1", peak)
	}
}

// TestExecutorUnknownTool 未知工具返回错误但不中断其它调用。
func TestExecutorUnknownTool(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&fakeTool{name: "ok", safe: true})
	ex := NewExecutor(r, 4)
	results := ex.Execute(context.Background(), []Call{
		{Name: "ok"}, {Name: "nope"}, {Name: "ok"},
	})
	if !results[1].Result.IsError {
		t.Fatal("未知工具应返回 IsError")
	}
	if results[0].Err != nil || results[2].Err != nil {
		t.Fatalf("相邻调用被影响: %v %v", results[0].Err, results[2].Err)
	}
}

// TestExecutorRecoversPanic panic 被捕获并转为错误结果。
func TestExecutorRecoversPanic(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&fakeTool{
		name: "boom", safe: true,
		fn: func(ctx context.Context, a Args) (Result, error) {
			panic("炸了")
		},
	}, &fakeTool{name: "ok", safe: true})
	ex := NewExecutor(r, 4)
	results := ex.Execute(context.Background(), []Call{{Name: "boom"}, {Name: "ok"}})
	if results[0].Err == nil {
		t.Fatal("panic 未被转为 error")
	}
	if results[1].Err != nil {
		t.Fatalf("panic 影响了同批其它调用: %v", results[1].Err)
	}
}

// TestExecutorContextCancel 取消后未开始的调用立即返回。
func TestExecutorContextCancel(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&fakeTool{
		name: "slow", safe: false, mutates: true, delay: 2 * time.Second,
		fn: func(ctx context.Context, a Args) (Result, error) {
			return Result{}, ctx.Err()
		},
	})
	ex := NewExecutor(r, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	results := ex.Execute(ctx, []Call{{Name: "slow"}, {Name: "slow"}})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消未及时生效，耗时 %v", elapsed)
	}
	if results[0].Err == nil && results[1].Err == nil {
		t.Fatal("超时后应至少有一个调用报错")
	}
}

// TestParallelToolRejectsMutations parallel 元工具必须拒绝写类工具。
func TestParallelToolRejectsMutations(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&fakeTool{name: "read", safe: true}, &fakeTool{name: "write", safe: false, mutates: true})
	pt := NewParallelTool(r, 4)
	res, err := pt.Execute(context.Background(), Args{
		"calls": []any{
			map[string]any{"tool": "read", "args": map[string]any{}},
			map[string]any{"tool": "write", "args": map[string]any{}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("包含写操作的 parallel 应被拒绝")
	}
	if !strings.Contains(res.Text(), "write") {
		t.Fatalf("错误信息未指明违规工具: %s", res.Text())
	}
}

// TestParallelToolRunsConcurrently parallel 元工具并发执行多个调用。
func TestParallelToolRunsConcurrently(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 4; i++ {
		r.MustRegister(&fakeTool{name: fmt.Sprintf("r%d", i), safe: true, delay: 60 * time.Millisecond})
	}
	pt := NewParallelTool(r, 4)
	calls := make([]any, 0, 4)
	for i := 0; i < 4; i++ {
		calls = append(calls, map[string]any{"tool": fmt.Sprintf("r%d", i), "args": map[string]any{}})
	}
	start := time.Now()
	res, err := pt.Execute(context.Background(), Args{"calls": calls})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应失败: %s", res.Text())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("parallel 元工具耗时 %v，没有并发", elapsed)
	}
}

// TestParallelToolNestingForbidden 禁止嵌套 parallel。
func TestParallelToolNestingForbidden(t *testing.T) {
	r := NewRegistry()
	pt := NewParallelTool(r, 4)
	r.MustRegister(pt)
	res, _ := pt.Execute(context.Background(), Args{
		"calls": []any{map[string]any{"tool": "parallel", "args": map[string]any{}}},
	})
	if !res.IsError {
		t.Fatal("嵌套 parallel 应被拒绝")
	}
}

// TestParallelHelper 便捷函数并发。
func TestParallelHelper(t *testing.T) {
	tool := &fakeTool{name: "job", safe: true, delay: 50 * time.Millisecond}
	args := []Args{{"x": 1}, {"x": 2}, {"x": 3}, {"x": 4}}
	start := time.Now()
	results := Parallel(context.Background(), tool, args, 4)
	elapsed := time.Since(start)
	if len(results) != 4 {
		t.Fatalf("结果数 %d", len(results))
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Parallel 未并发，耗时 %v", elapsed)
	}
	if results[2].Result.Text() != "job:3" {
		t.Fatalf("结果顺序错误: %q", results[2].Result.Text())
	}
}

// TestRegistryDuplicate 重复注册报错。
func TestRegistryDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeTool{name: "x", safe: true}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := r.Register(&fakeTool{name: "x", safe: true}); err == nil {
		t.Fatal("重复注册应报错")
	}
}

// TestRegistrySpecsStable Specs 顺序与注册顺序一致。
func TestRegistrySpecsStable(t *testing.T) {
	r := NewRegistry()
	names := []string{"b", "a", "c"}
	for _, n := range names {
		r.MustRegister(&fakeTool{name: n, safe: true})
	}
	specs := r.Specs()
	for i, s := range specs {
		if s.Name != names[i] {
			t.Fatalf("顺序错乱：%v vs %v", s.Name, names[i])
		}
	}
}

// TestExecutorEmptyCalls 空输入安全。
func TestExecutorEmptyCalls(t *testing.T) {
	ex := NewExecutor(NewRegistry(), 4)
	if got := ex.Execute(context.Background(), nil); len(got) != 0 {
		t.Fatalf("空调用返回了 %d 个结果", len(got))
	}
}
