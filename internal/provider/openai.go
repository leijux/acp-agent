// Package provider 实现 agent.Provider：把 agent.Request 翻成各家模型 API
// 的调用，并把流式增量回吐给 agent.Sink。
//
// 目前覆盖：
//
//	OpenAI 兼容（chat/completions）—— OpenAI、DeepSeek、Moonshot、Qwen、
//	vLLM、Ollama、LiteLLM 网关等
//	DeepSeek 特有行为 —— 思考参数、reasoning_content 回传、tool 消息内联图片
//	（见 deepseek.go 与 cfg.Kind）
//
// 实现了：流式 text/thinking、工具调用增量拼装、usage（含 cache 与
// reasoning token）、stopReason 映射、ctx 取消、图片（user 与 tool 消息）。
// 失败重试见 Retry。
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
		// Do 在重定向失败等场景会同时返回非 nil 的 resp（body 已可读），
		// 不关就是连接泄漏。
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		// 连不上 / 握手失败 / 连接被重置：标成 TransportError 让 Retry 能重试。
		return nil, &TransportError{Op: "provider/openai: 请求失败", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{
			Provider:   "openai",
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(msg)),
			Retryable:  isRetryableStatus(resp.StatusCode),
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
	opts := openAIMessageOptions{
		replayReasoning:  p.cfg.Kind == "deepseek" && len(req.Tools) > 0,
		vision:           req.Model.SupportsImages,
		toolInlineImages: p.cfg.Kind == "deepseek",
		maxImageBytes:    maxInlineImageBytes,
		maxImages:        maxRequestImages,
	}
	if p.cfg.Kind == "deepseek" {
		opts.maxImageBytes = maxInlineImageBytesDeepSeek
	}
	msgs, err := openAIMessages(req.SystemPrompt, req.Messages, opts)
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

// openAIMessageOptions 收拢 openAIMessages 的端点差异。
//
// 拆成结构体而不是五个参数：这些开关彼此无关（思考回传是 DeepSeek 工具协议、
// 视觉是模型能力、tool 内联图是端点家族），调用点写一行更不容易接错。
type openAIMessageOptions struct {
	// replayReasoning 为 true 时 assistant 消息带 reasoning_content：
	// DeepSeek 只要请求携了 tools，就必须在后续每一轮完整回传思考内容。
	replayReasoning bool
	// vision 表示当前模型能看图（agent.Model.SupportsImages）。为 false 时
	// 图片一律降级成文本占位，绝不发 image_url —— 发过去就是 400。
	vision bool
	// toolInlineImages 表示可以把图片放进 role:tool 的 content 数组。
	// DeepSeek 文档明确「图片可以出现在 user、system 和 tool 消息中」；
	// 通用 OpenAI 端点的 tool content 只接受字符串，只能后置成 user 消息。
	toolInlineImages bool
	// maxImageBytes/maxImages 是整次请求的图片闸门（见 imageGate）。
	maxImageBytes int
	maxImages     int
}

// 图片闸门默认值。
//
// 总量按 base64 累计，比的是请求体大小：DeepSeek 请求体上限 48 MiB、
// 单请求图片总量 64 MiB（不含 Files API）；通用 OpenAI 兼容端点没有公开
// 数字，但 20 MiB 附近就开始 400。取值都留了余量。
const (
	maxInlineImageBytes         = 16 << 20
	maxInlineImageBytesDeepSeek = 40 << 20
	maxRequestImages            = 32
)

// openAIMessages 把 agent 消息投影成 chat/completions 的 messages 数组。
func openAIMessages(system string, msgs []agent.Message, opts openAIMessageOptions) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs)+1)
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	gate := newImageGate(opts.maxImageBytes, opts.maxImages)
	// 不能进请求体、或只能后置的图片先攒在这里。
	var pending []pendingToolImages
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		blocks := make([]agent.Block, 0, len(pending)+1)
		blocks = append(blocks, agent.TextBlock{Text: toolImagesNote(pending)})
		for _, p := range pending {
			for _, im := range p.images {
				blocks = append(blocks, im)
			}
		}
		pending = nil
		content, err := userContent(blocks)
		if err != nil {
			return err
		}
		out = append(out, map[string]any{"role": "user", "content": content})
		return nil
	}
	for _, m := range msgs {
		// tool 消息必须连续紧跟 assistant 的 tool_calls，所以后置的 user 图片
		// 只能在「这一批 tool 消息结束之后」插入。一轮并行调多个工具时
		// （internal/agent/agent.go 的 Execute 结果顺序）这里就是唯一正确的位置。
		if _, isTool := m.(*agent.ToolResultMessage); !isTool {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		switch v := m.(type) {
		case *agent.SystemMessage:
			if txt := blocksText(v.Content); txt != "" {
				out = append(out, map[string]any{"role": "system", "content": txt})
			}

		case *agent.UserMessage:
			content, err := gatedUserContent(v.Content, gate, opts.vision)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"role": "user", "content": content})

		case *agent.AssistantMessage:
			txt := blocksText(v.Content)
			calls := v.ToolCalls()
			// 上游硬规则：content 与 tool_calls 至少有一个，否则
			// "Invalid assistant message: content or tool_calls must be set"。
			// 空壳（例如只剥剩思考块、或模型返回了空回复）直接不发。
			if txt == "" && len(calls) == 0 {
				continue
			}
			entry := map[string]any{"role": "assistant"}
			if txt != "" {
				entry["content"] = txt
			} else {
				entry["content"] = nil
			}
			if opts.replayReasoning {
				if think := blocksThinking(v.Content); think != "" {
					entry["reasoning_content"] = think
				}
			}
			if len(calls) > 0 {
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
			text := blocksText(v.Content)
			var admitted []agent.ImageBlock
			var notes []string
			for _, im := range agent.ImageBlocks(v.Content) {
				if !opts.vision {
					notes = appendNote(notes, "当前模型不支持图片输入（图片已省略）")
					continue
				}
				got, why := gate.admit(im)
				if why != "" {
					notes = appendNote(notes, why)
					continue
				}
				admitted = append(admitted, got)
			}
			text = withNotes(text, notes)
			entry := map[string]any{"role": "tool", "tool_call_id": v.ToolCallID}
			switch {
			case len(admitted) == 0:
				entry["content"] = text
			case opts.toolInlineImages:
				// DeepSeek：图直接留在 tool 消息里，模型能对上「哪张图是哪个工具给的」。
				parts := make([]map[string]any, 0, len(admitted)+1)
				parts = append(parts, map[string]any{"type": "text", "text": textOr(text, "[工具返回了图片]")})
				for _, im := range admitted {
					parts = append(parts, imagePart(im))
				}
				entry["content"] = parts
			default:
				// 通用 OpenAI 端点：tool content 只认字符串，图片后置成一条 user 消息。
				pending = append(pending, pendingToolImages{tool: v.ToolName, images: admitted})
				entry["content"] = textOr(text, "[工具 "+v.ToolName+" 返回了图片，见下一则消息]")
			}
			out = append(out, entry)

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
	// 会话末尾（最后一轮工具结果就是会话末尾）也要把图发出去。
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// pendingToolImages 是「暂时拿不进请求体的工具图片」。
type pendingToolImages struct {
	tool   string
	images []agent.ImageBlock
}

// imageGate 在整个请求上限制图片：格式、张数、base64 总量。
//
// 超限不报错，只把剩下的图换成一句说明。图片是附加信息，为它整轮 400
// （或让用户白等一次重试）的代价远大于「少看一张图」。
type imageGate struct {
	maxBytes, used int
	maxImages, n   int
}

func newImageGate(maxBytes, maxImages int) *imageGate {
	return &imageGate{maxBytes: maxBytes, maxImages: maxImages}
}

// admit 判定一张图能不能进请求体，返回规范化后的块和拒绝原因（接受时为空）。
func (g *imageGate) admit(im agent.ImageBlock) (agent.ImageBlock, string) {
	mime, ok := agent.VisionMIMEType(im.MimeType)
	if !ok {
		return agent.ImageBlock{}, fmt.Sprintf("图片格式 %s 不受支持（仅 JPEG/PNG/GIF/WebP），已省略", displayMIME(im.MimeType))
	}
	if g.maxImages > 0 && g.n >= g.maxImages {
		return agent.ImageBlock{}, fmt.Sprintf("单次请求图片数量上限 %d，已省略后面的图", g.maxImages)
	}
	if g.maxBytes > 0 && g.used+len(im.Data) > g.maxBytes {
		return agent.ImageBlock{}, "单次请求图片总量超限，已省略"
	}
	g.n++
	g.used += len(im.Data)
	im.MimeType = mime
	return im, ""
}

// displayMIME 让空 MIME 在提示里可读，而不是留一个空洞。
func displayMIME(mime string) string {
	if strings.TrimSpace(mime) == "" {
		return "(未声明)"
	}
	return mime
}

// imagePart 组装 OpenAI 的 image_url 内容块（本地图片一律走 base64 data URL）。
func imagePart(im agent.ImageBlock) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:" + im.MimeType + ";base64," + im.Data},
	}
}

// toolImagesNote 说明后置的图片是哪些工具给的。
func toolImagesNote(pending []pendingToolImages) string {
	var names []string
	for _, p := range pending {
		names = append(names, fmt.Sprintf("%s %d 张", p.tool, len(p.images)))
	}
	return "[以下是工具返回的图片：" + strings.Join(names, "；") + "]"
}

// appendNote 追加一条降级说明（同因去重：三张图不支持格式只需要说一次）。
func appendNote(notes []string, note string) []string {
	for _, n := range notes {
		if n == note {
			return notes
		}
	}
	return append(notes, note)
}

// withNotes 把降级说明接到文本后面，保持「先正文、后附注」的阅读顺序。
func withNotes(text string, notes []string) string {
	for _, n := range notes {
		if text != "" {
			text += "\n"
		}
		text += "[" + n + "]"
	}
	return text
}

// textOr 在正文为空时给出占位，避免 tool content 成为空串。
func textOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// userContent 只负责组装：入参里的图片视为已审核（供后置图片的 flush 用）。
// 有图片时返回 content parts 数组，否则返回纯字符串
// （纯字符串兼容性最好，很多兼容实现不认 parts）。
func userContent(blocks []agent.Block) (any, error) {
	if len(agent.ImageBlocks(blocks)) == 0 {
		return blocksText(blocks), nil
	}
	parts := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case agent.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": v.Text})
		case agent.ImageBlock:
			parts = append(parts, imagePart(v))
		}
	}
	return parts, nil
}

// gatedUserContent 是用户消息的入口：与工具返回的图片共用同一道闸门。
//
// 客户端完全可以把一张 BMP 或 30MB 截图塞进 prompt，不过闸的话整个请求
// 会因它 400 —— 而那些图本来也不在模型的视觉输入白名单里。
func gatedUserContent(blocks []agent.Block, gate *imageGate, vision bool) (any, error) {
	var parts []map[string]any
	var notes []string
	accepted := 0
	for _, b := range blocks {
		switch v := b.(type) {
		case agent.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": v.Text})
		case agent.ImageBlock:
			if !vision {
				notes = appendNote(notes, "当前模型不支持图片输入（图片已省略）")
				continue
			}
			got, why := gate.admit(v)
			if why != "" {
				notes = appendNote(notes, why)
				continue
			}
			accepted++
			parts = append(parts, imagePart(got))
		}
	}
	if accepted == 0 {
		// 没有能发出去的图：退回纯字符串（兼容性最好），并附上降级说明。
		return withNotes(blocksText(blocks), notes), nil
	}
	if len(notes) > 0 {
		parts = append(parts, map[string]any{"type": "text", "text": "[" + strings.Join(notes, "；") + "]"})
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
				// 没有 [DONE] / finish_reason 就 EOF：连接被上游或网络切断了。
				return &TransportError{Op: "provider/openai: 读取流", Err: ErrSSEUnexpectedEOF}
			}
			return nil
		}
		if err != nil {
			// 读 body 出错只可能是传输层（连接重置、TLS 中断、gzip 截断等）。
			return &TransportError{Op: "provider/openai: 读取流", Err: err}
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

// overflowKeywords 是各家「上下文超限」错误的关键词。
//
// 文案不稳（OpenAI: "maximum context length is N tokens"；DeepSeek / 网关:
// "context_length_exceeded"、"reduce the length of the messages"），所以按
// 关键词而不是精确匹配判定。
var overflowKeywords = []string{
	"maximum context length",
	"context_length_exceeded",
	"reduce the length of the messages",
	"prompt is too long",
	"too many tokens",
	"exceeds the context window",
}

// ContextOverflow 报告这个错误是否是「请求超过上下文窗口」。
//
// 实现 agent.OverflowDetector：上层（agent.Run）据此触发压缩重试。
// 判定不看 Retryable —— 超限重试原样重发没用，必须先缩上下文。
//
// StatusCode == 0 也要算：部分网关把超限放在 200 的 SSE error chunk 里
// （openai.go 里那个 chunk.Error 分支就是 StatusCode 0）。
func (e *APIError) ContextOverflow() bool {
	switch e.StatusCode {
	case 0, 400, 413, 422:
	default:
		return false
	}
	msg := strings.ToLower(e.Message)
	for _, k := range overflowKeywords {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}
