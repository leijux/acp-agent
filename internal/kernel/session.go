package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// ErrClosed 表示 Session 已关闭。
var ErrClosed = errors.New("kernel: session closed")

// Session 是持有 kernel state 的一次会话。
//
// 并发安全（内部 mutex 串行化所有 JS 调用）。多个 Session 可并行，
// 彼此不共享 state 与 runtime。
type Session struct {
	rt    *runtime
	cfg   goja.Value
	state goja.Value
	last  goja.Value // 上一轮渲染后的 messages（applyCompression 需要）
	limit int

	mu     sync.Mutex
	closed bool
}

func (s *Session) init(cfg Config) error {
	rt := s.rt
	vm := rt.vm

	cfgFn, ok := goja.AssertFunction(rt.exports.Get("defaultConfig"))
	if !ok {
		return errors.New("kernel: defaultConfig is not exported")
	}
	args := []goja.Value{vm.ToValue(s.limit)}
	if len(cfg.Overrides) > 0 {
		args = append(args, vm.ToValue(cfg.Overrides))
	}
	v, err := rt.callFn(cfgFn, args...)
	if err != nil {
		return fmt.Errorf("kernel: defaultConfig: %w", err)
	}
	s.cfg = v

	stateFn, ok := goja.AssertFunction(rt.exports.Get("createInitialState"))
	if !ok {
		return errors.New("kernel: createInitialState is not exported")
	}
	st, err := rt.callFn(stateFn)
	if err != nil {
		return fmt.Errorf("kernel: createInitialState: %w", err)
	}
	s.state = st
	s.last = vm.NewArray()
	return nil
}

// Close 释放会话资源；之后的调用返回 ErrClosed。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.state = nil
	s.last = nil
	s.cfg = nil
	return nil
}

func (s *Session) ensureOpen() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Session) messagesToJS(msgs []Message) *goja.Object {
	vm := s.rt.vm
	arr := vm.NewArray()
	for i, m := range msgs {
		o := vm.NewObject()
		_ = o.Set("id", m.ID)
		role := m.Role
		if role == "" {
			role = RoleUser
		}
		ct := m.ContentType
		if ct == "" {
			if role == RoleTool {
				ct = ContentToolResult
			} else {
				ct = ContentText
			}
		}
		_ = o.Set("role", string(role))
		_ = o.Set("contentType", string(ct))
		_ = o.Set("text", m.Text)
		if m.ToolName != "" {
			_ = o.Set("toolName", m.ToolName)
		}
		if m.ToolCallID != "" {
			_ = o.Set("toolCallId", m.ToolCallID)
		}
		if m.ThinkingTokens > 0 {
			_ = o.Set("thinkingTokens", m.ThinkingTokens)
		}
		if m.SummaryOfBlockID != "" {
			_ = o.Set("summaryOfBlockId", m.SummaryOfBlockID)
		}
		if m.LedgerSeq > 0 {
			_ = o.Set("ledgerSeq", m.LedgerSeq)
		}
		_ = arr.Set(intToKey(i), o)
	}
	return arr
}

type turnRaw struct {
	Messages          []Message       `json:"messages"`
	Nudge             json.RawMessage `json:"nudge,omitempty"`
	TerminalEscape    json.RawMessage `json:"terminalEscape,omitempty"`
	TruncationSkipped string          `json:"truncationSkipped,omitempty"`
	State             json.RawMessage `json:"state,omitempty"`
}

// Turn 执行一轮上下文处理：裁剪、折叠压缩块、渲染引用标签、决定是否提示压缩。
//
// tokenCount 是宿主的真实上下文占用（读不到可传 0，kernel 会走估算）。
func (s *Session) Turn(msgs []Message, tokenCount int) (TurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return TurnResult{}, err
	}

	vm := s.rt.vm
	in := vm.NewObject()
	_ = in.Set("messages", s.messagesToJS(msgs))
	_ = in.Set("state", s.state)
	_ = in.Set("config", s.cfg)
	_ = in.Set("tokenCount", tokenCount)

	out, err := s.rt.coreCall("processTurn", in)
	if err != nil {
		return TurnResult{}, fmt.Errorf("kernel: processTurn: %w", err)
	}
	obj := out.ToObject(vm)

	if st := obj.Get("state"); st != nil && !goja.IsUndefined(st) && !goja.IsNull(st) {
		s.state = st
	}
	if mv := obj.Get("messages"); mv != nil && !goja.IsUndefined(mv) {
		s.last = mv
	}

	var res TurnResult
	msgJSON, err := s.rt.toJSON(obj.Get("messages"))
	if err != nil {
		return TurnResult{}, fmt.Errorf("kernel: processTurn: encode messages: %w", err)
	}
	if len(msgJSON) > 0 {
		if err := json.Unmarshal(msgJSON, &res.Messages); err != nil {
			return TurnResult{}, fmt.Errorf("kernel: processTurn: decode messages: %w", err)
		}
		res.MessagesJSON = msgJSON
	}
	if res.NudgeJSON, err = s.rt.toJSON(obj.Get("nudge")); err != nil {
		return TurnResult{}, err
	}
	if len(res.NudgeJSON) > 0 && string(res.NudgeJSON) != "null" {
		var n Nudge
		if err := json.Unmarshal(res.NudgeJSON, &n); err == nil {
			res.Nudge = &n
		}
	}
	if res.TerminalEscape, err = s.rt.toJSON(obj.Get("terminalEscape")); err != nil {
		return TurnResult{}, err
	}
	if v := obj.Get("truncationSkipped"); v != nil && !goja.IsUndefined(v) {
		res.TruncationSkipped = v.String()
	}

	refs, err := s.refsLocked()
	if err != nil {
		return TurnResult{}, err
	}
	res.Refs = refs
	for i := range res.Messages {
		res.Messages[i].Ref = refs.ByRaw[res.Messages[i].ID]
	}
	return res, nil
}

// ApplyCompression 应用一组压缩区间，产出一个或多个压缩块。
//
// 必须在 Turn 之后调用（需要上一轮渲染后的消息视图）。
func (s *Session) ApplyCompression(ranges []Range) (CompressionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return CompressionResult{}, err
	}
	vm := s.rt.vm

	rs := vm.NewArray()
	for i, r := range ranges {
		o := vm.NewObject()
		_ = o.Set("startRef", r.StartRef)
		_ = o.Set("endRef", r.EndRef)
		_ = o.Set("summary", r.Summary)
		if r.Topic != "" {
			_ = o.Set("topic", r.Topic)
		}
		if r.SummaryMaxChars > 0 {
			_ = o.Set("summaryMaxChars", r.SummaryMaxChars)
		}
		_ = rs.Set(intToKey(i), o)
	}

	in := vm.NewObject()
	_ = in.Set("ranges", rs)
	_ = in.Set("messages", s.last)
	_ = in.Set("state", s.state)
	_ = in.Set("config", s.cfg)

	out, err := s.rt.coreCall("applyCompression", in)
	if err != nil {
		return CompressionResult{}, fmt.Errorf("kernel: applyCompression: %w", err)
	}
	obj := out.ToObject(vm)
	if st := obj.Get("state"); st != nil && !goja.IsUndefined(st) && !goja.IsNull(st) {
		s.state = st
	}

	var res CompressionResult
	if data, err := s.rt.toJSON(obj.Get("result")); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &res); err != nil {
			return CompressionResult{}, fmt.Errorf("kernel: applyCompression: decode result: %w", err)
		}
	}
	blocks, err := s.blocksLocked()
	if err != nil {
		return CompressionResult{}, err
	}
	active := blocks[:0]
	for _, b := range blocks {
		if b.Active {
			active = append(active, b)
		}
	}
	res.Blocks = active
	return res, nil
}

// Search 在已压缩的历史中检索（kernel 的 search 原语）。
func (s *Session) Search(query string) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	out, err := s.rt.coreCall("search", s.rt.vm.ToValue(query), s.state)
	if err != nil {
		return nil, fmt.Errorf("kernel: search: %w", err)
	}
	return s.toRawSlice(out)
}

// SearchBlocks 走词法检索管道（对压缩块）。默认算法与上游一致（hybrid）。
func (s *Session) SearchBlocks(query string) ([]json.RawMessage, error) {
	return s.SearchBlocksWith(query, nil)
}

// SearchBlocksWith 可指定算法，例如 {"algorithm": "bm25"} / {"algorithm": "substring"}。
func (s *Session) SearchBlocksWith(query string, options map[string]any) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	rt := s.rt
	bd, ok := goja.AssertFunction(rt.exports.Get("blockDocs"))
	if !ok {
		return nil, errors.New("kernel: blockDocs is not exported")
	}
	docs, err := rt.callFn(bd, s.state)
	if err != nil {
		return nil, fmt.Errorf("kernel: blockDocs: %w", err)
	}
	sb, ok := goja.AssertFunction(rt.exports.Get("searchBlocks"))
	if !ok {
		return nil, errors.New("kernel: searchBlocks is not exported")
	}
	args := []goja.Value{docs, rt.vm.ToValue(query)}
	if len(options) > 0 {
		args = append(args, rt.vm.ToValue(options))
	}
	hits, err := rt.callFn(sb, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: searchBlocks: %w", err)
	}
	return s.toRawSlice(hits)
}

func (s *Session) toRawSlice(v goja.Value) ([]json.RawMessage, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	data, err := s.rt.toJSON(v)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Status 报告上下文占用情况。
func (s *Session) Status(tokenCount int) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return Status{}, err
	}
	vm := s.rt.vm
	out, err := s.rt.coreCall("status", s.state, vm.ToValue(tokenCount), s.cfg)
	if err != nil {
		return Status{}, fmt.Errorf("kernel: status: %w", err)
	}
	var st Status
	data, err := s.rt.toJSON(out)
	if err != nil {
		return Status{}, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Decompress 读取一个压缩块的原文投影（不改变 state）。块不存在或已失效时返回 nil。
func (s *Session) Decompress(blockID string) (*Block, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	out, err := s.rt.coreCall("decompress", s.rt.vm.ToValue(blockID), s.state)
	if err != nil {
		return nil, fmt.Errorf("kernel: decompress: %w", err)
	}
	if out == nil || goja.IsUndefined(out) || goja.IsNull(out) {
		return nil, nil
	}
	var b Block
	data, err := s.rt.toJSON(out)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// Blocks 返回所有压缩块。
func (s *Session) Blocks() ([]Block, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	return s.blocksLocked()
}

func (s *Session) blocksLocked() ([]Block, error) {
	st := s.state.ToObject(s.rt.vm)
	v := st.Get("blocks")
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	data, err := s.rt.toJSON(v)
	if err != nil {
		return nil, err
	}
	var out []Block
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("kernel: decode blocks: %w", err)
	}
	return out, nil
}

// BlocksJSON 返回 blocks 的原始 JS 序列化结果（字段顺序与上游一致，可做回归比对）。
func (s *Session) BlocksJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	v := s.state.ToObject(s.rt.vm).Get("blocks")
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return []byte("[]"), nil
	}
	return s.rt.toJSON(v)
}

// Refs 返回 id ↔ 引用映射。
func (s *Session) Refs() (RefMap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return RefMap{}, err
	}
	return s.refsLocked()
}

func (s *Session) refsLocked() (RefMap, error) {
	var out RefMap
	st := s.state.ToObject(s.rt.vm)
	v := st.Get("messageRefs")
	if !isValue(v) {
		return out, nil
	}
	// 直接从 goja 对象读：refs 表随消息数线性增长，每轮做一次
	// stringify → parse → unmarshal 是纯浪费（pprof 里 refs 曾是独立热点）。
	out.ByRaw = readStringMap(v, "byRaw")
	out.ByRef = readStringMap(v, "byRef")
	if blocked := readBoolMap(v, "blocked"); len(blocked) > 0 {
		out.Blocked = blocked
	}
	return out, nil
}

// readStringMap 读取 JS 对象的某个子对象为 Go map（不经过 JSON）。
func readStringMap(v goja.Value, key string) map[string]string {
	obj, ok := v.(*goja.Object)
	if !ok || obj == nil {
		return nil
	}
	sub, ok := obj.Get(key).(*goja.Object)
	if !ok || sub == nil {
		return nil
	}
	keys := sub.Keys()
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = sub.Get(k).String()
	}
	return out
}

func readBoolMap(v goja.Value, key string) map[string]bool {
	obj, ok := v.(*goja.Object)
	if !ok || obj == nil {
		return nil
	}
	sub, ok := obj.Get(key).(*goja.Object)
	if !ok || sub == nil {
		return nil
	}
	keys := sub.Keys()
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = sub.Get(k).ToBoolean()
	}
	return out
}

// StateJSON 导出 state（可直接落盘/入库）。
func (s *Session) StateJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	return s.rt.toJSON(s.state)
}

// LoadState 载入先前导出的 state，恢复会话。
func (s *Session) LoadState(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	v, err := s.rt.fromJSON(data)
	if err != nil {
		return fmt.Errorf("kernel: load state: %w", err)
	}
	s.state = v
	return nil
}

// SetMessages 注入宿主自行渲染的消息视图（用于 LoadState 后第一次 ApplyCompression）。
func (s *Session) SetMessages(msgs []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = s.messagesToJS(msgs)
}

// CallCore 直接调用 core 上的方法，返回原始 JSON（逃生门）。
func (s *Session) CallCore(name string, args ...any) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	vals := make([]goja.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, s.rt.vm.ToValue(a))
	}
	v, err := s.rt.coreCall(name, vals...)
	if err != nil {
		return nil, err
	}
	return s.rt.toJSON(v)
}

// Call 直接调用 bundle 导出函数，返回原始 JSON（逃生门）。
func (s *Session) Call(name string, args ...any) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	vals := make([]goja.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, s.rt.vm.ToValue(a))
	}
	v, err := s.rt.call(name, vals...)
	if err != nil {
		return nil, err
	}
	return s.rt.toJSON(v)
}
