package main

import (
	"github.com/leijux/acp-agent/internal/tools"
	"context"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
)

func TestParseFlags_PositionalPrompt(t *testing.T) {
	f, err := parseFlags([]string{"-c", "继续改测试", "--provider", "openai"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.print != "继续改测试" {
		t.Errorf("位置参数应作为 prompt，得到 %q", f.print)
	}
	if !f.cont {
		t.Error("位置参数后面的 flag 不应被丢弃：-c 没生效")
	}
	// 关键回归：位置参数之后的 flag 也必须被解析到。
	if f.provider != "openai" {
		t.Errorf("位置参数之后的 --provider 被丢弃：得到 %q", f.provider)
	}
}

func TestParseFlags_PositionalJoined(t *testing.T) {
	f, err := parseFlags([]string{"修", "掉", "-m", "gpt-5"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.print != "修 掉" {
		t.Errorf("多个位置参数应按空格拼接，得到 %q", f.print)
	}
	if f.model != "gpt-5" {
		t.Errorf("模型 flag 丢失：得到 %q", f.model)
	}
}

func TestParseFlags_PositionalWithPConflicts(t *testing.T) {
	if _, err := parseFlags([]string{"-p", "a", "b"}); err == nil {
		t.Fatal("-p 与位置参数同时给出应报错")
	}
}

func TestParseFlags_NoPositionalKeepsDefault(t *testing.T) {
	f, err := parseFlags([]string{"-c"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.print != "" {
		t.Errorf("无位置参数时 prompt 应为空，得到 %q", f.print)
	}
	if !f.cont {
		t.Error("-c 未生效")
	}
}

func TestParseFlags_TemperatureInLaterSegment(t *testing.T) {
	f, err := parseFlags([]string{"任务", "--temperature", "0.7"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !f.temperatureSet {
		t.Error("被位置参数隔开的 --temperature 没被识别为已设置")
	}
	if f.temperature != 0.7 {
		t.Errorf("temperature=%v，期望 0.7", f.temperature)
	}
}

func TestParseFlags_UnknownFlagStillErrors(t *testing.T) {
	if _, err := parseFlags([]string{"任务", "--nope"}); err == nil {
		t.Fatal("未知 flag 应报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息应提到未知 flag，得到 %v", err)
	}
}

func TestSaveSession_AllocatesPathOnFirstSave(t *testing.T) {
	t.Setenv("ACP_AGENT_DATA", t.TempDir())
	root := t.TempDir()

	sess := agent.NewSession()
	if _, err := sess.Append(&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}, At: 1}); err != nil {
		t.Fatalf("追加消息失败: %v", err)
	}
	a := &app{sess: sess, store: newSessionStore(root)}

	// 之前 sessPath=="" 时直接 return：-p 模式中途被 kill 就丢历史。
	a.saveSession()

	if a.sessPath == "" {
		t.Fatal("sessPath 应被分配")
	}
	if _, err := os.Stat(a.sessPath); err != nil {
		t.Fatalf("会话文件未落盘: %v", err)
	}
	loaded, err := agent.LoadSession(a.sessPath)
	if err != nil {
		t.Fatalf("加载会话失败: %v", err)
	}
	if n := len(loaded.Messages()); n != 1 {
		t.Errorf("落盘消息数 %d，期望 1", n)
	}
}

func TestSaveSession_EmptySessionWritesNothing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACP_AGENT_DATA", dataDir)
	a := &app{sess: agent.NewSession(), store: newSessionStore(t.TempDir())}

	a.saveSession()

	if a.sessPath != "" {
		t.Errorf("空会话不应分配路径，得到 %q", a.sessPath)
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "sessions"))
	if len(entries) != 0 {
		t.Errorf("空会话不应产生文件，得到了 %d 个目录/文件", len(entries))
	}
}

// Multica 等编排器会按 Pi 的参数传一整套 no-* flag。任何一个不认，flag 解析就会
// 失败并（历史上）打印整段 usage —— 那段文本会被编排器的模型目录探测当成数据，
// 于是模型下拉框里出现 "acp-agent/[flags]"、"export/DEEPSEEK_API_KEY=sk-..." 这种
// 假模型（真实故障）。
func TestPiNoFlagsAccepted(t *testing.T) {
	for _, name := range []string{
		"no-prompt-templates", "np",
		"no-themes", "no-extensions", "ne",
		"no-approve", "na",
		"no-tools", "nt", "no-builtin-tools", "nbt",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFlags([]string{"--" + name}); err != nil {
				t.Fatalf("--%s 应被接受（兼容 Pi），实际报错: %v", name, err)
			}
		})
	}
}

// --list-models 允许出现在 launch prefix 之后（编排器会在前面拼自己的 token）。
func TestPiCatalogAfterPrefix(t *testing.T) {
	var buf bytes.Buffer
	handled, err := handlePiCatalog([]string{"--no-prompt-templates", "--list-models"}, &buf)
	if err != nil || !handled {
		t.Fatalf("带前缀的 --list-models 应被识别: handled=%v err=%v", handled, err)
	}
	if !strings.HasPrefix(buf.String(), "provider\tmodel") {
		t.Fatalf("应输出目录表头，实际: %q", buf.String())
	}
}

// prompt 里恰好含 --list-models 时不能被当成目录命令。
func TestPiCatalogIgnoresPromptText(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "--list-models"},
		{"--print", "--list-models"},
		{"帮我查一下 --list-models 的用法"},
		{"models", "帮我查一下"},
	} {
		var buf bytes.Buffer
		if handled, _ := handlePiCatalog(args, &buf); handled {
			t.Fatalf("%q 应被当成 prompt，不是目录命令", args)
		}
	}
}

// /reload mcp 只能摘自己注册的 mcp__* 工具：Manager.Tools() 返回远端全部工具，
// 其中包含与内建重名、注册时被跳过的项。按名字无条件 Unregister 会把内建
// read/grep 一起删掉。
func TestMCPToolNamesOnlyPrefix(t *testing.T) {
	list := []tools.Tool{
		fakeTool{name: "mcp__engram__search"},
		fakeTool{name: "read"},          // 远端重名、注册时被跳过
		fakeTool{name: "mcp__gh__pr_create"},
		fakeTool{name: "grep"},
	}
	got := mcpToolNames(list)
	want := []string{"mcp__engram__search", "mcp__gh__pr_create"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

type fakeTool struct{ name string }

func (f fakeTool) Name() string                       { return f.name }
func (f fakeTool) Description() string                { return "" }
func (f fakeTool) Parameters() map[string]any         { return nil }
func (f fakeTool) ConcurrencySafe() bool               { return true }
func (f fakeTool) Mutates() bool                      { return false }
func (f fakeTool) Execute(context.Context, tools.Args) (tools.Result, error) {
	return tools.Result{}, nil
}
