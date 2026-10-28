package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// globMaxCandidates 是一次遍历最多收集多少个匹配候选。
//
// 超过它就停止遍历并如实标注「结果可能不是最新的」—— 旧实现在 limit*4
// 处静默 SkipAll，结果是「遍历序前 N 个里最新的几个」冒充「全库最新」，
// 但工具描述承诺的是后者。宁可变慢一点，也不能静默骗调用方。
var globMaxCandidates = 20000

// globResult 是一次 glob 的结果。
type globResult struct {
	paths     []string
	truncated bool // 匹配数 > limit，只返回其中最新的 limit 个
	capped    bool // 候选超过 globMaxCandidates，结果可能不是全库最新的
}

// globHit 是一个匹配文件及其修改时间。
type globHit struct {
	path string
	mod  int64
}

// globTop 是「保留最新的 limit 个」的小顶堆（堆顶最旧）。
//
// 为什么要堆：按 mtime 取最新的 N 个，必须先拿全量候选的 mtime（stat），
// 内存上限 = O(limit)，而不是先收集再全量排序。
type globTop struct {
	limit int
	items []globHit
}

func (t *globTop) push(h globHit) {
	if t.limit <= 0 {
		return
	}
	if len(t.items) < t.limit {
		t.items = append(t.items, h)
		for i := len(t.items) - 1; i > 0; {
			parent := (i - 1) / 2
			if t.items[parent].mod <= t.items[i].mod {
				break
			}
			t.items[parent], t.items[i] = t.items[i], t.items[parent]
			i = parent
		}
		return
	}
	if h.mod <= t.items[0].mod {
		return
	}
	t.items[0] = h
	for i := 0; ; {
		left, right, smallest := 2*i+1, 2*i+2, i
		if left < len(t.items) && t.items[left].mod < t.items[smallest].mod {
			smallest = left
		}
		if right < len(t.items) && t.items[right].mod < t.items[smallest].mod {
			smallest = right
		}
		if smallest == i {
			return
		}
		t.items[smallest], t.items[i] = t.items[i], t.items[smallest]
		i = smallest
	}
}

// sorted 按 mtime 倒序输出；mtime 相同时按路径升序，保证结果可复现。
func (t *globTop) sorted() []string {
	if len(t.items) == 0 {
		return nil
	}
	items := append([]globHit(nil), t.items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].mod != items[j].mod {
			return items[i].mod > items[j].mod
		}
		return items[i].path < items[j].path
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.path
	}
	return out
}

// globMatcher 是预编译的匹配器。
//
// 旧实现每个文件要跑两次匹配（先按相对路径的正则，失败再 filepath.Match
// 兜底 basename），而后者每次调用都会重新解析 pattern。这里按 pattern 是否
// 含 "/" 分成两条路径，一次编译、循环里只调用一次：
//
//   - 含 "/"（如 src/**/*.go）：只比对相对路径。basename 里没有 /，兜底
//     匹配永远不成立，本来就是纯浪费。
//   - 不含 "/"（如 *_test.go）：只比对 basename。这与旧行为等价，也省掉
//     每文件的 filepath.Rel。
type globMatcher struct {
	re       *regexp.Regexp
	pathMode bool
}

func newGlobMatcher(pattern string, pathMode bool) (*globMatcher, error) {
	re, err := globToRegexp(filepath.ToSlash(pattern))
	if err != nil {
		return nil, err
	}
	return &globMatcher{re: re, pathMode: pathMode}, nil
}

// globFiles 按 glob 模式（支持 **）查找文件，按修改时间倒序返回。
func globFiles(ctx context.Context, root, pattern string, limit int) (globResult, error) {
	// 路径模式看「调用方原本写的 pattern」：`pkg/*.go` 被 splitStaticPrefix
	// 拆成 root=.../pkg、pattern=*.go 之后，语义仍然是「pkg 下这一层」，
	// 不能因为剩下 *.go 就降级成 basename 匹配（那会把 pkg/sub/b.go 也算进来）。
	pathMode := strings.Contains(filepath.ToSlash(pattern), "/")

	// 若 pattern 中的固定前缀是个已存在的目录，则把它作为新的根，减少遍历。
	root, pattern = splitStaticPrefix(root, pattern)

	info, err := os.Stat(root)
	if err != nil {
		return globResult{}, err
	}
	m, err := newGlobMatcher(pattern, pathMode)
	if err != nil {
		return globResult{}, err
	}
	if !info.IsDir() {
		if m.match(filepath.Base(root), filepath.Base(root)) {
			return globResult{paths: []string{root}}, nil
		}
		return globResult{}, nil
	}

	top := &globTop{limit: limit}
	candidates := 0
	capped := false

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if m.pathMode {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return nil
			}
			if !m.match(filepath.ToSlash(rel), base) {
				return nil
			}
		} else if !m.match(base, base) {
			return nil
		}
		candidates++
		if candidates > globMaxCandidates {
			capped = true
			return filepath.SkipAll
		}
		var mod int64
		if fi, ierr := d.Info(); ierr == nil {
			mod = fi.ModTime().UnixNano()
		}
		top.push(globHit{path: p, mod: mod})
		return nil
	})
	if walkErr != nil {
		return globResult{}, walkErr
	}
	return globResult{paths: top.sorted(), truncated: candidates > limit, capped: capped}, nil
}

func (m *globMatcher) match(rel, base string) bool {
	if m.pathMode {
		return m.re.MatchString(rel)
	}
	return m.re.MatchString(base)
}

// splitStaticPrefix 把 "src/**/*.go" 拆成 root=.../src, pattern=**/*.go。
func splitStaticPrefix(root, pattern string) (string, string) {
	pattern = filepath.ToSlash(pattern)
	if strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
		return root, pattern
	}
	parts := strings.Split(pattern, "/")
	i := 0
	cur := root
	for i < len(parts)-1 {
		seg := parts[i]
		if strings.ContainsAny(seg, "*?[{") {
			break
		}
		next := filepath.Join(cur, seg)
		st, err := os.Stat(next)
		if err != nil || !st.IsDir() {
			break
		}
		cur = next
		i++
	}
	if i == 0 {
		return root, pattern
	}
	return cur, strings.Join(parts[i:], "/")
}

// matchDoubleStar 用正则实现 glob 匹配（支持 ** / * / ? / [...]）。
func matchDoubleStar(pattern, name string) (bool, error) {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(filepath.ToSlash(name)), nil
}

var globCache sync.Map // pattern -> *regexp.Regexp

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = filepath.ToSlash(pattern)
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// ** 后紧跟 / 时匹配零或多层目录
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
					continue
				}
				b.WriteString(".*")
				i += 2
				continue
			}
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			j := i + 1
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j >= len(pattern) {
				b.WriteString(regexp.QuoteMeta("["))
				i++
				continue
			}
			cls := pattern[i : j+1]
			cls = strings.ReplaceAll(cls, "[!", "[^")
			b.WriteString(cls)
			i = j + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	globCache.Store(pattern, re)
	return re, nil
}
