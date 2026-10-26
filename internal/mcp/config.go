package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout 是单次 MCP 请求的默认超时。
const DefaultTimeout = 60 * time.Second

// Config 是 mcp.json 的顶层结构，格式与 Claude Desktop / Pi 兼容：
//
//	{
//	  "mcpServers": {
//	    "filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]},
//	    "docs": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
//	  }
//	}
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`

	// SourceFiles 记录实际读到的配置文件（不存在的路径不计入），仅用于诊断。
	SourceFiles []string `json:"-"`
}

// ServerConfig 是单个 MCP server 的配置。command 决定 stdio，url 决定 HTTP。
type ServerConfig struct {
	// stdio
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`

	// HTTP
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// 通用
	Type        string `json:"type,omitempty"`        // stdio | http | streamable-http
	Timeout     int    `json:"timeout,omitempty"`     // 秒
	Enabled     *bool  `json:"enabled,omitempty"`     // 默认 true
	Description string `json:"description,omitempty"` //
	// Exposure 控制工具如何暴露：direct（默认，直接给模型）| hidden（注册但不给模型）。
	//
	// 其他取值（例如 Pi 的 "codemode"）按 direct 处理：配置文件要能跨工具复用，
	// 遇到本程序不认识的扩展值时应降级而不是拒绝启动。
	Exposure string `json:"exposure,omitempty"`
}

// IsEnabled 报告该 server 是否应被连接。
func (c ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// IsHTTP 报告是否使用 HTTP 传输。
func (c ServerConfig) IsHTTP() bool {
	if c.URL != "" {
		return true
	}
	switch c.Type {
	case "http", "streamable-http":
		return true
	}
	return false
}

func (c ServerConfig) requestTimeout() time.Duration {
	if c.Timeout > 0 {
		return time.Duration(c.Timeout) * time.Second
	}
	return DefaultTimeout
}

// Validate 检查配置自洽性。返回的错误包含 server 名。
func (c ServerConfig) Validate(name string) error {
	if !validServerName(name) {
		return fmt.Errorf("mcp: server 名 %q 只能包含字母、数字、_ 和 -", name)
	}
	if c.Type != "" && c.Type != "stdio" && c.Type != "http" && c.Type != "streamable-http" {
		return fmt.Errorf("mcp: server %q 的 type=%q 不支持（stdio|http|streamable-http）", name, c.Type)
	}
	switch {
	case c.Command != "" && c.URL != "":
		return fmt.Errorf("mcp: server %q 同时给了 command 和 url", name)
	case c.Command == "" && c.URL == "":
		return fmt.Errorf("mcp: server %q 需要 command（stdio）或 url（http）", name)
	case c.IsHTTP() && c.URL == "":
		return fmt.Errorf("mcp: server %q 声明为 http 但缺少 url", name)
	case !c.IsHTTP() && c.Command == "":
		return fmt.Errorf("mcp: server %q 声明为 stdio 但缺少 command", name)
	}
	return nil
}

func validServerName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Load 从文件读取配置。文件不存在时返回空配置（不报错）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{MCPServers: map[string]ServerConfig{}}, nil
		}
		return nil, fmt.Errorf("mcp: 读取 %s: %w", path, err)
	}
	cfg, err := parseJSON(data)
	if err != nil {
		return nil, fmt.Errorf("mcp: 解析 %s: %w", path, err)
	}
	return cfg, nil
}

// LoadJSON 解析内联配置字符串，格式与 mcp.json 完全一致。
//
// 额外放宽一处：允许省略外层的 {"mcpServers": {...}} 包装，直接给出
// server 名到配置的映射（{"fs":{"command":"npx"}}）。命令行临时注入时
// 少两层括号，写错的概率也低。
func LoadJSON(data []byte) (*Config, error) {
	cfg, err := parseJSON(data)
	if err != nil {
		return nil, fmt.Errorf("mcp: 解析内联配置: %w", err)
	}
	return cfg, nil
}

// parseJSON 解析配置 JSON。错误不带来源前缀，由调用方补齐（文件名 / 内联）。
func parseJSON(data []byte) (*Config, error) {
	cfg := &Config{MCPServers: map[string]ServerConfig{}}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}

	// 有 mcpServers 键就按完整文件解析，否则按裸映射解析。
	// 不能只看「解析成 Config 后是否为空」：{"fs":{...}} 解析成 Config 会
	// 静默丢掉 fs（未知字段），看起来像是配置没生效。
	var probe map[string]json.RawMessage
	wrapped := false
	if err := json.Unmarshal(data, &probe); err == nil {
		_, wrapped = probe["mcpServers"]
	}

	if wrapped {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(data, &cfg.MCPServers); err != nil {
		return nil, fmt.Errorf("%w（应为 {\"mcpServers\":{...}} 或 {\"<server 名>\":{...}}）", err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	cfg.expand()
	return cfg, nil
}

// Merge 把 other 的 server 并入 c，同名覆盖（与 LoadFiles 的后者优先一致）。
// SourceFiles 不合并：调用方通常只关心文件来源。
func (c *Config) Merge(other *Config) {
	if other == nil {
		return
	}
	if c.MCPServers == nil {
		c.MCPServers = map[string]ServerConfig{}
	}
	for name, sc := range other.MCPServers {
		c.MCPServers[name] = sc
	}
}

// LoadFiles 依次读取多个文件，后面的覆盖前面的同名 server。
// 不存在的路径静默跳过，实际读到的文件记在 SourceFiles。
func LoadFiles(paths ...string) (*Config, error) {
	merged := &Config{MCPServers: map[string]ServerConfig{}}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("mcp: 检查 %s: %w", p, err)
		}
		cfg, err := Load(p)
		if err != nil {
			return nil, err
		}
		merged.SourceFiles = append(merged.SourceFiles, p)
		merged.Merge(cfg)
	}
	return merged, nil
}

// DefaultPaths 返回默认搜索的 mcp 配置文件，靠后的覆盖靠前的同名 server：
//
//  1. <workDir>/.mcp.json    —— 项目级，可随仓库分发
//  2. <configDir>/.mcp.json  —— 用户级，跨项目共享
//
// 要复用其他客户端的配置（例如 ~/.pi/agent/mcp.json）请用 --mcp-config 显式指定。
func DefaultPaths(workDir string) []string {
	var out []string
	if workDir != "" {
		out = append(out, filepath.Join(workDir, ".mcp.json"))
	}
	if dir := ConfigDir(); dir != "" {
		out = append(out, filepath.Join(dir, ".mcp.json"))
	}
	return out
}

// ConfigDir 返回 acp-agent 的配置目录：ACP_AGENT_CONFIG_DIR 优先，
// 否则用系统用户配置目录下的 acp-agent
// （Windows: %AppData%\acp-agent；Linux/macOS: ~/.config/acp-agent）。
//
// 该目录同时存放 config.json、.mcp.json 与 sessions/。
func ConfigDir() string {
	if p := os.Getenv("ACP_AGENT_CONFIG_DIR"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "acp-agent")
}

// expand 展开 ${VAR} / $VAR，并规范化相对 cwd。
func (c *Config) expand() {
	for name, sc := range c.MCPServers {
		sc.Command = expandEnv(sc.Command)
		for i := range sc.Args {
			sc.Args[i] = expandEnv(sc.Args[i])
		}
		sc.Cwd = expandEnv(sc.Cwd)
		sc.URL = expandEnv(sc.URL)
		if sc.Cwd != "" {
			sc.Cwd = expandHome(sc.Cwd)
		}
		if len(sc.Env) > 0 {
			env := make(map[string]string, len(sc.Env))
			for k, v := range sc.Env {
				env[k] = expandEnv(v)
			}
			sc.Env = env
		}
		if len(sc.Headers) > 0 {
			h := make(map[string]string, len(sc.Headers))
			for k, v := range sc.Headers {
				h[k] = expandEnv(v)
			}
			sc.Headers = h
		}
		c.MCPServers[name] = sc
	}
}

func expandEnv(s string) string {
	if s == "" || !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, os.Getenv)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Names 返回已配置的 server 名（排序，便于稳定输出）。
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.MCPServers))
	for name := range c.MCPServers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Validate 校验所有条目，返回首个错误。
func (c *Config) Validate() error {
	for _, name := range c.Names() {
		if err := c.MCPServers[name].Validate(name); err != nil {
			return err
		}
	}
	return nil
}

// IsHidden 报告该 server 的工具是否应从模型视野中隐藏。
func (c ServerConfig) IsHidden() bool { return c.Exposure == "hidden" }

// classifyTransport 返回人类可读的传输类型，供 CLI 展示。
func classifyTransport(c ServerConfig) string {
	if c.IsHTTP() {
		return "http"
	}
	return "stdio"
}
