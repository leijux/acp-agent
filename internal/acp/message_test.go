package acp

import (
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
)

// TestToAgentMessageTextOnly 纯文本仍合成单条 TextBlock，行为与旧版一致。
func TestToAgentMessageTextOnly(t *testing.T) {
	msg, err := toAgentMessage([]ContentBlock{
		{Type: "text", Text: "第一行"},
		{Type: "text", Text: "第二行"},
	})
	if err != nil {
		t.Fatal(err)
	}
	um, ok := msg.(*agent.UserMessage)
	if !ok {
		t.Fatalf("消息类型 %T", msg)
	}
	if len(um.Content) != 1 {
		t.Fatalf("blocks = %d，期望合并成 1 个", len(um.Content))
	}
	tb, ok := um.Content[0].(agent.TextBlock)
	if !ok || tb.Text != "第一行\n第二行" {
		t.Fatalf("文本块 = %#v", um.Content[0])
	}
}

// TestToAgentMessageImagePassthrough 图片必须原样保留为 ImageBlock。
func TestToAgentMessageImagePassthrough(t *testing.T) {
	const b64 = "iVBORw0KGgoAAAANSUhEUg=="
	msg, err := toAgentMessage([]ContentBlock{
		{Type: "text", Text: "看图"},
		{Type: "image", Data: b64, MIMEType: "image/png"},
		{Type: "text", Text: "这是什么？"},
	})
	if err != nil {
		t.Fatal(err)
	}
	um := msg.(*agent.UserMessage)
	if len(um.Content) != 3 {
		t.Fatalf("blocks = %d，期望 [text, image, text]：%#v", len(um.Content), um.Content)
	}
	if tb, ok := um.Content[0].(agent.TextBlock); !ok || tb.Text != "看图" {
		t.Fatalf("第 1 块 = %#v", um.Content[0])
	}
	ib, ok := um.Content[1].(agent.ImageBlock)
	if !ok {
		t.Fatalf("第 2 块应为 ImageBlock：%#v", um.Content[1])
	}
	if ib.Data != b64 || ib.MimeType != "image/png" {
		t.Fatalf("图片块内容错误：%#v", ib)
	}
	if tb, ok := um.Content[2].(agent.TextBlock); !ok || tb.Text != "这是什么？" {
		t.Fatalf("第 3 块 = %#v", um.Content[2])
	}
}

// TestToAgentMessageImageOnly 只有图片也要能构造出消息。
func TestToAgentMessageImageOnly(t *testing.T) {
	msg, err := toAgentMessage([]ContentBlock{
		{Type: "image", Data: "AAAA", MIMEType: "image/jpeg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	um := msg.(*agent.UserMessage)
	if len(um.Content) != 1 {
		t.Fatalf("blocks = %d", len(um.Content))
	}
	if _, ok := um.Content[0].(agent.ImageBlock); !ok {
		t.Fatalf("应为 ImageBlock：%#v", um.Content[0])
	}
}

// TestToAgentMessageImageWithoutData 客户端只给 uri 时降级为文本引用，不能丢。
func TestToAgentMessageImageWithoutData(t *testing.T) {
	msg, err := toAgentMessage([]ContentBlock{
		{Type: "image", MIMEType: "image/png", URI: "file:///tmp/a.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	um := msg.(*agent.UserMessage)
	if len(um.Content) != 1 {
		t.Fatalf("blocks = %d", len(um.Content))
	}
	tb, ok := um.Content[0].(agent.TextBlock)
	if !ok {
		t.Fatalf("应为 TextBlock：%#v", um.Content[0])
	}
	if want := "[图片：image/png file:///tmp/a.png]"; tb.Text != want {
		t.Fatalf("占位文本 = %q，期望 %q", tb.Text, want)
	}
}

// TestToAgentMessageEmpty 空 prompt 仍需报错。
func TestToAgentMessageEmpty(t *testing.T) {
	if _, err := toAgentMessage([]ContentBlock{{Type: "text", Text: ""}}); err == nil {
		t.Fatal("空内容应报错")
	}
	if _, err := toAgentMessage(nil); err == nil {
		t.Fatal("nil 应报错")
	}
}
