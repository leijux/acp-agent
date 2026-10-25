package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/proc"
)

// 子代理相关默认值。
const (
	// DefaultSubagentProtocol 是发起 initialize 时声明的协议版本。
	DefaultSubagentProtocol = ProtocolVersion
	// DefaultPromptGrace 是取消后等待子代理自行收尾的时间，超时则杀进程树。
	DefaultPromptGrace = 5 * time.Second
	// DefaultCloseTimeout 是 Close 等待子进程退出的时间。
	DefaultCloseTimeout = 3 * time.Second
	// DefaultHandshakeTimeout 是 initialize + session/new 的总超时。
	//
	// 没有它的话，子代理卡在启动（或干脆不读 stdin）会让 delegate 一直挂着，
	// 只能靠外层 turn 被取消。握手是「进程刚起来」的阶段，30s 足够。
	DefaultHandshakeTimeout = 30 * time.Second
)

// SubagentSpec 描述如何启动一个 ACP 子代理进程。
type SubagentSpec struct {
	// Command 是可执行文件路径（必填）。
	Command string
	// Args 是启动参数。子代理需以 ACP stdio 模式运行（对 acp-agent 是 --acp）。
	Args []string
	// Env 追加到继承的环境变量之后，格式 "KEY=VALUE"。
	Env []string
	// Dir 是子进程工作目录，同时作为 session/new 的 cwd。
	Dir string
}

// SubagentFS 是本进程代表子代理执行文件读写的回调。
// 两者都为 nil 时不向子代理声明 fs 能力。
type SubagentFS struct {
	Read  func(ctx context.Context, sessionID, path string, line, limit int) (string, error)
	Write func(ctx context.Context, sessionID, path, content string) error
}

// SubagentOptions 控制一个子代理会话。
type SubagentOptions struct {
	Spec SubagentSpec

	// ClientInfo 是自报的客户端信息，缺省为 acp-agent。
	ClientInfo Implementation
	// ProtocolVersion 缺省用 DefaultSubagentProtocol。
	ProtocolVersion int
	// PromptGrace 缺省用 DefaultPromptGrace。
	PromptGrace time.Duration
	// CloseTimeout 缺省用 DefaultCloseTimeout。
	CloseTimeout time.Duration
	// HandshakeTimeout 缺省用 DefaultHandshakeTimeout（initialize + session/new）。
	HandshakeTimeout time.Duration
	// Logger 可为 nil（静默）。
	Logger *slog.Logger

	// OnUpdate 在收到 session/update 时回调，可为 nil。
	OnUpdate func(SubagentUpdate)
	// OnPermission 处理子代理的授权请求。nil 表示一律拒绝 —— 子代理反过来
	// 请求权限时，默认不信任比错误放行更安全。
	// 返回空字符串表示取消（outcome=cancelled）。
	OnPermission func(ctx context.Context, p RequestPermissionParams) (optionID string, err error)
	// FS 非 nil 时向子代理声明 fs 能力。
	FS *SubagentFS
	// Stderr 接收子代理的 stderr。nil 表示丢弃（但仍会读走，避免管道阻塞）。
	Stderr io.Writer
}

// SubagentUpdate 是转发给宿主的 session/update 摘要。
type SubagentUpdate struct {
	SessionID string
	Kind      string
	// Text 是 agent_message_chunk / agent_thought_chunk 的增量文本。
	Text string
	// ToolCall 在 tool_call / tool_call_update 时非空。
	ToolCall *ToolCallUpdate
	Usage    *Usage
	// Notice 是会话级提示（notice 变体）。
	Notice string
}

// SubagentResult 是一次 Prompt 的结果。
type SubagentResult struct {
	// Text 是拼接后的助手文本（agent_message_chunk）。
	Text string
	// Thought 是拼接后的思考文本（agent_thought_chunk）。
	Thought string
	// StopReason 取值见 StopEndTurn 等常量。
	StopReason string
	// ToolCalls 是子代理执行过的工具调用数。
	ToolCalls int
	// Usage 在子代理上报时非 nil。
	Usage *Usage
	// Cancelled 表示因 ctx 取消而中断。
	Cancelled bool
}

// Subagent 是一个已连上的 ACP 子代理：本进程充任 client，子进程充任 agent。
//
// 生命周期：Spawn → Prompt（可多次）→ Close。Close 会回收整棵进程树。
type Subagent struct {
	spec SubagentSpec
	cmd  *exec.Cmd
	job  *proc.Job
	conn *Conn
	log  *slog.Logger
	opts SubagentOptions

	init      InitializeResult
	sessionID string

	mu        sync.Mutex
	text      []string
	thought   []string
	toolCalls map[string]struct{}
	usage     *Usage
	closed    bool
}

// SpawnSubagent 启动一个 ACP 子代理，完成 initialize 与 session/new。
//
// 失败时保证子进程已被回收，调用方无需清理。
func SpawnSubagent(ctx context.Context, opts SubagentOptions) (s *Subagent, err error) {
	if opts.Spec.Command == "" {
		return nil, errors.New("acp: 子代理 Command 不能为空")
	}
	if opts.ProtocolVersion == 0 {
		opts.ProtocolVersion = DefaultSubagentProtocol
	}
	if opts.PromptGrace <= 0 {
		opts.PromptGrace = DefaultPromptGrace
	}
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = DefaultCloseTimeout
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	cmd := exec.Command(opts.Spec.Command, opts.Spec.Args...)
	cmd.Dir = opts.Spec.Dir
	if len(opts.Spec.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Spec.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: 子代理 stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: 子代理 stdout: %w", err)
	}
	cmd.Stderr = opts.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	// 独立进程组 / Job Object：子代理会再起 shell 与 MCP 进程，
	// 只杀直接子进程会留下孤儿。
	proc.Setup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: 启动子代理 %s: %w", opts.Spec.Command, err)
	}

	s = &Subagent{
		spec:      opts.Spec,
		cmd:       cmd,
		job:       proc.NewJob(),
		log:       log,
		opts:      opts,
		toolCalls: map[string]struct{}{},
	}
	if s.job != nil {
		if err := s.job.Assign(cmd.Process); err != nil {
			log.Warn("acp: 子代理进程组设置失败（回收可能不完整）", "err", err)
		}
	}

	conn := NewConn(stdout, stdin, stdin)
	s.conn = conn
	conn.Handle(MethodSessionRequestPr, s.handlePermission)
	conn.HandleNotify(MethodSessionUpdate, s.handleUpdate)
	if opts.FS != nil {
		conn.Handle(MethodFsReadTextFile, s.handleFSRead)
		conn.Handle(MethodFsWriteTextFile, s.handleFSWrite)
	}
	go func() {
		if err := conn.Serve(); err != nil && !errors.Is(err, io.EOF) {
			log.Debug("acp: 子代理连接结束", "err", err)
		}
	}()

	// 任何后续失败都要回收进程。
	defer func() {
		if err != nil {
			_ = s.Close()
			s = nil
		}
	}()

	if err := s.handshake(ctx); err != nil {
		return s, err
	}
	return s, nil
}

// handshake 完成 initialize + session/new（受 HandshakeTimeout 约束）。
func (s *Subagent) handshake(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, s.opts.HandshakeTimeout)
	defer cancel()
	if err := s.doHandshake(hctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("acp: 子代理握手超时（%s）: %w", s.opts.HandshakeTimeout, err)
		}
		return err
	}
	return nil
}

// doHandshake 是握手本体。
func (s *Subagent) doHandshake(ctx context.Context) error {
	caps := ClientCapabilities{}
	if s.opts.FS != nil {
		caps.FS = FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}
	}
	info := s.opts.ClientInfo
	if info.Name == "" {
		info = Implementation{Name: "acp-agent", Version: "subagent"}
	}

	var initRes InitializeResult
	err := s.conn.CallInto(ctx, MethodInitialize, InitializeParams{
		ProtocolVersion:    s.opts.ProtocolVersion,
		ClientCapabilities: caps,
		ClientInfo:         &info,
	}, &initRes)
	if err != nil {
		return fmt.Errorf("acp: 子代理 initialize 失败: %w", err)
	}
	if initRes.ProtocolVersion != s.opts.ProtocolVersion {
		s.log.Warn("acp: 子代理协议版本与请求不一致",
			"got", initRes.ProtocolVersion, "want", s.opts.ProtocolVersion)
	}
	s.init = initRes
	s.log.Debug("acp: 子代理已连接",
		"name", initRes.AgentInfo.Name, "version", initRes.AgentInfo.Version)

	var sessRes struct {
		SessionID string `json:"sessionId"`
	}
	err = s.conn.CallInto(ctx, MethodSessionNew, SessionNewParams{
		Cwd:        s.spec.Dir,
		MCPServers: []MCPServer{},
	}, &sessRes)
	if err != nil {
		return fmt.Errorf("acp: 子代理 session/new 失败: %w", err)
	}
	if sessRes.SessionID == "" {
		return errors.New("acp: 子代理未返回 sessionId")
	}
	s.sessionID = sessRes.SessionID
	return nil
}

// Info 返回子代理自报的实现信息。
func (s *Subagent) Info() *Implementation { return s.init.AgentInfo }

// SessionID 返回子代理会话 ID。
func (s *Subagent) SessionID() string { return s.sessionID }

// Prompt 发送一轮用户消息并等待结束，返回累积的文本。
//
// ctx 取消时向子代理发 session/cancel 并等待 PromptGrace；仍未结束则杀进程树。
func (s *Subagent) Prompt(ctx context.Context, text string) (*SubagentResult, error) {
	if s == nil {
		return nil, errors.New("acp: 子代理未初始化")
	}
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("acp: prompt 不能为空")
	}
	s.reset()

	type outcome struct {
		res SessionPromptResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var res SessionPromptResult
		err := s.conn.CallInto(ctx, MethodSessionPrompt, SessionPromptParams{
			SessionID: s.sessionID,
			Prompt:    []ContentBlock{TextBlock(text)},
		}, &res)
		done <- outcome{res: res, err: err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			return nil, s.promptError(o.err)
		}
		return s.collect(o.res.StopReason, false), nil

	case <-ctx.Done():
		// 通知子代理取消；它应当结束当前轮并回 cancelled。
		notifyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.conn.Notify(notifyCtx, MethodSessionCancel, SessionCancelParams{SessionID: s.sessionID})
		cancel()

		select {
		case o := <-done:
			if o.err != nil {
				return nil, s.promptError(o.err)
			}
			return s.collect(o.res.StopReason, true), nil
		case <-time.After(s.opts.PromptGrace):
			// 子代理不响应取消：杀进程树，保证 delegate 不会永久挂起。
			s.kill()
			return nil, fmt.Errorf("acp: 子代理未在 %s 内响应取消，已终止", s.opts.PromptGrace)
		}
	}
}

// promptError 把连接中断翻译成更可诊断的错误。
func (s *Subagent) promptError(err error) error {
	if s.conn != nil {
		select {
		case <-s.conn.Done():
			if cerr := s.conn.Err(); cerr != nil {
				return fmt.Errorf("acp: 子代理连接中断（进程可能已退出）: %w", cerr)
			}
		default:
		}
	}
	return err
}

// collect 组装 Prompt 结果。调用后累积状态被清空。
func (s *Subagent) collect(stopReason string, cancelled bool) *SubagentResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &SubagentResult{
		Text:       strings.Join(s.text, ""),
		Thought:    strings.Join(s.thought, ""),
		StopReason: stopReason,
		ToolCalls:  len(s.toolCalls),
		Usage:      s.usage,
		Cancelled:  cancelled || stopReason == StopCancelled,
	}
	return res
}

func (s *Subagent) reset() {
	s.mu.Lock()
	s.text = nil
	s.thought = nil
	s.toolCalls = map[string]struct{}{}
	s.usage = nil
	s.mu.Unlock()
}

// handleUpdate 处理 session/update 通知。
func (s *Subagent) handleUpdate(_ context.Context, raw json.RawMessage) {
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	var u struct {
		SessionUpdate string        `json:"sessionUpdate"`
		Content       *ContentBlock `json:"content"`
		ToolCallID    string        `json:"toolCallId"`
		Name          string        `json:"name"`
		Title         string        `json:"title"`
		Kind          string        `json:"kind"`
		Status        string        `json:"status"`
		Usage         *Usage        `json:"usage"`
		Notice        string        `json:"notice"`
		Message       string        `json:"message"`
	}
	if err := json.Unmarshal(p.Update, &u); err != nil {
		return
	}

	upd := SubagentUpdate{SessionID: p.SessionID, Kind: u.SessionUpdate}

	s.mu.Lock()
	switch u.SessionUpdate {
	case SessionUpdateAgentMessage:
		if u.Content != nil && u.Content.Text != "" {
			s.text = append(s.text, u.Content.Text)
			upd.Text = u.Content.Text
		}
	case SessionUpdateAgentThought:
		if u.Content != nil && u.Content.Text != "" {
			s.thought = append(s.thought, u.Content.Text)
			upd.Text = u.Content.Text
		}
	case SessionUpdateToolCall, SessionUpdateToolCallUpdate:
		if u.ToolCallID != "" {
			s.toolCalls[u.ToolCallID] = struct{}{}
		}
		upd.ToolCall = &ToolCallUpdate{
			SessionUpdate: u.SessionUpdate,
			ToolCallID:    u.ToolCallID,
			Name:          u.Name,
			Title:         u.Title,
			Kind:          u.Kind,
			Status:        u.Status,
		}
	case SessionUpdateUsage:
		if u.Usage != nil {
			s.usage = u.Usage
			upd.Usage = u.Usage
		}
	case SessionUpdateNotice:
		upd.Notice = firstNonEmpty(u.Notice, u.Message)
	}
	s.mu.Unlock()

	if s.opts.OnUpdate != nil {
		s.opts.OnUpdate(upd)
	}
}

// handlePermission 把子代理的授权请求转给宿主。
func (s *Subagent) handlePermission(ctx context.Context, raw json.RawMessage) (any, error) {
	var p RequestPermissionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "解析 request_permission 失败", err.Error())
	}
	if s.opts.OnPermission == nil {
		// 未配置即拒绝：子代理不能凭默认值获得权限。
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{
			Outcome: "cancelled",
		}}, nil
	}
	optionID, err := s.opts.OnPermission(ctx, p)
	if err != nil {
		// 回调出错按取消处理，避免把错误当成「允许」。
		s.log.Warn("acp: 子代理授权回调失败", "err", err)
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{Outcome: "cancelled"}}, nil
	}
	if optionID == "" {
		return RequestPermissionResult{Outcome: RequestPermissionOutcome{Outcome: "cancelled"}}, nil
	}
	return RequestPermissionResult{Outcome: RequestPermissionOutcome{
		Outcome:  "selected",
		OptionID: optionID,
	}}, nil
}

// handleFSRead 代表子代理读文件。
func (s *Subagent) handleFSRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var p FsReadTextFileParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "解析 read_text_file 失败", err.Error())
	}
	if s.opts.FS == nil || s.opts.FS.Read == nil {
		return nil, NewRPCError(CodeMethodNotFound, "fs/read_text_file 未启用", nil)
	}
	content, err := s.opts.FS.Read(ctx, p.SessionID, p.Path, p.Line, p.Limit)
	if err != nil {
		return nil, NewRPCError(CodeInternalError, err.Error(), nil)
	}
	return FsReadTextFileResult{Content: content}, nil
}

// handleFSWrite 代表子代理写文件。
func (s *Subagent) handleFSWrite(ctx context.Context, raw json.RawMessage) (any, error) {
	var p FsWriteTextFileParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, "解析 write_text_file 失败", err.Error())
	}
	if s.opts.FS == nil || s.opts.FS.Write == nil {
		return nil, NewRPCError(CodeMethodNotFound, "fs/write_text_file 未启用", nil)
	}
	if err := s.opts.FS.Write(ctx, p.SessionID, p.Path, p.Content); err != nil {
		return nil, NewRPCError(CodeInternalError, err.Error(), nil)
	}
	return struct{}{}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// kill 终止子代理（含整棵进程树）。
func (s *Subagent) kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		proc.KillTree(s.cmd)
	}
	if s.job != nil {
		s.job.Terminate()
	}
}

// Close 关闭会话并回收子进程树。可重复调用。
func (s *Subagent) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.conn != nil {
		_ = s.conn.Close() // 关闭 stdin，子进程应据此退出
	}

	exited := make(chan struct{})
	if s.cmd != nil {
		go func() {
			_ = s.cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(s.opts.CloseTimeout):
			s.log.Warn("acp: 子代理未按时退出，强制终止")
			s.kill()
			select {
			case <-exited:
			case <-time.After(time.Second):
			}
		}
	}
	if s.job != nil {
		s.job.Close()
	}
	return nil
}
