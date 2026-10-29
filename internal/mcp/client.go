package mcp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leijux/acp-agent/internal/proc"
)

// Implementation 描述 MCP 客户端或服务端的身份。
type Implementation struct {
	Name    string
	Version string
}

// Client 是连接单个 MCP server 的客户端。
//
// 传输与协议（JSON-RPC 分帧、initialize 握手、SSE 解析、分页）全部交给
// 官方 Go SDK github.com/modelcontextprotocol/go-sdk，本包只保留
// 配置语义（mcp.json）、进程树回收、以及到 internal/tools 的适配。
type Client struct {
	name string
	cfg  ServerConfig

	sess *sdkmcp.ClientSession
	cmd  *exec.Cmd // 仅 stdio 模式非 nil，用于 Close 时回收进程树

	mu             sync.RWMutex
	tools          []*sdkmcp.Tool
	instructions   string
	onToolsChanged func([]*sdkmcp.Tool)

	closeOnce sync.Once
	closed    bool
}

// NewClient 启动（stdio）或连接（HTTP）一个 MCP server 并完成初始化握手。
func NewClient(ctx context.Context, name string, cfg ServerConfig, impl Implementation) (*Client, error) {
	if err := cfg.Validate(name); err != nil {
		return nil, err
	}

	transport, cmd, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}

	c := &Client{name: name, cfg: cfg, cmd: cmd}

	sdkImpl := &sdkmcp.Implementation{Name: impl.Name, Version: impl.Version}
	if sdkImpl.Name == "" {
		sdkImpl.Name = "acp-agent"
	}
	if sdkImpl.Version == "" {
		sdkImpl.Version = "0.1.0"
	}

	sdk := sdkmcp.NewClient(sdkImpl, &sdkmcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdkmcp.ToolListChangedRequest) {
			c.refreshTools()
		},
	})

	sess, err := sdk.Connect(ctx, transport, nil)
	if err != nil {
		if cmd != nil {
			proc.KillTree(cmd)
		}
		return nil, fmt.Errorf("mcp: 连接 %q: %w", name, err)
	}
	c.sess = sess
	if res := sess.InitializeResult(); res != nil {
		c.instructions = res.Instructions
	}

	// 顺手取一次工具列表，避免首次使用时多一次往返。
	if err := c.loadTools(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// headerRoundTripper 给 MCP 的每个 HTTP 请求补上配置里的 `headers`。
//
// 为什么要自己包一层：go-sdk 的 StreamableClientTransport 只有 Endpoint / HTTPClient
// 两个字段，没有 headers —— 配了 `Authorization` 如果不挂在 RoundTripper 上，
// 请求就一个字节都带不出去，对端的 401 会一路冒成
// `连接 "xxx": calling "initialize": sending "initialize": Unauthorized`。
//
// 注入规则：**只补不覆盖**。accept / content-type / mcp-session-id 这些协议头由 SDK
// 自己设，配置里写错一个 Accept 就能把 SSE 流弄坏，所以已经存在的头一律让位。
type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 不能改写调用方传进来的 request：RoundTrip 可能与其它请求并发、共享 header map。
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	if clone.Header == nil {
		clone.Header = http.Header{}
	}
	for name, values := range t.headers {
		if _, ok := clone.Header[name]; ok {
			continue
		}
		clone.Header[name] = values
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// headerOf 把配置里的 headers 规范成 http.Header，顺带丢掉空值。
func headerOf(cfg map[string]string) http.Header {
	h := make(http.Header, len(cfg))
	for name, value := range cfg {
		value = strings.TrimSpace(value)
		if name = strings.TrimSpace(name); name == "" || value == "" {
			continue
		}
		h.Set(name, value)
	}
	return h
}

// transportFor 按配置选择传输。stdio 模式预置进程属性，保证整棵进程树可回收。
func transportFor(cfg ServerConfig) (sdkmcp.Transport, *exec.Cmd, error) {
	if cfg.IsHTTP() {
		hc := &http.Client{Timeout: 0} // 单次请求超时由调用方 ctx 控制
		if cfg.Timeout > 0 {
			hc = &http.Client{Timeout: cfg.requestTimeout()}
		}
		// headers（Authorization / 自定义鉴权 / 路由头）必须挂在这里，否则配了也不生效。
		if h := headerOf(cfg.Headers); len(h) > 0 {
			hc.Transport = &headerRoundTripper{base: hc.Transport, headers: h}
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: hc,
			// 不自动重连：断连后由 Manager 决定是否重试，避免后台长连接堆积。
			MaxRetries: -1,
		}, nil, nil
	}

	if cfg.Command == "" {
		return nil, nil, fmt.Errorf("mcp: server 缺少 command 或 url")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Cwd
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	// 在 Start 之前配置：Unix 独立进程组 / Windows 预留 Job Object 标记。
	proc.Setup(cmd)
	return &sdkmcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: 2 * time.Second,
	}, cmd, nil
}

// Name 返回配置中的 server 名。
func (c *Client) Name() string { return c.name }

// ServerInfo 返回对端在 initialize 中自报的身份。
func (c *Client) ServerInfo() Implementation {
	res := c.sess.InitializeResult()
	if res == nil || res.ServerInfo == nil {
		return Implementation{}
	}
	return Implementation{Name: res.ServerInfo.Name, Version: res.ServerInfo.Version}
}

// ProtocolVersion 返回协商后的 MCP 协议版本。
func (c *Client) ProtocolVersion() string {
	if res := c.sess.InitializeResult(); res != nil {
		return res.ProtocolVersion
	}
	return ""
}

// SupportsTools 报告对端是否声明了 tools 能力。
func (c *Client) SupportsTools() bool {
	res := c.sess.InitializeResult()
	return res != nil && res.Capabilities != nil && res.Capabilities.Tools != nil
}

// Capabilities 返回对端能力（可能为 nil）。
func (c *Client) Capabilities() *sdkmcp.ServerCapabilities {
	if res := c.sess.InitializeResult(); res != nil {
		return res.Capabilities
	}
	return nil
}

// Instructions 返回服务端的用法提示（initialize 结果里的 instructions）。
func (c *Client) Instructions() string { return c.instructions }

// SetToolsChangedHandler 注册工具列表变化的回调。
func (c *Client) SetToolsChangedHandler(fn func([]*sdkmcp.Tool)) {
	c.mu.Lock()
	c.onToolsChanged = fn
	c.mu.Unlock()
}

// ListTools 返回当前工具列表（使用 SDK 的自动分页迭代器）。
func (c *Client) ListTools(ctx context.Context) iter.Seq2[*sdkmcp.Tool, error] {
	return c.sess.Tools(ctx, nil)
}

// CallTool 调用一个工具。超时取自配置。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*sdkmcp.CallToolResult, error) {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("mcp: server %q 已关闭", c.name)
	}

	timeout := c.cfg.requestTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain < timeout {
			timeout = remain
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := c.sess.CallTool(cctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("mcp: 调用 %s/%s 超时（%s）: %w", c.name, name, timeout, err)
		}
		return nil, fmt.Errorf("mcp: 调用 %s/%s: %w", c.name, name, err)
	}
	return res, nil
}

// Ping 做一次存活探测。
func (c *Client) Ping(ctx context.Context) error {
	return c.sess.Ping(ctx, nil)
}

// Close 关闭会话并回收 stdio 子进程树。
func (c *Client) Close() error {
	var first error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()

		if c.sess != nil {
			if err := c.sess.Close(); err != nil {
				first = err
			}
		}
		if c.cmd != nil {
			// SDK 已关闭 stdin 并尝试退出；这里兜底杀掉整棵进程树，
			// 防止 npx/node 之类派生的孙进程逃逸。
			proc.KillTree(c.cmd)
		}
	})
	return first
}

// Done 返回会话结束信号（server 主动断开时关闭）。
func (c *Client) Done() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		_ = c.sess.Wait()
		close(ch)
	}()
	return ch
}

// ---------- 内部 ----------

func (c *Client) loadTools(ctx context.Context) error {
	var list []*sdkmcp.Tool
	for tool, err := range c.ListTools(ctx) {
		if err != nil {
			return fmt.Errorf("mcp: 列出 %q 的工具: %w", c.name, err)
		}
		list = append(list, tool)
	}
	c.mu.Lock()
	c.tools = list
	c.mu.Unlock()
	return nil
}

func (c *Client) refreshTools() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.loadTools(ctx); err != nil {
		return
	}
	c.mu.RLock()
	handler := c.onToolsChanged
	list := c.tools
	c.mu.RUnlock()
	if handler != nil {
		handler(list)
	}
}

func (c *Client) cachedTools() []*sdkmcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*sdkmcp.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// IsClosed 报告客户端是否已关闭。
func (c *Client) IsClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}
