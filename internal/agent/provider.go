package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Model 描述一次请求用的模型及其能力。
type Model struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	API      string `json:"api,omitempty"`
	// ContextWindow 是上下文窗口（token）。决定压缩阈值。
	ContextWindow int `json:"contextWindow"`
	// MaxOutputTokens 单次响应的输出上限。
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`

	SupportsImages   bool `json:"supportsImages,omitempty"`
	SupportsThinking bool `json:"supportsThinking,omitempty"`
}

// ToolSpec 是发出给模型的工具定义。
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Request 是一次模型请求。
type Request struct {
	SystemPrompt  string
	Messages      []Message
	Tools         []ToolSpec
	Model         Model
	MaxTokens     int
	Temperature   *float64
	ThinkingLevel string
	// Purpose 区分用途："chat"（默认）或 "summarize"。压缩请求应禁用
	// prompt-cache write —— 这类一次性 prompt 基本不会被复用。
	Purpose string
}

// PurposeSummarize 是压缩请求的 Purpose 值。
const PurposeSummarize = "summarize"

// Sink 接收流式增量。实现方可以是 nil（表示不需要流式）。
type Sink interface {
	Text(delta string)
	Thinking(delta string)
	// ToolCall 在一个完整的工具调用确定后回调（参数已收齐）。
	ToolCall(call ToolCallBlock)
}

// SinkFunc 用函数字段实现 Sink，nil 字段即忽略。
type SinkFunc struct {
	OnText     func(delta string)
	OnThinking func(delta string)
	OnToolCall func(call ToolCallBlock)
}

func (s *SinkFunc) Text(d string) {
	if s != nil && s.OnText != nil {
		s.OnText(d)
	}
}
func (s *SinkFunc) Thinking(d string) {
	if s != nil && s.OnThinking != nil {
		s.OnThinking(d)
	}
}
func (s *SinkFunc) ToolCall(c ToolCallBlock) {
	if s != nil && s.OnToolCall != nil {
		s.OnToolCall(c)
	}
}

// NoopSink 丢弃所有增量。
type NoopSink struct{}

func (NoopSink) Text(string)            {}
func (NoopSink) Thinking(string)        {}
func (NoopSink) ToolCall(ToolCallBlock) {}

// Provider 是模型后端。Stream 必须尊重 ctx 取消，并在返回时给出终态
// stopReason（不能是 pending）。
type Provider interface {
	Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error)
}

// ProviderFunc 让函数实现 Provider。
type ProviderFunc func(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error)

func (f ProviderFunc) Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error) {
	return f(ctx, req, sink)
}

// ErrNoProvider 未配置 provider。
var ErrNoProvider = errors.New("agent: 未配置 Provider")

// ScriptedProvider 按预设序列返回响应，用于测试与回放。
//
// 每次 Stream 取下一个响应；响应可以是固定消息，也可以是函数（按请求动态生成）。
type ScriptedProvider struct {
	mu    sync.Mutex
	steps []any // *AssistantMessage 或 func(Request) (*AssistantMessage, error)
	calls []Request
}

// NewScriptedProvider 用一串响应构造。元素类型为 *AssistantMessage 或
// func(Request) (*AssistantMessage, error)。
func NewScriptedProvider(steps ...any) *ScriptedProvider {
	return &ScriptedProvider{steps: steps}
}

// Requests 返回已收到的请求（便于断言上下文组装是否正确）。
func (p *ScriptedProvider) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.calls...)
}

// Stream 实现 Provider。
func (p *ScriptedProvider) Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, req)
	if len(p.steps) == 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("agent: ScriptedProvider 响应序列已耗尽")
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	p.mu.Unlock()

	var msg *AssistantMessage
	switch v := step.(type) {
	case *AssistantMessage:
		msg = v.clone().(*AssistantMessage)
	case AssistantMessage:
		c := v.clone().(*AssistantMessage)
		msg = c
	case func(Request) (*AssistantMessage, error):
		m, err := v(req)
		if err != nil {
			return nil, err
		}
		msg = m
	default:
		return nil, fmt.Errorf("agent: ScriptedProvider 不支持的步骤类型 %T", step)
	}

	// 回放流式增量，让上层能测流式路径。
	if sink != nil {
		for _, blk := range msg.Content {
			switch b := blk.(type) {
			case TextBlock:
				if b.Text != "" {
					sink.Text(b.Text)
				}
			case ThinkingBlock:
				if b.Thinking != "" {
					sink.Thinking(b.Thinking)
				}
			case ToolCallBlock:
				sink.ToolCall(b)
			}
		}
	}
	if msg.At == 0 {
		msg.At = now()
	}
	if msg.StopReason == "" || msg.StopReason == StopPending {
		if len(msg.ToolCalls()) > 0 {
			msg.StopReason = StopToolUse
		} else {
			msg.StopReason = StopEnd
		}
	}
	return msg, nil
}

// EstimateTokens 粗略估算文本 token 数：CJK 字符 1 token，其余按 4 字符 1 token。
// 与 Pi 的估算口径接近，用于压缩阈值判断（不是精确计费）。
func EstimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF,
			r >= 0x3040 && r <= 0x30FF,
			r >= 0xAC00 && r <= 0xD7AF,
			r >= 0x3000 && r <= 0x303F,
			r >= 0xFF00 && r <= 0xFFEF:
			cjk++
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// EstimateMessagesTokens 估算一组消息的 token 数，含固定开销。
func EstimateMessagesTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 // 角色/分隔开销
		switch v := m.(type) {
		case *AssistantMessage:
			for _, blk := range v.Content {
				switch b := blk.(type) {
				case TextBlock:
					total += EstimateTokens(b.Text)
				case ThinkingBlock:
					total += EstimateTokens(b.Thinking)
				case ToolCallBlock:
					total += EstimateTokens(b.Name) + 8
					for k, val := range b.Arguments {
						total += EstimateTokens(k) + EstimateTokens(fmt.Sprint(val))
					}
				}
			}
		default:
			total += EstimateTokens(TextOf(m))
		}
	}
	return total
}
