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
// MaxOutputTokens，等于让输入天花板随输出上限一起塌陷。
func TestBuildRequestClampsMaxTokens(t *testing.T) {
	used := 0
	a := newTestAgent(t, NewScriptedProvider(), func(o *Options) {
		o.Model = Model{Provider: "test", ID: "big-out", ContextWindow: 1_048_576, MaxOutputTokens: 384_000}
		o.ContextWindow = 1_048_576
		o.TokenCounter = func([]Message) int { return used }
	})
	window := a.opts.ContextWindow
	sysTokens := EstimateTokens(BuildSystemPrompt(a.opts.Context))
	reserve := a.opts.ReserveTokens // 1M 窗口下不会被 win/8 截断

	// 空上下文：输出直接给模型声明的上限。上下文还空着时 full 输出完全合法，
	// 拿「窗口 1/4」当天花板只会把长生成白白截短。
	if got := a.buildRequest().MaxTokens; got != 384_000 {
		t.Fatalf("空会话 max_tokens = %d, want 384000（模型声明上限）", got)
	}

	// 输入占 700k：可用输出 = 窗口 − 输入×保守系数 − 预留。
	used = 700_000
	req := a.buildRequest()
	want := window - (used+sysTokens)*inputSafetyPct/100 - reserve
	if req.MaxTokens != want {
		t.Fatalf("max_tokens = %d, want %d（应被剩余空间收敛）", req.MaxTokens, want)
	}
	// 上游按 messages + completion 求和判窗口：这两个数必须放得下。
	if used+sysTokens+req.MaxTokens > window {
		t.Fatalf("输入 %d + 输出 %d 超过窗口 %d，上游必然 400", used+sysTokens, req.MaxTokens, window)
	}

	// 输出随输入单调收缩（不能出现「输入变多输出反而变大」）。
	prev := req.MaxTokens
	used = 800_000
	if got := a.buildRequest().MaxTokens; got > prev {
		t.Fatalf("max_tokens 反弹：used=800k 得到 %d > used=700k 的 %d", got, prev)
	}

	// 输入已经吃满：落到下限，而不是把剩余空间算成负数。
	used = 1_000_000
	if got := a.buildRequest().MaxTokens; got != a.minOutput() {
		t.Fatalf("max_tokens = %d, want %d（下限）", got, a.minOutput())
	}
}

// 小窗口不能被「绝对预留 + 绝对下限」钉死。
//
// 旧实现 reserve 恒 16384、下限恒 512：8k 窗口从第一轮起 max_tokens 就是 512
// （模型声明是 2048），1024 窗口则 512 > 窗口的一半，任何输入都超窗。
func TestBuildRequestSmallWindow(t *testing.T) {
	cases := []struct {
		name    string
		window  int
		maxOut  int
		wantMin int
	}{
		{name: "8k 窗口", window: 8_192, maxOut: 2_048, wantMin: 1_024},
		{name: "1k 窗口", window: 1_024, maxOut: 512, wantMin: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newTestAgent(t, NewScriptedProvider(), func(o *Options) {
				o.Model = Model{Provider: "test", ID: "small", ContextWindow: c.window, MaxOutputTokens: c.maxOut}
				o.ContextWindow = c.window
			})
			got := a.buildRequest().MaxTokens
			if got < c.wantMin {
				t.Fatalf("空会话 max_tokens = %d, want >= %d（被绝对预留/下限坑了）", got, c.wantMin)
			}
			if got > c.window {
				t.Fatalf("max_tokens = %d 超过窗口 %d", got, c.window)
			}
			// 真塞满历史时，输出必须收到剩余空间以内。
			used := 0
			a.opts.TokenCounter = func([]Message) int { return used }
			used = c.window - 4*a.minOutput()
			if req := a.buildRequest(); used+req.MaxTokens > c.window {
				t.Fatalf("输入 %d + 输出 %d > 窗口 %d", used, req.MaxTokens, c.window)
			}
			// 估算顶满时只能落到「随窗口缩放的下限」，而且不能超过窗口本身。
			used = c.window
			if got := a.buildRequest().MaxTokens; got > c.window {
				t.Fatalf("顶满时 max_tokens = %d 超过窗口 %d", got, c.window)
			}
		})
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

// 压缩水位必须与 max_tokens 用同一个式子：不允许「输出已经跌到下限、压缩
// 还没触发」，也不允许「离危险线还很远就压」。
func TestShouldCompactTracksOutputSqueeze(t *testing.T) {
	used := 0
	a := newTestAgent(t, NewScriptedProvider(reply("x")), func(o *Options) {
		o.Model = Model{Provider: "test", ID: "mid", ContextWindow: 128_000, MaxOutputTokens: 8_192}
		o.ContextWindow = 128_000
		o.TokenCounter = func([]Message) int { return used }
	})
	preferred := a.preferredOutputTokens()
	if preferred != 8_192 {
		t.Fatalf("preferredOutputTokens = %d, want 8192（模型声明上限）", preferred)
	}

	// 逐格扫：只要没触发压缩，输出预算就必须仍然 >= 理想值。
	for u := 0; u < 120_000; u += 2_000 {
		used = u
		if a.shouldCompact() {
			// 触发时立刻复核：此刻输出预算不应还很大（否则压得太早），
			// 也不能已经长期跌到下限（那是压得太晚）。
			if a.outputAllowance(a.contextTokens()) > preferred {
				t.Fatalf("used=%d 触发压缩，但输出预算仍 %.0f > %d（压得太早）",
					u, float64(a.outputAllowance(a.contextTokens())), preferred)
			}
			if u == 0 {
				t.Fatal("空上下文不该触发压缩")
			}
			// 触发点必须早于输出真的掉到下限：否则模型会在 512 上限下工作。
			if a.outputAllowance(a.contextTokens()) <= a.minOutput() {
				t.Fatalf("used=%d 触发压缩时输出已跌破下限 %d", u, a.minOutput())
			}
			return
		}
		if a.outputAllowance(a.contextTokens()) < preferred {
			t.Fatalf("used=%d 输出预算已低于理想值 %d 却没触发压缩", u, preferred)
		}
	}
	t.Fatal("扫完 120k 都没触发压缩")
}

// 上下文超限后必须「压缩 + 用更小的 max_tokens 重试一次」，而不是把 400
// 落进历史（那条错误消息本身又把上下文撑大一点，下一条用户消息必然复现）。
func TestRunRecoversFromContextOverflow(t *testing.T) {
	sends := 0
	maxTokens := []int{}
	p := &stubProvider{fn: func(req Request) (*AssistantMessage, error) {
		if req.Purpose == PurposeSummarize {
			return reply("## Goal\n旧历史已压缩为摘要。"), nil
		}
		sends++
		maxTokens = append(maxTokens, req.MaxTokens)
		if sends == 1 {
			return nil, &overflowErr{"HTTP 400: maximum context length is 1048576 tokens"}
		}
		return reply("重试成功"), nil
	}}
	a := newTestAgent(t, p, func(o *Options) {
		o.ContextWindow = 40_000
		o.Model = Model{Provider: "test", ID: "m", ContextWindow: 40_000, MaxOutputTokens: 4_000}
	})
	fillContext(t, a, 40)

	res, err := a.Run(context.Background(), "继续")
	if err != nil {
		t.Fatalf("超限后应压缩重试成功，实际 err = %v", err)
	}
	if sends != 2 {
		t.Fatalf("provider 被调用 %d 次，want 2（一次超限 + 一次重试）", sends)
	}
	if len(maxTokens) == 2 && maxTokens[1] >= maxTokens[0] {
		t.Fatalf("重试的 max_tokens（%d）应小于首次（%d）", maxTokens[1], maxTokens[0])
	}
	if last := res.LastAssistant(); last == nil || last.Text() != "重试成功" {
		t.Fatalf("最后一次回复 = %+v", last)
	}
	var users int
	for _, m := range a.Session().Messages() {
		if am, ok := m.(*AssistantMessage); ok && am.StopReason == StopError {
			t.Fatalf("400 不应落成历史消息: %q", am.Text())
		}
		if um, ok := m.(*UserMessage); ok && TextOf(um) == "继续" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("用户消息出现 %d 次，want 1", users)
	}
}

// 重试仍然超限：只重试一次、不循环，错误照常记录。
func TestRunOverflowRetryGivesUp(t *testing.T) {
	sends := 0
	p := &stubProvider{fn: func(req Request) (*AssistantMessage, error) {
		if req.Purpose == PurposeSummarize {
			return reply("## Goal\n旧历史已压缩为摘要。"), nil
		}
		sends++
		return nil, &overflowErr{"HTTP 400: maximum context length is 1048576 tokens"}
	}}
	a := newTestAgent(t, p, func(o *Options) { o.ContextWindow = 40_000 })
	fillContext(t, a, 40)

	res, err := a.Run(context.Background(), "继续")
	if err == nil {
		t.Fatal("一直超限时 Run 应返回错误")
	}
	if sends != 2 {
		t.Fatalf("provider 被调用 %d 次，want 2（只重试一次）", sends)
	}
	if res.Err == nil {
		t.Fatal("res.Err 应为原始 400")
	}
	var errs int
	for _, m := range a.Session().Messages() {
		if am, ok := m.(*AssistantMessage); ok && am.StopReason == StopError {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("error 消息 %d 条，want 1", errs)
	}
}

// DisableAutoCompact 时不得偷偷压缩（ACP 客户端会显式关掉自动压缩）。
func TestRunOverflowRespectsDisableAutoCompact(t *testing.T) {
	summaries := 0
	p := &stubProvider{fn: func(req Request) (*AssistantMessage, error) {
		if req.Purpose == PurposeSummarize {
			summaries++
			return reply("## Goal\n摘要"), nil
		}
		return nil, &overflowErr{"HTTP 400: maximum context length is 1048576 tokens"}
	}}
	a := newTestAgent(t, p, func(o *Options) {
		o.ContextWindow = 40_000
		o.DisableAutoCompact = true
	})
	fillContext(t, a, 40)

	if _, err := a.Run(context.Background(), "继续"); err == nil {
		t.Fatal("应返回原始 400")
	}
	if summaries != 0 {
		t.Fatalf("关掉自动压缩后仍调用了 %d 次摘要", summaries)
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
