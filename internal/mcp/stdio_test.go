package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leijux/acp-agent/internal/tools"
)

// fakeServerEnv 让测试二进制把自己当成 MCP stdio server 启动。
const fakeServerEnv = "MCP_FAKE_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

// runFakeServer 用官方 SDK 的 server 实现，暴露 4 个测试工具。
func runFakeServer() {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake", Version: "1.2.3"}, &sdkmcp.ServerOptions{
		Capabilities: &sdkmcp.ServerCapabilities{
			Tools: &sdkmcp.ToolCapabilities{ListChanged: true},
		},
	})

	readOnly := true
	server.AddTool(&sdkmcp.Tool{
		Name:        "echo",
		Description: "回显 text 参数",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = unmarshalArgs(req.Params.Arguments, &args)
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: args.Text}},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "write-file",
		Description: "有副作用的工具（未声明 readOnlyHint）",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = unmarshalArgs(req.Params.Arguments, &args)
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "wrote " + args.Path}},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "huge",
		Description: "返回超长文本",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("x", 300<<10)}},
		}, nil
	})

	// 故意只写 type 不写 properties，验证适配层的归一化。
	server.AddTool(&sdkmcp.Tool{
		Name:        "no-props",
		Description: "没有 properties 的 object schema",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}},
		}, nil
	})

	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

func unmarshalArgs(v any, out any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func fakeServerConfig(t *testing.T) ServerConfig {
	t.Helper()
	return ServerConfig{
		Command: os.Args[0],
		Env:     map[string]string{fakeServerEnv: "1"},
		Timeout: 30,
	}
}

func TestStdioClientLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewClient(ctx, "fake", fakeServerConfig(t), Implementation{Name: "test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	if got := client.ServerInfo().Name; got != "fake" {
		t.Errorf("serverInfo.name = %q, want fake", got)
	}
	if got := client.ServerInfo().Version; got != "1.2.3" {
		t.Errorf("serverInfo.version = %q, want 1.2.3", got)
	}
	if got := client.ProtocolVersion(); got == "" {
		t.Error("protocolVersion 为空")
	}
	if !client.SupportsTools() {
		t.Error("SupportsTools() = false")
	}
	if got := len(client.cachedTools()); got != 4 {
		t.Fatalf("tools = %d, want 4", got)
	}

	res, err := client.CallTool(ctx, "echo", map[string]any{"text": "你好"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %d", len(res.Content))
	}
	if txt, ok := res.Content[0].(*sdkmcp.TextContent); !ok || txt.Text != "你好" {
		t.Fatalf("content[0] = %#v", res.Content[0])
	}

	if err := client.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestStdioClientConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := NewClient(ctx, "fake", fakeServerConfig(t), Implementation{Name: "test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("msg-%02d", i)
			res, err := client.CallTool(ctx, "echo", map[string]any{"text": want})
			if err != nil {
				errs <- err
				return
			}
			if len(res.Content) == 0 {
				errs <- fmt.Errorf("%s: 空结果", want)
				return
			}
			if txt, ok := res.Content[0].(*sdkmcp.TextContent); !ok || txt.Text != want {
				errs <- fmt.Errorf("响应串了：want %q got %#v", want, res.Content[0])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStdioServerCrashIsReported(t *testing.T) {
	cfg := ServerConfig{Command: os.Args[0], Args: []string{"-test.run=NoSuchTest"}, Timeout: 5}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := NewClient(ctx, "crash", cfg, Implementation{Name: "test"}); err == nil {
		t.Fatal("期望连接失败，实际成功")
	}
}

func TestManagerConnectsAndRegisters(t *testing.T) {
	cfg := &Config{MCPServers: map[string]ServerConfig{"fake": fakeServerConfig(t)}}
	mgr := NewManager(cfg, WithConnectTimeout(30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Close()

	if got := len(mgr.Tools()); got != 4 {
		t.Fatalf("tools = %d, want 4", got)
	}

	reg := tools.NewRegistry()
	skipped, err := mgr.RegisterInto(reg)
	if err != nil {
		t.Fatalf("RegisterInto: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v", skipped)
	}
	tool, ok := reg.Get("mcp__fake__echo")
	if !ok {
		t.Fatalf("未注册 mcp__fake__echo，现有：%v", reg.Names())
	}

	res, err := tool.Execute(ctx, tools.Args{"text": "hi"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := res.Text(); got != "hi" {
		t.Errorf("result = %q, want hi", got)
	}

	status := mgr.Status()
	if len(status) != 1 || !status[0].Connected {
		t.Fatalf("Status = %+v", status)
	}
	if status[0].ToolCount != 4 {
		t.Errorf("ToolCount = %d, want 4", status[0].ToolCount)
	}
	if status[0].Transport != "stdio" {
		t.Errorf("Transport = %q", status[0].Transport)
	}
}

func TestManagerSyncRegistryRemovesStaleTools(t *testing.T) {
	cfg := &Config{MCPServers: map[string]ServerConfig{"fake": fakeServerConfig(t)}}
	mgr := NewManager(cfg, WithConnectTimeout(30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer mgr.Close()

	reg := tools.NewRegistry()
	if _, err := mgr.RegisterInto(reg); err != nil {
		t.Fatalf("RegisterInto: %v", err)
	}
	if err := reg.Register(stubTool{name: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(stubTool{name: "mcp__fake__gone"}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.SyncRegistry(reg); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	if _, ok := reg.Get("mcp__fake__gone"); ok {
		t.Error("失效的 MCP 工具未被移除")
	}
	if _, ok := reg.Get("read"); !ok {
		t.Error("内建工具被误删")
	}
}

func TestManagerPartialFailure(t *testing.T) {
	cfg := &Config{MCPServers: map[string]ServerConfig{
		"good": fakeServerConfig(t),
		"bad":  {Command: os.Args[0], Args: []string{"-test.run=NoSuchTest"}, Timeout: 5},
	}}
	mgr := NewManager(cfg, WithConnectTimeout(10*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Connect(ctx); err == nil {
		t.Fatal("期望聚合错误，实际 nil")
	}
	if len(mgr.Tools()) != 4 {
		t.Fatalf("好的 server 应该仍然可用，tools = %d", len(mgr.Tools()))
	}
	for _, st := range mgr.Status() {
		if st.Name == "bad" && st.Err == nil {
			t.Error("bad server 没有记录错误")
		}
		if st.Name == "good" && !st.Connected {
			t.Error("good server 未连接")
		}
	}
}

// stubTool 是注册表测试用的最小工具实现。
type stubTool struct{ name string }

func (s stubTool) Name() string          { return s.name }
func (s stubTool) Description() string   { return "stub" }
func (s stubTool) ConcurrencySafe() bool { return true }
func (s stubTool) Mutates() bool         { return false }
func (s stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (s stubTool) Execute(context.Context, tools.Args) (tools.Result, error) {
	return tools.Result{}, nil
}

// Close 会清空工具表 —— 这条行为是「/reload 必须先取名字再 Close」的依据。
// 先 Close 后问 Tools() 会拿到空列表，旧工具摘不掉，新连接注册时全部因同名被
// 跳过，表现出来就是「重载好像没生效」。
func TestManagerCloseClearsTools(t *testing.T) {
	cfg := &Config{MCPServers: map[string]ServerConfig{"fake": fakeServerConfig(t)}}
	mgr := NewManager(cfg, WithConnectTimeout(30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(mgr.Tools()) == 0 {
		t.Fatal("连接后应有工具")
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(mgr.Tools()); got != 0 {
		t.Fatalf("Close 之后工具表应为空（调用方必须在此之前取名字），实际 %d", got)
	}
}
