package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leijux/acp-agent/internal/tools"
)

// newFakeHTTPServer 用官方 SDK 的 streamable HTTP handler 起一个测试 server。
func newFakeHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newFakeHTTPServerWith(t, nil)
}

// newFakeHTTPServerWith 同上，但允许在 handler 外面套一层（例如鉴权middleware）。
func newFakeHTTPServerWith(t *testing.T, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()

	readOnly := true
	build := func() *sdkmcp.Server {
		server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-http", Version: "2.0.0"}, &sdkmcp.ServerOptions{
			Capabilities: &sdkmcp.ServerCapabilities{
				Tools: &sdkmcp.ToolCapabilities{ListChanged: false},
			},
		})
		server.AddTool(&sdkmcp.Tool{
			Name:        "http-echo",
			Description: "HTTP 传输的 echo",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
			Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: readOnly},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args struct {
				Text string `json:"text"`
			}
			_ = unmarshalArgs(req.Params.Arguments, &args)
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "http:" + args.Text}},
			}, nil
		})
		return server
	}

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return build()
	}, nil)
	var h http.Handler = handler
	if wrap != nil {
		h = wrap(h)
	}
	return httptest.NewServer(h)
}

func TestHTTPTransport(t *testing.T) {
	srv := newFakeHTTPServer(t)
	defer srv.Close()

	cfg := ServerConfig{URL: srv.URL, Timeout: 30}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewClient(ctx, "http", cfg, Implementation{Name: "test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	if got := client.ServerInfo().Name; got != "fake-http" {
		t.Errorf("serverInfo.name = %q", got)
	}
	defs := client.cachedTools()
	if len(defs) != 1 || defs[0].Name != "http-echo" {
		t.Fatalf("tools = %+v", defs)
	}

	res, err := client.CallTool(ctx, "http-echo", map[string]any{"text": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if txt, ok := res.Content[0].(*sdkmcp.TextContent); !ok || txt.Text != "http:x" {
		t.Fatalf("content = %#v", res.Content[0])
	}

	if err := client.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestHTTPTransportRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := ServerConfig{URL: srv.URL, Timeout: 5}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := NewClient(ctx, "http", cfg, Implementation{Name: "test"})
	if err == nil {
		t.Fatal("期望连接失败")
	}
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "连接") {
		t.Errorf("错误信息不含线索: %v", err)
	}
}

// TestHTTPTransportSendsConfiguredHeaders 守住一个真实踩过的坑：mcp.json 里的
// `headers` 曾经被完全丢掉（go-sdk 的 StreamableClientTransport 只有 Endpoint /
// HTTPClient，没有 headers 字段），需要 Bearer 的对端一律在
// `initialize` 上回 401，前端看到的就是 `未连接 ... Unauthorized`。
func TestHTTPTransportSendsConfiguredHeaders(t *testing.T) {
	const want = "Bearer YQAV-EAFM"

	// 鉴权 middleware：所有请求（POST initialize / GET SSE / DELETE）都得带对头。
	var seen []string
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			seen = append(seen, r.Method)
			next.ServeHTTP(w, r)
		})
	}
	srv := newFakeHTTPServerWith(t, wrap)
	defer srv.Close()

	cfg := ServerConfig{
		URL:     srv.URL,
		Timeout: 30,
		Headers: map[string]string{"Authorization": "  " + want + "  "},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewClient(ctx, "http", cfg, Implementation{Name: "test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	if defs := client.cachedTools(); len(defs) != 1 {
		t.Fatalf("tools = %+v", defs)
	}

	res, err := client.CallTool(ctx, "http-echo", map[string]any{"text": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if txt, ok := res.Content[0].(*sdkmcp.TextContent); !ok || txt.Text != "http:x" {
		t.Fatalf("content = %#v", res.Content[0])
	}

	// 握手、列表、调用、SSE 长连接全得经过注入层；一个 POST 都没走到就是没注入。
	if len(seen) == 0 {
		t.Fatal("没有任何请求带上 Authorization（headers 未注入）")
	}
}

func TestHTTPTransportMissingHeadersFails(t *testing.T) {
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
	srv := newFakeHTTPServerWith(t, wrap)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := NewClient(ctx, "http", ServerConfig{URL: srv.URL, Timeout: 5}, Implementation{Name: "test"})
	if err == nil {
		t.Fatal("401 时连接应该失败")
	}
}

func TestToolAdapterNamingAndSchema(t *testing.T) {
	client := &Client{name: "my-server.v2"}

	adapter := newToolAdapter(client, &sdkmcp.Tool{
		Name:        "read:file",
		Description: "读文件",
		InputSchema: nil,
	})
	if got, want := adapter.Name(), "mcp__my-server_v2__read_file"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	params := adapter.Parameters()
	if params["type"] != "object" {
		t.Errorf("type = %v", params["type"])
	}
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Errorf("properties = %#v，必须是对象", params["properties"])
	}
	if adapter.ConcurrencySafe() {
		t.Error("未声明 readOnlyHint 时应为不安全")
	}
	if !adapter.Mutates() {
		t.Error("未声明 readOnlyHint 时应视为会改状态")
	}

	readOnly := true
	adapter2 := newToolAdapter(client, &sdkmcp.Tool{
		Name:        "list",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: readOnly},
	})
	if !adapter2.ConcurrencySafe() {
		t.Error("readOnlyHint=true 时应该是并发安全的")
	}
	if adapter2.Mutates() {
		t.Error("readOnlyHint=true 时不该走权限门禁")
	}
}

func TestNormalizeSchemaVariants(t *testing.T) {
	// 无 properties 的 object schema
	m := normalizeSchema(map[string]any{"type": "object"})
	if _, ok := m["properties"].(map[string]any); !ok {
		t.Errorf("properties 未补齐: %#v", m)
	}

	// nil
	if m := normalizeSchema(nil); m["type"] != "object" {
		t.Errorf("nil schema = %#v", m)
	}

	// json.RawMessage
	raw := normalizeSchema([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	props, ok := raw["properties"].(map[string]any)
	if !ok || props["a"] == nil {
		t.Errorf("RawMessage 解析失败: %#v", raw)
	}

	// 结构化输入（非 map）
	type schema struct {
		Type string `json:"type"`
	}
	if m := normalizeSchema(schema{Type: "object"}); m["type"] != "object" {
		t.Errorf("struct schema = %#v", m)
	}
}

func TestMapResultContentTypes(t *testing.T) {
	res := mapResult(&sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "hello"},
			&sdkmcp.ImageContent{Data: []byte("AAAA"), MIMEType: "image/png"},
			&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///a.txt", Text: "内容"}},
			&sdkmcp.AudioContent{Data: []byte("BBBB"), MIMEType: "audio/wav"},
		},
		StructuredContent: map[string]any{"k": "v"},
	})
	if res.IsError {
		t.Error("IsError 不该为 true")
	}
	var text, images int
	for _, b := range res.Content {
		switch b.Kind {
		case tools.KindText:
			text++
		case tools.KindImage:
			images++
		}
	}
	if text != 3 {
		t.Errorf("text blocks = %d, want 3", text)
	}
	if images != 1 {
		t.Errorf("image blocks = %d, want 1", images)
	}
	if res.Details == nil {
		t.Error("结构化内容应放进 Details")
	}
	if !strings.Contains(res.Text(), "file:///a.txt") {
		t.Errorf("资源块丢失: %s", res.Text())
	}
}

func TestMapResultTruncatesHugeOutput(t *testing.T) {
	res := mapResult(&sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("x", maxResultBytes+1024)}},
	})
	if !res.Truncated {
		t.Fatal("应标记 Truncated")
	}
	if len(res.Text()) > maxResultBytes+200 {
		t.Errorf("截断后仍有 %d 字节", len(res.Text()))
	}
	if !strings.Contains(res.Text(), "已截断") {
		t.Errorf("缺少截断提示: %q", res.Text()[len(res.Text())-120:])
	}
}

func TestMapResultStructuredOnly(t *testing.T) {
	res := mapResult(&sdkmcp.CallToolResult{StructuredContent: map[string]any{"answer": 42}})
	if !strings.Contains(res.Text(), "answer") {
		t.Errorf("只有 structuredContent 时应序列化为文本: %q", res.Text())
	}
}

func TestMapResultErrorFlag(t *testing.T) {
	res := mapResult(&sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "失败"}},
		IsError: true,
	})
	if !res.IsError {
		t.Error("IsError 未透传")
	}
}

func TestSafeCutKeepsUTF8(t *testing.T) {
	s := "中文中文中文"
	for i := 0; i <= len(s); i++ {
		got := safeCut(s, i)
		if !strings.HasPrefix(s, got) {
			t.Fatalf("safeCut(%d) 不是前缀: %q", i, got)
		}
		for _, r := range got {
			if r == '\uFFFD' {
				t.Fatalf("safeCut(%d) 切坏了字符", i)
			}
		}
	}
}
