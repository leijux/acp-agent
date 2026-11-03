package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// stubProvider 是无限次可用的 Provider（ScriptedProvider 的响应序列会被消费完，
// 而压缩重试路径需要对同一个 provider 连发多次请求）。
type stubProvider struct{ fn func(req Request) (*AssistantMessage, error) }

func (p *stubProvider) Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error) {
	return p.fn(req)
}

// overflowErr 模拟 provider 的「上下文超限」错误（实现 OverflowDetector）。
type overflowErr struct{ msg string }

func (e *overflowErr) Error() string         { return e.msg }
func (e *overflowErr) ContextOverflow() bool { return true }

// 上下文超限的根因回归：max_tokens 必须收敛到「窗口 − 估算输入 − 预留」。
//
// 线上事故：667184(消息) + 384000(声明输出上限) > 1048576(真实窗口) →
// HTTP 400，而那时界面上的占用才 50%。旧实现把 MaxTokens 恒等于模型声明的
// MaxOutputTokens，于是输入天花板被悄悄压到 63%。
func TestBuildRequestClampsMaxTokens(t *testing.T) {
	used := 0
	a := newTestAgent(t, NewScriptedProvider(), func(o *Options) {
		o.Model = Model{Provider: "test", ID: "big-out", ContextWindow: 1_048_576, MaxOutputTokens: 384_000}
		o.ContextWindow = 1_048_576
		o.TokenCounter = func([]Message) int { return used }
	})
	sysTokens := EstimateTokens(BuildSystemPrompt(a.opts.Context))
	window := a.opts.ContextWindow

	// 空会话：输出拿「窗口 1/4」这个理想值，而不是模型声明的 384k ——
	// 若拿 384k，输入天花板只剩 63%，压缩水位（90%）永远够不到。
	if got, want := a.buildRequest().MaxTokens, window/4; got != want {
		t.Fatalf("空会话 max_tokens = %d, want %d", got, want)
	}

	// 输入占 700k：可用输出 = 窗口 − 输入×保守系数 − 预留。
	used = 700_000
	req := a.buildRequest()
	wantClamped := window - (used+sysTokens)*inputSafetyPct/100 - a.opts.ReserveTokens
	if req.MaxTokens != wantClamped {
		t.Fatalf("max_tokens = %d, want %d（应被剩余空间收敛）", req.MaxTokens, wantClamped)
	}
	// 上游按 messages + completion 求和判窗口：这两个数加起来必须放得下。
	if used+sysTokens+req.MaxTokens+a.opts.ReserveTokens > window {
		t.Fatalf("输入 %d + 输出 %d + 预留 %d 超过窗口 %d，上游必然 400",
			used, req.MaxTokens, a.opts.ReserveTokens, window)
	}

	// 输入已经吃满：不再把剩余空间算成负数，落到下限。
	used = 1_000_000
	if got := a.buildRequest().MaxTokens; got != minOutputTokens {
		t.Fatalf("max_tokens = %d, want %d（下限）", got, minOutputTokens)
	}
}

// 工具定义与图片都必须计入上下文估算。
//
// 旧实现只算消息文本 + system prompt：工具 schema 一次上万 token、图片被当 0，
// 这是「界面显示还有一半、真实请求已经超限」的另一个来源。
func TestContextTokensCountsToolsAndImages(t *testing.T) {
	plain := newTestAgent(t, NewScriptedProvider())
	plainTokens := plain.ContextTokens()

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewReadTool(tools.Policy{})); err != nil {
		t.Fatalf("注册工具: %v", err)
	}
	tooled := newTestAgent(t, NewScriptedProvider(), func(o *Options) {
		o.Registry = reg
		o.DisableContextTools = true // 只看工具定义，避免 system prompt 追加说明干扰
	})
	if tooled.ContextTokens() <= plainTokens {
		t.Fatalf("工具定义没有计入估算：%d <= %d", tooled.ContextTokens(), plainTokens)
	}

	// 图片不能再算 0 token。
	img := &UserMessage{Content: []Block{ImageBlock{Data: "AAAA", MimeType: "image/png"}}}
	if got := EstimateMessagesTokens([]Message{img}); got < imageTokens {
		t.Fatalf("图片消息估算 = %d, want >= %d", got, imageTokens)
	}
}

// 输入预算水位：占用还没到 90%，但再长就只能靠压缩 max_tokens 换空间时必须先压。
func TestShouldCompactOnInputBudget(t *testing.T) {
	used := 0
	a := newTestAgent(t, NewScriptedProvider(reply("x")), func(o *Options) {
		o.Model = Model{Provider: "test", ID: "big-out", ContextWindow: 40_000, MaxOutputTokens: 20_000}
		o.ContextWindow = 40_000
		o.ReserveTokens = 1_000
		o.TokenCounter = func([]Message) int { return used }
	})
	// idealOutput = min(20000, 40000/4) = 10000；预算 = 40000−1000−10000 = 29000（72.5%）。
	if got, want := a.idealOutputTokens(), 10_000; got != want {
		t.Fatalf("idealOutputTokens = %d, want %d", got, want)
	}
	if got, want := a.inputBudget(), 29_000; got != want {
		t.Fatalf("inputBudget = %d, want %d", got, want)
	}

	used = 20_000 // 还有空间，不该压
	if a.shouldCompact() {
		t.Fatalf("占用 %.0f%% 不该触发压缩", a.contextUsage()*100)
	}
	used = 33_000 // 低于 90% 硬水位，但已越过输入预算
	if a.contextUsage() >= HardCompactUsage {
		t.Fatalf("前置条件不成立：占用已到硬水位 %.0f%%", a.contextUsage()*100)
	}
	if !a.shouldCompact() {
		t.Fatalf("占用 %.0f%% 应触发压缩（inputBudget=%d）", a.contextUsage()*100, a.inputBudget())
	}
}

// 上下文超限后必须「压缩 + 重试一次」，而不是把 400 落进历史。
//
// 落进历史的后果是死锁：那条错误消息本身又把上下文撑大一点，下一条用户消息
// 必然复现同一个 400，只能手动 /compact 才能救。
func TestRunRecoversFromContextOverflow(t *testing.T) {
	sends := 0
	p := &stubProvider{fn: func(req Request) (*AssistantMessage, error) {
		if req.Purpose == PurposeSummarize {
			return reply("## Goal\n旧历史已压缩为摘要。"), nil
		}
		sends++
		if sends == 1 {
			return nil, &overflowErr{"HTTP 400: maximum context length is 1048576 tokens"}
		}
		return reply("重试成功"), nil
	}}
	a := newTestAgent(t, p, func(o *Options) { o.ContextWindow = 40_000 })
	fillContext(t, a, 40)

	res, err := a.Run(context.Background(), "继续")
	if err != nil {
		t.Fatalf("超限后应压缩重试成功，实际 err = %v", err)
	}
	if sends != 2 {
		t.Fatalf("provider 被调用 %d 次，want 2（一次超限 + 一次重试）", sends)
	}
	if last := res.LastAssistant(); last == nil || last.Text() != "重试成功" {
		t.Fatalf("最后一次回复 = %+v", last)
	}
	for _, m := range a.Session().Messages() {
		if am, ok := m.(*AssistantMessage); ok && am.StopReason == StopError {
			t.Fatalf("400 不应落成历史消息: %q", am.Text())
		}
	}
}

// 非超限错误仍然照旧记录，且不占用「压缩重试」的机会。
func TestRunRecordsNonOverflowError(t *testing.T) {
	p := &stubProvider{fn: func(req Request) (*AssistantMessage, error) {
		return nil, &plainErr{"HTTP 500: boom"}
	}}
	a := newTestAgent(t, p)
	res, err := a.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if res == nil || len(res.Messages) == 0 {
		t.Fatal("应记录一条 error 状态的 assistant 消息")
	}
	last, ok := res.Messages[len(res.Messages)-1].(*AssistantMessage)
	if !ok || last.StopReason != StopError {
		t.Fatalf("最后一条 = %#v", res.Messages[len(res.Messages)-1])
	}
	if !strings.Contains(last.Text(), "boom") {
		t.Fatalf("错误原文应保留: %q", last.Text())
	}
}

type plainErr struct{ msg string }

func (e *plainErr) Error() string { return e.msg }
