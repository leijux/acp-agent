package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/tools"
)

// AgentConfig 描述每个 ACP 会话背后的 agent 实例如何构造。
//
// 每个 session/new 都会用同一份配置新建一个 agent.Agent（历史与工具状态
// 彼此隔离）；Provider / Registry / Executor 可以安全共享。
type AgentConfig struct {
	// Provider 必填。
	Provider agent.Provider
	// Model 必填。
	Model agent.Model
	// Registry 提供工具定义；配合 Executor 才会执行工具。
	Registry *tools.Registry
	Executor *tools.Executor

	Context        agent.ContextOptions
	MaxTurns       int
	ContextWindow  int
	DisableCompact bool

	// Session 可选：为 true 时用 agent.Session 的持久化目录恢复历史。
	// Dir 为空则不持久化。
	Dir string

	// SessionFor 可选：按会话的工作目录构造该会话专属的工具、执行器、
	// 上下文与持久化目录。
	//
	// ACP 里 session/new 的 cwd 就是「这个会话在哪个项目里干活」，工作目录
	// 必须跟着会话走。而 AgentConfig 上的 Registry/Executor/Context/Dir 是
	// 进程级的：客户端一旦支持切换项目（acp-chat 这类多项目客户端），它们
	// 会把所有会话都指到进程启动目录。提供 SessionFor 后，每个会话拿到
	// 自己的 Policy.Root / WorkingDir / 上下文文件目录。
	//
	// 返回的字段为零值时沿用 AgentConfig 上的对应值（见 mergeSessionConfig）。
	SessionFor func(cwd string) (SessionConfig, error)

	// Permission 控制工具调用是否需要客户端授权。
	Permission PermissionMode
	// PermissionTimeout 是等待客户端授权的上限；零值用 DefaultPermissionTimeout。
	// 客户端不理会 session/request_permission 时，靠它把「永久挂住」
	// 变成一次明确的超时错误。
	PermissionTimeout time.Duration
	// AutoApprove 是无需询问即可执行的工具名（PermissionModeAsk 时生效）。
	AutoApprove []string
	// OnlyTools 限制可见工具集合；空表示全部。
	OnlyTools []string
}

// SessionConfig 是一个会话专属的运行时资源。
//
// 会话之间不能共享 Registry：工具（read/write/bash/js）在构造时就把
// Policy.Root 固化下来，共享意味着所有会话都按同一个目录解析相对路径。
// Provider 可以共享，工具与工作目录不行。
type SessionConfig struct {
	Registry *tools.Registry
	Executor *tools.Executor
	Context  agent.ContextOptions
	// Dir 是该会话的持久化目录；空表示不落盘。
	Dir string
	// Close 可选：会话关闭时调用，用来回收该会话独占的资源
	// （典型是 bash 后台进程管理器）。
	Close func()
}

// PermissionMode 决定工具权限门禁策略。
type PermissionMode string

const (
	// PermissionAllowAll 直接放行（默认，适合单用户本地 CLI）。
	PermissionAllowAll PermissionMode = "allow_all"
	// PermissionAsk 通过 session/request_permission 询问客户端。
	PermissionAsk PermissionMode = "ask"
	// PermissionReadOnly 只放行只读工具。
	PermissionReadOnly PermissionMode = "read_only"
)

// sessionState 是一个 ACP 会话的运行时状态。
type sessionState struct {
	agent   *agent.Agent
	history *agent.Session
	sink    *updates
	guard   Guard
	// dir 是该会话的持久化目录；空表示不落盘。
	dir string
	// close 回收该会话独占的资源（可能为 nil）。
	close func()

	mu     sync.Mutex
	mode   string
	closed bool
}

// AgentHandler 用 internal/agent 实现 ACP 的 Handler。
type AgentHandler struct {
	cfg AgentConfig
	log interface {
		Debug(string, ...any)
		Warn(string, ...any)
	}

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// NewAgentHandler 创建 handler。
func NewAgentHandler(cfg AgentConfig) (*AgentHandler, error) {
	if cfg.Provider == nil {
		return nil, errors.New("acp: AgentConfig.Provider 不能为空")
	}
	if cfg.Permission == "" {
		cfg.Permission = PermissionAllowAll
	}
	return &AgentHandler{cfg: cfg, sessions: make(map[string]*sessionState)}, nil
}

// Initialize 声明能力：图片输入始终支持；load 仅在有持久化目录时声明。
//
// 不能无条件声明 loadSession：客户端会据此在重启后尝试恢复会话，而
// LoadSession 在 AgentConfig.Dir 为空时必然报错。
func (h *AgentHandler) Initialize(_ context.Context, p InitializeParams) (AgentCapabilities, error) {
	caps := AgentCapabilities{
		// SessionFor 会按会话 cwd 给出持久化目录，此时会话级目录非空，
		// 与 cfg.Dir 的语义一致。
		LoadSession:        h.cfg.Dir != "" || h.cfg.SessionFor != nil,
		PromptCapabilities: PromptCapabilities{Image: true},
	}
	return caps, nil
}

// NewSession 新建会话。
func (h *AgentHandler) NewSession(ctx context.Context, s *ServerSession, p SessionNewParams) (SessionSetup, error) {
	sc, err := h.sessionConfig(s)
	if err != nil {
		return SessionSetup{}, err
	}
	return h.setup(ctx, s, agent.NewSession(), sc)
}

// LoadSession 从磁盘恢复会话。
func (h *AgentHandler) LoadSession(ctx context.Context, s *ServerSession, p SessionLoadParams) (SessionSetup, error) {
	sc, err := h.sessionConfig(s)
	if err != nil {
		return SessionSetup{}, err
	}
	if sc.Dir == "" {
		return SessionSetup{}, NewRPCError(CodeInvalidRequest,
			"本 agent 未配置会话持久化目录，无法 session/load", nil)
	}
	hist, err := agent.LoadSession(sessionPathIn(sc.Dir, p.SessionID))
	if err != nil {
		return SessionSetup{}, NewRPCError(CodeResourceNotFound,
			fmt.Sprintf("无法加载会话 %s: %v", p.SessionID, err), nil)
	}
	// 把已恢复的历史回放给客户端（session/load 的语义要求）。
	if u, ok := s.Sink.(*updates); ok {
		replayHistory(u, hist.Messages())
	}
	return h.setup(ctx, s, hist, sc)
}

// sessionConfig 解析某个会话该用的运行时资源。
//
// 没有 SessionFor 时就是 AgentConfig 上的全局值（旧行为）；有则按会话 cwd
// 构造，cwd 为空时退回 cfg.Context.WorkingDir（进程启动目录）。
func (h *AgentHandler) sessionConfig(s *ServerSession) (SessionConfig, error) {
	base := SessionConfig{
		Registry: h.cfg.Registry,
		Executor: h.cfg.Executor,
		Context:  h.cfg.Context,
		Dir:      h.cfg.Dir,
	}
	if h.cfg.SessionFor == nil {
		return base, nil
	}
	cwd := ""
	if s != nil {
		cwd = strings.TrimSpace(s.Cwd)
	}
	if cwd == "" {
		cwd = h.cfg.Context.WorkingDir
	}
	got, err := h.cfg.SessionFor(cwd)
	if err != nil {
		return SessionConfig{}, err
	}
	return mergeSessionConfig(base, got), nil
}

// mergeSessionConfig 用会话级配置覆盖全局配置；零值字段保留全局值。
func mergeSessionConfig(base, over SessionConfig) SessionConfig {
	if over.Registry != nil {
		base.Registry = over.Registry
	}
	if over.Executor != nil {
		base.Executor = over.Executor
	}
	if over.Dir != "" {
		base.Dir = over.Dir
	}
	if over.Close != nil {
		base.Close = over.Close
	}
	base.Context = mergeContextOptions(base.Context, over.Context)
	return base
}

func mergeContextOptions(base, over agent.ContextOptions) agent.ContextOptions {
	if over.BasePrompt != "" {
		base.BasePrompt = over.BasePrompt
	}
	if over.Sections != nil {
		base.Sections = over.Sections
	}
	if over.ContextFiles != nil {
		base.ContextFiles = over.ContextFiles
	}
	if over.WorkingDir != "" {
		base.WorkingDir = over.WorkingDir
	}
	if over.ExtraInstructions != "" {
		base.ExtraInstructions = over.ExtraInstructions
	}
	return base
}

func (h *AgentHandler) setup(ctx context.Context, s *ServerSession, hist *agent.Session, sc SessionConfig) (SessionSetup, error) {
	registry := sc.Registry
	if registry != nil {
		if len(h.cfg.OnlyTools) > 0 {
			sub, err := subsetRegistry(registry, h.cfg.OnlyTools)
			if err != nil {
				return SessionSetup{}, err
			}
			registry = sub
		}
		var guard Guard
		switch h.cfg.Permission {
		case PermissionAsk:
			u, _ := s.Sink.(*updates)
			guard = newAutoApprove(
				NewPermissionGuard(s.Client(), s.ID, u).SetTimeout(h.cfg.PermissionTimeout),
				h.cfg.AutoApprove...,
			)
		case PermissionReadOnly:
			guard = DenyMutating(registry)
		}
		if guard != nil {
			wrapped, err := WrapRegistry(registry, guard)
			if err != nil {
				return SessionSetup{}, err
			}
			registry = wrapped
		}
	}

	ag, err := agent.New(agent.Options{
		Provider:           h.cfg.Provider,
		Model:              h.cfg.Model,
		Session:            hist,
		Registry:           registry,
		Executor:           h.executorFor(sc.Executor, registry),
		Context:            sc.Context,
		MaxTurns:           h.cfg.MaxTurns,
		ContextWindow:      h.cfg.ContextWindow,
		DisableAutoCompact: h.cfg.DisableCompact,
	})
	if err != nil {
		return SessionSetup{}, err
	}

	st := &sessionState{agent: ag, history: hist, dir: sc.Dir, close: sc.Close}
	if u, ok := s.Sink.(*updates); ok {
		st.sink = u
		ag.On(u.Handle)
	}
	h.mu.Lock()
	h.sessions[s.ID] = st
	h.mu.Unlock()

	return SessionSetup{}, nil
}

// Prompt 处理一轮用户输入。
func (h *AgentHandler) Prompt(ctx context.Context, s *ServerSession, prompt []ContentBlock) (PromptResult, error) {
	st := h.state(s.ID)
	if st == nil {
		return PromptResult{}, NewRPCError(CodeResourceNotFound, "会话未初始化", nil)
	}

	msg, err := toAgentMessage(prompt)
	if err != nil {
		return PromptResult{}, NewRPCError(CodeInvalidParams, err.Error(), nil)
	}

	// 回显用户消息：ACP 要求由 Agent 自己把用户输入作为 user_message_chunk 广播，
	// 这样客户端 UI 与真实历史顺序一致。
	if st.sink != nil && msg != nil {
		echoUser(st.sink, msg)
	}

	res, err := st.agent.RunMessages(ctx, msg)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return PromptResult{StopReason: StopCancelled}, nil
		}
		if res != nil && res.Aborted {
			return PromptResult{StopReason: StopCancelled}, nil
		}
		return PromptResult{}, err
	}
	if st.dir != "" {
		if err := st.history.Save(sessionPathIn(st.dir, s.ID)); err != nil {
			// 持久化失败不应让 turn 失败，但要让调用方知道。
			return PromptResult{StopReason: mapStopReason(res), Usage: toUsage(res)}, nil
		}
	}
	return PromptResult{StopReason: mapStopReason(res), Usage: toUsage(res)}, nil
}

// Cancel 中断进行中的 turn。
func (h *AgentHandler) Cancel(_ context.Context, s *ServerSession) {
	if st := h.state(s.ID); st != nil {
		st.agent.Abort()
	}
}

// Close 释放会话。
func (h *AgentHandler) Close(_ context.Context, s *ServerSession) error {
	h.mu.Lock()
	st := h.sessions[s.ID]
	delete(h.sessions, s.ID)
	h.mu.Unlock()
	if st != nil {
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()
		st.agent.Abort()
		if st.close != nil {
			st.close()
		}
	}
	return nil
}

func (h *AgentHandler) state(id string) *sessionState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

// executorFor 把配置里的 Executor 重绑到（可能已被权限包装的）registry 上。
//
// 这一点很关键：Executor 自己持有 registry，若仍指向未包装的 registry，
// 权限门禁就会被静默绕过（工具照常执行，客户端收不到任何 request_permission）。
func (h *AgentHandler) executorFor(base *tools.Executor, registry *tools.Registry) *tools.Executor {
	if registry == nil {
		return nil
	}
	if base != nil {
		cp := *base
		cp.Registry = registry
		return &cp
	}
	return tools.NewExecutor(registry, 0)
}

// sessionPathIn 拼出会话文件路径。目录由会话级配置决定（见 SessionConfig.Dir），
// 不再是进程级的 AgentConfig.Dir。
func sessionPathIn(dir, id string) string {
	return strings.TrimRight(dir, "/\\") + string('/') + id + ".json"
}

// ---------- 转换 ----------

// toAgentMessage 把 ACP 的 prompt 内容块转成 agent 消息。
//
// 支持 text / resource_link / resource / image。图片保留为 ImageBlock，
// 由 provider 适配层转成 image_url（internal/provider/openai.go 已支持）；
// 只有 data 字段带 base64 的 image 块能这样直通，只有 uri 的降级为文本引用。
func toAgentMessage(blocks []ContentBlock) (agent.Message, error) {
	var out []agent.Block
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, agent.TextBlock{Text: text.String()})
			text.Reset()
		}
	}
	addText := func(s string) {
		if s == "" {
			return
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(s)
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			addText(b.Text)
		case "resource_link", "resource":
			addText(b.URI)
		case "image":
			if b.Data == "" {
				// 客户端只给了 uri（不在 data 里内联图片），无法直接送给模型。
				addText("[图片：" + b.MIMEType + " " + b.URI + "]")
				continue
			}
			flush()
			out = append(out, agent.ImageBlock{Data: b.Data, MimeType: b.MIMEType})
		}
	}
	flush()
	if len(out) == 0 {
		return nil, errors.New("prompt 中没有可用内容")
	}
	msg := agent.TextMessage("")
	msg.Content = out
	return msg, nil
}

func echoUser(u *updates, m agent.Message) {
	um, ok := m.(*agent.UserMessage)
	if !ok {
		return
	}
	for _, blk := range um.Content {
		switch b := blk.(type) {
		case agent.TextBlock:
			u.Send(UserMessageChunk{
				SessionUpdate: SessionUpdateUserMessage,
				Content:       TextBlock(b.Text),
			})
		case agent.ImageBlock:
			u.Send(UserMessageChunk{
				SessionUpdate: SessionUpdateUserMessage,
				Content: ContentBlock{
					Type:     "image",
					Data:     b.Data,
					MIMEType: b.MimeType,
				},
			})
		}
	}
}

func replayHistory(u *updates, msgs []agent.Message) {
	for _, m := range msgs {
		echoUser(u, m)
	}
}

func mapStopReason(res *agent.RunResult) string {
	if res == nil {
		return StopEndTurn
	}
	if res.Aborted {
		return StopCancelled
	}
	if res.Err != nil {
		if strings.Contains(res.Err.Error(), "最大轮数") {
			return StopMaxTurnReqs
		}
		return StopRefusal
	}
	last := res.LastAssistant()
	if last == nil {
		return StopEndTurn
	}
	switch last.StopReason {
	case agent.StopLength:
		return StopMaxTokens
	case agent.StopAborted:
		return StopCancelled
	case agent.StopError:
		return StopRefusal
	}
	return StopEndTurn
}

func toUsage(res *agent.RunResult) *Usage {
	if res == nil {
		return nil
	}
	u := res.Usage
	if u.Input == 0 && u.Output == 0 && u.TotalTokens == 0 {
		return nil
	}
	return &Usage{
		Input:      u.Input,
		Output:     u.Output,
		CacheRead:  u.CacheRead,
		CacheWrite: u.CacheWrite,
		Reasoning:  u.Reasoning,
		Total:      u.TotalTokens,
	}
}

// ---------- 自动放行 ----------

// autoApprove 在 guard 之前拦掉白名单工具，避免无意义的权限弹窗。
type autoApprove struct {
	inner Guard
	allow map[string]bool
}

func newAutoApprove(inner Guard, names ...string) Guard {
	a := &autoApprove{inner: inner, allow: make(map[string]bool, len(names))}
	for _, n := range names {
		a.allow[n] = true
	}
	return a
}

func (a *autoApprove) Authorize(ctx context.Context, toolName string, args map[string]any) error {
	if a.allow[toolName] {
		return nil
	}
	return a.inner.Authorize(ctx, toolName, args)
}

// subsetRegistry 复制一个只含指定工具的 Registry（未知名字忽略）。
func subsetRegistry(base *tools.Registry, names []string) (*tools.Registry, error) {
	out := tools.NewRegistry()
	for _, n := range names {
		if t, ok := base.Get(n); ok {
			if err := out.Register(t); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
