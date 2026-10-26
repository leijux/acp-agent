package tools

import (
	"os"
	"strings"
	"testing"
)

// TestAllowOutside 验证 Options.AllowOutside 让内置工具接受 Root 之外的路径。
//
// 场景：--permissions auto 下模型直接读写 root 外（同机其它仓库、~/.config），
// 而不是绕道 bash（bash 的命令本来就不受 Root 约束）。
func TestAllowOutside(t *testing.T) {
	root := t.TempDir()
	outside := writeTestFile(t, t.TempDir(), "secret.txt", "outside-content\n")

	// 默认：越界读被拒。
	strict, _ := DefaultRegistry(Options{Policy: Policy{Root: root}})
	rt, ok := strict.Get("read")
	if !ok {
		t.Fatal("缺少 read 工具")
	}
	res, err := rt.Execute(ctx_(), Args{"path": outside})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("默认策略应拒绝越界读，得到: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "超出允许目录") {
		t.Fatalf("拒绝原因不对: %q", res.Text())
	}

	// 放宽后：读写都作用于绝对路径，而不会被拼到 root 下。
	loose, _ := DefaultRegistry(Options{
		Policy:       Policy{Root: root},
		AllowOutside: true,
	})
	rt, _ = loose.Get("read")
	res, err = rt.Execute(ctx_(), Args{"path": outside})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("放宽后不应拒绝: %s", res.Text())
	}
	if !strings.Contains(res.Text(), "outside-content") {
		t.Fatalf("未读到内容: %q", res.Text())
	}

	wt, ok := loose.Get("write")
	if !ok {
		t.Fatal("缺少 write 工具")
	}
	res, err = wt.Execute(ctx_(), Args{"path": outside, "content": "overwritten\n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("放宽后 write 不应越界报错: %s", res.Text())
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "overwritten\n" {
		t.Fatalf("越界写未生效: %q, %v", b, err)
	}
}

// TestAllowOutsideOffByDefault 确认零值 Options 不放宽：root 仍然是边界，
// 否则 --permissions ask/readonly 就形同虚设。
func TestAllowOutsideOffByDefault(t *testing.T) {
	root := t.TempDir()
	outside := writeTestFile(t, t.TempDir(), "f.txt", "x")

	reg, _ := DefaultRegistry(Options{Policy: Policy{Root: root}})
	for _, name := range []string{"write", "edit", "ls", "glob", "grep"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("缺少 %s 工具", name)
		}
		var args Args
		switch name {
		case "write":
			args = Args{"path": outside, "content": "y"}
		case "edit":
			args = Args{"path": outside, "oldText": "x", "newText": "y"}
		default:
			args = Args{"path": outside, "pattern": "x"}
		}
		res, err := tool.Execute(ctx_(), args)
		if err != nil {
			t.Fatalf("%s Execute: %v", name, err)
		}
		if !res.IsError || !strings.Contains(res.Text(), "超出允许目录") {
			t.Fatalf("%s 应拒绝越界路径，得到: %s", name, res.Text())
		}
	}
}
