//go:build !shellnative

package tools

// 内建 grep：PATH 里没有真 grep 时顶上（纯 Windows 环境常见 —— grep 通常
// 跟着 Git Bash/MSYS 才来，裸 Windows 上一个 coreutils 都没有）。
//
// 与真 grep 的差异（都在 docs/tools.md 里列着）：
//
//   - 只做整行/子串匹配，没有 -A/-B/-C 上下文，也没有 -m/-P：这些选项会
//     明确报错而不是静默忽略 —— 静默忽略会让调用方以为拿到了上下文。
//   - 正则引擎是 Go 的 RE2，默认就按扩展正则（ERE）解析：GNU 默认的 BRE 里
//     `\+`、`\?`、`\{n,m\}` 才特殊，这里反过来（`+?{}` 直接特殊）。模型写
//     ERE 的概率远高于 BRE，选这一侧。
//   - 不检测二进制文件（真 grep 会打 "Binary file ... matches"），按字面行扫。
//   - 递归时符号链接目录不跟随（filepath.WalkDir 的默认语义）。
//
// 退出码与 GNU 一致：0 = 至少一处匹配，1 = 无匹配，2 = 出错（打不开文件、
// 选项非法、正则无效）。`grep x || echo none` 这种写法才靠得住。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// grepOpts 是解析后的开关集合。
type grepOpts struct {
	ignoreCase bool // -i
	invert     bool // -v
	lineNo     bool // -n
	count      bool // -c
	listMatch  bool // -l
	listNo     bool // -L
	onlyMatch  bool // -o
	quiet      bool // -q
	word       bool // -w
	wholeLine  bool // -x
	fixed      bool // -F
	recursive  bool // -r / -R
	forceName  bool // -H
	hideName   bool // -h

	before int // -B：命中行之前显示几行
	after  int // -A：命中行之后显示几行
}

func cmdGrep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts, patterns, paths, err := parseGrepArgs(hc, args)
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		return usagef(hc, "grep: missing pattern")
	}
	m, err := newGrepMatcher(opts, patterns)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "grep: %v\n", err)
		return exitStatus(2)
	}

	inputs, hadErr := grepInputs(ctx, hc, opts, paths)
	// 单文件不显示文件名，多文件/递归/显式 -H 才显示（GNU 语义）。
	showName := (len(inputs) > 1 || opts.recursive || opts.forceName) && !opts.hideName

	status := 2
	if hadErr {
		status = 2
	}
	anyMatch := false
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		matched, err := grepOne(ctx, hc, in, showName, opts, m)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			fmt.Fprintf(hc.Stderr, "grep: %s: %v\n", in, err)
			hadErr = true
			continue
		}
		if matched {
			anyMatch = true
			// -q 只关心「有没有」：第一个匹配就可以收工。
			if opts.quiet {
				return nil
			}
		}
	}

	switch {
	case hadErr:
		return exitStatus(status)
	case !anyMatch:
		return exitStatus(1)
	default:
		return nil
	}
}

// grepInputs 把路径列表展开成真正的输入（"-" 表示 stdin）。
//
// -r 时目录走 WalkDir（字典序，结果稳定）；非递归时目录按 GNU 语义报错。
func grepInputs(ctx context.Context, hc interp.HandlerContext, opts grepOpts, paths []string) ([]string, bool) {
	if len(paths) == 0 {
		if opts.recursive {
			paths = []string{"."}
		} else {
			return []string{"-"}, false
		}
	}
	var out []string
	hadErr := false
	for _, p := range paths {
		if p == "-" {
			out = append(out, "-")
			continue
		}
		abs := hcAbs(hc, p)
		st, err := os.Stat(abs)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "grep: %s: %s\n", p, errText(err))
			hadErr = true
			continue
		}
		if !st.IsDir() {
			out = append(out, p)
			continue
		}
		if !opts.recursive {
			fmt.Fprintf(hc.Stderr, "grep: %s: Is a directory\n", p)
			hadErr = true
			continue
		}
		walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// 无权限的子目录/竞态删除：报一行继续走，别让整次查找失败。
				fmt.Fprintf(hc.Stderr, "grep: %s: %s\n", path, errText(err))
				hadErr = true
				return nil
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if d.IsDir() {
				return nil
			}
			if rel, rerr := filepath.Rel(hc.Dir, path); rerr == nil {
				// 统一成 /：脚本世界（sh）里路径就是 / 分隔的，输出也要能直接
				// 喂回其他命令。Windows 的 filepath.Rel 会给出反斜杠。
				out = append(out, filepath.ToSlash(rel))
			} else {
				out = append(out, path)
			}
			return nil
		})
		if walkErr != nil {
			return out, true
		}
	}
	return out, hadErr
}

// grepOne 扫描一个输入，返回是否有匹配。
func grepOne(ctx context.Context, hc interp.HandlerContext, name string, showName bool, opts grepOpts, m *grepMatcher) (bool, error) {
	var r io.Reader = hc.Stdin
	if name != "-" {
		fh, err := os.Open(hcAbs(hc, name))
		if err != nil {
			return false, fmt.Errorf("%s", errText(err))
		}
		defer fh.Close()
		r = fh
	}

	br := bufio.NewReaderSize(r, 64<<10)
	matchedAny := false
	count := 0
	lineNo := 0
	// 上下文：before 是最近未输出的 N 行（命中时补打），afterLeft 是还要追打的
	// 行数，lastPrinted 保证同一行不重复出现（相邻命中的上下文会重叠）。
	var before []string
	afterLeft := 0
	lastPrinted := 0
	// 上下文行用 '-' 分隔（GNU 语义），命中行用 ':'。
	emit := func(no int, s, eol string, isCtx bool) {
		if no <= lastPrinted {
			return
		}
		writeGrepLine(hc, name, showName, opts.lineNo, no, s, eol, isCtx)
		lastPrinted = no
	}
	// 上下文只对「逐行输出」的形态有意义（-c/-l/-L/-q 是文件级输出）。
	wantCtx := !opts.count && !opts.listMatch && !opts.listNo && !opts.quiet
	for {
		if err := ctx.Err(); err != nil {
			return matchedAny, err
		}
		// 用 ReadString 而不是 Scanner：单行超过 64KB（minified 数据、长 JSON）
		// 时 Scanner 会误判 EOF。
		raw, rerr := br.ReadString('\n')
		if raw == "" && rerr != nil {
			break
		}
		lineNo++
		body, eol := splitLineEnding(raw)

		hits := m.find(body)
		hit := len(hits) > 0
		if opts.invert {
			hit = !hit
		}
		if hit {
			matchedAny = true
			switch {
			case opts.quiet:
				return true, nil
			case opts.listMatch:
				// -l：第一个匹配就够，输出文件名（stdin 按 GNU 文案）。
				if name == "-" {
					fmt.Fprintln(hc.Stdout, "(standard input)")
				} else {
					fmt.Fprintln(hc.Stdout, name)
				}
				return true, nil
			case opts.count:
				count++
			case opts.onlyMatch && !opts.invert:
				for _, h := range hits {
					writeGrepLine(hc, name, showName, opts.lineNo, lineNo, body[h[0]:h[1]], eol, false)
				}
			default:
				if wantCtx && opts.before > 0 {
					start := lineNo - len(before)
					for k, b := range before {
						emit(start+k, b, "\n", true)
					}
				}
				emit(lineNo, body, eol, false)
				afterLeft = opts.after
			}
		} else if wantCtx && afterLeft > 0 {
			emit(lineNo, body, eol, true)
			afterLeft--
		}
		if wantCtx && opts.before > 0 {
			before = append(before, body)
			if len(before) > opts.before {
				before = before[1:]
			}
		}
		if rerr != nil {
			break
		}
	}

	// -c / -L 是"文件级"输出，扫完才写。
	if opts.count && !opts.quiet && !opts.listMatch && !opts.listNo {
		prefix := ""
		if showName {
			prefix = name + ":"
		}
		fmt.Fprintf(hc.Stdout, "%s%d\n", prefix, count)
	}
	if opts.listNo && !matchedAny {
		fmt.Fprintln(hc.Stdout, name)
	}
	return matchedAny, nil
}

// splitLineEnding 把一行拆成「正文」与「原行尾」。
//
// 保留原行尾：CRLF 文件 grep 之后不该被悄悄改成 LF（grep 之后常接重定向）。
func splitLineEnding(s string) (body, eol string) {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return s[:len(s)-2], "\r\n"
	case strings.HasSuffix(s, "\n"):
		return s[:len(s)-1], "\n"
	default:
		return s, ""
	}
}

func writeGrepLine(hc interp.HandlerContext, name string, showName, lineNo bool, n int, body, eol string, isCtx bool) {
	// 上下文行与命中行用不同分隔符（- / :），模型一眼能看出哪行是命中。
	sep := byte(':')
	if isCtx {
		sep = '-'
	}
	var b strings.Builder
	if showName {
		b.WriteString(name)
		b.WriteByte(sep)
	}
	if lineNo {
		b.WriteString(strconv.Itoa(n))
		b.WriteByte(sep)
	}
	b.WriteString(body)
	if eol == "" {
		eol = "\n" // 末行没有换行时补一个，与 grep 一致
	}
	b.WriteString(eol)
	_, _ = io.WriteString(hc.Stdout, b.String())
}

// grepMatcher 是编译后的匹配器：固定串走 strings.Index（快、无元字符坑），
// 其余走 RE2。
type grepMatcher struct {
	opts    grepOpts
	re      *regexp.Regexp
	needles []string // 仅 fixed 且非 -i 时使用
}

func newGrepMatcher(opts grepOpts, patterns []string) (*grepMatcher, error) {
	m := &grepMatcher{opts: opts}
	// -F 且不做大小写折叠时用纯字符串查找：既快，也避免 -i 下 ToLower
	// 改变字节长度（如 'İ'）导致区间错位。
	if opts.fixed && !opts.ignoreCase {
		m.needles = patterns
		return m, nil
	}
	parts := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if opts.fixed {
			p = regexp.QuoteMeta(p)
		}
		parts = append(parts, "(?:"+p+")")
	}
	expr := strings.Join(parts, "|")
	if opts.ignoreCase {
		expr = "(?i)" + expr
	}
	if opts.wholeLine {
		expr = "^(?:" + expr + ")$"
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	m.re = re
	return m, nil
}

// find 返回这一行里所有「可用」的匹配区间（已套用 -w / -x）。
//
// 空 pattern 匹配每一行（与 GNU 的 grep 空模式行为一致）；-o 会输出零长度匹配，
// 这里按「区间 [0,0)」表达，调用方写出去就是空行。
func (m *grepMatcher) find(line string) [][2]int {
	var out [][2]int
	if m.re != nil {
		for _, loc := range m.re.FindAllStringIndex(line, -1) {
			out = append(out, [2]int{loc[0], loc[1]})
		}
	} else {
		for _, n := range m.needles {
			if n == "" {
				out = append(out, [2]int{0, 0})
				continue
			}
			from := 0
			for from <= len(line) {
				i := strings.Index(line[from:], n)
				if i < 0 {
					break
				}
				start := from + i
				out = append(out, [2]int{start, start + len(n)})
				from = start + len(n)
			}
		}
		// 多个 pattern 时按出现位置排序，输出顺序才与「按行扫一遍」一致。
		sort.Slice(out, func(i, j int) bool {
			if out[i][0] != out[j][0] {
				return out[i][0] < out[j][0]
			}
			return out[i][1] < out[j][1]
		})
	}

	if m.opts.wholeLine {
		out = filterHits(out, func(h [2]int) bool { return h[0] == 0 && h[1] == len(line) })
	}
	if m.opts.word {
		out = filterHits(out, func(h [2]int) bool {
			return wordBoundaryBefore(line, h[0]) && wordBoundaryAfter(line, h[1])
		})
	}
	// 去掉重叠区间：(?-o 时) 同一处只报一次，与 GNU 的 -o 行为对齐。
	if m.opts.onlyMatch {
		filtered := out[:0]
		lastEnd := -1
		for _, h := range out {
			if h[0] < lastEnd {
				continue
			}
			filtered = append(filtered, h)
			lastEnd = h[1]
		}
		out = filtered
	}
	return out
}

func filterHits(in [][2]int, keep func([2]int) bool) [][2]int {
	out := in[:0]
	for _, h := range in {
		if keep(h) {
			out = append(out, h)
		}
	}
	return out
}

// isWordByte 与 grep -w 的判定一致：字母、数字、下划线。
// 非 ASCII 字节（中文 UTF-8）一律算边界，与「中文前后加空格才算词」一致。
func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func wordBoundaryBefore(s string, start int) bool {
	return start == 0 || !isWordByte(s[start-1])
}

func wordBoundaryAfter(s string, end int) bool {
	return end >= len(s) || !isWordByte(s[end])
}

// parseGrepArgs 解析参数，返回开关、pattern 列表与路径列表。
//
// 规则与 GNU 一致：给了 -e 时所有位置参数都是文件；没给时第一个位置参数是
// pattern。`--` 之后一律当文件（`grep -- -n` 搜 "-n"）。
func parseGrepArgs(hc interp.HandlerContext, args []string) (grepOpts, []string, []string, error) {
	var opts grepOpts
	var patterns, positional []string
	flagsDone := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case flagsDone || a == "-" || !strings.HasPrefix(a, "-"):
			positional = append(positional, a)

		case a == "--":
			flagsDone = true

		case a == "--ignore-case":
			opts.ignoreCase = true
		case a == "--invert-match":
			opts.invert = true
		case a == "--line-number":
			opts.lineNo = true
		case a == "--count":
			opts.count = true
		case a == "--files-with-matches":
			opts.listMatch = true
		case a == "--files-without-match":
			opts.listNo = true
		case a == "--only-matching":
			opts.onlyMatch = true
		case a == "--quiet" || a == "--silent":
			opts.quiet = true
		case a == "--word-regexp":
			opts.word = true
		case a == "--line-regexp":
			opts.wholeLine = true
		case a == "--fixed-strings":
			opts.fixed = true
		case a == "--recursive":
			opts.recursive = true
		case a == "--with-filename":
			opts.forceName = true
		case a == "--no-filename":
			opts.hideName = true

		case a == "-e" || a == "--regexp":
			if i+1 >= len(args) {
				return opts, nil, nil, usagef(hc, "grep: option requires an argument -- 'e'")
			}
			i++
			patterns = append(patterns, args[i])

		case a == "-m" || strings.HasPrefix(a, "--max-count"):
			// -m 仍然不做：它要「命中 N 次后停止扫描」，而我们的输出是按文件
			// 顺序流式的，提前停会让结果依赖文件顺序。
			return opts, nil, nil, usagef(hc,
				"%s: 内建 grep 不支持 -m；可以管道接 head 或者缩小范围", a)

		case a == "-A" || a == "-B" || a == "-C" ||
			strings.HasPrefix(a, "--after-context") || strings.HasPrefix(a, "--before-context") ||
			strings.HasPrefix(a, "--context"):
			var val string
			flag := a
			// 注意别用 i 做短变量：外层 for 的 i 是参数下标，遮蔽掉之后
			// 「取下一个参数」会取错位置。
			if eq := strings.IndexByte(a, '='); eq > 0 {
				flag, val = a[:eq], a[eq+1:]
			} else {
				if i+1 >= len(args) {
					return opts, nil, nil, usagef(hc, "grep: option requires an argument -- %q", a)
				}
				i++
				val = args[i]
			}
			cnt, cerr := strconv.Atoi(strings.TrimSpace(val))
			if cerr != nil || cnt < 0 {
				return opts, nil, nil, usagef(hc, "grep: invalid context length %q", val)
			}
			switch {
			case flag == "-A" || strings.HasPrefix(flag, "--after-context"):
				opts.after = cnt
			case flag == "-B" || strings.HasPrefix(flag, "--before-context"):
				opts.before = cnt
			default: // -C / --context：(cnt, cnt)
				opts.before, opts.after = cnt, cnt
			}

		default:
			// 组合短选项（-in、-rn、-iE…），遇到带值的 e 就地把剩余部分当 pattern。
			rest := a[1:]
			for j := 0; j < len(rest); j++ {
				switch c := rest[j]; c {
				case 'i':
					opts.ignoreCase = true
				case 'v':
					opts.invert = true
				case 'n':
					opts.lineNo = true
				case 'c':
					opts.count = true
				case 'l':
					opts.listMatch = true
				case 'L':
					opts.listNo = true
				case 'o':
					opts.onlyMatch = true
				case 'q':
					opts.quiet = true
				case 'w':
					opts.word = true
				case 'x':
					opts.wholeLine = true
				case 'F':
					opts.fixed = true
				case 'E', 'G', 'P':
					// ERE/基本/PCRE 都落到同一个 RE2 引擎上（见文件头说明）。
				case 'r', 'R':
					opts.recursive = true
				case 'H':
					opts.forceName = true
				case 'h':
					opts.hideName = true
				case 'A', 'B', 'C':
					// 组合形式里的带值选项：-A3 / -rC2。值必须是剩余部分或下一个参数。
					val := rest[j+1:]
					if val == "" {
						if i+1 >= len(args) {
							return opts, nil, nil, usagef(hc, "grep: option requires an argument -- %q", string(c))
						}
						i++
						val = args[i]
					}
					cnt, cerr := strconv.Atoi(val)
					if cerr != nil || cnt < 0 {
						return opts, nil, nil, usagef(hc, "grep: invalid context length %q", val)
					}
					switch c {
					case 'A':
						opts.after = cnt
					case 'B':
						opts.before = cnt
					default:
						opts.before, opts.after = cnt, cnt
					}
					j = len(rest)

				case 'e':
					if j+1 < len(rest) {
						patterns = append(patterns, rest[j+1:])
						j = len(rest)
						continue
					}
					if i+1 >= len(args) {
						return opts, nil, nil, usagef(hc, "grep: option requires an argument -- 'e'")
					}
					i++
					patterns = append(patterns, args[i])
				default:
					return opts, nil, nil, usagef(hc, "grep: invalid option -- %q", string(c))
				}
			}
		}
	}

	paths := positional
	if len(patterns) == 0 {
		if len(positional) == 0 {
			return opts, nil, nil, usagef(hc, "grep: missing pattern")
		}
		patterns = []string{positional[0]}
		paths = positional[1:]
	}
	return opts, patterns, paths, nil
}
