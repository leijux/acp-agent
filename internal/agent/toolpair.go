package agent

// 工具调用的「父子配对」保护。
//
// 背景（真实事故）：一个 45 万 token 的会话里塞了十几张界面原型、抓包对照与
// SECS 设计的多轮讨论，自动压缩把 assistant 消息折叠成摘要，但它那两个 bash
// 调用的**结果**留在了上下文里。投影之后模型看到一串没有父的 role=tool 消息，
// OpenAI 兼容接口硬性要求：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 于是下一次请求直接 HTTP 400 —— 越"听话、步数越多"的会话越容易撞上，因为
// 工具结果在消息里占的比例更高、被折叠的机会更多。
//
// 两道防线：
//
//  1. expandBlockedToolResults：压缩落地时，把「父 assistant 已被折叠、自己却
//     没进折叠列表」的 tool result 一并加进去（省 token，也让语义自洽）；
//  2. dropOrphanToolResults：投影时兜底丢掉仍然找不到父的 tool 消息 —— 无论
//     kernel 返回什么、import/replay 出什么形状，都不会把 400 发出去。

// expandBlockedToolResults 把被折叠 assistant 的 tool 结果补进 blockedIDs。
//
// kernel 的 DirectMessageIDs 不保证包含区间内全部 tool result（实测：assistant
// 进区间、它的工具结果落在区间外），补进来之后投影会把它们一起换成摘要。
func expandBlockedToolResults(branch []*Entry, blockedIDs []string) []string {
	if len(blockedIDs) == 0 || len(branch) == 0 {
		return blockedIDs
	}
	blocked := make(map[string]bool, len(blockedIDs))
	for _, id := range blockedIDs {
		blocked[id] = true
	}

	// 收集「被折叠的 assistant」声明过的 tool_call id。
	want := map[string]bool{}
	for _, e := range branch {
		if !blocked[e.ID] || e.Type != EntryMessage || e.Message == nil {
			continue
		}
		am, ok := e.Message.(*AssistantMessage)
		if !ok {
			continue
		}
		for _, tc := range am.ToolCalls() {
			if tc.ID != "" {
				want[tc.ID] = true
			}
		}
	}
	if len(want) == 0 {
		return blockedIDs
	}

	out := append([]string(nil), blockedIDs...)
	for _, e := range branch {
		if blocked[e.ID] || e.Type != EntryMessage || e.Message == nil {
			continue
		}
		tr, ok := e.Message.(*ToolResultMessage)
		if !ok || tr.ToolCallID == "" {
			continue
		}
		if want[tr.ToolCallID] {
			blocked[e.ID] = true
			out = append(out, e.ID)
		}
	}
	return out
}

// dropOrphanToolResults 丢掉找不到父 assistant 的 tool 结果消息。
//
// 判定按顺序扫：遇到带 tool_calls 的 assistant 就记下这批 id；遇到 tool 结果
// 时 id 必须在这批里；任何其它消息（user / 摘要 / 普通 assistant）都会清空这批
// —— 与 OpenAI 的「tool 必须紧跟 assistant」语义一致。
func dropOrphanToolResults(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	live := map[string]bool{}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		switch v := m.(type) {
		case *AssistantMessage:
			live = map[string]bool{}
			for _, tc := range v.ToolCalls() {
				if tc.ID != "" {
					live[tc.ID] = true
				}
			}
		case *ToolResultMessage:
			if !live[v.ToolCallID] {
				continue // 孤儿：父被压缩/分支切走了
			}
		default:
			live = map[string]bool{}
		}
		out = append(out, m)
	}
	return out
}
