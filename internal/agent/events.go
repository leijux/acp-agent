package agent

import "time"

// Event 是 agent 生命周期事件。与 Pi 的 extension/RPC 事件对齐。
type Event interface {
	EventName() string
}

// AgentStartEvent run 开始。
type AgentStartEvent struct {
	Prompt []Message
}

func (AgentStartEvent) EventName() string { return "agent_start" }

// AgentEndEvent run 结束，附带本次 run 产生的全部消息。
type AgentEndEvent struct {
	Messages []Message
	// Aborted 表示本次 run 因 abort 提前结束。
	Aborted bool
	Err     error
}

func (AgentEndEvent) EventName() string { return "agent_end" }

// TurnStartEvent 一轮开始。
type TurnStartEvent struct {
	Turn int
}

func (TurnStartEvent) EventName() string { return "turn_start" }

// TurnEndEvent 一轮结束：模型响应 + 该响应里所有工具的执行结果。
type TurnEndEvent struct {
	Turn        int
	Message     Message
	ToolResults []*ToolResultMessage
}

func (TurnEndEvent) EventName() string { return "turn_end" }

// MessageStartEvent 一条消息开始（流式首个 chunk 或立即完整）。
type MessageStartEvent struct {
	Message Message
}

func (MessageStartEvent) EventName() string { return "message_start" }

// MessageUpdateEvent 流式增量。Kind 为 "text" 或 "thinking"。
type MessageUpdateEvent struct {
	Message Message
	Kind    string
	Delta   string
}

func (MessageUpdateEvent) EventName() string { return "message_update" }

// MessageEndEvent 一条消息完成。流的 assistant 消息在此时才拥有终态 stopReason。
type MessageEndEvent struct {
	Message Message
}

func (MessageEndEvent) EventName() string { return "message_end" }

// ToolExecutionStartEvent 单个工具开始执行。
type ToolExecutionStartEvent struct {
	Call ToolCallBlock
}

func (ToolExecutionStartEvent) EventName() string { return "tool_execution_start" }

// ToolExecutionEndEvent 单个工具执行结束。
type ToolExecutionEndEvent struct {
	Call     ToolCallBlock
	Result   *ToolResultMessage
	Duration time.Duration
	// Parallel 表示该调用是与其他调用并发执行的。
	Parallel bool
}

func (ToolExecutionEndEvent) EventName() string { return "tool_execution_end" }

// CompactionStartEvent 压缩开始。Reason 为 "manual" / "threshold" / "overflow"。
type CompactionStartEvent struct {
	Reason       string
	TokensBefore int
	CutEntryID   string
}

func (CompactionStartEvent) EventName() string { return "session_before_compact" }

// CompactionEndEvent 压缩完成。
type CompactionEndEvent struct {
	Summary      string
	TokensBefore int
	TokensAfter  int
	CutEntryID   string
}

func (CompactionEndEvent) EventName() string { return "session_compact" }

// CompactionAbortEvent 表示「发过 CompactionStartEvent、但没走到完成」。
//
// 必须发：订阅 Start 的消费者（ACP 的 compaction_update 实体、界面上的
// 「压缩中」提示）只会在收到 End 或 Abort 时收尾。之前四条失败路径
// （摘要模型报错、kernel 拒绝、镜像块失败、区间已被覆盖的 noop）都是
// 「发过 Start 就直接 return error」，于是客户端永远停在 in_progress。
type CompactionAbortEvent struct {
	// Reason 与对应 Start 事件相同（"manual" / "threshold" / "overflow"）。
	Reason string
	// Phase 是哪一步出的问题："summarize" / "apply" / "save" / "mirror" / "noop"。
	Phase string
	// Err 是失败原因；noop 也算失败终态（已经有块覆盖了这段，没得可压）。
	Err error
}

func (CompactionAbortEvent) EventName() string { return "session_compact_aborted" }

// ErrorEvent 非致命错误（例如 provider 报错但 run 仍可继续时）。
type ErrorEvent struct {
	Err error
}

func (ErrorEvent) EventName() string { return "error" }

// EventHandler 消费事件。返回 error 不会中断 run（与 Pi 的 handler 一致：
// 单个 handler 失败不应破坏会话），但会通过 ErrorEvent 报告。
type EventHandler func(Event)

// Emitter 是事件分发器。零值可用。
type Emitter struct {
	handlers []EventHandler
}

// On 注册 handler，返回取消函数。
func (e *Emitter) On(h EventHandler) (off func()) {
	e.handlers = append(e.handlers, h)
	i := len(e.handlers) - 1
	return func() {
		if i < len(e.handlers) {
			e.handlers[i] = nil
		}
	}
}

// Emit 同步分发。handler 在调用 goroutine 上执行，必须自行保证非阻塞。
func (e *Emitter) Emit(ev Event) {
	for _, h := range e.handlers {
		if h != nil {
			h(ev)
		}
	}
}

// CompactionSkipEvent 表示压缩检查跑过了，但 kernel 没有给出可压区间。
//
// 不是错误：上下文里还没有足够大的可压单元（新会话、或历史几乎都是一条
// 大消息）。单独成为一个事件，是为了让「水位到了但压不动」可观测，而不是
// 每轮抛一条 error notice。
type CompactionSkipEvent struct {
	// Reason 是触发检查的原因："threshold" / "overflow"。
	Reason string
	// Usage 是当时的占用比例（0..1）。
	Usage float64
}

func (CompactionSkipEvent) EventName() string { return "session_compact_skipped" }
