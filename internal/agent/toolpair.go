package agent

import "strings"

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
//  2. normalizeToolPairs：投影时把配对关系规范化 —— 既丢掉找不到父的 tool
//     消息，也剥掉没有 tool 消息响应的 tool_calls。
//
// 第 2 条必须双向。OpenAI 兼容接口两条规则都是硬性的：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//	An assistant message with 'tool_calls' must be followed by tool messages
//	  responding to each 'tool_call_id'. (insufficient tool messages following tool_calls message)
//
// 只兜第一条时，只要压缩把一个 tool result 折掉、却留着它的 assistant（或者把
// 摘要插进了这一轮中间），请求就会撞上第二条 —— 越早触发压缩的会话越容易碰上。

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

// normalizeToolPairs 把消息序列规范化成「assistant.tool_calls ↔ 紧随其后的 tool
// 消息」严格成对，双向都补。
//
//   - tool 结果找不到父（父被折叠 / 分支切走 / 更晚的 assistant 顶掉了它）
//     → 丢掉这条结果；
//   - assistant 的 tool_call 没有任何 tool 消息响应（结果被折叠、摘要被插在
//     这一轮中间、run 中途中断）→ 从 assistant 上剥掉这些 tool_calls；剥完
//     只剩空壳（无文本、无思考）就整条丢掉，否则保留其余内容。
//
// 判定口径与 OpenAI 一致：「响应」必须是**紧邻**其后的连续 tool 消息，中间隔了
// 任何其它消息（包括压缩摘要）都算没响应。
func normalizeToolPairs(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}

	// 第一遍：每个 assistant 后面紧跟的那段 tool 消息认领了哪些 id。
	// 只给「后面真的跟着 tool 消息」的位置分配 map（绝大多数 assistant 没有）。
	answered := make([]map[string]bool, len(msgs))
	for i, m := range msgs {
		if _, ok := m.(*AssistantMessage); !ok {
			continue
		}
		if _, ok := nextToolResult(msgs, i); !ok {
			continue
		}
		set := map[string]bool{}
		for j := i + 1; j < len(msgs); j++ {
			tr, ok := msgs[j].(*ToolResultMessage)
			if !ok {
				break
			}
			set[tr.ToolCallID] = true
		}
		answered[i] = set
	}

	live := map[string]bool{}
	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		switch v := m.(type) {
		case *AssistantMessage:
			live = map[string]bool{}
			calls := v.ToolCalls()
			if len(calls) == 0 {
				out = append(out, v)
				continue
			}
			kept := make([]Block, 0, len(v.Content))
			for _, blk := range v.Content {
				if tc, ok := blk.(ToolCallBlock); ok {
					// 空 id 一律剥掉：上游会把它当成一条无法响应的 tool_call，
					// 而 replay/import 出来的历史里确实可能是空的。
					if tc.ID == "" || !answered[i][tc.ID] {
						continue // 没有 tool 消息响应：剥掉，否则上游 400
					}
					live[tc.ID] = true
				}
				kept = append(kept, blk)
			}
			if len(live) == 0 && !hasTextContent(kept) {
				// 「我要调工具」的壳子，或者只剩思考块：上游硬规则是
				// content 与 tool_calls 至少有一个，否则报
				// "Invalid assistant message: content or tool_calls must be set"。
				continue
			}
			if len(kept) == len(v.Content) {
				out = append(out, v)
				continue
			}
			c := *v
			c.Content = kept
			out = append(out, &c)
		case *ToolResultMessage:
			if v.ToolCallID == "" || !live[v.ToolCallID] {
				continue // 空 id / 孤儿：父被压缩、分支切走，或 id 对不上
			}
			out = append(out, m)
		default:
			live = map[string]bool{}
			out = append(out, m)
		}
	}
	return out
}

// hasTextContent 判断还有没有可发的正文。
//
// 只数 TextBlock：思考块不算。思考块走独立字段（DeepSeek 的 reasoning_content），
// content 仍然为空 —— 而「content 空且没有 tool_calls」的 assistant 消息会被
// 上游直接 400（content or tool_calls must be set）。
func hasTextContent(blocks []Block) bool {
	for _, blk := range blocks {
		if b, ok := blk.(TextBlock); ok && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}


// nextToolResult 返回紧邻 i 之后的那条 tool 消息（没有则 false）。
func nextToolResult(msgs []Message, i int) (*ToolResultMessage, bool) {
	if i+1 >= len(msgs) {
		return nil, false
	}
	tr, ok := msgs[i+1].(*ToolResultMessage)
	return tr, ok
}
