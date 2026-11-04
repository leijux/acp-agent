package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// rpcResponse 是测试侧对 response 行的宽松解码。
type rpcResponse struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// rpcLine 是所有输出行的最小共同形状（response / 事件 / queue_update …）。
type rpcLine struct {
	Type     string          `json:"type"`
	Command  string          `json:"command"`
	ID       string          `json:"id"`
	Success  bool            `json:"success"`
	Error    string          `json:"error"`
	Data     json.RawMessage `json:"data"`
	Steering []string        `json:"steering"`
	Message  struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func decodeRPCLines(t *testing.T, out string) []rpcLine {
	t.Helper()
	var lines []rpcLine
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l rpcLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("不是合法的 JSON 行: %q (%v)", line, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func findResponses(lines []rpcLine, command string) []rpcLine {
	var out []rpcLine
	for _, l := range lines {
		if l.Type == "response" && l.Command == command {
			out = append(out, l)
		}
	}
	return out
}

// 编排器探测用的就是这两条命令：get_state（当前模型/档位）+ get_available_models。
func TestPiRPCGetStateAndModels(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG_DIR", t.TempDir())
	// 目录只列「有凭据」的端点（无凭据的会把一个一选就 401 的模型摆到别人面前），
	// 所以给 deepseek 一个假 key，让断言不依赖开发机环境。
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")

	var out, errb bytes.Buffer
	in := strings.NewReader(
		`{"id":"multica-state","type":"get_state"}` + "\n" +
			`{"id":"multica-models","type":"get_available_models"}` + "\n" +
			"这不是 JSON\n" +
			`{"id":"unknown-1","type":"interrupt"}` + "\n" +
			`{"id":"no-type"}` + "\n",
	)
	cfg := Config{Provider: "deepseek", Model: "deepseek-flash", ThinkingLevel: "medium"}
	if err := runPiRPC(in, &out, &errb, "", "", cfg); err != nil {
		t.Fatalf("runPiRPC: %v", err)
	}
	lines := decodeRPCLines(t, out.String())
	if len(lines) != 4 {
		t.Fatalf("响应数 = %d, want 4（非 JSON 行应被忽略）:\n%s", len(lines), out.String())
	}

	// 1) get_state
	state := lines[0]
	if state.ID != "multica-state" || state.Command != "get_state" || !state.Success {
		t.Fatalf("get_state 响应不对: %+v", state)
	}
	var st struct {
		Model struct {
			ID               string            `json:"id"`
			Provider         string            `json:"provider"`
			Reasoning        bool              `json:"reasoning"`
			ThinkingLevelMap map[string]string `json:"thinkingLevelMap"`
		} `json:"model"`
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(state.Data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Model.ID != "deepseek-flash" || st.Model.Provider != "deepseek" {
		t.Errorf("get_state 的模型不对: %+v", st.Model)
	}
	if !st.Model.Reasoning {
		t.Error("deepseek-flash 应声明 reasoning=true（否则 UI 不显示档位）")
	}
	if st.ThinkingLevel != "medium" {
		t.Errorf("thinkingLevel = %q, want medium", st.ThinkingLevel)
	}

	// 2) get_available_models
	if lines[1].ID != "multica-models" || lines[1].Command != "get_available_models" || !lines[1].Success {
		t.Fatalf("get_available_models 响应不对: %+v", lines[1])
	}
	var payload struct {
		Models []struct {
			ID               string            `json:"id"`
			Provider         string            `json:"provider"`
			Reasoning        bool              `json:"reasoning"`
			ThinkingLevelMap map[string]string `json:"thinkingLevelMap"`
		} `json:"models"`
	}
	if err := json.Unmarshal(lines[1].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) == 0 {
		t.Fatal("模型目录为空（至少应该有回退的 deepseek）")
	}
	found := false
	for _, m := range payload.Models {
		if m.Provider == "" || m.ID == "" {
			t.Errorf("模型条目缺少 provider/id: %+v", m)
		}
		if m.Provider == "deepseek" && m.ID == "deepseek-flash" {
			found = true
			if !m.Reasoning || m.ThinkingLevelMap["max"] != "max" || m.ThinkingLevelMap["minimal"] != "low" {
				t.Errorf("deepseek-flash 的档位声明不对: %+v", m)
			}
		}
	}
	if !found {
		t.Errorf("目录里没有 deepseek/deepseek-flash: %+v", payload.Models)
	}

	// 3) 未实现的命令必须回失败而不是挂住
	if lines[2].Success || !strings.Contains(lines[2].Error, "未实现") || lines[2].Command != "interrupt" {
		t.Errorf("未知命令响应不对: %+v", lines[2])
	}
	// 4) 缺 type 也要回一条失败
	if lines[3].Success || lines[3].Error == "" {
		t.Errorf("缺 type 的响应不对: %+v", lines[3])
	}

	// 非 JSON 行只留一行 stderr 提示，不该中断会话
	if !strings.Contains(errb.String(), "非 JSON") {
		t.Errorf("stderr 应提示忽略了非 JSON 行: %q", errb.String())
	}
}

// --mode rpc 不需要 -p、不需要 API key；与 --acp 互斥。
func TestApplyRunModeRPC(t *testing.T) {
	var cfg Config
	if err := applyRunMode(&flags{mode: "rpc"}, &cfg); err != nil {
		t.Fatalf("applyRunMode(rpc): %v", err)
	}
	if !cfg.RPCMode {
		t.Fatal("--mode rpc 应置 RPCMode")
	}
	if cfg.JSONMode {
		t.Fatal("rpc 不该同时打开 JSONMode（两条输出面互斥）")
	}
	// 无人值守：ask 必须提成 auto，否则改动型工具会被逐个拒掉
	if cfg.Permissions != permAuto {
		t.Fatalf("rpc 下 permissions = %q, want auto", cfg.Permissions)
	}

	if err := applyRunMode(&flags{mode: "rpc", acpMode: true}, &Config{}); err == nil {
		t.Fatal("--acp 与 --mode rpc 应互斥")
	}

	// 未知模式仍然报错
	if err := applyRunMode(&flags{mode: "yaml"}, &Config{}); err == nil {
		t.Fatal("未知 --mode 应该报错")
	}
}

// stdin 立即 EOF（编排器写完就关管道）：不该报错、不该挂住。
func TestPiRPCExitsOnEOF(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runPiRPC(strings.NewReader(""), &out, &errb, "", "", Config{}); err != nil {
		t.Fatalf("空输入应正常返回: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("空输入不该有输出: %q", out.String())
	}
}

// fakeRPCRunner 让命令循环可以在不建 provider / 不连 MCP 的前提下被完整测试。
type fakeRPCRunner struct {
	mu       sync.Mutex
	handlers []agent.EventHandler

	steered chan string
	prompt  string
	runErr  error
}

func newFakeRPCRunner() *fakeRPCRunner {
	return &fakeRPCRunner{steered: make(chan string, 1)}
}

func (f *fakeRPCRunner) On(h agent.EventHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
}

func (f *fakeRPCRunner) emit(ev agent.Event) {
	f.mu.Lock()
	hs := append([]agent.EventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h(ev)
	}
}

func (f *fakeRPCRunner) Steer(text string) { f.steered <- text }

// Run 模拟「跑一轮 + 收到一条插话并在下一轮开始时消费它」。
func (f *fakeRPCRunner) Run(ctx context.Context, prompt string) error {
	f.prompt = prompt
	f.emit(agent.AgentStartEvent{})
	select {
	case <-f.steered:
		f.emit(agent.TurnStartEvent{Turn: 1}) // 消费点：插话在这一刻进入上下文
	case <-ctx.Done():
	}
	f.emit(agent.AgentEndEvent{})
	return f.runErr
}

// 任务 + steering 全链路：prompt 回执 → steer 回执与队列快照 → 消费凭证 →
// agent_settled 收尾。这些形状是 Multica 的 pi backend 逐条读的。
func TestPiRPCTaskWithSteering(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG_DIR", t.TempDir())

	var out, errb bytes.Buffer
	cfg := Config{Provider: "deepseek", Model: "deepseek-flash", ThinkingLevel: "medium"}
	s := newRPCSession(&out, &errb, "", "", cfg)
	fake := newFakeRPCRunner()
	s.newRunner = func(context.Context) (rpcTaskRunner, error) { return fake, nil }

	input := strings.Join([]string{
		`{"id":"initial-prompt","type":"prompt","message":"把 README 改一下"}`,
		`{"id":"steer-1","type":"steer","message":"顺便跑一下测试"}`,
	}, "\n") + "\n"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.loop(ctx, strings.NewReader(input)); err != nil {
		t.Fatalf("loop: %v", err)
	}
	lines := decodeRPCLines(t, out.String())
	if len(lines) == 0 {
		t.Fatal("没有任何输出")
	}

	// prompt：必须回 success + disposition=queued
	// （handled 会被调用方当成「整轮已结束」，它会立刻关 stdin 并停掉 supplement）
	prompts := findResponses(lines, "prompt")
	if len(prompts) != 1 || !prompts[0].Success {
		t.Fatalf("prompt 回执不对: %+v", prompts)
	}
	var disp struct {
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(prompts[0].Data, &disp); err != nil {
		t.Fatal(err)
	}
	if disp.Disposition != "queued" {
		t.Errorf("prompt disposition = %q, want queued", disp.Disposition)
	}

	// steer：成功 + queued
	steers := findResponses(lines, "steer")
	if len(steers) != 1 || !steers[0].Success {
		t.Fatalf("steer 回执不对: %+v", steers)
	}
	if err := json.Unmarshal(steers[0].Data, &disp); err != nil {
		t.Fatal(err)
	}
	if disp.Disposition != "queued" {
		t.Errorf("steer disposition = %q, want queued", disp.Disposition)
	}

	// queue_update：先看到队列里有这条插话
	sawQueued := false
	for _, l := range lines {
		if l.Type == "queue_update" && len(l.Steering) == 1 && l.Steering[0] == "顺便跑一下测试" {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Errorf("缺少带插话的 queue_update:\n%s", out.String())
	}

	// 消费凭证：role=user 且正文完全一致的 message_start
	receipt := false
	for _, l := range lines {
		if l.Type != "message_start" || l.Message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(l.Message.Content, &text) == nil && text == "顺便跑一下测试" {
			receipt = true
		}
	}
	if !receipt {
		t.Errorf("缺少插话被消费的 message_start 凭证:\n%s", out.String())
	}

	// 收尾必须是 agent_settled（否则调用方判 "RPC exited before agent_settled"）
	if last := lines[len(lines)-1]; last.Type != "agent_settled" {
		t.Errorf("最后一行应是 agent_settled，实际 %+v", last)
	}
	if fake.prompt != "把 README 改一下" {
		t.Errorf("投递的 prompt = %q", fake.prompt)
	}
}

// steer 只能落在 prompt 之后、轮次结束之前。
func TestPiRPCSteerBeforePromptFails(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG_DIR", t.TempDir())

	var out, errb bytes.Buffer
	s := newRPCSession(&out, &errb, "", "", Config{Provider: "deepseek", Model: "deepseek-flash"})
	s.newRunner = func(context.Context) (rpcTaskRunner, error) { return newFakeRPCRunner(), nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.loop(ctx, strings.NewReader(`{"id":"steer-1","type":"steer","message":"嗨"}`+"\n")); err != nil {
		t.Fatalf("loop: %v", err)
	}
	lines := decodeRPCLines(t, out.String())
	steers := findResponses(lines, "steer")
	if len(steers) != 1 || steers[0].Success || steers[0].Error == "" {
		t.Fatalf("prompt 之前的 steer 应回失败: %+v", steers)
	}
}

// 第二轮 prompt 必须被拒（一个进程只跑一轮）。
func TestPiRPCPromptTwiceFails(t *testing.T) {
	t.Setenv("ACP_AGENT_CONFIG_DIR", t.TempDir())

	var out, errb bytes.Buffer
	s := newRPCSession(&out, &errb, "", "", Config{Provider: "deepseek", Model: "deepseek-flash"})
	s.newRunner = func(context.Context) (rpcTaskRunner, error) { return newFakeRPCRunner(), nil }

	// 第二条 prompt 在第一轮还没结束时到达（fake 会阻塞在等 steer）
	input := strings.Join([]string{
		`{"id":"p1","type":"prompt","message":"第一轮"}`,
		`{"id":"p2","type":"prompt","message":"第二轮"}`,
		`{"id":"steer-1","type":"steer","message":"插话"}`,
	}, "\n") + "\n"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.loop(ctx, strings.NewReader(input)); err != nil {
		t.Fatalf("loop: %v", err)
	}
	prompts := findResponses(decodeRPCLines(t, out.String()), "prompt")
	if len(prompts) != 2 {
		t.Fatalf("应有两条 prompt 回执，实际 %d", len(prompts))
	}
	if !prompts[0].Success {
		t.Errorf("第一条 prompt 应成功: %+v", prompts[0])
	}
	if prompts[1].Success || prompts[1].Error == "" {
		t.Errorf("第二条 prompt 应被拒: %+v", prompts[1])
	}
}
