package main

// 提示词里的 @文件 / @文件夹。
//
// 实现在 internal/mentions（纯逻辑，可单测）；这里只负责三件事：
//   1. 把 app 的配置（root / 越界策略 / --no-mentions）翻译成 mentions.Options；
//   2. 把警告与附件摘要打到面向人的输出（TUI 走 note 行，-p 走 stderr）；
//   3. 保证「展开发生在 agent.Run 之前」——历史里存的是展开后的文本，
//      resume 时不依赖磁盘（文件可能已经变了）。
//
// ACP 不做展开：ACP 客户端有原生 resource_link，@ 文本可能被它自己消费，
// 双份注入比不注入更糟（docs/mentions.md §9-a）。

import (
	"fmt"
	"strings"

	"github.com/leijux/acp-agent/internal/mentions"
)

// expandMentions 把 prompt 里的 @token 展开成附件块；没有 token 时原样返回。
func (a *app) expandMentions(prompt string) string {
	if a.cfg.NoMentions || !strings.ContainsRune(prompt, '@') {
		return prompt
	}
	// 越界读的口径与工具层一致：只有 auto（全放行）才允许 root 之外的路径。
	allowOutside := a.guard != nil && a.guard.mode == modeAuto
	res := mentions.Expand(mentions.Options{
		Root:         a.cfg.Root,
		AllowOutside: allowOutside,
	}, prompt)
	for _, w := range res.Warnings {
		a.notice("%s", "↳ "+w)
	}
	if n := len(res.Attachments); n > 0 {
		var files, dirs, truncated int
		var bytes int64
		for _, at := range res.Attachments {
			switch at.Kind {
			case "dir":
				dirs++
			default:
				files++
				bytes += at.Truncated
			}
			if at.Truncated > 0 && at.Truncated < at.Bytes {
				truncated++
			}
		}
		parts := make([]string, 0, 3)
		if files > 0 {
			parts = append(parts, fmt.Sprintf("%d 个文件 %s", files, humanSize(bytes)))
		}
		if dirs > 0 {
			parts = append(parts, fmt.Sprintf("%d 个目录清单", dirs))
		}
		msg := "↳ 附件：" + strings.Join(parts, "，")
		if truncated > 0 {
			msg += fmt.Sprintf("（%d 个已截断）", truncated)
		}
		a.notice("%s", msg)
	}
	return res.Text
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
