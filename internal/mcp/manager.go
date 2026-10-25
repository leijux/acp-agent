package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.com/leijux/acp-agent/internal/tools"
)

// DefaultConnectTimeout 是单个 server 建连（含 initialize 握手）的默认超时。
const DefaultConnectTimeout = 30 * time.Second

// Options 是 Manager 的可选配置。
type Options struct {
	// ClientInfo 声明给服务端的客户端身份。
	ClientInfo Implementation
	// ConnectTimeout 是单个 server 的建连超时。
	ConnectTimeout time.Duration
	// OnServerError 在单个 server 连接失败或中途断开时调用（可选）。
	OnServerError func(server string, err error)
}

// Option 是 Options 的设置函数。
type Option func(*Options)

// WithClientInfo 设置客户端身份。
func WithClientInfo(info Implementation) Option {
	return func(o *Options) { o.ClientInfo = info }
}

// WithConnectTimeout 设置建连超时。
func WithConnectTimeout(d time.Duration) Option {
	return func(o *Options) { o.ConnectTimeout = d }
}

// WithServerErrorHandler 设置连接失败回调。
func WithServerErrorHandler(fn func(server string, err error)) Option {
	return func(o *Options) { o.OnServerError = fn }
}

// ServerStatus 描述一个 server 的连接状态，用于 CLI 展示。
type ServerStatus struct {
	Name       string
	Transport  string
	Connected  bool
	ToolCount  int
	ServerName string
	Version    string
	Protocol   string
	Err        error
}

// Manager 管理多个 MCP server 的连接与工具聚合。
//
// 一个 server 失败不会影响其它 server：Connect 会返回聚合错误，但成功连接的
// server 仍然可用（与 Pi 的行为一致）。
type Manager struct {
	cfg  *Config
	opts Options

	mu      sync.RWMutex
	clients map[string]*Client
	tools   map[string][]tools.Tool
	errs    map[string]error

	onToolsChanged func(all []tools.Tool)
}

// NewManager 创建管理器。cfg 为 nil 时等价于空配置。
func NewManager(cfg *Config, opts ...Option) *Manager {
	if cfg == nil {
		cfg = &Config{MCPServers: map[string]ServerConfig{}}
	}
	o := Options{ConnectTimeout: DefaultConnectTimeout}
	for _, fn := range opts {
		fn(&o)
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	return &Manager{
		cfg:     cfg,
		opts:    o,
		clients: map[string]*Client{},
		tools:   map[string][]tools.Tool{},
		errs:    map[string]error{},
	}
}

// Configured 返回配置里的 server 数量。
func (m *Manager) Configured() int { return len(m.cfg.MCPServers) }

// SetToolsChangedHandler 注册工具集合变化回调。
//
// 服务端推送 notifications/tools/list_changed 时，Manager 会重新拉取该 server
// 的工具并调用这个回调，调用方据此同步 tools.Registry。
func (m *Manager) SetToolsChangedHandler(fn func(all []tools.Tool)) {
	m.mu.Lock()
	m.onToolsChanged = fn
	m.mu.Unlock()
}

// Connect 并发连接所有 enabled 的 server，返回聚合错误。
//
// ctx 只控制整体等待；单个 server 另有 ConnectTimeout。
func (m *Manager) Connect(ctx context.Context) error {
	names := m.cfg.Names()
	if len(names) == 0 {
		return nil
	}

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	for _, name := range names {
		cfg := m.cfg.MCPServers[name]
		if !cfg.IsEnabled() {
			continue
		}
		wg.Add(1)
		go func(name string, cfg ServerConfig) {
			defer wg.Done()
			if err := m.ConnectOne(ctx, name); err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				errMu.Unlock()
			}
		}(name, cfg)
	}
	wg.Wait()

	return errors.Join(errs...)
}

// ConnectOne 连接单个 server 并拉取它的工具。
func (m *Manager) ConnectOne(ctx context.Context, name string) error {
	cfg, ok := m.cfg.MCPServers[name]
	if !ok {
		return fmt.Errorf("mcp: 未配置 server %q", name)
	}

	connectCtx, cancel := context.WithTimeout(ctx, m.opts.ConnectTimeout)
	defer cancel()

	client, err := NewClient(connectCtx, name, cfg, m.opts.ClientInfo)
	if err != nil {
		m.recordError(name, err)
		return err
	}

	var ts []tools.Tool
	if client.SupportsTools() {
		defs := client.cachedTools()
		if !cfg.IsHidden() {
			ts = make([]tools.Tool, 0, len(defs))
			for _, def := range defs {
				ts = append(ts, newToolAdapter(client, def))
			}
		}
	}

	// tools/list_changed 通知到达时由 Client 刷新缓存，再回调这里重建适配器。
	client.SetToolsChangedHandler(func(defs []*sdkmcp.Tool) {
		m.applyToolList(name, client, defs)
	})

	m.mu.Lock()
	if old := m.clients[name]; old != nil && old != client {
		go old.Close()
	}
	m.clients[name] = client
	m.tools[name] = ts
	delete(m.errs, name)
	cb := m.onToolsChanged
	m.mu.Unlock()

	if cb != nil {
		cb(m.Tools())
	}
	return nil
}

// applyToolList 用最新的工具定义替换某个 server 的工具集合。
func (m *Manager) applyToolList(name string, client *Client, defs []*sdkmcp.Tool) {
	hidden := m.cfg.MCPServers[name].IsHidden()
	var ts []tools.Tool
	if !hidden {
		ts = make([]tools.Tool, 0, len(defs))
		for _, def := range defs {
			ts = append(ts, newToolAdapter(client, def))
		}
	}

	m.mu.Lock()
	m.tools[name] = ts
	cb := m.onToolsChanged
	m.mu.Unlock()

	if cb != nil {
		cb(m.Tools())
	}
}

// refreshTools 重新拉取某个 server 的工具（应对 tools/list_changed）。
func (m *Manager) refreshTools(name string) {
	m.mu.RLock()
	client := m.clients[name]
	m.mu.RUnlock()
	if client == nil || client.IsClosed() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.opts.ConnectTimeout)
	defer cancel()

	var defs []*sdkmcp.Tool
	for def, err := range client.ListTools(ctx) {
		if err != nil {
			m.recordError(name, fmt.Errorf("刷新工具列表: %w", err))
			return
		}
		defs = append(defs, def)
	}
	m.applyToolList(name, client, defs)
}

func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	m.errs[name] = err
	m.mu.Unlock()
	if m.opts.OnServerError != nil {
		m.opts.OnServerError(name, err)
	}
}

// Tools 返回所有已连接 server 的工具（按 server 名 + 工具名排序，顺序稳定）。
func (m *Manager) Tools() []tools.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.tools))
	for name := range m.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []tools.Tool
	for _, name := range names {
		out = append(out, m.tools[name]...)
	}
	return out
}

// RegisterInto 把全部 MCP 工具注册进注册表。
//
// 已存在同名工具时跳过（不覆盖内建工具），并把跳过的名字放进返回值。
func (m *Manager) RegisterInto(reg *tools.Registry) (skipped []string, err error) {
	if reg == nil {
		return nil, errors.New("mcp: Registry 为 nil")
	}
	for _, tool := range m.Tools() {
		if _, exists := reg.Get(tool.Name()); exists {
			skipped = append(skipped, tool.Name())
			continue
		}
		if rerr := reg.Register(tool); rerr != nil {
			skipped = append(skipped, tool.Name())
			err = errors.Join(err, rerr)
		}
	}
	return skipped, err
}

// SyncRegistry 让注册表与当前工具集合保持一致：注册新增、移除已消失的 mcp__ 工具。
//
// 用于响应 tools/list_changed —— 服务端可能撤回工具，必须能真正删掉。
func (m *Manager) SyncRegistry(reg *tools.Registry) error {
	if reg == nil {
		return errors.New("mcp: Registry 为 nil")
	}
	current := map[string]tools.Tool{}
	for _, tool := range m.Tools() {
		current[tool.Name()] = tool
	}

	var stale []string
	for _, name := range reg.Names() {
		if len(name) < len(toolNamePrefix) || name[:len(toolNamePrefix)] != toolNamePrefix {
			continue
		}
		if _, ok := current[name]; !ok {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		reg.Unregister(stale...)
	}

	for name, tool := range current {
		if _, exists := reg.Get(name); exists {
			continue
		}
		if err := reg.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// Status 返回所有配置条目的状态（含未连接的），供 CLI 展示。
func (m *Manager) Status() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := m.cfg.Names()
	out := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		cfg := m.cfg.MCPServers[name]
		st := ServerStatus{
			Name:      name,
			Transport: classifyTransport(cfg),
			Err:       m.errs[name],
		}
		if client := m.clients[name]; client != nil {
			info := client.ServerInfo()
			st.Connected = !client.IsClosed()
			st.ToolCount = len(m.tools[name])
			st.ServerName = info.Name
			st.Version = info.Version
			st.Protocol = client.ProtocolVersion()
		}
		out = append(out, st)
	}
	return out
}

// Client 返回指定 server 的客户端（未连接时返回 nil）。
func (m *Manager) Client(name string) *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[name]
}

// Close 关闭所有连接。
func (m *Manager) Close() error {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.tools = map[string][]tools.Tool{}
	m.mu.Unlock()

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error
	for name, c := range clients {
		wg.Add(1)
		go func(name string, c *Client) {
			defer wg.Done()
			if err := c.Close(); err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				errMu.Unlock()
			}
		}(name, c)
	}
	wg.Wait()
	return errors.Join(errs...)
}
