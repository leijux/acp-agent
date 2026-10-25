package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPathsLayout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ACP_AGENT_CONFIG_DIR", filepath.Join(dir, "cfg"))

	paths := DefaultPaths(filepath.Join(dir, "work"))
	if len(paths) != 2 {
		t.Fatalf("paths = %v，期望 2 条", paths)
	}
	if want := filepath.Join(dir, "work", ".mcp.json"); paths[0] != want {
		t.Errorf("paths[0] = %q, want %q", paths[0], want)
	}
	if want := filepath.Join(dir, "cfg", ".mcp.json"); paths[1] != want {
		t.Errorf("paths[1] = %q, want %q", paths[1], want)
	}
}

func TestDefaultPathsFallsBackToUserConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ACP_AGENT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux；Windows 走 APPDATA
	t.Setenv("APPDATA", dir)         // Windows
	t.Setenv("HOME", dir)            // macOS

	cfgDir := ConfigDir()
	if cfgDir == "" {
		t.Fatal("ConfigDir 不应为空")
	}
	if !strings.HasSuffix(cfgDir, "acp-agent") {
		t.Errorf("ConfigDir = %q，应以 acp-agent 结尾", cfgDir)
	}

	paths := DefaultPaths("")
	if len(paths) != 1 || paths[0] != filepath.Join(cfgDir, ".mcp.json") {
		t.Errorf("workDir 为空时只应返回用户级路径，得到 %v", paths)
	}
}

func TestConfigDirEnvOverride(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG_DIR", `D:\custom\dir`)
	if got := ConfigDir(); got != `D:\custom\dir` {
		t.Errorf("ConfigDir = %q", got)
	}
}

func TestLoadFilesSkipsMissingAndRecordsSources(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".mcp.json")
	user := filepath.Join(dir, "user.mcp.json")

	writeFile(t, project, `{"mcpServers":{"same":{"command":"project"},"onlyProject":{"command":"p"}}}`)
	writeFile(t, user, `{"mcpServers":{"same":{"command":"user"},"onlyUser":{"command":"u"}}}`)

	cfg, err := LoadFiles(filepath.Join(dir, "missing.json"), project, "", user)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if len(cfg.SourceFiles) != 2 {
		t.Fatalf("SourceFiles = %v，期望只记存在的两个", cfg.SourceFiles)
	}
	if cfg.SourceFiles[0] != project || cfg.SourceFiles[1] != user {
		t.Errorf("SourceFiles 顺序不对: %v", cfg.SourceFiles)
	}
	// 靠后的覆盖靠前的
	if got := cfg.MCPServers["same"].Command; got != "user" {
		t.Errorf("同名 server 未被覆盖: %q", got)
	}
	if _, ok := cfg.MCPServers["onlyProject"]; !ok {
		t.Error("项目级独有的 server 丢失")
	}
	if _, ok := cfg.MCPServers["onlyUser"]; !ok {
		t.Error("用户级独有的 server 丢失")
	}
}

func TestLoadFilesNoneExist(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadFiles(filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json"))
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("不应有 server: %v", cfg.MCPServers)
	}
	if len(cfg.SourceFiles) != 0 {
		t.Errorf("SourceFiles 应为空: %v", cfg.SourceFiles)
	}
}

func TestLoadFilesUnreadablePathIsError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "dir-not-file")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFiles(bad)
	if err == nil {
		t.Fatal("目录当文件读应当报错")
	}
	if !strings.Contains(err.Error(), "dir-not-file") {
		t.Errorf("错误信息应含路径: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
