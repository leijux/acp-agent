// Package agent 是 Pi（@earendil-works/pi-coding-agent）agent 核心循环的 Go 移植。
//
// 对齐的 Pi 语义（见 pi docs 的 how-pi-works / message-types / compaction）：
//
//   - 会话是 entry 树，每条 entry 有 id 与 parentId；从当前 entry 回溯得到的路径叫
//     active branch，它提供下一次模型请求的历史。
//   - 一轮（turn）= 模型响应 + 其中所有 tool call 的执行 + 结果记录。只要还有
//     tool result 或排队消息需要模型响应，就继续下一轮；否则本次 run 结束。
//   - steering 消息插在当前 assistant turn 之后；follow-up 消息在 agent 完成手上
//     所有工作之后才进入；abort 停止当前 run 并把排队消息退回编辑器。
//   - 压缩（compaction）在 contextTokens > contextWindow - reserveTokens 时触发，
//     从后往前累计 keepRecentTokens 找到切点，切点只允许落在 user / assistant /
//     bashExecution / custom 消息上，**绝不切在 toolResult 上**（必须与它的
//     tool call 待在一起）。
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Role 是消息角色。与 Pi 的 AgentMessage 联合类型一一对应。
type Role string

const (
	RoleSystem            Role = "system"
	RoleUser              Role = "user"
	RoleAssistant         Role = "assistant"
	RoleToolResult        Role = "toolResult"
	RoleBashExecution     Role = "bashExecution"
	RoleCustom            Role = "custom"
	RoleCompactionSummary Role = "compactionSummary"
	RoleBranchSummary     Role = "branchSummary"
)

// Message 是会话里的一条消息。具体类型见下方各结构体。
type Message interface {
	Role() Role
	Timestamp() int64
	// clone 用于在分支/fork 时做浅拷贝，避免共享可变切片。
	clone() Message
}

var clock atomic.Int64

// now 返回 Unix 毫秒时间戳；测试可注入。
var now = func() int64 { return time.Now().UnixMilli() }

// ---------- 内容块 ----------

// Block 是消息内容块。Pi 的 text / image / thinking / toolCall。
type Block interface {
	BlockType() string
}

// TextBlock 文本。
type TextBlock struct {
	Text string `json:"text"`
	// TextSignature 是 provider 专属元数据，按不透明处理。
	TextSignature string `json:"textSignature,omitempty"`
}

func (TextBlock) BlockType() string { return "text" }

// ThinkingBlock 思考内容。redacted 时可能没有可见文本，但 signature 里带加密载荷。
type ThinkingBlock struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"thinkingSignature,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

func (ThinkingBlock) BlockType() string { return "thinking" }

// ImageBlock base64 图片。
type ImageBlock struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func (ImageBlock) BlockType() string { return "image" }

// ToolCallBlock 模型发起的工具调用。
type ToolCallBlock struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Arguments        map[string]any `json:"arguments"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
	Namespace        string         `json:"namespace,omitempty"`
}

func (ToolCallBlock) BlockType() string { return "toolCall" }

// ---------- 用量 ----------

// Cost 费用明细。
type Cost struct {
	// Currency 是计价币种："USD"（空值视为 USD）或 "CNY"。
	// 各家定价原文币种不同（DeepSeek 用人民币），直接累加会得出错误总数。
	Currency   string  `json:"currency,omitempty"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Add 累加两次费用，币种取先出现的非空值。
func (c Cost) Add(o Cost) Cost {
	cur := c.Currency
	if cur == "" {
		cur = o.Currency
	}
	return Cost{
		Currency:   cur,
		Input:      c.Input + o.Input,
		Output:     c.Output + o.Output,
		CacheRead:  c.CacheRead + o.CacheRead,
		CacheWrite: c.CacheWrite + o.CacheWrite,
		Total:      c.Total + o.Total,
	}
}

// Usage 一次模型调用的 token 用量。Reasoning 已包含在 Output 内，不要重复累加。
type Usage struct {
	Input       int  `json:"input"`
	Output      int  `json:"output"`
	CacheRead   int  `json:"cacheRead"`
	CacheWrite  int  `json:"cacheWrite"`
	Reasoning   int  `json:"reasoning,omitempty"`
	TotalTokens int  `json:"totalTokens"`
	Cost        Cost `json:"cost"`
}

// Add 累加两次用量。
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:       u.Input + o.Input,
		Output:      u.Output + o.Output,
		CacheRead:   u.CacheRead + o.CacheRead,
		CacheWrite:  u.CacheWrite + o.CacheWrite,
		Reasoning:   u.Reasoning + o.Reasoning,
		TotalTokens: u.TotalTokens + o.TotalTokens,
		Cost:        u.Cost.Add(o.Cost),
	}
}

// ---------- 停止原因 ----------

// StopReason 与 Pi 完全对齐。pending 只用于流式中的部分消息，不落盘。
type StopReason string

const (
	StopPending StopReason = "pending"
	StopEnd     StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// ---------- 具体消息 ----------

// SystemMessage 声明初始 prompt 与工具集；后续 system 消息可追加指令或替换具名 section。
type SystemMessage struct {
	Content  []Block            `json:"content"`
	Sections map[string]*string `json:"sections,omitempty"`
	At       int64              `json:"timestamp"`
}

func (*SystemMessage) Role() Role         { return RoleSystem }
func (m *SystemMessage) Timestamp() int64 { return m.At }
func (m *SystemMessage) clone() Message {
	c := *m
	c.Content = cloneBlocks(m.Content)
	return &c
}

// UserMessage 用户输入。
type UserMessage struct {
	Content []Block `json:"content"`
	At      int64   `json:"timestamp"`
}

func (*UserMessage) Role() Role         { return RoleUser }
func (m *UserMessage) Timestamp() int64 { return m.At }
func (m *UserMessage) clone() Message {
	c := *m
	c.Content = cloneBlocks(m.Content)
	return &c
}

// AssistantMessage 模型响应。usage 总是存在；stopReason 在 message_end 时必须是终态。
type AssistantMessage struct {
	Content       []Block    `json:"content"`
	API           string     `json:"api,omitempty"`
	Provider      string     `json:"provider,omitempty"`
	Model         string     `json:"model,omitempty"`
	ResponseModel string     `json:"responseModel,omitempty"`
	ResponseID    string     `json:"responseId,omitempty"`
	ThinkingLevel string     `json:"thinkingLevel,omitempty"`
	Usage         Usage      `json:"usage"`
	StopReason    StopReason `json:"stopReason"`
	ErrorMessage  string     `json:"errorMessage,omitempty"`
	RawStopReason string     `json:"rawStopReason,omitempty"`
	At            int64      `json:"timestamp"`
}

func (*AssistantMessage) Role() Role         { return RoleAssistant }
func (m *AssistantMessage) Timestamp() int64 { return m.At }
func (m *AssistantMessage) clone() Message {
	c := *m
	c.Content = cloneBlocks(m.Content)
	return &c
}

// Text 返回所有 text 块拼接。
func (m *AssistantMessage) Text() string {
	var b strings.Builder
	for _, blk := range m.Content {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Thinking 返回所有 thinking 块拼接。
func (m *AssistantMessage) Thinking() string {
	var b strings.Builder
	for _, blk := range m.Content {
		if t, ok := blk.(ThinkingBlock); ok {
			b.WriteString(t.Thinking)
		}
	}
	return b.String()
}

// ToolCalls 返回响应中的全部工具调用（保持出现顺序，决定执行分批）。
func (m *AssistantMessage) ToolCalls() []ToolCallBlock {
	var out []ToolCallBlock
	for _, blk := range m.Content {
		if tc, ok := blk.(ToolCallBlock); ok {
			out = append(out, tc)
		}
	}
	return out
}

// ToolResultMessage 工具结果。
type ToolResultMessage struct {
	ToolCallID string  `json:"toolCallId"`
	ToolName   string  `json:"toolName"`
	Content    []Block `json:"content"`
	Details    any     `json:"details,omitempty"`
	Usage      *Usage  `json:"usage,omitempty"`
	IsError    bool    `json:"isError"`
	At         int64   `json:"timestamp"`
}

func (*ToolResultMessage) Role() Role         { return RoleToolResult }
func (m *ToolResultMessage) Timestamp() int64 { return m.At }
func (m *ToolResultMessage) clone() Message {
	c := *m
	c.Content = cloneBlocks(m.Content)
	return &c
}

// Text 返回结果中的文本。
func (m *ToolResultMessage) Text() string {
	var b strings.Builder
	for _, blk := range m.Content {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// BashExecutionMessage 直接 shell 命令的产物，不是 LLM 的工具结果。
// 除非 ExcludeFromContext，否则在下一次模型请求前会转成 user 文本。
type BashExecutionMessage struct {
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode,omitempty"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
	At                 int64  `json:"timestamp"`
}

func (*BashExecutionMessage) Role() Role         { return RoleBashExecution }
func (m *BashExecutionMessage) Timestamp() int64 { return m.At }
func (m *BashExecutionMessage) clone() Message   { c := *m; return &c }

// CustomMessage 扩展注入的上下文消息。Display 控制终端渲染，Details 不进模型。
type CustomMessage struct {
	CustomType string  `json:"customType"`
	Content    []Block `json:"content"`
	Display    bool    `json:"display"`
	Details    any     `json:"details,omitempty"`
	At         int64   `json:"timestamp"`
}

func (*CustomMessage) Role() Role         { return RoleCustom }
func (m *CustomMessage) Timestamp() int64 { return m.At }
func (m *CustomMessage) clone() Message {
	c := *m
	c.Content = cloneBlocks(m.Content)
	return &c
}

// CompactionSummaryMessage 压缩摘要。它替换更早的消息进入后续模型请求，
// 原始 entry 仍留在会话树里。
type CompactionSummaryMessage struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore"`
	At           int64  `json:"timestamp"`
}

func (*CompactionSummaryMessage) Role() Role         { return RoleCompactionSummary }
func (m *CompactionSummaryMessage) Timestamp() int64 { return m.At }
func (m *CompactionSummaryMessage) clone() Message   { c := *m; return &c }

// BranchSummaryMessage 切换分支时保留离开分支的上下文。
type BranchSummaryMessage struct {
	Summary string  `json:"summary"`
	FromID  *string `json:"fromId"`
	At      int64   `json:"timestamp"`
}

func (*BranchSummaryMessage) Role() Role         { return RoleBranchSummary }
func (m *BranchSummaryMessage) Timestamp() int64 { return m.At }
func (m *BranchSummaryMessage) clone() Message   { c := *m; return &c }

// ---------- 辅助 ----------

func cloneBlocks(in []Block) []Block {
	if in == nil {
		return nil
	}
	out := make([]Block, len(in))
	copy(out, in)
	return out
}

// TextMessage 构造只含文本的 user 消息。
func TextMessage(text string) *UserMessage {
	return &UserMessage{Content: []Block{TextBlock{Text: text}}, At: now()}
}

// TextOf 提取消息的纯文本（用于序列化与调试）。
func TextOf(m Message) string {
	switch v := m.(type) {
	case *SystemMessage:
		return blocksText(v.Content)
	case *UserMessage:
		return blocksText(v.Content)
	case *AssistantMessage:
		return v.Text()
	case *ToolResultMessage:
		return v.Text()
	case *BashExecutionMessage:
		return v.Output
	case *CustomMessage:
		return blocksText(v.Content)
	case *CompactionSummaryMessage:
		return v.Summary
	case *BranchSummaryMessage:
		return v.Summary
	}
	return ""
}

func blocksText(bs []Block) string {
	var b strings.Builder
	for _, blk := range bs {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// ---------- JSON（判别联合） ----------

// MarshalBlock 把 Block 编码成带 type 字段的 JSON。
func MarshalBlock(b Block) (json.RawMessage, error) {
	var payload any
	switch v := b.(type) {
	case TextBlock:
		payload = v
	case *TextBlock:
		payload = v
	case ThinkingBlock:
		payload = v
	case *ThinkingBlock:
		payload = v
	case ImageBlock:
		payload = v
	case *ImageBlock:
		payload = v
	case ToolCallBlock:
		payload = v
	case *ToolCallBlock:
		payload = v
	default:
		return nil, fmt.Errorf("agent: 未知内容块类型 %T", b)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["type"] = b.BlockType()
	return json.Marshal(m)
}

// UnmarshalBlock 解析带 type 字段的内容块 JSON。
func UnmarshalBlock(raw json.RawMessage) (Block, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "text":
		var v TextBlock
		return v, json.Unmarshal(raw, &v)
	case "thinking":
		var v ThinkingBlock
		return v, json.Unmarshal(raw, &v)
	case "image":
		var v ImageBlock
		return v, json.Unmarshal(raw, &v)
	case "toolCall":
		var v ToolCallBlock
		return v, json.Unmarshal(raw, &v)
	default:
		return nil, fmt.Errorf("agent: 未知内容块 type=%q", probe.Type)
	}
}
