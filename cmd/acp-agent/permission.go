package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/leijux/acp-agent/internal/tools"
)

// permMode 决定工具调用是否需要人工确认。
type permMode int

const (
	// modeAsk：改动型工具逐个询问。
	modeAsk permMode = iota
	// modeAuto：全部放行（等价 --yolo）。
	modeAuto
	// modeReadonly：禁止一切改动型工具。
	modeReadonly
)

func parsePermMode(s string) (permMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "ask":
		return modeAsk, nil
	case "auto", "allow", "yolo":
		return modeAuto, nil
	case "readonly", "read-only", "ro":
		return modeReadonly, nil
	default:
		return modeAsk, fmt.Errorf("未知 permissions 值 %q（可选 ask/auto/readonly）", s)
	}
}

// choice 是一次权限问答的结果。
type choice int

const (
	choiceYes choice = iota
	choiceAlways
	choiceNo
	choiceNever
)

// guard 把权限策略接进工具执行链路。
//
// 与 internal/acp 的 Guard 是同一套语义，但这里的应答方是终端用户：
// 询问走同一个（被 REPL 共享的）输入源，所以必须串行化。
type guard struct {
	mode permMode

	mu       sync.Mutex
	reader   *bufio.Reader
	out      io.Writer
	always   map[string]bool
	never    map[string]bool
	promptMu sync.Mutex
}

func newGuard(mode permMode, reader *bufio.Reader, out io.Writer) *guard {
	return &guard{
		mode:   mode,
		reader: reader,
		out:    out,
		always: map[string]bool{},
		never:  map[string]bool{},
	}
}

// errDenied 表示用户拒绝；调用方把它变成工具结果而不是中断 turn。
type errDenied struct{ tool string }

func (e errDenied) Error() string { return fmt.Sprintf("用户拒绝执行 %s", e.tool) }

// authorize 决定是否允许执行某个工具。
func (g *guard) authorize(ctx context.Context, name string, mutates bool) error {
	if g == nil {
		return nil
	}
	switch g.mode {
	case modeAuto:
		return nil
	case modeReadonly:
		if mutates {
			return errDenied{tool: name}
		}
		return nil
	}

	if !mutates {
		return nil // ask 模式只拦改动型工具
	}

	g.mu.Lock()
	allowed, denied := g.always[name], g.never[name]
	g.mu.Unlock()
	if allowed {
		return nil
	}
	if denied {
		return errDenied{tool: name}
	}

	c, err := g.ask(ctx, name)
	if err != nil {
		return err
	}
	switch c {
	case choiceAlways:
		g.mu.Lock()
		g.always[name] = true
		g.mu.Unlock()
		return nil
	case choiceNever:
		g.mu.Lock()
		g.never[name] = true
		g.mu.Unlock()
		return errDenied{tool: name}
	case choiceYes:
		return nil
	default:
		return errDenied{tool: name}
	}
}

// ask 在终端上问一次。问句与候选单行显示，避免把流式输出搅乱。
func (g *guard) ask(ctx context.Context, name string) (choice, error) {
	g.promptMu.Lock()
	defer g.promptMu.Unlock()

	if err := ctx.Err(); err != nil {
		return choiceNo, err
	}
	fmt.Fprintf(g.out, "\n允许执行 %s？[y]是 / [a]总是 / [n]否 / [d]永不 (默认 n) ", name)

	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := g.reader.ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return choiceNo, ctx.Err()
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return choiceNo, r.err
		}
		switch strings.ToLower(strings.TrimSpace(r.line)) {
		case "y", "yes", "是":
			return choiceYes, nil
		case "a", "always", "总是":
			return choiceAlways, nil
		case "d", "never", "永不":
			return choiceNever, nil
		default:
			return choiceNo, nil
		}
	}
}

// ---------- 工具包装 ----------

// guardedTool 在原工具外面加一层授权检查。
//
// 三个必须转发的东西：
//   - Mutation.Mutates：Executor 靠它决定能否并行；
//   - PermissionMutation.PermissionMutates：权限层单独判断（bash 就是这种：
//     调度上可并行，但只读模式必须拦）；
//   - ConcurrencySafe：被拦下的写工具不该参与并发批次。
type guardedTool struct {
	inner tools.Tool
	g     *guard
	// mutates 只影响调度（能否与其它调用并行）。
	mutates bool
	// permMutates 影响授权判定，缺省跟随 mutates。
	permMutates bool
}

func wrap(t tools.Tool, g *guard) tools.Tool {
	mutates := false
	if m, ok := t.(tools.Mutation); ok {
		mutates = m.Mutates()
	}
	// 关键：bash 没有 Mutation（调度上保持可并行），但实现了
	// PermissionMutation —— 只看 Mutation 会让只读模式放行 shell。
	permMutates := mutates
	if pm, ok := t.(tools.PermissionMutation); ok {
		permMutates = pm.PermissionMutates()
	}
	return &guardedTool{inner: t, g: g, mutates: mutates, permMutates: permMutates}
}

func (t *guardedTool) Name() string               { return t.inner.Name() }
func (t *guardedTool) Description() string        { return t.inner.Description() }
func (t *guardedTool) Parameters() map[string]any { return t.inner.Parameters() }
func (t *guardedTool) Mutates() bool              { return t.mutates }

func (t *guardedTool) ConcurrencySafe() bool {
	// 改动型工具默认不参与并发；自带路径级互斥的（write/edit）例外。
	if t.mutates && !tools.IsPathScoped(t.inner) {
		return false
	}
	return t.inner.ConcurrencySafe()
}

func (t *guardedTool) PathScoped() bool { return tools.IsPathScoped(t.inner) }

func (t *guardedTool) Execute(ctx context.Context, args tools.Args) (tools.Result, error) {
	if err := t.g.authorize(ctx, t.inner.Name(), t.permMutates); err != nil {
		// 拒绝是「工具结果」，让模型自己换个办法；只有 ctx 取消才向上抛。
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return tools.Result{
			Content: []tools.Block{{
				Kind: tools.KindText,
				Text: err.Error() + "。请改用其它方式，或先询问用户。",
			}},
			IsError: true,
		}, nil
	}
	return t.inner.Execute(ctx, args)
}

// wrapRegistry 返回一个每个工具都被权限门禁包住的 Registry 视图。
//
// 用视图而不是拷贝：MCP server 会通过 tools/list_changed 动态增删工具，
// 拷贝快照会让新增的工具模型看不到、撤回的工具还留着。
func wrapRegistry(base *tools.Registry, g *guard) *tools.Registry {
	out := tools.NewRegistry()
	out.UseBase(base, func(t tools.Tool) tools.Tool { return wrap(t, g) })
	return out
}
