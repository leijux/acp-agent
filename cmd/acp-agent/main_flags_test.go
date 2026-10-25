package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/leijux/acp-agent/internal/agent"
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
