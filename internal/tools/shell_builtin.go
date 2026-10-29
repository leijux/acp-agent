//go:build !shellnative

package tools

// 内建命令回退层。
//
// 内置引擎（mvdan/sh）只实现 shell 内建命令（cd/echo/test/printf/…）；
// sleep、ls、cat 这些在 POSIX 里本来就是外部程序，解释器会去 exec 它们。
// Linux/macOS 的 PATH 里自然有，但纯 Windows 环境往往一个 coreutils 都没有
// （连 sleep 都没有），于是 `ls`、`sleep 1` 直接 exit 127 —— 模型在纯 Windows
// 上基本没法用 bash 工具干活。
//
// 这里用 interp.ExecHandlers 中间件补一层：PATH 里能找到同名可执行文件时
// 一律让真程序跑（Git Bash / MSYS / scoop 装了的用户行为不变），找不到才用
// 下面这些纯 Go 实现顶上。命令语义与 GNU coreutils 保持接近，只在少数细节上
// 有差异（见 docs/tools.md）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// builtinCmd 是一个内建命令的实现。hc 提供当前 cwd、stdin/stdout/stderr 与环境。
type builtinCmd func(ctx context.Context, hc interp.HandlerContext, args []string) error

var builtinCmds = map[string]builtinCmd{
	"sleep": cmdSleep,
	"ls":    cmdLs,
	"cat":   cmdCat,
	"head":  cmdHead,
	"tail":  cmdTail,
	"pwd":   cmdPwd,
	"mkdir": cmdMkdir,
	"rm":    cmdRm,
	"grep":  cmdGrep,
	"cp":    cmdCp,
}

// builtinExecHandler 返回 ExecHandler 中间件。policy 用于约束需要访问文件的
// 内建命令（目前是 goja 的 fs 桥）。
func builtinExecHandler(policy Policy) func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			hc := interp.HandlerCtx(ctx)
			// goja 需要一个 policy，单独分派（其它内建命令的签名里没有它）。
			if args[0] == "goja" && findExecutable(hc.Env, args[0]) == "" {
				return cmdGoja(ctx, hc, args[1:], policy)
			}
			fn, ok := builtinFor(hc.Env, args[0])
			if !ok {
				return next(ctx, args)
			}
			return fn(ctx, hc, args[1:])
		}
	}
}

// builtinFor 判断命令该由内建实现接手：命令名认识，且 PATH 里没有同名可执行文件。
func builtinFor(env expand.Environ, name string) (builtinCmd, bool) {
	fn, ok := builtinCmds[name]
	if !ok {
		return nil, false
	}
	if findExecutable(env, name) != "" {
		return nil, false
	}
	return fn, true
}

// findExecutable 按解释器当前的 PATH 查找可执行文件，找不到返回 ""。
//
// 不能直接用 exec.LookPath：它查的是进程自己的 PATH，脚本里
// `export PATH=...` 之后就对不上了。Windows 上还要按 PATHEXT 补扩展名。
func findExecutable(env expand.Environ, name string) string {
	if name == "" {
		return ""
	}
	if strings.ContainsAny(name, `/\`) {
		if isExecutableFile(name) {
			return name
		}
		return ""
	}
	path := env.Get("PATH").String()
	if path == "" {
		return ""
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = pathexts(env)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			cand := filepath.Join(dir, name+ext)
			if isExecutableFile(cand) {
				return cand
			}
		}
	}
	return ""
}

func pathexts(env expand.Environ) []string {
	raw := strings.TrimSpace(env.Get("PATHEXT").String())
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	out := []string{""}
	for _, e := range strings.Split(raw, ";") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e, strings.ToLower(e))
		}
	}
	return out
}

func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// ---------------------------------------------------------------------------
// 内建命令实现
// ---------------------------------------------------------------------------

// exitStatus 返回指定退出码，供内建命令直接返回。
func exitStatus(code int) error { return interp.ExitStatus(uint8(code)) }

func usagef(hc interp.HandlerContext, format string, a ...any) error {
	fmt.Fprintf(hc.Stderr, format+"\n", a...)
	return exitStatus(2)
}

// hcAbs 把命令参数里的路径解析成绝对路径（相对解释器当前目录）。
func hcAbs(hc interp.HandlerContext, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(hc.Dir, p)
}

// parseDuration 解析 sleep 的时间参数：纯数字为秒，支持 s/m/h/d 后缀。
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f < 0 {
			return 0, errors.New("negative")
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	unit := time.Second
	switch s[len(s)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid unit")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid duration")
	}
	return time.Duration(f * float64(unit)), nil
}

// cmdSleep 睡指定时长。多个参数按 GNU 语义累加。
//
// 必须响应 ctx：内建命令没有子进程可杀，除非自己退出，bash_kill / 前台超时
// 都停不掉它（mvdan/sh 没有「中止脚本」的 API）。
func cmdSleep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 0 {
		return usagef(hc, "sleep: missing operand")
	}
	var total time.Duration
	for _, a := range args {
		d, err := parseDuration(a)
		if err != nil {
			return usagef(hc, "sleep: invalid time interval %q", a)
		}
		total += d
	}
	if total <= 0 {
		return nil
	}
	t := time.NewTimer(total)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cmdPwd 打印解释器当前目录。
func cmdPwd(_ context.Context, hc interp.HandlerContext, args []string) error {
	for _, a := range args {
		switch a {
		case "-L", "-P", "--logical", "--physical":
		default:
			return usagef(hc, "pwd: invalid option %q", a)
		}
	}
	fmt.Fprintln(hc.Stdout, hc.Dir)
	return nil
}

// cmdLs 列出目录内容。支持 -a / -l / -1 / -d，目录名带 "/" 后缀（方便区分目录与文件）。
func cmdLs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var all, long, dirsOnly bool
	var paths []string
	flagsDone := false
	for _, a := range args {
		switch {
		case flagsDone || a == "-" || !strings.HasPrefix(a, "-"):
			paths = append(paths, a)
		case a == "--":
			flagsDone = true
		default:
			for _, c := range a[1:] {
				switch c {
				case 'a', 'A':
					all = true
				case 'l':
					long = true
				case '1':
					// 非 tty 场景本来就是一行一个，接受即可
				case 'd':
					dirsOnly = true
				default:
					return usagef(hc, "ls: invalid option -- %q", string(c))
				}
			}
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var dirs, files []string
	status := 0
	for _, p := range paths {
		st, err := os.Stat(hcAbs(hc, p))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: %s: %s\n", p, errText(err))
			status = 1
			continue
		}
		if st.IsDir() && !dirsOnly {
			dirs = append(dirs, p)
		} else {
			files = append(files, p)
		}
	}

	showHeader := len(paths) > 1
	for _, p := range files {
		st, err := os.Stat(hcAbs(hc, p))
		if err != nil {
			continue
		}
		if err := writeEntry(hc, p, st.IsDir(), st, long); err != nil {
			return err
		}
	}
	for i, d := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if showHeader || len(files) > 0 {
			if i > 0 || len(files) > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintf(hc.Stdout, "%s:\n", d)
		}
		if err := listDir(ctx, hc, d, all, long); err != nil {
			fmt.Fprintf(hc.Stderr, "ls: %s: %s\n", d, errText(err))
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

func listDir(ctx context.Context, hc interp.HandlerContext, dir string, all, long bool) error {
	entries, err := os.ReadDir(hcAbs(hc, dir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if !all && strings.HasPrefix(name, ".") {
			continue
		}
		// 非 -l 不需要额外 stat：DirEntry 自带类型信息。
		var info fs.FileInfo
		if long {
			if i, ierr := e.Info(); ierr == nil {
				info = i
			}
		}
		if err := writeEntry(hc, name, e.IsDir(), info, long); err != nil {
			return err
		}
	}
	return nil
}

// writeEntry 输出单个条目：-l 时带权限/大小/时间，否则只输出名字。
func writeEntry(hc interp.HandlerContext, name string, isDir bool, info fs.FileInfo, long bool) error {
	display := name
	if isDir && !strings.HasSuffix(display, string(filepath.Separator)) {
		display += "/"
	}
	if !long || info == nil {
		_, err := fmt.Fprintln(hc.Stdout, display)
		return err
	}
	size := humanSize(info.Size())
	if isDir {
		size = "-"
	}
	_, err := fmt.Fprintf(hc.Stdout, "%-10s %8s %s %s\n",
		info.Mode().String(), size, info.ModTime().Format("2006-01-02 15:04"), display)
	return err
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// cmdCat 输出文件内容；无参数或 "-" 时读 stdin。
func cmdCat(ctx context.Context, hc interp.HandlerContext, args []string) error {
	files := args
	if len(files) == 0 {
		files = []string{"-"}
	}
	status := 0
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f == "-" {
			if _, err := io.Copy(hc.Stdout, hc.Stdin); err != nil {
				return exitStatus(1)
			}
			continue
		}
		fh, err := os.Open(hcAbs(hc, f))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cat: %s: %s\n", f, errText(err))
			status = 1
			continue
		}
		_, cerr := io.Copy(hc.Stdout, fh)
		_ = fh.Close()
		if cerr != nil {
			fmt.Fprintf(hc.Stderr, "cat: %s: %v\n", f, cerr)
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

func cmdHead(ctx context.Context, hc interp.HandlerContext, args []string) error {
	return headTail(ctx, hc, args, true)
}

func cmdTail(ctx context.Context, hc interp.HandlerContext, args []string) error {
	return headTail(ctx, hc, args, false)
}

// headTail 实现 head/tail：-n N（也接受 -nN 与 -N），-q / -v 控制多文件表头。
func headTail(ctx context.Context, hc interp.HandlerContext, args []string, head bool) error {
	name := "head"
	if !head {
		name = "tail"
	}
	n := 10
	quiet := false
	verbose := false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--lines":
			if i+1 >= len(args) {
				return usagef(hc, "%s: option requires an argument -- 'n'", name)
			}
			i++
			v, err := parseCount(args[i])
			if err != nil {
				return usagef(hc, "%s: invalid number of lines: %q", name, args[i])
			}
			n = v
		case strings.HasPrefix(a, "-n") && len(a) > 2:
			v, err := parseCount(a[2:])
			if err != nil {
				return usagef(hc, "%s: invalid number of lines: %q", name, a[2:])
			}
			n = v
		case a == "-q" || a == "--quiet" || a == "--silent":
			quiet = true
		case a == "-v" || a == "--verbose":
			verbose = true
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case len(a) > 1 && a[0] == '-' && isDigits(a[1:]):
			n, _ = strconv.Atoi(a[1:])
		case len(a) > 1 && a[0] == '-':
			return usagef(hc, "%s: invalid option -- %q", name, a[1:])
		default:
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	showHeader := (len(files) > 1 || verbose) && !quiet

	status := 0
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if showHeader {
			if i > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintf(hc.Stdout, "==> %s <==\n", f)
		}
		if f == "-" {
			if err := copyLines(ctx, hc.Stdin, hc.Stdout, n, head); err != nil {
				return err
			}
			continue
		}
		fh, err := os.Open(hcAbs(hc, f))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s: %s\n", name, f, errText(err))
			status = 1
			continue
		}
		err = copyLines(ctx, fh, hc.Stdout, n, head)
		_ = fh.Close()
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s: %v\n", name, f, err)
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

// copyLines 逐行复制：head 取前 n 行后提前停止，tail 用环形缓冲保留最后 n 行。
//
// 用 bufio.Reader 而不是 Scanner：单行超过 64KB（压缩过的 JS、minified 数据）时
// Scanner 会直接报 token too long。
func copyLines(ctx context.Context, r io.Reader, w io.Writer, n int, head bool) error {
	if n < 0 {
		n = 0
	}
	br := bufio.NewReaderSize(r, 64<<10)
	if head {
		for i := 0; i < n; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, err := br.ReadString('\n')
			if line != "" {
				if _, werr := io.WriteString(w, line); werr != nil {
					return werr
				}
			}
			if err != nil {
				return nil // EOF 或读错误，head 都到此为止
			}
		}
		return nil
	}
	if n == 0 {
		return nil
	}
	ring := make([]string, 0, n)
	pos := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadString('\n')
		if line != "" {
			if len(ring) < n {
				ring = append(ring, line)
			} else {
				ring[pos] = line
				pos = (pos + 1) % n
			}
		}
		if err != nil {
			break
		}
	}
	for i := 0; i < len(ring); i++ {
		if _, werr := io.WriteString(w, ring[(pos+i)%len(ring)]); werr != nil {
			return werr
		}
	}
	return nil
}

func parseCount(s string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "+")))
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid count")
	}
	return v, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// cmdMkdir 创建目录，支持 -p。
func cmdMkdir(_ context.Context, hc interp.HandlerContext, args []string) error {
	parents := false
	flagsDone := false
	var paths []string
	for _, a := range args {
		switch {
		case flagsDone || a == "-" || !strings.HasPrefix(a, "-"):
			paths = append(paths, a)
		case a == "--":
			flagsDone = true
		case a == "-p" || a == "--parents":
			parents = true
		default:
			return usagef(hc, "mkdir: invalid option -- %q", a[1:])
		}
	}
	if len(paths) == 0 {
		return usagef(hc, "mkdir: missing operand")
	}
	status := 0
	for _, p := range paths {
		var err error
		if parents {
			err = os.MkdirAll(hcAbs(hc, p), 0o755)
		} else {
			err = os.Mkdir(hcAbs(hc, p), 0o755)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "mkdir: %s: %s\n", p, errText(err))
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

// cmdRm 删除文件/目录，支持 -r / -R / -f。危险命令由 Policy.CheckCommand 在入口拦。
func cmdRm(ctx context.Context, hc interp.HandlerContext, args []string) error {
	recursive, force := false, false
	var paths []string
	flagsDone := false
	for _, a := range args {
		switch {
		case flagsDone || a == "-" || !strings.HasPrefix(a, "-"):
			paths = append(paths, a)
		case a == "--":
			flagsDone = true
		default:
			for _, c := range a[1:] {
				switch c {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				case 'v':
					// 忽略 -v（不打印删除过程）
				default:
					return usagef(hc, "rm: invalid option -- %q", string(c))
				}
			}
		}
	}
	if len(paths) == 0 {
		if force {
			return nil
		}
		return usagef(hc, "rm: missing operand")
	}
	status := 0
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		abs := hcAbs(hc, p)
		st, err := os.Lstat(abs)
		if err != nil {
			if force && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			fmt.Fprintf(hc.Stderr, "rm: %s: %s\n", p, errText(err))
			status = 1
			continue
		}
		if st.IsDir() && !recursive {
			fmt.Fprintf(hc.Stderr, "rm: %s: is a directory\n", p)
			status = 1
			continue
		}
		if recursive {
			err = os.RemoveAll(abs)
		} else {
			err = os.Remove(abs)
		}
		if err != nil {
			if force && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			fmt.Fprintf(hc.Stderr, "rm: %s: %s\n", p, errText(err))
			status = 1
		}
	}
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}
