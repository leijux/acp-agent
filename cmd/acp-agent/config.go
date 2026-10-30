package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/mcp"
	"github.com/leijux/acp-agent/internal/provider"
	"github.com/leijux/acp-agent/internal/web"
)

// Config 是落盘的配置。字段为空时由默认值或环境变量补齐。
//
// 优先级：命令行 flag > 环境变量 > 配置文件 > 内置默认。
type Config struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	BaseURL     string  `json:"baseURL,omitempty"`
	APIKey      string  `json:"apiKey,omitempty"`
	Permissions string  `json:"permissions,omitempty"` // ask | auto | readonly
	MaxTurns    int     `json:"maxTurns,omitempty"`
	// maxTurnsSet 记录轮数上限是否由调用方显式指定（flag / env）。
	// 区分「没给」（补默认 500）与「显式 0/负数」（不限制）——光看值区分不了，
	// 而这两种语义相反。非导出字段，不参与 JSON 解码。
	maxTurnsSet bool
	Root        string  `json:"root,omitempty"`
	Session     string  `json:"session,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	// ThinkingLevel: off | minimal | low | medium | high | max；空 = medium（默认）
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	// DisableTools 不注册的工具名。
	DisableTools []string `json:"disableTools,omitempty"`
	// Subagent 控制 delegate 工具如何启动子代理（默认自身 + --acp）。
	Subagent *SubagentConfig `json:"subagent,omitempty"`
	// NoMCP 为真时不连接任何 MCP server（子代理默认开启，避免背着主进程的远程工具）。
	NoMCP bool `json:"noMCP,omitempty"`
	// MCPConfig 是 MCP server 配置文件的路径；为空时按默认位置查找。
	// 也接受内联 JSON 字符串（以 { 开头）—— 语义与路径一致：只用它，不合并默认文件。
	MCPConfig string `json:"mcpConfig,omitempty"`
	// MCPJSON 是内联 MCP 配置 JSON，叠加在上面那层配置之上（同名 server 覆盖）。
	// 主要用于临时注入：不改 .mcp.json 也能多挂一个 server。
	MCPJSON string `json:"mcpJSON,omitempty"`
	// Web 配置 web_search / web_fetch 的后端（缺省用 DuckDuckGo，无需 key）。
	Web *web.Config `json:"web,omitempty"`
	// Segmenter 选中文分词器：空/gse（默认，真词典）/ rune（单字 + bigram 回退）。
	// gse 只带简体词表，需要带 `segmenter ne` tag 的构建（`task build:full`），
	// 二进制比轻量版多约 1.4MB。没有命令行开关：默认就是 gse，要降级只能靠这里。
	Segmenter string `json:"segmenter,omitempty"`
	// SegmenterExpand 控制 gse 是否额外输出 CJK run 内的 bigram（默认开）。
	// off 会让 kernel 的 cjkRunTokens 丢掉单字与跨词边界的召回。
	SegmenterExpand string `json:"segmenterExpand,omitempty"` // "" | on | off

	// JSONMode 由 --mode json 打开：stdout 只输出 Pi 兼容的 JSON 事件流。
	// 只存在于本次运行，不落盘。
	JSONMode bool `json:"-"`
	// NoSession 由 --no-session 打开：不读也不写会话文件。
	NoSession bool `json:"-"`
	// JSONOut 是事件流的出口；为空时用 os.Stdout（测试用）。
	JSONOut io.Writer `json:"-"`
	// NoContextFiles 由 --no-context-files 打开：不注入 AGENTS.md 等上下文文件。
	NoContextFiles bool `json:"-"`

	// Skills 是额外的技能发现目录（相对 root 或绝对路径）。
	// 默认位置永远是规范里的 .agents/skills（项目级 + 用户级），这里只追加。
	Skills []string `json:"skills,omitempty"`
	// DisableSkills 关闭 .agents/skills 发现（--no-skills / disableSkills）。
	DisableSkills bool `json:"disableSkills,omitempty"`

	// ExtensionFiles 记录本次从 --extension 里真的取到 mcpServers 的文件，
	// 只用于 -mcp-status 的来源显示（合并结果写进 MCPJSON 后就分不清来源了）。
	ExtensionFiles []string `json:"-"`
	// hadInlineJSON 记录 --mcp-json 是否由调用方给出：mergePiExtensions 会把
	// 扩展合并进 MCPJSON，事后再看 MCPJSON 无法区分「用户传的」与「扩展来的」。
	hadInlineJSON bool
}

const (
	permAsk      = "ask"
	permAuto     = "auto"
	permReadonly = "readonly"

	// defaultMaxTurns 是 CLI 层的默认轮数兜底。
	//
	// agent 包本身不设上限（internal/agent 的 MaxTurns<=0 = 不限制，与上游 Pi
	// 的 agent loop 一致）；这里是产品默认：交互/编排场景没人看着时不能让它
	// 无限跑。想彻底不限就显式给 0 或负数（见 applyDefaults）。
	defaultMaxTurns = 500
)

// ConfigPath 返回配置文件路径（配置目录下的 config.json，遵循 XDG）。
func ConfigPath() string {
	if p := os.Getenv("ACP_AGENT_CONFIG"); p != "" {
		return p
	}
	dir := mcp.ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.json")
}

// DataDir 返回会话等数据目录（与 config.json、.mcp.json 同目录）。
func DataDir() string {
	if p := os.Getenv("ACP_AGENT_DATA"); p != "" {
		return p
	}
	dir := mcp.ConfigDir()
	if dir == "" {
		return filepath.Join(os.TempDir(), "acp-agent")
	}
	return dir
}

// LoadConfig 读配置文件；文件不存在不算错误。
func LoadConfig() (Config, error) {
	var cfg Config
	path := ConfigPath()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("读配置 %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return cfg, nil
}

// applyEnv 应用环境变量。优先级：命令行 flag > 环境变量 > 配置文件 > 内置默认。
// 调用时机：读完配置文件之后、应用命令行 flag 之前。
func (c *Config) applyEnv() {
	if v := os.Getenv("ACP_AGENT_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := os.Getenv("ACP_AGENT_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("ACP_AGENT_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("ACP_AGENT_PERMISSIONS"); v != "" {
		c.Permissions = v
	}
	if v := os.Getenv("ACP_AGENT_ROOT"); v != "" {
		c.Root = v
	}
	if v := os.Getenv("ACP_AGENT_SESSION"); v != "" {
		c.Session = v
	}
	if v := os.Getenv("ACP_AGENT_THINKING"); v != "" {
		c.ThinkingLevel = v
	}
	if v := os.Getenv("ACP_AGENT_MCP_JSON"); v != "" {
		c.MCPJSON = v
	}
	if v := os.Getenv("ACP_AGENT_MAX_TURNS"); v != "" {
		// 0 / 负数表示不限制，所以要接 != 0 而不仅是 > 0。
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			c.MaxTurns = n
			c.maxTurnsSet = true
		}
	}
	if f, ok := envFloat("ACP_AGENT_TEMPERATURE"); ok {
		c.Temperature = f
	}
	if v := os.Getenv("ACP_AGENT_SEGMENTER"); v != "" {
		c.Segmenter = v
	}
	if v := os.Getenv("ACP_AGENT_SEGMENTER_EXPAND"); v != "" {
		c.SegmenterExpand = v
	}
	c.applyWebEnv()
}

// applyWebEnv 应用 web 相关环境变量。
//
// 除了 ACP_AGENT_WEB_* 这组通用变量，也认各家自己的 key 变量
// （TAVILY_API_KEY / BRAVE_API_KEY / EXA_API_KEY），方便直接复用现成环境。
func (c *Config) applyWebEnv() {
	c.ensureWeb()
	if v := os.Getenv("ACP_AGENT_WEB_SEARCH_PROVIDER"); v != "" {
		c.Web.Search.Provider = v
	}
	if v := os.Getenv("ACP_AGENT_WEB_SEARCH_BASE_URL"); v != "" {
		c.Web.Search.BaseURL = v
	}
	if v := os.Getenv("ACP_AGENT_WEB_SEARCH_API_KEY"); v != "" {
		c.Web.Search.APIKey = v
	}
	if c.Web.Search.APIKey == "" {
		for _, name := range []string{"TAVILY_API_KEY", "BRAVE_API_KEY", "EXA_API_KEY"} {
			if v := os.Getenv(name); v != "" {
				c.Web.Search.APIKey = v
				break
			}
		}
	}
	if os.Getenv("ACP_AGENT_WEB_OFF") != "" {
		c.Web.Search.Provider = web.ProviderOff
	}
}

// ensureWeb 保证 c.Web 可用。
func (c *Config) ensureWeb() {
	if c.Web == nil {
		c.Web = &web.Config{}
	}
}

// applyFlags 应用命令行 flag（优先级最高）。
func (c *Config) applyFlags(f *flags) {
	if f.provider != "" {
		c.Provider = f.provider
		c.Model = "" // 换 provider 后旧模型名通常无效
	}
	if f.model != "" {
		c.Model = f.model
	}
	if f.baseURL != "" {
		c.BaseURL = f.baseURL
	}
	if f.permissions != "" {
		c.Permissions = f.permissions
	}
	if f.root != "" {
		c.Root = f.root
	}
	if f.session != "" {
		c.Session = f.session
	}
	if f.maxTurnsSet {
		c.MaxTurns = f.maxTurns
		c.maxTurnsSet = true
	}
	if f.thinking != "" {
		c.ThinkingLevel = f.thinking
	}
	if f.temperatureSet {
		c.Temperature = f.temperature
	}
	if f.mcpConfig != "" {
		c.MCPConfig = f.mcpConfig
	}
	if f.mcpJSON != "" {
		c.MCPJSON = f.mcpJSON
	}
	if f.noMCP {
		c.NoMCP = true
	}
	if f.webSearch != "" {
		c.ensureWeb()
		c.Web.Search.Provider = f.webSearch
	}
	if f.noWeb {
		c.ensureWeb()
		c.Web.Search.Provider = web.ProviderOff
	}
	if f.webSearchKey != "" {
		c.ensureWeb()
		c.Web.Search.APIKey = f.webSearchKey
	}
	if f.disableTools != "" {
		for _, name := range strings.Split(f.disableTools, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.DisableTools = append(c.DisableTools, name)
			}
		}
	}
	// 工具全关：DisableTools 里的 "*" 由 tools.DefaultRegistry 解释成「跳过全部」。
	if f.noBuiltinTools || f.noTools {
		c.DisableTools = append(c.DisableTools, "*")
	}
	if f.noTools {
		// Pi 的 --no-tools 连 MCP 也不连；--no-builtin-tools 只关内建。
		c.NoMCP = true
	}
	// 子代理禁用 / 指定命令。两者都写入 Subagent，方便配置与 flag 统一处理。
	if f.noSubagent {
		c.DisableTools = append(c.DisableTools, "delegate")
	}
	if f.subagentCmd != "" {
		sub := SubagentConfig{Command: f.subagentCmd}
		if c.Subagent != nil {
			sub = *c.Subagent
			sub.Command = f.subagentCmd
		}
		c.Subagent = &sub
	}
}

// applyDefaults 补默认值。
func (c *Config) applyDefaults() {
	if c.Provider == "" {
		c.Provider = "deepseek"
	}
	if c.Permissions == "" {
		c.Permissions = permAsk
	}
	if c.MaxTurns < 0 {
		// 负数 = 不限制（文件里写 "maxTurns": -1；flag/env 用 -1）。
		// 规整成 0 交给 agent：agent 包的 <=0 就是不限。
		c.MaxTurns = 0
	} else if c.MaxTurns == 0 && !c.maxTurnsSet {
		c.MaxTurns = defaultMaxTurns
	}
	if c.Root == "" {
		if wd, err := os.Getwd(); err == nil {
			c.Root = wd
		} else {
			c.Root = "."
		}
	}
	if c.Model == "" {
		if ep, ok := provider.LookupEndpoint(c.Provider); ok {
			c.Model = ep.DefaultModel
		}
	}
	if c.ThinkingLevel == "" {
		// 默认 medium，而不是 off：这是个编码 agent，大多数改动都需要先想清楚再
		// 动手，静默关闭推理是反直觉的。取 medium 而不是 high：DeepSeek 侧
		// medium/high 映射到同一个 reasoning_effort=high（服务端默认也是 high），
		// medium 与你今后上调时保持单调。
		//
		// 不留给空串（= 不干预，交给服务端默认）：那可依赖端点默认值，
		// DeepSeek 现在默认开、以后不一定；显式给值才可控。
		// 想关掉仍然是 --thinking off / thinkingLevel=off。
		c.ThinkingLevel = "medium"
	}
	c.ensureWeb()
	if c.Web.Search.Provider == "" {
		c.Web.Search.Provider = web.ProviderDuckDuckGo
	}
}

// resolveAPIKey 按 flag > 配置 > 环境变量 取 key。
func resolveAPIKey(cfg Config, flagKey string) string {
	if flagKey != "" {
		return flagKey
	}
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	if k := os.Getenv("ACP_AGENT_API_KEY"); k != "" {
		return k
	}
	if ep, ok := provider.LookupEndpoint(cfg.Provider); ok && ep.EnvKey != "" {
		return os.Getenv(ep.EnvKey)
	}
	return ""
}

// buildProvider 依据配置组装 Provider 与 Model。
//
// 返回的 Provider 已经带上重试：429/5xx/网络错误会退避重试，
// 但一旦开始输出增量就不再重试（避免重复的半截回复）。
func buildProvider(cfg Config, apiKey string) (agent.Provider, agent.Model, error) {
	ep, ok := provider.LookupEndpoint(cfg.Provider)
	if !ok {
		var names []string
		for _, e := range provider.Endpoints {
			names = append(names, e.Name)
		}
		return nil, agent.Model{}, fmt.Errorf("未知 provider %q，可选：%s", cfg.Provider, strings.Join(names, ", "))
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = ep.BaseURL
	}
	if ep.EnvKey != "" && apiKey == "" && !isLocalURL(baseURL) {
		return nil, agent.Model{}, fmt.Errorf(
			"缺少 API key：设置 %s 环境变量，或用 --api-key 指定", ep.EnvKey)
	}

	base := provider.NewOpenAI(provider.OpenAIConfig{
		BaseURL:       baseURL,
		APIKey:        apiKey,
		NoStreamUsage: ep.NoStreamUsage,
		Headers:       ep.Headers,
		Kind:          ep.Kind,
	})

	modelID := cfg.Model
	if modelID == "" {
		modelID = ep.DefaultModel
	}
	model := ep.ModelFor(modelID)
	if model.ContextWindow <= 0 {
		model.ContextWindow = 128_000
	}
	p := provider.Retry(base, provider.RetryOptions{MaxAttempts: 4})
	return p, model, nil
}

func isLocalURL(u string) bool {
	return strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost") ||
		strings.Contains(u, "0.0.0.0") || strings.Contains(u, "::1")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envFloat 读可选浮点环境变量。
func envFloat(key string) (float64, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
