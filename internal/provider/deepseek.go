package provider

import (
	"strings"

	"gitlab.com/leijux/acp-agent/internal/agent"
)

// Endpoint 是一个 OpenAI 兼容服务端点的预设。
type Endpoint struct {
	// Name 是 CLI/配置里用的短名。
	Name string
	// BaseURL 不带尾斜杠，也不带 /chat/completions。
	BaseURL string
	// EnvKey 是读 API key 的环境变量名；空表示不需要（本地服务）。
	EnvKey string
	// DefaultModel 是未显式指定模型时用的 ID。
	DefaultModel string
	// Models 是推荐模型列表（供 CLI 提示，不限制用户填别的）。
	Models []string
	// Headers 是额外请求头（如 OpenRouter 的 Referer）。
	Headers map[string]string
	// NoStreamUsage 对不认 stream_options 的网关置 true。
	NoStreamUsage bool
	// Kind 传到 OpenAIConfig.Kind，启用该家特有行为（目前只有 "deepseek"）：
	// 思考模式参数与 reasoning_content 回传。空值 = 通用 OpenAI 语义。
	Kind string
}

// Endpoints 是内置端点表。CLI 的 --provider 从这里查。
var Endpoints = []Endpoint{
	{
		Name:         "deepseek",
		BaseURL:      "https://api.deepseek.com",
		EnvKey:       "DEEPSEEK_API_KEY",
		DefaultModel: "deepseek-flash",
		Models:       []string{"deepseek-flash", "deepseek-v4-pro"},
		Kind:         "deepseek",
	},
	{
		Name:         "openai",
		BaseURL:      "https://api.openai.com/v1",
		EnvKey:       "OPENAI_API_KEY",
		DefaultModel: "gpt-5",
		Models:       []string{"gpt-5", "gpt-5-mini", "gpt-4.1", "o4-mini"},
	},
	{
		Name:         "moonshot",
		BaseURL:      "https://api.moonshot.cn/v1",
		EnvKey:       "MOONSHOT_API_KEY",
		DefaultModel: "kimi-k2",
		Models:       []string{"kimi-k2", "kimi-k2-turbo-preview"},
	},
	{
		Name:         "dashscope",
		BaseURL:      "https://dashscope.aliyuncs.com/compatible-mode/v1",
		EnvKey:       "DASHSCOPE_API_KEY",
		DefaultModel: "qwen3-coder-plus",
		Models:       []string{"qwen3-coder-plus", "qwen3-coder", "qwen-max"},
	},
	{
		Name:         "zhipu",
		BaseURL:      "https://open.bigmodel.cn/api/paas/v4",
		EnvKey:       "ZHIPU_API_KEY",
		DefaultModel: "glm-4.6",
		Models:       []string{"glm-4.6", "glm-4.5"},
	},
	{
		Name:         "minimax",
		BaseURL:      "https://api.minimaxi.com/v1",
		EnvKey:       "MINIMAX_API_KEY",
		DefaultModel: "minimax-m2",
		Models:       []string{"minimax-m2"},
	},
	{
		Name:         "openrouter",
		BaseURL:      "https://openrouter.ai/api/v1",
		EnvKey:       "OPENROUTER_API_KEY",
		DefaultModel: "anthropic/claude-sonnet-4.5",
		Models:       []string{"anthropic/claude-sonnet-4.5", "openai/gpt-5", "deepseek/deepseek-flash"},
	},
	{
		// 本地 Ollama / vLLM / LM Studio：不校验 key。
		Name:         "ollama",
		BaseURL:      "http://127.0.0.1:11434/v1",
		EnvKey:       "",
		DefaultModel: "qwen3-coder",
		Models:       []string{"qwen3-coder", "gpt-oss:20b", "deepseek-r1:8b"},
	},
}

// LookupEndpoint 按名字查预设（大小写不敏感）。
func LookupEndpoint(name string) (Endpoint, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, e := range Endpoints {
		if e.Name == n {
			return e, true
		}
	}
	return Endpoint{}, false
}

// NewDeepSeek 构造 DeepSeek Provider。
//
// DeepSeek 与 OpenAI 的实质差异（都已在通用实现里处理）：
//
//  1. usage 用 prompt_cache_hit_tokens / prompt_cache_miss_tokens 计缓存，
//     不是 prompt_tokens_details.cached_tokens —— 见 convertUsage。
//  2. 思考模式**默认开启**，用 {"thinking":{"type":"enabled|disabled"}} 开关、
//     reasoning_effort(low/high/max) 控强度 —— 见 ThinkingParams。
//  3. 思考内容走 delta.reasoning_content。**带 tools 的请求必须在后续轮次
//     原样回传**，否则 400；不带 tools 时回传会被忽略。见 openAIMessages。
//  4. 思考模式下 temperature / presence_penalty / frequency_penalty 不生效
//     （不报错）；deepseek-flash 的 top_p 仅思考模式生效且下限 0.95。
//
// 所有 V4 模型共用 https://api.deepseek.com，靠 model 名区分。
func NewDeepSeek(cfg OpenAIConfig) *OpenAI {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.deepseek.com"
	}
	cfg.Kind = "deepseek"
	return NewOpenAI(cfg)
}

// ThinkingParams 把通用的思考等级映射成 DeepSeek 的两个参数。
//
// 官方映射（请求值 → 实际）：minimal→low、low→low、medium→high、
// high→high、xhigh→high、max→max。空字符串交由服务端默认（启用 + high）。
func ThinkingParams(level string) map[string]any {
	if level == "" {
		return nil
	}
	out := map[string]any{}
	if strings.EqualFold(level, "off") || strings.EqualFold(level, "none") {
		out["thinking"] = map[string]any{"type": "disabled"}
		return out
	}
	out["thinking"] = map[string]any{"type": "enabled"}
	switch strings.ToLower(level) {
	case "minimal", "low":
		out["reasoning_effort"] = "low"
	case "medium", "high", "xhigh":
		out["reasoning_effort"] = "high"
	case "max", "ultra":
		out["reasoning_effort"] = "max"
	}
	return out
}

// NewFromEndpoint 用预设端点构造 Provider。apiKey 为空时尝试读 EnvKey。
func NewFromEndpoint(endpoint Endpoint, apiKey string, headers map[string]string) *OpenAI {
	merged := map[string]string{}
	for k, v := range endpoint.Headers {
		merged[k] = v
	}
	for k, v := range headers {
		merged[k] = v
	}
	if len(merged) == 0 {
		merged = nil
	}
	return NewOpenAI(OpenAIConfig{
		BaseURL:       endpoint.BaseURL,
		APIKey:        apiKey,
		Headers:       merged,
		NoStreamUsage: endpoint.NoStreamUsage,
	})
}

// SupportsSampling 判断某模型是否接受 temperature 等采样参数。
//
// 拒绝采样参数的都是推理模型：它们内部固定采样策略，传 temperature 会被
// 直接拒绝（OpenAI o1/o3/o4、gpt-5 系列；DeepSeek 的 reasoner）。
func SupportsSampling(modelID string) bool {
	id := strings.ToLower(modelID)
	// 去掉 openrouter 风格的 "vendor/" 前缀。
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	for _, prefix := range []string{
		"deepseek-reasoner",
		"o1", "o3", "o4",
		"gpt-5",
		"gemini-2.5-flash-thinking",
	} {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	// o1-mini / o3-mini 这类也命中前缀规则。
	return true
}

// ModelFor 返回该端点上的模型元数据（无则走通用默认）。
func (e Endpoint) ModelFor(modelID string) agent.Model {
	if modelID == "" {
		modelID = e.DefaultModel
	}
	m := ResolveModel(e.Name, modelID)
	if info, ok := LookupModel(modelID); ok && info.MaxOutput > 0 && m.MaxOutputTokens > info.MaxOutput {
		// 预算裁剪不能超过模型真实输出上限。
		m.MaxOutputTokens = info.MaxOutput
	}
	return m
}
