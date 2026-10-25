package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/acp"
)

// 这一组代码解决一个问题：ACP 模式下「谁有权问用户」。
//
//	本地 REPL：stdin 是终端，guard 直接问 y/n。
//	ACP 模式：stdin 是 JSON-RPC 帧流，只能通过 session/request_permission
//	          反向调用客户端，让编辑器弹窗。
//
// 内置工具由 internal/acp 的 PermissionGuard 负责；delegate 子代理的授权
// 请求走 OnPermission（cmd/acp-agent/subagent.go），必须显式转到客户端，
// 否则只能落回本地 guard —— 而本地 guard 在 ACP 模式下不允许读 stdin，
// 结果是「子代理的写操作一律被静默拒绝」。

// setACPSession 记录当前活动 ACP 会话（nil 表示已关闭/非 ACP 模式）。
func (a *app) setACPSession(s *acp.ServerSession) {
	a.acpMu.Lock()
	a.acpCur = s
	a.acpMu.Unlock()
}

// currentACPSession 返回当前活动 ACP 会话，可能为 nil。
func (a *app) currentACPSession() *acp.ServerSession {
	a.acpMu.RLock()
	defer a.acpMu.RUnlock()
	return a.acpCur
}

// subagentPermTimeout 是等待客户端授权的上限。
//
// 客户端可能压根不支持 session/request_permission（或把未知 toolCallId 的请求
// 静默丢掉），那种情况下无超时就是永久挂住 —— 用户只能 Esc。
//
// 是 var 而不是 const：测试需要一个几百毫秒的超时。
var subagentPermTimeout = acp.DefaultPermissionTimeout

// acpAuthorize 把一次授权请求转给 ACP 客户端。
//
// handled=false 表示当前没有可用的 ACP 会话，调用方应回退到本地 guard。
// 只有 handled=true 时 allow/err 才有意义。
//
// 客户端选「总是允许/总是拒绝」后，同一 label 不再打扰用户（见 subagentPermCache）。
func (a *app) acpAuthorize(ctx context.Context, label string, mutates bool) (allow bool, handled bool, err error) {
	sess := a.currentACPSession()
	if sess == nil {
		return false, false, nil
	}
	client := sess.Client()
	if client == nil {
		return false, false, nil
	}

	// 之前的「总是允许 / 总是拒绝」先于一切：连 tool_call 都不用发。
	if cached, ok := a.subPerm.verdict(label); ok {
		return cached, true, nil
	}
	// 已经确认这个客户端不处理权限请求：不再每步等满一个超时。
	if a.subPerm.unsupported() {
		return false, true, fmt.Errorf("%w（%s；该客户端不处理 session/request_permission，请改用 permissions=auto 或 readonly）",
			acp.ErrPermissionTimeout, label)
	}

	kind := acp.ToolKindRead
	if mutates {
		kind = acp.ToolKindEdit
	}
	id := fmt.Sprintf("subagent-%d", time.Now().UnixNano())

	// 必须先发 tool_call 再请求权限：客户端要靠 toolCallId 把确认框关联到
	// 一张工具卡片上。只发 request_permission、ID 却不认识时，多数客户端会
	// 静默丢弃 —— 现象就是「卡住直到 Esc」。
	if sess.Sink != nil {
		sess.Sink.Send(acp.ToolCallUpdate{
			SessionUpdate: "tool_call",
			ToolCallID:    id,
			Name:          label,
			Title:         label,
			Kind:          kind,
			Status:        "pending",
		})
	}
	if a.term != nil {
		a.term.logf("  [%s] ? 等待客户端授权…\n", label)
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, subagentPermTimeout)
	defer cancel()

	tc := acp.ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    id,
		Name:          label,
		Title:         label,
		Kind:          kind,
	}
	outcome, err := client.RequestPermission(ctx, sess.ID, tc, acp.DefaultPermissionOptions())
	allow = err == nil && outcome != nil && outcome.Outcome == "selected" && optionAllows(outcome.OptionID)

	// 记忆「总是」：客户端只弹一次，后续同 label 直接按记忆走。
	if err == nil && outcome != nil && outcome.Outcome == "selected" {
		k := optionKindOf(outcome.OptionID)
		if k == acp.PermAllowAlways || k == acp.PermRejectAlways {
			a.subPerm.remember(label, allow)
			if a.term != nil {
				verdict := "总是拒绝"
				if allow {
					verdict = "总是允许"
				}
				a.term.logf("  [%s] 已记住：%s（本会话不再询问）\n", label, verdict)
			}
		}
	}

	// 收尾：把卡片从 pending 收掉，否则客户端会一直显示转圈。
	if sess.Sink != nil {
		status := "failed"
		if allow {
			status = "completed"
		}
		sess.Sink.Send(acp.ToolCallUpdate{
			SessionUpdate: "tool_call_update",
			ToolCallID:    id,
			Status:        status,
		})
	}

	switch {
	case err != nil:
		// 区分三种结束：客户端不回（超时）、turn 被取消、其它错误。
		// 先看父 ctx 是否被取消，否则父 ctx 取消与子 ctx 超时会混淆。
		if errors.Is(parent.Err(), context.Canceled) {
			return false, true, acp.ErrPermissionCancelled
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(parent.Err(), context.DeadlineExceeded) {
			a.subPerm.markUnsupported()
			if a.term != nil {
				a.term.logf("  [%s] 客户端未响应授权请求，本会话后续直接拒绝（建议 permissions=auto）\n", label)
			}
			return false, true, fmt.Errorf("等待客户端授权超时（%s）：客户端可能不支持 session/request_permission", subagentPermTimeout)
		}
		return false, true, fmt.Errorf("请求客户端授权失败: %w", err)
	case outcome == nil:
		return false, true, nil
	case outcome.Outcome == "cancelled":
		return false, true, acp.ErrPermissionCancelled
	default:
		return allow, true, nil
	}
}

// optionAllows 判断客户端选中的选项是否属于「允许」。
func optionAllows(optionID string) bool {
	k := optionKindOf(optionID)
	return k == acp.PermAllowOnce || k == acp.PermAllowAlways
}

// optionKindOf 返回选项 ID 对应的 Kind；未知 ID 返回空字符串。
func optionKindOf(optionID string) string {
	for _, o := range acp.DefaultPermissionOptions() {
		if o.OptionID == optionID {
			return o.Kind
		}
	}
	return ""
}

// subagentPermCache 记住子代理授权的「总是允许 / 总是拒绝」。
//
// 与 internal/acp.PermissionGuard 的 allowed/denied 同语义，但作用在
// cmd 层：内置工具的记忆归协议层，子代理（delegate）的归这里。
// key 是带上角色前缀的 label（如 "worker: write"），因此「总是允许 worker 写」
// 不会顺带放开 "reviewer: write"。
type subagentPermCache struct {
	mu    sync.Mutex
	allow map[string]bool
	deny  map[string]bool
	// noUI 为 true 表示该客户端一次都没响应过权限请求。
	// 之后不再浪费时间重试，直接快速失败（否则每个工具都等一个完整超时）。
	noUI bool
}

// unsupported 报告是否已判定客户端不处理权限请求。
func (c *subagentPermCache) unsupported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noUI
}

// markUnsupported 记住「客户端不响应」，本会话不再请求它。
func (c *subagentPermCache) markUnsupported() {
	c.mu.Lock()
	c.noUI = true
	c.mu.Unlock()
}

// verdict 返回已记住的决定；ok=false 表示没记过。
func (c *subagentPermCache) verdict(label string) (allow bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allow[label] {
		return true, true
	}
	if c.deny[label] {
		return false, true
	}
	return false, false
}

// remember 记录一个「总是」决定。
func (c *subagentPermCache) remember(label string, allow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if allow {
		if c.allow == nil {
			c.allow = make(map[string]bool)
		}
		c.allow[label] = true
		delete(c.deny, label)
		return
	}
	if c.deny == nil {
		c.deny = make(map[string]bool)
	}
	c.deny[label] = true
	delete(c.allow, label)
}

// acpSessionTracker 把 Handler 包一层，记录 server 交给当前活动会话的指针。
//
// 需要它是因为 AgentConfig 没有「会话建立」回调，而子代理授权要用到
// *ServerSession（拿 Client 与 SessionID）。选择包装而不是改 internal/acp：
// 这是 cmd 层独有的需求，不该让协议库知道「delegate」这回事。
type acpSessionTracker struct {
	acp.Handler
	onSession func(*acp.ServerSession)

	mu  sync.Mutex
	cur *acp.ServerSession
}

func (t *acpSessionTracker) Initialize(ctx context.Context, p acp.InitializeParams) (acp.AgentCapabilities, error) {
	return t.Handler.Initialize(ctx, p)
}

func (t *acpSessionTracker) NewSession(ctx context.Context, s *acp.ServerSession, p acp.SessionNewParams) (acp.SessionSetup, error) {
	t.track(s)
	return t.Handler.NewSession(ctx, s, p)
}

func (t *acpSessionTracker) LoadSession(ctx context.Context, s *acp.ServerSession, p acp.SessionLoadParams) (acp.SessionSetup, error) {
	t.track(s)
	return t.Handler.LoadSession(ctx, s, p)
}

func (t *acpSessionTracker) Prompt(ctx context.Context, s *acp.ServerSession, prompt []acp.ContentBlock) (acp.PromptResult, error) {
	t.track(s)
	return t.Handler.Prompt(ctx, s, prompt)
}

func (t *acpSessionTracker) Cancel(ctx context.Context, s *acp.ServerSession) {
	t.Handler.Cancel(ctx, s)
}

func (t *acpSessionTracker) Close(ctx context.Context, s *acp.ServerSession) error {
	defer t.forget(s)
	return t.Handler.Close(ctx, s)
}

// track 更新当前会话。
func (t *acpSessionTracker) track(s *acp.ServerSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur = s
	t.onSession(s)
}

// forget 只在被关闭的就是当前会话时才清空，避免「关旧会话把新会话的授权
// 通道一起拔掉」。
func (t *acpSessionTracker) forget(s *acp.ServerSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s != nil && t.cur != s {
		return
	}
	t.cur = nil
	t.onSession(nil)
}
