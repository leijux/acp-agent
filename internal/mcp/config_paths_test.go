package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultPathsLayout(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("ACP_AGENT_CONFIG_DIR", filepath.Join(dir, "cfg"))
	// Pi 原生文件在 home 下：把 home 钉到临时目录，测试才不依赖开发机上的 ~/.pi。
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	paths := DefaultPaths(filepath.Join(dir, "work"))
	want := []string{
		filepath.Join(home, ".pi", "agent", "mcp.json"), // Pi 原生（机器级，最低）
		filepath.Join(dir, "work", ".mcp.json"),         // 仓库级
		filepath.Join(dir, "cfg", ".mcp.json"),          // 用户级
		filepath.Join(dir, "work", ".pi", "mcp.json"),   // per-agent（最高）
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %v\nwant = %v", paths, want)
	}
}

// 同名 server 的覆盖顺序：Pi 原生 < 仓库 .mcp.json < 用户级 < per-agent .pi/mcp.json。
func TestDefaultPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	work := filepath.Join(dir, "work")
	t.Setenv("ACP_AGENT_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, p := range []string{
		filepath.Join(home, ".pi", "agent"),
		work,
		filepath.Join(work, ".pi"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(home, ".pi", "agent", "mcp.json"), `{"mcpServers":{"dup":{"command":"pi-home"}}}`)
	writeFile(t, filepath.Join(work, ".mcp.json"), `{"mcpServers":{"dup":{"command":"repo"}}}`)
	writeFile(t, filepath.Join(work, ".pi", "mcp.json"), `{"mcpServers":{"dup":{"command":"agent"}}}`)

	cfg, err := LoadFiles(DefaultPaths(work)...)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if got := cfg.MCPServers["dup"].Command; got != "agent" {
		t.Errorf(".pi/mcp.json 应压过其他文件，得到 %q", got)
	}

	// 没有 per-agent 文件时，仓库 .mcp.json 压过 Pi 原生。
	if err := os.Remove(filepath.Join(work, ".pi", "mcp.json")); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadFiles(DefaultPaths(work)...)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if got := cfg.MCPServers["dup"].Command; got != "repo" {
		t.Errorf("仓库 .mcp.json 应压过 ~/.pi/agent/mcp.json，得到 %q", got)
	}
}

func TestDefaultPathsFallsBackToUserConfigDir(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("ACP_AGENT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux；Windows 走 APPDATA
	t.Setenv("APPDATA", dir)         // Windows
	t.Setenv("HOME", home)           // macOS / Linux
	t.Setenv("USERPROFILE", home)    // Windows

	cfgDir := ConfigDir()
	if cfgDir == "" {
		t.Fatal("ConfigDir 不应为空")
	}
	if !strings.HasSuffix(cfgDir, "acp-agent") {
		t.Errorf("ConfigDir = %q，应以 acp-agent 结尾", cfgDir)
	}

	paths := DefaultPaths("")
	want := []string{
		filepath.Join(home, ".pi", "agent", "mcp.json"),
		filepath.Join(cfgDir, ".mcp.json"),
	}
	if !slices.Equal(paths, want) {
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
