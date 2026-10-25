package tools

import (
	"context"
	"fmt"
	"strings"
)

// 本文件实现「上下文管理工具」：把压缩的控制权交给模型（对齐上游
// billion-context 的主动模式）。
//
// 分工：本包只定义工具契约（参数 schema、结果结构），真正的 kernel/session
// 操作由 agent 包通过 ContextBackend 注入 —— tools 包因此不依赖 agent，
// 也不依赖 kernel 的具体实现。

// 上下文管理工具名。模型通过这些名字调用。
const (
	ToolContextCheck      = "context_check"
	ToolContextApply      = "context_apply"
	ToolContextSearch     = "context_search"
	ToolContextDecompress = "context_decompress"
)

// ContextRange 是一次压缩请求。
type ContextRange struct {
	// StartRef / EndRef 是 kernel 的消息引用（形如 m00012），
	// 也就是 context_check 返回的区间端点。
	StartRef string `json:"startRef"`
	EndRef   string `json:"endRef"`
	// Summary 是模型自己写的摘要，必须自包含：以后只能看到它，看不到原文。
	Summary string `json:"summary"`
	// Topic 是可选短标签（用于检索命中时的展示）。
	Topic string `json:"topic,omitempty"`
}

// ContextRangeInfo 是 kernel 推荐的可压缩区间。
type ContextRangeInfo struct {
	StartRef string `json:"startRef"`
	EndRef   string `json:"endRef"`
	Messages int    `json:"messages"`
	Tokens   int    `json:"tokens"`
	// Preview 是区间首条消息的一行摘要，便于模型判断值不值得压。
	Preview string `json:"preview,omitempty"`
}

// ContextStatus 是上下文占用情况与可压缩区间。
type ContextStatus struct {
	// Usage 是占用比例（0..1）。
	Usage        float64 `json:"usage"`
	TokenCount   int     `json:"tokenCount"`
	ContextLimit int     `json:"contextLimit"`
	// ActiveBlocks 是当前生效的压缩块数。
	ActiveBlocks     int `json:"activeBlocks"`
	TokensCompressed int `json:"tokensCompressed"`
	// Ranges 是 kernel 认为可压的区间，已按优先级排序。
	Ranges []ContextRangeInfo `json:"ranges,omitempty"`
	// Reason 是 kernel 给出建议的原因（如用水位、压缩收益）。
	Reason string `json:"reason,omitempty"`
}

// ContextBlock 是一个压缩块的快照。
type ContextBlock struct {
	BlockID string `json:"blockId"`
	Topic   string `json:"topic,omitempty"`
	Tier    int    `json:"tier"`
	// Summary 是压缩时写入的摘要，也是投影进对话的内容。
	Summary string `json:"summary"`
	// MessageIDs 是被该块折叠掉的原始消息 id。
	MessageIDs       []string `json:"messageIds,omitempty"`
	TokensBefore     int      `json:"tokensBefore,omitempty"`
	TokensAfter      int      `json:"tokensAfter,omitempty"`
	TokensCompressed int      `json:"tokensCompressed,omitempty"`
}

// ContextHit 是一条检索命中（在已压缩块里检索）。
type ContextHit struct {
	BlockID string  `json:"blockId"`
	Topic   string  `json:"topic,omitempty"`
	Summary string  `json:"summary"`
	Score   float64 `json:"score,omitempty"`
}

// ContextApplyResult 是压缩执行结果。
type ContextApplyResult struct {
	BlocksCreated    int            `json:"blocksCreated"`
	TokensCompressed int            `json:"tokensCompressed"`
	Blocks           []ContextBlock `json:"blocks,omitempty"`
	Errors           []string       `json:"errors,omitempty"`
}

// ContextBackend 由宿主实现（agent 包），把工具调用翻译成 kernel 操作。
//
// 四个方法都可能返回错误；错误会以 IsError 结果回传给模型（而不是中断 run），
// 让模型有机会修正参数重试。
type ContextBackend interface {
	// ContextStatus 报告占用与可压区间。
	ContextStatus(ctx context.Context) (ContextStatus, error)
	// ContextApply 执行压缩。ranges 为空时按 kernel 的最高优先级区间自动选一段。
	ContextApply(ctx context.Context, ranges []ContextRange) (ContextApplyResult, error)
	// ContextSearch 在已压缩块里做关键词检索。
	ContextSearch(ctx context.Context, query string, limit int) ([]ContextHit, error)
	// ContextDecompress 取出一个块的摘要与被折叠的消息 id。
	ContextDecompress(ctx context.Context, blockID string) (ContextBlock, error)
}

// ---------------------------------------------------------------------------
// context_check
// ---------------------------------------------------------------------------

// ContextCheckTool 让模型查看上下文占用与可压缩区间。
type ContextCheckTool struct{ Backend ContextBackend }

func (t *ContextCheckTool) Name() string { return ToolContextCheck }

func (t *ContextCheckTool) Description() string {
	return `查看上下文占用情况，以及 kernel 建议压缩的区间（带消息引用 mNNNNN 与预估 token）。

在以下情况调用：
- 收到「上下文即将用尽」的提示；
- 一轮长任务开始前，想确认还有多少余量；
- 打算调用 context_apply，但不确定该压哪一段。

返回的 ranges 已按压缩收益排序，每项给出 startRef/endRef —— 直接把它们填进
context_apply 即可。ranges 为空表示当前没有值得压缩的内容。`
}

func (t *ContextCheckTool) Parameters() map[string]any { return Schema(nil) }

func (t *ContextCheckTool) ConcurrencySafe() bool { return true }

func (t *ContextCheckTool) Execute(ctx context.Context, _ Args) (Result, error) {
	if t.Backend == nil {
		return ErrorResult("上下文工具未启用"), nil
	}
	st, err := t.Backend.ContextStatus(ctx)
	if err != nil {
		return ErrorResult("读取上下文状态失败: %v", err), nil
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: formatContextStatus(st)}},
		Details: st,
	}, nil
}

func formatContextStatus(st ContextStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "上下文占用: %.1f%%（%d / %d tokens）\n",
		st.Usage*100, st.TokenCount, st.ContextLimit)
	if st.ActiveBlocks > 0 {
		fmt.Fprintf(&b, "已压缩: %d 个块，累计省下 %d tokens\n", st.ActiveBlocks, st.TokensCompressed)
	}
	if len(st.Ranges) == 0 {
		b.WriteString("\n当前没有值得压缩的区间。")
		if st.Reason != "" {
			b.WriteString("\n原因: ")
			b.WriteString(st.Reason)
		}
		return b.String()
	}
	b.WriteString("\n可压缩区间（按优先级排序）:\n")
	for i, r := range st.Ranges {
		fmt.Fprintf(&b, "%d. %s..%s  %d 条消息，约 %d tokens\n", i+1, r.StartRef, r.EndRef, r.Messages, r.Tokens)
		if r.Preview != "" {
			b.WriteString("   ")
			b.WriteString(r.Preview)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n用 context_apply 提交：给每个区间写一份自包含的摘要" +
		"（保留目标、决定与理由、文件路径、签名、错误原文、待办）。")
	return b.String()
}

// ---------------------------------------------------------------------------
// context_apply
// ---------------------------------------------------------------------------

// ContextApplyTool 让模型提交压缩。
type ContextApplyTool struct{ Backend ContextBackend }

func (t *ContextApplyTool) Name() string { return ToolContextApply }

func (t *ContextApplyTool) Description() string {
	return `压缩一段对话历史。原文不会删除，只是从后续上下文中折叠掉，
被折叠的内容可以随时用 context_search / context_decompress 找回。

ranges 每项需要 startRef、endRef、summary：
- startRef/endRef 取自 context_check 返回的区间（不要自己编）；
- summary 由你撰写，必须自包含 —— 之后你只能看到摘要，看不到原文。

摘要写作要点：
1. 保留当前目标、用户明确的要求与约束（逐条列出）。
2. 保留已做的决定及其理由（"选了 X 而不是 Y，因为 Z"）。
3. 保留精确技术产物：文件路径、函数签名、错误原文、版本号、阈值。
4. 保留结论；丢弃冗长日志与重复读取（教训留一行）。
5. 保留未解决的问题与待办。

省略 ranges 参数时，会按 kernel 的最高优先级区间自动选一段（此时仍需 summary）。`
}

func (t *ContextApplyTool) Parameters() map[string]any {
	rangeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"startRef": Prop("string", "区间起始引用，如 m00012"),
			"endRef":   Prop("string", "区间结束引用，如 m00040"),
			"summary":  Prop("string", "自包含的摘要正文（至少 50 字符）"),
			"topic":    Prop("string", "可选短标签，用于之后检索展示"),
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	}
	return Schema(map[string]any{
		"ranges": map[string]any{
			"type":        "array",
			"description": "要压缩的区间列表。省略时自动选 kernel 推荐的第一段。",
			"items":       rangeSchema,
		},
		"summary": Prop("string", "当只压一段且未在 ranges 里给 summary 时的摘要正文"),
	})
}

func (t *ContextApplyTool) ConcurrencySafe() bool { return false }

func (t *ContextApplyTool) Mutates() bool { return true }

func (t *ContextApplyTool) Execute(ctx context.Context, args Args) (Result, error) {
	if t.Backend == nil {
		return ErrorResult("上下文工具未启用"), nil
	}
	ranges := parseContextRanges(args)
	if len(ranges) == 0 {
		// 允许只给顶层 summary，由后端挑区间。
		if s := strings.TrimSpace(args.Str("summary", "")); s != "" {
			ranges = []ContextRange{{Summary: s}}
		}
	}
	if len(ranges) == 0 {
		return ErrorResult("缺少 summary：压缩必须由你撰写摘要（先调 context_check 看区间，再写摘要）"), nil
	}
	for i := range ranges {
		if strings.TrimSpace(ranges[i].Summary) == "" {
			return ErrorResult("第 %d 个区间缺少 summary", i+1), nil
		}
	}

	res, err := t.Backend.ContextApply(ctx, ranges)
	if err != nil {
		return ErrorResult("压缩失败: %v", err), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "压缩完成：新建 %d 个块，节省 %d tokens。\n", res.BlocksCreated, res.TokensCompressed)
	for _, blk := range res.Blocks {
		fmt.Fprintf(&b, "- %s（tier %d，折叠 %d 条消息）", blk.BlockID, blk.Tier, len(blk.MessageIDs))
		if blk.Topic != "" {
			fmt.Fprintf(&b, " 主题: %s", blk.Topic)
		}
		b.WriteString("\n")
	}
	if len(res.Errors) > 0 {
		b.WriteString("\n警告:\n")
		for _, e := range res.Errors {
			b.WriteString("- ")
			b.WriteString(e)
			b.WriteString("\n")
		}
	}
	return Result{Content: []Block{{Kind: KindText, Text: b.String()}}, Details: res}, nil
}

// parseContextRanges 从参数里解析区间列表。
func parseContextRanges(args Args) []ContextRange {
	raw, ok := args["ranges"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]ContextRange, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		a := Args(m)
		out = append(out, ContextRange{
			StartRef: strings.TrimSpace(a.Str("startRef", "")),
			EndRef:   strings.TrimSpace(a.Str("endRef", "")),
			Summary:  a.Str("summary", ""),
			Topic:    strings.TrimSpace(a.Str("topic", "")),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// context_search
// ---------------------------------------------------------------------------

// ContextSearchTool 在已压缩块里检索。
type ContextSearchTool struct{ Backend ContextBackend }

func (t *ContextSearchTool) Name() string { return ToolContextSearch }

func (t *ContextSearchTool) Description() string {
	return `在「已被压缩掉的历史」里检索（BM25 关键词匹配）。当前可见的对话搜不到东西再用它。

压缩之后你只看到摘要，细节可通过本工具找回：命中会返回 blockId，
再用 context_decompress 取出该块的完整摘要与被折叠的消息 id。`
}

func (t *ContextSearchTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"query": Prop("string", "检索关键词，尽量用原文里出现的具体词（文件名、函数名、错误串）"),
		"limit": PropInt("返回条数上限", 5),
	}, "query")
}

func (t *ContextSearchTool) ConcurrencySafe() bool { return true }

func (t *ContextSearchTool) Execute(ctx context.Context, args Args) (Result, error) {
	if t.Backend == nil {
		return ErrorResult("上下文工具未启用"), nil
	}
	query, err := args.Required("query")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	limit := int(args.Int("limit", 5))
	hits, err := t.Backend.ContextSearch(ctx, query, limit)
	if err != nil {
		return ErrorResult("检索失败: %v", err), nil
	}
	if len(hits) == 0 {
		return TextResult("没有命中已压缩的内容（关键词: %s）", query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "命中 %d 条压缩块：\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "\n%d. %s", i+1, h.BlockID)
		if h.Topic != "" {
			fmt.Fprintf(&b, "  [%s]", h.Topic)
		}
		b.WriteString("\n")
		b.WriteString(indentBlock(h.Summary, "   "))
		b.WriteString("\n")
	}
	b.WriteString("\n用 context_decompress <blockId> 查看某块折叠了哪些消息。")
	return Result{Content: []Block{{Kind: KindText, Text: b.String()}}, Details: hits}, nil
}

// ---------------------------------------------------------------------------
// context_decompress
// ---------------------------------------------------------------------------

// ContextDecompressTool 展开一个压缩块。
type ContextDecompressTool struct{ Backend ContextBackend }

func (t *ContextDecompressTool) Name() string { return ToolContextDecompress }

func (t *ContextDecompressTool) Description() string {
	return `查看一个压缩块的完整内容：摘要、被它折叠的消息 id、以及压缩前后的 token 数。

块不会被真正「展开」回上下文（那会立刻把省下的空间还回去）——本工具只把
块的信息展示给你。若需要某条消息的原文，用它的 id 配合 read/grep 从工作
现场重新获取，或看摘要里记下的路径与结论。`
}

func (t *ContextDecompressTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"blockId": Prop("string", "压缩块 id，如 b1（可由 context_check 或 context_search 得到）"),
	}, "blockId")
}

func (t *ContextDecompressTool) ConcurrencySafe() bool { return true }

func (t *ContextDecompressTool) Execute(ctx context.Context, args Args) (Result, error) {
	if t.Backend == nil {
		return ErrorResult("上下文工具未启用"), nil
	}
	blockID, err := args.Required("blockId")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	blk, err := t.Backend.ContextDecompress(ctx, blockID)
	if err != nil {
		return ErrorResult("读取压缩块 %s 失败: %v", blockID, err), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "压缩块 %s（tier %d）", blk.BlockID, blk.Tier)
	if blk.Topic != "" {
		fmt.Fprintf(&b, " 主题: %s", blk.Topic)
	}
	b.WriteString("\n")
	if blk.TokensBefore > 0 {
		fmt.Fprintf(&b, "token: %d → %d\n", blk.TokensBefore, blk.TokensAfter)
	}
	b.WriteString("\n摘要:\n")
	b.WriteString(indentBlock(blk.Summary, "  "))
	if len(blk.MessageIDs) > 0 {
		fmt.Fprintf(&b, "\n\n折叠的消息（%d 条）: %s\n", len(blk.MessageIDs), strings.Join(blk.MessageIDs, ", "))
	}
	return Result{Content: []Block{{Kind: KindText, Text: b.String()}}, Details: blk}, nil
}

// ContextTools 返回四个上下文管理工具。
func ContextTools(b ContextBackend) []Tool {
	return []Tool{
		&ContextCheckTool{Backend: b},
		&ContextApplyTool{Backend: b},
		&ContextSearchTool{Backend: b},
		&ContextDecompressTool{Backend: b},
	}
}

// indentBlock 给多行文本加前缀。
func indentBlock(s, prefix string) string {
	if s == "" {
		return prefix
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// IsContextTool 判断工具名是否属于上下文管理工具。
func IsContextTool(name string) bool {
	switch name {
	case ToolContextCheck, ToolContextApply, ToolContextSearch, ToolContextDecompress:
		return true
	}
	return false
}
