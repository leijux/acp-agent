package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ACP 约定由 Agent 返回的额外错误码（JSON-RPC 标准码见 jsonrpc.go）。
const (
	// CodeAuthRequired 要求客户端先完成 authenticate。
	CodeAuthRequired = -32000
	// CodeResourceNotFound 会话/资源不存在。
	CodeResourceNotFound = -32002
	// CodeRequestCancelled 请求被对端取消。
	CodeRequestCancelled = -32800
)

// Sink 是 Handler 向客户端推送 session/update 的出口。
type Sink interface {
	// SessionID 返回当前会话 ID。
	SessionID() string
	// Send 原样发送一条 session/update 的 update 载荷。
	Send(update any)
}

// SessionSetup 是 session/new 与 session/load 的可选返回内容。
type SessionSetup struct {
	// SessionID 为空时沿用 server 生成的 ID。
	SessionID     string
	Modes         *SessionModes
	ConfigOptions []ConfigOption
	Meta          map[string]any
}

// PromptResult 是一轮 prompt 的结果。
type PromptResult struct {
	// StopReason 取值见 Stop* 常量；空值按 end_turn 处理。
	StopReason string
	Usage      *Usage
}

// Handler 是 ACP 语义层后端。
//
// 约定：
//   - Initialize 在 initialize 请求中调用一次，返回值与 ServerOptions.Capabilities 合并。
//   - 方法在独立 goroutine 中调用，实现方需自行保证并发安全。
//   - Prompt 是长任务：ctx 在 session/cancel、连接关闭时取消。实现应立即返回
//     context.Canceled —— server 会把它翻译成 stopReason: "cancelled"。
type Handler interface {
	Initialize(ctx context.Context, params InitializeParams) (AgentCapabilities, error)
	NewSession(ctx context.Context, s *ServerSession, params SessionNewParams) (SessionSetup, error)
	LoadSession(ctx context.Context, s *ServerSession, params SessionLoadParams) (SessionSetup, error)
	Prompt(ctx context.Context, s *ServerSession, prompt []ContentBlock) (PromptResult, error)
	Cancel(ctx context.Context, s *ServerSession)
	Close(ctx context.Context, s *ServerSession) error
}

// ModeHandler 可选接口：支持 session/set_mode。
type ModeHandler interface {
	SetMode(ctx context.Context, s *ServerSession, modeID string) error
}

// ConfigHandler 可选接口：支持 session/set_config_option。
type ConfigHandler interface {
	SetConfigOption(ctx context.Context, s *ServerSession, configID, value string) error
}

// Authenticator 可选接口：支持 authenticate。
//
// 未实现时 authenticate 一律返回成功。
type Authenticator interface {
	Authenticate(ctx context.Context, methodID string) error
}

// ServerOptions 配置 Server。
type ServerOptions struct {
	// Handler 必填。
	Handler Handler
	// Info 是 Agent 自报的实现信息。
	Info Implementation
	// Capabilities 是基础能力，与 Handler.Initialize 的返回值合并。
	Capabilities AgentCapabilities
	// AuthMethods 在 initialize 响应里声明的认证方式。
	AuthMethods []AuthMethod
	// RequireAuth 为 true 时，未带 authMethods 的客户端会在 prompt 时被拒绝。
	RequireAuth bool
	// OnClient 在 initialize 之后回调（可拿到协商后的客户端能力）。
	OnClient func(*Client)
	// Logger 可为 nil。
	Logger *slog.Logger
}

// ServerSession 是 server 侧的一个 ACP 会话。
//
// ID/Cwd 等在注册后不再变化；其余可变状态由 server 内部锁保护，
// Handler 可以把自己的上下文放进 Data（建议自带锁）。
type ServerSession struct {
	ID                    string
	Cwd                   string
	MCPServers            []MCPServer
	AdditionalDirectories []string

	// Data 是 Handler 私有的会话状态槽。
	Data map[string]any
	// Sink 推送给客户端的更新出口。
	Sink Sink
	// Conn 是底层连接。
	Conn *Conn
	// Owner 是所属 server。
	Owner *Server

	mu      sync.Mutex
	cancel  context.CancelFunc
	closed  bool
	meta    map[string]any
	running atomic.Bool
}

// Client 返回本会话使用的客户端调用封装。
func (s *ServerSession) Client() *Client {
	if s.Owner == nil {
		return nil
	}
	return s.Owner.Client()
}

// Meta 返回 Handler 在 setup 时给出的元信息。
func (s *ServerSession) Meta() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// Running 报告是否有 prompt 正在处理。
func (s *ServerSession) Running() bool { return s.running.Load() }

func (s *ServerSession) setCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.cancel = c
	s.mu.Unlock()
}

func (s *ServerSession) abort() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *ServerSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Server 在 Conn 上注册 ACP Agent 侧方法。
type Server struct {
	conn *Conn
	opts ServerOptions
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[string]*ServerSession
	seq      int
	client   *Client
	caps     ClientCapabilities
	initDone bool
}

// NewServer 注册所有 ACP 方法（不阻塞），随后调用 Serve 进入读循环。
func NewServer(conn *Conn, opts ServerOptions) (*Server, error) {
	if conn == nil {
		return nil, errors.New("acp: conn 不能为空")
	}
	if opts.Handler == nil {
		return nil, errors.New("acp: ServerOptions.Handler 不能为空")
	}
	if opts.Info.Name == "" {
		opts.Info.Name = "acp-agent"
	}
	if opts.Info.Version == "" {
		opts.Info.Version = "0.1.0"
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{conn: conn, opts: opts, log: log, sessions: make(map[string]*ServerSession)}
	s.register()
	return s, nil
}

// Serve 运行读循环直到连接关闭；正常关闭返回 nil。
func (s *Server) Serve() error {
	err := s.conn.Serve()
	s.closeAll()
	if errors.Is(err, ErrConnClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Conn 返回底层连接。
func (s *Server) Conn() *Conn { return s.conn }

// Client 返回客户端调用封装（initialize 之前为 nil）。
func (s *Server) Client() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Session 查会话。
func (s *Server) Session(id string) (*ServerSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// Sessions 返回全部会话 ID 快照。
func (s *Server) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		out = append(out, id)
	}
	return out
}

// Close 中断所有会话并关闭连接。
func (s *Server) Close() error {
	s.closeAll()
	return s.conn.Close()
}

func (s *Server) closeAll() {
	s.mu.Lock()
	sessions := make([]*ServerSession, 0, len(s.sessions))
	for id, sess := range s.sessions {
		sessions = append(sessions, sess)
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.mu.Lock()
		sess.closed = true
		sess.mu.Unlock()
	}
	for _, sess := range sessions {
		sess.abort()
	}
}

func (s *Server) register() {
	s.conn.Handle(MethodInitialize, s.handleInitialize)
	s.conn.Handle(MethodAuthenticate, s.handleAuthenticate)
	s.conn.Handle(MethodSessionNew, s.handleSessionNew)
	s.conn.Handle(MethodSessionLoad, s.handleSessionLoad)
	s.conn.Handle(MethodSessionResume, s.handleSessionLoad)
	s.conn.Handle(MethodSessionPrompt, s.handlePrompt)
	s.conn.Handle(MethodSessionSetMode, s.handleSetMode)
	s.conn.Handle(MethodSessionSetConfigOpt, s.handleSetConfig)
	s.conn.Handle(MethodSessionClose, s.handleSessionClose)
	s.conn.HandleNotify(MethodSessionCancel, s.handleCancel)
}

func (s *Server) handleInitialize(ctx context.Context, params json.RawMessage) (any, error) {
	var p InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "initialize 参数无效", err.Error())
	}
	if p.ProtocolVersion != 0 && p.ProtocolVersion != ProtocolVersion {
		return nil, NewRPCError(CodeInvalidRequest,
			fmt.Sprintf("不支持的协议版本 %d（本实现支持 %d）", p.ProtocolVersion, ProtocolVersion), nil)
	}
	caps, err := s.opts.Handler.Initialize(ctx, p)
	if err != nil {
		return nil, err
	}
	caps = mergeCapabilities(s.opts.Capabilities, caps)
	if s.opts.RequireAuth && len(s.opts.AuthMethods) > 0 {
		caps.AuthCapabilities.LoginRequired = true
	}

	client := NewClient(s.conn, p.ClientCapabilities, p.ClientInfo)
	s.mu.Lock()
	s.client = client
	s.caps = p.ClientCapabilities
	s.initDone = true
	s.mu.Unlock()

	if s.opts.OnClient != nil {
		s.opts.OnClient(client)
	}
	return InitializeResult{
		ProtocolVersion:   ProtocolVersion,
		AgentCapabilities: caps,
		AgentInfo:         &s.opts.Info,
		AuthMethods:       s.opts.AuthMethods,
	}, nil
}

func (s *Server) handleAuthenticate(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		MethodID string `json:"methodId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "authenticate 参数无效", err.Error())
	}
	if h, ok := s.opts.Handler.(Authenticator); ok {
		if err := h.Authenticate(ctx, p.MethodID); err != nil {
			return nil, err
		}
	}
	return struct{}{}, nil
}

func (s *Server) handleSessionNew(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionNewParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/new 参数无效", err.Error())
	}
	sess := s.newSession(p.Cwd, p.MCPServers, p.AdditionalDirectories)
	setup, err := s.opts.Handler.NewSession(ctx, sess, p)
	if err != nil {
		s.dropSession(sess.ID)
		return nil, err
	}
	sess.applySetup(setup)
	return sessionResult(sess, setup), nil
}

func (s *Server) handleSessionLoad(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionLoadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/load 参数无效", err.Error())
	}
	if _, exists := s.Session(p.SessionID); exists {
		return nil, NewRPCError(CodeInvalidRequest, "会话已存在: "+p.SessionID, nil)
	}
	sess := s.newSession(p.Cwd, p.MCPServers, p.AdditionalDirectories)
	if p.SessionID != "" {
		s.rekeySession(sess, p.SessionID)
	}
	setup, err := s.opts.Handler.LoadSession(ctx, sess, p)
	if err != nil {
		s.dropSession(sess.ID)
		return nil, err
	}
	sess.applySetup(setup)
	return sessionResult(sess, setup), nil
}

func (s *Server) handlePrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionPromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/prompt 参数无效", err.Error())
	}
	sess, ok := s.Session(p.SessionID)
	if !ok {
		return nil, NewRPCError(CodeResourceNotFound, "会话不存在: "+p.SessionID, nil)
	}
	if sess.isClosed() {
		return nil, NewRPCError(CodeResourceNotFound, "会话已关闭: "+p.SessionID, nil)
	}
	if s.opts.RequireAuth && len(s.opts.AuthMethods) > 0 && s.Client() == nil {
		return nil, NewRPCError(CodeAuthRequired, "需要先完成 authenticate", nil)
	}
	// 同一会话不允许并发 prompt：两个 turn 交错会写坏同一条消息历史。
	if !sess.running.CompareAndSwap(false, true) {
		return nil, NewRPCError(CodeInvalidRequest, "会话正在处理另一个 prompt", nil)
	}
	defer sess.running.Store(false)

	runCtx, cancel := context.WithCancel(ctx)
	sess.setCancel(cancel)
	defer func() {
		sess.setCancel(nil)
		cancel()
	}()

	res, err := s.opts.Handler.Prompt(runCtx, sess, p.Prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || runCtx.Err() != nil {
			return SessionPromptResult{StopReason: StopCancelled}, nil
		}
		return nil, err
	}
	stop := res.StopReason
	if stop == "" {
		stop = StopEndTurn
	}
	return SessionPromptResult{StopReason: stop, Usage: res.Usage}, nil
}

func (s *Server) handleCancel(_ context.Context, params json.RawMessage) {
	var p SessionCancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	sess, ok := s.Session(p.SessionID)
	if !ok {
		return
	}
	sess.abort()
	s.opts.Handler.Cancel(s.conn.ctx, sess)
}

func (s *Server) handleSetMode(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionSetModeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/set_mode 参数无效", err.Error())
	}
	sess, ok := s.Session(p.SessionID)
	if !ok {
		return nil, NewRPCError(CodeResourceNotFound, "会话不存在", nil)
	}
	h, ok := s.opts.Handler.(ModeHandler)
	if !ok {
		return nil, NewRPCError(CodeMethodNotFound, "不支持 session/set_mode", nil)
	}
	if err := h.SetMode(ctx, sess, p.ModeID); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Server) handleSetConfig(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionSetConfigOptionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/set_config_option 参数无效", err.Error())
	}
	sess, ok := s.Session(p.SessionID)
	if !ok {
		return nil, NewRPCError(CodeResourceNotFound, "会话不存在", nil)
	}
	h, ok := s.opts.Handler.(ConfigHandler)
	if !ok {
		return nil, NewRPCError(CodeMethodNotFound, "不支持 session/set_config_option", nil)
	}
	if err := h.SetConfigOption(ctx, sess, p.ConfigID, p.Value); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Server) handleSessionClose(ctx context.Context, params json.RawMessage) (any, error) {
	var p SessionCloseParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "session/close 参数无效", err.Error())
	}
	sess, ok := s.Session(p.SessionID)
	if !ok {
		return nil, NewRPCError(CodeResourceNotFound, "会话不存在", nil)
	}
	sess.abort()
	if err := s.opts.Handler.Close(ctx, sess); err != nil {
		return nil, err
	}
	s.dropSession(sess.ID)
	return struct{}{}, nil
}

func (s *Server) newSession(cwd string, mcp []MCPServer, dirs []string) *ServerSession {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("sess_%d", s.seq)
	s.mu.Unlock()

	sess := &ServerSession{
		ID:                    id,
		Cwd:                   cwd,
		MCPServers:            mcp,
		AdditionalDirectories: dirs,
		Data:                  make(map[string]any),
		Conn:                  s.conn,
		Owner:                 s,
		Sink:                  newUpdates(s.conn, id, s.clientCaps()),
	}

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

// rekeySession 把会话移到客户端指定的 ID 下（session/load 场景）。
func (s *Server) rekeySession(sess *ServerSession, id string) {
	s.mu.Lock()
	delete(s.sessions, sess.ID)
	sess.ID = id
	s.sessions[id] = sess
	s.mu.Unlock()
	if u, ok := sess.Sink.(*updates); ok {
		u.sessionID = id
	}
}

func (sess *ServerSession) applySetup(setup SessionSetup) {
	if setup.SessionID != "" && setup.SessionID != sess.ID {
		if sess.Owner != nil {
			sess.Owner.rekeySession(sess, setup.SessionID)
		} else {
			sess.ID = setup.SessionID
		}
	}
	sess.mu.Lock()
	sess.meta = setup.Meta
	sess.mu.Unlock()
}

func (s *Server) clientCaps() ClientCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

func (s *Server) dropSession(id string) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if ok {
		sess.mu.Lock()
		sess.closed = true
		sess.mu.Unlock()
	}
}

func sessionResult(sess *ServerSession, setup SessionSetup) SessionNewResult {
	return SessionNewResult{
		SessionID:     sess.ID,
		Modes:         setup.Modes,
		ConfigOptions: setup.ConfigOptions,
		Meta:          setup.Meta,
	}
}

func mergeCapabilities(base, extra AgentCapabilities) AgentCapabilities {
	out := base
	out.LoadSession = out.LoadSession || extra.LoadSession
	out.PromptCapabilities.Image = out.PromptCapabilities.Image || extra.PromptCapabilities.Image
	out.PromptCapabilities.Audio = out.PromptCapabilities.Audio || extra.PromptCapabilities.Audio
	out.PromptCapabilities.EmbeddedContext = out.PromptCapabilities.EmbeddedContext || extra.PromptCapabilities.EmbeddedContext
	out.MCPCapabilities.HTTP = out.MCPCapabilities.HTTP || extra.MCPCapabilities.HTTP
	out.MCPCapabilities.SSE = out.MCPCapabilities.SSE || extra.MCPCapabilities.SSE
	out.AuthCapabilities.LoginRequired = out.AuthCapabilities.LoginRequired || extra.AuthCapabilities.LoginRequired
	out.AuthCapabilities.Logout = out.AuthCapabilities.Logout || extra.AuthCapabilities.Logout
	return out
}

// 断言 updates 实现了 Sink。
var _ Sink = (*updates)(nil)
