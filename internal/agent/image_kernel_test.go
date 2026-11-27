package agent

import (
	"strings"
	"testing"
)

// 压缩视图（kernel）只吃文本：图片必须留下占位行，否则压缩摘要里
// 「这个会话给过一张截图」这件事会彻底消失，模型还会以为工具什么都没返回。

func TestBlocksForKernel_UserImagePlaceholder(t *testing.T) {
	got := blocksForKernel([]Block{
		TextBlock{Text: "这张截图里是什么"},
		ImageBlock{Data: strings.Repeat("A", 4000), MimeType: "image/png"},
	})
	if !strings.Contains(got, "这张截图里是什么") {
		t.Fatalf("正文丢了：%q", got)
	}
	if !strings.Contains(got, "[图片：image/png") || !strings.Contains(got, "KB]") {
		t.Fatalf("缺图片占位：%q", got)
	}
}

func TestBlocksForKernel_ImageOnly(t *testing.T) {
	got := blocksForKernel([]Block{ImageBlock{Data: "AAAA", MimeType: "image/jpeg"}})
	if !strings.HasPrefix(got, "[图片：image/jpeg") {
		t.Fatalf("只有图片时应给占位，实际：%q", got)
	}
}

func TestToKernelMessage_ToolResultImageVisible(t *testing.T) {
	msg := &ToolResultMessage{
		ToolCallID: "c1", ToolName: "read",
		Content: []Block{ImageBlock{Data: "AAAA", MimeType: "image/png"}},
	}
	km, ok := toKernelMessage("m00001", msg)
	if !ok {
		t.Fatal("tool 结果应能转换")
	}
	if !strings.Contains(km.Text, "[图片：image/png") {
		t.Fatalf("压缩视图里看不到图片：%q", km.Text)
	}
}

func TestToKernelMessage_UserTextOnlyUnchanged(t *testing.T) {
	msg := &UserMessage{Content: []Block{TextBlock{Text: "只有文字"}}}
	km, ok := toKernelMessage("m00001", msg)
	if !ok || km.Text != "只有文字" {
		t.Fatalf("纯文本消息不该被改写：%q", km.Text)
	}
}

func TestImageBlocksAndDescribe(t *testing.T) {
	bs := []Block{
		TextBlock{Text: "x"},
		ImageBlock{Data: "AAAA", MimeType: "image/png"},
		&ImageBlock{Data: "BBBB", MimeType: "image/jpeg"},
		ThinkingBlock{Thinking: "略"},
	}
	imgs := ImageBlocks(bs)
	if len(imgs) != 2 {
		t.Fatalf("应取出 2 张图（值 + 指针），实际 %d", len(imgs))
	}
	desc := DescribeImages(imgs)
	if !strings.Contains(desc, "image/png") || !strings.Contains(desc, "image/jpeg") {
		t.Fatalf("描述缺格式：%q", desc)
	}
	if strings.Count(desc, "[图片：") != 2 {
		t.Fatalf("每张图一行：%q", desc)
	}
}

func TestVisionMIMEType(t *testing.T) {
	cases := map[string]string{
		"image/png":  "image/png",
		"image/jpg":  "image/jpeg", // 非标准写法，必须归一化
		"IMAGE/JPEG": "image/jpeg",
		"image/gif":  "image/gif",
		"image/webp": "image/webp",
	}
	for in, want := range cases {
		got, ok := VisionMIMEType(in)
		if !ok || got != want {
			t.Fatalf("VisionMIMEType(%q) = %q,%v，期望 %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"image/bmp", "image/tiff", "image/heic", "application/pdf", ""} {
		if _, ok := VisionMIMEType(bad); ok {
			t.Fatalf("%q 不该被当作视觉输入", bad)
		}
	}
}
