//go:build !shellnative

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
)

// 内建命令回退层的测试。
//
// 本机（纯 Windows）PATH 里没有 coreutils，端到端用例天然走内建实现；
// Linux/macOS 上 PATH 里有真程序，所以端到端用例统一把解释器的 PATH 指到
// 空目录，强制走内建实现，保证两个平台测的是同一份代码。

func envPath(dir string) expand.Environ {
	return expand.ListEnviron("PATH=" + dir)
}

func mustWrite(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// TestBuiltinForDispatch PATH 有空/有同名可执行文件时的接管决策。
func TestBuiltinForDispatch(t *testing.T) {
	empty := t.TempDir()

	if fn, ok := builtinFor(envPath(empty), "ls"); !ok || fn == nil {
		t.Fatal("PATH 为空时应回退到内建 ls")
	}
	if _, ok := builtinFor(envPath(empty), "git"); ok {
		t.Fatal("未实现的命令不应被内建接管")
	}
	if _, ok := builtinFor(envPath(empty), "sleep.exe"); ok {
		t.Fatal("带扩展名的命令名不应命中内建表")
	}

	// PATH 里有同名可执行文件 → 让真程序跑。
	bin := t.TempDir()
	name := "ls"
	if runtime.GOOS == "windows" {
		name = "LS.EXE"
	}
	mustWrite(t, filepath.Join(bin, name), "x", 0o755)
	if _, ok := builtinFor(envPath(bin), "ls"); ok {
		t.Fatalf("PATH 里有 %s 时不应被内建接管", name)
	}
}

// TestFindExecutableFollowsEnvPATH 查找用的是解释器的 PATH（脚本里 export 后要生效），
// 不是进程自己的 PATH。
func TestFindExecutableFollowsEnvPATH(t *testing.T) {
	bin := t.TempDir()
	name := "mysleep"
	if runtime.GOOS == "windows" {
		name = "mysleep.EXE"
	}
	mustWrite(t, filepath.Join(bin, name), "x", 0o755)

	env := envPath(bin)
	if got := findExecutable(env, "mysleep"); got == "" {
		t.Fatalf("应在解释器 PATH 里找到 mysleep，实际为空")
	}
	// 进程 PATH 里有 go，但解释器 PATH 指向空目录 → 必须找不到。
	if got := findExecutable(envPath(t.TempDir()), "go"); got != "" {
		t.Fatalf("不应回落到进程 PATH，却找到了 %q", got)
	}
	// 空的 PATH 成分要跳过。
	if got := findExecutable(expand.ListEnviron("PATH="), "ls"); got != "" {
		t.Fatalf("PATH 为空时应返回空，实际 %q", got)
	}
}

// TestShellBuiltinEndToEnd 通过 bash 工具跑一遍回退到内建实现的常用命令。
func TestShellBuiltinEndToEnd(t *testing.T) {
	bt, _ := newBash(t)
	empty := t.TempDir()
	run := func(t *testing.T, script string) string {
		t.Helper()
		res, err := bt.Execute(ctx_(), Args{"command": script, "env": map[string]any{"PATH": empty}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res.Text()
	}

	// 准备文件：用解释器内建 printf + 重定向，不依赖外部程序。
	run(t, "mkdir -p sub/deep && printf 'l1\\nl2\\nl3\\n' > a.txt")

	t.Run("pwd", func(t *testing.T) {
		text := run(t, "pwd")
		if !strings.Contains(text, filepath.Base(bt.Policy.Root)) {
			t.Fatalf("pwd 输出不含 root: %q", text)
		}
	})

	t.Run("ls", func(t *testing.T) {
		text := run(t, "ls")
		if !strings.Contains(text, "sub/") || !strings.Contains(text, "a.txt") {
			t.Fatalf("ls 输出不对: %q", text)
		}
		// 多路径时带 header
		text = run(t, "ls sub a.txt")
		if !strings.Contains(text, "sub:") || !strings.Contains(text, "deep/") {
			t.Fatalf("多参数 ls 输出不对: %q", text)
		}
		// -d 只列目录本身
		if text = run(t, "ls -d sub"); !strings.Contains(text, "sub/") || strings.Contains(text, "deep") {
			t.Fatalf("ls -d 应只列目录本身: %q", text)
		}
		// 不存在的路径要报错且非零退出
		text = run(t, "ls nope || echo FAILED")
		if !strings.Contains(text, "FAILED") || !strings.Contains(text, "No such file") {
			t.Fatalf("ls 错误处理不对: %q", text)
		}
	})

	t.Run("cat", func(t *testing.T) {
		text := run(t, "cat a.txt")
		for _, want := range []string{"l1", "l2", "l3"} {
			if !strings.Contains(text, want) {
				t.Fatalf("cat 缺少 %q: %q", want, text)
			}
		}
		// stdin 管道
		if text = run(t, "cat a.txt | cat"); !strings.Contains(text, "l3") {
			t.Fatalf("cat 读 stdin 失败: %q", text)
		}
		// 缺文件不影响其它文件
		text = run(t, "cat nope.txt a.txt || echo FAILED")
		if !strings.Contains(text, "l1") || !strings.Contains(text, "FAILED") {
			t.Fatalf("cat 多文件处理不对: %q", text)
		}
	})

	t.Run("head", func(t *testing.T) {
		if text := run(t, "head -n 2 a.txt"); strings.Count(text, "l") != 2 || !strings.Contains(text, "l2") {
			t.Fatalf("head -n 2 输出不对: %q", text)
		}
		// 老式 -2 写法
		if text := run(t, "head -2 a.txt"); strings.Count(text, "l") != 2 {
			t.Fatalf("head -2 输出不对: %q", text)
		}
		// head 0
		if text := run(t, "head -n 0 a.txt; echo END"); strings.Contains(text, "l1") {
			t.Fatalf("head -n 0 不应有输出: %q", text)
		}
		// 管道输入的 head
		if text := run(t, "cat a.txt | head -n 1"); !strings.Contains(text, "l1") || strings.Contains(text, "l2") {
			t.Fatalf("管道 head 不对: %q", text)
		}
	})

	t.Run("tail", func(t *testing.T) {
		if text := run(t, "tail -n 1 a.txt"); !strings.Contains(text, "l3") || strings.Contains(text, "l2") {
			t.Fatalf("tail -n 1 输出不对: %q", text)
		}
		// tail -n 大于总行数时输出全部
		if text := run(t, "tail -n 99 a.txt"); !strings.Contains(text, "l1") || !strings.Contains(text, "l3") {
			t.Fatalf("tail -n 99 应输出全部: %q", text)
		}
	})

	t.Run("mkdir_rm", func(t *testing.T) {
		run(t, "mkdir -p d1/d2 && printf x > d1/d2/f.txt")
		if text := run(t, "ls d1/d2"); !strings.Contains(text, "f.txt") {
			t.Fatalf("mkdir -p 后 ls 不对: %q", text)
		}
		// 非空目录不加 -r 不能删
		if text := run(t, "rm d1 || echo FAILED"); !strings.Contains(text, "FAILED") {
			t.Fatalf("rm 非空目录应失败: %q", text)
		}
		if text := run(t, "rm -rf d1 && ls"); strings.Contains(text, "d1") {
			t.Fatalf("rm -rf 未删除目录: %q", text)
		}
		// -f 静默忽略不存在
		if text := run(t, "rm -f nope.txt && echo OK"); !strings.Contains(text, "OK") {
			t.Fatalf("rm -f 不存在文件应静默成功: %q", text)
		}
	})

	t.Run("sleep", func(t *testing.T) {
		start := time.Now()
		run(t, "sleep 0.2 && echo DONE")
		if d := time.Since(start); d < 150*time.Millisecond {
			t.Fatalf("sleep 0.2 只用了 %v", d)
		}
		// 多参数累加
		start = time.Now()
		run(t, "sleep 0.1 0.1 && echo DONE")
		if d := time.Since(start); d < 180*time.Millisecond {
			t.Fatalf("sleep 0.1 0.1 只用了 %v", d)
		}
		// 非法参数
		if text := run(t, "sleep abc || echo FAILED"); !strings.Contains(text, "FAILED") {
			t.Fatalf("非法参数应失败: %q", text)
		}
	})
}

// TestBuiltinHeadLongLine 单行超过 64KB 不能被吞掉（bufio.Scanner 的老坑）。
// 输出重定向到文件再量大小：工具层会把 stdout 截到 50KB。
func TestBuiltinHeadLongLine(t *testing.T) {
	bt, _ := newBash(t)
	long := strings.Repeat("x", 100<<10)
	mustWrite(t, filepath.Join(bt.Policy.Root, "long.txt"), long+"\nsecond\n", 0o644)

	res, err := bt.Execute(ctx_(), Args{"command": "head -n 1 long.txt > out.txt", "env": map[string]any{"PATH": t.TempDir()}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	st, err := os.Stat(filepath.Join(bt.Policy.Root, "out.txt"))
	if err != nil {
		t.Fatalf("out.txt 未生成（%s）: %v", res.Text(), err)
	}
	if got, want := st.Size(), int64(len(long)+1); got != want {
		t.Fatalf("长行被截断：%d 字节，想要 %d", got, want)
	}
}

// TestBuiltinSleepInterruptible 内建 sleep 必须响应 Kill，否则 bash_kill 和
// 前台超时都停不掉它（内建命令没有子进程可杀）。
func TestBuiltinSleepInterruptible(t *testing.T) {
	bt, _ := newBash(t)
	empty := t.TempDir()
	id, err := bt.Manager.Start("sleep 30", bt.Policy.Root, map[string]string{"PATH": empty})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // 确认真的在睡

	start := time.Now()
	if err := bt.Manager.Kill(id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		readRes, _ := bt.Manager.Read(ctx_(), id, 0, "", WaitNone, 0)
		done := readRes.Done
		if done {
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("kill 后 %v 才结束", d)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("内建 sleep 未被 Kill 终止")
}

// TestShellPrefersRealExecutable PATH 里有真程序时必须用它，不能被内建覆盖。
func TestShellPrefersRealExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上不能直接 exec 无扩展名的脚本")
	}
	bt, _ := newBash(t)
	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "ls"), "#!/bin/sh\necho REAL-LS\n", 0o755)

	sep := string(os.PathListSeparator)
	res, err := bt.Execute(ctx_(), Args{
		"command": "ls",
		"env":     map[string]any{"PATH": bin + sep + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text(), "REAL-LS") {
		t.Fatalf("应执行 PATH 里的真 ls，实际输出: %q", res.Text())
	}
}
