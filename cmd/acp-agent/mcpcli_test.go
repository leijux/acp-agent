package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inlineJSON 是命令行里最常见的用法：一行 JSON 临时挂一个 server。
const inlineJSON = `{"mcpServers":{"playwright":{"command":"npx","args":["-y","@playwright/mcp"]}}}`

func TestLoadMCPConfig_InlineConfigReplacesFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ACP_AGENT_CONFIG_DIR", filepath.Join(root, "cfg"))
	// 项目里有配置，但 --mcp-config 传 JSON 时不该被读进来（与传路径语义一致）。
	writeMCPFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"fromFile":{"command":"x"}}}`)

	cfg, err := loadMCPConfig(Config{Root: root, MCPConfig: inlineJSON})
	if err != nil {
		t.Fatalf("loadMCPConfig: %v", err)
	}
	if _, ok := cfg.MCPServers["playwright"]; !ok {
		t.Errorf("内联配置的 server 未生效: %v", cfg.Names())
	}
	if _, ok := cfg.MCPServers["fromFile"]; ok {
		t.Errorf("--mcp-config 给内联 JSON 时不该再读默认文件: %v", cfg.Names())
	}
	if len(cfg.SourceFiles) != 0 {
		t.Errorf("内联 JSON 不该产生 SourceFiles: %v", cfg.SourceFiles)
	}
}

func TestLoadMCPConfig_InlineJSONMergesAndOverrides(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ACP_AGENT_CONFIG_DIR", filepath.Join(root, "cfg"))
	writeMCPFile(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"keep":{"command":"keep"},"same":{"command":"old"}}}`)

	cfg, err := loadMCPConfig(Config{
		Root:    root,
		MCPJSON: `{"same":{"command":"new"},"extra":{"url":"https://x.example/mcp"}}`,
	})
	if err != nil {
		t.Fatalf("loadMCPConfig: %v", err)
	}
	if got := cfg.MCPServers["same"].Command; got != "new" {
		t.Errorf("同名 server 应被 --mcp-json 覆盖，得到 %q", got)
	}
	if _, ok := cfg.MCPServers["keep"]; !ok {
		t.Error("配置文件里的其他 server 丢失了")
	}
	if _, ok := cfg.MCPServers["extra"]; !ok {
		t.Error("--mcp-json 注入的 server 缺失")
	}
	if len(cfg.SourceFiles) != 1 {
		t.Errorf("文件来源应保留: %v", cfg.SourceFiles)
	}
}

func TestLoadMCPConfig_BareInlineJSON(t *testing.T) {
	// 省略 mcpServers 包装也要能用：临时注入时少一层括号。
	cfg, err := loadMCPConfig(Config{Root: t.TempDir(), MCPJSON: `{"fs":{"command":"npx"}}`})
	if err != nil {
		t.Fatalf("loadMCPConfig: %v", err)
	}
	if cfg.MCPServers["fs"].Command != "npx" {
		t.Errorf("裸映射未解析: %v", cfg.Names())
	}
}

func TestLoadMCPConfig_InvalidInlineJSON(t *testing.T) {
	if _, err := loadMCPConfig(Config{Root: t.TempDir(), MCPJSON: `{"fs":`}); err == nil {
		t.Fatal("非法 JSON 应报错")
	} else if !strings.Contains(err.Error(), "内联配置") {
		t.Errorf("错误信息应指明来源: %v", err)
	}
}

func TestDescribeSources_MarksInline(t *testing.T) {
	got := describeSources(Config{MCPConfig: inlineJSON}, nil)
	if !strings.Contains(got, "--mcp-config") {
		t.Errorf("describeSources = %q，应标出内联 --mcp-config", got)
	}
	got = describeSources(Config{MCPJSON: inlineJSON}, nil)
	if !strings.Contains(got, "--mcp-json") {
		t.Errorf("describeSources = %q，应标出 --mcp-json", got)
	}
	if got := describeSources(Config{}, nil); got != "无配置文件" {
		t.Errorf("describeSources = %q", got)
	}
}

func writeMCPFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
