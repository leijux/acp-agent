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
func TestNormalizeToolPairsDropsOrphans(t *testing.T) {
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "跑两条命令"}}},
		asstWithCalls("c1", "c2"),
		toolRes("c1"),
		toolRes("c2"),
		&CompactionSummaryMessage{Summary: "摘要：把上面的 user+assistant 折叠了"},
		toolRes("c1"), // ← 事故原型：父已经不在可见消息里
		&UserMessage{Content: []Block{TextBlock{Text: "继续"}}},
	}
	out := normalizeToolPairs(msgs)

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
func TestNormalizeToolPairsKeepsPaired(t *testing.T) {
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "go"}}},
		asstWithCalls("c1", "c2", "c3"),
		toolRes("c1"),
		toolRes("c2"),
		asstWithCalls("c4"),
		toolRes("c4"),
		toolRes("c3"), // 迟到的结果：父 c3 已被后面的 assistant 顶掉
	}
	out := normalizeToolPairs(msgs)
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

// ---------- 反方向：assistant 的 tool_calls 没有响应 ----------

// assertToolPairsValid 校验 OpenAI 兼容接口的两条硬性规则：
//   - 每条 role=tool 都能找到紧邻其前的 tool_calls；
//   - 每个 tool_call_id 都有紧随其后的 tool 消息响应。
func assertToolPairsValid(t *testing.T, msgs []Message) {
	t.Helper()
	pending := map[string]bool{}
	fail := func(why string, next any) {
		var ids []string
		for id := range pending {
			ids = append(ids, id)
		}
		t.Fatalf("%s（下一条是 %T，未响应 %v）", why, next, ids)
	}
	for _, m := range msgs {
		if _, isTool := m.(*ToolResultMessage); !isTool && len(pending) > 0 {
			fail("上一条 assistant 的 tool_calls 没有被响应", m)
		}
		switch v := m.(type) {
		case *AssistantMessage:
			pending = map[string]bool{}
			for _, tc := range v.ToolCalls() {
				pending[tc.ID] = true
			}
		case *ToolResultMessage:
			if !pending[v.ToolCallID] {
				t.Fatalf("孤儿 tool 结果 %q", v.ToolCallID)
			}
			delete(pending, v.ToolCallID)
		}
	}
	if len(pending) > 0 {
		fail("末尾仍有未响应的 tool_calls", "EOF")
	}
}

// 摘要把 tool 结果折掉、assistant 留在外面 —— 事故原型（本轮报错原文：
// "insufficient tool messages following tool_calls message"）。
func TestNormalizeToolPairsStripsUnansweredCalls(t *testing.T) {
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "跑两条命令"}}},
		asstWithCalls("c1", "c2"), // 只有工具调用，没有文本
		&CompactionSummaryMessage{Summary: "摘要：上面的工具结果已压缩"},
		toolRes("c1"), // 父与摘要之间隔了摘要 → 算没响应
		&UserMessage{Content: []Block{TextBlock{Text: "继续"}}},
	}
	out := normalizeToolPairs(msgs)
	assertToolPairsValid(t, out)

	for _, m := range out {
		if _, ok := m.(*AssistantMessage); ok {
			t.Fatal("只剩「我要调工具」的空壳 assistant，应整条丢掉")
		}
		if tr, ok := m.(*ToolResultMessage); ok {
			t.Fatalf("孤儿 tool 结果 %q 应被丢掉", tr.ToolCallID)
		}
	}
	if len(out) != 3 { // user + summary + user
		t.Fatalf("输出 %d 条，want 3", len(out))
	}
}

// assistant 有文本 + 部分结果缺失：只剥掉没响应的调用，文本必须留住。
func TestNormalizeToolPairsStripsPartialAndKeepsText(t *testing.T) {
	asst := asstWithCalls("c1", "c2")
	asst.Content = append([]Block{TextBlock{Text: "我先看看目录"}}, asst.Content...)

	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "看看"}}},
		asst,
		toolRes("c1"), // c2 的结果丢了
		&UserMessage{Content: []Block{TextBlock{Text: "继续"}}},
	}
	out := normalizeToolPairs(msgs)
	assertToolPairsValid(t, out)

	got, ok := out[1].(*AssistantMessage)
	if !ok {
		t.Fatalf("第 2 条应是 assistant，实际 %T", out[1])
	}
	if calls := got.ToolCalls(); len(calls) != 1 || calls[0].ID != "c1" {
		t.Fatalf("应只保留 c1，实际 %+v", calls)
	}
	if got.Text() != "我先看看目录" {
		t.Fatalf("文本被弄丢了: %q", got.Text())
	}
	if len(asst.ToolCalls()) != 2 {
		t.Fatal("不能就地改原消息（session 里的 entry 是共享的）")
	}
}

// 端到端：压缩块只盖住「tool 结果」时，摘要不能插进工具轮中间。
func TestCompactionBlockStartingAtToolResultKeepsPairs(t *testing.T) {
	s := NewSession()
	asst := asstWithCalls("c1")
	asst.Content = append([]Block{TextBlock{Text: "跑一下 ls"}}, asst.Content...)
	for _, m := range []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "第一步"}}},
		asst,
		toolRes("c1"),
		&UserMessage{Content: []Block{TextBlock{Text: "第二步"}}},
	} {
		if _, err := s.Append(m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 块只吃了 e3（tool 结果）：kernel 的区间可以从工具结果开始。
	if _, err := s.AppendCompactionBlock("b1", strings.Repeat("摘要内容", 20), []string{"e3"}, 100, 50); err != nil {
		t.Fatalf("AppendCompactionBlock: %v", err)
	}

	out := s.Messages()
	assertToolPairsValid(t, out)

	var hasSummary, hasText bool
	for _, m := range out {
		switch v := m.(type) {
		case *CompactionSummaryMessage:
			hasSummary = true
		case *AssistantMessage:
			if v.Text() == "跑一下 ls" {
				hasText = true
			}
		}
	}
	if !hasSummary {
		t.Fatal("摘要不见了")
	}
	if !hasText {
		t.Fatalf("assistant 的文本应在剥离 tool_calls 后保留: %+v", out)
	}
}

// 空 tool_call id / 空 tool_call_id：replay、import 与部分网关会产出这种形状。
// 空 id 既不能当「已响应」，也不能原样发出去（上游同样 400）。
func TestNormalizeToolPairsEmptyIDs(t *testing.T) {
	asst := asstWithCalls("c1", "")
	asst.Content = append([]Block{TextBlock{Text: "查一下"}}, asst.Content...)
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "查"}}},
		asst,
		toolRes("c1"),
		toolRes(""),
	}
	out := normalizeToolPairs(msgs)
	assertToolPairsValid(t, out)

	if len(out) != 3 { // user + assistant(只剩 c1) + 一条 tool(c1)
		t.Fatalf("输出 %d 条，want 3", len(out))
	}
	got := out[1].(*AssistantMessage)
	if calls := got.ToolCalls(); len(calls) != 1 || calls[0].ID != "c1" {
		t.Fatalf("应只保留 c1，实际 %+v", calls)
	}
	if got.Text() != "查一下" {
		t.Fatalf("文本被弄丢了: %q", got.Text())
	}
}

// 连续两个 assistant：前一个的 tool_calls 不可能被后面的 tool 消息响应。
func TestNormalizeToolPairsConsecutiveAssistants(t *testing.T) {
	first := asstWithCalls("c1")
	first.Content = append([]Block{TextBlock{Text: "先跑 ls"}}, first.Content...)
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "跑"}}},
		first,
		asstWithCalls("c2"),
		toolRes("c2"),
	}
	out := normalizeToolPairs(msgs)
	assertToolPairsValid(t, out)

	got := out[1].(*AssistantMessage)
	if len(got.ToolCalls()) != 0 {
		t.Fatalf("第一个 assistant 的 c1 不可能被响应，应剥掉: %+v", got.ToolCalls())
	}
	if got.Text() != "先跑 ls" {
		t.Fatalf("文本被弄丢了: %q", got.Text())
	}
	if len(out) != 4 {
		t.Fatalf("输出 %d 条，want 4", len(out))
	}
}

// 只剩思考块的 assistant：剥掉 tool_calls 后 content 为空，上游会报
// "Invalid assistant message: content or tool_calls must be set"。
// 思考块不算「可发正文」，整条必须丢掉。
func TestNormalizeToolPairsDropsThinkingOnlyShell(t *testing.T) {
	asst := &AssistantMessage{
		Content: []Block{
			ThinkingBlock{Thinking: "先看看目录再决定"},
			ToolCallBlock{ID: "c1", Name: "ls", Arguments: map[string]any{"path": "."}},
		},
	}
	msgs := []Message{
		&UserMessage{Content: []Block{TextBlock{Text: "看看"}}},
		asst,
		&CompactionSummaryMessage{Summary: "摘要"},
		&UserMessage{Content: []Block{TextBlock{Text: "继续"}}},
	}
	out := normalizeToolPairs(msgs)
	assertToolPairsValid(t, out)

	for _, m := range out {
		if am, ok := m.(*AssistantMessage); ok {
			t.Fatalf("只剩思考的 assistant 不应发出去，实际 %+v", am.Content)
		}
	}
	if len(out) != 3 {
		t.Fatalf("输出 %d 条，want 3（user + summary + user）", len(out))
	}
}
