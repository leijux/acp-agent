package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Registry 持有已注册工具。
//
// 支持两层：自己注册的工具（可增删）+ 一个可选的 base 视图。
// base 视图是「动态」的 —— 查自己没有的名字时实时去 base 取并用 wrap 包一层，
// 因此 base 上新增/删除的工具立刻可见。MCP 的 tools/list_changed 依赖这个语义。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string

	// base 非 nil 时作为回退来源；wrap 为 nil 表示原样返回。
	base *Registry
	wrap func(Tool) Tool
}

// UseBase 把 r 变成 base 的包装视图。
//
// 自己注册的工具优先（例如 agent 在门禁之后注册的 context_* 工具），
// 其余名字实时查 base 并套 wrap。
//
// 为什么不拷贝：早先的实现是「逐项复制 + 包装」的快照，于是 MCP server
// 通过 tools/list_changed 新增的工具模型永远看不到，撤回的工具还留在
// 视图里可被调用。改成视图后 base 一变视图就跟着变。
func (r *Registry) UseBase(base *Registry, wrap func(Tool) Tool) {
	if base == r {
		panic("tools: Registry 不能以自己作为 base")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.base = base
	r.wrap = wrap
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册工具；重名返回错误（避免静默覆盖）。
func (r *Registry) Register(ts ...Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range ts {
		name := t.Name()
		if name == "" {
			return errors.New("tools: 工具名为空")
		}
		if _, dup := r.tools[name]; dup {
			return fmt.Errorf("tools: 工具 %q 重复注册", name)
		}
		r.tools[name] = t
		r.order = append(r.order, name)
	}
	return nil
}

// MustRegister 注册并在失败时 panic（用于内置工具组装）。
func (r *Registry) MustRegister(ts ...Tool) *Registry {
	if err := r.Register(ts...); err != nil {
		panic(err)
	}
	return r
}

// Unregister 移除工具（MCP server 可能撤回工具）。不存在的名字被忽略。
func (r *Registry) Unregister(names ...string) {
	if len(names) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	remove := make(map[string]bool, len(names))
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			continue
		}
		delete(r.tools, name)
		remove[name] = true
	}
	if len(remove) == 0 {
		return
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if remove[name] {
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
}

// Get 按名称查找（自己没有时查 base 并套 wrap）。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	t, ok := r.tools[name]
	base, wrapFn := r.base, r.wrap
	r.mu.RUnlock()
	if ok {
		return t, true
	}
	if base == nil {
		return nil, false
	}
	bt, ok := base.Get(name)
	if !ok {
		return nil, false
	}
	if wrapFn != nil {
		return wrapFn(bt), true
	}
	return bt, true
}

// Names 按注册顺序返回工具名：自己的在前，base 独有的在后。
func (r *Registry) Names() []string {
	r.mu.RLock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	base := r.base
	r.mu.RUnlock()
	if base == nil {
		return out
	}
	seen := make(map[string]bool, len(out))
	for _, n := range out {
		seen[n] = true
	}
	for _, n := range base.Names() {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

// Len 返回工具数量（含 base 提供的）。
func (r *Registry) Len() int { return len(r.Names()) }

// Specs 返回面向模型的工具定义列表（顺序稳定）。
//
// 走 Names()+Get()，这样 base 上的动态工具也会出现（包装层不改
// name/description/parameters，所以 Spec 与直连 base 完全一致）。
func (r *Registry) Specs() []Spec {
	names := r.Names()
	out := make([]Spec, 0, len(names))
	for _, name := range names {
		t, ok := r.Get(name)
		if !ok {
			continue
		}
		out = append(out, Spec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// Spec 是喂给 LLM 的工具定义。
type Spec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Call 是一次工具调用请求（来自模型）。
type Call struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ExecResult 是单次调用的执行结果。
type ExecResult struct {
	Call     Call          `json:"call"`
	Result   Result        `json:"result"`
	Err      error         `json:"-"`
	ErrorMsg string        `json:"error,omitempty"` // Err 的文本形式，便于序列化回模型
	Duration time.Duration `json:"duration"`
	// Parallel 表示该调用是否在并发批次内执行（可观测性）。
	Parallel bool `json:"parallel"`
}

// Mode 是调度策略。
type Mode int

const (
	// ModeAuto 按工具的 ConcurrencySafe 自动分批（默认）。
	ModeAuto Mode = iota
	// ModeSequential 全部串行。
	ModeSequential
	// ModeParallel 尽可能并行（仍受 ConcurrencySafe 约束，避免并发写同一文件）。
	ModeParallel
)

// Executor 按 ConcurrencySafe 声明分批执行工具调用。
//
// 调度规则：
//  1. 连续的并发安全工具合成一批，批内并行（受 MaxParallel 限制）；
//  2. 任何可能修改状态的工具（ConcurrencySafe=false）独占一批，前后形成栅栏；
//  3. batch/parallel 这类「元工具」由自身声明为独占，避免嵌套调度混乱；
//  4. 结果顺序始终与输入调用顺序一致，便于与模型消息对齐。
type Executor struct {
	Registry    *Registry
	MaxParallel int
	Mode        Mode
	// OnStart/OnDone 可用于进度上报（可选）。
	OnStart func(call Call)
	OnDone  func(res ExecResult)
}

// NewExecutor 创建执行器；maxParallel<=0 时取 GOMAXPROCS*2。
func NewExecutor(r *Registry, maxParallel int) *Executor {
	if maxParallel <= 0 {
		maxParallel = runtime.GOMAXPROCS(0) * 2
	}
	return &Executor{Registry: r, MaxParallel: maxParallel, Mode: ModeAuto}
}

// Execute 执行一批调用。返回切片与 calls 一一对应。
func (e *Executor) Execute(ctx context.Context, calls []Call) []ExecResult {
	results := make([]ExecResult, len(calls))
	for i, c := range calls {
		results[i] = ExecResult{Call: c}
	}
	if len(calls) == 0 {
		return results
	}

	type group struct {
		indices  []int
		parallel bool
	}
	var groups []group

	if e.Mode == ModeSequential {
		for i := range calls {
			groups = append(groups, group{indices: []int{i}})
		}
	} else {
		i := 0
		for i < len(calls) {
			safe := e.safeCall(calls[i])
			if !safe {
				groups = append(groups, group{indices: []int{i}})
				i++
				continue
			}
			j := i
			var batch []int
			for j < len(calls) && e.safeCall(calls[j]) {
				batch = append(batch, j)
				j++
			}
			groups = append(groups, group{indices: batch, parallel: e.Mode == ModeParallel || len(batch) > 1})
			i = j
		}
	}

	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			for _, idx := range g.indices {
				results[idx] = e.canceled(calls[idx], false)
			}
			continue
		}
		if g.parallel && len(g.indices) > 1 {
			e.runParallel(ctx, calls, g.indices, results)
		} else {
			for _, idx := range g.indices {
				results[idx] = e.runOne(ctx, calls[idx], false)
			}
		}
	}
	return results
}

// canceled 把「因取消而没执行」的调用转成模型可见的占位结果。
func (e *Executor) canceled(c Call, parallel bool) ExecResult {
	err := context.Canceled
	return ExecResult{
		Call:     c,
		Err:      err,
		ErrorMsg: err.Error(),
		Result:   ErrorResult("canceled: %v", err),
		Parallel: parallel,
	}
}

func (e *Executor) safeCall(c Call) bool {
	t, ok := e.Registry.Get(c.Name)
	if !ok {
		// 未知工具视为安全：它只会立即返回错误，不值得单独占一个栅栏。
		return true
	}
	if !t.ConcurrencySafe() {
		return false
	}
	if m, ok := t.(Mutation); ok && m.Mutates() {
		// 改动型工具默认独占整批；实现 PathScoped 的工具自带路径级互斥
		// （不同文件真并行、同文件串行），不再需要栅栏。
		return IsPathScoped(t)
	}
	return true
}

func (e *Executor) runParallel(ctx context.Context, calls []Call, indices []int, results []ExecResult) {
	sem := make(chan struct{}, e.MaxParallel)
	var wg sync.WaitGroup
	for _, idx := range indices {
		idx := idx
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 注意：只有真正拿到 token 才登记释放。
			// 之前两个分支共用同一个 defer <-sem，ctx 已取消、一个 token 都没
			// 拿到时会在空的 sem 上永久阻塞，导致 wg.Wait() 再也回不来。
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = e.canceled(calls[idx], true)
				return
			}
			results[idx] = e.runOne(ctx, calls[idx], true)
		}()
	}
	wg.Wait()
}

func (e *Executor) runOne(ctx context.Context, call Call, parallel bool) (out ExecResult) {
	out = ExecResult{Call: call, Parallel: parallel}
	start := time.Now()
	if e.OnStart != nil {
		e.OnStart(call)
	}
	defer func() {
		out.Duration = time.Since(start)
		if r := recover(); r != nil {
			out.Err = fmt.Errorf("工具 %q panic: %v", call.Name, r)
			out.ErrorMsg = out.Err.Error()
			out.Result = ErrorResult("%s", out.ErrorMsg)
		}
		if out.Err != nil {
			out.ErrorMsg = out.Err.Error()
		}
		if e.OnDone != nil {
			e.OnDone(out)
		}
	}()

	t, ok := e.Registry.Get(call.Name)
	if !ok {
		out.Err = fmt.Errorf("未知工具 %q", call.Name)
		out.Result = ErrorResult("unknown tool: %s（可用工具：%v）", call.Name, e.Registry.Names())
		return out
	}
	res, err := t.Execute(ctx, Args(call.Args))
	out.Result, out.Err = res, err
	if err != nil {
		// Go 层错误也转成模型可见结果，避免整轮失败。
		out.Result = ErrorResult("%s: %v", call.Name, err)
	}
	return out
}

// Parallel 便捷函数：对一组相同工具的调用做并发（例如批量 read/glob）。
func Parallel(ctx context.Context, t Tool, argList []Args, maxParallel int) []ExecResult {
	if maxParallel <= 0 {
		maxParallel = runtime.GOMAXPROCS(0) * 2
	}
	results := make([]ExecResult, len(argList))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i := range argList {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			res, err := t.Execute(ctx, argList[i])
			results[i] = ExecResult{
				Call:     Call{ID: fmt.Sprintf("%s-%d", t.Name(), i), Name: t.Name(), Args: argList[i]},
				Result:   res,
				Err:      err,
				Duration: time.Since(start),
				Parallel: true,
			}
			if err != nil {
				results[i].ErrorMsg = err.Error()
			}
		}()
	}
	wg.Wait()
	return results
}

// SortByName 便于稳定输出/测试。
func SortByName(specs []Spec) {
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
}
