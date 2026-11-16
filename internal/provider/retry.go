package provider

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// RetryOptions 控制重试策略。
type RetryOptions struct {
	// MaxAttempts 总尝试次数（含首次），<=0 时取 3。
	MaxAttempts int
	// BaseDelay 首次退避，默认 500ms，之后指数增长。
	BaseDelay time.Duration
	// MaxDelay 单次退避上限，默认 8s。
	MaxDelay time.Duration
	// ShouldRetry 覆盖默认可重试判定。
	ShouldRetry func(error) bool
}

// Retry 给 Provider 加重试。
//
// 关键约束：向 sink 吐过增量之后的失败，只在 sink 实现了 agent.Resetter
// （下游能撤销已渲染内容）时才重发，并在重发前先 Reset；否则直接抛给上层
// （agent 会记录 errorMessage 并按 StopError 处理）。不这么做，客户端就会
// 看到两遍半截回复。
func Retry(inner agent.Provider, opts RetryOptions) agent.Provider {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = 500 * time.Millisecond
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = 8 * time.Second
	}
	return &retrier{inner: inner, opts: opts}
}

type retrier struct {
	inner agent.Provider
	opts  RetryOptions
}

func (r *retrier) Stream(ctx context.Context, req agent.Request, sink agent.Sink) (*agent.AssistantMessage, error) {
	if sink == nil {
		sink = agent.NoopSink{}
	}
	shouldRetry := r.opts.ShouldRetry
	if shouldRetry == nil {
		shouldRetry = IsRetryable
	}

	var lastMsg *agent.AssistantMessage
	var lastErr error
	delay := r.opts.BaseDelay

	for attempt := 1; attempt <= r.opts.MaxAttempts; attempt++ {
		cs := &countingSink{inner: sink}
		msg, err := r.inner.Stream(ctx, req, cs)
		if err == nil {
			return msg, nil
		}
		lastMsg, lastErr = msg, err

		if ctx.Err() != nil {
			return lastMsg, ctx.Err()
		}
		if !shouldRetry(err) || attempt == r.opts.MaxAttempts {
			return lastMsg, lastErr
		}
		if cs.emitted() {
			// 已经向客户端吐过增量：只有下游能撤销（Sink 实现 Resetter，
			// 例如终端界面）才敢重发，否则用户会看到重复的半截回复。
			rs, ok := sink.(agent.Resetter)
			if !ok {
				return lastMsg, lastErr
			}
			rs.Reset()
		}

		// 指数退避 + 抖动（±25%），避免多个并发请求同时重试。
		wait := delay
		jitter := time.Duration(rand.Int64N(int64(wait)/2+1)) - wait/4
		if wait+jitter > 0 {
			wait += jitter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastMsg, ctx.Err()
		case <-timer.C:
		}
		if delay *= 2; delay > r.opts.MaxDelay {
			delay = r.opts.MaxDelay
		}
	}
	return lastMsg, lastErr
}

// TransportError 标记传输层失败：连不上、连接被重置、响应体读到一半断了。
//
// 和 APIError 的区别是错误发生在字节流上、而不是协议层——服务端可能
// 已经开始回数据了。所以它**可重试**；至于重发会不会让用户看到两遍半截
// 回复，由 Retry 按 sink 是否支持撤销（agent.Resetter）决定。
//
// 它用 Unwrap 保留原始错误，errors.Is/As 仍能取到 context.Canceled、
// *net.OpError 等底层原因。
type TransportError struct {
	// Op 是操作描述（如 "provider/openai: 读取流"），用于拼错误文本。
	Op  string
	Err error
}

func (e *TransportError) Error() string {
	if e.Err == nil {
		return e.Op
	}
	if e.Op == "" {
		return e.Err.Error()
	}
	return e.Op + ": " + e.Err.Error()
}

func (e *TransportError) Unwrap() error { return e.Err }

// IsRetryable 判定错误是否值得重试：429/5xx/408/425、传输层失败、连接错误、超时。
//
// 明确**不**重试的：用户取消（context.Canceled）、域名不存在（DNS NXDOMAIN，
// 重试只会再错一次）、请求构造/编码错误、上游在流里返回的业务错误
// （chunk.error，见 APIError.Retryable=false）。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable
	}
	// 域名解析不出结果（BaseURL 写错、DNS 配错）不是抖动，重发没有意义。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	// 传输层失败：连接被重置、响应体半截断开、流没有终止帧就 EOF。
	// 这些都值得把同一个请求原样再发一次。
	var trErr *TransportError
	if errors.As(err, &trErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

// isRetryableStatus 判断 HTTP 状态码是否值得重试。
//
// 408/425 是「这次请求本身没送达」，429/5xx 是服务端临时不可用，
// 其余 4xx（400/401/403/404）重发只会得到同样的结果。
func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooEarly ||
		code == http.StatusTooManyRequests || code >= 500
}

// countingSink 记录是否已经产生过增量。
type countingSink struct {
	inner agent.Sink
	n     int
}

func (c *countingSink) Text(d string) {
	if d == "" {
		return
	}
	c.n++
	c.inner.Text(d)
}

func (c *countingSink) Thinking(d string) {
	if d == "" {
		return
	}
	c.n++
	c.inner.Thinking(d)
}

func (c *countingSink) ToolCall(call agent.ToolCallBlock) {
	c.n++
	c.inner.ToolCall(call)
}

// Reset 转发到底层 sink（若支持），并清零增量计数：重发后计数从零开始。
func (c *countingSink) Reset() {
	c.n = 0
	if rs, ok := c.inner.(agent.Resetter); ok {
		rs.Reset()
	}
}

func (c *countingSink) emitted() bool { return c.n > 0 }
