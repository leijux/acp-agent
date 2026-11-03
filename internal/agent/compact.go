package agent

import (
	"context"
	"errors"
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
	// HardCompactUsage 是宿主强制压缩的绝对兜底比例，对齐上游的
	// maxContextLimitPct = 0.75 之上再留一层余量。
	//
	// 主动模式（模型自己调用 context_apply）正常运行时不应走到这里；
	// 它是模型不肯压缩时的安全网，避免上下文撞上硬上限。
	//
	// 注意：真正先刹车的是 outputAllowance 跌破 preferredOutputTokens，
	// 它比本水位更早触发，具体水位见 outputAllowance。
	HardCompactUsage = 0.90

	// minOutputTokens 是 max_tokens 的下限。上下文挤到没有输出空间时
	// 也发这个值而不是 0：0 会让 provider 省掉该字段，服务端改用默认上限，
	// 反而更容易超窗。实际下限还会随窗口缩小（见 minOutput）。
	minOutputTokens = 512
	// inputSafetyPct 是输入估算的保守系数（百分比）。估算走字符启发式，
	// 对代码/JSON 仍偏向低估，所以扣输出空间时按偏大值算。
	inputSafetyPct = 115
	// preferredOutputShare 是「值得为输出保留的份额」：窗口 / 8。
	//
	// 它只用于定压缩水位（输出快被挤扁了就压），**不是** max_tokens 的
	// 天花板：上下文还空着时发满模型声明上限完全合法（sum 检验过得去）。
	preferredOutputShare = 8
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

// shouldCompact 判断是否达到「宿主强制压缩」的水位。
//
// 两个条件满足任一即压缩：
//  1. 占用达到 HardCompactUsage（绝对兜底，模型死活不肯压时用）；
//  2. 估算占用超过 inputBudget —— 再长下去就只能靠压缩 max_tokens 换空间了。
//
// 条件 2 是真正先刹车的那个：上游按 messages + completion 求和判窗口，
// 一个 1M 窗口 / 384k 输出的模型输入天花板只有约 63%，光靠 90% 水位永远
// 够不到（输入天花板只有约 63%，命中的是 667184 + 384000 > 1048576）。
//
// 这只是廉价的快速判断，避免每轮都跑一次 kernel 渲染。真正的决策（压哪一段、
// 压多少）由 kernel 的 nudge 给出，见 Compact。
func (a *Agent) shouldCompact() bool {
	used := a.contextTokens()
	if a.opts.ContextWindow > 0 &&
		float64(used)/float64(a.opts.ContextWindow) >= HardCompactUsage {
		return true
	}
	return a.outputAllowance(used) < a.preferredOutputTokens()
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
	return a.estimateInputTokens(
		BuildSystemPrompt(a.opts.Context),
		ProjectMessages(a.opts.Session.Messages()),
		toolSpecs(a.opts.Registry),
	)
}

// estimateInputTokens 估算一次请求的输入侧 token：system prompt + 消息 + 工具定义。
//
// 三者都是实发内容，必须一起算。只算消息会让估算系统性偏低，而低估的
// 唯一后果是「以为还有空间」——然后被上游 400。
func (a *Agent) estimateInputTokens(sys string, msgs []Message, tools []ToolSpec) int {
	return a.opts.TokenCounter(msgs) + EstimateTokens(sys) + EstimateToolSpecsTokens(tools)
}

// ContextTokens 暴露当前上下文估算值（供状态栏/测试用）。
func (a *Agent) ContextTokens() int { return a.contextTokens() }

// minOutput 返回 max_tokens 的下限。
//
// 下限本身也必须随窗口缩放：窗口 1024 时发 512 就可能把输入顶出窗口，
// 而上游要的是 messages + completion ≤ 窗口。
func (a *Agent) minOutput() int {
	if limit := a.opts.ContextWindow / 8; limit < minOutputTokens {
		return limit
	}
	return minOutputTokens
}

// outputReserve 返回给「估算偏差 + 下一轮思考」留的余量，随窗口缩放。
//
// 不能直接用 Options.ReserveTokens：它默认 16384 且与窗口无关，8k/32k 的
// 本地模型会被这个绝对量吃成负数（max_tokens 永远停在下限）。
func (a *Agent) outputReserve() int {
	limit := a.opts.ContextWindow / 8
	if a.opts.ReserveTokens < limit {
		return a.opts.ReserveTokens
	}
	return limit
}

// outputAllowance 返回「给定输入估算下，还能留给输出的 token 数」。
//
// 上游的硬约束是输入 + completion ≤ 窗口，输入按保守系数放大后解出右边：
//
//	max_tokens ≤ 窗口 − used×inputSafetyPct/100 − outputReserve
//
// 它既是 max_tokens 的唯一上界来源（再加上模型自己的声明上限），也是压缩
// 水位的依据 —— 两边用同一个式子，才不会出现「输出已经跌到下限、压缩还没
// 触发」的错位。
func (a *Agent) outputAllowance(used int) int {
	avail := a.opts.ContextWindow - used*inputSafetyPct/100 - a.outputReserve()
	if floor := a.minOutput(); avail < floor {
		return floor
	}
	return avail
}

// preferredOutputTokens 是「值得为输出保留的份额」：min(模型上限, 窗口/8)。
//
// 输出低于它就意味着该压缩了。不拿模型声明的值当水位：deepseek-flash 声明
// 384k 输出（窗口的 37%），拿它当水位等于逼着在 60% 占用就压缩。
func (a *Agent) preferredOutputTokens() int {
	out := a.opts.Model.MaxOutputTokens
	if limit := a.opts.ContextWindow / preferredOutputShare; out <= 0 || out > limit {
		out = limit
	}
	return out
}

// outputBudget 是本次请求实际可用的 max_tokens。
//
// 只受两件事约束：模型声明的上限、以及窗口剩余空间（outputAllowance）。
// 旧实现恒发模型上限，于是 667184(输入) + 384000(输出) > 1048576 直接 400；
// 但反向的错误同样不能犯：拿「理想输出」当天花板会把长生成白白截短。
func (a *Agent) outputBudget(used int) int {
	out := a.opts.Model.MaxOutputTokens
	if out <= 0 {
		out = a.opts.ContextWindow / 4
	}
	if avail := a.outputAllowance(used); avail < out {
		out = avail
	}
	if floor := a.minOutput(); out < floor {
		out = floor
	}
	return out
}

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

// ErrNothingToCompact 表示 kernel 判断当前没有可压缩区间。
//
// 它不是错误：上下文里还没有足够大的可压单元（新会话、或全都是一条大消息）。
// 调用方应把它当成「这一轮不需要压」而不是向用户报错 —— 否则水位提前到
// 输出快被挤扁时，每轮都会刷一条 error notice。
var ErrNothingToCompact = errors.New("agent: 没有可压缩区间")

// Compact 执行一次压缩：kernel 划定区间 → 模型生成摘要 → 落成压缩块。
//
// reason 为 "manual" / "threshold" / "overflow"；instructions 是可选的人工侧重
// （对应 /compact <instructions>）。
//
// 返回 err 满足 errors.Is(err, ErrNothingToCompact) 时表示「kernel 没有给出
// 可压区间」（即本次没压，不是失败）。
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
		// 阈值/溢出触发的自动压缩是兜底；拿不出区间时必须报出来：
		// 沉默会把问题推给下一次请求（max_tokens 被挤到下限 → 回复无故截断），
		// 用户看不到「该 /new 或手动 /compact 了」这个信号。
		//
		// 判据是 CompressibleRanges 而不是 nudge.ShouldInject：后者在上游只是
		// 「要不要把提示文本注入对话」的节流开关（frequency / iterationThreshold /
		// growthFloor），拿它做门禁会在区间充足时因为节流窗口没到而整轮跳过。
		return nil, fmt.Errorf(
			"%w（上下文占用 %.0f%%，%d tokens）",
			ErrNothingToCompact, usage*100, tokensBefore,
		)
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
