package agent

import (
	"strconv"
	"strings"
)

// ---------- 图片块辅助 ----------
//
// 一个 ImageBlock 有三条完全不同的下游，措辞必须共用一份：
//
//  1. 线上请求（internal/provider）——只有 JPEG/PNG/GIF/WebP 能发，其余必须
//     降级成文本；DeepSeek《图像理解》与 OpenAI vision 文档的交集就是这四项
//     （DeepSeek 按文件内容判断格式，不看声明的 MIME，但我们没有解码器，
//     只能按 MIME 收口，宁可少发也不发必错的东西）。
//  2. 压缩视图（internal/agent/kernel.go）——kernel 只吃文本，图片要留占位，
//     否则压缩摘要里"这个会话给过截图"这件事会彻底消失。
//  3. ACP 客户端（internal/acp/update.go）——原样透传，客户端负责显示。

// VisionMIMEType 归一化图片 MIME 并报告它能否进请求体。
//
// 归一化是必需的：`image/jpg` 不是合法 MIME（应为 image/jpeg），某些来源
// （尤其 MCP server）会这么写，直接拼成 data URL 会得到 `data:image/jpg;base64,`，
// 严格的端点可能直接 400。
func VisionMIMEType(mime string) (string, bool) {
	switch m := strings.ToLower(strings.TrimSpace(mime)); m {
	case "image/jpeg", "image/jpg":
		return "image/jpeg", true
	case "image/png", "image/gif", "image/webp":
		return m, true
	}
	return "", false
}

// ImageBlocks 取出内容块里的图片（值/指针两种形态都认）。
func ImageBlocks(bs []Block) []ImageBlock {
	var out []ImageBlock
	for _, b := range bs {
		switch v := b.(type) {
		case ImageBlock:
			out = append(out, v)
		case *ImageBlock:
			if v != nil {
				out = append(out, *v)
			}
		}
	}
	return out
}

// ImageBytes 估算图片原始字节数。Data 是 base64，长度乘 3/4 即为原文大小
// （不精确到字节，够用来判上限和写提示）。
func ImageBytes(im ImageBlock) int { return len(im.Data) / 4 * 3 }

// DescribeImages 把图片渲染成一行占位，如 `[图片：image/png，约 12KB]`；多张按行拼接。
//
// 供压缩视图与降级提示共用，格式保持稳定：模型看到它就知道"这里本来有图、
// 内容没传给我"，而不是看到一个空洞的 `{}` 或空字符串。
func DescribeImages(imgs []ImageBlock) string {
	var b strings.Builder
	for i, im := range imgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[图片：")
		b.WriteString(im.MimeType)
		b.WriteString("，约 ")
		b.WriteString(ImageSizeText(ImageBytes(im)))
		b.WriteByte(']')
	}
	return b.String()
}

// ImageSizeText 人类可读的字节数（KB/MB，向上取整避免出现 "0KB"）。
func ImageSizeText(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.Itoa((n+(1<<20)-1)/(1<<20)) + "MB"
	case n >= 1<<10:
		return strconv.Itoa((n+(1<<10)-1)/(1<<10)) + "KB"
	}
	return strconv.Itoa(n) + "B"
}
