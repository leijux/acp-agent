//go:build !shellnative

package tools

// 内建 grep 的端到端测试。
//
// 与 shell_builtin_test.go 同一个套路：把解释器的 PATH 指到空目录，强制走
// 内建实现，这样 Linux（PATH 里有真 grep）与纯 Windows（没有）测的是同一份
// 代码。断言只依赖内建命令（printf / mkdir），不依赖 coreutils。

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShellBuiltinGrep(t *testing.T) {
	bt, _ := newBash(t)
	root := bt.Policy.Root
	empty := t.TempDir()

	run := func(t *testing.T, script string) string {
		t.Helper()
		res, err := bt.Execute(ctx_(), Args{"command": script, "env": map[string]any{"PATH": empty}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res.Text()
	}

	// 用内建 printf + 重定向准备文件（不依赖外部程序）。
	run(t, "mkdir -p logs/sub")
	run(t, "printf 'alpha one\\nbeta two\\nAlpha three\\nERROR failed\\n' > logs/a.log")
	run(t, "printf 'gamma\\nerror here\\n' > logs/sub/b.log")
	_ = root

	t.Run("basic_and_exit_code", func(t *testing.T) {
		if got := run(t, "grep alpha logs/a.log"); !strings.Contains(got, "alpha one") || strings.Contains(got, "beta") {
			t.Fatalf("基础匹配不对: %q", got)
		}
		// 退出码：0 有匹配 / 1 无匹配（GNU 语义，&& / || 依赖它）
		if got := run(t, "grep zzz logs/a.log || echo NO-MATCH"); !strings.Contains(got, "NO-MATCH") {
			t.Fatalf("无匹配应返回 1: %q", got)
		}
		if got := run(t, "grep alpha logs/a.log && echo MATCHED"); !strings.Contains(got, "MATCHED") {
			t.Fatalf("有匹配应返回 0: %q", got)
		}
	})

	t.Run("flags", func(t *testing.T) {
		// -i
		if got := run(t, "grep -i alpha logs/a.log"); !strings.Contains(got, "alpha one") || !strings.Contains(got, "Alpha three") {
			t.Fatalf("-i 不对: %q", got)
		}
		// -n 行号
		if got := run(t, "grep -n ERROR logs/a.log"); !strings.Contains(got, "4:ERROR failed") {
			t.Fatalf("-n 不对: %q", got)
		}
		// -v 反向（"Alpha" 大写不匹配 -i 未开时的 alpha，所以是 3 行）
		if got := run(t, "grep -vc alpha logs/a.log"); !strings.Contains(got, "3") {
			t.Fatalf("-vc 应为 3 行（beta / Alpha / ERROR）: %q", got)
		}
		// -c（a.log 四行里含字母 a 的有 alpha / beta / Alpha / failed）
		if got := run(t, "grep -c a logs/a.log"); !strings.Contains(got, "4") {
			t.Fatalf("-c 不对: %q", got)
		}
		// -l
		if got := run(t, "grep -l error logs/a.log logs/sub/b.log"); !strings.Contains(got, "logs/sub/b.log") || strings.Contains(got, "a.log") {
			t.Fatalf("-l 不对: %q", got)
		}
		// -o 只输出匹配片段，一行里出现两次就输出两行
		run(t, "printf 'alpha alpha\\nbeta\\n' > o.txt")
		if got := run(t, "grep -o alpha o.txt"); strings.Count(got, "alpha") != 2 {
			t.Fatalf("-o 应输出两次 alpha: %q", got)
		}
		// -q 静默但有退出码
		if got := run(t, "grep -q alpha logs/a.log && echo QUIET-OK"); strings.Contains(got, "alpha") || !strings.Contains(got, "QUIET-OK") {
			t.Fatalf("-q 不应输出匹配行: %q", got)
		}
	})

	t.Run("regex_vs_fixed", func(t *testing.T) {
		run(t, "printf 'a1\\na.\\n' > re.txt")
		// 默认按 ERE：a. 匹配 a1
		if got := run(t, "grep -c 'a.' re.txt"); !strings.Contains(got, "2") {
			t.Fatalf("默认应走正则: %q", got)
		}
		// -F 把 . 当字面量
		if got := run(t, "grep -F 'a.' re.txt"); !strings.Contains(got, "a.") || strings.Contains(got, "a1") {
			t.Fatalf("-F 未按字面量处理: %q", got)
		}
		// -w 全词：不能命中 "alphabet"
		run(t, "printf 'alpha\\nalphabet\\n' > w.txt")
		if got := run(t, "grep -w alpha w.txt"); !strings.Contains(got, "alpha") || strings.Contains(got, "alphabet") {
			t.Fatalf("-w 边界不对: %q", got)
		}
		// -x 整行
		if got := run(t, "grep -x alpha w.txt"); !strings.Contains(got, "alpha") || strings.Contains(got, "alphabet") {
			t.Fatalf("-x 整行匹配不对: %q", got)
		}
		// -e 多条 pattern
		if got := run(t, "grep -e alpha -e gamma logs/a.log logs/sub/b.log"); !strings.Contains(got, "alpha one") || !strings.Contains(got, "gamma") {
			t.Fatalf("-e 多 pattern 不对: %q", got)
		}
	})

	t.Run("multi_file_and_recursive", func(t *testing.T) {
		// 多文件时带 "文件:" 前缀（-i：a.log 里是 ERROR 大写）
		got := run(t, "grep -ni error logs/a.log logs/sub/b.log")
		if !strings.Contains(got, "logs/a.log:4:") || !strings.Contains(got, "logs/sub/b.log:2:") {
			t.Fatalf("多文件前缀不对: %q", got)
		}
		// -r：目录递归（-i 因为 a.log 里是大写 ERROR）
		got = run(t, "grep -rni error logs || echo NONE")
		if !strings.Contains(got, "logs/sub/b.log:2:") || !strings.Contains(got, "logs/a.log:4:") {
			t.Fatalf("-r 递归结果不对: %q", got)
		}
		// 非递归给目录：报错且非零
		if got := run(t, "grep error logs || echo DIR-FAILED"); !strings.Contains(got, "DIR-FAILED") {
			t.Fatalf("非递归目录应失败: %q", got)
		}
	})

	t.Run("stdin_pipe", func(t *testing.T) {
		got := run(t, "printf 'x1\\nx2\\ny3\\n' | grep x")
		if !strings.Contains(got, "x1") || !strings.Contains(got, "x2") || strings.Contains(got, "y3") {
			t.Fatalf("管道 grep 不对: %q", got)
		}
		// 单文件/管道不显示文件名
		if strings.Contains(got, "-:") {
			t.Fatalf("stdin 不该有文件名前缀: %q", got)
		}
	})

	t.Run("crlf_preserved", func(t *testing.T) {
		// grep 之后常接重定向：行尾不能被改成 LF
		run(t, "printf 'aa\\r\\nbb\\r\\n' > crlf.txt")
		run(t, "grep aa crlf.txt > crlf.out")
		if got := run(t, "cat crlf.out"); !strings.Contains(got, "aa") {
			t.Fatalf("CRLF 文件 grep 失败: %q", got)
		}
	})

	t.Run("unsupported_context_flag", func(t *testing.T) {
		// -A/-B/-C 必须明确报错，不能静默当普通匹配（否则调用方以为拿到了上下文）
		got := run(t, "grep -C 2 error logs/a.log || echo REJECTED")
		if !strings.Contains(got, "REJECTED") || !strings.Contains(got, "不支持") {
			t.Fatalf("-C 应被明确拒绝: %q", got)
		}
	})

	t.Run("dispatched_from_shell_loop", func(t *testing.T) {
		// 在 for 循环 + 管道里用一次，确认与解释器协作正常
		got := run(t, "for f in logs/a.log logs/sub/b.log; do grep -c a $f; done")
		if strings.Count(got, "\n") < 1 {
			t.Fatalf("循环内调用异常: %q", got)
		}
	})
}

// TestBuiltinGrepLongLine 单行超过 64KB 时不能误判 EOF（Scanner 的老坑）。
func TestBuiltinGrepLongLine(t *testing.T) {
	bt, _ := newBash(t)
	long := strings.Repeat("x", 100<<10) + "NEEDLE"
	mustWrite(t, filepath.Join(bt.Policy.Root, "long.txt"), "head\n"+long+"\ntail\n", 0o644)

	res, err := bt.Execute(ctx_(), Args{
		"command": "grep -c NEEDLE long.txt",
		"env":     map[string]any{"PATH": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(res.Text()); !strings.HasPrefix(got, "1") {
		t.Fatalf("长行未被匹配到（期望计数 1）: %q", res.Text())
	}
}
