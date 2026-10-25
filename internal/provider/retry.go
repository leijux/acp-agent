package provider

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
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
// 关键约束：**一旦已经向 sink 吐过任何增量就不再重试** —— 否则客户端会看到
// 重复的半截回复。流式响应在开始传输之后失败，只能把错误抛给上层
// （agent 会记录 errorMessage 并按 StopError 处理）。
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
		if cs.emitted() || !shouldRetry(err) || attempt == r.opts.MaxAttempts {
			return lastMsg, lastErr
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

// IsRetryable 判定错误是否值得重试：429/5xx、连接错误、超时。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
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

func (c *countingSink) emitted() bool { return c.n > 0 }
