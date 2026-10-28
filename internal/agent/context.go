package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leijux/acp-agent/internal/skills"
	"github.com/leijux/acp-agent/internal/tools"
)

// ContextOptions 控制系统 prompt 与请求组装。
type ContextOptions struct {
	// BasePrompt 是基础指令。为空时用 DefaultSystemPrompt。
	BasePrompt string
	// Sections 是可替换的具名 section（Pi 的 system prompt sections）。
	Sections map[string]string
	// ContextFiles 是注入的上下文文件（典型是 AGENTS.md 的内容）。
	ContextFiles []ContextFile
	// Skills 是发现的技能清单。非空时在「项目上下文」之前注入名单
	// （只放 name/description/location，正文由 skill 工具按需加载）。
	Skills []skills.Skill
	// WorkingDir 供 prompt 里描述环境。
	WorkingDir string
	// ExtraInstructions 追加在末尾。
	ExtraInstructions string
}

// ContextFile 是一个已加载的上下文文件。
type ContextFile struct {
	Path    string
	Content string
}

// DefaultSystemPrompt 是基础指令。刻意保持精简 —— 冗长的 prompt 会挤占上下文，
// 且容易与用户的实际要求冲突。
const DefaultSystemPrompt = `你是一个在用户机器上工作的编码助手。你可以读写文件、搜索代码、执行命令。

准则：
- 动手前先读相关代码，不要凭猜测修改。
- 搜索代码优先用 grep/glob ，而不是 bash 里的 grep/find。
- 修改文件优先用 edit（精确替换），而不是 write（整体覆盖）。
- 同时需要多个只读探查时，一次发出多个工具调用，它们会并发执行。
- 改动后运行相关的构建/测试验证，不要声称"应该能过"。
- 回答简洁，直接说结论和影响，不要复述代码。`

// BuildSystemPrompt 组装系统 prompt。
//
// 顺序：基础指令 → 上下文文件 → 用户 section 覆盖 → 额外指令。
// 与 Pi 一致：上下文文件不是拼接成一坨，而是带文件名标注，让模型知道来源。
func BuildSystemPrompt(opts ContextOptions) string {
	var b strings.Builder

	base := opts.BasePrompt
	if strings.TrimSpace(base) == "" {
		base = DefaultSystemPrompt
	}
	b.WriteString(strings.TrimRight(base, "\n"))
	b.WriteString("\n")

	if opts.WorkingDir != "" {
		fmt.Fprintf(&b, "\n当前工作目录: %s\n", opts.WorkingDir)
	}

	for _, sk := range sortedKeys(opts.Sections) {
		b.WriteString("\n## ")
		b.WriteString(sk)
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(opts.Sections[sk], "\n"))
		b.WriteString("\n")
	}

	// 技能名单放在项目上下文之前：它决定「该不该去读某份说明」，比
	// AGENTS.md 更靠近决策点。只列名单，正文走 skill 工具按需加载。
	if block := skills.PromptBlock(opts.Skills, 0); block != "" {
		b.WriteString("\n## Skills\n")
		b.WriteString(block)
		b.WriteString("\n")
	}

	if len(opts.ContextFiles) > 0 {
		b.WriteString("\n# 项目上下文\n")
		b.WriteString("下面是从项目中自动发现的指令文件，请遵守。\n")
		for _, cf := range opts.ContextFiles {
			fmt.Fprintf(&b, "\n<file path=%q>\n%s\n</file>\n", cf.Path, strings.TrimRight(cf.Content, "\n"))
		}
	}

	if strings.TrimSpace(opts.ExtraInstructions) != "" {
		b.WriteString("\n# 附加说明\n")
		b.WriteString(strings.TrimRight(opts.ExtraInstructions, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ContextFileNames 是自动发现的上下文文件名，按优先级排序。
// 这与 Pi 的 context files 机制对齐：从工作目录向上找最近的指令文件。
var ContextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// DiscoverContextFiles 从 dir 向上查找上下文文件，每个名字取最近的一份。
// 到文件系统根或遇到 .git 目录停止。
func DiscoverContextFiles(dir string) []ContextFile {
	var out []ContextFile
	seen := map[string]bool{}
	cur := dir
	for {
		for _, name := range ContextFileNames {
			if seen[name] {
				continue
			}
			p := filepath.Join(cur, name)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			seen[name] = true
			out = append(out, ContextFile{Path: p, Content: string(data)})
		}
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return out
}

// ProjectMessages 把会话消息转换成模型可见形式（Pi 的 convertToLlm 等价物）：
//
//   - BashExecutionMessage 除非 ExcludeFromContext，否则转成 user 文本；
//   - CustomMessage 的 content 转成 user 内容（details 不发送）；
//   - Compaction/Branch summary 当作 user 前缀内容发送。
func ProjectMessages(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		switch v := m.(type) {
		case *BashExecutionMessage:
			if v.ExcludeFromContext {
				continue
			}
			text := "$ " + v.Command + "\n" + v.Output
			if v.Truncated {
				text += "\n[输出已截断]"
			}
			out = append(out, &UserMessage{Content: []Block{TextBlock{Text: text}}, At: v.At})
		case *CustomMessage:
			out = append(out, &UserMessage{Content: cloneBlocks(v.Content), At: v.At})
		default:
			out = append(out, m)
		}
	}
	return out
}

// toolSpecs 把 tools.Registry 转成模型可见的工具定义。
func toolSpecs(reg *tools.Registry) []ToolSpec {
	if reg == nil {
		return nil
	}
	specs := reg.Specs()
	out := make([]ToolSpec, 0, len(specs))
	for _, s := range specs {
		out = append(out, ToolSpec{
			Name:        s.Name,
			Description: s.Description,
			Parameters:  s.Parameters,
		})
	}
	return out
}

// ToolResultToBlocks 把 tools.Result 转成消息内容块。
func ToolResultToBlocks(r tools.Result) []Block {
	var blocks []Block
	for _, c := range r.Content {
		switch c.Kind {
		case tools.KindText:
			if t := strings.TrimRight(c.Text, "\n"); t != "" {
				blocks = append(blocks, TextBlock{Text: t})
			}
		case tools.KindImage:
			blocks = append(blocks, ImageBlock{Data: c.Data, MimeType: c.MIMEType})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, TextBlock{Text: "(无输出)"})
	}
	return blocks
}

// firstLine 取文本首行并截断，用于错误摘要。
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
