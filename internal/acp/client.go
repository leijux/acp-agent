package acp

import (
	"context"
	"errors"
)

// Client 是 Agent 侧对客户端能力的封装（JSON-RPC 反向调用）。
//
// 所有方法都假设调用方已经确认对应能力位为真；未确认而调用时，
// 客户端会返回 method not found，这里会原样向上传递错误。
type Client struct {
	conn *Conn
	caps ClientCapabilities
	info *Implementation
}

// NewClient 基于连接构造客户端调用封装。
func NewClient(conn *Conn, caps ClientCapabilities, info *Implementation) *Client {
	return &Client{conn: conn, caps: caps, info: info}
}

// Capabilities 返回 initialize 时协商到的能力。
func (c *Client) Capabilities() ClientCapabilities { return c.caps }

// Info 返回客户端自报的实现信息。
func (c *Client) Info() *Implementation { return c.info }

// RequestPermission 请求用户授权一次工具调用。
func (c *Client) RequestPermission(ctx context.Context, sessionID string, tc ToolCallUpdate, options []PermissionOption) (*RequestPermissionOutcome, error) {
	if len(options) == 0 {
		options = DefaultPermissionOptions()
	}
	params := RequestPermissionParams{
		SessionID: sessionID,
		ToolCall:  tc,
		Options:   options,
	}
	var out RequestPermissionResult
	if err := c.conn.CallInto(ctx, MethodSessionRequestPr, params, &out); err != nil {
		return nil, err
	}
	return &out.Outcome, nil
}

// DefaultPermissionOptions 是标准四选项：允许一次/总是、拒绝一次/总是。
func DefaultPermissionOptions() []PermissionOption {
	return []PermissionOption{
		{OptionID: "allow-once", Name: "允许一次", Kind: PermAllowOnce},
		{OptionID: "allow-always", Name: "总是允许", Kind: PermAllowAlways},
		{OptionID: "reject-once", Name: "拒绝", Kind: PermRejectOnce},
		{OptionID: "reject-always", Name: "总是拒绝", Kind: PermRejectAlways},
	}
}

// ReadTextFile 通过客户端读取文件（要求 caps.FS.ReadTextFile）。
func (c *Client) ReadTextFile(ctx context.Context, sessionID, path string, line, limit int) (string, error) {
	if !c.caps.FS.ReadTextFile {
		return "", errors.New("acp: 客户端不支持 fs/read_text_file")
	}
	params := FsReadTextFileParams{SessionID: sessionID, Path: path, Line: line, Limit: limit}
	var out FsReadTextFileResult
	if err := c.conn.CallInto(ctx, MethodFsReadTextFile, params, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// WriteTextFile 通过客户端写入文件（要求 caps.FS.WriteTextFile）。
func (c *Client) WriteTextFile(ctx context.Context, sessionID, path, content string) error {
	if !c.caps.FS.WriteTextFile {
		return errors.New("acp: 客户端不支持 fs/write_text_file")
	}
	params := FsWriteTextFileParams{SessionID: sessionID, Path: path, Content: content}
	return c.conn.CallInto(ctx, MethodFsWriteTextFile, params, nil)
}

// TerminalCreate 创建一个客户端侧终端（要求 caps.Terminal）。
func (c *Client) TerminalCreate(ctx context.Context, p TerminalCreateParams) (string, error) {
	if !c.caps.Terminal {
		return "", errors.New("acp: 客户端不支持 terminal")
	}
	var out TerminalCreateResult
	if err := c.conn.CallInto(ctx, MethodTerminalCreate, p, &out); err != nil {
		return "", err
	}
	return out.TerminalID, nil
}

// TerminalOutput 读取终端当前输出。
func (c *Client) TerminalOutput(ctx context.Context, sessionID, terminalID string) (*TerminalOutputResult, error) {
	var out TerminalOutputResult
	err := c.conn.CallInto(ctx, MethodTerminalOutput,
		TerminalOutputParams{SessionID: sessionID, TerminalID: terminalID}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TerminalWaitForExit 等待终端退出并返回最后的输出。
func (c *Client) TerminalWaitForExit(ctx context.Context, sessionID, terminalID string) (*TerminalOutputResult, error) {
	var out TerminalOutputResult
	err := c.conn.CallInto(ctx, MethodTerminalWaitFor,
		TerminalWaitForExitParams{SessionID: sessionID, TerminalID: terminalID}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TerminalKill 杀死终端进程（不释放）。
func (c *Client) TerminalKill(ctx context.Context, sessionID, terminalID string) error {
	return c.conn.CallInto(ctx, MethodTerminalKill,
		TerminalIDParams{SessionID: sessionID, TerminalID: terminalID}, nil)
}

// TerminalRelease 释放终端资源。
func (c *Client) TerminalRelease(ctx context.Context, sessionID, terminalID string) error {
	return c.conn.CallInto(ctx, MethodTerminalRelease,
		TerminalIDParams{SessionID: sessionID, TerminalID: terminalID}, nil)
}

// Call 暴露底层连接，供自定义扩展方法（_ 前缀）使用。
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	return c.conn.CallInto(ctx, method, params, out)
}
