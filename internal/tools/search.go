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

// globFiles 按 glob 模式（支持 **）查找文件，按修改时间倒序返回。
func globFiles(ctx context.Context, root, pattern string, limit int) ([]string, bool, error) {
	// 若 pattern 中的固定前缀是个已存在的目录，则把它作为新的根，减少遍历。
	root, pattern = splitStaticPrefix(root, pattern)

	info, err := os.Stat(root)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		if ok, _ := matchDoubleStar(pattern, filepath.Base(root)); ok {
			return []string{root}, false, nil
		}
		return nil, false, nil
	}

	type fileInfo struct {
		path string
		mod  int64
	}
	var found []fileInfo
	truncated := false

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		ok, _ := matchDoubleStar(pattern, rel)
		if !ok {
			// 也允许只匹配 basename（例如 *.go）
			if ok2, _ := filepath.Match(pattern, d.Name()); !ok2 {
				return nil
			}
		}
		mod := int64(0)
		if fi, ierr := d.Info(); ierr == nil {
			mod = fi.ModTime().UnixNano()
		}
		found = append(found, fileInfo{path: p, mod: mod})
		if len(found) > limit*4 { // 提前截流，避免超大仓库全量排序
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	sort.Slice(found, func(i, j int) bool { return found[i].mod > found[j].mod })
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.path)
	}
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated, nil
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
