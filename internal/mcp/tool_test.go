package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/leijux/acp-agent/internal/tools"
)

// TestMapResultCountsImageBytes 回归：图片的 base64 体积必须计入总量。
func TestMapResultCountsImageBytes(t *testing.T) {
	data := make([]byte, 50<<10)
	res := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: strings.Repeat("t", 60<<10)},
			&sdkmcp.ImageContent{Data: data, MIMEType: "image/png"},
		},
	}
	out := mapResult(res)
	// 文本 60KiB + 图片 50KiB > 100KiB → 必须触发截断
	if !out.Truncated {
		t.Fatalf("总大小超过上限却没有标记截断（图片字节没被计入）")
	}
}

// TestMapResultDropsOversizedImage 单张超大图要降级成占位文本，不能整段进上下文。
func TestMapResultDropsOversizedImage(t *testing.T) {
	res := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.ImageContent{Data: make([]byte, maxImageBytes+1), MIMEType: "image/png"},
		},
	}
	out := mapResult(res)
	if len(out.Content) != 1 {
		t.Fatalf("块数 %d，期望 1", len(out.Content))
	}
	b := out.Content[0]
	if b.Kind != tools.KindText {
		t.Fatalf("超大图应降级为文本，实际 kind=%v", b.Kind)
	}
	if !strings.Contains(b.Text, "已丢弃") || !strings.Contains(b.Text, "image/png") {
		t.Fatalf("占位文本不对: %q", b.Text)
	}
}

// TestMapResultKeepsNormalImage 正常大小的图片必须原样保留。
func TestMapResultKeepsNormalImage(t *testing.T) {
	data := make([]byte, 8<<10)
	res := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.ImageContent{Data: data, MIMEType: "image/jpeg"},
		},
	}
	out := mapResult(res)
	if len(out.Content) != 1 || out.Content[0].Kind != tools.KindImage {
		t.Fatalf("正常图片被丢弃: %+v", out.Content)
	}
	if out.Content[0].MIMEType != "image/jpeg" || len(out.Content[0].Data) != len(data) {
		t.Fatalf("图片内容被破坏")
	}
}
