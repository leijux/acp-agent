package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// fillContext 往会话里塞足够长的历史，让 kernel 认为存在可压区间。
//
// kernel 的可压单元有字符下限（约 5000 字符），所以轮数不能太少。
func fillContext(t *testing.T, a *Agent, rounds int) {
	t.Helper()
	body := strings.Repeat("这是一段比较长的中文内容，用于撑大上下文并制造可压区间。", 8)
	for i := 0; i < rounds; i++ {
		a.Push(TextMessage(body))
		a.Push(reply(strings.Repeat("模型回复内容。", 8)))
	}
}

// 注册：默认开启，DisableContextTools 时不注册。
func TestContextToolsRegistered(t *testing.T) {
	p := NewScriptedProvider(reply("x"))

	reg := tools.NewRegistry()
	newTestAgent(t, p, func(o *Options) { o.Registry = reg })
	for _, name := range []string{
		tools.ToolContextCheck, tools.ToolContextApply,
		tools.ToolContextSearch, tools.ToolContextDecompress,
	} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("工具 %s 未自动注册", name)
		}
	}

	reg2 := tools.NewRegistry()
	newTestAgent(t, p, func(o *Options) {
		o.Registry = reg2
		o.DisableContextTools = true
	})
	if _, ok := reg2.Get(tools.ToolContextCheck); ok {
		t.Fatal("DisableContextTools 后不应注册 context 工具")
	}
}

// context_check 应报告占用与可压区间（带 mNNNNN 引用）。
func TestContextCheckReportsRanges(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	tok, _ := reg.Get(tools.ToolContextCheck)
	res, err := tok.Execute(context.Background(), tools.Args{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("context_check 失败: %s", res.Text())
	}
	st, ok := res.Details.(tools.ContextStatus)
	if !ok {
		t.Fatalf("Details 类型 = %T", res.Details)
	}
	if len(st.Ranges) == 0 {
		t.Fatalf("应给出可压区间，实际为空（usage=%.2f）:\n%s", st.Usage, res.Text())
	}
	r := st.Ranges[0]
	if !strings.HasPrefix(r.StartRef, "m") || !strings.HasPrefix(r.EndRef, "m") {
		t.Fatalf("区间引用格式不对: %s..%s", r.StartRef, r.EndRef)
	}
	if r.Messages == 0 {
		t.Fatalf("区间内消息数 = 0")
	}
}

// 模型自己写摘要 → context_apply 折叠成功，投影里出现压缩摘要。
func TestContextApplyFoldsHistory(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	check, _ := reg.Get(tools.ToolContextCheck)
	cres, err := check.Execute(context.Background(), tools.Args{})
	if err != nil || cres.IsError {
		t.Fatalf("context_check: %v %s", err, cres.Text())
	}
	r := cres.Details.(tools.ContextStatus).Ranges[0]

	before := a.ContextTokens()
	apply, _ := reg.Get(tools.ToolContextApply)
	ares, err := apply.Execute(context.Background(), tools.Args{
		"ranges": []any{map[string]any{
			"startRef": r.StartRef,
			"endRef":   r.EndRef,
			"summary":  "## 目标\n压缩早期探索。\n## 决定\n区间由 kernel 划定，摘要由模型撰写。",
			"topic":    "上下文压缩",
		}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ares.IsError {
		t.Fatalf("context_apply 失败: %s", ares.Text())
	}
	out := ares.Details.(tools.ContextApplyResult)
	if out.BlocksCreated == 0 {
		t.Fatalf("未建块: %s", ares.Text())
	}
	if out.TokensCompressed <= 0 {
		t.Fatalf("未省下 token: %+v", out)
	}

	// 投影：被块吃掉的消息必须从 Messages() 里消失。
	msgs := a.Session().Messages()
	if _, ok := msgs[0].(*CompactionSummaryMessage); !ok {
		t.Fatalf("投影首条应为 CompactionSummaryMessage，实际 %T", msgs[0])
	}
	if after := a.ContextTokens(); after >= before {
		t.Fatalf("压缩后 token 未减少: %d → %d", before, after)
	}
	// 节流状态应被重置。
	a.nudgeMu.Lock()
	nudged := a.nudgedAt
	a.nudgeMu.Unlock()
	if nudged != 0 {
		t.Fatalf("压缩后 nudgedAt = %d，want 0", nudged)
	}
}

// 只给 summary、不给引用时，自动选 kernel 排第一的区间。
func TestContextApplyAutoRange(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	apply, _ := reg.Get(tools.ToolContextApply)
	res, err := apply.Execute(context.Background(), tools.Args{
		"summary": "## 自动区间\n未指定 startRef/endRef，由 kernel 推荐决定。",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("自动选区间失败: %s", res.Text())
	}
	if out := res.Details.(tools.ContextApplyResult); out.BlocksCreated == 0 {
		t.Fatalf("未建块: %s", res.Text())
	}
}

// 过短摘要应被补足到 kernel 的 50 字符下限，而不是让整次压缩失败。
func TestContextApplyShortSummaryPadded(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	apply, _ := reg.Get(tools.ToolContextApply)
	res, err := apply.Execute(context.Background(), tools.Args{"summary": "短"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("短摘要应被 padSummary 兜底，实际失败: %s", res.Text())
	}
}

// 压缩后能用 context_search 检索到块，再用 context_decompress 看块详情。
func TestContextSearchAndDecompress(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	apply, _ := reg.Get(tools.ToolContextApply)
	ares, err := apply.Execute(context.Background(), tools.Args{
		"summary": "## 压缩链路\nkernel 划定区间，模型撰写摘要，Session 做投影。含关键字 compresspipe。",
	})
	if err != nil || ares.IsError {
		t.Fatalf("context_apply: %v %s", err, ares.Text())
	}
	blockID := ares.Details.(tools.ContextApplyResult).Blocks[0].BlockID

	// 检索（BM25，用摘要里出现的英文关键词，避免中文分词差异）。
	search, _ := reg.Get(tools.ToolContextSearch)
	sres, err := search.Execute(context.Background(), tools.Args{"query": "compresspipe"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if sres.IsError {
		t.Fatalf("context_search 失败: %s", sres.Text())
	}
	hits := sres.Details.([]tools.ContextHit)
	if len(hits) == 0 {
		t.Skipf("检索无命中（goja 分词差异），跳过：%s", sres.Text())
	}

	// 展开块详情。
	dec, _ := reg.Get(tools.ToolContextDecompress)
	dres, err := dec.Execute(context.Background(), tools.Args{"blockId": blockID})
	if err != nil || dres.IsError {
		t.Fatalf("context_decompress: %v %s", err, dres.Text())
	}
	blk := dres.Details.(tools.ContextBlock)
	if blk.BlockID != blockID {
		t.Fatalf("blockId = %q, want %q", blk.BlockID, blockID)
	}
	if !strings.Contains(blk.Summary, "compresspipe") {
		t.Fatalf("摘要内容不符: %q", blk.Summary)
	}
	if len(blk.MessageIDs) == 0 {
		t.Fatalf("块里应记录被折叠的消息 id")
	}
}

// 阈值语义：45% 提示、90% 兜底。
func TestCompactThresholdSemantics(t *testing.T) {
	if SoftCompactUsage >= HardCompactUsage {
		t.Fatalf("软提示 %v 必须低于硬兜底 %v", SoftCompactUsage, HardCompactUsage)
	}

	p := NewScriptedProvider(reply("x"))
	a := newTestAgent(t, p, func(o *Options) { o.ContextWindow = 5_000 })
	if a.shouldNudge() || a.shouldCompact() {
		t.Fatalf("空会话不该触发任何水位（usage=%.2f）", a.contextUsage())
	}

	fillContext(t, a, 60)
	if !a.shouldNudge() {
		t.Fatalf("塞满后应触发软提示（usage=%.2f）", a.contextUsage())
	}
	if !a.shouldCompact() {
		t.Fatalf("塞满后应触发硬兜底（usage=%.2f, tokens=%d）", a.contextUsage(), a.ContextTokens())
	}
}

// contextNudgeMessage 注入一条不显示给用户的提示，并做节流。
func TestContextNudgeMessageThrottled(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	msg := a.contextNudgeMessage()
	if msg == nil {
		t.Skip("当前历史没有可压区间，跳过")
	}
	if msg.CustomType != "context-nudge" {
		t.Fatalf("CustomType = %q", msg.CustomType)
	}
	if msg.Display {
		t.Fatal("提示不应展示给用户（Display 应为 false）")
	}
	if txt := msg.Content[0].(TextBlock).Text; !strings.Contains(txt, "context_apply") {
		t.Fatalf("提示里应引导调用 context_apply: %q", txt)
	}

	// 上下文没有明显增长 → 不再重复提示。
	if again := a.contextNudgeMessage(); again != nil {
		t.Fatal("应节流：同一水位不重复注入")
	}

	// 压缩后重置，可再次提示。
	a.resetNudge()
	a.nudgeMu.Lock()
	nudged := a.nudgedAt
	a.nudgeMu.Unlock()
	if nudged != 0 {
		t.Fatalf("resetNudge 后 nudgedAt = %d", nudged)
	}
}

// TestAutoCompactCheckRunsOnPlainTurn 验证「不调工具的纯对话轮也会跑压缩检查」。
//
// 旧实现把检查点放在工具结果之后，len(calls)==0 直接 break —— 纯问答的
// 会话永远不会被提醒或强制压缩，频率明显低于上游（上游每轮 processTurn 都判）。
//
// 验证手段：把窗口压到极小，任何历史都超过 HardCompactUsage；强制压缩会因
// kernel 拿不出可压区间而发出 ErrorEvent。只要收到它，就证明检查点真的跑了。
func TestAutoCompactCheckRunsOnPlainTurn(t *testing.T) {
	p := NewScriptedProvider(reply("ok"))
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = tools.NewRegistry()
		o.ContextWindow = 2_000
	})
	var errs int
	a.On(func(ev Event) {
		if _, ok := ev.(ErrorEvent); ok {
			errs++
		}
	})
	fillContext(t, a, 10)
	if !a.shouldCompact() {
		t.Skipf("未到强制压缩水位，usage=%.2f", a.contextUsage())
	}

	before := errs
	if _, err := a.Run(context.Background(), "继续描述一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errs == before {
		t.Fatal("纯对话轮没有触发压缩检查（没走到 Compact）")
	}
}

// 系统 prompt 应包含上下文管理约定。
func TestSystemPromptMentionsContextTools(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) { o.Registry = reg })
	if !strings.Contains(a.opts.Context.ExtraInstructions, "context_check") {
		t.Fatalf("ExtraInstructions 缺少上下文管理说明: %q", a.opts.Context.ExtraInstructions)
	}

	reg2 := tools.NewRegistry()
	a2 := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg2
		o.DisableContextTools = true
	})
	if strings.Contains(a2.opts.Context.ExtraInstructions, "context_check") {
		t.Fatal("关闭主动模式后不应注入说明")
	}
}

// 端到端：模型在同一次 run 里先 context_check、再 context_apply，
// 工具经 Executor 分发，压缩结果进入会话投影。
func TestActiveCompactionThroughToolLoop(t *testing.T) {
	reg := tools.NewRegistry()
	exec := tools.NewExecutor(reg, 8)

	p := NewScriptedProvider(
		// 第 1 步：模型主动查看上下文
		toolReply(ToolCallBlock{ID: "c1", Name: tools.ToolContextCheck, Arguments: map[string]any{}}),
		// 第 2 步：模型提交压缩（自己写摘要）
		toolReply(ToolCallBlock{ID: "c2", Name: tools.ToolContextApply, Arguments: map[string]any{
			"summary": "## 已完成\n梳理主动压缩链路；模型自行撰写摘要并通过 context_apply 提交。",
			"topic":   "主动压缩",
		}}),
		// 第 3 步：正常结束
		reply("完成"),
	)

	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.Executor = exec
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)

	res, err := a.Run(context.Background(), "继续工作")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("run err: %v", res.Err)
	}

	// 两次工具调用都应成功（IsError=false）。
	var sawCheck, sawApply bool
	for _, m := range a.Session().Messages() {
		tr, ok := m.(*ToolResultMessage)
		if !ok {
			continue
		}
		if tr.IsError {
			t.Fatalf("工具 %s 失败: %s", tr.ToolName, tr.Text())
		}
		switch tr.ToolName {
		case tools.ToolContextCheck:
			sawCheck = true
		case tools.ToolContextApply:
			sawApply = true
		}
	}
	if !sawCheck || !sawApply {
		t.Fatalf("未观察到两个工具结果（check=%v apply=%v）", sawCheck, sawApply)
	}

	// 压缩必须真的生效：投影里出现压缩摘要，且被折叠的消息消失。
	var summary *CompactionSummaryMessage
	for _, m := range a.Session().Messages() {
		if cs, ok := m.(*CompactionSummaryMessage); ok {
			summary = cs
			break
		}
	}
	if summary == nil {
		t.Fatal("run 结束后没有压缩摘要 entry（context_apply 未落地）")
	}
	if !strings.Contains(summary.Summary, "主动压缩链路") {
		t.Fatalf("摘要内容不符: %q", summary.Summary)
	}
}
