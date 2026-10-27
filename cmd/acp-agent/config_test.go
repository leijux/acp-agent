package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leijux/acp-agent/internal/web"
)

// 写一个配置文件并让 LoadConfig 指向它。
func useConfigFile(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACP_AGENT_CONFIG", path)
}

func TestConfigPriorityFileEnvFlag(t *testing.T) {
	useConfigFile(t, `{
		"provider": "openai",
		"model": "gpt-4.1",
		"baseURL": "https://from-file/v1",
		"permissions": "readonly",
		"maxTurns": 5,
		"root": "/file/root"
	}`)
	t.Setenv("ACP_AGENT_MODEL", "from-env")
	t.Setenv("ACP_AGENT_BASE_URL", "https://from-env/v1")
	t.Setenv("ACP_AGENT_PERMISSIONS", "auto")
	t.Setenv("ACP_AGENT_MAX_TURNS", "42")
	t.Setenv("ACP_AGENT_ROOT", "/env/root")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyEnv()

	// 环境变量覆盖配置文件
	if cfg.Model != "from-env" || cfg.BaseURL != "https://from-env/v1" ||
		cfg.Permissions != "auto" || cfg.MaxTurns != 42 || cfg.Root != "/env/root" {
		t.Fatalf("环境变量未覆盖配置文件: %+v", cfg)
	}

	// flag 覆盖环境变量
	f := &flags{model: "from-flag", permissions: "readonly", root: "/flag/root", maxTurns: 3, maxTurnsSet: true}
	cfg.applyFlags(f)
	if cfg.Model != "from-flag" || cfg.Permissions != "readonly" ||
		cfg.Root != "/flag/root" || cfg.MaxTurns != 3 {
		t.Fatalf("flag 未覆盖环境变量: %+v", cfg)
	}
	// 未指定的字段保留 env 值
	if cfg.BaseURL != "https://from-env/v1" {
		t.Fatalf("未指定的 baseURL 被改写: %q", cfg.BaseURL)
	}
}

func TestConfigEnvProviderAndThinking(t *testing.T) {
	useConfigFile(t, `{"provider":"openai","thinkingLevel":"low"}`)
	t.Setenv("ACP_AGENT_PROVIDER", "ollama")
	t.Setenv("ACP_AGENT_THINKING", "high")
	t.Setenv("ACP_AGENT_SESSION", "/tmp/s.jsonl")
	t.Setenv("ACP_AGENT_TEMPERATURE", "0.35")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyEnv()
	cfg.applyDefaults()

	if cfg.Provider != "ollama" {
		t.Fatalf("provider = %q", cfg.Provider)
	}
	if cfg.ThinkingLevel != "high" {
		t.Fatalf("thinkingLevel = %q", cfg.ThinkingLevel)
	}
	if cfg.Session != "/tmp/s.jsonl" {
		t.Fatalf("session = %q", cfg.Session)
	}
	if cfg.Temperature != 0.35 {
		t.Fatalf("temperature = %v", cfg.Temperature)
	}
}

// 非法数值不应把配置弄坏，静默忽略并退回配置文件/默认值。
func TestConfigEnvInvalidNumbersIgnored(t *testing.T) {
	useConfigFile(t, `{"maxTurns": 9}`)
	t.Setenv("ACP_AGENT_MAX_TURNS", "abc")
	t.Setenv("ACP_AGENT_TEMPERATURE", "hot")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyEnv()
	cfg.applyDefaults()

	if cfg.MaxTurns != 9 {
		t.Fatalf("非法 maxTurns 污染了配置: %d", cfg.MaxTurns)
	}
	if cfg.Temperature != 0 {
		t.Fatalf("非法 temperature 污染了配置: %v", cfg.Temperature)
	}
}

func TestConfigDefaultsWhenNothingSet(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, k := range []string{
		"ACP_AGENT_PROVIDER", "ACP_AGENT_MODEL", "ACP_AGENT_BASE_URL", "ACP_AGENT_PERMISSIONS",
		"ACP_AGENT_ROOT", "ACP_AGENT_SESSION", "ACP_AGENT_THINKING", "ACP_AGENT_MAX_TURNS",
		"ACP_AGENT_TEMPERATURE", "ACP_AGENT_WEB_SEARCH_PROVIDER", "ACP_AGENT_WEB_SEARCH_API_KEY",
		"ACP_AGENT_WEB_SEARCH_BASE_URL", "ACP_AGENT_WEB_OFF", "TAVILY_API_KEY", "BRAVE_API_KEY", "EXA_API_KEY",
	} {
		t.Setenv(k, "")
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyEnv()
	cfg.applyDefaults()

	if cfg.Provider != "deepseek" {
		t.Fatalf("默认 provider = %q", cfg.Provider)
	}
	if cfg.Permissions != permAsk || cfg.MaxTurns != defaultMaxTurns || cfg.ThinkingLevel != "medium" {
		t.Fatalf("默认值不对: %+v", cfg)
	}
	if cfg.Root == "" {
		t.Fatal("root 应回退到 cwd")
	}
	if cfg.Web == nil || cfg.Web.Search.Provider != web.ProviderDuckDuckGo {
		t.Fatalf("web 后端应默认 duckduckgo：%+v", cfg.Web)
	}
}

// 轮数上限：默认 500 兜底，但 0 / 负数表示「不限制」。
//
// 只看值区分不了「没给」与「显式不限」（两者都是 0 附近的值），
// 所以 flag/env 另外记了一个「显式设置」标记，这里是那个约定的回归点。
func TestConfigMaxTurnsUnlimited(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		fl   *flags
		want int
	}{
		{"未设置", `{}`, "", nil, defaultMaxTurns},
		{"flag 正数", `{}`, "", &flags{maxTurns: 20, maxTurnsSet: true}, 20},
		{"flag 0", `{}`, "", &flags{maxTurns: 0, maxTurnsSet: true}, 0},
		{"flag 负数", `{}`, "", &flags{maxTurns: -1, maxTurnsSet: true}, 0},
		{"env 正数", `{}`, "50", nil, 50},
		{"env 负数", `{}`, "-1", nil, 0},
		{"文件正数", `{"maxTurns": 20}`, "", nil, 20},
		{"文件负数", `{"maxTurns": -1}`, "", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useConfigFile(t, tc.file)
			t.Setenv("ACP_AGENT_MAX_TURNS", tc.env)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.applyEnv()
			if tc.fl != nil {
				cfg.applyFlags(tc.fl)
			}
			cfg.applyDefaults()
			if cfg.MaxTurns != tc.want {
				t.Fatalf("MaxTurns = %d, want %d", cfg.MaxTurns, tc.want)
			}
		})
	}
}

// web 配置的优先级：file < env < flag，off 能关掉。
func TestWebConfigPriority(t *testing.T) {
	t.Setenv("ACP_AGENT_WEB_SEARCH_PROVIDER", "searxng")
	t.Setenv("ACP_AGENT_WEB_SEARCH_BASE_URL", "https://sx.example")
	t.Setenv("ACP_AGENT_WEB_SEARCH_API_KEY", "")

	cfg := Config{Web: &web.Config{Search: web.SearchConfig{Provider: "tavily", APIKey: "from-file"}}}
	cfg.applyEnv()
	if cfg.Web.Search.Provider != "searxng" || cfg.Web.Search.BaseURL != "https://sx.example" {
		t.Fatalf("env 未生效：%+v", cfg.Web.Search)
	}
	if cfg.Web.Search.APIKey != "from-file" {
		t.Fatalf("空 env 不应清掉文件里的 key：%q", cfg.Web.Search.APIKey)
	}

	// flag 压过 env。
	cfg.applyFlags(&flags{webSearch: "exa", webSearchKey: "ek"})
	if cfg.Web.Search.Provider != "exa" || cfg.Web.Search.APIKey != "ek" {
		t.Fatalf("flag 未生效：%+v", cfg.Web.Search)
	}

	// --no-web 压过一切。
	cfg.applyFlags(&flags{noWeb: true})
	if cfg.Web.Search.Provider != web.ProviderOff {
		t.Fatalf("off 未生效：%q", cfg.Web.Search.Provider)
	}
}

// 第三方 key 环境变量能直接复用。
func TestWebConfigProviderKeyEnv(t *testing.T) {
	for _, k := range []string{"ACP_AGENT_WEB_SEARCH_API_KEY", "BRAVE_API_KEY", "EXA_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("TAVILY_API_KEY", "tvly-from-env")
	cfg := Config{}
	cfg.applyEnv()
	if cfg.Web.Search.APIKey != "tvly-from-env" {
		t.Fatalf("key = %q", cfg.Web.Search.APIKey)
	}
}

// DataDir / ConfigPath 的环境变量覆盖。
func TestDataAndConfigPathEnv(t *testing.T) {
	t.Setenv("ACP_AGENT_DATA", "/tmp/dd")
	if got := DataDir(); got != "/tmp/dd" {
		t.Fatalf("DataDir() = %q", got)
	}
	t.Setenv("ACP_AGENT_CONFIG", "/tmp/cc.json")
	if got := ConfigPath(); got != "/tmp/cc.json" {
		t.Fatalf("ConfigPath() = %q", got)
	}
}
