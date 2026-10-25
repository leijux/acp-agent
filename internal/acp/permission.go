package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/tools"
)

// Guard 决定一次工具调用是否放行。
//
// 返回 nil 表示允许；返回错误表示拒绝，错误文本会作为工具结果回给模型
// （让模型自己换策略），而不是中断整个 turn。
type Guard interface {
	Authorize(ctx context.Context, toolName string, args map[string]any) error
}

// GuardFunc 让普通函数满足 Guard。
type GuardFunc func(ctx context.Context, toolName string, args map[string]any) error

// Authorize 实现 Guard。
func (f GuardFunc) Authorize(ctx context.Context, toolName string, args map[string]any) error {
	return f(ctx, toolName, args)
}

// 权限门禁的拒绝原因。
var (
	// ErrPermissionDenied 表示用户拒绝本次调用。
	ErrPermissionDenied = errors.New("用户拒绝了该工具调用")
	// ErrPermissionCancelled 表示 turn 被取消，权限请求未决。
	ErrPermissionCancelled = errors.New("turn 已取消，权限请求未决")
	// ErrPermissionTimeout 表示客户端在超时内没有响应。
	//
	// 常见于客户端根本不渲染 session/request_permission：请求发出去无人应答，
	// 没有超时就是无限挂住（整个 turn 卡死，用户只能取消）。
	ErrPermissionTimeout = errors.New("客户端未在超时内响应授权请求")
)

// DefaultPermissionTimeout 是等待客户端授权的默认上限。
const DefaultPermissionTimeout = 60 * time.Second

// PermissionGuard 把 ACP 的 session/request_permission 接到工具调度上。
//
// 状态：允许「总是允许 / 总是拒绝」之后，同名工具后续调用直接按记忆决策，
// 不再打扰用户。记忆只在本次 ACP 会话内有效。
type PermissionGuard struct {
	client    *Client
	sessionID string
	updates   *updates

	mu      sync.Mutex
	allowed map[string]bool
	denied  map[string]bool

	// timeout 是单次等待上限；零值用 DefaultPermissionTimeout。
	timeout time.Duration
	// unresponsive 为 true 表示曾经超时：客户端不处理权限请求。
	// 之后不再浪费时间重试，直接快速失败。
	unresponsive bool
}

// NewPermissionGuard 创建权限门禁。
func NewPermissionGuard(client *Client, sessionID string, u *updates) *PermissionGuard {
	return &PermissionGuard{
		client:    client,
		sessionID: sessionID,
		updates:   u,
		allowed:   make(map[string]bool),
		denied:    make(map[string]bool),
		timeout:   DefaultPermissionTimeout,
	}
}

// SetTimeout 覆盖等待客户端授权的上限（零或负值恢复默认）。
func (g *PermissionGuard) SetTimeout(d time.Duration) *PermissionGuard {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d <= 0 {
		d = DefaultPermissionTimeout
	}
	g.timeout = d
	return g
}

// Authorize 实现 Guard。
func (g *PermissionGuard) Authorize(ctx context.Context, toolName string, args map[string]any) error {
	g.mu.Lock()
	if g.allowed[toolName] {
		g.mu.Unlock()
		return nil
	}
	if g.denied[toolName] {
		g.mu.Unlock()
		return fmt.Errorf("%w（%s 已被设为总是拒绝）", ErrPermissionDenied, toolName)
	}
	if g.unresponsive {
		g.mu.Unlock()
		return fmt.Errorf("%w（%s；该客户端不处理 session/request_permission，请改用 permissions=auto 或 read_only）", ErrPermissionTimeout, toolName)
	}
	wait := g.timeout
	if wait <= 0 {
		wait = DefaultPermissionTimeout
	}
	g.mu.Unlock()

	id := g.updates.takeToolID(toolName)
	tc := ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    id,
		Name:          toolName,
		Title:         toolTitle(toolName, args),
		Kind:          toolKind(toolName),
		RawInput:      args,
	}

	// 关键：请求本身也必须受超时约束。客户端不实现权限 UI 时，
	// 没有这层 ctx 就是永久挂住（既没有错误也没有结果）。
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	outcome, err := g.client.RequestPermission(ctx, g.sessionID, tc, DefaultPermissionOptions())
	if err != nil {
		if parent.Err() != nil {
			return ErrPermissionCancelled
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			g.mu.Lock()
			g.unresponsive = true
			g.mu.Unlock()
			return fmt.Errorf("%w（%s 等待 %s）", ErrPermissionTimeout, toolName, wait)
		}
		return fmt.Errorf("请求权限失败: %w", err)
	}
	switch outcome.Outcome {
	case "cancelled":
		return ErrPermissionCancelled
	case "selected":
		switch optionKind(outcome.OptionID) {
		case PermAllowAlways:
			g.mu.Lock()
			g.allowed[toolName] = true
			g.mu.Unlock()
			return nil
		case PermAllowOnce:
			return nil
		case PermRejectAlways:
			g.mu.Lock()
			g.denied[toolName] = true
			g.mu.Unlock()
			return fmt.Errorf("%w（%s）", ErrPermissionDenied, toolName)
		default:
			return fmt.Errorf("%w（%s）", ErrPermissionDenied, toolName)
		}
	}
	return fmt.Errorf("%w（未知 outcome %q）", ErrPermissionDenied, outcome.Outcome)
}

func optionKind(optionID string) string {
	for _, o := range DefaultPermissionOptions() {
		if o.OptionID == optionID {
			return o.Kind
		}
	}
	return ""
}

// AllowAll 是无条件放行的 Guard（用于无人值守场景）。
func AllowAll() Guard {
	return GuardFunc(func(context.Context, string, map[string]any) error { return nil })
}

// DenyMutating 拒绝所有会修改状态的工具，只放行只读工具。
//
// 用于「只读审查」这类模式：不需要客户端交互，也不需要权限 UI。
func DenyMutating(base *tools.Registry) Guard {
	return GuardFunc(func(_ context.Context, toolName string, _ map[string]any) error {
		r, ok := base.Get(toolName)
		if !ok || r == nil {
			return nil
		}
		if !r.ConcurrencySafe() {
			return fmt.Errorf("%w（%s 会修改状态）", ErrPermissionDenied, toolName)
		}
		if m, ok := r.(tools.Mutation); ok && m.Mutates() {
			return fmt.Errorf("%w（%s 会修改状态）", ErrPermissionDenied, toolName)
		}
		// bash 这类工具调度上可并行、权限上却能改文件（shell 里跑 rm/git/python），
		// 只查 Mutation 会让只读模式形同虚设。
		if pm, ok := r.(tools.PermissionMutation); ok && pm.PermissionMutates() {
			return fmt.Errorf("%w（%s 会修改状态）", ErrPermissionDenied, toolName)
		}
		return nil
	})
}

// WrapRegistry 返回一个把所有工具包上 Guard 的新 Registry。
//
// 注意：Executor 通过 Registry 取工具，所以门禁发生在 Execute 内部，
// 时序是 OnStart（tool_call 更新）→ Authorize（可能阻塞等用户）→ 真正执行。
//
// 已知限制：内置 parallel 工具内部持有的是原始 Registry，其嵌套调用不会
// 经过这里。只读场景无碍；需要严格门禁时请把 parallel 列入 Disable。
func WrapRegistry(base *tools.Registry, g Guard) (*tools.Registry, error) {
	if g == nil {
		return base, nil
	}
	// 一次分配，永不失败：视图在 Get 时实时包装，base 上增删的工具立刻可见。
	out := tools.NewRegistry()
	out.UseBase(base, func(t tools.Tool) tools.Tool { return &guardedTool{inner: t, guard: g} })
	return out, nil
}

// guardedTool 给工具执行加上权限门禁。
//
// 必须继续实现 tools.Mutation，否则 Executor 会忘记「这个工具会改动状态」，
// 从而把改文件的工具排进并发批次。
type guardedTool struct {
	inner tools.Tool
	guard Guard
}

func (t *guardedTool) Name() string               { return t.inner.Name() }
func (t *guardedTool) Description() string        { return t.inner.Description() }
func (t *guardedTool) Parameters() map[string]any { return t.inner.Parameters() }
func (t *guardedTool) ConcurrencySafe() bool      { return t.inner.ConcurrencySafe() }
func (t *guardedTool) PathScoped() bool           { return tools.IsPathScoped(t.inner) }

func (t *guardedTool) Mutates() bool {
	if m, ok := t.inner.(tools.Mutation); ok {
		return m.Mutates()
	}
	return false
}

func (t *guardedTool) Execute(ctx context.Context, args tools.Args) (tools.Result, error) {
	if err := t.guard.Authorize(ctx, t.inner.Name(), map[string]any(args)); err != nil {
		if errors.Is(err, ErrPermissionCancelled) {
			// 取消要真的中断执行，且不计为模型可见的普通错误。
			return tools.ErrorResult("%s", err.Error()), ctx.Err()
		}
		return tools.ErrorResult("权限门禁：%s", err.Error()), nil
	}
	return t.inner.Execute(ctx, args)
}
