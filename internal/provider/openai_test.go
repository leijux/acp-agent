package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

func ptr[T any](v T) *T { return &v }

// sseServer 起一个假 OpenAI 兼容服务端，返回收到的请求体与路径。
func sseServer(t *testing.T, frames []string, status int) (*httptest.Server, *atomic.Value, *atomic.Value) {
	t.Helper()
	var gotBody atomic.Value
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotPath.Store(r.URL.Path)

		if status != 0 && status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &gotPath
}

func frame(data string) string { return "data: " + data + "\n\n" }

// deepseekReasonerFrames 模拟 deepseek-reasoner 的一次工具调用流。
// 关键点：思考走 reasoning_content；usage 用 prompt_cache_hit_tokens。
func deepseekReasonerFrames() []string {
	return []string{
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"我需要"}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"先读文件。"}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"好的，"}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"我来读。"}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		frame(`{"id":"ds-1","model":"deepseek-reasoner","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20,"completion_tokens_details":{"reasoning_tokens":8}}}`),
		"data: [DONE]\n\n",
	}
}

func TestOpenAI_DeepSeekReasonerStream(t *testing.T) {
	srv, gotBody, gotPath := sseServer(t, deepseekReasonerFrames(), 0)

	var text, thinking strings.Builder
	var calls []agent.ToolCallBlock
	sink := &agent.SinkFunc{
		OnText:     func(d string) { text.WriteString(d) },
		OnThinking: func(d string) { thinking.WriteString(d) },
		OnToolCall: func(c agent.ToolCallBlock) { calls = append(calls, c) },
	}

	p := provider.NewDeepSeek(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "sk-test"})
	req := agent.Request{
		SystemPrompt: "你是助手",
		Messages:     []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "读一下 a.go"}}}},
		Tools:        []agent.ToolSpec{{Name: "read", Description: "读文件", Parameters: map[string]any{"type": "object"}}},
		Model:        provider.ResolveModel("deepseek", "deepseek-reasoner"),
		MaxTokens:    8192,
		Temperature:  ptr(0.7), // reasoner 必须丢弃
	}
	msg, err := p.Stream(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// 流式增量
	if got := thinking.String(); got != "我需要先读文件。" {
		t.Errorf("thinking 增量 = %q", got)
	}
	if got := text.String(); got != "好的，我来读。" {
		t.Errorf("text 增量 = %q", got)
	}
	if len(calls) != 1 || calls[0].Name != "read" || calls[0].ID != "call_1" {
		t.Fatalf("ToolCall 回调 = %+v", calls)
	}

	// 终态消息
	if got := msg.Thinking(); got != "我需要先读文件。" {
		t.Errorf("ThinkingBlock = %q", got)
	}
	if got := msg.Text(); got != "好的，我来读。" {
		t.Errorf("TextBlock = %q", got)
	}
	tcs := msg.ToolCalls()
	if len(tcs) != 1 {
		t.Fatalf("ToolCalls = %+v", tcs)
	}
	if tcs[0].Name != "read" || tcs[0].Arguments["path"] != "a.go" || tcs[0].ID != "call_1" {
		t.Errorf("工具调用拼装错误: %+v", tcs[0])
	}
	if msg.StopReason != agent.StopToolUse {
		t.Errorf("StopReason = %q, want toolUse", msg.StopReason)
	}
	if msg.RawStopReason != "tool_calls" {
		t.Errorf("RawStopReason = %q", msg.RawStopReason)
	}
	if msg.ResponseID != "ds-1" || msg.ResponseModel != "deepseek-reasoner" {
		t.Errorf("响应标识 = %q/%q", msg.ResponseID, msg.ResponseModel)
	}

	// usage：DeepSeek 的缓存命中字段要被识别
	if msg.Usage.CacheRead != 80 {
		t.Errorf("Usage.CacheRead = %d, want 80（prompt_cache_hit_tokens）", msg.Usage.CacheRead)
	}
	if msg.Usage.Reasoning != 8 {
		t.Errorf("Usage.Reasoning = %d, want 8", msg.Usage.Reasoning)
	}
	if msg.Usage.TotalTokens != 120 {
		t.Errorf("Usage.TotalTokens = %d", msg.Usage.TotalTokens)
	}
	if msg.Usage.Cost.CacheRead <= 0 {
		t.Errorf("Cost.CacheRead = %v，应为非零（deepseek-reasoner 命中价 0.14/M）", msg.Usage.Cost.CacheRead)
	}
	if msg.Usage.Cost.Total <= 0 {
		t.Errorf("Cost.Total = %v", msg.Usage.Cost.Total)
	}

	// 请求体
	if got := gotPath.Load().(string); got != "/chat/completions" {
		t.Errorf("路径 = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody.Load().(string)), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if _, ok := body["temperature"]; ok {
		t.Error("deepseek-reasoner 不应发送 temperature（会被 400 拒绝）")
	}
	if body["model"] != "deepseek-reasoner" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v", body["stream"])
	}
	if _, ok := body["stream_options"]; !ok {
		t.Error("应带 stream_options.include_usage 以拿到 usage")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d, want 2（system+user）", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Errorf("首条应为 system: %v", first)
	}
	if _, ok := body["tools"]; !ok {
		t.Error("应带 tools")
	}
}

func TestOpenAI_DeepSeekChatSendsTemperature(t *testing.T) {
	frames := []string{
		frame(`{"id":"ds-2","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
		frame(`{"id":"ds-2","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)

	p := provider.NewDeepSeek(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})
	req := agent.Request{
		Messages:    []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:       provider.ResolveModel("deepseek", "deepseek-chat"),
		MaxTokens:   1000,
		Temperature: ptr(0.3),
	}
	msg, err := p.Stream(context.Background(), req, agent.NoopSink{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if msg.StopReason != agent.StopEnd {
		t.Errorf("StopReason = %q", msg.StopReason)
	}
	var body map[string]any
	json.Unmarshal([]byte(gotBody.Load().(string)), &body)
	if body["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", body["temperature"])
	}
}

func TestOpenAI_ToolResultConversion(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "读文件"}}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.TextBlock{Text: "我来读"},
			agent.ToolCallBlock{ID: "call_9", Name: "read", Arguments: map[string]any{"path": "x.go"}},
		}},
		&agent.ToolResultMessage{ToolCallID: "call_9", ToolName: "read", Content: []agent.Block{agent.TextBlock{Text: "package x"}}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("openai", "gpt-4.1"), MaxTokens: 100}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody.Load().(string)), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(body.Messages))
	}
	asst := body.Messages[1]
	if asst["role"] != "assistant" {
		t.Fatalf("第 2 条 role = %v", asst["role"])
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", asst["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("tool name = %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "x.go") {
		t.Errorf("tool arguments = %v", fn["arguments"])
	}
	tr := body.Messages[2]
	if tr["role"] != "tool" || tr["tool_call_id"] != "call_9" {
		t.Errorf("tool result 转换错误: %v", tr)
	}
	if tr["content"] != "package x" {
		t.Errorf("tool result content = %v", tr["content"])
	}
}

func TestRetry_ReattemptsOn429(t *testing.T) {
	var hits atomic.Int32
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprint(w, f)
		}
	}))
	defer srv.Close()

	base := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL})
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 3, BaseDelay: 1})

	req := agent.Request{
		Messages: []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:    provider.ResolveModel("openai", "gpt-4.1"),
	}
	msg, err := p.Stream(context.Background(), req, agent.NoopSink{})
	if err != nil {
		t.Fatalf("重试后仍失败: %v", err)
	}
	if msg.Text() != "ok" {
		t.Errorf("text = %q", msg.Text())
	}
	if hits.Load() != 2 {
		t.Errorf("请求次数 = %d, want 2", hits.Load())
	}
}

func TestRetry_NoRetryAfterPartialOutput(t *testing.T) {
	var hits atomic.Int32
	// 先发一个增量，然后直接断开。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, frame(`{"choices":[{"index":0,"delta":{"content":"半截"}}]}`))
		w.(http.Flusher).Flush()
		// 不发终止帧就断开
	}))
	defer srv.Close()

	base := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL})
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 3, BaseDelay: 1})

	var text strings.Builder
	req := agent.Request{
		Messages: []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:    provider.ResolveModel("openai", "gpt-4.1"),
	}
	msg, err := p.Stream(context.Background(), req, &agent.SinkFunc{OnText: func(d string) { text.WriteString(d) }})
	if err == nil {
		t.Fatal("截断的流应返回错误")
	}
	// 已吐出的增量不能重放
	if text.String() != "半截" {
		t.Errorf("增量被重复: %q", text.String())
	}
	if hits.Load() != 1 {
		t.Errorf("已产生增量后不应重试，实际请求 %d 次", hits.Load())
	}
	if msg == nil || msg.StopReason != agent.StopError {
		t.Errorf("部分消息的 StopReason 应为 error: %+v", msg)
	}
}

func TestSupportsSampling(t *testing.T) {
	cases := map[string]bool{
		"deepseek-chat":              true,
		"deepseek-reasoner":          false,
		"deepseek/deepseek-reasoner": false,
		"o3":                         false,
		"o4-mini":                    false,
		"o1-mini":                    false,
		"gpt-5":                      false,
		"gpt-5-mini":                 false,
		"gpt-4.1":                    true,
		"claude-sonnet-4-5":          true,
		"qwen3-coder":                true,
	}
	for id, want := range cases {
		if got := provider.SupportsSampling(id); got != want {
			t.Errorf("SupportsSampling(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestLookupModel_PrefixMatch(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"gpt-4.1-2025-04-14", "gpt-4.1"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"},
		{"deepseek-reasoner", "deepseek-reasoner"},
		{"unknown-model", ""},
	}
	for _, c := range cases {
		info, ok := provider.LookupModel(c.id)
		if c.want == "" {
			if ok {
				t.Errorf("LookupModel(%q) 不该命中，得到 %+v", c.id, info)
			}
			continue
		}
		if !ok {
			t.Errorf("LookupModel(%q) 未命中", c.id)
			continue
		}
		if info.ID != c.want {
			t.Errorf("LookupModel(%q) = %q, want %q", c.id, info.ID, c.want)
		}
	}
}

func TestResolveModel_DeepSeekDefaults(t *testing.T) {
	m := provider.ResolveModel("deepseek", "deepseek-flash")
	if m.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000", m.ContextWindow)
	}
	if m.MaxOutputTokens != 384_000 {
		t.Errorf("deepseek-flash 输出上限 = %d, want 384000", m.MaxOutputTokens)
	}
	if !m.SupportsThinking {
		t.Error("deepseek-flash 支持思考模式")
	}

	pro := provider.ResolveModel("deepseek", "deepseek-v4-pro")
	if pro.ContextWindow != 1_000_000 || pro.MaxOutputTokens != 384_000 {
		t.Errorf("v4-pro = %d/%d", pro.ContextWindow, pro.MaxOutputTokens)
	}
	if pro.SupportsImages {
		t.Error("deepseek-v4-pro 不支持图像理解")
	}

	ep, ok := provider.LookupEndpoint("deepseek")
	if !ok {
		t.Fatal("缺少 deepseek 端点预设")
	}
	if ep.BaseURL != "https://api.deepseek.com" || ep.EnvKey != "DEEPSEEK_API_KEY" {
		t.Errorf("端点预设 = %+v", ep)
	}
	if got := ep.ModelFor(""); got.ID != "deepseek-flash" {
		t.Errorf("默认模型 = %q, want deepseek-flash", got.ID)
	}
	// 旧模型名仍可解析，供历史会话/账单回算。
	if got := ep.ModelFor("deepseek-reasoner"); got.MaxOutputTokens != 65_536 {
		t.Errorf("reasoner 输出上限 = %d, want 65536", got.MaxOutputTokens)
	}
}

func TestApplyCost_DeepSeekPeakAndOffPeak(t *testing.T) {
	restore := provider.SetNowFuncForTest(time.Now)
	defer restore()

	// 北京时间周二 10:00 = 高峰。
	provider.SetNowFuncForTest(func() time.Time {
		return time.Date(2026, 3, 10, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	})
	m := &agent.AssistantMessage{}
	m.Usage = agent.Usage{Input: 1_000_000, Output: 1_000_000}
	provider.ApplyCost(m, provider.ResolveModel("deepseek", "deepseek-flash"))
	if m.Usage.Cost.Currency != "CNY" {
		t.Errorf("Currency = %q, want CNY", m.Usage.Cost.Currency)
	}
	// 高峰：输入 2 元/M + 输出 8 元/M = 10。
	if !nearly(m.Usage.Cost.Total, 10) {
		t.Errorf("高峰成本 = %v, want 10", m.Usage.Cost.Total)
	}

	// 北京时间周二 20:00 = 空闲（半价）：1 + 4 = 5。
	provider.SetNowFuncForTest(func() time.Time {
		return time.Date(2026, 3, 10, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	})
	m2 := &agent.AssistantMessage{}
	m2.Usage = agent.Usage{Input: 1_000_000, Output: 1_000_000}
	provider.ApplyCost(m2, provider.ResolveModel("deepseek", "deepseek-flash"))
	if !nearly(m2.Usage.Cost.Total, 5) {
		t.Errorf("空闲成本 = %v, want 5", m2.Usage.Cost.Total)
	}

	// 周末全天空闲：输出 4 元/M。
	provider.SetNowFuncForTest(func() time.Time {
		return time.Date(2026, 3, 14, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)) // 周六
	})
	m3 := &agent.AssistantMessage{}
	m3.Usage = agent.Usage{Output: 1_000_000}
	provider.ApplyCost(m3, provider.ResolveModel("deepseek", "deepseek-flash"))
	if !nearly(m3.Usage.Cost.Total, 4) {
		t.Errorf("周末输出成本 = %v, want 4", m3.Usage.Cost.Total)
	}

	// 午休 12:30 属于空闲时段。
	provider.SetNowFuncForTest(func() time.Time {
		return time.Date(2026, 3, 10, 12, 30, 0, 0, time.FixedZone("CST", 8*3600))
	})
	m4 := &agent.AssistantMessage{}
	m4.Usage = agent.Usage{Input: 1_000_000}
	provider.ApplyCost(m4, provider.ResolveModel("deepseek", "deepseek-flash"))
	if !nearly(m4.Usage.Cost.Total, 1) {
		t.Errorf("午休输入成本 = %v, want 1", m4.Usage.Cost.Total)
	}

	// 法定节假日全天空闲。
	restoreHolidays := provider.SetDeepSeekHolidaysForTest(map[string]bool{"2026-03-10": true})
	defer restoreHolidays()
	provider.SetNowFuncForTest(func() time.Time {
		return time.Date(2026, 3, 10, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	})
	m5 := &agent.AssistantMessage{}
	m5.Usage = agent.Usage{Input: 1_000_000}
	provider.ApplyCost(m5, provider.ResolveModel("deepseek", "deepseek-flash"))
	if !nearly(m5.Usage.Cost.Total, 1) {
		t.Errorf("节假日输入成本 = %v, want 1", m5.Usage.Cost.Total)
	}
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestThinkingParams(t *testing.T) {
	cases := map[string]string{
		"low":     "low",
		"minimal": "low",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "high",
		"max":     "max",
	}
	for level, want := range cases {
		got := provider.ThinkingParams(level)
		if got["reasoning_effort"] != want {
			t.Errorf("ThinkingParams(%q).effort = %v, want %q", level, got["reasoning_effort"], want)
		}
		think, _ := got["thinking"].(map[string]any)
		if think["type"] != "enabled" {
			t.Errorf("ThinkingParams(%q).thinking = %v", level, think)
		}
	}
	if got := provider.ThinkingParams("off"); got["reasoning_effort"] != nil {
		t.Errorf("off 不该带 effort: %v", got)
	}
	if got := provider.ThinkingParams(""); got != nil {
		t.Errorf("空等级应交由服务端默认: %v", got)
	}
}

// DeepSeek 带 tools 时必须回传 reasoning_content，否则真实 API 会 400。
func TestOpenAIMessages_ReplaysReasoningWithTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		var req struct {
			Messages []map[string]any `json:"messages"`
			Tools    []any            `json:"tools"`
			Thinking map[string]any   `json:"thinking"`
			Effort   string           `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求不是合法 JSON: %v", err)
			return
		}
		var foundReasoning bool
		for _, m := range req.Messages {
			if m["role"] == "assistant" {
				if rc, ok := m["reasoning_content"].(string); ok && rc == "先读文件" {
					foundReasoning = true
				}
			}
		}
		if !foundReasoning {
			t.Errorf("带 tools 的请求缺少 reasoning_content: %s", body)
		}
		if req.Thinking["type"] != "enabled" {
			t.Errorf("thinking 参数 = %v", req.Thinking)
		}
		if req.Effort != "high" {
			t.Errorf("reasoning_effort = %q", req.Effort)
		}
	}))
	defer srv.Close()
	prov := provider.NewDeepSeek(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	sess := agent.NewSession()
	sess.Append(&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "读文件"}}})
	sess.Append(&agent.AssistantMessage{Content: []agent.Block{
		agent.ThinkingBlock{Thinking: "先读文件"},
		agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a"}},
	}})
	sess.Append(&agent.ToolResultMessage{ToolCallID: "c1", Content: []agent.Block{agent.TextBlock{Text: "x"}}})

	req := agent.Request{
		Model:         provider.ResolveModel("deepseek", "deepseek-flash"),
		Messages:      sess.Messages(),
		Tools:         []agent.ToolSpec{{Name: "read", Description: "read"}},
		ThinkingLevel: "high",
	}
	if _, err := prov.Stream(context.Background(), req, &agent.SinkFunc{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}
