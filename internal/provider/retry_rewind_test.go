package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

// resetSink 模拟能重绘的界面：记下增量，并支持撤销。
type resetSink struct {
	text   strings.Builder
	resets int
}

func (s *resetSink) Text(d string)                { s.text.WriteString(d) }
func (s *resetSink) Thinking(string)              {}
func (s *resetSink) ToolCall(agent.ToolCallBlock) {}
func (s *resetSink) Reset()                       { s.text.Reset(); s.resets++ }

// 已经吐过增量之后流中断：下游能撤销时应该重发，且不留下重复文本。
func TestRetry_RewindsAndReattemptsAfterPartialOutput(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		if hits.Add(1) == 1 {
			// 先吐半截，然后不发终止帧就断开。
			fmt.Fprint(w, frame(`{"choices":[{"index":0,"delta":{"content":"半截"}}]}`))
			fl.Flush()
			return
		}
		fmt.Fprint(w, frame(`{"choices":[{"index":0,"delta":{"content":"答复"}}]}`))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	base := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL})
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 3, BaseDelay: 1})
	sink := &resetSink{}

	req := agent.Request{
		Messages: []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:    provider.ResolveModel("openai", "gpt-4.1"),
	}
	msg, err := p.Stream(context.Background(), req, sink)
	if err != nil {
		t.Fatalf("下游可撤销时应重试成功，实际: %v", err)
	}
	if msg.Text() != "答复" {
		t.Errorf("消息文本 = %q", msg.Text())
	}
	// 关键：屏上只有重发的那份内容，没有「半截答复」叠在一起。
	if got := sink.text.String(); got != "答复" {
		t.Errorf("sink 收到的增量 = %q, want %q", got, "答复")
	}
	if sink.resets != 1 {
		t.Errorf("Reset 调用次数 = %d, want 1", sink.resets)
	}
	if hits.Load() != 2 {
		t.Errorf("请求次数 = %d, want 2", hits.Load())
	}
}
