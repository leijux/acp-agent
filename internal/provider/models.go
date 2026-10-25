package provider

import (
	"strings"
	"sync"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// ModelInfo 是一个模型的静态元数据。
//
// 价格单位是「币种 / 百万 token」，币种由 Currency 指定（空值为 USD）。
// 部分服务商（DeepSeek）按时段浮动定价：Input/Output/CacheRead 是**高峰价**，
// *OffPeak 是空闲时段价；OffPeak 三项为 0 表示不分时段。
type ModelInfo struct {
	ID            string
	ContextWindow int
	MaxOutput     int
	Currency      string // "USD"（空）/ "CNY"

	Input            float64
	Output           float64
	CacheRead        float64
	CacheWrite       float64
	InputOffPeak     float64
	OutputOffPeak    float64
	CacheReadOffPeak float64

	SupportsImages   bool
	SupportsThinking bool

	// Deprecated 非空表示该 ID 已下线/被取代，值是建议的替代模型。
	Deprecated string
}

// 内置价格表。值取各家公开定价，用作成本估算的默认值；
// 真实计费请以账单为准，可用 RegisterModel 覆盖。
var builtinModels = []ModelInfo{
	// ---------- Anthropic ----------
	{ID: "claude-opus-4-1", ContextWindow: 200_000, MaxOutput: 32_000, Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-opus-4", ContextWindow: 200_000, MaxOutput: 32_000, Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-sonnet-4-5", ContextWindow: 200_000, MaxOutput: 64_000, Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-sonnet-4", ContextWindow: 200_000, MaxOutput: 64_000, Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-3-7-sonnet", ContextWindow: 200_000, MaxOutput: 64_000, Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-haiku-4-5", ContextWindow: 200_000, MaxOutput: 32_000, Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25, SupportsImages: true, SupportsThinking: true},
	{ID: "claude-3-5-haiku", ContextWindow: 200_000, MaxOutput: 8_192, Input: 0.8, Output: 4, CacheRead: 0.08, CacheWrite: 1, SupportsImages: true},

	// ---------- OpenAI ----------
	{ID: "gpt-5", ContextWindow: 400_000, MaxOutput: 128_000, Input: 1.25, Output: 10, CacheRead: 0.125, SupportsImages: true, SupportsThinking: true},
	{ID: "gpt-5-mini", ContextWindow: 400_000, MaxOutput: 128_000, Input: 0.25, Output: 2, CacheRead: 0.025, SupportsImages: true, SupportsThinking: true},
	{ID: "gpt-5-nano", ContextWindow: 400_000, MaxOutput: 128_000, Input: 0.05, Output: 0.4, CacheRead: 0.005, SupportsImages: true, SupportsThinking: true},
	{ID: "gpt-4.1", ContextWindow: 1_047_576, MaxOutput: 32_768, Input: 2, Output: 8, CacheRead: 0.5, SupportsImages: true},
	{ID: "gpt-4.1-mini", ContextWindow: 1_047_576, MaxOutput: 32_768, Input: 0.4, Output: 1.6, CacheRead: 0.1, SupportsImages: true},
	{ID: "o3", ContextWindow: 200_000, MaxOutput: 100_000, Input: 2, Output: 8, CacheRead: 0.5, SupportsImages: true, SupportsThinking: true},
	{ID: "o4-mini", ContextWindow: 200_000, MaxOutput: 100_000, Input: 1.1, Output: 4.4, CacheRead: 0.275, SupportsImages: true, SupportsThinking: true},

	// ---------- Google ----------
	{ID: "gemini-2.5-pro", ContextWindow: 1_048_576, MaxOutput: 65_536, Input: 1.25, Output: 10, CacheRead: 0.31, SupportsImages: true, SupportsThinking: true},
	{ID: "gemini-2.5-flash", ContextWindow: 1_048_576, MaxOutput: 65_536, Input: 0.3, Output: 2.5, CacheRead: 0.075, SupportsImages: true, SupportsThinking: true},

	// ---------- 国产 / 开源 ----------
	// DeepSeek V4 系列（2026）：1M 上下文 / 384K 输出，思考模式默认开启。
	// 价格单位人民币/百万 token；Input/Output/CacheRead 是高峰价，*OffPeak 是空闲价。
	// 高峰：北京时间周一~周五 9:00-12:00、14:00-18:00（不含法定节假日）。
	{
		ID: "deepseek-flash", ContextWindow: 1_000_000, MaxOutput: 384_000, Currency: "CNY",
		Input: 2, Output: 8, CacheRead: 0.04,
		InputOffPeak: 1, OutputOffPeak: 4, CacheReadOffPeak: 0.02,
		SupportsImages: true, SupportsThinking: true,
	},
	{
		ID: "deepseek-v4-pro", ContextWindow: 1_000_000, MaxOutput: 384_000, Currency: "CNY",
		Input: 9, Output: 27, CacheRead: 0.30,
		InputOffPeak: 4.5, OutputOffPeak: 13.5, CacheReadOffPeak: 0.15,
		SupportsThinking: true,
	},
	// 旧名：仍可调用，实际由 V4.1-Flash 提供服务并按 Flash 价计费。
	{ID: "deepseek-v4-flash", ContextWindow: 1_000_000, MaxOutput: 384_000, Currency: "CNY",
		Input: 2, Output: 8, CacheRead: 0.04,
		InputOffPeak: 1, OutputOffPeak: 4, CacheReadOffPeak: 0.02,
		SupportsImages: true, SupportsThinking: true, Deprecated: "deepseek-flash"},
	{ID: "deepseek-v4-flash-vision-exp", ContextWindow: 1_000_000, MaxOutput: 384_000, Currency: "CNY",
		Input: 2, Output: 8, CacheRead: 0.04,
		InputOffPeak: 1, OutputOffPeak: 4, CacheReadOffPeak: 0.02,
		SupportsImages: true, SupportsThinking: true, Deprecated: "deepseek-flash"},
	// V3 时代的模型名，保留价格供历史账单回算；新请求请用 deepseek-flash。
	{ID: "deepseek-chat", ContextWindow: 128_000, MaxOutput: 8_192, Currency: "CNY",
		Input: 2, Output: 8, CacheRead: 0.5, Deprecated: "deepseek-flash"},
	{ID: "deepseek-reasoner", ContextWindow: 128_000, MaxOutput: 65_536, Currency: "CNY",
		Input: 4, Output: 16, CacheRead: 1, SupportsThinking: true, Deprecated: "deepseek-flash"},
	{ID: "qwen3-coder-plus", ContextWindow: 1_000_000, MaxOutput: 65_536, Input: 1, Output: 5},
	{ID: "qwen3-coder", ContextWindow: 262_144, MaxOutput: 65_536, Input: 0.3, Output: 1.2},
	{ID: "kimi-k2", ContextWindow: 262_144, MaxOutput: 32_768, Input: 0.6, Output: 2.5},
	{ID: "glm-4.6", ContextWindow: 204_800, MaxOutput: 131_072, Input: 0.6, Output: 2.2},
	{ID: "minimax-m2", ContextWindow: 204_800, MaxOutput: 65_536, Input: 0.3, Output: 1.2},
}

var (
	modelMu     sync.RWMutex
	modelCustom []ModelInfo
)

// RegisterModel 注册/覆盖模型元数据（最长 ID 优先匹配）。
func RegisterModel(info ModelInfo) {
	modelMu.Lock()
	defer modelMu.Unlock()
	for i, m := range modelCustom {
		if m.ID == info.ID {
			modelCustom[i] = info
			return
		}
	}
	modelCustom = append(modelCustom, info)
}

// LookupModel 按 ID 查元数据，支持「前缀 + 版本后缀」匹配
// （如 gpt-4.1-2025-04-14 命中 gpt-4.1）。找不到返回 ok=false。
func LookupModel(id string) (ModelInfo, bool) {
	if id == "" {
		return ModelInfo{}, false
	}
	lid := strings.ToLower(id)

	modelMu.RLock()
	custom := append([]ModelInfo(nil), modelCustom...)
	modelMu.RUnlock()

	best := ModelInfo{}
	found := false
	consider := func(m ModelInfo) {
		mid := strings.ToLower(m.ID)
		if lid == mid || strings.HasPrefix(lid, mid+"-") || strings.HasPrefix(lid, mid+":") {
			// 取最长匹配，避免 "gpt-4" 抢走 "gpt-4.1"。
			if !found || len(m.ID) > len(best.ID) {
				best, found = m, true
			}
		}
	}
	for _, m := range custom {
		consider(m)
	}
	for _, m := range builtinModels {
		consider(m)
	}
	return best, found
}

// ResolveModel 组装 agent.Model。元数据缺失时给出保守默认（128K 窗口）。
func ResolveModel(providerName, id string) agent.Model {
	m := agent.Model{Provider: providerName, ID: id}
	if info, ok := LookupModel(id); ok {
		m.ContextWindow = info.ContextWindow
		m.MaxOutputTokens = info.MaxOutput
		m.SupportsImages = info.SupportsImages
		m.SupportsThinking = info.SupportsThinking
	}
	if m.ContextWindow <= 0 {
		m.ContextWindow = 128_000
	}
	return m
}

// ApplyCost 按价格表填充 msg.Usage.Cost。未登记的模型保持零价。
//
// 按时段浮动的服务商会根据**调用结束时刻**选价（DeepSeek 的高峰/空闲）。
// 因此同一批消息在不同时间重算可能得到不同成本——这是真实计费规则，不是 bug。
func ApplyCost(msg *agent.AssistantMessage, model agent.Model) {
	if msg == nil {
		return
	}
	if msg.ResponseModel != "" && msg.ResponseModel != model.ID {
		if _, ok := LookupModel(msg.ResponseModel); ok {
			model.ID = msg.ResponseModel
		}
	}
	info, ok := LookupModel(model.ID)
	if !ok {
		return
	}

	in, out, cacheRead := info.Input, info.Output, info.CacheRead
	if info.InputOffPeak > 0 && !peakNow(info.Currency) {
		in, out, cacheRead = info.InputOffPeak, info.OutputOffPeak, info.CacheReadOffPeak
	}

	const perMillion = 1_000_000.0
	u := &msg.Usage
	// CacheRead/CacheWrite 是 Input 的子集：要把它们从全价部分扣掉。
	plainInput := u.Input - u.CacheRead - u.CacheWrite
	if plainInput < 0 {
		plainInput = 0
	}
	u.Cost = agent.Cost{
		Currency:   info.Currency,
		Input:      float64(plainInput) / perMillion * in,
		Output:     float64(u.Output) / perMillion * out,
		CacheRead:  float64(u.CacheRead) / perMillion * cacheRead,
		CacheWrite: float64(u.CacheWrite) / perMillion * info.CacheWrite,
	}
	u.Cost.Total = u.Cost.Input + u.Cost.Output + u.Cost.CacheRead + u.Cost.CacheWrite
}

// nowFunc 便于测试固定时间点。
var nowFunc = time.Now

// deepSeekHolidays 是中国法定节假日（北京时间，YYYY-MM-DD），节假日全天空闲价。
// 默认不含任何日期：法定节假日每年变化，接入方按需填充（公共假期不连坐调休）。
var deepSeekHolidays = map[string]bool{}

// peakNow 判断当前是否处于高峰（标准）价时段。
// 目前只有按人民币计价的 DeepSeek 采用浮动定价。
func peakNow(currency string) bool {
	if currency != "CNY" {
		return true
	}
	bj := nowFunc().In(time.FixedZone("CST", 8*60*60))
	switch bj.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	if deepSeekHolidays[bj.Format("2006-01-02")] {
		return false
	}
	h := bj.Hour()
	return (h >= 9 && h < 12) || (h >= 14 && h < 18)
}
