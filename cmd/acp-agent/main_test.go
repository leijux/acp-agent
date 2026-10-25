package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
	"github.com/leijux/acp-agent/internal/tools"
)

func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// fakeModel 起一个按「是否已有 tool 消息」决定下一帧的假模型：
// 第一轮发起 read 调用，第二轮给出结论。
func fakeModel(t *testing.T, filePath string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var round atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := round.Add(1)

		hasToolResult := strings.Contains(string(body), `"role":"tool"`)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		if !hasToolResult {
			args, _ := json.Marshal(map[string]string{"path": filePath})
			fmt.Fprint(w, sse(
				`{"id":"r1","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"先看文件。"}}]}`,
				fmt.Sprintf(`{"id":"r1","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":%q}}]}}]}`,
					string(args)),
				`{"id":"r1","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"id":"r1","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"prompt_cache_hit_tokens":40}}`,
			))
			fl.Flush()
			return
		}
		fmt.Fprint(w, sse(
			`{"id":"r2","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"文件内容已读到，共一行。"}}]}`,
			`{"id":"r2","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"r2","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":80,"completion_tokens":12,"total_tokens":92}}`,
		))
		fl.Flush()
		_ = n
	}))
	t.Cleanup(srv.Close)
	return srv, &round
}

// newTestApp 构造一个不碰真实终端的 app。
func newTestApp(t *testing.T, cfg Config, baseURL string) (*app, *bytes.Buffer) {
	t.Helper()
	// 隔离 MCP 配置：否则会读到开发机上真实的 MCP 配置。
	if cfg.MCPConfig == "" {
		cfg.MCPConfig = filepath.Join(t.TempDir(), "no-mcp.json")
	}
	prov := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: baseURL, NoStreamUsage: false})
	model := provider.ResolveModel(cfg.Provider, cfg.Model)
	buf := &bytes.Buffer{}
	term := &terminal{out: buf, errOut: buf}
	in := bufio.NewReader(strings.NewReader(""))
	a, err := newApp(context.Background(), cfg, prov, model, in, term, false, false)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(a.close)
	return a, buf
}

func TestEndToEnd_ToolLoop(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(target, []byte("你好，世界\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, rounds := fakeModel(t, target)

	cfg := Config{
		Provider:    "deepseek",
		Model:       "deepseek-chat",
		Root:        dir,
		Permissions: permAuto,
		MaxTurns:    5,
		Session:     filepath.Join(dir, "s.jsonl"),
	}
	a, buf := newTestApp(t, cfg, srv.URL)

	if err := a.runOnce(context.Background(), "读 hello.txt 并总结"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "文件内容已读到") {
		t.Errorf("缺少最终回复，输出：\n%s", out)
	}
	if rounds.Load() != 2 {
		t.Errorf("模型调用轮数 = %d, want 2", rounds.Load())
	}

	// 工具结果里应带文件内容
	msgs := a.sess.Messages()
	var toolText string
	for _, m := range msgs {
		if tr, ok := m.(*agent.ToolResultMessage); ok {
			if tr.IsError {
				t.Errorf("工具执行失败: %v", tr.Content)
			}
			toolText += blocksToText(tr.Content)
		}
	}
	if !strings.Contains(toolText, "你好，世界") {
		t.Errorf("工具结果未包含文件内容: %q", toolText)
	}

	// 会话已落盘且可恢复
	if _, err := os.Stat(cfg.Session); err != nil {
		t.Fatalf("会话文件未保存: %v", err)
	}
	loaded, err := agent.LoadSession(cfg.Session)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(loaded.Messages()) != len(msgs) {
		t.Errorf("恢复的消息数 = %d, want %d", len(loaded.Messages()), len(msgs))
	}

	// 累计用量包含两轮
	if got := a.agent.Turns(); got != 2 {
		t.Errorf("Turns = %d, want 2", got)
	}
	u := a.agent.TotalUsage()
	if u.Input != 130 {
		t.Errorf("累计 Input = %d, want 130（50+80）", u.Input)
	}
	if u.CacheRead != 40 {
		t.Errorf("累计 CacheRead = %d, want 40", u.CacheRead)
	}
}

func TestReadOnly_BlocksWriteTool(t *testing.T) {
	dir := t.TempDir()
	// 模型要求写文件
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if strings.Contains(string(body), `"role":"tool"`) {
			fmt.Fprint(w, sse(
				`{"choices":[{"index":0,"delta":{"content":"写不了，我换个办法。"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			))
			fl.Flush()
			return
		}
		args, _ := json.Marshal(map[string]string{"path": "evil.txt", "content": "x"})
		fmt.Fprint(w, sse(
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_w","type":"function","function":{"name":"write","arguments":%q}}]}}]}`, string(args)),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		))
		fl.Flush()
	}))
	defer srv.Close()

	cfg := Config{
		Provider:    "deepseek",
		Model:       "deepseek-chat",
		Root:        dir,
		Permissions: permReadonly,
		MaxTurns:    4,
	}
	a, buf := newTestApp(t, cfg, srv.URL)
	if err := a.runOnce(context.Background(), "写一个文件"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	// 文件绝不能落盘
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("readonly 模式下 write 仍然写入了文件")
	}
	// 拒绝要变成工具错误让模型看到
	var sawError bool
	for _, m := range a.sess.Messages() {
		if tr, ok := m.(*agent.ToolResultMessage); ok && tr.IsError {
			sawError = true
			if !strings.Contains(blocksToText(tr.Content), "拒绝") {
				t.Errorf("错误信息应说明被拒绝: %q", blocksToText(tr.Content))
			}
		}
	}
	if !sawError {
		t.Error("tool 结果里应有一条 IsError")
	}
	if !strings.Contains(buf.String(), "换个办法") {
		t.Errorf("turn 应继续（模型能看到拒绝并改策略），实际输出：\n%s", buf.String())
	}
}

func TestBuildProvider_DeepSeekDefaults(t *testing.T) {
	cfg := Config{Provider: "deepseek"}
	cfg.applyDefaults()
	prov, model, err := buildProvider(cfg, "")
	if err == nil {
		t.Fatal("缺少 key 时应报错")
	}
	_, _, _ = prov, model, err

	cfg2 := Config{Provider: "deepseek"}
	cfg2.applyDefaults()
	prov2, model2, err := buildProvider(cfg2, "sk-test")
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	if prov2 == nil {
		t.Fatal("provider 为空")
	}
	if model2.ID != "deepseek-flash" {
		t.Errorf("默认模型 = %q, want deepseek-flash", model2.ID)
	}
	if model2.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000", model2.ContextWindow)
	}
	if model2.MaxOutputTokens != 384_000 {
		t.Errorf("MaxOutputTokens = %d, want 384000", model2.MaxOutputTokens)
	}

	// 未知 provider 给出可选项
	cfg3 := Config{Provider: "nope"}
	if _, _, err := buildProvider(cfg3, "k"); err == nil || !strings.Contains(err.Error(), "deepseek") {
		t.Errorf("错误信息应列出可选项: %v", err)
	}
}

func TestParsePermMode(t *testing.T) {
	cases := map[string]permMode{
		"":          modeAsk,
		"ask":       modeAsk,
		"auto":      modeAuto,
		"yolo":      modeAuto,
		"readonly":  modeReadonly,
		"read-only": modeReadonly,
	}
	for in, want := range cases {
		got, err := parsePermMode(in)
		if err != nil {
			t.Errorf("parsePermMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parsePermMode(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parsePermMode("nonsense"); err == nil {
		t.Error("未知模式应报错")
	}
}

func TestGuard_AlwaysAndNever(t *testing.T) {
	var out bytes.Buffer
	in := bufio.NewReader(strings.NewReader("a\nd\n"))
	g := newGuard(modeAsk, in, &out)

	// 第一次问 → always
	if err := g.authorize(context.Background(), "write", true); err != nil {
		t.Fatalf("always 应放行: %v", err)
	}
	// 第二次不再问
	if err := g.authorize(context.Background(), "write", true); err != nil {
		t.Fatalf("记住 always 后应放行: %v", err)
	}
	// 另一个工具问 → never
	err := g.authorize(context.Background(), "edit", true)
	if err == nil {
		t.Fatal("never 应拒绝")
	}
	if err := g.authorize(context.Background(), "edit", true); err == nil {
		t.Fatal("记住 never 后应继续拒绝")
	}
	// 只读工具不问
	if err := g.authorize(context.Background(), "read", false); err != nil {
		t.Fatalf("只读工具不该被拦: %v", err)
	}
	if strings.Count(out.String(), "允许执行") != 2 {
		t.Errorf("提问次数 = %d, want 2\n%s", strings.Count(out.String(), "允许执行"), out.String())
	}
}

func TestWrapRegistry_PreservesMutationAndOrder(t *testing.T) {
	cfg := Config{Provider: "deepseek", Root: t.TempDir(), Permissions: permAuto}
	cfg.applyDefaults()
	reg, _ := tools.DefaultRegistry(tools.Options{Policy: tools.Policy{Root: cfg.Root}})
	wrapped := wrapRegistry(reg, newGuard(modeAuto, bufio.NewReader(strings.NewReader("")), io.Discard))

	if len(wrapped.Names()) != len(reg.Names()) {
		t.Fatalf("工具数不一致: %d vs %d", len(wrapped.Names()), len(reg.Names()))
	}
	// write 必须仍然被识别为改动型（否则会被并行调度）
	wt, ok := wrapped.Get("write")
	if !ok {
		t.Fatal("缺少 write 工具")
	}
	m, ok := wt.(interface{ Mutates() bool })
	if !ok {
		t.Fatal("包装后的 write 丢了 Mutates()")
	}
	if !m.Mutates() {
		t.Error("write 应报告 Mutates()=true")
	}
	// write 自带路径级互斥（lockFile）：调度上可并行，同一文件的读-改-写
	// 由工具内部串行化。权限层仍然按改动型处理。
	if !wt.ConcurrencySafe() {
		t.Error("write 应为 ConcurrencySafe（自带路径级互斥）")
	}
	if !tools.IsPathScoped(wt) {
		t.Error("包装后的 write 丢了 PathScoped()")
	}
	// read 仍然只读
	rt, _ := wrapped.Get("read")
	if !rt.ConcurrencySafe() {
		t.Error("read 应可并发")
	}
}
