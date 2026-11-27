package provider_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

// TestOpenAI_ImageBlockBecomesImageURL ImageBlock 必须转成 OpenAI 的
// image_url 内容块（data URL 形式），而不是被丢掉或降级成文本。
func TestOpenAI_ImageBlockBecomesImageURL(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"看到了"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	const b64 = "iVBORw0KGgoAAAANSUhEUg=="
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{
			agent.TextBlock{Text: "这张图是什么？"},
			agent.ImageBlock{Data: b64, MimeType: "image/png"},
		}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("openai", "gpt-4.1"), MaxTokens: 50}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	raw := gotBody.Load().(string)
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %d，请求体：%s", len(body.Messages), raw)
	}
	parts, ok := body.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("content 不是数组（说明图片被降级成纯文本）：%v", body.Messages[0].Content)
	}

	var sawText, sawImage bool
	for _, part := range parts {
		m, _ := part.(map[string]any)
		switch m["type"] {
		case "text":
			if s, _ := m["text"].(string); strings.Contains(s, "这张图") {
				sawText = true
			}
		case "image_url":
			iu, _ := m["image_url"].(map[string]any)
			url, _ := iu["url"].(string)
			if !strings.HasPrefix(url, "data:image/png;base64,") {
				t.Fatalf("image_url.url 不是 data URL：%q", url)
			}
			if !strings.HasSuffix(url, b64) {
				t.Fatalf("image_url.url 没带上 base64 载荷：%q", url)
			}
			sawImage = true
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("text=%v image=%v，请求体：%s", sawText, sawImage, raw)
	}
}

// TestOpenAI_ImageOnlyMessage 只有图片没有文字时，content 仍需是合法数组。
func TestOpenAI_ImageOnlyMessage(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{
			agent.ImageBlock{Data: "AAAA", MimeType: "image/jpeg"},
		}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("openai", "gpt-4.1"), MaxTokens: 50}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var body map[string]any
	raw := gotBody.Load().(string)
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	msg := body["messages"].([]any)[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %v，请求体：%s", msg["content"], raw)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatalf("唯一的内容块应为 image_url，实际：%v", part)
	}
}

// TestOpenAI_DeepSeekToolImageInline 端到端：DeepSeek 的 tool 消息可以直接带图。
// 文档依据 https://api-docs.deepseek.com/zh-cn/guides/vision/ —— 「图片可以出现在
// user、system 和 tool 消息中」。这里断言真实请求体里 tool content 是 block 数组。
func TestOpenAI_DeepSeekToolImageInline(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"看到图了"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewDeepSeek(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	const b64 = "iVBORw0KGgoAAAANSUhEUg=="
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "看看这张截图"}}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.png"}},
		}},
		&agent.ToolResultMessage{ToolCallID: "c1", ToolName: "read",
			Content: []agent.Block{agent.ImageBlock{Data: b64, MimeType: "image/png"}}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("deepseek", "deepseek-flash"), MaxTokens: 50}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	raw := gotBody.Load().(string)
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d，期望 3 条（图片内联，不额外后置），请求体：%s", len(body.Messages), raw)
	}
	tool := body.Messages[2]
	if tool.Role != "tool" {
		t.Fatalf("第 3 条 role = %q", tool.Role)
	}
	parts, ok := tool.Content.([]any)
	if !ok {
		t.Fatalf("tool content 应为数组（内联图片），实际 %#v，请求体：%s", tool.Content, raw)
	}
	seen := false
	for _, part := range parts {
		m, _ := part.(map[string]any)
		if m["type"] != "image_url" {
			continue
		}
		iu, _ := m["image_url"].(map[string]any)
		if url, _ := iu["url"].(string); url != "data:image/png;base64,"+b64 {
			t.Fatalf("image_url.url = %q", url)
		}
		seen = true
	}
	if !seen {
		t.Fatalf("tool content 里没有图片：%s", raw)
	}
}

// TestOpenAI_GenericToolImageDeferred 端到端：通用 OpenAI 兼容端点不认 parts，
// 图片必须作为紧随其后的 user 消息发出，且 tool content 保持字符串。
func TestOpenAI_GenericToolImageDeferred(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "看图"}}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.png"}},
		}},
		&agent.ToolResultMessage{ToolCallID: "c1", ToolName: "read",
			Content: []agent.Block{agent.ImageBlock{Data: "AAAA", MimeType: "image/png"}}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("openai", "gpt-4.1"), MaxTokens: 50}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	raw := gotBody.Load().(string)
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d，期望 4 条（tool 后追加 user），请求体：%s", len(body.Messages), raw)
	}
	if _, ok := body.Messages[2].Content.(string); !ok {
		t.Fatalf("tool content 必须是字符串：%#v", body.Messages[2].Content)
	}
	if body.Messages[3].Role != "user" {
		t.Fatalf("第 4 条 role = %q，期望 user", body.Messages[3].Role)
	}
	parts, ok := body.Messages[3].Content.([]any)
	if !ok || len(parts) < 2 {
		t.Fatalf("后置 user 消息应含说明 + 图片，实际 %#v", body.Messages[3].Content)
	}
}

// TestOpenAI_VisionlessModelDropsToolImage 端到端：模型没视觉能力时不发 image_url。
// deepseek-v4-pro 在 models.go 里没有 SupportsImages —— 发过去必然 400。
func TestOpenAI_VisionlessModelDropsToolImage(t *testing.T) {
	frames := []string{
		frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`),
		"data: [DONE]\n\n",
	}
	srv, gotBody, _ := sseServer(t, frames, 0)
	p := provider.NewDeepSeek(provider.OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})

	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "看图"}}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.png"}},
		}},
		&agent.ToolResultMessage{ToolCallID: "c1", ToolName: "read",
			Content: []agent.Block{agent.ImageBlock{Data: "AAAA", MimeType: "image/png"}}},
	}
	req := agent.Request{Messages: msgs, Model: provider.ResolveModel("deepseek", "deepseek-v4-pro"), MaxTokens: 50}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := gotBody.Load().(string)
	if strings.Contains(raw, "image_url") {
		t.Fatalf("无视觉能力的模型不该收到 image_url：%s", raw)
	}
	if !strings.Contains(raw, "不支持图片输入") {
		t.Fatalf("应留下降级说明：%s", raw)
	}
}
