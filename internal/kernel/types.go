// Package kernel 把 billion-context 的 acp-kernel（TypeScript 上下文压缩引擎）
// 封装成可嵌入 Go 进程的库：goja 执行其 CJS 产物，Go 侧提供宿主能力
// （Intl.Segmenter 分词、crypto、path、fs、状态持久化）。
//
// 上游：https://github.com/ranxianglei/billion-context/tree/master/kernel
//
// 核心模型与上游一致：Kernel 无状态，Session 持有 state；state 可导出 JSON，
// 由调用方自行持久化（JSON 文件 / SQLite / Redis 均可）。
package kernel

import "encoding/json"

// Role 对应 CoreMessage.role。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// ContentType 对应 CoreMessage.contentType。
type ContentType string

const (
	ContentText       ContentType = "text"
	ContentToolCall   ContentType = "tool-call"
	ContentToolResult ContentType = "tool-result"
	ContentReasoning  ContentType = "reasoning"
)

// Message 是传给 kernel 的一条消息（上游 CoreMessage 的 Go 视图）。
type Message struct {
	ID string `json:"id"`
	// Role / ContentType 为空时按 user / text 处理。
	Role        Role        `json:"role,omitempty"`
	ContentType ContentType `json:"contentType,omitempty"`
	Text        string      `json:"text,omitempty"`

	ToolName   string `json:"toolName,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`

	// ThinkingTokens 是宿主额外投影的 reasoning 载荷大小（只计费不渲染）。
	ThinkingTokens int `json:"thinkingTokens,omitempty"`

	// SummaryOfBlockID 标记“这是宿主自己渲染的某压缩块摘要”。
	SummaryOfBlockID string `json:"summaryOfBlockId,omitempty"`
	// LedgerSeq 是宿主账本编号（服务端连续性 ≥ 1）。
	LedgerSeq int64 `json:"ledgerSeq,omitempty"`

	// Ref 由 kernel 回填：本消息在本轮被分配的引用（mNNNNN）。
	Ref string `json:"-"`
}

// Range 是一次压缩请求覆盖的引用区间。
type Range struct {
	StartRef string `json:"startRef"`
	EndRef   string `json:"endRef"`
	Summary  string `json:"summary"`
	Topic    string `json:"topic,omitempty"`
	// SummaryMaxChars 覆盖压缩块的摘要长度上限（0 = 用 config）。
	SummaryMaxChars int `json:"summaryMaxChars,omitempty"`
}

// Block 是一个压缩块（上游 CompressionBlock 的常用字段）。
type Block struct {
	BlockID             string   `json:"blockId"`
	RunID               string   `json:"runId"`
	Tier                int      `json:"tier"`
	Topic               string   `json:"topic,omitempty"`
	Summary             string   `json:"summary"`
	DirectMessageIDs    []string `json:"directMessageIds"`
	EffectiveMessageIDs []string `json:"effectiveMessageIds"`
	DirectBlockIDs      []string `json:"directBlockIds"`
	CompressedTokens    int      `json:"compressedTokens"`
	CreatedAt           int64    `json:"createdAt"`
	SurvivedCount       int      `json:"survivedCount"`
	Generation          string   `json:"generation"`
	Active              bool     `json:"active"`
	Expanded            bool     `json:"expanded,omitempty"`
	StartRef            string   `json:"startRef,omitempty"`
	EndRef              string   `json:"endRef,omitempty"`
}

// NudgeRange 是 nudge 推荐的（或需保护的）压缩区间。
// 直接拿它的 StartRef/EndRef 去压缩，比自己算区间安全：kernel 已经把活跃
// 压缩块、受保护工具、最近消息等因素排除掉。
type NudgeRange struct {
	StartRef   string  `json:"startRef"`
	EndRef     string  `json:"endRef"`
	Count      int     `json:"count"`
	Tokens     int     `json:"tokens"`
	Chars      int     `json:"chars"`
	ToolPct    float64 `json:"toolPct"`
	TextPct    float64 `json:"textPct"`
	UserMsgs   int     `json:"userMsgs"`
	StartIndex int     `json:"startIndex"`
	EndIndex   int     `json:"endIndex"`
}

// Nudge 是 kernel 每轮给出的压缩建议。
type Nudge struct {
	// ShouldInject 为 true 时，应当向模型展示压缩提示并向策略层报告。
	ShouldInject bool   `json:"shouldInject"`
	Reason       string `json:"reason"`
	// ContextUsage 是生成建议时的上下文占用比例。
	ContextUsage float64 `json:"contextUsage"`

	// CompressibleRanges 是 kernel 认为“可以压”的区间，已按优先级排序。
	CompressibleRanges []NudgeRange `json:"compressibleRanges"`
	// ProtectedRanges 是受保护的区间（不可压）。
	ProtectedRanges []NudgeRange `json:"protectedRanges"`

	Tier             json.RawMessage `json:"tier,omitempty"`
	Breakdown        map[string]any  `json:"breakdown,omitempty"`
	ContextBreakdown map[string]int  `json:"contextBreakdown,omitempty"`
}

// RefMap 是 kernel 的 id ↔ 引用映射。
type RefMap struct {
	ByRaw   map[string]string `json:"byRaw"`
	ByRef   map[string]string `json:"byRef"`
	Blocked map[string]bool   `json:"blocked,omitempty"`
}

// TurnResult 是 ProcessTurn 的结果。
type TurnResult struct {
	// Messages 是渲染后的消息视图（已裁剪、已打 <acp> 标签）。
	Messages []Message `json:"messages"`
	// Nudge 是本轮生成的压缩提示（无提示时为 nil）。
	Nudge *Nudge `json:"-"`
	// NudgeJSON 是 nudge 的原始 JSON（保留上游新增字段）。
	NudgeJSON json.RawMessage `json:"nudge,omitempty"`
	// TerminalEscape 表示压缩与截断都已到顶。
	TerminalEscape json.RawMessage `json:"terminalEscape,omitempty"`
	// TruncationSkipped 非空表示紧急截断跑了但没省下 token。
	TruncationSkipped string `json:"truncationSkipped,omitempty"`
	// Refs 是本轮分配的消息引用。
	Refs RefMap `json:"refs"`

	// MessagesJSON 是渲染后消息的原始 JSON（保留上游新增字段）。
	MessagesJSON json.RawMessage `json:"-"`
}

// CompressionResult 是 ApplyCompression 的结果统计。
type CompressionResult struct {
	BlocksCreated    int      `json:"blocksCreated"`
	TokensCompressed int      `json:"tokensCompressed"`
	Errors           []string `json:"errors,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	Notes            []string `json:"notes,omitempty"`
	// Blocks 是应用后仍然 active 的压缩块。
	Blocks []Block `json:"blocks"`
}

// Status 是上下文使用状态。
type Status struct {
	ContextUsage      float64        `json:"contextUsage"`
	TokenCount        int            `json:"tokenCount"`
	ModelContextLimit int            `json:"modelContextLimit"`
	ActiveBlocks      int            `json:"activeBlocks"`
	TotalBlocks       int            `json:"totalBlocks"`
	TokensCompressed  int            `json:"tokensCompressed"`
	Breakdown         map[string]int `json:"breakdown"`
}

// RetrieveResult 是 acp_retrieve 原语的返回（上游结构较大，保持 JSON）。
type RetrieveResult struct {
	Raw json.RawMessage `json:"-"`
}

// Config 描述一次会话的配置。ModelContextLimit 必填；Overrides 会浅合并到
// 上游 defaultConfig 的顶层字段（如 "nudge"、"compress"、"truncate"、
// "protectedTools"、"preserveRecentMessages" 等），未提供的字段沿用上游默认值。
type Config struct {
	ModelContextLimit int            `json:"modelContextLimit"`
	Overrides         map[string]any `json:"overrides,omitempty"`
}
