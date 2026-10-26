package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leijux/acp-agent/internal/acp"
)

// buildSpec 决定子代理进程的全部启动参数，错了就是「子代理跑起来不是自己」，
// 所以这里逐项钉住。
func TestBuildSpecSelfDefaults(t *testing.T) {
	root := t.TempDir()
	a := &app{cfg: Config{Root: root, Provider: "deepseek", Model: "deepseek-v4-flash"}}

	spec, err := a.buildSpec("worker")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Command != exe {
		t.Errorf("Command = %q，期望自身 %q", spec.Command, exe)
	}
	if spec.Dir != root {
		t.Errorf("Dir = %q，期望 %q", spec.Dir, root)
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"--acp",                    // 必须走 ACP agent 模式
		"--root " + root,           // 与主进程同一根
		"--permissions ask",        // 改动型工具必须上报回主进程
		"--disable-tools delegate", // 防无限递归
		"--provider deepseek",
		"-m deepseek-v4-flash",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("参数缺少 %q，实际: %s", want, joined)
		}
	}
	// 子代理不加载分词词典：分词器没有命令行开关了，改用环境变量降级。
	if strings.Contains(joined, "--segmenter") {
		t.Errorf("分词器 flag 已移除，不该再传给子代理: %s", joined)
	}
	if !slices.Contains(spec.Env, "ACP_AGENT_SEGMENTER=rune") {
		t.Errorf("子代理应通过环境变量降级到 rune，实际 Env: %v", spec.Env)
	}
}

// Subagent.Command 明确指定时不能再被 self 覆盖。
func TestBuildSpecExternalCommand(t *testing.T) {
	a := &app{cfg: Config{
		Root:     t.TempDir(),
		Subagent: &SubagentConfig{Command: "claude-code-acp", Args: []string{"--verbose"}},
	}}
	spec, err := a.buildSpec("worker")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if spec.Command != "claude-code-acp" {
		t.Errorf("Command = %q，期望 claude-code-acp", spec.Command)
	}
	if spec.Args[len(spec.Args)-1] != "--verbose" {
		t.Errorf("自定义 Args 未追加到末尾: %v", spec.Args)
	}
}

// 用户在配置里显式给了模型/权限时以它为准。
func TestBuildSpecOverrides(t *testing.T) {
	a := &app{cfg: Config{
		Root:     t.TempDir(),
		Model:    "main-model",
		Subagent: &SubagentConfig{Model: "cheap-model", Permissions: "readonly"},
	}}
	spec, err := a.buildSpec("reviewer")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "-m cheap-model") {
		t.Errorf("模型未覆盖: %s", joined)
	}
	if !strings.Contains(joined, "--permissions readonly") {
		t.Errorf("权限未覆盖: %s", joined)
	}
	if strings.Contains(joined, "main-model") {
		t.Errorf("主模型名泄漏进子代理参数: %s", joined)
	}
}

func TestSubagentKindMutates(t *testing.T) {
	for _, k := range []string{"read", "search", "think", "fetch", "Read", "SEARCH"} {
		if subagentKindMutates(k) {
			t.Errorf("kind=%q 被判为改动型", k)
		}
	}
	for _, k := range []string{"edit", "delete", "move", "execute", "", "weird"} {
		if !subagentKindMutates(k) {
			t.Errorf("kind=%q 应判为改动型（未知一律从严）", k)
		}
	}
}

func TestPickOption(t *testing.T) {
	opts := []acp.PermissionOption{
		{OptionID: "a1", Kind: "allow_always"},
		{OptionID: "a2", Kind: "allow_once"},
		{OptionID: "r1", Kind: "reject_once"},
	}
	if got := pickOption(opts, "allow_once", "allow"); got != "a2" {
		t.Errorf("allow 优先级 = %q，期望 a2", got)
	}
	if got := pickOption(opts, "reject_once", "reject"); got != "r1" {
		t.Errorf("reject = %q，期望 r1", got)
	}
	if got := pickOption(opts, "nope"); got != "" {
		t.Errorf("无匹配应返回空，得到 %q", got)
	}
}

func TestFormatSubagentResult(t *testing.T) {
	t.Run("正常结束不加噪声", func(t *testing.T) {
		got := formatSubagentResult("worker", &acp.SubagentResult{
			Text:       "答案",
			StopReason: acp.StopEndTurn,
		})
		if strings.Contains(got, "子代理 worker：") {
			t.Errorf("end_turn 不该附注: %q", got)
		}
		if !strings.HasPrefix(got, "答案") {
			t.Errorf("文本 = %q", got)
		}
	})

	t.Run("异常必须显式说明", func(t *testing.T) {
		got := formatSubagentResult("worker", &acp.SubagentResult{
			Text:       "半截",
			StopReason: acp.StopMaxTokens,
			ToolCalls:  3,
		})
		for _, want := range []string{"max_tokens", "工具调用 3 次"} {
			if !strings.Contains(got, want) {
				t.Errorf("缺少 %q: %q", want, got)
			}
		}
	})

	t.Run("空文本加标记", func(t *testing.T) {
		got := formatSubagentResult("worker", &acp.SubagentResult{})
		if !strings.Contains(got, "没有返回任何文本") {
			t.Errorf("文本 = %q", got)
		}
	})
}

func TestPrefixWriter(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := &prefixWriter{w: f, prefix: "[w] "}
	if _, err := w.Write([]byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("c\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "[w] a\n[w] bc\n"; got != want {
		t.Errorf("输出 = %q，期望 %q", got, want)
	}
}

// TestPrefixWriterKeepsMultibyteChars 回归：管道在任意字节处切断时，
// 多字节字符不能被转成 U+FFFD（早先按 rune 遍历 string(b) 就会）。
func TestPrefixWriterKeepsMultibyteChars(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: "[w] "}

	// 把「中文」两个字节能切开：e4 b8 ad | e6 96 87
	p.Write([]byte{'e', '4', ' ', 0xe4})
	p.Write([]byte{0xb8, 0xad, 0xe6})
	p.Write([]byte{0x96, 0x87, '\n'})

	got := buf.String()
	if strings.Contains(got, "\uFFFD") {
		t.Fatalf("出现替换字符（多字节被切断）: %q", got)
	}
	if !strings.Contains(got, "[w] e4 中文") {
		t.Fatalf("输出 = %q，期望 \"[w] e4 中文\"", got)
	}
}

// TestPrefixWriterFlushTail 没有结尾换行的内容要靠 Flush 输出。
func TestPrefixWriterFlushTail(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: "[w] "}
	p.Write([]byte("没有换行的尾巴"))
	if buf.Len() != 0 {
		t.Fatalf("未换行时不应输出: %q", buf.String())
	}
	p.Flush()
	if !strings.Contains(buf.String(), "[w] 没有换行的尾巴") {
		t.Fatalf("Flush 后输出 = %q", buf.String())
	}
}

// TestPrefixWriterLineCap 超长单行必须被强制断行，不能无界缓冲。
func TestPrefixWriterLineCap(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: "[w] "}
	p.Write(bytes.Repeat([]byte("x"), prefixMaxLine*3))
	p.Write([]byte("\n"))

	lines := strings.Count(buf.String(), "\n")
	if lines < 3 {
		t.Fatalf("超长行没有被强制断行，只输出了 %d 行", lines)
	}
	if buf.Len() > prefixMaxLine*4 {
		t.Fatalf("输出异常膨胀: %d 字节", buf.Len())
	}
}

// TestPrefixWriterConcurrentWrites 并发写入不能触发 race（原实现用 lazy chan 做锁）。
func TestPrefixWriterConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: "[w] "}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p.Write([]byte("行内容\n"))
			}
		}(i)
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "\n"); got != 400 {
		t.Fatalf("输出 %d 行，期望 400", got)
	}
}
