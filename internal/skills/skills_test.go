package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 在 root 下造一个技能目录并返回 SKILL.md 路径。
func writeSkill(t *testing.T, root, rel, content string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SkillFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverProjectAndUser(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "pdf-tools", `---
name: pdf-tools
description: Extract text from PDFs. Use when reading PDFs.
---
# PDF tools
Read references/formats.md first.
`)
	// 用户级
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "my-style", `---
description: 我的写作风格。
---
写简洁。
`)

	got := Discover(Options{Root: root, Home: home})
	if len(got.Skills) != 2 {
		t.Fatalf("发现 %d 个技能, want 2: %+v (warnings=%v)", len(got.Skills), got.Skills, got.Warnings)
	}
	proj, ok := Find(got.Skills, "pdf-tools")
	if !ok {
		t.Fatal("缺少项目级 pdf-tools")
	}
	if !proj.Project {
		t.Error("pdf-tools 应标记为项目级")
	}
	if !strings.Contains(proj.Body, "Read references/formats.md first.") {
		t.Errorf("正文未解析: %q", proj.Body)
	}
	if strings.Contains(proj.Body, "description:") {
		t.Errorf("正文里残留 frontmatter: %q", proj.Body)
	}
	// name 缺省时用父目录名
	us, ok := Find(got.Skills, "my-style")
	if !ok {
		t.Fatalf("缺少用户级技能（name 应回退到目录名）: %+v", got.Skills)
	}
	if us.Project {
		t.Error("my-style 不该是项目级")
	}
}

func TestDiscoverAncestorAndGitBoundary(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "repo-skill", `---
description: 仓库级技能。
---
body
`)
	// 仓库外面（父目录）放一个不该被发现的
	outside := filepath.Dir(repo)
	writeSkill(t, filepath.Join(outside, ".agents", "skills"), "outside-skill", `---
description: 仓库外，不该发现。
---
body
`)

	deep := filepath.Join(repo, "pkg", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Discover(Options{Root: deep, Home: t.TempDir()})
	if _, ok := Find(got.Skills, "repo-skill"); !ok {
		t.Fatalf("从子目录应能发现仓库根的技能: %+v", got.Skills)
	}
	if _, ok := Find(got.Skills, "outside-skill"); ok {
		t.Fatalf(".git 之外的目录不该被扫描: %+v", got.Skills)
	}
}

func TestDiscoverProjectWinsOnNameConflict(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "dup", `---
description: 项目版
---
project
`)
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "dup", `---
description: 用户版
---
user
`)

	got := Discover(Options{Root: root, Home: home})
	if len(got.Skills) != 1 {
		t.Fatalf("同名应只保留一个: %+v", got.Skills)
	}
	s, _ := Find(got.Skills, "dup")
	if s.Description != "项目版" {
		t.Fatalf("应保留项目级定义，实际 %q", s.Description)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "重复") {
		t.Fatalf("缺少重复定义的 warning: %v", got.Warnings)
	}
}

func TestDiscoverSkipsMissingDescription(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "no-desc", `---
name: no-desc
---
body only
`)
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "good", `---
description: ok
---
body
`)
	got := Discover(Options{Root: root, Home: t.TempDir()})
	if len(got.Skills) != 1 || got.Skills[0].Name != "good" {
		t.Fatalf("缺 description 的技能应被跳过: %+v", got.Skills)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "缺少 description") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少 warning: %v", got.Warnings)
	}
}

func TestDiscoverBadNameWarnsButLoads(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "BadDir", `---
name: Bad_Name
description: 名字不合法但仍应加载
---
body
`)
	got := Discover(Options{Root: root, Home: t.TempDir()})
	if len(got.Skills) != 1 {
		t.Fatalf("坏名字只应 warning，不该丢弃: %+v", got.Skills)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("缺少名字校验 warning")
	}
}

func TestDiscoverRecursiveAndDisabled(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 多层嵌套
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "team/review", `---
description: 代码评审流程。
---
body
`)
	got := Discover(Options{Root: root, Home: t.TempDir()})
	if _, ok := Find(got.Skills, "review"); !ok {
		t.Fatalf("递归目录里的技能未发现（name 应回退到 review）: %+v", got.Skills)
	}

	// Disabled 直接返回空
	off := Discover(Options{Root: root, Home: t.TempDir(), Disabled: true})
	if len(off.Skills) != 0 {
		t.Fatalf("Disabled 时应为空: %+v", off.Skills)
	}
}

func TestDiscoverExtraPaths(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "x", `---
description: 自定义目录里的技能。
---
body
`)
	got := Discover(Options{Root: root, Home: t.TempDir(), ExtraPaths: []string{extra}})
	if _, ok := Find(got.Skills, "x"); !ok {
		t.Fatalf("未从 ExtraPaths 发现: %+v", got.Skills)
	}

	// 相对路径相对 Root
	rel := t.TempDir()
	writeSkill(t, rel, "sub/y", `---
description: 相对路径。
---
body
`)
	got = Discover(Options{Root: rel, Home: t.TempDir(), ExtraPaths: []string{"sub"}})
	if _, ok := Find(got.Skills, "y"); !ok {
		t.Fatalf("相对 ExtraPaths 未生效: %+v", got.Skills)
	}
}

func TestPromptBlock(t *testing.T) {
	list := []Skill{
		{Name: "a", Description: "用 <b> & 转义", Path: "/s/a/SKILL.md"},
		{Name: "hidden", Description: "不该出现", Path: "/s/h/SKILL.md", DisableModelInvocation: true},
	}
	block := PromptBlock(list, 0)
	if !strings.Contains(block, "<name>a</name>") {
		t.Fatalf("缺少技能条目:\n%s", block)
	}
	if !strings.Contains(block, "&lt;b&gt; &amp;") {
		t.Fatalf("XML 未转义:\n%s", block)
	}
	if strings.Contains(block, "hidden") || strings.Contains(block, "不该出现") {
		t.Fatalf("disable-model-invocation 的技能不该进 prompt:\n%s", block)
	}
	if !strings.Contains(block, "<location>/s/a/SKILL.md</location>") {
		t.Fatalf("缺少 location:\n%s", block)
	}

	// 无技能 → 空串
	if got := PromptBlock(nil, 0); got != "" {
		t.Fatalf("无技能时应返回空串，实际:\n%s", got)
	}
	// 只有隐藏技能 → 也应为空
	if got := PromptBlock(list[1:], 0); got != "" {
		t.Fatalf("只有隐藏技能时应返回空串，实际:\n%s", got)
	}

	// 上限与省略提示
	many := make([]Skill, 0, 5)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		many = append(many, Skill{Name: n, Description: "d", Path: "/" + n})
	}
	block = PromptBlock(many, 3)
	if strings.Count(block, "<skill>") != 3 {
		t.Fatalf("应只列出 3 个:\n%s", block)
	}
	if !strings.Contains(block, "另有 2 个 skill 未列出") {
		t.Fatalf("缺少省略提示:\n%s", block)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string]string
		body string
	}{
		{
			name: "基本",
			src:  "---\nname: a\ndescription: hi\n---\nbody\n",
			want: map[string]string{"name": "a", "description": "hi"},
			body: "body\n",
		},
		{
			name: "引号与布尔",
			src:  "---\ndescription: \"含: 冒号\"\ndisable-model-invocation: true\n---\nx",
			want: map[string]string{"description": "含: 冒号", "disable-model-invocation": "true"},
			body: "x",
		},
		{
			name: "块标量",
			src:  "---\ndescription: |\n  第一行\n  第二行\n---\nbody",
			want: map[string]string{"description": "第一行\n第二行"},
			body: "body",
		},
		{
			name: "没有 frontmatter",
			src:  "# 只有正文\n",
			want: map[string]string{},
			body: "# 只有正文\n",
		},
		{
			name: "未闭合",
			src:  "---\nname: a\n",
			want: map[string]string{},
			body: "",
		},
		{
			name: "CRLF",
			src:  "---\r\nname: a\r\ndescription: hi\r\n---\r\nbody\r\n",
			want: map[string]string{"name": "a", "description": "hi"},
			// 正文统一归一成 LF：CRLF 在技能正文里没有语义，
			// 统一后拼进 prompt 也不会混入多余的 \r。
			body: "body\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, body := splitFrontmatter(tc.src)
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q（全量 %+v）", k, got[k], v, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("字段数 = %d, want %d（%+v）", len(got), len(tc.want), got)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
		})
	}
}
