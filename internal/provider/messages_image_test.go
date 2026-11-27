package provider

import (
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/agent"
)

// 投影层的行为约定（这些断言是「图片不会静默消失」的唯一保障）：
//
//   - DeepSeek：图片留在 role:tool 的 content 数组里（官方支持，语义最准）；
//   - 通用 OpenAI 兼容端点：tool content 只认字符串，图片后置成一条 user 消息；
//   - 后置位置必须在一整批 tool 消息之后 —— tool 必须连续紧跟 tool_calls；
//   - 模型无视觉能力、格式不支持、超上限时降级为文本说明，不发 image_url。

func projOpts(vision, inline bool) openAIMessageOptions {
	return openAIMessageOptions{
		vision:           vision,
		toolInlineImages: inline,
		maxImageBytes:    maxInlineImageBytes,
		maxImages:        maxRequestImages,
	}
}

func pngBlock(data string) agent.ImageBlock {
	return agent.ImageBlock{Data: data, MimeType: "image/png"}
}

// toolCallMsgs 构造「用户提问 → 模型调 read → 工具返回 content」的三段。
func toolCallMsgs(results ...*agent.ToolResultMessage) []agent.Message {
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{agent.TextBlock{Text: "看看这张图"}}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.png"}},
		}},
	}
	for _, r := range results {
		msgs = append(msgs, r)
	}
	return msgs
}

func toolResult(id, tool string, blocks ...agent.Block) *agent.ToolResultMessage {
	return &agent.ToolResultMessage{ToolCallID: id, ToolName: tool, Content: blocks}
}

// imageURLs 从 content（字符串或 parts 数组）里摘出所有 image_url。
func imageURLs(content any) []string {
	parts, ok := content.([]map[string]any)
	if !ok {
		return nil
	}
	var urls []string
	for _, p := range parts {
		if p["type"] != "image_url" {
			continue
		}
		if iu, ok := p["image_url"].(map[string]any); ok {
			if u, ok := iu["url"].(string); ok {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

func roles(out []map[string]any) []string {
	rs := make([]string, len(out))
	for i, m := range out {
		rs[i], _ = m["role"].(string)
	}
	return rs
}

func TestMessages_ToolImageInlineForDeepSeek(t *testing.T) {
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", pngBlock("AAAA"))), projOpts(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(out), ","); got != "user,assistant,tool" {
		t.Fatalf("roles = %s，期望 user,assistant,tool", got)
	}
	urls := imageURLs(out[2]["content"])
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/png;base64,") {
		t.Fatalf("tool 消息里应内联一张 image_url，实际：%v", out[2]["content"])
	}
}

func TestMessages_ToolImageDeferredToUser(t *testing.T) {
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", pngBlock("AAAA"))), projOpts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(out), ","); got != "user,assistant,tool,user" {
		t.Fatalf("roles = %s，期望末尾追加一条 user", got)
	}
	// tool 消息必须是字符串（通用端点只认这个），且不能是空的。
	text, ok := out[2]["content"].(string)
	if !ok || text == "" {
		t.Fatalf("tool content 应为非空字符串，实际 %#v", out[2]["content"])
	}
	urls := imageURLs(out[3]["content"])
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/png;base64,") {
		t.Fatalf("后置 user 消息应带一张图，实际：%v", out[3]["content"])
	}
}

// 一轮并行多个工具：图片必须整批后置，不能插在 tool 之间（否则服务端拒绝）。
func TestMessages_DeferredImagesStayAfterToolBatch(t *testing.T) {
	msgs := toolCallMsgs(
		toolResult("c1", "read", pngBlock("AAAA")),
		toolResult("c2", "read", pngBlock("BBBB")),
	)
	out, err := openAIMessages("", msgs, projOpts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(out), ","); got != "user,assistant,tool,tool,user" {
		t.Fatalf("roles = %s，期望两条 tool 连续后再后置 user", got)
	}
	if urls := imageURLs(out[4]["content"]); len(urls) != 2 {
		t.Fatalf("后置消息应含 2 张图，实际 %v", out[4]["content"])
	}
}

func TestMessages_ImageDroppedWithoutVision(t *testing.T) {
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", pngBlock("AAAA"))), projOpts(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(out), ","); got != "user,assistant,tool" {
		t.Fatalf("roles = %s，无视觉能力时不应多出 user 消息", got)
	}
	text, _ := out[2]["content"].(string)
	if !strings.Contains(text, "不支持图片输入") {
		t.Fatalf("应说明图片被省略，实际：%q", text)
	}
	if len(imageURLs(out[2]["content"])) != 0 {
		t.Fatalf("无视觉能力时不能发 image_url：%v", out[2]["content"])
	}
}

// 上游只认 JPEG/PNG/GIF/WebP；read 工具会给 image/bmp，必须降级而不是发出去换 400。
func TestMessages_UnsupportedFormatDropped(t *testing.T) {
	bmp := agent.ImageBlock{Data: "AAAA", MimeType: "image/bmp"}
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", bmp)), projOpts(true, true))
	if err != nil {
		t.Fatal(err)
	}
	text, ok := out[2]["content"].(string)
	if !ok {
		t.Fatalf("只有不支持的图时 tool content 应是字符串，实际 %#v", out[2]["content"])
	}
	if !strings.Contains(text, "不受支持") || !strings.Contains(text, "image/bmp") {
		t.Fatalf("应写明格式不受支持，实际：%q", text)
	}
}

// 空文本的 tool 消息不能变成空 content（模型会以为工具什么都没返回）。
func TestMessages_ImageOnlyToolGetsPlaceholder(t *testing.T) {
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", pngBlock("AAAA"))), projOpts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := out[2]["content"].(string)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("tool content 为空，图片信息就彻底丢了")
	}
}

// 闸门：超总量的图降级为说明，请求继续发。
func TestMessages_ImageBudgetGate(t *testing.T) {
	opts := projOpts(true, false)
	opts.maxImageBytes = 10 // 每张 8 字节，第二张必然超
	msgs := toolCallMsgs(
		toolResult("c1", "read", pngBlock("ABCDEFGH")),
		toolResult("c2", "read", pngBlock("IJKLMNOP")),
	)
	out, err := openAIMessages("", msgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	if urls := imageURLs(out[4]["content"]); len(urls) != 1 {
		t.Fatalf("应只保留 1 张图，实际 %v", urls)
	}
	if text, _ := out[3]["content"].(string); !strings.Contains(text, "总量超限") {
		t.Fatalf("被丢的图要留下说明，实际：%q", text)
	}
}

func TestMessages_ImageCountGate(t *testing.T) {
	opts := projOpts(true, false)
	opts.maxImages = 1
	msgs := toolCallMsgs(
		toolResult("c1", "read", pngBlock("AAAA")),
		toolResult("c2", "read", pngBlock("BBBB")),
	)
	out, err := openAIMessages("", msgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	if urls := imageURLs(out[4]["content"]); len(urls) != 1 {
		t.Fatalf("应只保留 1 张图，实际 %v", urls)
	}
}

// assistant 消息带图会让 DeepSeek 直接 400，投影层必须永不产出。
func TestMessages_AssistantNeverCarriesImage(t *testing.T) {
	msgs := []agent.Message{
		&agent.AssistantMessage{Content: []agent.Block{
			agent.TextBlock{Text: "给你看个图"},
			agent.ImageBlock{Data: "AAAA", MimeType: "image/png"},
		}},
	}
	out, err := openAIMessages("", msgs, projOpts(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(imageURLs(out[0]["content"])) != 0 {
		t.Fatalf("assistant 消息不能带 image_url：%v", out[0]["content"])
	}
	if _, ok := out[0]["content"].(string); !ok {
		t.Fatalf("assistant content 应为纯文本，实际 %#v", out[0]["content"])
	}
}

// image/jpg 不是合法 MIME，必须归一化成 image/jpeg 再拼 data URL。
func TestMessages_MIMENormalized(t *testing.T) {
	jpg := agent.ImageBlock{Data: "AAAA", MimeType: "image/jpg"}
	out, err := openAIMessages("", toolCallMsgs(toolResult("c1", "read", jpg)), projOpts(true, true))
	if err != nil {
		t.Fatal(err)
	}
	urls := imageURLs(out[2]["content"])
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/jpeg;base64,") {
		t.Fatalf("MIME 未归一化：%v", urls)
	}
}

// 用户消息里的图片不经过闸门逻辑之外的处理：仍按 parts 数组发送。
func TestMessages_UserImageStillParts(t *testing.T) {
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{
			agent.TextBlock{Text: "这是什么"},
			pngBlock("AAAA"),
		}},
	}
	out, err := openAIMessages("", msgs, projOpts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if urls := imageURLs(out[0]["content"]); len(urls) != 1 {
		t.Fatalf("用户图片应保留为 image_url，实际 %v", out[0]["content"])
	}
}

// 用户消息的图片也过同一道闸门：客户端塞 BMP / 无视觉模型时不该发出去。
func TestMessages_UserImageDroppedForUnsupportedFormat(t *testing.T) {
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{
			agent.TextBlock{Text: "看这个"},
			agent.ImageBlock{Data: "AAAA", MimeType: "image/bmp"},
		}},
	}
	out, err := openAIMessages("", msgs, projOpts(true, false))
	if err != nil {
		t.Fatal(err)
	}
	content, ok := out[0]["content"].(string)
	if !ok {
		t.Fatalf("没有能发的图时应退回纯字符串，实际 %#v", out[0]["content"])
	}
	if !strings.Contains(content, "看这个") || !strings.Contains(content, "不受支持") {
		t.Fatalf("应保留正文并说明格式不支持，实际：%q", content)
	}
}

func TestMessages_UserImageDroppedWithoutVision(t *testing.T) {
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{
			agent.TextBlock{Text: "看这个"},
			pngBlock("AAAA"),
		}},
	}
	out, err := openAIMessages("", msgs, projOpts(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(imageURLs(out[0]["content"])) != 0 {
		t.Fatalf("无视觉能力时不能发 image_url：%#v", out[0]["content"])
	}
}

// 用户消息与工具图片共用配额：同一请求里两处都要计入。
func TestMessages_GateSharedAcrossUserAndTool(t *testing.T) {
	opts := projOpts(true, false)
	opts.maxImages = 1
	msgs := []agent.Message{
		&agent.UserMessage{Content: []agent.Block{pngBlock("AAAA")}},
		&agent.AssistantMessage{Content: []agent.Block{
			agent.ToolCallBlock{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a.png"}},
		}},
		toolResult("c1", "read", pngBlock("BBBB")),
	}
	out, err := openAIMessages("", msgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	total := len(imageURLs(out[0]["content"]))
	for i, m := range out {
		if i == 0 {
			continue
		}
		if m["role"] == "user" {
			total += len(imageURLs(m["content"]))
		}
	}
	if total != 1 {
		t.Fatalf("配额应跨用户/工具共享，实际发了 %d 张", total)
	}
}
