//go:build !shellnative

package tools

// 内建命令 `moejs`：在 shell 里直接执行 JavaScript。
//
// 为什么值得存在：模型（和人）经常需要「对命令输出做点变形再判断」——
// 统计、过滤、JSON 提取。以前只有两条路：调 js 工具（把数据手动搬进 args），
// 或者调系统 python/node（换台机器可能没有）。有了内建 moejs，
//
//	jq_like() { moejs -e '...' ; }
//	ls -l | moejs -e 'const s = fs.readFile("/dev/stdin")'
//	git status --short | moejs -e 'fs.readFile("/dev/stdin").split("\n").length'
//
// 就能在管道里做纯计算，且跨平台（不依赖任何外部程序）。
//
// 与 js 工具的能力对齐：console / 只读 fs / crypto.sha256 / 超时中断；
// 差异：输出直接写终端（warn/error 走 stderr）、多一个 argv、默认 30s 超时。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"mvdan.cc/sh/v3/interp"
)

// moejsDefaultTimeout 是内建 moejs 的默认超时。
//
// 比 js 工具的 5s 宽松：shell 里常常要扫一批文件，5s 容易误杀；
// 但仍然必须有个上限，否则一个死循环会把整个 bash 工具卡住
// （纯内建命令没有子进程可杀，只能靠 vm.Interrupt）。
const moejsDefaultTimeout = 30 * time.Second

const moejsUsage = `用法: moejs [选项] [脚本文件] [参数...]

不带脚本文件时从 stdin 读。选项:
  -e, --eval <code>    直接执行代码；此时管道数据用 fs.readStdin() 读
  -t, --timeout <时长> 执行上限，如 30s / 1m（默认 30s）
  -q, --quiet          不打印最后一个表达式的值
  -h, --help           显示本帮助
  --                   之后全部当作脚本参数（写入 argv）

（执行引擎是纯 Go 的 moejs）
脚本内可用: console.log/info/warn/error、fs.readFile/readFileBytes/readFileBase64/
readDir/stat/exists/glob/readStdin（只读，受 Root 约束）、crypto.sha256、argv。

示例:
  moejs -e '1 + 2'
  ls -l | moejs -e 'fs.readStdin().split("\n").length'
  moejs script.js a b c      # argv 为 ["a","b","c"]
`

// cmdMoejs 实现内建 moejs 命令。
func cmdMoejs(ctx context.Context, hc interp.HandlerContext, args []string, policy Policy) error {
	var (
		script  string
		srcName = "<stdin>"
		timeout = moejsDefaultTimeout
		quiet   bool
		argv    []string
		// scriptFromStdin 记录脚本本身是不是从 stdin 读的：
		// 那就不能再给脚本读同一根管道。
		scriptFromStdin bool
	)

	parseFailed := func(format string, a ...any) error { return usagef(hc, format, a...) }

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			// 必须在「脚本已确定」之前判断，否则 -- 本身会被当成脚本参数。
			argv = append(argv, args[i+1:]...)
			i = len(args)

		case script != "":
			// 脚本已经确定，后续参数都是脚本参数。
			argv = append(argv, a)

		case a == "-h" || a == "--help":
			fmt.Fprint(hc.Stdout, moejsUsage)
			return nil

		case a == "-q" || a == "--quiet":
			quiet = true

		case a == "-e" || a == "--eval":
			if i+1 >= len(args) {
				return parseFailed("moejs: %s 需要一个代码参数", a)
			}
			i++
			script, srcName = args[i], "-e"

		case a == "-t" || a == "--timeout":
			if i+1 >= len(args) {
				return parseFailed("moejs: %s 需要一个时长", a)
			}
			i++
			d, err := parseMoejsTimeout(args[i])
			if err != nil || d <= 0 {
				return parseFailed("moejs: 无效的时长 %q（如 200ms / 30s / 1m）", args[i])
			}
			timeout = d

		case strings.HasPrefix(a, "-") && a != "-":
			return parseFailed("moejs: 未知选项 %q（--help 看用法）", a)

		default:
			// 第一个非选项参数是脚本文件。
			if a == "-" {
				data, err := readAllLimited(builtinStdin(hc), 4<<20)
				if err != nil {
					return parseFailed("moejs: 读取 stdin 失败: %v", err)
				}
				script, srcName = string(data), "<stdin>"
				scriptFromStdin = true
				continue
			}
			data, err := os.ReadFile(hcAbs(hc, a))
			if err != nil {
				fmt.Fprintf(hc.Stderr, "moejs: %s: %s\n", a, errText(err))
				return exitStatus(1)
			}
			script, srcName = string(data), a
		}
	}

	if strings.TrimSpace(script) == "" {
		// 没有脚本文件也没 -e：从 stdin 读（管道里的常见用法）。
		data, err := readAllLimited(builtinStdin(hc), 4<<20)
		if err != nil {
			return parseFailed("moejs: 读取 stdin 失败: %v", err)
		}
		script, srcName = string(data), "<stdin>"
		scriptFromStdin = true
		if strings.TrimSpace(script) == "" {
			return parseFailed("moejs: 没有脚本内容（-e 或文件名，或从 stdin 传入）")
		}
	}

	return runMoejs(ctx, hc, policy, srcName, script, argv, timeout, quiet, !scriptFromStdin)
}

// parseMoejsTimeout 解析 -t 的时长：支持 ms/s/m/h/d 后缀，纯数字按秒。
//
// 不复用 sleep 的 parseDuration：那里没有 ms（GNU sleep 也没有），
// 但脚本超时用毫秒最自然。
func parseMoejsTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	unit := time.Second
	mult := time.Duration(1)
	for _, suf := range []struct {
		suffix string
		unit   time.Duration
	}{
		{"ms", time.Millisecond},
		{"s", time.Second},
		{"m", time.Minute},
		{"h", time.Hour},
		{"d", 24 * time.Hour},
	} {
		if strings.HasSuffix(strings.ToLower(s), suf.suffix) {
			unit = suf.unit
			s = s[:len(s)-len(suf.suffix)]
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid duration")
	}
	return time.Duration(f * float64(unit) * float64(mult)), nil
}

// runMoejs 执行脚本并把结果写到 hc。
func runMoejs(ctx context.Context, hc interp.HandlerContext, policy Policy, srcName, script string, argv []string, timeout time.Duration, quiet, stdinFree bool) error {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: jsMemoryLimit})

	sink := &consoleSink{out: hc.Stdout, errOut: hc.Stderr}
	if err := installConsole(rt, sink); err != nil {
		fmt.Fprintf(hc.Stderr, "moejs: 初始化 console 失败: %v\n", err)
		return exitStatus(1)
	}
	// 管道里最常见的写法是 `cmd | moejs -e '...'`：脚本来自 -e，数据从 stdin 来。
	// Windows 上没有 /dev/stdin，stdin 又是解释器的 io.Reader 而不是文件，
	// 所以只能由宿主提供 fs.readStdin()。它必须在装 fs 的同一次 SetGlobal 里
	// 加进去 —— moejs 建好的全局对象没有「事后加一个属性」的顶层 API。
	fsObj := newJSFS(policy).methods(rt)
	if stdinFree {
		fsObj["readStdin"] = stdinReader(rt, builtinStdin(hc), 8<<20)
	}
	if err := rt.SetGlobal("fs", fsObj); err != nil {
		fmt.Fprintf(hc.Stderr, "moejs: 初始化 fs 失败: %v\n", err)
		return exitStatus(1)
	}
	if err := installCrypto(rt); err != nil {
		fmt.Fprintf(hc.Stderr, "moejs: 初始化 crypto 失败: %v\n", err)
		return exitStatus(1)
	}
	if err := installArgv(rt, argv); err != nil {
		fmt.Fprintf(hc.Stderr, "moejs: 注入 argv 失败: %v\n", err)
		return exitStatus(1)
	}

	// 超时与取消都靠 Interrupt（moejs 支持跨 goroutine 调用）。
	timer := time.AfterFunc(timeout, func() { rt.Interrupt("timeout") })
	defer timer.Stop()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			rt.Interrupt("canceled")
		case <-stopWatch:
		}
	}()

	value, err := runJSScript(rt, srcName, script)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s\n", jsErrorText(err, int64(timeout/time.Millisecond)))
		return exitStatus(1)
	}
	if quiet {
		return nil
	}
	// 打印最后一个表达式的值（真正的 undefined/null 不打印）——`moejs -e '1+2'` 得到 3。
	//
	// 注意不能用「字符串等于 "undefined"」来判断：脚本返回**字符串** "undefined"
	// 时也要打印出来（`typeof x` 的结果就是这样）。
	if !value.IsNullish() {
		if s := jsValueString(rt, value); s != "" {
			fmt.Fprintln(hc.Stdout, s)
		}
	}
	return nil
}

// stdinReader 构造 fs.readStdin()：只允许读一次（管道不可回退）。
//
// 有了它，`cmd | moejs -e '...'` 这种最自然的写法才成立；否则脚本来自 -e 时
// 上游数据根本没法被读到（Windows 连 /dev/stdin 都没有）。
func stdinReader(rt *moejs.Runtime, r io.Reader, limit int64) moejs.NativeFunc {
	used := false
	return func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		if used {
			return moejs.Undefined(), throwErr("readStdin: stdin 只能读一次")
		}
		used = true
		data, err := readAllLimited(r, limit)
		if err != nil {
			return moejs.Undefined(), throwErr("readStdin: %v", err)
		}
		return moejs.String(string(data)), nil
	}
}

// installArgv 把命令行参数注入为原生的 JS 数组。
func installArgv(rt *moejs.Runtime, argv []string) error {
	if len(argv) == 0 {
		return rt.SetGlobal("argv", []string{})
	}
	data, err := json.Marshal(argv)
	if err != nil {
		return err
	}
	v, err := rt.ParseJSONString(string(data))
	if err != nil {
		return err
	}
	return rt.SetGlobal("argv", v)
}

// readAllLimited 读全部内容但不超过 limit 字节（防止在管道里吃爆内存）。
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("输入超过 %d 字节", limit)
	}
	return b, nil
}
