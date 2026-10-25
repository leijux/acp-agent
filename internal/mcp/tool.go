package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leijux/acp-agent/internal/tools"
)

// maxResultBytes 是单次工具结果回传给模型的上限（文本），超出部分截断。
const maxResultBytes = 100 << 10

// maxImageBytes 是单张图片的 base64 上限。
//
// 图片块不参与文本截断（截断 base64 会得到一张坏图），必须单独设上限：
// 早先 mapResult 只统计文本字节，一张 5MB 的图能整段进上下文。
const maxImageBytes = 2 << 20

// toolNamePrefix 是 MCP 工具在 Registry 中的命名空间前缀。
const toolNamePrefix = "mcp__"

// newToolAdapter 把 SDK 的 *mcp.Tool 包装成 internal/tools.Tool。
func newToolAdapter(client *Client, def *sdkmcp.Tool) *remoteTool {
	readOnly := def.Annotations != nil && def.Annotations.ReadOnlyHint
	return &remoteTool{
		client:   client,
		def:      def,
		name:     ToolName(client.name, def.Name),
		schema:   normalizeSchema(def.InputSchema),
		readOnly: readOnly,
	}
}

// ToolName 生成带命名空间的工具名：mcp__<server>__<tool>。
//
// 模型侧工具名只允许 [A-Za-z0-9_-]，所以 server 名和工具名里的
// 其他字符统一替换成下划线。同一 server 内重名由 Registry 注册时报错。
func ToolName(server, tool string) string {
	return toolNamePrefix + sanitize(server) + "__" + sanitize(tool)
}

// IsMCPTool 判断工具名是否由本包生成。
func IsMCPTool(name string) bool { return strings.HasPrefix(name, toolNamePrefix) }

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unnamed"
	}
	return out
}

// normalizeSchema 把任意 schema 表达（nil / map / json.RawMessage / struct）
// 归一化成 tools 包要求的 map[string]any，并补齐零参数工具的 properties。
//
// 上游 server 常把 inputSchema 写成 {"type":"object"} 而省略 properties，
// 直接把 nil 塞给模型会生成 {"properties":null}，OpenAI 会以
// "null is not of type object" 拒绝请求。
func normalizeSchema(v any) map[string]any {
	var m map[string]any
	switch s := v.(type) {
	case nil:
		m = nil
	case map[string]any:
		m = s
	case json.RawMessage:
		if len(s) > 0 {
			_ = json.Unmarshal(s, &m)
		}
	case []byte:
		if len(s) > 0 {
			_ = json.Unmarshal(s, &m)
		}
	default:
		if data, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(data, &m)
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	if props, ok := m["properties"].(map[string]any); !ok || props == nil {
		m["properties"] = map[string]any{}
	}
	return m
}

type remoteTool struct {
	client   *Client
	def      *sdkmcp.Tool
	name     string
	schema   map[string]any
	readOnly bool
}

func (t *remoteTool) Name() string { return t.name }

func (t *remoteTool) Description() string {
	if t.def.Description != "" {
		return t.def.Description
	}
	if t.def.Title != "" {
		return t.def.Title
	}
	return "MCP 工具 " + t.def.Name + "（来自 " + t.client.name + "）"
}

func (t *remoteTool) Parameters() map[string]any { return t.schema }

// ConcurrencySafe 只在服务端显式声明 readOnlyHint 时才允许并行。
func (t *remoteTool) ConcurrencySafe() bool { return t.readOnly }

// Mutates 表示调用可能改变外部状态，需要走权限门禁。
func (t *remoteTool) Mutates() bool { return !t.readOnly }

func (t *remoteTool) Execute(ctx context.Context, args tools.Args) (tools.Result, error) {
	if args == nil {
		args = tools.Args{}
	}
	res, err := t.client.CallTool(ctx, t.def.Name, args)
	if err != nil {
		return tools.Result{}, err
	}
	return mapResult(res), nil
}

// mapResult 把 MCP 的内容块映射成 tools.Result。
func mapResult(res *sdkmcp.CallToolResult) tools.Result {
	if res == nil {
		return tools.Result{Content: []tools.Block{{Kind: tools.KindText, Text: "（MCP 返回空结果）"}}}
	}

	var (
		blocks []tools.Block
		size   int
	)
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdkmcp.TextContent:
			blocks = append(blocks, tools.Block{Kind: tools.KindText, Text: v.Text})
			size += len(v.Text)

		case *sdkmcp.ImageContent:
			if len(v.Data) > maxImageBytes {
				blocks = append(blocks, tools.Block{
					Kind: tools.KindText,
					Text: fmt.Sprintf("[image %s 已丢弃：%d 字节，超过单张上限 %d]",
						v.MIMEType, len(v.Data), maxImageBytes),
				})
				size += 64
				break
			}
			blocks = append(blocks, tools.Block{
				Kind:     tools.KindImage,
				Data:     string(v.Data),
				MIMEType: v.MIMEType,
			})
			// 图片体积也要记账，否则「总输出多大」这个数完全失真。
			size += len(v.Data)

		case *sdkmcp.AudioContent:
			blocks = append(blocks, tools.Block{
				Kind: tools.KindText,
				Text: fmt.Sprintf("[audio %s, %d bytes]", v.MIMEType, len(v.Data)),
			})

		case *sdkmcp.EmbeddedResource:
			blocks = append(blocks, resourceBlock(v.Resource))
			size += len(blocks[len(blocks)-1].Text)

		case *sdkmcp.ResourceLink:
			blocks = append(blocks, tools.Block{
				Kind: tools.KindText,
				Text: fmt.Sprintf("[resource %s %s]", v.URI, v.Name),
			})

		default:
			// 未来新增的内容类型：退化成 JSON 文本，至少不丢信息。
			if data, err := json.Marshal(c); err == nil {
				blocks = append(blocks, tools.Block{Kind: tools.KindText, Text: string(data)})
				size += len(data)
			}
		}
	}

	// 只有 structuredContent 时，序列化成文本给模型看。
	if len(blocks) == 0 && res.StructuredContent != nil {
		if data, err := json.MarshalIndent(res.StructuredContent, "", "  "); err == nil {
			blocks = append(blocks, tools.Block{Kind: tools.KindText, Text: string(data)})
			size += len(data)
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, tools.Block{Kind: tools.KindText, Text: "（MCP 工具没有返回内容）"})
	}

	out := tools.Result{Content: blocks, IsError: res.IsError}
	if res.StructuredContent != nil {
		out.Details = res.StructuredContent
	}

	if size > maxResultBytes {
		out = truncateResult(out, maxResultBytes)
	}
	return out
}

func resourceBlock(r *sdkmcp.ResourceContents) tools.Block {
	if r == nil {
		return tools.Block{Kind: tools.KindText, Text: "[resource 无内容]"}
	}
	switch {
	case r.Text != "":
		return tools.Block{Kind: tools.KindText, Text: fmt.Sprintf("--- %s ---\n%s", r.URI, r.Text)}
	case len(r.Blob) > 0:
		if strings.HasPrefix(r.MIMEType, "image/") {
			return tools.Block{Kind: tools.KindImage, Data: string(r.Blob), MIMEType: r.MIMEType}
		}
		return tools.Block{
			Kind: tools.KindText,
			Text: fmt.Sprintf("--- %s (%s, %d bytes) ---", r.URI, r.MIMEType, len(r.Blob)),
		}
	default:
		return tools.Block{Kind: tools.KindText, Text: fmt.Sprintf("--- %s ---", r.URI)}
	}
}

// truncateResult 保留前 maxResultBytes 字节（按 rune 边界切），并注明省略量。
func truncateResult(res tools.Result, maxBytes int) tools.Result {
	var kept []tools.Block
	total, keptBytes := 0, 0
	for _, b := range res.Content {
		if b.Kind != tools.KindText {
			kept = append(kept, b)
			continue
		}
		total += len(b.Text)
		if keptBytes >= maxBytes {
			continue
		}
		remain := maxBytes - keptBytes
		if len(b.Text) > remain {
			b.Text = safeCut(b.Text, remain)
		}
		keptBytes += len(b.Text)
		kept = append(kept, b)
	}
	trimmed := total - keptBytes
	if trimmed > 0 {
		kept = append(kept, tools.Block{
			Kind: tools.KindText,
			Text: fmt.Sprintf("\n…（MCP 输出过长，已截断 %d 字节；完整内容未保留）", trimmed),
		})
	}
	res.Content = kept
	res.Truncated = true
	return res
}

// safeCut 按字节截断但不切开 UTF-8 字符。
func safeCut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
