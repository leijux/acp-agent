package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/leijux/acp-agent/internal/kernel"
)

// 压缩相关的默认值。
const (
	// defaultReserveTokens 为模型响应预留空间（Pi 默认 16384）。
	defaultReserveTokens = 16_384
	// serializeToolResultLimit 序列化 tool result 时的字符上限（Pi 用 2000）。
	serializeToolResultLimit = 2000

	// SoftCompactUsage 是「提示模型主动压缩」的占用比例，对齐上游的
	// minContextLimitPct = 0.45：到这个水位就应告知模型可以压缩了。
	SoftCompactUsage = 0.45
	// HardCompactUsage 是宿主强制压缩的兜底比例，对齐上游的
	// maxContextLimitPct = 0.75 之上再留一层余量。
	//
	// 主动模式（模型自己调用 context_apply）正常运行时不应走到这里；
	// 它是模型不肯压缩时的安全网，避免上下文撞上硬上限。
	HardCompactUsage = 0.90
)

// SummaryPrompt 是生成压缩摘要用的系统提示（对齐 Pi 的 compaction prompt）。
const SummaryPrompt = `你是会话压缩器。把给定的对话历史压缩成一份结构化摘要，供后续继续工作使用。

要求：
1. 保留当前目标、用户明确提出的要求与约束（逐条列出，不要合并）。
2. 保留已做出的决定及其理由（“选了 X 而不是 Y，因为 Z”）。
3. 保留精确的技术产物：文件路径、函数/类型签名、错误原文、版本号、配置项、阈值。
4. 保留结论与关键发现；丢弃冗长的日志、重复的文件读取、已走死的探索（只留一行教训）。
5. 保留未解决的问题与待办。
6. 用简体中文输出，分条列出，不要叙述式长文。

直接输出摘要正文，不要加前缀说明。`

// shouldCompact 判断是否达到「宿主强制压缩」的兜底水位（HardCompactUsage）。
//
// 这只是廉价的快速判断，避免每轮都跑一次 kernel 渲染。真正的决策（压哪一段、
// 压多少）由 kernel 的 nudge 给出，见 Compact。
//
// 主动模式下常驻路径是 shouldNudge（SoftCompactUsage），本函数只在模型
// 迟迟不压缩时兜底。
func (a *Agent) shouldCompact() bool {
	return a.contextUsage() >= HardCompactUsage
}

// shouldNudge 判断是否该提醒模型可以主动压缩（SoftCompactUsage）。
func (a *Agent) shouldNudge() bool {
	return a.contextUsage() >= SoftCompactUsage
}

// contextUsage 返回上下文占用比例（0..1）。
func (a *Agent) contextUsage() float64 {
	if a.opts.ContextWindow <= 0 {
		return 0
	}
	return float64(a.contextTokens()) / float64(a.opts.ContextWindow)
}

// ContextUsage 暴露当前占用比例（供状态栏/测试用）。
func (a *Agent) ContextUsage() float64 { return a.contextUsage() }

// contextTokens 估算当前会发送给模型的 token 数。
func (a *Agent) contextTokens() int {
	msgs := ProjectMessages(a.opts.Session.Messages())
	return a.opts.TokenCounter(msgs) + EstimateTokens(BuildSystemPrompt(a.opts.Context))
}

// ContextTokens 暴露当前上下文估算值（供状态栏/测试用）。
func (a *Agent) ContextTokens() int { return a.contextTokens() }

// CompactionResult 是压缩结果。
type CompactionResult struct {
	Summary      string
	TokensBefore int
	TokensAfter  int
	// BlockID 是 kernel 为这次压缩创建的块 id。
	BlockID string
	// StartRef / EndRef 是 kernel 划定的压缩区间引用。
	StartRef string
	EndRef   string
	// Summarized 是被摘要掉的原始消息（调试用）。
	Summarized []Message
}

// Compact 执行一次压缩：kernel 划定区间 → 模型生成摘要 → 落成压缩块。
//
// reason 为 "manual" / "threshold" / "overflow"；instructions 是可选的人工侧重
// （对应 /compact <instructions>）。
//
// 返回 (nil, nil) 表示 kernel 认为当前不需要压缩（不应视为错误）。
func (a *Agent) Compact(ctx context.Context, instructions, reason string) (*CompactionResult, error) {
	// 串行化 kernel 复合操作：手动 /compact 可能与正在执行的 context_apply 重叠。
	a.ctxMu.Lock()
	defer a.ctxMu.Unlock()

	ke, err := a.kernelEngine()
	if err != nil {
		return nil, err
	}

	branch := a.opts.Session.ActiveBranch()
	km := kernelMessages(branch)
	if len(km) == 0 {
		return nil, fmt.Errorf("agent: 历史为空，没有可压缩的内容")
	}
	tokensBefore := a.contextTokens()

	res, err := ke.s.Turn(km, tokensBefore)
	if err != nil {
		return nil, err
	}
	if err := a.saveKernelState(ke); err != nil {
		return nil, err
	}

	nudge := res.Nudge
	if nudge == nil || len(nudge.CompressibleRanges) == 0 {
		usage := 0.0
		if nudge != nil {
			usage = nudge.ContextUsage
		}
		return nil, fmt.Errorf(
			"agent: kernel 没有给出可压缩区间（上下文占用 %.0f%%，%d tokens）",
			usage*100, tokensBefore,
		)
	}
	// 阈值触发的自动压缩尊重 kernel 的判断；手动 /compact 一律执行。
	//
	// 注意判断依据是 CompressibleRanges 是否为空，不是 nudge.ShouldInject：
	// ShouldInject 在上游是「是否把提示文本注入对话」的节流开关（受 frequency /
	// iterationThreshold / growthFloor 控制），不是「该不该压缩」。用它做门禁会
	// 在上下文已经很高、区间也够的时候因为节流窗口没到而整轮跳过。
	if reason != "manual" && len(nudge.CompressibleRanges) == 0 {
		return nil, nil
	}

	// 一次只压 kernel 排名第一的区间：每个区间都要一次模型调用，
	// 剩下的留给后续轮次，避免单轮耗时叠加。
	r := nudge.CompressibleRanges[0]
	selected := selectRefRange(res.Messages, r.StartRef, r.EndRef)
	if len(selected) == 0 {
		return nil, fmt.Errorf("agent: kernel 区间 %s..%s 内没有消息", r.StartRef, r.EndRef)
	}

	a.emit.Emit(CompactionStartEvent{
		Reason: reason, TokensBefore: tokensBefore, CutEntryID: r.StartRef,
	})

	summary, err := a.summarizeTranscript(ctx, transcript(selected), instructions)
	if err != nil {
		return nil, err
	}
	// kernel 对摘要长度有硬下限（50 字符），过短的摘要会被整批拒绝；
	// 这里用区间内消息的概要兵底，避免因为摘要太短而丢掉整次压缩。
	summary = padSummary(summary, selected)

	cr, err := ke.s.ApplyCompression([]kernel.Range{{
		StartRef: r.StartRef,
		EndRef:   r.EndRef,
		Summary:  summary,
	}})
	if err != nil {
		return nil, err
	}
	if err := a.saveKernelState(ke); err != nil {
		return nil, err
	}
	if len(cr.Errors) > 0 {
		return nil, fmt.Errorf("agent: kernel 压缩失败: %s", strings.Join(cr.Errors, "; "))
	}

	// 把 kernel 的新块镜像成 compaction entry，供 Messages() 投影。
	tokensAfter := tokensBefore - cr.TokensCompressed
	if tokensAfter < 0 {
		tokensAfter = 0
	}
	mirrored, err := a.mirrorBlocks(ke, cr, tokensBefore, tokensAfter)
	if err != nil {
		return nil, err
	}
	blockID := ""
	if len(mirrored) > 0 {
		blockID = mirrored[0].BlockID
	}
	// 压缩成功 → 重置提示节流，下一轮可以重新提醒。
	a.resetNudge()

	// kernel 块里的直接消息若还是 session 里的 entry，就一并算作被摘要的消息。
	summarized := summarizedMessages(a.opts.Session, cr.Blocks)

	result := &CompactionResult{
		Summary:      summary,
		TokensBefore: tokensBefore,
		TokensAfter:  tokensAfter,
		BlockID:      blockID,
		StartRef:     r.StartRef,
		EndRef:       r.EndRef,
		Summarized:   summarized,
	}
	a.emit.Emit(CompactionEndEvent{
		Summary: summary, TokensBefore: tokensBefore, TokensAfter: tokensAfter,
		CutEntryID: r.StartRef,
	})
	return result, nil
}

// minSummaryChars 是 kernel 要求的摘要最小字符数。
const minSummaryChars = 50

// padSummary 在摘要过短时追加区间概要，满足 kernel 的长度下限。
func padSummary(summary string, msgs []kernel.Message) string {
	if len([]rune(summary)) >= minSummaryChars {
		return summary
	}
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n被压缩区间包含以下内容：")
	n := 0
	for _, m := range msgs {
		line := firstLine(m.Text, 80)
		if line == "" {
			continue
		}
		b.WriteString("\n- [")
		b.WriteString(string(m.Role))
		b.WriteString("] ")
		b.WriteString(line)
		if n++; n >= 20 {
			break
		}
	}
	return b.String()
}

// saveKernelState 把 kernel 状态快照塞回 Session，随 JSONL 一起落盘。
func (a *Agent) saveKernelState(ke *kernelEngine) error {
	st, err := ke.s.StateJSON()
	if err != nil {
		return fmt.Errorf("agent: 导出 kernel 状态失败: %w", err)
	}
	a.opts.Session.SetKernelState(st)
	return nil
}

// summarizedMessages 把块里的消息 id 还原成 session 中的消息（调试用）。
func summarizedMessages(s *Session, blocks []kernel.Block) []Message {
	var out []Message
	for _, b := range blocks {
		for _, id := range b.DirectMessageIDs {
			e, ok := s.GetEntry(id)
			if !ok || e.Message == nil {
				continue
			}
			out = append(out, e.Message)
		}
	}
	return out
}

// summarizeTranscript 调用模型生成摘要。
func (a *Agent) summarizeTranscript(ctx context.Context, body, instructions string) (string, error) {
	var b strings.Builder
	b.WriteString("以下是需要压缩的会话历史：\n\n")
	b.WriteString(body)
	if strings.TrimSpace(instructions) != "" {
		b.WriteString("\n\n压缩的侧重点：")
		b.WriteString(instructions)
	}

	req := Request{
		SystemPrompt: SummaryPrompt,
		Messages:     []Message{&UserMessage{Content: []Block{TextBlock{Text: b.String()}}, At: now()}},
		Model:        a.opts.SummarizeModel,
		// 压缩请求不携带工具：这是纯文本总结任务。
		Purpose: PurposeSummarize,
	}
	msg, err := a.opts.Summarizer.Stream(ctx, req, nil)
	if err != nil {
		return "", fmt.Errorf("agent: 生成压缩摘要失败: %w", err)
	}
	text := strings.TrimSpace(msg.Text())
	if text == "" {
		return "", fmt.Errorf("agent: 压缩摘要为空")
	}
	return text, nil
}

// serializeConversation 把消息序列化成纯文本，供摘要模型阅读。
func serializeConversation(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch v := m.(type) {
		case *UserMessage:
			b.WriteString("[user] ")
			b.WriteString(blocksText(v.Content))
			b.WriteString("\n")
		case *AssistantMessage:
			b.WriteString("[assistant] ")
			b.WriteString(blocksText(v.Content))
			for _, blk := range v.Content {
				if tc, ok := blk.(ToolCallBlock); ok {
					b.WriteString("\n  [tool-call] ")
					b.WriteString(tc.Name)
					b.WriteString(formatArgs(tc.Arguments))
				}
			}
			if v.ErrorMessage != "" {
				b.WriteString("\n  [error] ")
				b.WriteString(v.ErrorMessage)
			}
			b.WriteString("\n")
		case *ToolResultMessage:
			b.WriteString("[tool-result:")
			b.WriteString(v.ToolName)
			b.WriteString("] ")
			b.WriteString(truncateForSummary(blocksText(v.Content), serializeToolResultLimit))
			b.WriteString("\n")
		case *BashExecutionMessage:
			b.WriteString("[bash] $ ")
			b.WriteString(v.Command)
			b.WriteString("\n")
			b.WriteString(truncateForSummary(v.Output, serializeToolResultLimit))
			b.WriteString("\n")
		case *CustomMessage:
			b.WriteString("[custom:")
			b.WriteString(v.CustomType)
			b.WriteString("] ")
			b.WriteString(blocksText(v.Content))
			b.WriteString("\n")
		case *CompactionSummaryMessage:
			b.WriteString("[previous-summary] ")
			b.WriteString(v.Summary)
			b.WriteString("\n")
		case *BranchSummaryMessage:
			b.WriteString("[branch-summary] ")
			b.WriteString(v.Summary)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// formatArgs 把工具参数渲染成紧凑的一行。
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return "()"
	}
	var b strings.Builder
	b.WriteString("(")
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// 稳定顺序：按 key 排序。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		fmt.Fprintf(&b, "%v", args[k])
	}
	b.WriteString(")")
	return b.String()
}

// truncateForSummary 截断过长文本，首尾保留。
func truncateForSummary(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := limit / 2
	tail := limit - head
	return s[:head] + "\n...[截断]...\n" + s[len(s)-tail:]
}
