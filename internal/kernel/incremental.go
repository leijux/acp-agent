package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dop251/goja"
)

// 增量 / 轻量路径（②）。
//
// 实测 `Turn` 的开销分布（benchmark + pprof）：
//   - 输入侧：每条消息在 JS 里建一个对象（N 次 Set）
//   - 输出侧：整个 messages 视图做一次 JSON.stringify，再 json.Unmarshal 回 Go
//   - 另外 refs 还要独立做一次 JSON 编解码
//
// 对「每轮只想知道要不要压缩」的调用方，输出侧的两趟 JSON 是完全浪费的。
// TurnMeta 只回 nudge + refs（refs 直接从 goja 读，不过 JSON），
// 需要消息视图时再显式调 View()。

// MetaResult 是 TurnMeta 的结果。
type MetaResult struct {
	// Nudge 是 kernel 的压缩建议（nil 表示本轮没有建议）。
	Nudge *Nudge
	// NudgeJSON 是 Nudge 的原始 JSON。
	NudgeJSON json.RawMessage
	// TerminalEscape 原样透传。
	TerminalEscape json.RawMessage
	// TruncationSkipped 非空表示本轮跳过裁剪的原因。
	TruncationSkipped string
	// RefsByRaw 是 message id → ref 的映射。
	RefsByRaw map[string]string
}

// RefCount 返回 ref 表大小（≈ 已被 kernel 登记的消息数）。
func (m *MetaResult) RefCount() int { return len(m.RefsByRaw) }

// TurnMeta 是 Turn 的轻量版：推进 kernel 状态、返回 nudge 与 refs，
// 但**不**序列化渲染后的消息视图。
//
// 和 Turn 的差别只有输出侧；state/last 的推进与 Turn 一致，因此可以
// 在 TurnMeta 之后直接 ApplyCompression。
//
// tokenCount <= 0 时自动用 EstimateTokens(messages) 兜底。
func (s *Session) TurnMeta(messages []Message, tokenCount int) (*MetaResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if tokenCount <= 0 {
		tokenCount = EstimateTokens(messages)
	}

	vm := s.rt.vm
	in := vm.NewObject()
	_ = in.Set("messages", s.messagesToJS(messages))
	_ = in.Set("state", s.state)
	_ = in.Set("config", s.cfg)
	_ = in.Set("tokenCount", tokenCount)

	out, err := s.rt.coreCall("processTurn", in)
	if err != nil {
		return nil, fmt.Errorf("kernel: processTurn: %w", err)
	}
	obj := out.ToObject(vm)

	if st := obj.Get("state"); isValue(st) {
		s.state = st
	}
	if mv := obj.Get("messages"); isValue(mv) {
		s.last = mv
	}

	res := &MetaResult{}
	if nj, err := s.rt.toJSON(obj.Get("nudge")); err == nil && len(nj) > 0 && string(nj) != "null" {
		res.NudgeJSON = nj
		var n Nudge
		if err := json.Unmarshal(nj, &n); err == nil {
			res.Nudge = &n
		}
	}
	if te, err := s.rt.toJSON(obj.Get("terminalEscape")); err == nil && len(te) > 0 && string(te) != "null" {
		res.TerminalEscape = te
	}
	if v := obj.Get("truncationSkipped"); isValue(v) {
		res.TruncationSkipped = v.String()
	}
	res.RefsByRaw = readStringMap(s.state.ToObject(vm).Get("messageRefs"), "byRaw")
	return res, nil
}

// View 把最近一次 processTurn 的渲染视图转成 Go 消息（按需付费）。
//
// 这是 TurnMeta 的配套调用：只有当调用方确实需要消息文本时才付这趟 JSON。
func (s *Session) View() ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if !isValue(s.last) {
		return nil, nil
	}
	raw, err := s.rt.toJSON(s.last)
	if err != nil {
		return nil, fmt.Errorf("kernel: encode messages view: %w", err)
	}
	var msgs []Message
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("kernel: decode messages view: %w", err)
		}
	}
	refs, err := s.refsLocked()
	if err == nil && len(refs.ByRaw) > 0 {
		for i := range msgs {
			msgs[i].Ref = refs.ByRaw[msgs[i].ID]
		}
	}
	return msgs, nil
}

func isValue(v goja.Value) bool {
	return v != nil && !goja.IsUndefined(v) && !goja.IsNull(v)
}

// BatchTurn 是批量提交的一项。
type BatchTurn struct {
	Messages []Message
	Tokens   int
}

// BatchResult 是 TurnBatch 的一项结果。
type BatchResult struct {
	Meta *MetaResult
	View []Message
}

// TurnBatch 连续处理多组消息。
//
// 只要每项成功后把 nudge 回给调用方，遇到 ShouldInject 就停下（把决策权交回），
// 因为再跑下去只会浪费一轮：kernel 已经在提示该压缩了。
// ctx 取消或任一项失败时，返回已处理项与错误。
func (s *Session) TurnBatch(ctx context.Context, items []BatchTurn) ([]BatchResult, error) {
	out := make([]BatchResult, 0, len(items))
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("kernel: batch[%d]: %w", i, err)
		}
		meta, err := s.TurnMeta(it.Messages, it.Tokens)
		if err != nil {
			return out, fmt.Errorf("kernel: batch[%d]: %w", i, err)
		}
		view, err := s.View()
		if err != nil {
			return out, fmt.Errorf("kernel: batch[%d]: %w", i, err)
		}
		out = append(out, BatchResult{Meta: meta, View: view})
		if meta.Nudge != nil && meta.Nudge.ShouldInject {
			return out, nil
		}
	}
	return out, nil
}
