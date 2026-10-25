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
