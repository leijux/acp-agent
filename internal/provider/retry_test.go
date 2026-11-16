package provider_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

// 网络异常（200 之后什么都没发就断开）应该整体重试，因为客户端还没看到
// 任何增量，重发不会造成重复内容。
func TestRetry_ReattemptsAfterEmptyDroppedStream(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// 先回 200 + 头，然后立刻断开：典型的网络闪断。
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`))
		fmt.Fprint(w, "data: [DONE]\n\n")
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
		t.Fatalf("空流中断应重试并成功，实际: %v", err)
	}
	if msg.Text() != "ok" {
		t.Errorf("text = %q", msg.Text())
	}
	if hits.Load() != 2 {
		t.Errorf("请求次数 = %d, want 2", hits.Load())
	}
}

// 连不上（服务端端口关掉）属于传输层失败，要重试满 MaxAttempts 次。
func TestRetry_ReattemptsOnConnectError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	url := srv.URL
	srv.Close() // 端口关掉，连不上

	base := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: url})
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 3, BaseDelay: 1})

	req := agent.Request{
		Messages: []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:    provider.ResolveModel("openai", "gpt-4.1"),
	}
	if _, err := p.Stream(context.Background(), req, agent.NoopSink{}); err == nil {
		t.Fatal("连不上应返回错误")
	}
}

// 用户取消不能被当成网络异常重试。
func TestRetry_NoRetryOnCancel(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	base := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL})
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 3, BaseDelay: 1})

	ctx, cancel := context.WithCancel(context.Background())
	req := agent.Request{
		Messages: []agent.Message{&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}}},
		Model:    provider.ResolveModel("openai", "gpt-4.1"),
	}
	go func() {
		for hits.Load() == 0 {
		}
		cancel()
	}()
	if _, err := p.Stream(ctx, req, agent.NoopSink{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("取消后不应重试，实际请求 %d 次", n)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &provider.APIError{StatusCode: 429, Retryable: true}, true},
		{"5xx", &provider.APIError{StatusCode: 503, Retryable: true}, true},
		{"400", &provider.APIError{StatusCode: 400}, false},
		{"流内业务错误", &provider.APIError{Message: "invalid request"}, false},
		{"传输层断开", &provider.TransportError{Op: "provider/openai: 读取流", Err: io.ErrUnexpectedEOF}, true},
		{"裸 EOF", io.EOF, true},
		{"意外 EOF", io.ErrUnexpectedEOF, true},
		{"连接重置", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"域名不存在", &net.DNSError{Err: "no such host", Name: "api.exmaple.invalid", IsNotFound: true}, false},
		{"DNS 临时失败", &net.DNSError{Err: "server misbehaving", Name: "api.example.com", IsTemporary: true}, true},
		{"超时", context.DeadlineExceeded, true},
		{"取消", context.Canceled, false},
		{"包装过的取消", &provider.TransportError{Op: "provider/openai: 读取流", Err: context.Canceled}, false},
		{"编码错误", errors.New("json: unsupported value"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := provider.IsRetryable(c.err); got != c.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
