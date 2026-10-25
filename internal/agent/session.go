package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EntryType 是会话树节点类型。
type EntryType string

const (
	EntryMessage       EntryType = "message"
	EntryCompaction    EntryType = "compaction"
	EntryBranchSummary EntryType = "branch_summary"
	EntryModelChange   EntryType = "model_change"
)

// Entry 是会话树的一个节点。每个 entry 有 id 与 parentId，从 leaf 回溯即活动分支。
type Entry struct {
	Type      EntryType `json:"type"`
	ID        string    `json:"id"`
	ParentID  *string   `json:"parentId"`
	Timestamp string    `json:"timestamp"`

	// type=message
	Message Message `json:"-"`

	// type=compaction
	Summary          string `json:"summary,omitempty"`
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int    `json:"tokensBefore,omitempty"`
	// BlockID 是 kernel 压缩块 id（新格式；旧格式只有 FirstKeptEntryID）。
	BlockID string `json:"blockId,omitempty"`
	// BlockedIDs 是被该块吃掉的 entry id。存在时投影改用「块感知」逻辑，
	// 不再依赖「最后一个 compaction 为界」的假设（kernel 可以压中间段）。
	BlockedIDs  []string `json:"blockedIds,omitempty"`
	TokensAfter int      `json:"tokensAfter,omitempty"`

	// type=branch_summary
	FromID string `json:"fromId,omitempty"`

	// type=model_change
	Model *Model `json:"model,omitempty"`
}

// Session 是会话树。并发安全（内部加锁），因为事件 handler 可能在其它 goroutine。
type Session struct {
	mu      sync.Mutex
	entries []*Entry
	byID    map[string]*Entry
	leafID  *string
	seq     int
	// kernelState 是压缩引擎（kernel）的 state 快照。随 JSONL 一起落盘，
	// 恢复后压缩块仍在，不会重复压同一段历史。
	kernelState []byte
	// OnAppend 在每次追加 entry 后调用（用于 JSONL 落盘）。
	OnAppend func(*Entry) error
}

// NewSession 创建空会话。
func NewSession() *Session {
	return &Session{byID: make(map[string]*Entry)}
}

// id 生成稳定可读的 entry id。
func (s *Session) id() string {
	s.seq++
	return fmt.Sprintf("e%d", s.seq)
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// append 追加 entry 到当前 leaf 之下，并推进 leaf。调用方需持锁。
func (s *Session) append(e *Entry) (*Entry, error) {
	if e.ID == "" {
		e.ID = s.id()
	}
	if e.Timestamp == "" {
		e.Timestamp = isoNow()
	}
	if e.ParentID == nil && s.leafID != nil {
		p := *s.leafID
		e.ParentID = &p
	}
	s.entries = append(s.entries, e)
	s.byID[e.ID] = e
	id := e.ID
	s.leafID = &id
	if s.OnAppend != nil {
		if err := s.OnAppend(e); err != nil {
			return e, err
		}
	}
	return e, nil
}

// Append 追加一条消息 entry。
func (s *Session) Append(m Message) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(&Entry{Type: EntryMessage, Message: m})
}

// AppendCompaction 追加旧式压缩 entry（全量前缀摘要，靠 FirstKeptEntryID 定界）。
// 新代码应使用 AppendCompactionBlock。
func (s *Session) AppendCompaction(summary, firstKeptID string, tokensBefore int) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(&Entry{
		Type:             EntryCompaction,
		Summary:          summary,
		FirstKeptEntryID: firstKeptID,
		TokensBefore:     tokensBefore,
	})
}

// AppendCompactionBlock 追加一个 kernel 压缩块对应的 entry。
//
// blockedIDs 是被该块折叠掉的 entry id（来自 kernel 的 Block.DirectMessageIDs）。
func (s *Session) AppendCompactionBlock(blockID, summary string, blockedIDs []string, tokensBefore, tokensAfter int) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := ""
	if len(blockedIDs) > 0 {
		first = blockedIDs[0]
	}
	return s.append(&Entry{
		Type:             EntryCompaction,
		Summary:          summary,
		FirstKeptEntryID: first,
		TokensBefore:     tokensBefore,
		TokensAfter:      tokensAfter,
		BlockID:          blockID,
		BlockedIDs:       append([]string(nil), blockedIDs...),
	})
}

// SetKernelState 保存压缩引擎的 state 快照（随 JSONL 落盘）。
func (s *Session) SetKernelState(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kernelState = append([]byte(nil), b...)
}

// KernelState 返回压缩引擎 state 快照（未设置时为 nil）。
func (s *Session) KernelState() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.kernelState...)
}

// AppendBranchSummary 追加分支摘要 entry。
func (s *Session) AppendBranchSummary(summary, fromID string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(&Entry{Type: EntryBranchSummary, Summary: summary, FromID: fromID})
}

// AppendModelChange 记录模型切换。
func (s *Session) AppendModelChange(m Model) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(&Entry{Type: EntryModelChange, Model: &m})
}

// LeafID 返回当前活动分支末端 entry id。
func (s *Session) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leafID == nil {
		return ""
	}
	return *s.leafID
}

// SetLeaf 切换活动分支到指定 entry（用于 /tree 式导航）。
func (s *Session) SetLeaf(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("agent: entry %q 不存在", id)
	}
	v := id
	s.leafID = &v
	return nil
}

// Entries 返回全部 entry 的浅拷贝。
func (s *Session) Entries() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Entry(nil), s.entries...)
}

// GetEntry 按 id 查询。
func (s *Session) GetEntry(id string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	return e, ok
}

// ActiveBranch 返回从根到 leaf 的 entry 序列。
func (s *Session) ActiveBranch() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeBranchLocked()
}

func (s *Session) activeBranchLocked() []*Entry {
	if s.leafID == nil {
		return nil
	}
	var rev []*Entry
	cur := s.leafID
	for cur != nil {
		e, ok := s.byID[*cur]
		if !ok {
			break
		}
		rev = append(rev, e)
		cur = e.ParentID
	}
	out := make([]*Entry, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// Messages 返回活动分支投影出的模型可见消息。
//
// 两种压缩格式：
//
//   - 块格式（kernel）：compaction entry 带 BlockedIDs，逐块把它吃掉的 entry
//     换成一条摘要消息，插在该块首个被吃 entry 的位置。kernel 可以压中段，
//     所以不能再假设「最后一个 compaction 为界」。
//   - 旧格式：以分支上最后一个 compaction entry 为界，它之前的不再发送，
//     改用它自己的 summary；从 firstKeptEntryId 起的照常发送。
func (s *Session) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messagesLocked()
}

func (s *Session) messagesLocked() []Message {
	branch := s.activeBranchLocked()

	// 块格式（kernel 驱动）。
	if insert, blocked := s.compactionBlocksLocked(branch); len(insert) > 0 {
		var out []Message
		for i, e := range branch {
			for _, c := range insert[i] {
				out = append(out, &CompactionSummaryMessage{
					Summary:      c.Summary,
					TokensBefore: c.TokensBefore,
					At:           mustParseISO(c.Timestamp),
				})
			}
			if blocked[e.ID] {
				continue
			}
			if m := projectEntry(e); m != nil {
				out = append(out, m)
			}
		}
		return out
	}

	// 旧格式。
	lastCompact := -1
	for i, e := range branch {
		if e.Type == EntryCompaction {
			lastCompact = i
		}
	}

	var out []Message
	start := 0
	if lastCompact >= 0 {
		c := branch[lastCompact]
		out = append(out, &CompactionSummaryMessage{
			Summary:      c.Summary,
			TokensBefore: c.TokensBefore,
			At:           mustParseISO(c.Timestamp),
		})
		idx := -1
		for i, e := range branch {
			if e.ID == c.FirstKeptEntryID {
				idx = i
				break
			}
		}
		if idx < 0 {
			// 保留起点丢失时退化为「压缩 entry 之后」。
			idx = lastCompact + 1
		}
		start = idx
	}

	for i := start; i < len(branch); i++ {
		if m := projectEntry(branch[i]); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// compactionBlocksLocked 返回活动分支上的压缩块：
//
//	insert[i]  = 应在 branch[i] 之前插入摘要的块（按创建顺序）
//	blocked[e.ID] = true 表示该 entry 已被某块吃掉
func (s *Session) compactionBlocksLocked(branch []*Entry) (map[int][]*Entry, map[string]bool) {
	if len(branch) == 0 {
		return nil, nil
	}
	idx := make(map[string]int, len(branch))
	for i, e := range branch {
		idx[e.ID] = i
	}
	insert := make(map[int][]*Entry)
	blocked := make(map[string]bool)
	for _, e := range branch {
		if e.Type != EntryCompaction || len(e.BlockedIDs) == 0 {
			continue
		}
		min := -1
		for _, id := range e.BlockedIDs {
			blocked[id] = true
			if i, ok := idx[id]; ok && (min < 0 || i < min) {
				min = i
			}
		}
		if min >= 0 {
			insert[min] = append(insert[min], e)
		}
	}
	if len(insert) == 0 {
		return nil, nil
	}
	return insert, blocked
}

// projectEntry 把一个 entry 转成模型可见消息；无消息的 entry 返回 nil。
func projectEntry(e *Entry) Message {
	switch e.Type {
	case EntryMessage:
		return e.Message
	case EntryBranchSummary:
		var from *string
		if e.FromID != "" {
			f := e.FromID
			from = &f
		}
		return &BranchSummaryMessage{
			Summary: e.Summary, FromID: from, At: mustParseISO(e.Timestamp),
		}
	}
	return nil
}

func mustParseISO(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", s)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// Fork 从指定 entry 开新分支：把 leaf 指向它，后续 Append 会成为它的子节点。
// 这与 Pi 的 /tree 导航 + 继续对话等价。
func (s *Session) Fork(entryID string) error { return s.SetLeaf(entryID) }

// ---------- 持久化 ----------

// entryDisk 是 JSONL 落盘格式。消息体用判别联合编码。
type entryDisk struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	ParentID         *string         `json:"parentId"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message,omitempty"`
	Summary          string          `json:"summary,omitempty"`
	FirstKeptEntryID string          `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int             `json:"tokensBefore,omitempty"`
	BlockID          string          `json:"blockId,omitempty"`
	BlockedIDs       []string        `json:"blockedIds,omitempty"`
	TokensAfter      int             `json:"tokensAfter,omitempty"`
	FromID           string          `json:"fromId,omitempty"`
	Model            *Model          `json:"model,omitempty"`
}

type messageDisk struct {
	Role string          `json:"role"`
	Body json.RawMessage `json:"body"`
}

// Save 把会话树写成 JSONL（每行一个 entry）。写入是原子的：先写临时文件再 rename。
func (s *Session) Save(path string) error {
	s.mu.Lock()
	entries := append([]*Entry(nil), s.entries...)
	leaf := ""
	if s.leafID != nil {
		leaf = *s.leafID
	}
	kernelState := append([]byte(nil), s.kernelState...)
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		d := entryDisk{
			Type: string(e.Type), ID: e.ID, ParentID: e.ParentID, Timestamp: e.Timestamp,
			Summary: e.Summary, FirstKeptEntryID: e.FirstKeptEntryID,
			TokensBefore: e.TokensBefore, FromID: e.FromID, Model: e.Model,
			BlockID: e.BlockID, BlockedIDs: e.BlockedIDs, TokensAfter: e.TokensAfter,
		}
		if e.Message != nil {
			raw, err := marshalMessage(e.Message)
			if err != nil {
				f.Close()
				return err
			}
			d.Message = raw
		}
		line, err := json.Marshal(d)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	// 最后一行记录 leaf，方便恢复活动分支。
	if leaf != "" {
		line, _ := json.Marshal(map[string]string{"type": "leaf", "id": leaf})
		w.Write(append(line, '\n'))
	}
	// 压缩引擎 state（存在时）单独一行，不参与 entry 树。
	if len(kernelState) > 0 {
		line, err := json.Marshal(map[string]any{"type": "kernel_state", "state": kernelState})
		if err == nil {
			w.Write(append(line, '\n'))
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSession 读取 JSONL 会话。
func LoadSession(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := NewSession()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, fmt.Errorf("agent: 解析会话行失败: %w", err)
		}
		if probe.Type == "leaf" {
			var l struct {
				ID string `json:"id"`
			}
			json.Unmarshal(line, &l)
			s.seq++
			s.leafID = &l.ID
			continue
		}
		if probe.Type == "kernel_state" {
			var k struct {
				State []byte `json:"state"`
			}
			if err := json.Unmarshal(line, &k); err == nil {
				s.kernelState = k.State
			}
			continue
		}
		var d entryDisk
		if err := json.Unmarshal(line, &d); err != nil {
			return nil, fmt.Errorf("agent: 解析 entry 失败: %w", err)
		}
		e := &Entry{
			Type: EntryType(d.Type), ID: d.ID, ParentID: d.ParentID, Timestamp: d.Timestamp,
			Summary: d.Summary, FirstKeptEntryID: d.FirstKeptEntryID,
			TokensBefore: d.TokensBefore, FromID: d.FromID, Model: d.Model,
			BlockID: d.BlockID, BlockedIDs: d.BlockedIDs, TokensAfter: d.TokensAfter,
		}
		if len(d.Message) > 0 {
			m, err := unmarshalMessage(d.Message)
			if err != nil {
				return nil, err
			}
			e.Message = m
		}
		s.entries = append(s.entries, e)
		s.byID[e.ID] = e
		s.seq++
		// 恢复 leaf：文件里没有 leaf 行时，用最后一个 entry。
		id := e.ID
		s.leafID = &id
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// ---------- 消息 JSON ----------

func marshalMessage(m Message) (json.RawMessage, error) {
	var body any
	switch v := m.(type) {
	case *SystemMessage:
		body = struct {
			Content   []json.RawMessage  `json:"content"`
			Sections  map[string]*string `json:"sections,omitempty"`
			Timestamp int64              `json:"timestamp"`
		}{marshalBlocks(v.Content), v.Sections, v.At}
	case *UserMessage:
		body = struct {
			Content   []json.RawMessage `json:"content"`
			Timestamp int64             `json:"timestamp"`
		}{marshalBlocks(v.Content), v.At}
	case *AssistantMessage:
		body = struct {
			Content       []json.RawMessage `json:"content"`
			Provider      string            `json:"provider,omitempty"`
			Model         string            `json:"model,omitempty"`
			API           string            `json:"api,omitempty"`
			ResponseID    string            `json:"responseId,omitempty"`
			Usage         Usage             `json:"usage"`
			StopReason    StopReason        `json:"stopReason"`
			ErrorMessage  string            `json:"errorMessage,omitempty"`
			RawStopReason string            `json:"rawStopReason,omitempty"`
			Timestamp     int64             `json:"timestamp"`
		}{marshalBlocks(v.Content), v.Provider, v.Model, v.API, v.ResponseID,
			v.Usage, v.StopReason, v.ErrorMessage, v.RawStopReason, v.At}
	case *ToolResultMessage:
		body = struct {
			ToolCallID string            `json:"toolCallId"`
			ToolName   string            `json:"toolName"`
			Content    []json.RawMessage `json:"content"`
			Details    any               `json:"details,omitempty"`
			IsError    bool              `json:"isError"`
			Timestamp  int64             `json:"timestamp"`
		}{v.ToolCallID, v.ToolName, marshalBlocks(v.Content), v.Details, v.IsError, v.At}
	case *BashExecutionMessage:
		body = struct {
			Command   string `json:"command"`
			Output    string `json:"output"`
			ExitCode  *int   `json:"exitCode,omitempty"`
			Cancelled bool   `json:"cancelled"`
			Truncated bool   `json:"truncated"`
			Timestamp int64  `json:"timestamp"`
		}{v.Command, v.Output, v.ExitCode, v.Cancelled, v.Truncated, v.At}
	case *CustomMessage:
		body = struct {
			CustomType string            `json:"customType"`
			Content    []json.RawMessage `json:"content"`
			Display    bool              `json:"display"`
			Timestamp  int64             `json:"timestamp"`
		}{v.CustomType, marshalBlocks(v.Content), v.Display, v.At}
	case *CompactionSummaryMessage:
		body = struct {
			Summary      string `json:"summary"`
			TokensBefore int    `json:"tokensBefore"`
			Timestamp    int64  `json:"timestamp"`
		}{v.Summary, v.TokensBefore, v.At}
	case *BranchSummaryMessage:
		body = struct {
			Summary   string  `json:"summary"`
			FromID    *string `json:"fromId"`
			Timestamp int64   `json:"timestamp"`
		}{v.Summary, v.FromID, v.At}
	default:
		return nil, fmt.Errorf("agent: 不支持序列化的消息类型 %T", m)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(messageDisk{Role: string(m.Role()), Body: raw})
}

func marshalBlocks(bs []Block) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(bs))
	for _, b := range bs {
		raw, err := MarshalBlock(b)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func unmarshalMessage(raw json.RawMessage) (Message, error) {
	var d messageDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	switch Role(d.Role) {
	case RoleSystem:
		var b struct {
			Content   []json.RawMessage  `json:"content"`
			Sections  map[string]*string `json:"sections,omitempty"`
			Timestamp int64              `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &SystemMessage{Content: parseBlocks(b.Content), Sections: b.Sections, At: b.Timestamp}, nil
	case RoleUser:
		var b struct {
			Content   []json.RawMessage `json:"content"`
			Timestamp int64             `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &UserMessage{Content: parseBlocks(b.Content), At: b.Timestamp}, nil
	case RoleAssistant:
		var b struct {
			Content       []json.RawMessage `json:"content"`
			Provider      string            `json:"provider"`
			Model         string            `json:"model"`
			API           string            `json:"api"`
			ResponseID    string            `json:"responseId"`
			Usage         Usage             `json:"usage"`
			StopReason    StopReason        `json:"stopReason"`
			ErrorMessage  string            `json:"errorMessage"`
			RawStopReason string            `json:"rawStopReason"`
			Timestamp     int64             `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &AssistantMessage{
			Content: parseBlocks(b.Content), Provider: b.Provider, Model: b.Model,
			API: b.API, ResponseID: b.ResponseID, Usage: b.Usage, StopReason: b.StopReason,
			ErrorMessage: b.ErrorMessage, RawStopReason: b.RawStopReason, At: b.Timestamp,
		}, nil
	case RoleToolResult:
		var b struct {
			ToolCallID string            `json:"toolCallId"`
			ToolName   string            `json:"toolName"`
			Content    []json.RawMessage `json:"content"`
			Details    any               `json:"details"`
			IsError    bool              `json:"isError"`
			Timestamp  int64             `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &ToolResultMessage{
			ToolCallID: b.ToolCallID, ToolName: b.ToolName, Content: parseBlocks(b.Content),
			Details: b.Details, IsError: b.IsError, At: b.Timestamp,
		}, nil
	case RoleBashExecution:
		var b struct {
			Command   string `json:"command"`
			Output    string `json:"output"`
			ExitCode  *int   `json:"exitCode"`
			Cancelled bool   `json:"cancelled"`
			Truncated bool   `json:"truncated"`
			Timestamp int64  `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &BashExecutionMessage{
			Command: b.Command, Output: b.Output, ExitCode: b.ExitCode,
			Cancelled: b.Cancelled, Truncated: b.Truncated, At: b.Timestamp,
		}, nil
	case RoleCustom:
		var b struct {
			CustomType string            `json:"customType"`
			Content    []json.RawMessage `json:"content"`
			Display    bool              `json:"display"`
			Timestamp  int64             `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &CustomMessage{CustomType: b.CustomType, Content: parseBlocks(b.Content), Display: b.Display, At: b.Timestamp}, nil
	case RoleCompactionSummary:
		var b struct {
			Summary      string `json:"summary"`
			TokensBefore int    `json:"tokensBefore"`
			Timestamp    int64  `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &CompactionSummaryMessage{Summary: b.Summary, TokensBefore: b.TokensBefore, At: b.Timestamp}, nil
	case RoleBranchSummary:
		var b struct {
			Summary   string  `json:"summary"`
			FromID    *string `json:"fromId"`
			Timestamp int64   `json:"timestamp"`
		}
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return nil, err
		}
		return &BranchSummaryMessage{Summary: b.Summary, FromID: b.FromID, At: b.Timestamp}, nil
	default:
		return nil, fmt.Errorf("agent: 未知消息 role=%q", d.Role)
	}
}

func parseBlocks(raws []json.RawMessage) []Block {
	out := make([]Block, 0, len(raws))
	for _, raw := range raws {
		b, err := UnmarshalBlock(raw)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}
