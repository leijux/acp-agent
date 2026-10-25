package acp

import (
	"fmt"
	"strings"
	"sync"

	"github.com/leijux/acp-agent/internal/agent"
)

// updates 把 agent.Event 流翻译成 ACP session/update 通知。
//
// 它同时负责 <b>toolCallId 登记</b>：agent 在工具真正执行前发出
// ToolExecutionStartEvent，而权限门禁（guard）在 tool.Execute 内部才被调用，
// 因此这里维护 name → 待消费 toolCallId 的队列，让
// session/request_permission 能带上与 tool_call 更新一致的 ID。
type updates struct {
	conn      *Conn
	sessionID string
	caps      ClientCapabilities

	mu           sync.Mutex
	seq          int
	currentMsgID string
	toolIDs      map[string][]string
	toolNames    map[string]string
	compactSeq   int
}

func newUpdates(conn *Conn, sessionID string, caps ClientCapabilities) *updates {
	return &updates{
		conn:      conn,
		sessionID: sessionID,
		caps:      caps,
		toolIDs:   make(map[string][]string),
		toolNames: make(map[string]string),
	}
}

// Handle 是 agent.EventHandler。
func (u *updates) Handle(ev agent.Event) {
	switch e := ev.(type) {
	case agent.MessageStartEvent:
		u.messageStart(e)
	case agent.MessageUpdateEvent:
		u.chunk(e)
	case agent.MessageEndEvent:
		u.messageEnd(e)
	case agent.ToolExecutionStartEvent:
		u.toolStart(e)
	case agent.ToolExecutionEndEvent:
		u.toolEnd(e)
	case agent.CompactionStartEvent:
		u.compactionStart(e)
	case agent.CompactionEndEvent:
		u.compactionEnd(e)
	case agent.ErrorEvent:
		u.errorEvent(e)
	}
}

// takeToolID 在权限门禁里按工具名取一个尚未消费的 toolCallId。
func (u *updates) takeToolID(name string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	q := u.toolIDs[name]
	if len(q) == 0 {
		return ""
	}
	id := q[0]
	u.toolIDs[name] = q[1:]
	return id
}

// toolName 反查 toolCallId 对应的工具名。
func (u *updates) toolName(id string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.toolNames[id]
}

func (u *updates) messageStart(e agent.MessageStartEvent) {
	u.mu.Lock()
	u.seq++
	u.currentMsgID = fmt.Sprintf("msg_%d", u.seq)
	id := u.currentMsgID
	u.mu.Unlock()

	// 用户消息在 prompt 处理入口已经先行发出（见 session.go），这里只处理
	// assistant/system 等由 agent 产生的消息。
	_ = id
}

func (u *updates) chunk(e agent.MessageUpdateEvent) {
	if e.Delta == "" {
		return
	}
	u.mu.Lock()
	id := u.currentMsgID
	u.mu.Unlock()
	if id == "" {
		u.mu.Lock()
		u.seq++
		id = fmt.Sprintf("msg_%d", u.seq)
		u.currentMsgID = id
		u.mu.Unlock()
	}

	block := TextBlock(e.Delta)
	switch e.Kind {
	case "thinking":
		u.Send(AgentThoughtChunk{
			SessionUpdate: "agent_thought_chunk",
			MessageID:     id,
			Content:       block,
		})
	default:
		u.Send(AgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			MessageID:     id,
			Content:       block,
		})
	}
}

func (u *updates) messageEnd(e agent.MessageEndEvent) {
	// 模型响应结束时，把该响应里的工具调用一次性预告为 pending，
	// 之后 ToolExecutionStartEvent 再用 tool_call_update 推成 in_progress。
	am, ok := e.Message.(*agent.AssistantMessage)
	if !ok {
		return
	}
	for _, tc := range am.ToolCalls() {
		u.mu.Lock()
		u.toolNames[tc.ID] = tc.Name
		u.toolIDs[tc.Name] = append(u.toolIDs[tc.Name], tc.ID)
		u.mu.Unlock()

		u.Send(ToolCallUpdate{
			SessionUpdate: "tool_call",
			ToolCallID:    tc.ID,
			Name:          tc.Name,
			Title:         toolTitle(tc.Name, tc.Arguments),
			Kind:          toolKind(tc.Name),
			Status:        ToolStatusPending,
			RawInput:      tc.Arguments,
		})
	}
	if u.caps.SupportsNotices() && am.ErrorMessage != "" {
		u.notice("error", "模型返回错误", am.ErrorMessage)
	}
}

func (u *updates) toolStart(e agent.ToolExecutionStartEvent) {
	// 有些 provider 会在执行期才补齐调用（或 tool_call 预告缺失），这里补一次。
	u.mu.Lock()
	known := false
	for _, id := range u.toolIDs[e.Call.Name] {
		if id == e.Call.ID {
			known = true
			break
		}
	}
	if !known && e.Call.ID != "" {
		u.toolNames[e.Call.ID] = e.Call.Name
		u.toolIDs[e.Call.Name] = append(u.toolIDs[e.Call.Name], e.Call.ID)
		u.Send(ToolCallUpdate{
			SessionUpdate: "tool_call",
			ToolCallID:    e.Call.ID,
			Name:          e.Call.Name,
			Title:         toolTitle(e.Call.Name, e.Call.Arguments),
			Kind:          toolKind(e.Call.Name),
			Status:        ToolStatusPending,
			RawInput:      e.Call.Arguments,
		})
	}
	u.mu.Unlock()

	u.Send(ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    e.Call.ID,
		Status:        ToolStatusInProgress,
	})
}

func (u *updates) toolEnd(e agent.ToolExecutionEndEvent) {
	status := ToolStatusCompleted
	var contents []ToolCallContent
	if e.Result != nil {
		for _, blk := range e.Result.Content {
			switch t := blk.(type) {
			case agent.TextBlock:
				contents = append(contents, ContentToolContent(TextBlock(t.Text)))
			case agent.ImageBlock:
				contents = append(contents, ContentToolContent(ContentBlock{
					Type:     "image",
					Data:     t.Data,
					MIMEType: t.MimeType,
				}))
			}
		}
		if e.Result.IsError {
			status = ToolStatusFailed
		}
	}
	var rawOutput any
	if e.Result != nil && len(contents) <= 1 {
		rawOutput = e.Result.Text()
	}
	u.Send(ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    e.Call.ID,
		Status:        status,
		Content:       contents,
		RawOutput:     rawOutput,
	})
}

func (u *updates) compactionStart(e agent.CompactionStartEvent) {
	if !u.caps.SupportsCompaction() {
		u.notice("info", "正在压缩上下文",
			fmt.Sprintf("压缩前约 %d tokens（原因：%s）", e.TokensBefore, e.Reason))
		return
	}
	u.mu.Lock()
	u.compactSeq++
	id := fmt.Sprintf("cmp_%d", u.compactSeq)
	u.mu.Unlock()
	u.Send(CompactionUpdate{
		SessionUpdate: "compaction_update",
		CompactionID:  id,
		Status:        "in_progress",
	})
}

func (u *updates) compactionEnd(e agent.CompactionEndEvent) {
	if !u.caps.SupportsCompaction() {
		u.notice("info", "上下文已压缩",
			fmt.Sprintf("%d → %d tokens", e.TokensBefore, e.TokensAfter))
		return
	}
	u.mu.Lock()
	id := fmt.Sprintf("cmp_%d", u.compactSeq)
	u.mu.Unlock()
	if id == "cmp_0" {
		u.compactSeq = 1
		id = "cmp_1"
	}
	var summary []ContentBlock
	if e.Summary != "" {
		summary = []ContentBlock{TextBlock(e.Summary)}
	}
	u.Send(CompactionUpdate{
		SessionUpdate: "compaction_update",
		CompactionID:  id,
		Status:        "completed",
		Summary:       summary,
	})
}

func (u *updates) errorEvent(e agent.ErrorEvent) {
	if e.Err == nil {
		return
	}
	u.notice("error", "Agent 错误", e.Err.Error())
}

// notice 在客户端支持时发 notice，否则退回成普通 agent 消息（可进入历史）。
func (u *updates) notice(severity, title, description string) {
	if u.caps.SupportsNotices() {
		d := description
		u.Send(NoticeUpdate{
			SessionUpdate: "notice",
			Severity:      severity,
			Title:         title,
			Description:   &d,
		})
		return
	}
	u.mu.Lock()
	id := u.currentMsgID
	u.mu.Unlock()
	text := title
	if description != "" {
		text += ": " + description
	}
	u.Send(AgentMessageChunk{
		SessionUpdate: "agent_message_chunk",
		MessageID:     id,
		Content:       TextBlock(text + "\n"),
	})
}

// SessionID 实现 Sink。
func (u *updates) SessionID() string { return u.sessionID }

// Send 实现 Sink：原样发送一条 session/update 载荷。
func (u *updates) Send(update any) {
	_ = u.conn.Notify(u.conn.ctx, MethodSessionUpdate, SessionUpdateParams{
		SessionID: u.sessionID,
		Update:    update,
	})
}

// ---------- 工具名 → ACP 呈现信息 ----------

func toolKind(name string) string {
	switch name {
	case "read", "ls":
		return ToolKindRead
	case "write", "edit":
		return ToolKindEdit
	case "glob", "grep":
		return ToolKindSearch
	case "bash", "bash_output", "bash_kill", "parallel":
		return ToolKindExecute
	case "web_search", "web_fetch":
		return ToolKindFetch
	case "todo":
		return ToolKindThink
	case "delegate":
		return ToolKindOther
	default:
		return ToolKindOther
	}
}

func toolTitle(name string, args map[string]any) string {
	get := func(k string) string {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprint(v)
		}
		return ""
	}
	short := func(s string, n int) string {
		s = strings.ReplaceAll(s, "\n", " ")
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	}

	switch name {
	case "read":
		return "读取 " + get("path")
	case "write":
		return "写入 " + get("path")
	case "edit":
		return "编辑 " + get("path")
	case "ls":
		return "列出 " + get("path")
	case "glob":
		return "匹配 " + get("pattern")
	case "grep":
		return "搜索 " + short(get("pattern"), 40)
	case "bash":
		return "执行 " + short(get("command"), 60)
	case "web_search":
		return "搜索网页 " + short(get("query"), 40)
	case "web_fetch":
		return "抓取 " + get("url")
	case "delegate":
		return "委派子任务"
	}
	return name
}
