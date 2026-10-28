package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/skills"
)

func seedSkill(t *testing.T, dir string) skills.Skill {
	t.Helper()
	root := filepath.Join(dir, "demo")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Demo skill\n\nRun `scripts/run.sh` before converting.\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "run.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return skills.Skill{
		Name:        "demo",
		Description: "演示技能。",
		Path:        filepath.Join(root, "SKILL.md"),
		Dir:         root,
		Body:        body,
		Project:     true,
	}
}

// 无参数：列出技能。
func TestSkillToolList(t *testing.T) {
	dir := t.TempDir()
	s := seedSkill(t, dir)
	tool := NewSkillTool([]skills.Skill{s})
	res, err := tool.Execute(ctx_(), Args{})
	if err != nil || res.IsError {
		t.Fatalf("list: %v %s", err, res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "demo") || !strings.Contains(text, "演示技能") {
		t.Fatalf("列表缺少技能信息:\n%s", text)
	}
	if !strings.Contains(text, "dir: ") {
		t.Fatalf("列表缺少目录:\n%s", text)
	}

	// 没有技能时也要有明确说明（而不是报错）
	empty, _ := NewSkillTool(nil).Execute(ctx_(), Args{})
	if empty.IsError {
		t.Fatalf("无技能不应算错误: %s", empty.Text())
	}
	if !strings.Contains(empty.Text(), "没有可用的 skill") {
		t.Fatalf("提示不明确: %s", empty.Text())
	}
}

// name：返回正文 + 目录 + 随附文件清单。
func TestSkillToolLoad(t *testing.T) {
	dir := t.TempDir()
	s := seedSkill(t, dir)
	res, err := NewSkillTool([]skills.Skill{s}).Execute(ctx_(), Args{"name": "demo"})
	if err != nil || res.IsError {
		t.Fatalf("load: %v %s", err, res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "Run `scripts/run.sh`") {
		t.Fatalf("缺少正文:\n%s", text)
	}
	if !strings.Contains(text, s.Dir) {
		t.Fatalf("缺少 skill dir:\n%s", text)
	}
	if !strings.Contains(text, "scripts/run.sh") {
		t.Fatalf("缺少随附文件清单:\n%s", text)
	}
}

// name + file：读随附文件；越界必须拒绝。
func TestSkillToolFile(t *testing.T) {
	dir := t.TempDir()
	s := seedSkill(t, dir)
	tool := NewSkillTool([]skills.Skill{s})

	res, _ := tool.Execute(ctx_(), Args{"name": "demo", "file": "scripts/run.sh"})
	if res.IsError || !strings.Contains(res.Text(), "echo hi") {
		t.Fatalf("读随附文件失败: %s", res.Text())
	}

	for _, bad := range []string{"../secret.txt", "/etc/passwd", "scripts/../../x"} {
		res, _ := tool.Execute(ctx_(), Args{"name": "demo", "file": bad})
		if !res.IsError {
			t.Fatalf("越界路径 %q 应被拒绝，实际: %s", bad, res.Text())
		}
	}

	// 目录不是文件
	res, _ = tool.Execute(ctx_(), Args{"name": "demo", "file": "scripts"})
	if !res.IsError {
		t.Fatalf("目录应被拒绝: %s", res.Text())
	}

	// 只给 file 不给 name
	res, _ = tool.Execute(ctx_(), Args{"file": "scripts/run.sh"})
	if !res.IsError {
		t.Fatalf("缺 name 时应报错: %s", res.Text())
	}
}

// 未知名字要给出可用名单（一次纠正，别让模型反复猜）。
func TestSkillToolUnknownName(t *testing.T) {
	dir := t.TempDir()
	s := seedSkill(t, dir)
	res, _ := NewSkillTool([]skills.Skill{s}).Execute(ctx_(), Args{"name": "nope"})
	if !res.IsError {
		t.Fatal("未知名字应返回错误结果")
	}
	if !strings.Contains(res.Text(), "demo") {
		t.Fatalf("错误信息应带可用名单: %s", res.Text())
	}
}

// 工具契约：只读、可并发；名字与参数名不能改（kernel 依赖 skill/<name>）。
func TestSkillToolContract(t *testing.T) {
	tool := NewSkillTool(nil)
	if tool.Name() != "skill" {
		t.Fatalf("工具名必须是 skill（kernel 的 toolPathOf 依赖它），实际 %q", tool.Name())
	}
	if tool.Mutates() || !tool.ConcurrencySafe() {
		t.Fatal("skill 是只读工具：Mutates=false、ConcurrencySafe=true")
	}
	params := tool.Parameters()
	if _, ok := params["properties"].(map[string]any)["name"]; !ok {
		t.Fatal("参数名必须是 name（kernel 从 args.name 取技能名）")
	}
}

// skill 工具按技能发现结果注册：没有技能时不该出现在工具表里
// （一段只会回答「没有技能」的描述白占 prompt）。
func TestDefaultRegistrySkillTool(t *testing.T) {
	root := t.TempDir()
	empty, _ := DefaultRegistry(Options{Policy: Policy{Root: root}})
	if _, ok := empty.Get("skill"); ok {
		t.Fatal("没有技能时不该注册 skill 工具")
	}

	withSkill, _ := DefaultRegistry(Options{
		Policy: Policy{Root: root},
		Skills: []skills.Skill{{Name: "demo", Description: "d", Dir: root, Path: filepath.Join(root, "SKILL.md")}},
	})
	if _, ok := withSkill.Get("skill"); !ok {
		t.Fatal("有技能时应注册 skill 工具")
	}

	// Disable 里点名也能关掉
	off, _ := DefaultRegistry(Options{
		Policy:  Policy{Root: root},
		Skills:  []skills.Skill{{Name: "demo", Description: "d"}},
		Disable: []string{"skill"},
	})
	if _, ok := off.Get("skill"); ok {
		t.Fatal("Disable 应能关掉 skill 工具")
	}
}
