package tools

// skill 工具：按需加载 Agent Skills（.agents/skills）里的技能正文。
//
// 为什么不让模型直接用 read 读 SKILL.md：
//
//  1. 用户级技能在 `~/.agents/skills`，在 Policy.Root 之外 —— ask/readonly 下
//     长 prompt 里给个读不到的路径，等于骗模型；
//  2. kernel 的 toolPathOf 已经把 toolName=="skill" 的调用归一成
//     `skill/<name>`（internal/kernel/kernel.dist.cjs），并从 args 的
//     skill/name/command 取名字。**工具名与参数名都不能改**，否则压缩/检索
//     层的锚定失效（表现是压缩后相关内容失去 skill/<name> 标签、BM25 变差）；
//  3. 宿主读文件可以顺手返回技能目录与 bundled 文件清单，让「相对路径以
//     技能目录为基准」这条约定可执行，而不是靠 prompt 里一句话提醒。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leijux/acp-agent/internal/skills"
)

const (
	// skillMaxFileBytes 是 bundled 文件（scripts/、references/ 等）的读取上限。
	skillMaxFileBytes = 1 << 20
	// skillMaxListFiles 是列目录时最多列多少个 bundled 文件。
	skillMaxListFiles = 40
)

// SkillTool 加载技能正文与随附文件。
type SkillTool struct {
	Skills []skills.Skill
}

// NewSkillTool 创建工具；skills 为空时工具仍然可用（会告诉模型没有技能）。
func NewSkillTool(list []skills.Skill) *SkillTool {
	return &SkillTool{Skills: list}
}

func (*SkillTool) Name() string { return "skill" }
func (*SkillTool) Description() string {
	return "加载 .agents/skills 里的技能。无参数：列出所有可用技能（名字 + 描述 + 目录）；" +
		"name：返回该技能的 SKILL.md 全文（随附脚本/参考文档用 file 参数读）。" +
		"技能正文里的相对路径以返回的技能目录为基准。"
}
func (*SkillTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"name": Prop("string", "技能名（省略则列出全部）"),
		"file": Prop("string", "读该技能目录下的某个随附文件，如 scripts/run.sh"),
	})
}

// ConcurrencySafe：只读，无共享状态。
func (*SkillTool) ConcurrencySafe() bool { return true }

// Mutates：不写文件、不碰进程，调度与权限上都算只读。
func (*SkillTool) Mutates() bool { return false }

func (t *SkillTool) Execute(ctx context.Context, a Args) (Result, error) {
	if err := CtxError(ctx); err != nil {
		return Result{}, err
	}
	name := strings.TrimSpace(a.Str("name", ""))
	file := strings.TrimSpace(a.Str("file", ""))

	if name == "" {
		if file != "" {
			return ErrorResult("file 需要配合 name 使用：先给出技能名"), nil
		}
		return t.listResult(), nil
	}

	s, ok := skills.Find(t.Skills, name)
	if !ok {
		return t.unknownResult(name), nil
	}
	if file == "" {
		return t.loadResult(s), nil
	}
	return t.fileResult(s, file), nil
}

// listResult 列出全部技能。
func (t *SkillTool) listResult() Result {
	if len(t.Skills) == 0 {
		return Result{
			Content: []Block{{Kind: KindText, Text: "没有可用的 skill（未发现 .agents/skills 目录，或已用 --no-skills 关闭）"}},
			Details: map[string]any{"count": 0},
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "可用 skill（%d 个）：\n", len(t.Skills))
	for _, s := range t.Skills {
		fmt.Fprintf(&b, "\n- **%s** — %s\n  dir: %s\n", s.Name, s.Description, s.Dir)
		if s.DisableModelInvocation {
			b.WriteString("  （不在 system prompt 名单里，只能显式加载）\n")
		}
	}
	return Result{
		Content: []Block{{Kind: KindText, Text: strings.TrimRight(b.String(), "\n")}},
		Details: map[string]any{"count": len(t.Skills)},
	}
}

// loadResult 返回 SKILL.md 正文 + 目录信息。
func (t *SkillTool) loadResult(s skills.Skill) Result {
	var b strings.Builder
	fmt.Fprintf(&b, "[skill: %s]\n[skill dir: %s]\n\n", s.Name, s.Dir)
	b.WriteString(strings.TrimRight(s.Body, "\n"))
	b.WriteString("\n")

	if files := listSkillFiles(s.Dir); len(files) > 0 {
		b.WriteString("\n随附文件（相对 skill dir，用 skill(name, file) 读取）：\n")
		for _, f := range files {
			b.WriteString("  ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	text, truncated := Truncate(strings.TrimRight(b.String(), "\n"), 0, 0)
	if truncated {
		text += Note(truncated, "正文很长：先用上面的文件清单挑需要的部分，再逐段读")
	}
	return Result{
		Content:   []Block{{Kind: KindText, Text: text}},
		Truncated: truncated,
		Details:   map[string]any{"skill": s.Name, "dir": s.Dir},
	}
}

// fileResult 读取技能目录下的随附文件。
func (t *SkillTool) fileResult(s skills.Skill, file string) Result {
	// 只允许读技能目录内的文件：技能是外部内容，不能变成任意路径读取器。
	clean := filepath.Clean(filepath.FromSlash(file))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ErrorResult("file 必须是技能目录内的相对路径，收到 %q", file)
	}
	full := filepath.Join(s.Dir, clean)
	rel, err := filepath.Rel(s.Dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrorResult("file 越出技能目录：%q", file)
	}
	info, err := os.Stat(full)
	if err != nil {
		return ErrorResult("读取 %s 失败：%s", file, errText(err))
	}
	if info.IsDir() {
		return ErrorResult("%s 是目录，不是文件", file)
	}
	if info.Size() > skillMaxFileBytes {
		return ErrorResult("%s 有 %d 字节，超过 %d 上限；请用 read 工具分页", file, info.Size(), skillMaxFileBytes)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ErrorResult("读取 %s 失败：%s", file, errText(err))
	}
	text, truncated := Truncate(strings.TrimRight(string(data), "\n"), 0, 0)
	if truncated {
		text += Note(truncated, "文件较长：用 read 工具按 offset 继续")
	}
	return Result{
		Content:   []Block{{Kind: KindText, Text: text}},
		Truncated: truncated,
		Details:   map[string]any{"skill": s.Name, "file": file, "bytes": len(data)},
	}
}

// unknownResult 给未知名字返回可用名单：一次纠正，不要让模型反复猜。
func (t *SkillTool) unknownResult(name string) Result {
	if len(t.Skills) == 0 {
		return ErrorResult("没有名为 %q 的 skill；当前没有可用 skill", name)
	}
	names := make([]string, 0, len(t.Skills))
	for _, s := range t.Skills {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return ErrorResult("没有名为 %q 的 skill。可用：%s", name, strings.Join(names, ", "))
}

// listSkillFiles 列出技能目录下的随附文件（不含 SKILL.md 本身）。
func listSkillFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == skills.SkillFile {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		if len(out) >= skillMaxListFiles {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}
