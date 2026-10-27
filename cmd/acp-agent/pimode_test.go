package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/provider"
)

// multicaPiArgs 是 Multica 守护进程真实会发过来的 argv（见其 pi backend 的
// buildPiArgs：-p --mode json [--session] [--model] [--thinking] … [--extension]）。
func multicaPiArgs(extra ...string) []string {
	return append([]string{
		"-p", "--mode", "json",
		"--session", "/tmp/pi-sessions/s-1/transcript.jsonl",
		"--model", "deepseek/deepseek-flash",
		"--thinking", "high",
	}, extra...)
}

func TestNormalizePiArgs_BareP(t *testing.T) {
	fs := newFlagSet(&flags{})
	got := normalizePiArgs(multicaPiArgs("--extension", "/tmp/mcp-extension.mjs"), fs)
	want := []string{
		"-p=-", "--mode", "json",
		"--session", "/tmp/pi-sessions/s-1/transcript.jsonl",
		"--model", "deepseek/deepseek-flash",
		"--thinking", "high",
		"--extension", "/tmp/mcp-extension.mjs",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("归一化结果 = %q\nwant %q", got, want)
	}
}

func TestNormalizePiArgs_KeepsPromptValue(t *testing.T) {
	// 人类手打的老用法不能被改坏。
	fs := newFlagSet(&flags{})
	got := normalizePiArgs([]string{"-p", "修一下测试", "--no-color"}, fs)
	want := []string{"-p", "修一下测试", "--no-color"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("归一化结果 = %q, want %q", got, want)
	}
}

// TestNormalizePiArgs_DashValueIsPrompt：prompt 本身以 - 开头（贴 markdown 列表）时
// 必须原样当值，不能被当成裸 -p。
func TestNormalizePiArgs_DashValueIsPrompt(t *testing.T) {
	fs := newFlagSet(&flags{})
	for _, prompt := range []string{"- 修一下测试", "--list-models", "--not-a-flag"} {
		got := normalizePiArgs([]string{"-p", prompt}, fs)
		want := []string{"-p", prompt}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("prompt=%q 归一化为 %q，期望原样", prompt, got)
		}
	}
}

func TestNormalizePiArgs_PrintAtEnd(t *testing.T) {
	fs := newFlagSet(&flags{})
	got := normalizePiArgs([]string{"--print"}, fs)
	if len(got) != 1 || got[0] != "--print=-" {
		t.Fatalf("末尾的裸 --print 应变成读 stdin，得到 %q", got)
	}
}

func TestParseFlags_PiInvocation(t *testing.T) {
	f, err := parseFlags(multicaPiArgs("--extension", "/tmp/mcp-extension.mjs", "-e", "/tmp/other.mjs"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.print != piStdinPrompt {
		t.Errorf("裸 -p 应表示从 stdin 读 prompt，得到 %q", f.print)
	}
	if f.mode != piModeJSON {
		t.Errorf("mode = %q, want json", f.mode)
	}
	if f.session != "/tmp/pi-sessions/s-1/transcript.jsonl" {
		t.Errorf("session = %q", f.session)
	}
	if f.model != "deepseek/deepseek-flash" {
		t.Errorf("model = %q", f.model)
	}
	if f.thinking != "high" {
		t.Errorf("thinking = %q", f.thinking)
	}
	if len(f.extensions) != 2 || f.extensions[0] != "/tmp/mcp-extension.mjs" || f.extensions[1] != "/tmp/other.mjs" {
		t.Errorf("extensions = %q", f.extensions)
	}
}

func TestSplitModelSelector(t *testing.T) {
	cases := []struct {
		in       string
		provider string
		model    string
		ok       bool
	}{
		{"deepseek/deepseek-flash", "deepseek", "deepseek-flash", true},
		// 模型 ID 自带斜杠：只切第一段。
		{"openrouter/anthropic/claude-sonnet-4.5", "openrouter", "anthropic/claude-sonnet-4.5", true},
		// 前缀不是已知 provider，原样保留。
		{"anthropic/claude-sonnet-4.5", "", "", false},
		{"gpt-5", "", "", false},
		{"/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		p, m, ok := splitModelSelector(c.in)
		if ok != c.ok || p != c.provider || m != c.model {
			t.Errorf("splitModelSelector(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, p, m, ok, c.provider, c.model, c.ok)
		}
	}
}

// TestPiEventHandlerStream 校验事件流形状：调用方就是按 type 和这几个字段解析的。
func TestPiEventHandlerStream(t *testing.T) {
	var buf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), nil)
	h(agent.AgentStartEvent{})
	h(agent.TurnStartEvent{Turn: 1})
	h(agent.MessageUpdateEvent{Kind: "thinking", Delta: "想一想"})
	h(agent.MessageUpdateEvent{Kind: "text", Delta: "改好了"})
	h(agent.ToolExecutionStartEvent{Call: agent.ToolCallBlock{
		ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.txt"},
	}})
	h(agent.ToolExecutionEndEvent{
		Call:   agent.ToolCallBlock{ID: "c1", Name: "read"},
		Result: &agent.ToolResultMessage{Content: []agent.Block{agent.TextBlock{Text: "内容"}}, IsError: true},
	})
	h(agent.TurnEndEvent{Turn: 1, Message: &agent.AssistantMessage{
		Model: "deepseek-flash", StopReason: agent.StopError, ErrorMessage: "boom",
		Usage: agent.Usage{Input: 10, Output: 5, CacheRead: 3, CacheWrite: 2, TotalTokens: 15},
	}})
	h(agent.AgentEndEvent{Messages: []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "任务"}}},
		&agent.AssistantMessage{StopReason: agent.StopError, ErrorMessage: "boom"},
	}})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("事件数 = %d, want 8：%q", len(lines), lines)
	}
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("非法 JSON 行 %q: %v", line, err)
		}
		events = append(events, ev)
	}

	wantTypes := []string{
		"agent_start", "turn_start", "message_update", "message_update",
		"tool_execution_start", "tool_execution_end", "turn_end", "agent_end",
	}
	for i, want := range wantTypes {
		if got := events[i]["type"]; got != want {
			t.Fatalf("第 %d 个事件 type = %v, want %s", i, got, want)
		}
	}

	thinking := events[2]["assistantMessageEvent"].(map[string]any)
	if thinking["type"] != "thinking_delta" || thinking["delta"] != "想一想" {
		t.Errorf("思考增量 = %v", thinking)
	}
	text := events[3]["assistantMessageEvent"].(map[string]any)
	if text["type"] != "text_delta" || text["delta"] != "改好了" {
		t.Errorf("正文增量 = %v", text)
	}

	start := events[4]
	if start["toolCallId"] != "c1" || start["toolName"] != "read" {
		t.Errorf("tool_execution_start = %v", start)
	}
	if args, _ := start["args"].(map[string]any); args["path"] != "a.txt" {
		t.Errorf("tool args 丢失: %v", start["args"])
	}

	end := events[5]
	if end["toolCallId"] != "c1" || end["result"] != "内容" || end["isError"] != true {
		t.Errorf("tool_execution_end = %v", end)
	}

	turn := events[6]["message"].(map[string]any)
	if turn["role"] != "assistant" || turn["stopReason"] != "error" || turn["errorMessage"] != "boom" {
		t.Errorf("turn_end.message = %v", turn)
	}
	usage := turn["usage"].(map[string]any)
	if usage["input"] != float64(10) || usage["output"] != float64(5) ||
		usage["cacheRead"] != float64(3) || usage["cacheWrite"] != float64(2) ||
		usage["totalTokens"] != float64(15) {
		t.Errorf("turn_end usage = %v", usage)
	}

	msgs := events[7]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "assistant" || last["stopReason"] != "error" {
		t.Errorf("agent_end 最后一条消息 = %v", last)
	}
}

// TestPiEventHandlerIgnoresNoise 校验不会往 stdout 吐非事件内容：
// 没有 delta 的增量和空错误不能变成行。
func TestPiEventHandlerIgnoresNoise(t *testing.T) {
	var buf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), nil)
	h(agent.MessageUpdateEvent{Kind: "text", Delta: ""})
	h(agent.MessageUpdateEvent{Kind: "unknown", Delta: "x"})
	h(agent.ErrorEvent{})
	if buf.Len() != 0 {
		t.Fatalf("不应产生输出，得到 %q", buf.String())
	}
}

func TestWritePiCatalogTable(t *testing.T) {
	rows := []piCatalogEntry{
		{Provider: "deepseek", ID: "deepseek-flash", ContextWindow: 128000, MaxOutput: 8192},
	}
	var buf bytes.Buffer
	if err := writePiCatalogTable(&buf, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("行数 = %d, want 2（表头 + 1 行）：%q", len(lines), lines)
	}
	// 解析方：首列 provider 按表头跳过，其余行取前两列拼 provider/model。
	if fields := strings.Fields(lines[0]); !strings.EqualFold(fields[0], "provider") {
		t.Errorf("表头首列 = %q, want provider", fields[0])
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 || fields[0] != "deepseek" || fields[1] != "deepseek-flash" {
		t.Errorf("数据行前两列 = %v", fields)
	}
}

func TestWritePiCatalogJSON(t *testing.T) {
	var buf bytes.Buffer
	rows := []piCatalogEntry{
		{Provider: "deepseek", ID: "deepseek-flash", Selector: "deepseek/deepseek-flash"},
	}
	if err := writePiCatalogJSON(&buf, rows); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models []struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
			Selector string `json:"selector"`
		} `json:"models"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, buf.String())
	}
	if len(doc.Models) != 1 || doc.Models[0].Selector != "deepseek/deepseek-flash" {
		t.Fatalf("目录内容不对: %+v", doc.Models)
	}
}

func TestHandlePiCatalog_OnlySubcommand(t *testing.T) {
	// 只有第一个参数是目录命令时才拦截。
	if handled, _ := handlePiCatalog([]string{"models", "看看目录"}, &bytes.Buffer{}); handled {
		t.Fatal("带位置参数的 models 应按 prompt 处理")
	}
	if handled, _ := handlePiCatalog([]string{"-p", "任务"}, &bytes.Buffer{}); handled {
		t.Fatal("普通运行不应被目录命令拦截")
	}
	// prompt 里恰好含有 --list-models 字样的运行不能被吃掉。
	if handled, _ := handlePiCatalog([]string{"-p", "--list-models"}, &bytes.Buffer{}); handled {
		t.Fatal("prompt 里的 --list-models 不应被当成目录命令")
	}
}

func TestHandlePiCatalog_ListModelsRespectsConfig(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("ACP_AGENT_PROVIDER", "ollama")
	var buf bytes.Buffer
	handled, err := handlePiCatalog([]string{"--list-models"}, &buf)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(buf.String(), "ollama") {
		t.Errorf("目录里应包含配置的 provider：%q", buf.String())
	}
}

// TestPiExtensionMCPServers 用 Multica 生成的真实脚本形状验证解析。
func TestPiExtensionMCPServers(t *testing.T) {
	servers := `{"mcpServers":{"fs":{"command":"npx","args":["-y","server-fs"]},"docs":{"url":"https://x.example/mcp"}}}`
	// Multica 把配置 json.Marshal 成字符串字面量嵌进脚本。
	literal, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	script := "const servers = JSON.parse(" + string(literal) + ");\n" +
		"export default function (pi) {\n" +
		"  for (const [name, config] of Object.entries(servers)) { pi.registerMcpServer(name, config); }\n" +
		"}\n"
	path := filepath.Join(t.TempDir(), "mcp-extension.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := piExtensionMCPServers(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("server 数 = %d, want 2：%v", len(got), got)
	}
	var fs struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(got["fs"], &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Command != "npx" || len(fs.Args) != 2 {
		t.Errorf("fs server 解析错误: %+v", fs)
	}
}

func TestPiExtensionMCPServers_IgnoresUnknownScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.mjs")
	if err := os.WriteFile(path, []byte("export default function (pi) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := piExtensionMCPServers(path)
	if err != nil {
		t.Fatalf("认不出的扩展不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不应解析出 server：%v", got)
	}
}

func TestMergePiExtensions_OverlaysExisting(t *testing.T) {
	servers := `{"mcpServers":{"fs":{"command":"npx"},"docs":{"url":"https://x.example/mcp"}}}`
	literal, _ := json.Marshal(servers)
	path := filepath.Join(t.TempDir(), "mcp-extension.mjs")
	script := "const servers = JSON.parse(" + string(literal) + ");\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{MCPJSON: `{"mcpServers":{"fs":{"command":"old"},"local":{"command":"keep"}}}`}
	mergePiExtensions(&cfg, []string{path})

	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
			URL     string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg.MCPJSON), &doc); err != nil {
		t.Fatalf("合并结果不是合法 JSON: %v (%q)", err, cfg.MCPJSON)
	}
	if len(doc.MCPServers) != 3 {
		t.Fatalf("server 数 = %d, want 3：%v", len(doc.MCPServers), cfg.MCPJSON)
	}
	if doc.MCPServers["fs"].Command != "npx" {
		t.Errorf("同名 server 应以扩展为准，得到 %q", doc.MCPServers["fs"].Command)
	}
	if doc.MCPServers["local"].Command != "keep" {
		t.Errorf("本地已有 server 不该丢: %v", doc.MCPServers["local"])
	}
	if doc.MCPServers["docs"].URL == "" {
		t.Errorf("扩展里的 HTTP server 丢失: %v", doc.MCPServers["docs"])
	}
}

// TestMergePiExtensions_SkipsInvalidServer：一个写错的 server 不能拖垮整轮运行。
func TestMergePiExtensions_SkipsInvalidServer(t *testing.T) {
	servers := `{"mcpServers":{"good":{"command":"npx"},"bad":{"args":["x"]}}}`
	literal, _ := json.Marshal(servers)
	path := filepath.Join(t.TempDir(), "mcp-extension.mjs")
	if err := os.WriteFile(path, []byte("const servers = JSON.parse("+string(literal)+");\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{}
	mergePiExtensions(&cfg, []string{path})

	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg.MCPJSON), &doc); err != nil {
		t.Fatalf("合并结果不是合法 JSON: %v (%q)", err, cfg.MCPJSON)
	}
	if _, ok := doc.MCPServers["good"]; !ok {
		t.Errorf("合法 server 被丢了: %q", cfg.MCPJSON)
	}
	if _, ok := doc.MCPServers["bad"]; ok {
		t.Errorf("缺 command/url 的 server 应被跳过: %q", cfg.MCPJSON)
	}
}

func TestMergePiExtensions_NoMCPWins(t *testing.T) {
	cfg := Config{NoMCP: true}
	mergePiExtensions(&cfg, []string{"/nonexistent.mjs"})
	if cfg.MCPJSON != "" {
		t.Fatalf("--no-mcp 时不应合并，得到 %q", cfg.MCPJSON)
	}
}

// TestPiEventHandlerErrorBeforeAgentEnd 校验失败原因先于「收尾」到达：
// 调用方看到 agent_end 就会把结果定稿。
func TestPiEventHandlerErrorBeforeAgentEnd(t *testing.T) {
	var buf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), nil)
	h(agent.AgentEndEvent{Err: errors.New("连接失败")})

	events := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(events) != 2 {
		t.Fatalf("事件数 = %d, want 2：%q", len(events), events)
	}
	if !strings.Contains(events[0], `"type":"error"`) {
		t.Errorf("error 应排在前面: %q", events[0])
	}
	if !strings.Contains(events[1], `"type":"agent_end"`) {
		t.Errorf("agent_end 应在最后: %q", events[1])
	}
}

// TestRenderHandlerQuietKeepsStdoutClean：-p / --mode json 下哪怕 run 失败，
// renderHandler 也不能往 stdout 写「错误: …」；stderr 也不该重复——失败原因由
// main 统一写一次，否则同一条错误会出现两次。
func TestRenderHandlerQuietKeepsStdoutClean(t *testing.T) {
	var out, errOut bytes.Buffer
	term := &terminal{out: &out, errOut: &errOut, quiet: true}
	a := &app{term: term}
	a.renderHandler()(agent.AgentEndEvent{Err: errors.New("炸了")})
	if out.Len() != 0 {
		t.Errorf("stdout 被污染: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("安静模式不该在这里重复报错: %q", errOut.String())
	}
}

// TestRenderHandlerInteractiveKeepsError：交互模式仍要把错误打到屏幕上。
func TestRenderHandlerInteractiveKeepsError(t *testing.T) {
	var out, errOut bytes.Buffer
	term := &terminal{out: &out, errOut: &errOut}
	a := &app{term: term}
	a.renderHandler()(agent.AgentEndEvent{Err: errors.New("炸了")})
	if !strings.Contains(out.String(), "炸了") {
		t.Errorf("交互模式应把错误写出去，得到 %q", out.String())
	}
}

// TestJSONModeEndToEnd 跑一次完整的 -p --mode json：stdout 只能是事件流，
// 最终结果由 text_delta 拼出，人读日志走 stderr，会话落到调用方给的路径。
func TestJSONModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(target, []byte("你好，世界\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, rounds := fakeModel(t, target)

	// Multica 冷启动会先把会话文件建出来（空文件）再传 --session。
	sessPath := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(sessPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var human, events bytes.Buffer
	cfg := Config{
		Provider:    "deepseek",
		Model:       "deepseek-chat",
		Root:        dir,
		Permissions: permAuto,
		MaxTurns:    5,
		Session:     sessPath,
		MCPConfig:   filepath.Join(dir, "no-mcp.json"),
		JSONMode:    true,
		JSONOut:     &events,
	}
	prov := provider.NewOpenAI(provider.OpenAIConfig{BaseURL: srv.URL})
	model := provider.ResolveModel(cfg.Provider, cfg.Model)
	term := &terminal{out: &human, errOut: &human}
	a, err := newApp(context.Background(), cfg, prov, model,
		bufio.NewReader(strings.NewReader("")), term, false, false)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.close()

	if err := a.runOnce(context.Background(), "读 hello.txt 并总结"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if rounds.Load() != 2 {
		t.Errorf("模型调用轮数 = %d, want 2", rounds.Load())
	}

	// 1) 每一行都是合法 JSON，首尾事件对得上。
	var kinds []string
	var text, toolOut string
	var agentEnd map[string]any
	for _, line := range strings.Split(strings.TrimSpace(events.String()), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout 出现非 JSON 行 %q: %v", line, err)
		}
		kind, _ := ev["type"].(string)
		kinds = append(kinds, kind)
		switch kind {
		case "message_update":
			delta, _ := ev["assistantMessageEvent"].(map[string]any)
			if delta["type"] == "text_delta" {
				text += delta["delta"].(string)
			}
		case "tool_execution_end":
			if isErr, _ := ev["isError"].(bool); isErr {
				t.Errorf("工具执行失败: %v", ev["result"])
			}
			toolOut, _ = ev["result"].(string)
		case "agent_end":
			agentEnd = ev
		}
	}
	if len(kinds) == 0 || kinds[0] != "agent_start" {
		t.Fatalf("事件流开头不对: %v", kinds)
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "agent_end" {
		t.Fatalf("事件流结尾不对: %v", kinds)
	}

	// 2) 调用方靠拼 text_delta 拿到最终结果。
	if !strings.Contains(text, "文件内容已读到") {
		t.Errorf("text_delta 拼接结果不对: %q", text)
	}
	if !strings.Contains(toolOut, "你好，世界") {
		t.Errorf("工具结果不该丢: %q", toolOut)
	}

	// 3) agent_end 的最后一条 assistant 是无错结束（调用方决定成败就看它）。
	msgs, _ := agentEnd["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("agent_end 没带消息: %v", agentEnd)
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "assistant" || last["stopReason"] == "error" {
		t.Errorf("agent_end 末条消息 = %v", last)
	}

	// 4) 人读内容全在 stderr，不能污染事件流所在的 stdout。
	if strings.Contains(human.String(), "文件内容已读到") {
		t.Errorf("最终结果不该再写进人读通道: %q", human.String())
	}

	// 5) 会话写回调用方给的路径，下一次运行能恢复。
	loaded, err := agent.LoadSession(sessPath)
	if err != nil {
		t.Fatalf("加载会话失败: %v", err)
	}
	if len(loaded.Messages()) == 0 {
		t.Fatal("会话文件没有内容，续跑会丢上下文")
	}
}

// TestApplyModelSelectorPrecedence：显式 provider 不能被 --model 的前缀劫持。
func TestApplyModelSelectorPrecedence(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		model        string
		wantProvider string
		wantModel    string
	}{
		// Multica 链路：不指定 provider，选择器带前缀 → 采纳。
		{"无 provider，采纳前缀", "", "deepseek/deepseek-flash", "deepseek", "deepseek-flash"},
		{"无 provider，跨家选择器", "", "openai/gpt-5", "openai", "gpt-5"},
		// 前缀就是当前 provider：只剥前缀。
		{"前缀同名", "deepseek", "deepseek/deepseek-flash", "deepseek", "deepseek-flash"},
		// 关键回归：openrouter 的模型 ID 本身就带斜杠。
		{"openrouter 模型自带斜杠", "openrouter", "openai/gpt-5", "openrouter", "openai/gpt-5"},
		{"openrouter 嵌套斜杠", "openrouter", "anthropic/claude-sonnet-4.5", "openrouter", "anthropic/claude-sonnet-4.5"},
		// 不带斜杠的普通 ID 不受影响。
		{"普通模型 ID", "deepseek", "deepseek-flash", "deepseek", "deepseek-flash"},
	}
	for _, c := range cases {
		cfg := Config{Provider: c.provider, Model: c.model}
		applyModelSelector(&cfg)
		if cfg.Provider != c.wantProvider || cfg.Model != c.wantModel {
			t.Errorf("%s: (%q,%q)，want (%q,%q)",
				c.name, cfg.Provider, cfg.Model, c.wantProvider, c.wantModel)
		}
	}
}

// TestApplyRunModeValidation：--mode 不能让程序滑进人类交互模式。
func TestApplyRunModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		flags   flags
		wantErr bool
		want    bool // JSONMode
	}{
		{name: "空模式", flags: flags{}, want: false},
		{name: "json + -p", flags: flags{mode: piModeJSON, print: piStdinPrompt}, want: true},
		{name: "json + 显式 prompt", flags: flags{mode: piModeJSON, print: "任务"}, want: true},
		{name: "json 漏 -p", flags: flags{mode: piModeJSON}, wantErr: true},
		{name: "json 与 acp 冲突", flags: flags{mode: piModeJSON, print: "x", acpMode: true}, wantErr: true},
		{name: "rpc 不支持", flags: flags{mode: piModeRPC}, wantErr: true},
		{name: "拼错的模式", flags: flags{mode: "yaml", print: "x"}, wantErr: true},
	}
	for _, c := range cases {
		cfg := Config{Permissions: "ask"}
		f := c.flags
		err := applyRunMode(&f, &cfg)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 应该报错", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不该报错: %v", c.name, err)
		}
		if cfg.JSONMode != c.want {
			t.Errorf("%s: JSONMode = %v, want %v", c.name, cfg.JSONMode, c.want)
		}
	}
}

// TestApplyRunModePermissions：json 模式下 ask（含大小写/空格变体）提成 auto，
// readonly 保留，--no-session 也要真的关掉。
func TestApplyRunModePermissions(t *testing.T) {
	for _, ask := range []string{"ask", "Ask", " ask "} {
		cfg := Config{Permissions: ask}
		f := flags{mode: piModeJSON, print: piStdinPrompt}
		if err := applyRunMode(&f, &cfg); err != nil {
			t.Fatalf("applyRunMode: %v", err)
		}
		if cfg.Permissions != permAuto {
			t.Errorf("permissions=%q 应提成 auto，得到 %q", ask, cfg.Permissions)
		}
	}

	cfg := Config{Permissions: permReadonly}
	f := flags{mode: piModeJSON, print: piStdinPrompt}
	if err := applyRunMode(&f, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions != permReadonly {
		t.Errorf("显式 readonly 不该被改写，得到 %q", cfg.Permissions)
	}

	cfg = Config{Session: "/tmp/x.jsonl", Permissions: permAuto}
	f = flags{mode: piModeJSON, print: piStdinPrompt, noSession: true}
	if err := applyRunMode(&f, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.NoSession || cfg.Session != "" {
		t.Errorf("--no-session 应关掉会话文件，得到 NoSession=%v Session=%q", cfg.NoSession, cfg.Session)
	}
}

// TestPiEventHandlerTurnEndOnProviderFailure：provider 失败时 agent 只发
// message_end（StopError），没有 turn_end；这里必须补上，否则调用方的「一轮
// 结束」永远等不到。成功轮次不能重复发。
func TestPiEventHandlerTurnEndOnProviderFailure(t *testing.T) {
	var buf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), nil)
	h(agent.MessageEndEvent{Message: &agent.AssistantMessage{
		Model: "deepseek-flash", StopReason: agent.StopError, ErrorMessage: "boom",
	}})
	h(agent.TurnEndEvent{Message: &agent.AssistantMessage{StopReason: agent.StopEnd}})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("事件数 = %d, want 2（失败轮补一条，成功轮一条）：%q", len(lines), lines)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	msg, _ := first["message"].(map[string]any)
	if first["type"] != "turn_end" || msg["stopReason"] != "error" || msg["errorMessage"] != "boom" {
		t.Errorf("失败轮 turn_end 不对: %v", first)
	}
}

func TestPiEventHandlerSuccessTurnNotDuplicated(t *testing.T) {
	var buf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), nil)
	h(agent.MessageEndEvent{Message: &agent.AssistantMessage{StopReason: agent.StopEnd}})
	if buf.Len() != 0 {
		t.Errorf("成功轮不该从 message_end 补 turn_end，得到 %q", buf.String())
	}
}

// TestPiEventHandlerNonFatalErrorStaysOffStdout：非致命错误（压缩失败等）不能
// 冒充 "error" 事件——调用方会把整轮判成失败。
func TestPiEventHandlerNonFatalErrorStaysOffStdout(t *testing.T) {
	var buf, errBuf bytes.Buffer
	h := piEventHandler(newPiEventSink(&buf), &errBuf)
	h(agent.ErrorEvent{Err: errors.New("压缩失败")})
	if buf.Len() != 0 {
		t.Errorf("stdout 不该有事件，得到 %q", buf.String())
	}
	if !strings.Contains(errBuf.String(), "压缩失败") {
		t.Errorf("stderr 应有痕迹，得到 %q", errBuf.String())
	}
}

// TestMergePiExtensions_BareInlineKeepsServers：--mcp-json 允许省略 mcpServers
// 包装，合并扩展时不能把这类已有的 server 全弄丢。
func TestMergePiExtensions_BareInlineKeepsServers(t *testing.T) {
	servers := `{"mcpServers":{"ext":{"command":"npx"}}}`
	literal, _ := json.Marshal(servers)
	path := filepath.Join(t.TempDir(), "mcp-extension.mjs")
	if err := os.WriteFile(path, []byte("const servers = JSON.parse("+string(literal)+");\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{MCPJSON: `{"local":{"command":"keep"}}`}
	mergePiExtensions(&cfg, []string{path})

	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg.MCPJSON), &doc); err != nil {
		t.Fatalf("合并结果不是合法 JSON: %v (%q)", err, cfg.MCPJSON)
	}
	if _, ok := doc.MCPServers["local"]; !ok {
		t.Errorf("裸映射形式的本地 server 被丢了: %q", cfg.MCPJSON)
	}
	if _, ok := doc.MCPServers["ext"]; !ok {
		t.Errorf("扩展里的 server 没合并进去: %q", cfg.MCPJSON)
	}
}

// TestPiExtensionMCPServers_MultipleParseCalls：脚本里先解析了别的 JSON 时，
// 不能因此拿不到真正的 mcpServers。
func TestPiExtensionMCPServers_MultipleParseCalls(t *testing.T) {
	servers := `{"mcpServers":{"fs":{"command":"npx"}}}`
	literal, _ := json.Marshal(servers)
	script := "const meta = JSON.parse(\"{}\");\n" +
		"const servers = JSON.parse(" + string(literal) + ");\n"
	path := filepath.Join(t.TempDir(), "mcp-extension.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := piExtensionMCPServers(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, ok := got["fs"]; !ok {
		t.Fatalf("没找到第二处 JSON.parse 里的 server: %v", got)
	}
}

// TestSaveSession_NoSessionWritesNothing：--no-session 不能偷偷落盘到数据目录。
func TestSaveSession_NoSessionWritesNothing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ACP_AGENT_DATA", dataDir)
	sess := agent.NewSession()
	if _, err := sess.Append(&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "hi"}}, At: 1}); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: Config{NoSession: true}, sess: sess, store: newSessionStore(t.TempDir())}

	a.saveSession()

	if a.sessPath != "" {
		t.Errorf("--no-session 不应分配会话路径，得到 %q", a.sessPath)
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "sessions"))
	if len(entries) != 0 {
		t.Errorf("--no-session 不应写盘，得到了 %d 个目录/文件", len(entries))
	}
}

// TestTerminalHumanOutQuiet：安静模式下人读输出必须走 stderr（stdout 留给事件流）。
func TestTerminalHumanOutQuiet(t *testing.T) {
	var out, errOut bytes.Buffer
	term := &terminal{out: &out, errOut: &errOut, quiet: true}
	term.printf("web 能力已关闭：%v\n", "no key")
	if out.Len() != 0 {
		t.Errorf("stdout 被污染: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "web 能力已关闭") {
		t.Errorf("stderr 应有该行，得到 %q", errOut.String())
	}

	// 非安静模式照旧写 stdout。
	out.Reset()
	errOut.Reset()
	term.quiet = false
	term.printf("hello\n")
	if out.String() != "hello\n" || errOut.Len() != 0 {
		t.Errorf("非安静模式应写 stdout：out=%q err=%q", out.String(), errOut.String())
	}
}

// failingWriter 模拟「磁盘满 / 设备错」：永远写失败。
type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// brokenPipeWriter 模拟调用方已经不读了。
type brokenPipeWriter struct{}

func (brokenPipeWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

// TestPiEventSink_RecordsWriteError：写不出去必须能被上层看到，不能静默
// 变成「退出码 0 + 半截事件流」。
func TestPiEventSink_RecordsWriteError(t *testing.T) {
	sink := newPiEventSink(failingWriter{err: errors.New("no space left on device")})
	h := piEventHandler(sink, nil)
	h(agent.AgentStartEvent{})
	h(agent.AgentStartEvent{})

	if err := sink.Err(); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("应记下首个写错误，得到 %v", err)
	}
}

// TestPiEventSink_IgnoresBrokenPipe：对面不读了（EPIPE / io.ErrClosedPipe）不是失败。
func TestPiEventSink_IgnoresBrokenPipe(t *testing.T) {
	sink := newPiEventSink(brokenPipeWriter{})
	h := piEventHandler(sink, nil)
	h(agent.AgentStartEvent{})
	if err := sink.Err(); err != nil {
		t.Fatalf("管道关闭不应计入错误，得到 %v", err)
	}
}

// TestEventStreamErr_NilSink：非 JSON 模式没有 sink，不能 nil panic。
func TestEventStreamErr_NilSink(t *testing.T) {
	a := &app{cfg: Config{}}
	if err := a.eventStreamErr(); err != nil {
		t.Fatalf("没有事件流时不应报错，得到 %v", err)
	}
}

// TestErrBrokenPipe：管道关闭类错误不算失败，其余错误要报。
func TestErrBrokenPipe(t *testing.T) {
	if !errBrokenPipe(io.ErrClosedPipe) {
		t.Error("io.ErrClosedPipe 应判为管道关闭")
	}
	if errBrokenPipe(errors.New("no space left on device")) {
		t.Error("磁盘满不该被当成管道关闭")
	}
	if errBrokenPipe(nil) {
		t.Error("nil 不该判为真")
	}
}

// TestLoadOrResetSession_MissingFileKeepsPath：编排列器冷启动传的路径可能不存在，
// 但必须原样留着（Save 会创建，下一轮靠它 resume）。
func TestLoadOrResetSession_MissingFileKeepsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	sess, gotPath, err := loadOrNewSession(Config{Session: path}, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if gotPath != path {
		t.Errorf("路径应保留，得到 %q", gotPath)
	}
	if len(sess.Messages()) != 0 {
		t.Errorf("应是空会话，得到 %d 条消息", len(sess.Messages()))
	}
}

// TestLoadOrResetSession_EmptyFileIsFresh：冷启动时文件已经建好但为空。
func TestLoadOrResetSession_EmptyFileIsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, gotPath, err := loadOrNewSession(Config{Session: path}, false); err != nil || gotPath != path {
		t.Fatalf("空文件应当当新会话：path=%q err=%v", gotPath, err)
	}
}

// TestLoadOrResetSession_CorruptFileBackedUp：坏文件不能再把整轮钉死。
func TestLoadOrResetSession_CorruptFileBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte("this is not jsonl\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, gotPath, err := loadOrNewSession(Config{Session: path}, false)
	if err != nil {
		t.Fatalf("坏文件不应导致失败: %v", err)
	}
	if gotPath != path {
		t.Errorf("应继续用原路径（下一轮还要 resume），得到 %q", gotPath)
	}
	if len(sess.Messages()) != 0 {
		t.Errorf("应是空会话，得到 %d 条", len(sess.Messages()))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("坏文件应被移走，原路径仍有文件: %v", err)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("应有备份文件，得到 %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "this is not jsonl\n" {
		t.Errorf("备份内容不对: %q", string(b))
	}
}

// TestLoadOrResetSession_DirectoryPicksNewPath：--session 指到目录时不能用。
func TestLoadOrResetSession_DirectoryPicksNewPath(t *testing.T) {
	dir := t.TempDir()
	sess, gotPath, err := loadOrNewSession(Config{Session: dir}, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if gotPath != "" {
		t.Errorf("目录不能当会话文件，应自动分配，得到 %q", gotPath)
	}
	if len(sess.Messages()) != 0 {
		t.Errorf("应是空会话，得到 %d 条", len(sess.Messages()))
	}
}
