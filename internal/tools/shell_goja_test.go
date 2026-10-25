//go:build !shellnative

package tools

import (
	"runtime"
	"strings"
	"testing"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// runGojaBuiltin 用空 PATH 强制走内建命令，模拟「机器上什么都没装」。
func runGojaBuiltin(t *testing.T, script string) string {
	t.Helper()
	bt, _ := newBash(t)
	empty := t.TempDir()
	res, err := bt.Execute(ctx_(), Args{
		"command": script,
		"env":     map[string]any{"PATH": empty},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text()
}

func TestBuiltinGojaEval(t *testing.T) {
	out := runGojaBuiltin(t, `goja -e '1 + 2 * 3'`)
	if !strings.Contains(out, "7") {
		t.Fatalf("输出 = %q，期望含 7", out)
	}
}

func TestBuiltinGojaConsoleAndStderr(t *testing.T) {
	// console.log 进 stdout，console.error 进 stderr；bash 工具两者合并显示，
	// 这里只验证都出现了。
	out := runGojaBuiltin(t, `goja -e 'console.log("to-out"); console.error("to-err"); 0'`)
	if !strings.Contains(out, "to-out") || !strings.Contains(out, "to-err") {
		t.Fatalf("输出 = %q", out)
	}
}

func TestBuiltinGojaQuiet(t *testing.T) {
	out := runGojaBuiltin(t, `goja -q -e '"不该出现"'`)
	if strings.Contains(out, "不该出现") {
		t.Fatalf("-q 仍打印了返回值: %q", out)
	}
}

func TestBuiltinGojaStdin(t *testing.T) {
	out := runGojaBuiltin(t, `printf 'fs.readFile("/dev/stdin")' | goja`)
	// /dev/stdin 在 Windows 上不存在，这里只要求不崩；真正的 stdin 管道
	// 由下面这个用例覆盖（无参数 + 重定向）。
	_ = out

	out = runGojaBuiltin(t, `printf '6 * 7' | goja`)
	if !strings.Contains(out, "42") {
		t.Fatalf("stdin 脚本输出 = %q，期望 42", out)
	}
}

func TestBuiltinGojaScriptFile(t *testing.T) {
	bt, _ := newBash(t)
	empty := t.TempDir()
	env := map[string]any{"PATH": empty}

	// 用内建 printf 落一个脚本文件，再执行它。
	if res, err := bt.Execute(ctx_(), Args{
		"command": `printf 'const xs=[3,1,2]; xs.sort((a,b)=>a-b).join(",")' > s.js`,
		"env":     env,
	}); err != nil || res.IsError {
		t.Fatalf("准备脚本失败: %v / %s", err, res.Text())
	}

	res, err := bt.Execute(ctx_(), Args{"command": `goja s.js`, "env": env})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "1,2,3") {
		t.Fatalf("脚本文件输出 = %q", res.Text())
	}
}

func TestBuiltinGojaArgv(t *testing.T) {
	out := runGojaBuiltin(t, `goja -e 'argv.join("/")' a b c`)
	if !strings.Contains(out, "a/b/c") {
		t.Fatalf("argv 输出 = %q，期望 a/b/c", out)
	}
	out = runGojaBuiltin(t, `goja -e 'argv.length' -q a b`)
	_ = out // -q 时不打印；下面用非 -q 再验一次
	out = runGojaBuiltin(t, `goja -e 'argv.length' -- a b`)
	if !strings.Contains(out, "2") {
		t.Fatalf("argv 长度 = %q，期望 2", out)
	}
}

// fs 在 shell 里同样受 Root 约束（和 js 工具一致）。
func TestBuiltinGojaFSScopedToRoot(t *testing.T) {
	bt, _ := newBash(t)
	empty := t.TempDir()
	env := map[string]any{"PATH": empty}
	mustWrite(t, bt.Policy.Root+"/data.txt", "hello", 0o644)

	res, err := bt.Execute(ctx_(), Args{
		"command": `goja -e 'fs.readFile("data.txt").toUpperCase()'`,
		"env":     env,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "HELLO") {
		t.Fatalf("输出 = %q", res.Text())
	}

	// 越界必须拒绝（非零退出 + 报错）
	res, err = bt.Execute(ctx_(), Args{
		"command": `goja -e 'fs.readFile("../outside.txt")' || echo DENIED`,
		"env":     env,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "DENIED") {
		t.Fatalf("越界读取未被拒绝: %q", res.Text())
	}
}

func TestBuiltinGojaCrypto(t *testing.T) {
	out := runGojaBuiltin(t, `goja -e 'crypto.sha256("abc")'`)
	if !strings.Contains(out, "ba7816bf8f01cfea414140de5dae2223") {
		t.Fatalf("sha256 输出 = %q", out)
	}
}

func TestBuiltinGojaErrorExitCode(t *testing.T) {
	out := runGojaBuiltin(t, `goja -e 'throw new Error("boom")' || echo "EXIT=$?"`)
	if !strings.Contains(out, "boom") {
		t.Fatalf("未输出错误信息: %q", out)
	}
	if !strings.Contains(out, "EXIT=1") {
		t.Fatalf("未以非零码退出: %q", out)
	}
}

func TestBuiltinGojaTimeout(t *testing.T) {
	// 纯 JS 死循环也要能被打断（否则 bash 工具会永久挂住）。
	out := runGojaBuiltin(t, `goja -t 200ms -e 'while(true){}' || echo "TIMEOUT=$?"`)
	if !strings.Contains(out, "TIMEOUT=1") {
		t.Fatalf("死循环未被超时打断: %q", out)
	}
	if !strings.Contains(out, "中断") {
		t.Fatalf("缺少超时提示: %q", out)
	}
}

func TestBuiltinGojaHelp(t *testing.T) {
	out := runGojaBuiltin(t, `goja --help`)
	if !strings.Contains(out, "用法: goja") || !strings.Contains(out, "crypto.sha256") {
		t.Fatalf("帮助输出不对: %q", out)
	}
}

// PATH 里有真的 goja 时应该让它跑，而不是内建接管。
func TestBuiltinGojaPrefersRealExecutable(t *testing.T) {
	bt, _ := newBash(t)
	bin := t.TempDir()
	name := "goja"
	if isWindows() {
		name = "goja.EXE"
	}
	mustWrite(t, bin+"/"+name, "x", 0o755)

	res, err := bt.Execute(ctx_(), Args{
		"command": `goja --version`,
		"env":     map[string]any{"PATH": bin},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(res.Text(), "用法: goja") {
		t.Fatalf("PATH 里有 goja 时应让真程序跑，实际走了内建: %q", res.Text())
	}
}

// TestBuiltinGojaReadStdin 回归：`cmd | goja -e '...'` 必须能读到管道数据。
//
// Windows 上没有 /dev/stdin，stdin 又只是解释器的 io.Reader，
// 所以这条链路只能由宿主提供 fs.readStdin()。
func TestBuiltinGojaReadStdin(t *testing.T) {
	bt, _ := newBash(t)
	env := map[string]any{"PATH": t.TempDir()}

	res, err := bt.Execute(ctx_(), Args{
		"command": `printf 'a\nb\nc\n' | goja -e 'fs.readStdin().trim().split("\n").length'`,
		"env":     env,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "3") {
		t.Fatalf("stdin 行数 = %q，期望 3", res.Text())
	}

	// 读两次要报错（管道不可回退）
	res, err = bt.Execute(ctx_(), Args{
		"command": `printf 'x' | goja -e 'fs.readStdin(); fs.readStdin()' || echo DENIED`,
		"env":     env,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "DENIED") {
		t.Fatalf("重复读 stdin 未报错: %q", res.Text())
	}
}

// TestBuiltinGojaStdinUsedByScript 脚本自身来自 stdin 时，readStdin 不可用。
func TestBuiltinGojaStdinUsedByScript(t *testing.T) {
	out := runGojaBuiltin(t, `printf 'typeof fs.readStdin' | goja`)
	if !strings.Contains(out, "undefined") {
		t.Fatalf("脚本占用 stdin 时不应暴露 readStdin: %q", out)
	}
}
