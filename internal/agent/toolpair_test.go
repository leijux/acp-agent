package agent

// 工具调用父子配对的回归测试。
//
// 事故原型：压缩把 assistant（带 tool_calls）折叠成摘要，它的 tool 结果留在
// 上下文里 → provider 收到无父的 role=tool 消息 → HTTP 400
// 「Messages with role 'tool' must be a response to a preceding message with
// 'tool_calls'」。

import (
	"strings"
	"testing"
)

func asstWithCalls(callIDs ...string) *AssistantMessage {
	m := &AssistantMessage{At: 1}
	for _, id := range callIDs {
		m.Content = append(m.Content, ToolCallBlock{ID: id, Name: "bash", Arguments: map[string]any{"command": "ls"}})
	}
	return m
}

func toolRes(callID string) *ToolResultMessage {
	return &ToolResultMessage{ToolCallID: callID, ToolName: "bash", Content: []Block{TextBlock{Text: "ok"}}}
}

// 投影层兜底：父被折叠后，残留的 tool 结果必须被丢掉，不能发给 provider。
func TestDropOrphanToolResults(t *testing.T) {
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "跑两条命令"}}},
		asstWithCalls("c1", "c2"),
		toolRes("c1"),
		toolRes("c2"),
		&CompactionSummaryMessage{Summary: "摘要：把上面的 user+assistant 折叠了"},
		toolRes("c1"), // ← 事故原型：父已经不在可见消息里
		&UserMessage{Content: []Block{TextBlock{Text: "继续"}}},
	}
	out := dropOrphanToolResults(msgs)

	var kept []Message
	for _, m := range out {
		if tr, ok := m.(*ToolResultMessage); ok {
			kept = append(kept, tr)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("应保留 2 条有父的 tool 结果，实际 %d（输出 %d 条）", len(kept), len(out))
	}
	for _, m := range kept {
		if tr := m.(*ToolResultMessage); tr.ToolCallID != "c1" && tr.ToolCallID != "c2" {
			t.Fatalf("留下了不该留的 %q", tr.ToolCallID)
		}
	}
	// 摘要之后的孤儿必须消失
	if len(out) != 6 {
		t.Fatalf("输出条数 = %d, want 6", len(out))
	}
}

// 配对完整时一条都不能少。
func TestDropOrphanToolResultsKeepsPaired(t *testing.T) {
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "go"}}},
		asstWithCalls("c1", "c2", "c3"),
		toolRes("c1"),
		toolRes("c2"),
		asstWithCalls("c4"),
		toolRes("c4"),
		toolRes("c3"), // 迟到的结果：父 c3 已被后面的 assistant 顶掉
	}
	out := dropOrphanToolResults(msgs)
	if len(out) != 6 {
		t.Fatalf("输出条数 = %d, want 6（迟到的 c3 应被丢弃）", len(out))
	}
}

// 压缩落地时要把「父被折叠」的 tool 结果补进 blockedIDs。
func TestExpandBlockedToolResults(t *testing.T) {
	s := NewSession()
	e1 := &Entry{ID: "e1", Type: EntryMessage, Message: &UserMessage{Content: []Block{TextBlock{Text: "跑命令"}}}}
	e2 := &Entry{ID: "e2", Type: EntryMessage, Message: asstWithCalls("c1", "c2")}
	e3 := &Entry{ID: "e3", Type: EntryMessage, Message: toolRes("c1")}
	e4 := &Entry{ID: "e4", Type: EntryMessage, Message: toolRes("c2")}
	e5 := &Entry{ID: "e5", Type: EntryMessage, Message: &UserMessage{Content: []Block{TextBlock{Text: "继续"}}}}
	for _, e := range []*Entry{e1, e2, e3, e4, e5} {
		if _, err := s.Append(e.Message); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	branch := s.ActiveBranch()

	// kernel 只给了 user+assistant（事故里的形状）
	got := expandBlockedToolResults(branch, []string{"e1", "e2"})
	want := map[string]bool{"e1": true, "e2": true, "e3": true, "e4": true}
	if len(got) != 4 {
		t.Fatalf("blockedIDs = %v, want 4 条（e1..e4）", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("多补了 %q", id)
		}
	}

	// 别的 assistant 的结果不能被牵连
	got = expandBlockedToolResults(branch, []string{"e2"})
	if len(got) != 3 {
		t.Fatalf("只折叠 e2 时应补 c1/c2 两条：%v", got)
	}
	for _, id := range got {
		if id == "e5" {
			t.Fatal("不该动无关 entry")
		}
	}

	// 空输入 / 无 tool_calls 的区间原样返回
	if got := expandBlockedToolResults(branch, nil); len(got) != 0 {
		t.Fatalf("nil 应原样返回: %v", got)
	}
	if got := expandBlockedToolResults(branch, []string{"e1", "e5"}); len(got) != 2 {
		t.Fatalf("没有 tool_calls 时不该补东西: %v", got)
	}
}

// 端到端：真实的 Messages() 投影在「压缩只盖住 assistant」时不产孤儿。
func TestSessionMessagesDropOrphanAfterCompaction(t *testing.T) {
	s := NewSession()
	for _, m := range []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "第一步"}}},
		asstWithCalls("c1"),
		toolRes("c1"),
		&UserMessage{Content: []Block{TextBlock{Text: "第二步"}}},
	} {
		if _, err := s.Append(m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 只折叠 user+assistant，故意把 tool 结果漏在外面（模拟 kernel 的 direct 列表）
	if _, err := s.AppendCompactionBlock("b1", strings.Repeat("摘要内容", 20), []string{"e1", "e2"}, 100, 50); err != nil {
		t.Fatalf("AppendCompactionBlock: %v", err)
	}

	out := s.Messages()
	live := map[string]bool{}
	for _, m := range out {
		switch v := m.(type) {
		case *AssistantMessage:
			for _, tc := range v.ToolCalls() {
				live[tc.ID] = true
			}
		case *ToolResultMessage:
			if !live[v.ToolCallID] {
				t.Fatalf("投影后仍有孤儿 tool 结果 %q（这正是 400 的来源）", v.ToolCallID)
			}
		}
	}
	// 摘要与「第二步」还在
	var hasSummary, hasSecond bool
	for _, m := range out {
		switch m.(type) {
		case *CompactionSummaryMessage:
			hasSummary = true
		case *UserMessage:
			hasSecond = true
		}
	}
	if !hasSummary || !hasSecond {
		t.Fatalf("摘要或后续消息丢了: %+v", out)
	}
}
