package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/leijux/acp-agent/internal/kernel"
	"github.com/leijux/acp-agent/internal/tools"
)

// 本文件把 tools.ContextBackend 接到 kernel 上，实现「主动压缩」：
// 由模型自己调用 context_check / context_apply / context_search /
// context_decompress，而不是宿主在后台偷偷摘要。
//
// 宿主只剩两层兜底：
//   - SoftCompactUsage（0.45）：往对话里注入一条提示，告知模型可以压缩了；
//   - HardCompactUsage（0.90）：模型迟迟不动手时强制调用 Compact。
//
// 对齐上游 billion-context 的主动模式（README.zh-CN.md:75-102）。

// contextBackend 是 tools.ContextBackend 的实现，绑定到某个 Agent 的 kernel 会话。
type contextBackend struct{ a *Agent }

// ContextTools 返回可直接注册进 Registry 的上下文管理工具。
//
// 正常情况下不需要手动调用：New 在 Options.EnableContextTools 为 true 且
// Registry 非空时会自动注册。
func (a *Agent) ContextTools() []tools.Tool { return tools.ContextTools(&contextBackend{a: a}) }

// registerContextTools 把四个工具注册进 Registry（已存在同名工具时跳过）。
func (a *Agent) registerContextTools() error {
	if a.opts.Registry == nil || a.opts.DisableContextTools {
		return nil
	}
	var add []tools.Tool
	for _, t := range a.ContextTools() {
		if _, exists := a.opts.Registry.Get(t.Name()); exists {
			continue
		}
		add = append(add, t)
	}
	if len(add) == 0 {
		return nil
	}
	return a.opts.Registry.Register(add...)
}

// withKernel 串行化所有 kernel 复合操作。
//
// Turn + ApplyCompression 不是原子的；context_check（并发安全）可能与
// context_apply（独占）同时跑，所以这一层必须自己加锁，不能只依赖
// kernel.Session 内部的细粒度锁。
func (a *Agent) withKernel(fn func(*kernelEngine) error) error {
	a.ctxMu.Lock()
	defer a.ctxMu.Unlock()
	ke, err := a.kernelEngine()
	if err != nil {
		return err
	}
	return fn(ke)
}

// ContextStatus 实现 tools.ContextBackend。
func (c *contextBackend) ContextStatus(_ context.Context) (tools.ContextStatus, error) {
	var out tools.ContextStatus
	err := c.a.withKernel(func(ke *kernelEngine) error {
		branch := c.a.opts.Session.ActiveBranch()
		km := kernelMessages(branch)
		tokens := c.a.contextTokens()

		res, err := ke.s.Turn(km, tokens)
		if err != nil {
			return err
		}
		if err := c.a.saveKernelState(ke); err != nil {
			return err
		}

		out = tools.ContextStatus{
			Usage:        float64(tokens) / float64(limitOr(ke.limit, 1)),
			TokenCount:   tokens,
			ContextLimit: ke.limit,
		}
		if st, err := ke.s.Status(tokens); err == nil {
			out.Usage = st.ContextUsage
			out.ActiveBlocks = st.ActiveBlocks
			out.TokensCompressed = st.TokensCompressed
			if st.TokenCount > 0 {
				out.TokenCount = st.TokenCount
			}
			if st.ModelContextLimit > 0 {
				out.ContextLimit = st.ModelContextLimit
			}
		}
		if res.Nudge == nil {
			out.Reason = fmt.Sprintf("占用 %.0f%%，尚未达到 kernel 的压缩水位", out.Usage*100)
			return nil
		}
		out.Reason = res.Nudge.Reason
		for _, r := range res.Nudge.CompressibleRanges {
			sel := selectRefRange(res.Messages, r.StartRef, r.EndRef)
			info := tools.ContextRangeInfo{
				StartRef: r.StartRef,
				EndRef:   r.EndRef,
				Messages: len(sel),
				Tokens:   sumTokens(sel),
			}
			if len(sel) > 0 {
				info.Preview = firstLine(sel[0].Text, 100)
			}
			out.Ranges = append(out.Ranges, info)
		}
		return nil
	})
	return out, err
}

// ContextApply 实现 tools.ContextBackend。
//
// ranges 里没有 startRef/endRef 时（模型只写了 summary），自动选 kernel 推荐的
// 第一段区间 —— 这样模型可以先看 context_check 再决定，也可以直接压。
func (c *contextBackend) ContextApply(_ context.Context, ranges []tools.ContextRange) (tools.ContextApplyResult, error) {
	var out tools.ContextApplyResult
	err := c.a.withKernel(func(ke *kernelEngine) error {
		branch := c.a.opts.Session.ActiveBranch()
		km := kernelMessages(branch)
		if len(km) == 0 {
			return fmt.Errorf("历史为空，没有可压缩的内容")
		}
		tokensBefore := c.a.contextTokens()

		res, err := ke.s.Turn(km, tokensBefore)
		if err != nil {
			return err
		}
		if err := c.a.saveKernelState(ke); err != nil {
			return err
		}

		nudges := map[string]tools.ContextRangeInfo{}
		if res.Nudge != nil {
			for _, r := range res.Nudge.CompressibleRanges {
				nudges[r.StartRef+".."+r.EndRef] = tools.ContextRangeInfo{StartRef: r.StartRef, EndRef: r.EndRef}
			}
		}

		krs := make([]kernel.Range, 0, len(ranges))
		for i, r := range ranges {
			kr := kernel.Range{StartRef: r.StartRef, EndRef: r.EndRef, Topic: r.Topic, Summary: r.Summary}
			if kr.StartRef == "" || kr.EndRef == "" {
				// 没给引用：用 kernel 排名第一的区间（多个缺引用时依次顺延）。
				if res.Nudge == nil || i >= len(res.Nudge.CompressibleRanges) {
					return fmt.Errorf("没有可用的推荐区间，请先调用 context_check 获取 startRef/endRef")
				}
				rec := res.Nudge.CompressibleRanges[i]
				kr.StartRef, kr.EndRef = rec.StartRef, rec.EndRef
			}
			// kernel 对摘要长度有硬下限（50 字符），过短会整批被拒。
			sel := selectRefRange(res.Messages, kr.StartRef, kr.EndRef)
			kr.Summary = padSummary(kr.Summary, sel)
			krs = append(krs, kr)
		}

		cr, err := ke.s.ApplyCompression(krs)
		if err != nil {
			return err
		}
		if err := c.a.saveKernelState(ke); err != nil {
			return err
		}

		tokensAfter := tokensBefore - cr.TokensCompressed
		if tokensAfter < 0 {
			tokensAfter = 0
		}
		blocks, err := c.a.mirrorBlocks(ke, cr, tokensBefore, tokensAfter)
		if err != nil {
			return err
		}

		out = tools.ContextApplyResult{
			BlocksCreated:    cr.BlocksCreated,
			TokensCompressed: cr.TokensCompressed,
			Errors:           cr.Errors,
			Blocks:           blocks,
		}
		c.a.resetNudge()
		return nil
	})
	return out, err
}

// ContextSearch 实现 tools.ContextBackend。
func (c *contextBackend) ContextSearch(_ context.Context, query string, limit int) ([]tools.ContextHit, error) {
	if limit <= 0 {
		limit = 5
	}
	var out []tools.ContextHit
	err := c.a.withKernel(func(ke *kernelEngine) error {
		raw, err := ke.s.SearchBlocksWith(query, map[string]any{"limit": limit})
		if err != nil {
			return err
		}
		for _, item := range raw {
			var blk struct {
				BlockID string  `json:"blockId"`
				Topic   string  `json:"topic"`
				Summary string  `json:"summary"`
				Score   float64 `json:"score"`
			}
			if err := json.Unmarshal(item, &blk); err != nil {
				continue
			}
			if blk.BlockID == "" {
				continue
			}
			out = append(out, tools.ContextHit{
				BlockID: blk.BlockID,
				Topic:   blk.Topic,
				Summary: blk.Summary,
				Score:   blk.Score,
			})
			if len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// ContextDecompress 实现 tools.ContextBackend。
func (c *contextBackend) ContextDecompress(_ context.Context, blockID string) (tools.ContextBlock, error) {
	var out tools.ContextBlock
	err := c.a.withKernel(func(ke *kernelEngine) error {
		blk, err := ke.s.Decompress(strings.TrimSpace(blockID))
		if err != nil {
			return err
		}
		if blk == nil {
			return fmt.Errorf("压缩块 %s 不存在", blockID)
		}
		out = tools.ContextBlock{
			BlockID:          blk.BlockID,
			Topic:            blk.Topic,
			Tier:             blk.Tier,
			Summary:          blk.Summary,
			MessageIDs:       blk.DirectMessageIDs,
			TokensCompressed: blk.CompressedTokens,
		}
		return nil
	})
	return out, err
}

// mirrorBlocks 把 kernel 的新块镜像成 compaction entry。
//
// 必须在 kernel 建块之后立刻调用：Session.Messages() 靠这些 entry 做投影
// （块摘要插回原位置、被折叠的消息隐藏）。
func (a *Agent) mirrorBlocks(ke *kernelEngine, cr kernel.CompressionResult, tokensBefore, tokensAfter int) ([]tools.ContextBlock, error) {
	var out []tools.ContextBlock
	for _, b := range cr.Blocks {
		if ke.mirrored[b.BlockID] {
			continue
		}
		ke.mirrored[b.BlockID] = true
		if _, err := a.opts.Session.AppendCompactionBlock(
			b.BlockID, b.Summary, b.DirectMessageIDs, tokensBefore, tokensAfter,
		); err != nil {
			return nil, err
		}
		out = append(out, tools.ContextBlock{
			BlockID:      b.BlockID,
			Topic:        b.Topic,
			Tier:         b.Tier,
			Summary:      b.Summary,
			MessageIDs:   b.DirectMessageIDs,
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfter,
		})
	}
	return out, nil
}

// contextNudgeText 是注入对话的压缩提示。
func contextNudgeText(usage float64, limit int, ranges []tools.ContextRangeInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【上下文提示】当前占用 %.0f%%（上限 %d tokens）。", usage*100, limit)
	if len(ranges) == 0 {
		b.WriteString("暂时没有值得压缩的内容，继续工作即可。")
		return b.String()
	}
	b.WriteString("以下区间可以压缩：\n")
	for i, r := range ranges {
		fmt.Fprintf(&b, "%d. %s..%s（%d 条，约 %d tokens）\n", i+1, r.StartRef, r.EndRef, r.Messages, r.Tokens)
	}
	b.WriteString("用 context_apply 提交：为每个区间写一份自包含摘要" +
		"（保留目标、决定与理由、文件路径、签名、错误原文、待办）。" +
		"原文不会删除，之后可用 context_search / context_decompress 找回。")
	return b.String()
}

// contextNudgeMessage 生成一条注入对话的压缩提示。
//
// 返回 nil 表示本次不注入（已提示过且上下文没有明显增长，或根本没有可压区间）。
// 节流阀是必需的：提示本身也占 token，每轮重复只会加剧拥挤。
func (a *Agent) contextNudgeMessage() *CustomMessage {
	tokens := a.contextTokens()
	a.nudgeMu.Lock()
	prev := a.nudgedAt
	if prev > 0 && tokens < prev+prev/5 {
		a.nudgeMu.Unlock()
		return nil
	}
	a.nudgeMu.Unlock()

	st, err := (&contextBackend{a: a}).ContextStatus(context.Background())
	if err != nil || len(st.Ranges) == 0 {
		// 没有可压区间就不提，避免催模型做无用功。
		return nil
	}

	a.nudgeMu.Lock()
	a.nudgedAt = tokens
	a.nudgeMu.Unlock()

	return &CustomMessage{
		CustomType: "context-nudge",
		Content:    []Block{TextBlock{Text: contextNudgeText(st.Usage, st.ContextLimit, st.Ranges)}},
		Display:    false,
		At:         now(),
	}
}

// resetNudge 在压缩成功后重置节流状态，让下一轮还能再提示。
func (a *Agent) resetNudge() {
	a.nudgeMu.Lock()
	a.nudgedAt = 0
	a.nudgeMu.Unlock()
}

// limitOr 返回 v，v<=0 时返回 def。
func limitOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// sumTokens 估算一组消息的 token 总量。
func sumTokens(msgs []kernel.Message) int {
	n := 0
	for _, m := range msgs {
		n += EstimateTokens(m.Text)
	}
	return n
}
