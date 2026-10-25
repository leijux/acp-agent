// Package tools 提供 Go 版 pi agent 的基础工具集。
//
// 设计目标（对齐 Pi 的 tool 契约）：
//   - 每个工具显式声明是否会修改文件系统/进程状态（Mutation），
//     这决定它能否与其它工具并发执行；
//   - 工具结果带截断预算（MaxBytes/MaxLines），避免把大输出灌进模型上下文；
//   - 参数用 JSON Schema（map[string]any）描述，便于直接喂给 LLM；
//   - Abort/超时通过 context 传递，工具内部必须尊重 ctx.Done()。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool 是单个工具的实现。
type Tool interface {
	// Name 是模型调用时使用的唯一名称。
	Name() string
	// Description 是给模型看的说明（何时用、何时不用）。
	Description() string
	// Parameters 返回 JSON Schema 对象（type: object, properties, required ...）。
	Parameters() map[string]any
	// Execute 执行工具。args 是模型传来的已解析 JSON 参数。
	Execute(ctx context.Context, args Args) (Result, error)
	// ConcurrencySafe 表示该工具只读、可与其它并发安全工具同时执行。
	// 返回 false 的工具会被串行调度（见 Executor）。
	ConcurrencySafe() bool
}

// Mutation 由「可能修改文件系统/进程」的工具实现，
// 用于让 Executor 决定调度：改动型工具独占执行。
type Mutation interface {
	Mutates() bool
}

// PermissionMutation 是 Mutation 的「权限视角」版本：调度上安全、权限上算改动。
//
// 典型是 bash：多条只读命令并行是刻意保留的能力（ConcurrencySafe=true），
// 但 shell 本身能写文件、跑 git、删目录，所以只读/ask 模式必须把它当改动型工具拦。
//
// 只实现 Mutation 的工具两个视角一致；只实现本接口的工具只影响权限判定。
type PermissionMutation interface {
	PermissionMutates() bool
}

// PathScoped 由「自带路径级互斥」的改动型工具实现。
//
// 语义：工具会改文件（Mutation 仍返回 true，权限层照旧按改动处理），
// 但内部已用 lockFile 把「同一路径的读-改-写」串行化，所以调度上允许
// 与其它调用并行执行 —— 不同路径真正并行，同路径互斥。
//
// 不实现本接口的改动型工具仍然独占整个批次（例如 delete/move）。
type PathScoped interface {
	PathScoped() bool
}

// IsPathScoped 报告 t 是否自带路径级互斥。
func IsPathScoped(t Tool) bool {
	s, ok := t.(PathScoped)
	return ok && s.PathScoped()
}

// ContentKind 区分结果块类型。
type ContentKind string

const (
	KindText  ContentKind = "text"
	KindImage ContentKind = "image"
)

// Block 是一段工具输出。多块结果（如 read 的文本 + 图片）按顺序拼接。
type Block struct {
	Kind     ContentKind `json:"type"`
	Text     string      `json:"text,omitempty"`
	Data     string      `json:"data,omitempty"`      // image: base64
	MIMEType string      `json:"mimeType,omitempty"`  // image
	Start    int         `json:"startLine,omitempty"` // read: 起始行
	End      int         `json:"endLine,omitempty"`   // read: 结束行
}

// Result 是工具执行结果。
type Result struct {
	Content []Block `json:"content"`
	// Details 是结构化附加信息（供 UI/程序消费，模型不直接看到）。
	Details any `json:"details,omitempty"`
	// IsError 表示工具逻辑失败（不是 Go error）。这类错误通常回传给模型让它重试。
	IsError bool `json:"isError,omitempty"`
	// Truncated 表示输出被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// TextResult 构造单块文本结果。
func TextResult(format string, a ...any) Result {
	return Result{Content: []Block{{Kind: KindText, Text: fmt.Sprintf(format, a...)}}}
}

// ErrorResult 构造错误结果（模型可见）。
func ErrorResult(format string, a ...any) Result {
	return Result{
		Content: []Block{{Kind: KindText, Text: fmt.Sprintf(format, a...)}},
		IsError: true,
	}
}

// Text 返回拼接后的纯文本（忽略图片块），便于测试与日志。
func (r Result) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Kind == KindText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// JSONSchema 相关构造辅助，保持 schema 书写简洁。

// Schema 构造一个 object schema。
// Schema 构造一个 JSON Schema 对象。
//
// 零参数工具传 nil 时，properties 会显式序列化成空对象：`"properties": null`
// 会被 OpenAI 一类的严格校验拒绝（`null is not of type "object"`）。
func Schema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Prop 构造一个属性 schema。
func Prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// PropEnum 构造枚举属性。
func PropEnum(typ, desc string, values ...string) map[string]any {
	return map[string]any{"type": typ, "description": desc, "enum": values}
}

// PropArray 构造数组属性。
func PropArray(itemType, desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": itemType},
	}
}

// PropInt 构造整数属性，带默认值说明。
func PropInt(desc string, def int64) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": desc,
		"default":     def,
	}
}

// Args 是参数读取辅助，统一错误信息。
type Args map[string]any

// String 读取字符串参数。
func (a Args) String(key string) (string, bool) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Str 读取字符串参数，缺失返回 def。
func (a Args) Str(key, def string) string {
	if s, ok := a.String(key); ok {
		return s
	}
	return def
}

// Required 读取必填字符串，报错信息面向模型。
func (a Args) Required(key string) (string, error) {
	s, ok := a.String(key)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("missing required parameter %q", key)
	}
	return s, nil
}

// Int 读取整数参数（JSON 数字可能是 float64 / json.Number / int）。
func (a Args) Int(key string, def int64) int64 {
	v, ok := a[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return i
	default:
		return def
	}
}

// Bool 读取布尔参数。
func (a Args) Bool(key string, def bool) bool {
	v, ok := a[key]
	if !ok || v == nil {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// StrSlice 读取字符串数组参数。
func (a Args) StrSlice(key string) []string {
	v, ok := a[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// CtxError 统一把 ctx 取消转换成工具可见的错误。
func CtxError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("canceled: %w", err)
	}
	return nil
}
