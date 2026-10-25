package acp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion 是本实现支持的 ACP 主版本。
const ProtocolVersion = 1

// Agent 侧方法名。
const (
	MethodInitialize          = "initialize"
	MethodAuthenticate        = "authenticate"
	MethodLogout              = "logout"
	MethodSessionNew          = "session/new"
	MethodSessionLoad         = "session/load"
	MethodSessionResume       = "session/resume"
	MethodSessionClose        = "session/close"
	MethodSessionPrompt       = "session/prompt"
	MethodSessionCancel       = "session/cancel"
	MethodSessionSetMode      = "session/set_mode"
	MethodSessionSetConfigOpt = "session/set_config_option"
	MethodSessionUpdate       = "session/update"
	MethodSessionRequestPr    = "session/request_permission"
)

// 客户端侧（Agent 反向调用）方法名。
const (
	MethodFsReadTextFile  = "fs/read_text_file"
	MethodFsWriteTextFile = "fs/write_text_file"
	MethodTerminalCreate  = "terminal/create"
	MethodTerminalOutput  = "terminal/output"
	MethodTerminalWaitFor = "terminal/wait_for_exit"
	MethodTerminalKill    = "terminal/kill"
	MethodTerminalRelease = "terminal/release"
)

// Implementation 是 clientInfo / agentInfo。
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// FileSystemCapabilities 客户端文件系统能力。
type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

// ClientCapabilities 在 initialize 请求里由客户端声明。
type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                   `json:"terminal,omitempty"`
	// Session 汇集 session 级可选能力：notices 与 compaction。
	Session *SessionCapabilities `json:"session,omitempty"`
	// Meta 用于自定义能力扩展。
	Meta map[string]any `json:"_meta,omitempty"`
}

// SessionCapabilities 客户端对 session 可选更新的支持。
type SessionCapabilities struct {
	// Notices 非 nil 表示支持 notice 更新。
	Notices map[string]any `json:"notices,omitempty"`
	// Compaction 非 nil 表示支持 compaction_update / compaction_summary_chunk。
	Compaction map[string]any `json:"compaction,omitempty"`
}

// SupportsNotices 报告客户端是否支持 notice。
func (c ClientCapabilities) SupportsNotices() bool {
	return c.Session != nil && c.Session.Notices != nil
}

// SupportsCompaction 报告客户端是否支持压缩更新。
func (c ClientCapabilities) SupportsCompaction() bool {
	return c.Session != nil && c.Session.Compaction != nil
}

// PromptCapabilities 声明 prompt 里允许的内容块类型。
type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

// MCPCapabilities 声明支持的 MCP 传输。
type MCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

// AgentCapabilities 在 initialize 响应里由 Agent 声明。
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession,omitempty"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
	MCPCapabilities    MCPCapabilities    `json:"mcpCapabilities"`
	AuthCapabilities   AuthCapabilities   `json:"auth"`
	Meta               map[string]any     `json:"_meta,omitempty"`
}

// AuthCapabilities 声明 Agent 的认证行为。
type AuthCapabilities struct {
	// LoginRequired 表示客户端必须先 authenticate 才能 session/prompt。
	LoginRequired bool `json:"loginRequired,omitempty"`
	// Logout 表示支持 logout。
	Logout bool `json:"logout,omitempty"`
}

// AuthMethod 是 authenticate 的可选项声明。
type AuthMethod struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

// InitializeParams 是 initialize 请求参数。
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

// InitializeResult 是 initialize 响应。
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// ---------- 内容块 ----------

// ContentBlock 是与 MCP 对齐的 content block。
//
// 采用扁平结构体而非接口：字段按 type 取用，omitempty 保证不发送无关字段。
// 这比给六个变体各写一套 Unmarshal 简单，且能原样透传未知字段的风险更低
// （未知字段会被丢弃，对 Agent 侧只读内容是安全的）。
type ContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=image / audio
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`

	// type=resource
	Resource *EmbeddedResource `json:"resource,omitempty"`

	// type=resource_link
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`

	// Annotations 原样透传。
	Annotations map[string]any `json:"annotations,omitempty"`
}

// TextBlock 构造文本内容块。
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// EmbeddedResource 是 type=resource 的内嵌资源。
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// UnmarshalJSON 兼容 resource 字段直接是 uri/text 对象或 {resource: {...}} 包裹的形式。
func (r *EmbeddedResource) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Resource *EmbeddedResource `json:"resource"`
		URI      string            `json:"uri"`
		MimeType string            `json:"mimeType"`
		Text     string            `json:"text"`
		Blob     string            `json:"blob"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	if wrapper.Resource != nil {
		*r = *wrapper.Resource
		return nil
	}
	r.URI, r.MimeType, r.Text, r.Blob = wrapper.URI, wrapper.MimeType, wrapper.Text, wrapper.Blob
	return nil
}

// ---------- 工具调用 ----------

// ToolKind 是工具类别，供客户端选图标。
const (
	ToolKindRead       = "read"
	ToolKindEdit       = "edit"
	ToolKindDelete     = "delete"
	ToolKindMove       = "move"
	ToolKindSearch     = "search"
	ToolKindExecute    = "execute"
	ToolKindThink      = "think"
	ToolKindFetch      = "fetch"
	ToolKindSwitchMode = "switch_mode"
	ToolKindOther      = "other"
)

// ToolCallStatus 是工具执行状态。
const (
	ToolStatusPending    = "pending"
	ToolStatusInProgress = "in_progress"
	ToolStatusCompleted  = "completed"
	ToolStatusFailed     = "failed"
)

// ToolCallLocation 是工具涉及的文件位置（follow-along）。
type ToolCallLocation struct {
	Path string `json:"path"`
	// Line 为 1-based；0 表示不给。
	Line int `json:"line,omitempty"`
}

// ToolCallContent 是工具产出的内容：普通块 / diff / terminal。
type ToolCallContent struct {
	Type string `json:"type"`

	// type=content
	Content *ContentBlock `json:"content,omitempty"`

	// type=diff
	Path    string  `json:"path,omitempty"`
	OldText *string `json:"oldText,omitempty"`
	NewText string  `json:"newText,omitempty"`

	// type=terminal
	TerminalID string `json:"terminalId,omitempty"`
}

// ContentToolContent 包装一个普通内容块。
func ContentToolContent(b ContentBlock) ToolCallContent {
	return ToolCallContent{Type: "content", Content: &b}
}

// DiffToolContent 构造 diff 内容。
func DiffToolContent(path, oldText, newText string) ToolCallContent {
	return ToolCallContent{Type: "diff", Path: path, OldText: &oldText, NewText: newText}
}

// ---------- session/update 变体 ----------

// 每种 SessionUpdate 用独立结构体，避免 `content` 字段在不同变体里类型冲突
// （message chunk 是 ContentBlock，tool call 是 ToolCallContent[]）。

// UserMessageChunk 用户消息分片（也是 session/load 重放用）。
type UserMessageChunk struct {
	SessionUpdate string       `json:"sessionUpdate"` // user_message_chunk
	MessageID     string       `json:"messageId,omitempty"`
	Content       ContentBlock `json:"content"`
}

// AgentMessageChunk Agent 回复分片。
type AgentMessageChunk struct {
	SessionUpdate string       `json:"sessionUpdate"` // agent_message_chunk
	MessageID     string       `json:"messageId,omitempty"`
	Content       ContentBlock `json:"content"`
}

// AgentThoughtChunk 推理分片。
type AgentThoughtChunk struct {
	SessionUpdate string       `json:"sessionUpdate"` // agent_thought_chunk
	MessageID     string       `json:"messageId,omitempty"`
	Content       ContentBlock `json:"content"`
}

// ToolCallUpdate 同时用于 tool_call（新建）与 tool_call_update（增量）。
type ToolCallUpdate struct {
	SessionUpdate string             `json:"sessionUpdate"` // tool_call | tool_call_update
	ToolCallID    string             `json:"toolCallId"`
	Name          string             `json:"name,omitempty"`
	Title         string             `json:"title,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Status        string             `json:"status,omitempty"`
	Content       []ToolCallContent  `json:"content,omitempty"`
	Locations     []ToolCallLocation `json:"locations,omitempty"`
	RawInput      any                `json:"rawInput,omitempty"`
	RawOutput     any                `json:"rawOutput,omitempty"`
}

// NoticeUpdate 是面向用户的活体提示（不进入会话历史）。
type NoticeUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // notice
	Severity      string         `json:"severity"`      // info | warning | error
	Title         string         `json:"title"`
	Description   *string        `json:"description,omitempty"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

// UsageUpdate 报告上下文用量与累计成本。
type UsageUpdate struct {
	SessionUpdate string    `json:"sessionUpdate"` // usage_update
	Used          int       `json:"used"`
	Size          int       `json:"size"`
	Cost          *CostInfo `json:"cost,omitempty"`
}

// CostInfo 是 usage_update 的成本字段。
type CostInfo struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// CompactionUpdate 表示一次上下文压缩实体。
type CompactionUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // compaction_update
	CompactionID  string         `json:"compactionId"`
	Status        string         `json:"status"` // in_progress | completed | failed | cancelled
	Summary       []ContentBlock `json:"summary,omitempty"`
	Error         *string        `json:"error,omitempty"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

// CompactionSummaryChunk 追加压缩摘要内容。
type CompactionSummaryChunk struct {
	SessionUpdate string       `json:"sessionUpdate"` // compaction_summary_chunk
	CompactionID  string       `json:"compactionId"`
	Content       ContentBlock `json:"content"`
}

// SessionUpdateParams 是 session/update 通知参数。
type SessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// ---------- 停止原因 ----------

// ACP stopReason 取值。
const (
	StopEndTurn     = "end_turn"
	StopMaxTokens   = "max_tokens"
	StopMaxTurnReqs = "max_turn_requests"
	StopRefusal     = "refusal"
	StopCancelled   = "cancelled"
)

// ---------- session/* ----------

// MCPServer 是 stdio MCP 服务配置。
type MCPServer struct {
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Env     []EnvVariable `json:"env,omitempty"`
	// Type 为空表示 stdio；http/sse 时使用 URL/Headers。
	Type    string       `json:"type,omitempty"`
	URL     string       `json:"url,omitempty"`
	Headers []HTTPHeader `json:"headers,omitempty"`
}

// EnvVariable 是环境变量项。
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HTTPHeader 是 HTTP 头项。
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionNewParams 是 session/new 参数。
type SessionNewParams struct {
	Cwd                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

// SessionUpdateKind 是 session/update 的变体名（update.update 字段）。
const (
	SessionUpdateUserMessage       = "user_message_chunk"
	SessionUpdateAgentMessage      = "agent_message_chunk"
	SessionUpdateAgentThought      = "agent_thought_chunk"
	SessionUpdateToolCall          = "tool_call"
	SessionUpdateToolCallUpdate    = "tool_call_update"
	SessionUpdatePlan              = "plan"
	SessionUpdateAvailableCommands = "available_commands_update"
	SessionUpdateModeUpdate        = "current_mode_update"
	SessionUpdateConfigUpdate      = "config_option_update"
	SessionUpdateNotice            = "notice"
	SessionUpdateUsage             = "usage_update"
	SessionUpdateCompaction        = "compaction_update"
	SessionUpdateCompactionChunk   = "compaction_summary_chunk"
)

// SessionMode 是一个可选会话模式（如 ask / code / architect）。
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SessionModes 是会话模式集合。
type SessionModes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// ConfigOptionChoice 是配置项的可选值。
type ConfigOptionChoice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ConfigOption 是一个会话级配置项（模型、思考等级等）。
type ConfigOption struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Value       string               `json:"value"`
	Options     []ConfigOptionChoice `json:"options,omitempty"`
}

// SessionNewResult 是 session/new 响应。
//
// 除 sessionId 外其余字段都是可选的：只给出服务端真正支持的部分。
type SessionNewResult struct {
	SessionID     string         `json:"sessionId"`
	Modes         *SessionModes  `json:"modes,omitempty"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

// SessionLoadParams 是 session/load 参数。
type SessionLoadParams struct {
	SessionID             string      `json:"sessionId"`
	Cwd                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

// SessionResumeParams 是 session/resume 参数。
type SessionResumeParams = SessionLoadParams

// SessionPromptParams 是 session/prompt 参数。
type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// SessionPromptResult 是 session/prompt 响应。
type SessionPromptResult struct {
	StopReason string `json:"stopReason"`
	Usage      *Usage `json:"usage,omitempty"`
}

// Usage 是 ACP 的用量结构（与 agent.Usage 对齐）。
type Usage struct {
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cacheRead,omitempty"`
	CacheWrite int       `json:"cacheWrite,omitempty"`
	Reasoning  int       `json:"reasoning,omitempty"`
	Total      int       `json:"totalTokens,omitempty"`
	Cost       *CostInfo `json:"cost,omitempty"`
}

// SessionCancelParams 是 session/cancel 通知参数。
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// SessionCloseParams 是 session/close 参数。
type SessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

// SessionSetModeParams 是 session/set_mode 参数。
type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// SessionSetConfigOptionParams 是 session/set_config_option 参数。
type SessionSetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// ---------- 权限 ----------

// PermissionOptionKind 是权限选项类型。
const (
	PermAllowOnce    = "allow_once"
	PermAllowAlways  = "allow_always"
	PermRejectOnce   = "reject_once"
	PermRejectAlways = "reject_always"
)

// PermissionOption 是供用户选择的选项。
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// RequestPermissionParams 是 session/request_permission 参数。
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// RequestPermissionOutcome 是用户决定。
type RequestPermissionOutcome struct {
	// Outcome 为 "selected" 或 "cancelled"。
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResult 是 session/request_permission 响应。
type RequestPermissionResult struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
}

// ---------- 客户端反向方法 ----------

// FsReadTextFileParams 是 fs/read_text_file 参数。
type FsReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// FsReadTextFileResult 是 fs/read_text_file 响应。
type FsReadTextFileResult struct {
	Content string `json:"content"`
}

// FsWriteTextFileParams 是 fs/write_text_file 参数。
type FsWriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// TerminalCreateParams 是 terminal/create 参数。
type TerminalCreateParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	OutputByteLimit int           `json:"outputByteLimit,omitempty"`
}

// TerminalCreateResult 是 terminal/create 响应。
type TerminalCreateResult struct {
	TerminalID string `json:"terminalId"`
}

// TerminalOutputParams 是 terminal/output 参数。
type TerminalOutputParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// TerminalOutputResult 是 terminal/output 响应。
type TerminalOutputResult struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
	// ExitStatus 为 nil 表示还在运行。
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

// TerminalExitStatus 是终端退出状态。
type TerminalExitStatus struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

// TerminalWaitForExitParams 是 terminal/wait_for_exit 参数。
type TerminalWaitForExitParams = TerminalOutputParams

// TerminalIDParams 是 terminal/kill 与 terminal/release 的参数。
type TerminalIDParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// InvalidParams 构造参数错误。
func invalidParams(format string, a ...any) *RPCError {
	return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf(format, a...)}
}

// notFound 构造方法不存在错误。
func methodNotFound(method string) *RPCError {
	return &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
}
