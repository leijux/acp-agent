// Package skills 发现并解析 Agent Skills 规范（https://agentskills.io/specification）
// 里的技能目录。
//
// 位置只认规范的 `.agents/skills`：
//
//	<cwd>/.agents/skills/**/SKILL.md      项目级（从 cwd 向祖先找到 .git 根）
//	~/.agents/skills/**/SKILL.md          用户级
//	+ Options.ExtraPaths 里的自定义目录
//
// 刻意不读 Pi 私有的 `~/.pi/agent/skills`、`<root>/.pi/skills`：只用规范位置，
// 避免两套布局同时存在时"哪个生效"说不清。
//
// 语义对齐上游 Pi：只把 name/description/location 放进 system prompt，正文在
// 模型判断匹配时才加载（见 internal/tools 的 skill 工具）。
package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// AgentsDir 是规范目录名（`<dir>/.agents/skills`）。
	AgentsDir = ".agents"
	// SubDir 是 .agents 下的子目录名。
	SubDir = "skills"
	// SkillFile 是技能正文文件名（大小写敏感）。
	SkillFile = "SKILL.md"

	// MaxNameLen / MaxDescriptionLen 来自规范。
	MaxNameLen        = 64
	MaxDescriptionLen = 1024

	// DefaultMaxPrompt 是注入 system prompt 的技能条数上限。
	DefaultMaxPrompt = 64
)

var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Skill 是一个已发现的技能。
type Skill struct {
	Name        string
	Description string
	// Path 是 SKILL.md 的绝对路径（prompt 里的 location）。
	Path string
	// Dir 是技能目录，正文里的相对路径以它为基准。
	Dir string
	// Body 是 frontmatter 之后的正文。
	Body string
	// Project 表示来自项目目录（否则是用户级/自定义）。
	Project bool
	// DisableModelInvocation 为真时不进 prompt 名单，但仍可用工具显式加载。
	DisableModelInvocation bool
}

// Options 控制发现范围。
type Options struct {
	// Root 是项目根（通常是 cwd）。
	Root string
	// Home 默认 os.UserHomeDir。
	Home string
	// ExtraPaths 是额外的发现目录；相对路径相对 Root。
	ExtraPaths []string
	// Disabled 为真时直接返回空结果（--no-skills / disableSkills）。
	Disabled bool
}

// Result 是发现结果。Warnings 不阻断加载（与 Pi 一致），供调用方打日志。
type Result struct {
	Skills   []Skill
	Warnings []string
}

// Discover 扫描全部位置。
//
// 同名技能保留**先发现的**：项目级在用户级之前，因此项目可以覆盖用户级。
func Discover(opts Options) Result {
	var res Result
	if opts.Disabled {
		return res
	}
	seen := make(map[string]bool, 8)

	addDir := func(dir string, project bool) {
		if dir == "" {
			return
		}
		if _, err := os.Stat(dir); err != nil {
			return
		}
		for _, s := range scanDir(dir, project, &res.Warnings) {
			if seen[s.Name] {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("skill %q 重复定义，忽略 %s（保留先发现的）", s.Name, s.Path))
				continue
			}
			seen[s.Name] = true
			res.Skills = append(res.Skills, s)
		}
	}

	for _, dir := range projectSkillDirs(opts.Root) {
		addDir(dir, true)
	}
	home := opts.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		addDir(filepath.Join(home, AgentsDir, SubDir), false)
	}
	for _, p := range opts.ExtraPaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(opts.Root, p)
		}
		addDir(filepath.Clean(p), false)
	}
	return res
}

// projectSkillDirs 返回从 root 到仓库根的各层 `.agents/skills`（近的在前）。
//
// 到含 `.git` 的那一层为止（含）：往仓库外面扫会把「上上个项目的技能」也带进来。
func projectSkillDirs(root string) []string {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	cur, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var out []string
	for {
		out = append(out, filepath.Join(cur, AgentsDir, SubDir))
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

// scanDir 递归扫描 dir 下所有 SKILL.md。
func scanDir(dir string, project bool, warnings *[]string) []Skill {
	var out []Skill
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != SkillFile {
			return nil
		}
		s, warns, ok := loadSkill(p, project)
		*warnings = append(*warnings, warns...)
		if ok {
			out = append(out, s)
		}
		return nil
	})
	return out
}

// skipDirNames 是扫描技能目录时跳过的目录（技能可能自带 node_modules/依赖）。
var skipDirNames = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
}

// loadSkill 解析一个 SKILL.md。
//
// 返回 ok=false 表示**不加载**：只有"没有 description"会走到这里（与 Pi 一致）。
// 名字不合法只产生 warning，仍然加载。
func loadSkill(path string, project bool) (Skill, []string, bool) {
	var warnings []string
	data, err := os.ReadFile(path)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("读取 %s 失败: %v", path, err))
		return Skill{}, warnings, false
	}
	dir := filepath.Dir(path)
	front, body := splitFrontmatter(string(data))

	name := strings.TrimSpace(front["name"])
	if name == "" {
		// 规范允许缺省，用父目录名（与 Pi 一致）。
		name = filepath.Base(dir)
	}
	desc := strings.TrimSpace(front["description"])
	if desc == "" {
		warnings = append(warnings, fmt.Sprintf("skill %s 缺少 description，已跳过", path))
		return Skill{}, warnings, false
	}
	if len(desc) > MaxDescriptionLen {
		desc = desc[:MaxDescriptionLen]
	}
	for _, msg := range validateName(name) {
		warnings = append(warnings, fmt.Sprintf("skill %s: %s", path, msg))
	}
	return Skill{
		Name:                   name,
		Description:            desc,
		Path:                   path,
		Dir:                    dir,
		Body:                   body,
		Project:                project,
		DisableModelInvocation: isTrue(front["disable-model-invocation"]),
	}, warnings, true
}

func validateName(name string) []string {
	var errs []string
	if len(name) > MaxNameLen {
		errs = append(errs, fmt.Sprintf("name 超过 %d 字符（%d）", MaxNameLen, len(name)))
	}
	if !nameRe.MatchString(name) {
		errs = append(errs, "name 只能包含小写字母、数字与连字符")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name 不能以连字符开头或结尾")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name 不能包含连续连字符")
	}
	return errs
}

// splitFrontmatter 拆出 frontmatter 与正文。
//
// 只支持 YAML 的最小子集：顶层 `key: value`，值可以是裸串、单/双引号串、
// true/false，或 `|` / `>` 块标量。嵌套映射、列表、锚点等一律忽略 ——
// 技能的规范字段都是扁平标量，为此引一个 YAML 依赖不划算。
func splitFrontmatter(src string) (map[string]string, string) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	fields := map[string]string{}
	if !strings.HasPrefix(src, "---\n") {
		return fields, src
	}
	rest := src[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		// frontmatter 未闭合：按"只有 frontmatter，没有正文"处理，避免把
		// 半截 YAML 当指令喂给模型。
		return fields, ""
	}
	block := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")

	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue // 嵌套结构，忽略
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if val == "|" || val == ">" || val == "|-" || val == ">-" {
			// 块标量：收集后面的缩进行
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "  ") || strings.HasPrefix(lines[i+1], "\t") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			joined := strings.Join(parts, " ")
			if val == "|" || val == "|-" {
				joined = strings.Join(parts, "\n")
			}
			fields[key] = strings.TrimSpace(joined)
			continue
		}
		fields[key] = unquote(val)
	}
	return fields, body
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on":
		return true
	}
	return false
}

// PromptBlock 渲染 system prompt 里的技能清单。
//
// 格式照抄上游 Pi 的 <available_skills>：社区模型见过这个结构，比自定义
// Markdown 更不容易和 AGENTS.md 的内容混淆。只输出 name/description/location，
// **不放正文** —— 正文由 skill 工具按需加载。
//
// maxPrompt<=0 时用 DefaultMaxPrompt；为 0 个技能时返回空串（不留空 section）。
func PromptBlock(list []Skill, maxPrompt int) string {
	visible := make([]Skill, 0, len(list))
	for _, s := range list {
		if s.DisableModelInvocation {
			continue
		}
		visible = append(visible, s)
	}
	if len(visible) == 0 {
		return ""
	}
	if maxPrompt <= 0 {
		maxPrompt = DefaultMaxPrompt
	}
	omitted := 0
	if len(visible) > maxPrompt {
		omitted = len(visible) - maxPrompt
		visible = visible[:maxPrompt]
	}

	var b strings.Builder
	b.WriteString("The following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the skill tool (or the read tool) to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory ")
	b.WriteString("(parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range visible {
		b.WriteString("  <skill>\n")
		fmt.Fprintf(&b, "    <name>%s</name>\n", escapeXML(s.Name))
		fmt.Fprintf(&b, "    <description>%s</description>\n", escapeXML(s.Description))
		fmt.Fprintf(&b, "    <location>%s</location>\n", escapeXML(s.Path))
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	if omitted > 0 {
		fmt.Fprintf(&b, "\n[另有 %d 个 skill 未列出]", omitted)
	}
	return b.String()
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// Find 按名字查找技能。
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}
