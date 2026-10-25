// Package provider 实现 agent.Provider：把 agent.Request 翻成各家模型 API
// 的调用，并把流式增量回吐给 agent.Sink。
//
// 目前覆盖：
//
//	OpenAI 兼容（chat/completions）—— OpenAI、DeepSeek、Moonshot、Qwen、
//	vLLM、Ollama、LiteLLM 网关等
//	Anthropic 兼容（messages）—— Claude 直连与 Bedrock 网关
//
// 两者都实现了：流式 text/thinking、工具调用增量拼装、usage（含 cache 与
// reasoning token）、stopReason 映射、ctx 取消。失败重试见 Retry。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// OpenAIConfig 配置 OpenAI 兼容端点。
type OpenAIConfig struct {
	// BaseURL 形如 https://api.openai.com/v1（不带尾斜杠）。
	BaseURL string
	APIKey  string
	// HTTP 为 nil 时用 DefaultHTTPClient。
	HTTP *http.Client
	// Headers 追加到每个请求（覆盖默认头）。
	Headers map[string]string
	// Extra 合并进请求体的顶层字段（如 reasoning_effort、thinking）。
	Extra map[string]any
	// MaxTokensField 指定输出上限的字段名，默认 "max_tokens"。
	// OpenAI 的 o 系列与新模型要求 "max_completion_tokens"。
	MaxTokensField string
	// NoStreamUsage 关闭 stream_options.include_usage（部分旧网关不认这个字段）。
	NoStreamUsage bool
	// Kind 标识端点家族（"deepseek"），用于启用该家的特有行为：
	// 思考模式参数、带 tools 时回传 reasoning_content。空值表示通用 OpenAI。
	Kind string
}

// OpenAI 是 OpenAI 兼容 chat/completions 的 Provider。
type OpenAI struct {
	cfg OpenAIConfig
}

// NewOpenAI 构造 Provider。BaseURL 为空时用 OpenAI 官方地址。
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = DefaultHTTPClient()
	}
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	return &OpenAI{cfg: cfg}
}

// DefaultHTTPClient 返回适合流式请求的客户端：无整体超时（由 ctx 控制），
// 但握手上限 30s，并保持连接复用。
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Stream 执行一次流式请求。
func (p *OpenAI) Stream(ctx context.Context, req agent.Request, sink agent.Sink) (*agent.AssistantMessage, error) {
	if sink == nil {
		sink = agent.NoopSink{}
	}
	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider/openai: 编码请求: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("provider/openai: 构造请求: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.cfg.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider/openai: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{
			Provider:   "openai",
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(msg)),
			Retryable:  resp.StatusCode == 429 || resp.StatusCode >= 500,
		}
	}

	msg := &agent.AssistantMessage{
		Provider:      valueOr(req.Model.Provider, "openai"),
		Model:         req.Model.ID,
		ThinkingLevel: req.ThinkingLevel,
		At:            time.Now().UnixMilli(),
	}
	acc := &streamAcc{sink: sink}
	if err := acc.consume(resp.Body); err != nil {
		// 保留已收到的部分内容，便于上层记录；错误仍然要返回。
		acc.flushInto(msg)
		msg.ErrorMessage = err.Error()
		if ctx.Err() != nil {
			msg.StopReason = agent.StopAborted
		} else {
			// 流中断绝不能留 StopEnd：上层会把它当完整回复落盘。
			msg.StopReason = agent.StopError
		}
		return msg, err
	}
	acc.flushInto(msg)
	ApplyCost(msg, req.Model)
	return msg, nil
}

func valueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *OpenAI) buildBody(req agent.Request) (map[string]any, error) {
	// DeepSeek 在**带 tools** 的请求中强制要求把历史轮次的 reasoning_content
	// 原样回传（不回传直接 400）；不带 tools 时回传会被忽略。
	replayReasoning := p.cfg.Kind == "deepseek" && len(req.Tools) > 0
	msgs, err := openAIMessages(req.SystemPrompt, req.Messages, replayReasoning)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"model":    req.Model.ID,
		"messages": msgs,
		"stream":   true,
	}
	if !p.cfg.NoStreamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		body["tools"] = openAITools(req.Tools)
		body["tool_choice"] = "auto"
	}
	if req.MaxTokens > 0 {
		body[p.cfg.MaxTokensField] = req.MaxTokens
	}
	// 思考模式：DeepSeek 的开关与强度是两个字段，且默认开启。
	if p.cfg.Kind == "deepseek" {
		for k, v := range ThinkingParams(req.ThinkingLevel) {
			body[k] = v
		}
	}
	// 推理模型（DeepSeek 思考模式、o 系列、gpt-5）忽略采样参数：
	// o1 传了会直接 400，所以干脆不发。
	if req.Temperature != nil && SupportsSampling(req.Model.ID) {
		body["temperature"] = *req.Temperature
	}
	for k, v := range p.cfg.Extra {
		if _, exists := body[k]; !exists {
			body[k] = v
		}
	}
	return body, nil
}

func openAITools(tools []agent.ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// openAIMessages 把 agent 消息投影成 chat/completions 的 messages 数组。
//
// replayReasoning 为 true 时，assistant 消息会带上 reasoning_content ——
// DeepSeek 只要请求携了 tools，就必须在后续每一轮完整回传思考内容。
func openAIMessages(system string, msgs []agent.Message, replayReasoning bool) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs)+1)
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		switch v := m.(type) {
		case *agent.SystemMessage:
			if txt := blocksText(v.Content); txt != "" {
				out = append(out, map[string]any{"role": "system", "content": txt})
			}

		case *agent.UserMessage:
			content, err := userContent(v.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"role": "user", "content": content})

		case *agent.AssistantMessage:
			entry := map[string]any{"role": "assistant"}
			if txt := blocksText(v.Content); txt != "" {
				entry["content"] = txt
			} else {
				entry["content"] = nil
			}
			if replayReasoning {
				if think := blocksThinking(v.Content); think != "" {
					entry["reasoning_content"] = think
				}
			}
			if calls := v.ToolCalls(); len(calls) > 0 {
				tcs := make([]map[string]any, 0, len(calls))
				for _, c := range calls {
					args, err := json.Marshal(c.Arguments)
					if err != nil {
						return nil, fmt.Errorf("provider/openai: 工具 %s 参数编码: %w", c.Name, err)
					}
					tcs = append(tcs, map[string]any{
						"id":   c.ID,
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": string(args),
						},
					})
				}
				entry["tool_calls"] = tcs
			}
			out = append(out, entry)

		case *agent.ToolResultMessage:
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": v.ToolCallID,
				"content":      blocksText(v.Content),
			})

		case *agent.CompactionSummaryMessage:
			out = append(out, map[string]any{
				"role":    "user",
				"content": "[之前对话的压缩摘要]\n" + v.Summary,
			})

		case *agent.BranchSummaryMessage:
			out = append(out, map[string]any{
				"role":    "user",
				"content": "[另一分支的摘要]\n" + v.Summary,
			})

		case *agent.BashExecutionMessage:
			out = append(out, map[string]any{
				"role":    "user",
				"content": "$ " + v.Command + "\n" + v.Output,
			})

		case *agent.CustomMessage:
			out = append(out, map[string]any{"role": "user", "content": blocksText(v.Content)})

		default:
			return nil, fmt.Errorf("provider/openai: 不支持的消息类型 %T", m)
		}
	}
	return out, nil
}

// userContent 在有图片时返回 content parts 数组，否则返回纯字符串
// （纯字符串兼容性最好，很多兼容实现不认 parts）。
func userContent(blocks []agent.Block) (any, error) {
	hasImage := false
	for _, b := range blocks {
		if _, ok := b.(agent.ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return blocksText(blocks), nil
	}
	parts := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case agent.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": v.Text})
		case agent.ImageBlock:
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + v.MimeType + ";base64," + v.Data,
				},
			})
		}
	}
	return parts, nil
}

func blocksText(blocks []agent.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		switch v := blk.(type) {
		case agent.TextBlock:
			b.WriteString(v.Text)
		case agent.ThinkingBlock:
			// thinking 不进 content（有独立字段）；见 blocksThinking。
		case agent.ToolCallBlock:
			// toolCall 由 tool_calls 字段表达。
		}
	}
	return b.String()
}

// blocksThinking 拼出思考内容（用于 reasoning_content 回传）。
func blocksThinking(blocks []agent.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(agent.ThinkingBlock); ok {
			b.WriteString(tb.Thinking)
		}
	}
	return b.String()
}

// ---------- 流式解析 ----------

type openAIChunk struct {
	ID      string              `json:"id"`
	Model   string              `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
	Usage   *openAIUsage        `json:"usage"`
	Error   *openAIError        `json:"error"`
}

type openAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type openAIChunkDelta struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent *string          `json:"reasoning_content"`
	Reasoning        *string          `json:"reasoning"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	// DeepSeek 的缓存命中计量用的是独立字段（不是 prompt_tokens_details），
	// 两者都要取，指标才能通。
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// streamAcc 累积流式增量。
type streamAcc struct {
	sink          agent.Sink
	text          strings.Builder
	reasoning     strings.Builder
	calls         []*accCall
	usage         agent.Usage
	responseID    string
	responseModel string
	finish        *string

	// done 记录是否见到过终止信号（[DONE] / finish_reason / usage）。
	// 连接层 EOF 不等于逻辑结束：上游被切断时必须报错，否则半截回复会
	// 被当成完整响应落盘。
	done bool
}

type accCall struct {
	id    string
	name  string
	args  strings.Builder
	order int
}

func (a *streamAcc) callAt(index int) *accCall {
	for _, c := range a.calls {
		if c.order == index {
			return c
		}
	}
	c := &accCall{order: index}
	a.calls = append(a.calls, c)
	return c
}

func (a *streamAcc) consume(r io.Reader) error {
	sse := newSSEReader(r)
	for {
		ev, err := sse.Next()
		if err == io.EOF {
			if !a.done {
				return ErrSSEUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("provider/openai: 读取流: %w", err)
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			a.done = true
			return nil
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("provider/openai: 解析流数据: %w (%s)", err, truncate(data, 200))
		}
		if chunk.Error != nil {
			return &APIError{Provider: "openai", Message: chunk.Error.Message, Retryable: false}
		}
		if chunk.ID != "" {
			a.responseID = chunk.ID
		}
		if chunk.Model != "" {
			a.responseModel = chunk.Model
		}
		if chunk.Usage != nil {
			a.usage = convertUsage(chunk.Usage)
			a.done = true
		}
		for _, ch := range chunk.Choices {
			// 只关心 index 0：n>1 时 agent 用不到其它候选。
			if ch.Index != 0 {
				continue
			}
			if ch.FinishReason != nil {
				fr := *ch.FinishReason
				a.finish = &fr
				a.done = true
			}
			d := ch.Delta
			if d.ReasoningContent != nil && *d.ReasoningContent != "" {
				a.reasoning.WriteString(*d.ReasoningContent)
				a.sink.Thinking(*d.ReasoningContent)
			}
			if d.Reasoning != nil && *d.Reasoning != "" {
				a.reasoning.WriteString(*d.Reasoning)
				a.sink.Thinking(*d.Reasoning)
			}
			if d.Content != nil && *d.Content != "" {
				a.text.WriteString(*d.Content)
				a.sink.Text(*d.Content)
			}
			for i, tc := range d.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				c := a.callAt(idx)
				if tc.ID != "" {
					c.id = tc.ID
				}
				if tc.Function.Name != "" {
					c.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					c.args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
}

// flushInto 把累积内容写进 assistant 消息，并回调 sink.ToolCall。
func (a *streamAcc) flushInto(msg *agent.AssistantMessage) {
	msg.ResponseID = a.responseID
	msg.ResponseModel = a.responseModel
	msg.Usage = a.usage

	if a.reasoning.Len() > 0 {
		msg.Content = append(msg.Content, agent.ThinkingBlock{Thinking: a.reasoning.String()})
	}
	if a.text.Len() > 0 {
		msg.Content = append(msg.Content, agent.TextBlock{Text: a.text.String()})
	}
	for i, c := range a.calls {
		args := map[string]any{}
		raw := strings.TrimSpace(c.args.String())
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				// 参数不是合法 JSON：把原文塞进 _raw，让模型/日志能看出问题。
				args = map[string]any{"_raw": raw}
			}
		}
		id := c.id
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", msg.At, i)
		}
		call := agent.ToolCallBlock{ID: id, Name: c.name, Arguments: args}
		msg.Content = append(msg.Content, call)
		a.sink.ToolCall(call)
	}

	msg.StopReason = mapFinishReason(a.finish, len(a.calls) > 0)
	if a.finish != nil {
		msg.RawStopReason = *a.finish
	}
}

func mapFinishReason(fr *string, hasToolCalls bool) agent.StopReason {
	if hasToolCalls {
		return agent.StopToolUse
	}
	if fr == nil {
		return agent.StopEnd
	}
	switch *fr {
	case "tool_calls", "function_call":
		return agent.StopToolUse
	case "length":
		return agent.StopLength
	case "content_filter":
		return agent.StopError
	case "stop", "":
		return agent.StopEnd
	default:
		return agent.StopEnd
	}
}

func convertUsage(u *openAIUsage) agent.Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	// DeepSeek 只给 prompt_cache_hit_tokens；OpenAI/Moonshot 走
	// prompt_tokens_details.cached_tokens。取较大值即可兼容两者。
	cacheRead := u.PromptTokensDetails.CachedTokens
	if u.PromptCacheHitTokens > cacheRead {
		cacheRead = u.PromptCacheHitTokens
	}
	return agent.Usage{
		Input:       u.PromptTokens,
		Output:      u.CompletionTokens,
		CacheRead:   cacheRead,
		Reasoning:   u.CompletionTokensDetails.ReasoningTokens,
		TotalTokens: total,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// APIError 是上游返回的错误。
type APIError struct {
	Provider   string
	StatusCode int
	Message    string
	Retryable  bool
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("provider/%s: HTTP %d: %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("provider/%s: %s", e.Provider, e.Message)
}
