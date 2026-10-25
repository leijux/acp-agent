package agent

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"gitlab.com/leijux/acp-agent/internal/kernel"
)

// 本文件把 Agent 的会话历史桥接到 kernel（billion-context 的 acp-kernel）。
//
// 分工：
//   - kernel 决定「何时压、压哪一段、压完还剩多少」（nudge / refs / 块管理）；
//   - Agent 只负责把区间内的消息交给模型生成摘要，再把结果记成 compaction entry。
//
// 对齐方式：kernel.Message.ID 直接使用 entry ID，这样 kernel 返回的
// Block.DirectMessageIDs 可以原样当作 Session 的 BlockedIDs 使用。

// sharedKernel 是进程级共享的 Kernel 实例：bundle 只加载一次（约 30ms），
// 每个 Agent 各持一个 kernel.Session（state 独立，互不影响）。
var sharedKernel struct {
	once sync.Once
	k    *kernel.Kernel
	err  error
}

func getKernel() (*kernel.Kernel, error) {
	sharedKernel.once.Do(func() {
		sharedKernel.k, sharedKernel.err = kernel.New()
	})
	return sharedKernel.k, sharedKernel.err
}

// kernelEngine 是某个 Agent 的 kernel 会话。
type kernelEngine struct {
	s     *kernel.Session
	limit int

	// mirrored 记录已经写进 Session entry 的块 id，避免重复 append。
	mirrored map[string]bool
}

func newKernelEngine(limit int, state []byte) (*kernelEngine, error) {
	k, err := getKernel()
	if err != nil {
		return nil, err
	}
	ks, err := k.NewSession(kernel.Config{ModelContextLimit: limit})
	if err != nil {
		return nil, err
	}
	if len(state) > 0 {
		if err := ks.LoadState(state); err != nil {
			_ = ks.Close()
			return nil, fmt.Errorf("agent: 恢复 kernel 状态失败: %w", err)
		}
	}
	return &kernelEngine{s: ks, limit: limit, mirrored: map[string]bool{}}, nil
}

// kernelEngine 返回（必要时创建）本 Agent 的 kernel 会话。
//
// ContextWindow 变化时重建 session 并回灌状态，避免 kernel 用旧的 limit 判阈值。
func (a *Agent) kernelEngine() (*kernelEngine, error) {
	a.kernelMu.Lock()
	defer a.kernelMu.Unlock()

	limit := a.opts.ContextWindow
	if a.kernel != nil && a.kernel.limit == limit {
		return a.kernel, nil
	}

	var state []byte
	if a.kernel != nil {
		state, _ = a.kernel.s.StateJSON()
		_ = a.kernel.s.Close()
		a.kernel = nil
	} else {
		state = a.opts.Session.KernelState()
	}

	ke, err := newKernelEngine(limit, state)
	if err != nil {
		return nil, err
	}
	ke.syncMirrored(a.opts.Session)
	a.kernel = ke
	return ke, nil
}

// syncMirrored 从已有 compaction entry 里恢复「已镜像的块」。
func (e *kernelEngine) syncMirrored(s *Session) {
	for _, en := range s.Entries() {
		if en.Type == EntryCompaction && en.BlockID != "" {
			e.mirrored[en.BlockID] = true
		}
	}
}

// Close 释放 kernel 会话（Agent 关闭时调用；进程级 Kernel 保留复用）。
func (a *Agent) closeKernel() {
	a.kernelMu.Lock()
	defer a.kernelMu.Unlock()
	if a.kernel != nil {
		_ = a.kernel.s.Close()
		a.kernel = nil
	}
}

// kernelMessages 把活动分支的 entry 转成 kernel 视角的消息。
//
// 只喂「原始消息 entry」：压缩摘要 entry 与分支摘要 entry 跳过，因为块本身
// 由 kernel 维护；把摘要再喂进去会让 kernel 把摘要当正文重复计算。
func kernelMessages(entries []*Entry) []kernel.Message {
	out := make([]kernel.Message, 0, len(entries))
	for _, e := range entries {
		if e.Type != EntryMessage || e.Message == nil {
			continue
		}
		if m, ok := toKernelMessage(e.ID, e.Message); ok {
			out = append(out, m)
		}
	}
	return out
}

func toKernelMessage(id string, msg Message) (kernel.Message, bool) {
	switch m := msg.(type) {
	case *UserMessage:
		return kernel.Message{
			ID: id, Role: kernel.RoleUser, ContentType: kernel.ContentText,
			Text: blocksText(m.Content),
		}, true

	case *AssistantMessage:
		var b strings.Builder
		b.WriteString(blocksText(m.Content))
		for _, blk := range m.Content {
			if tc, ok := blk.(ToolCallBlock); ok {
				b.WriteString("\n[tool-call] ")
				b.WriteString(tc.Name)
				b.WriteString(formatArgs(tc.Arguments))
			}
		}
		text := strings.TrimSpace(b.String())
		if text == "" && m.ErrorMessage != "" {
			text = "[error] " + m.ErrorMessage
		}
		return kernel.Message{
			ID: id, Role: kernel.RoleAssistant, ContentType: kernel.ContentText,
			Text: text,
		}, true

	case *ToolResultMessage:
		text := blocksText(m.Content)
		if m.IsError {
			text = "[error] " + text
		}
		return kernel.Message{
			ID: id, Role: kernel.RoleTool, ContentType: kernel.ContentToolResult,
			Text: text, ToolName: m.ToolName, ToolCallID: m.ToolCallID,
		}, true

	case *BashExecutionMessage:
		if m.ExcludeFromContext {
			return kernel.Message{}, false
		}
		return kernel.Message{
			ID: id, Role: kernel.RoleUser, ContentType: kernel.ContentText,
			Text: "$ " + m.Command + "\n" + m.Output,
		}, true

	case *CustomMessage:
		return kernel.Message{
			ID: id, Role: kernel.RoleUser, ContentType: kernel.ContentText,
			Text: blocksText(m.Content),
		}, true
	}
	return kernel.Message{}, false
}

// transcript 把 kernel 渲染视图里的消息拼成给模型看的会话文本。
func transcript(msgs []kernel.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("[")
		b.WriteString(string(m.Role))
		if m.ContentType == kernel.ContentToolResult && m.ToolName != "" {
			b.WriteString(":")
			b.WriteString(m.ToolName)
		}
		b.WriteString("] ")
		if m.Ref != "" {
			b.WriteString(m.Ref)
			b.WriteString(" ")
		}
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// refOrdinal 解析 "m00012" → 12；无法解析返回 -1。
func refOrdinal(ref string) int {
	if len(ref) < 2 || ref[0] != 'm' {
		return -1
	}
	n, err := strconv.Atoi(ref[1:])
	if err != nil {
		return -1
	}
	return n
}

// selectRefRange 取出 [start, end] 引用区间内的消息。
func selectRefRange(msgs []kernel.Message, start, end string) []kernel.Message {
	lo, hi := refOrdinal(start), refOrdinal(end)
	if lo < 0 || hi < 0 {
		return nil
	}
	var out []kernel.Message
	for _, m := range msgs {
		if n := refOrdinal(m.Ref); n >= lo && n <= hi {
			out = append(out, m)
		}
	}
	return out
}
