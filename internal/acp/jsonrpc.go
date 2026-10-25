// Package acp 实现 Agent Client Protocol（ACP v1）的 Agent 侧。
//
// 传输层是双向 JSON-RPC 2.0 over newline-delimited JSON：同一根管道上
// 客户端与 Agent 都会发请求，因此 Conn 同时承担「发送请求」与「响应请求」。
//
// 分包结构：
//
//	jsonrpc.go  JSON-RPC 2.0 连接（读循环、pending 请求、并发写）
//	schema.go   协议类型（ContentBlock / SessionUpdate / 各方法参数）
//	server.go   Agent 侧方法注册与分发
//	session.go  ACP 会话 ↔ internal/agent 绑定
//	update.go   agent.Event → session/update 映射
//	client.go   Agent 主动调用客户端的反向方法
//	permission.go 把 session/request_permission 接进 tools 调度
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

const jsonrpcVersion = "2.0"

// JSON-RPC 2.0 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// RPCError 是 JSON-RPC 错误对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Data != nil {
		return fmt.Sprintf("jsonrpc %d: %s (%v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// NewRPCError 构造一个可映射成 JSON-RPC error 的错误。
func NewRPCError(code int, msg string, data any) *RPCError {
	return &RPCError{Code: code, Message: msg, Data: data}
}

// ErrConnClosed 表示连接已关闭。
var ErrConnClosed = errors.New("acp: 连接已关闭")

// rpcMessage 同时承载请求、响应与通知。
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// MethodHandler 处理入站请求。返回的值为 result；返回 *RPCError 可自定义错误码。
type MethodHandler func(ctx context.Context, params json.RawMessage) (any, error)

// NotifyHandler 处理入站通知（无响应）。
type NotifyHandler func(ctx context.Context, params json.RawMessage)

// Conn 是一条双向 JSON-RPC 2.0 连接。
//
// 并发语义：Serve 在单独 goroutine 中跑读循环；Call/Notify 可被任意
// goroutine 并发调用；入站请求的 handler 各自在自己的 goroutine 中执行，
// 所以 handler 里可以安全地再次 Call（例如 prompt 处理中发权限请求）。
type Conn struct {
	r      io.Reader
	w      io.Writer
	closer io.Closer
	br     *bufio.Reader

	wmu sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan rpcMessage
	nextID  atomic.Int64

	hmu       sync.RWMutex
	methods   map[string]MethodHandler
	notifiers map[string]NotifyHandler

	reqWG sync.WaitGroup

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}

	errMu sync.Mutex
	err   error
}

// NewConn 用一对读写流构造连接。rwc 可为 nil（不需要 Close 语义时）。
//
// 典型用法是 NewConn(os.Stdin, os.Stdout, nil)，或测试里用 net.Pipe()。
func NewConn(r io.Reader, w io.Writer, closer io.Closer) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{
		r:         r,
		w:         w,
		closer:    closer,
		br:        bufio.NewReaderSize(r, 64*1024),
		pending:   make(map[string]chan rpcMessage),
		methods:   make(map[string]MethodHandler),
		notifiers: make(map[string]NotifyHandler),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
}

// Handle 注册入站请求处理方法。
func (c *Conn) Handle(method string, h MethodHandler) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.methods[method] = h
}

// HandleNotify 注册入站通知处理方法。
func (c *Conn) HandleNotify(method string, h NotifyHandler) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	c.notifiers[method] = h
}

// Done 在连接关闭时关闭。
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err 返回连接终止原因（正常关闭时为 ErrConnClosed）。
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		return ErrConnClosed
	}
	return c.err
}

// Serve 运行读循环，直到连接关闭或出错。它应当只被调用一次。
func (c *Conn) Serve() error {
	for {
		line, err := c.br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrConnClosed
			}
			c.close(err)
			return err
		}
	}
}

// Close 关闭连接并唤醒所有等待中的请求。
func (c *Conn) Close() error {
	c.close(ErrConnClosed)
	return nil
}

func (c *Conn) close(cause error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		c.err = cause
		c.errMu.Unlock()
		c.cancel()
		close(c.done)

		c.pmu.Lock()
		pending := c.pending
		c.pending = make(map[string]chan rpcMessage)
		c.pmu.Unlock()
		for _, ch := range pending {
			close(ch)
		}

		if c.closer != nil {
			_ = c.closer.Close()
		}
	})
}

// dispatch 解析并路由一条消息。它不阻塞：请求会转到独立 goroutine。
func (c *Conn) dispatch(raw []byte) {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return
	}
	// JSON-RPC 2.0 batch 在本协议里没有使用；按单条消息处理。
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.writeMessage(map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      nil,
			"error":   &RPCError{Code: CodeParseError, Message: "parse error", Data: err.Error()},
		})
		return
	}

	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case msg.Method != "" && hasID:
		c.serveRequest(msg)
	case msg.Method != "":
		c.serveNotification(msg)
	case hasID:
		c.resolve(msg)
	}
}

func (c *Conn) serveRequest(msg rpcMessage) {
	c.hmu.RLock()
	h := c.methods[msg.Method]
	c.hmu.RUnlock()
	if h == nil {
		c.writeMessage(map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      msg.ID,
			"error": &RPCError{
				Code:    CodeMethodNotFound,
				Message: "method not found: " + msg.Method,
			},
		})
		return
	}
	c.reqWG.Add(1)
	go func() {
		defer c.reqWG.Done()
		res, err := h(c.ctx, msg.Params)
		if err != nil {
			rpcErr := &RPCError{Code: CodeInternalError, Message: err.Error()}
			var re *RPCError
			if errors.As(err, &re) {
				rpcErr = re
			}
			c.writeMessage(map[string]any{
				"jsonrpc": jsonrpcVersion,
				"id":      msg.ID,
				"error":   rpcErr,
			})
			return
		}
		if res == nil {
			res = struct{}{}
		}
		c.writeMessage(map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      msg.ID,
			"result":  res,
		})
	}()
}

func (c *Conn) serveNotification(msg rpcMessage) {
	c.hmu.RLock()
	h := c.notifiers[msg.Method]
	c.hmu.RUnlock()
	if h == nil {
		return // 未注册的通知按规范忽略
	}
	// 通知必须串行处理：session/update 的顺序就是客户端 UI 的渲染顺序，
	// 用 goroutine 并发处理会让 *_chunk 与 tool_call_update 乱序到达。
	// 通知 handler 因此必须快速、非阻塞（写管道本身有锁，但不阻塞读者）。
	h(c.ctx, msg.Params)
}

func (c *Conn) resolve(msg rpcMessage) {
	key := string(msg.ID)
	c.pmu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.pmu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

// Call 发送请求并等待响应。params 会被 JSON 编码。
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-c.done:
		return nil, c.Err()
	default:
	}

	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan rpcMessage, 1)

	c.pmu.Lock()
	c.pending[key] = ch
	c.pmu.Unlock()

	msg := map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	if err := c.writeMessage(msg); err != nil {
		c.pmu.Lock()
		delete(c.pending, key)
		c.pmu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, c.Err()
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.pmu.Lock()
		delete(c.pending, key)
		c.pmu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}

// CallInto 发送请求并把 result 解码到 out。
func (c *Conn) CallInto(ctx context.Context, method string, params, out any) error {
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Notify 发送通知（不等待响应）。
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	msg := map[string]any{
		"jsonrpc": jsonrpcVersion,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	return c.writeMessage(msg)
}

func (c *Conn) writeMessage(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("acp: 编码消息失败: %w", err)
	}
	data = append(data, '\n')

	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	if _, err := c.w.Write(data); err != nil {
		c.close(fmt.Errorf("acp: 写入失败: %w", err))
		return err
	}
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			b = b[1:]
			continue
		}
		break
	}
	for len(b) > 0 {
		switch b[len(b)-1] {
		case ' ', '\t', '\r', '\n':
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}
