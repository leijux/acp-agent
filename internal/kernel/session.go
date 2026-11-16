package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/MoeForge/moejs"
)

// ErrClosed 表示 Session 已关闭。
var ErrClosed = errors.New("kernel: session closed")

// Session 是持有 kernel state 的一次会话。
//
// 并发安全（内部 mutex 串行化所有 JS 调用）。多个 Session 可并行，
// 彼此不共享 state 与 runtime。
type Session struct {
	rt    *runtime
	cfg   moejs.Value
	state moejs.Value
	last  moejs.Value // 上一轮渲染后的 messages（applyCompression 需要）
	limit int

	mu     sync.Mutex
	closed bool
}

func (s *Session) init(cfg Config) error {
	rt := s.rt

	cfgFn := rt.prop(rt.exports, "defaultConfig")
	if !cfgFn.IsObject() {
		return errors.New("kernel: defaultConfig is not exported")
	}
	args := []moejs.Value{rt.val(s.limit)}
	if len(cfg.Overrides) > 0 {
		args = append(args, rt.val(cfg.Overrides))
	}
	v, err := rt.callFn(cfgFn, args...)
	if err != nil {
		return fmt.Errorf("kernel: defaultConfig: %w", err)
	}
	s.cfg = v

	stateFn := rt.prop(rt.exports, "createInitialState")
	if !stateFn.IsObject() {
		return errors.New("kernel: createInitialState is not exported")
	}
	st, err := rt.callFn(stateFn)
	if err != nil {
		return fmt.Errorf("kernel: createInitialState: %w", err)
	}
	s.state = st
	s.last = rt.val([]any{})
	return nil
}

// Close 释放会话资源；之后的调用返回 ErrClosed。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.state = moejs.Undefined()
	s.last = moejs.Undefined()
	s.cfg = moejs.Undefined()
	return nil
}

func (s *Session) ensureOpen() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

// jsMessage 是传给 JS 的消息形状。
//
// 字段顺序是**契约**：kernel 会把消息透传/拷贝进渲染结果，渲染后的 JSON
// 的 key 顺序参与 messagesHash 与上游的逐字节比对。以前是逐个 o.Set(...)
// 建对象（插入顺序），现在走 json.Marshal + JSON.parse，所以顺序只能靠
// struct 字段顺序固定 —— 调整字段等于改上游比对基准。
type jsMessage struct {
	ID               string      `json:"id"`
	Role             Role        `json:"role"`
	ContentType      ContentType `json:"contentType"`
	Text             string      `json:"text"`
	ToolName         string      `json:"toolName,omitempty"`
	ToolCallID       string      `json:"toolCallId,omitempty"`
	ThinkingTokens   int         `json:"thinkingTokens,omitempty"`
	SummaryOfBlockID string      `json:"summaryOfBlockId,omitempty"`
	LedgerSeq        int64       `json:"ledgerSeq,omitempty"`
}

// messagesToJS 把消息视图转成 JS 数组（一次 JSON.parse，不在 JS 里逐个建对象）。
func (s *Session) messagesToJS(msgs []Message) moejs.Value {
	out := make([]jsMessage, 0, len(msgs))
	for _, m := range msgs {
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
		out = append(out, jsMessage{
			ID:               m.ID,
			Role:             role,
			ContentType:      ct,
			Text:             m.Text,
			ToolName:         m.ToolName,
			ToolCallID:       m.ToolCallID,
			ThinkingTokens:   m.ThinkingTokens,
			SummaryOfBlockID: m.SummaryOfBlockID,
			LedgerSeq:        m.LedgerSeq,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return moejs.Undefined()
	}
	return s.rt.parse(data)
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
// tokenCount 是宿主的上下文占用读数（读不到可传 0，kernel 会走估算），
// 它究竟是计费级采样还是本地估值由 TurnSource 声明；
// 不声明来源，等价于 TurnSource(…, TokenCountUnspecified)。
func (s *Session) Turn(msgs []Message, tokenCount int) (TurnResult, error) {
	return s.TurnSource(msgs, tokenCount, TokenCountUnspecified)
}

// TurnSource 是 Turn 的带来源版本：告诉 kernel 这个 tokenCount 是计费级采样
// 还是本地估值，它决定压力档 nudge 要不要等一个 usage 级读数（上游幻压守卫）。
//
// 据实声明很重要：估值冒充 usage 会让 kernel 在不该动手的时候动手；而声明
// estimate 的代价有限 —— 上游只在同一会话的前 maxEstimateHoldRounds 次
// estimate 调用上押住压力档，之后自动退化为旧行为（详见 TokenCountSource）。
func (s *Session) TurnSource(msgs []Message, tokenCount int, source TokenCountSource) (TurnResult, error) {
	if !source.valid() {
		return TurnResult{}, fmt.Errorf("kernel: unknown TokenCountSource %q（只接受 \"\" / usage / estimate）", string(source))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return TurnResult{}, err
	}

	in := map[string]any{
		"messages":   s.messagesToJS(msgs),
		"state":      s.state,
		"config":     s.cfg,
		"tokenCount": tokenCount,
	}
	if source != TokenCountUnspecified {
		in["tokenCountSource"] = string(source)
	}

	out, err := s.rt.coreCall("processTurn", s.rt.val(in))
	if err != nil {
		return TurnResult{}, fmt.Errorf("kernel: processTurn: %w", err)
	}

	if st := s.rt.prop(out, "state"); !st.IsNullish() {
		s.state = st
	}
	if mv := s.rt.prop(out, "messages"); !mv.IsNullish() {
		s.last = mv
	}

	var res TurnResult
	msgJSON, err := s.rt.toJSON(s.rt.prop(out, "messages"))
	if err != nil {
		return TurnResult{}, fmt.Errorf("kernel: processTurn: encode messages: %w", err)
	}
	if len(msgJSON) > 0 {
		if err := json.Unmarshal(msgJSON, &res.Messages); err != nil {
			return TurnResult{}, fmt.Errorf("kernel: processTurn: decode messages: %w", err)
		}
		res.MessagesJSON = msgJSON
	}
	if res.NudgeJSON, err = s.rt.toJSON(s.rt.prop(out, "nudge")); err != nil {
		return TurnResult{}, err
	}
	if len(res.NudgeJSON) > 0 && string(res.NudgeJSON) != "null" {
		var n Nudge
		if err := json.Unmarshal(res.NudgeJSON, &n); err == nil {
			res.Nudge = &n
		}
	}
	if res.TerminalEscape, err = s.rt.toJSON(s.rt.prop(out, "terminalEscape")); err != nil {
		return TurnResult{}, err
	}
	if v := s.rt.prop(out, "truncationSkipped"); !v.IsNullish() {
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
	rs := make([]any, 0, len(ranges))
	for _, r := range ranges {
		o := map[string]any{
			"startRef": r.StartRef,
			"endRef":   r.EndRef,
			"summary":  r.Summary,
		}
		if r.Topic != "" {
			o["topic"] = r.Topic
		}
		if r.SummaryMaxChars > 0 {
			o["summaryMaxChars"] = r.SummaryMaxChars
		}
		rs = append(rs, o)
	}

	in := map[string]any{
		"ranges":   rs,
		"messages": s.last,
		"state":    s.state,
		"config":   s.cfg,
	}

	out, err := s.rt.coreCall("applyCompression", s.rt.val(in))
	if err != nil {
		return CompressionResult{}, fmt.Errorf("kernel: applyCompression: %w", err)
	}
	if st := s.rt.prop(out, "state"); !st.IsNullish() {
		s.state = st
	}

	var res CompressionResult
	if data, err := s.rt.toJSON(s.rt.prop(out, "result")); err == nil && len(data) > 0 {
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
	out, err := s.rt.coreCall("search", s.rt.val(query), s.state)
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
	bd := rt.prop(rt.exports, "blockDocs")
	if !bd.IsObject() {
		return nil, errors.New("kernel: blockDocs is not exported")
	}
	docs, err := rt.callFn(bd, s.state)
	if err != nil {
		return nil, fmt.Errorf("kernel: blockDocs: %w", err)
	}
	sb := rt.prop(rt.exports, "searchBlocks")
	if !sb.IsObject() {
		return nil, errors.New("kernel: searchBlocks is not exported")
	}
	args := []moejs.Value{docs, rt.val(query)}
	if len(options) > 0 {
		args = append(args, rt.val(options))
	}
	hits, err := rt.callFn(sb, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: searchBlocks: %w", err)
	}
	return s.toRawSlice(hits)
}

func (s *Session) toRawSlice(v moejs.Value) ([]json.RawMessage, error) {
	if v.IsNullish() {
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
	out, err := s.rt.coreCall("status", s.state, s.rt.val(tokenCount), s.cfg)
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
	out, err := s.rt.coreCall("decompress", s.rt.val(blockID), s.state)
	if err != nil {
		return nil, fmt.Errorf("kernel: decompress: %w", err)
	}
	if out.IsNullish() {
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
	v := s.rt.prop(s.state, "blocks")
	if v.IsNullish() {
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
	v := s.rt.prop(s.state, "blocks")
	if v.IsNullish() {
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
	v := s.rt.prop(s.state, "messageRefs")
	if !isValue(v) {
		return out, nil
	}
	// 直接从引擎对象读：refs 表随消息数线性增长，每轮做一次
	// stringify → parse → unmarshal 是纯浪费（pprof 里 refs 曾是独立热点）。
	out.ByRaw = readStringMap(s.rt, v, "byRaw")
	out.ByRef = readStringMap(s.rt, v, "byRef")
	if blocked := readBoolMap(s.rt, v, "blocked"); len(blocked) > 0 {
		out.Blocked = blocked
	}
	return out, nil
}

// readStringMap 读取 JS 对象的某个子对象为 Go map（不经过 JSON）。
func readStringMap(rt *runtime, v moejs.Value, key string) map[string]string {
	sub := rt.prop(v, key)
	if !sub.IsObject() {
		return nil
	}
	x, err := rt.rt.ToGo(sub)
	if err != nil {
		return nil
	}
	m, ok := x.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprint(val)
	}
	return out
}

func readBoolMap(rt *runtime, v moejs.Value, key string) map[string]bool {
	sub := rt.prop(v, key)
	if !sub.IsObject() {
		return nil
	}
	x, err := rt.rt.ToGo(sub)
	if err != nil {
		return nil
	}
	m, ok := x.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]bool, len(m))
	for k, val := range m {
		b, _ := val.(bool)
		out[k] = b
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
	vals := make([]moejs.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, s.rt.val(a))
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
	vals := make([]moejs.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, s.rt.val(a))
	}
	v, err := s.rt.call(name, vals...)
	if err != nil {
		return nil, err
	}
	return s.rt.toJSON(v)
}
