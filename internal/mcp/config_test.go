package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/tools"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "s3cret")
	cases := map[string]string{
		"${MCP_TEST_TOKEN}":        "s3cret",
		"Bearer ${MCP_TEST_TOKEN}": "Bearer s3cret",
		"$MCP_TEST_TOKEN":          "s3cret",
		"${MCP_MISSING_VAR}":       "",
		"plain":                    "plain",
	}
	for in, want := range cases {
		if got := expandEnv(in); got != want {
			t.Errorf("expandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadAndExpand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCP_TEST_BASE", dir)
	path := filepath.Join(dir, "mcp.json")
	content := `{
	  "mcpServers": {
	    "fs": {"command": "node", "args": ["${MCP_TEST_BASE}/srv.js"], "env": {"TOKEN": "${MCP_TEST_BASE}"}},
	    "web": {"url": "https://x.example/mcp", "headers": {"Authorization": "Bearer ${MCP_TEST_BASE}"}},
	    "off": {"command": "x", "enabled": false}
	  }
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MCPServers) != 3 {
		t.Fatalf("servers = %d, want 3", len(cfg.MCPServers))
	}
	if got := cfg.MCPServers["fs"].Args[0]; got != dir+"/srv.js" {
		t.Errorf("args[0] = %q，环境变量未展开", got)
	}
	if got := cfg.MCPServers["web"].Headers["Authorization"]; got != "Bearer "+dir {
		t.Errorf("header = %q", got)
	}
	if cfg.MCPServers["off"].IsEnabled() {
		t.Error("enabled=false 仍被视为启用")
	}
	if !cfg.MCPServers["web"].IsHTTP() {
		t.Error("有 url 应判定为 HTTP")
	}

	if got := cfg.Names(); len(got) != 3 || got[0] != "fs" {
		t.Errorf("Names() = %v（应排序）", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("不存在的文件不该报错: %v", err)
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("servers = %d", len(cfg.MCPServers))
	}
}

func TestLoadFilesOverride(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	_ = os.WriteFile(a, []byte(`{"mcpServers":{"x":{"command":"old"},"y":{"command":"keep"}}}`), 0o600)
	_ = os.WriteFile(b, []byte(`{"mcpServers":{"x":{"command":"new"}}}`), 0o600)

	cfg, err := LoadFiles(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["x"].Command != "new" {
		t.Errorf("后者应覆盖前者: %q", cfg.MCPServers["x"].Command)
	}
	if cfg.MCPServers["y"].Command != "keep" {
		t.Errorf("未覆盖的条目丢失: %+v", cfg.MCPServers)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		expect string
	}{
		{
			name:   "无 command 也无 url",
			cfg:    Config{MCPServers: map[string]ServerConfig{"a": {}}},
			expect: "command",
		},
		{
			name:   "非法 server 名",
			cfg:    Config{MCPServers: map[string]ServerConfig{"a b": {Command: "x"}}},
			expect: "server 名",
		},
		{
			name:   "未知 type",
			cfg:    Config{MCPServers: map[string]ServerConfig{"a": {Command: "x", Type: "grpc"}}},
			expect: "type=",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("期望错误")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.expect) {
				t.Errorf("错误信息 %q 不含 %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestRequestTimeout(t *testing.T) {
	if got := (ServerConfig{}).requestTimeout(); got != DefaultTimeout {
		t.Errorf("默认 = %v, want %v", got, DefaultTimeout)
	}
	if got := (ServerConfig{Timeout: 5}).requestTimeout(); got != 5*time.Second {
		t.Errorf("= %v, want 5s", got)
	}
}

func TestLoadJSONWrappedAndBare(t *testing.T) {
	wrapped, err := LoadJSON([]byte(`{"mcpServers":{"a":{"command":"x"}}}`))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if wrapped.MCPServers["a"].Command != "x" {
		t.Errorf("完整格式未解析: %v", wrapped.Names())
	}

	// 允许省略 mcpServers 包装。
	bare, err := LoadJSON([]byte(`{"a":{"command":"x"}}`))
	if err != nil {
		t.Fatalf("LoadJSON(bare): %v", err)
	}
	if bare.MCPServers["a"].Command != "x" {
		t.Errorf("裸映射未解析: %v", bare.Names())
	}

	// 空 mcpServers 不能被当成一个叫 mcpServers 的 server。
	empty, err := LoadJSON([]byte(`{"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("LoadJSON(empty): %v", err)
	}
	if len(empty.MCPServers) != 0 {
		t.Errorf("空配置应无 server: %v", empty.Names())
	}
}

func TestLoadJSONExpandsEnv(t *testing.T) {
	t.Setenv("MCP_JSON_TOKEN", "tk")
	cfg, err := LoadJSON([]byte(`{"h":{"url":"https://x.example/mcp","headers":{"Authorization":"Bearer ${MCP_JSON_TOKEN}"}}}`))
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if got := cfg.MCPServers["h"].Headers["Authorization"]; got != "Bearer tk" {
		t.Errorf("headers = %q，内联配置也要展开环境变量", got)
	}
}

func TestLoadJSONInvalid(t *testing.T) {
	if _, err := LoadJSON([]byte(`{"a":`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	} else if !strings.Contains(err.Error(), "内联配置") {
		t.Errorf("错误信息应指明来源: %v", err)
	}
}

func TestMergeOverrides(t *testing.T) {
	base, _ := LoadJSON([]byte(`{"a":{"command":"old"},"b":{"command":"keep"}}`))
	over, _ := LoadJSON([]byte(`{"a":{"command":"new"}}`))
	base.Merge(over)
	if got := base.MCPServers["a"].Command; got != "new" {
		t.Errorf("同名应覆盖: %q", got)
	}
	if _, ok := base.MCPServers["b"]; !ok {
		t.Error("未被覆盖的条目丢失")
	}
	base.Merge(nil)
	if len(base.MCPServers) != 2 {
		t.Errorf("Merge(nil) 不该改动配置: %v", base.Names())
	}
}

func TestManagerStatusForDisabledServer(t *testing.T) {
	off := false
	cfg := &Config{MCPServers: map[string]ServerConfig{
		"off": {Command: "x", Enabled: &off},
	}}
	mgr := NewManager(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("禁用的 server 不该导致 Connect 失败: %v", err)
	}
	status := mgr.Status()
	if len(status) != 1 || status[0].Connected {
		t.Fatalf("Status = %+v", status)
	}
	if mgr.Configured() != 1 {
		t.Errorf("Configured = %d", mgr.Configured())
	}
}

func TestManagerHiddenExposureNotGivenToModel(t *testing.T) {
	cfg := &Config{MCPServers: map[string]ServerConfig{
		"fake": mergeConfig(fakeServerConfig(t), ServerConfig{Exposure: "hidden"}),
	}}
	mgr := NewManager(cfg, WithConnectTimeout(30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Close()

	if got := len(mgr.Tools()); got != 0 {
		t.Errorf("hidden 的 server 不该暴露工具，得到 %d 个", got)
	}

	reg := tools.NewRegistry()
	skipped, err := mgr.RegisterInto(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Names()) != 0 && len(skipped) == 0 {
		t.Errorf("hidden 工具应被跳过或忽略，reg=%v skipped=%v", reg.Names(), skipped)
	}
}

func mergeConfig(base ServerConfig, over ServerConfig) ServerConfig {
	base.Exposure = over.Exposure
	return base
}
