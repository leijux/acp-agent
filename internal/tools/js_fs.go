package tools

// js 的只读文件系统桥。
//
// 设计取向：给 js 工具补的是「读」，不是「写」。
//   - 读：脚本要扫描/统计/正则过滤文件时，没必要起进程，也不该逼调用方
//     先把几十个文件 read 进来再塞进 args；
//   - 写：仍然只走 write/edit 工具 —— 它们有权限门禁、有 diff、有审计。
//     沙箱里不出现写接口，就不会有「脚本悄悄改了文件」这种事。
//
// 所有路径都过 Policy.Resolve：越界（含符号链接指向 root 外）直接抛错。
// 另外每次执行有总量预算，避免一个脚本把整个磁盘读进内存。

import (
	"encoding/base64"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
)

const (
	// jsFSDefaultReadLimit 是单文件读取上限（Policy.MaxReadBytes 优先）。
	jsFSDefaultReadLimit = 2 << 20
	// jsFSMaxFiles / jsFSMaxBytes 是「一次脚本」的累计预算。
	jsFSMaxFiles = 512
	jsFSMaxBytes = 16 << 20
	// jsFSMaxGlob 限制 glob 返回条数。
	jsFSMaxGlob = 2000
)

// jsFS 是一次脚本执行的文件系统上下文。
type jsFS struct {
	policy Policy
	files  int
	bytes  int64
}

// newJSFS 按 Policy 建一次脚本的文件系统上下文。
func newJSFS(p Policy) *jsFS {
	if p.Root == "" {
		p = DefaultPolicy()
	}
	return &jsFS{policy: p}
}

// installFS 注入全局 fs 对象（只读）。
func installFS(rt *moejs.Runtime, p Policy) error {
	return rt.SetGlobal("fs", newJSFS(p).methods(rt))
}

// methods 构造 fs 对象的方法表。
//
// 单独抽出来是给内建命令 `moejs` 用的：它要在同一个对象上多挂一个
// readStdin()，而 moejs 没有「建好对象后再加一个属性」的顶层 API。
func (fs *jsFS) methods(rt *moejs.Runtime) map[string]any {
	return map[string]any{
		"root":           fs.policy.Root,
		"readFile":       fs.handler(rt, "readFile"),
		"readFileBytes":  fs.handler(rt, "readFileBytes"),
		"readFileBase64": fs.handler(rt, "readFileBase64"),
		"readDir":        fs.handler(rt, "readDir"),
		"stat":           fs.handler(rt, "stat"),
		"exists":         fs.handler(rt, "exists"),
		"glob":           fs.handler(rt, "glob"),
		"writeFile":      fs.handler(rt, "writeFile"),
	}
}

// handler 按名字分派一个 fs 方法。
//
// 用一张 switch 而不是每个方法各自建闭包：错误现在走返回值（moejs 的宿主
// 函数返回 error 就是抛异常），返回值则要经过 FromGo/ParseJSON 转换，集中
// 在一处好核对。
func (fs *jsFS) handler(rt *moejs.Runtime, name string) moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		switch name {
		case "readFile":
			data, err := fs.readRaw(name, jsArg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(string(data)), nil

		case "readFileBytes":
			// ArrayBuffer 而不是 Uint8Array：交给 JS 自己选视图
			// （Uint8Array/DataView），也方便直接喂给 crypto.sha256。
			data, err := fs.readRaw(name, jsArg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return rt.FromGo(data)

		case "readFileBase64":
			data, err := fs.readRaw(name, jsArg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(base64.StdEncoding.EncodeToString(data)), nil

		case "writeFile":
			return fs.writeFile(jsArg(args, 0), jsArg(args, 1))

		case "readDir":
			path := jsArg(args, 0)
			if strings.TrimSpace(path) == "" {
				path = "."
			}
			abs, err := fs.resolve(path)
			if err != nil {
				return moejs.Undefined(), err
			}
			entries, err := os.ReadDir(abs)
			if err != nil {
				return moejs.Undefined(), throwErr("readDir: %s: %s", path, errText(err))
			}
			if err := fs.charge(0); err != nil {
				return moejs.Undefined(), err
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				item := e.Name()
				if e.IsDir() {
					item += "/" // 和 ls 工具保持一致，目录带斜杠
				}
				out = append(out, item)
			}
			return rt.FromGo(out)

		case "stat":
			path := jsArg(args, 0)
			abs, err := fs.resolve(path)
			if err != nil {
				return moejs.Undefined(), err
			}
			info, err := os.Stat(abs)
			if err != nil {
				return moejs.Undefined(), throwErr("stat: %s: %s", path, errText(err))
			}
			return rt.FromGo(map[string]any{
				"path":    fs.policy.Rel(abs),
				"size":    info.Size(),
				"isDir":   info.IsDir(),
				"mode":    info.Mode().String(),
				"mtimeMs": info.ModTime().UnixMilli(),
				"mtime":   info.ModTime().Format(time.RFC3339),
			})

		case "exists":
			abs, err := fs.policy.Resolve(jsArg(args, 0))
			if err != nil {
				return moejs.Bool(false), nil // 越界也算「不存在」，不抛错
			}
			_, statErr := os.Stat(abs)
			return moejs.Bool(statErr == nil), nil

		case "glob":
			return fs.glob(rt, jsArg(args, 0))
		}
		return moejs.Undefined(), throwErr("fs: 未知方法 %q", name)
	}
}

// writeFile 写一个文件（fs.writeFile(path, content) → 写入字节数）。
//
// 只在 Policy.AllowScriptWrite 打开时可用，且路径同样受 Root 约束；写盘复用
// write 工具的 LockFile，所以与并发的 write/edit 不会互相踩。
func (fs *jsFS) writeFile(path, content string) (moejs.Value, error) {
	if fs.policy.ReadOnly {
		return moejs.Undefined(), throwErr("当前策略为只读，禁止写文件")
	}
	if !fs.policy.AllowScriptWrite {
		return moejs.Undefined(), throwErr("fs.writeFile 需要 permissions=auto；在 ask 模式下请用 write/edit 工具" +
			"（那条路有确认与 diff 审计）")
	}
	abs, err := fs.resolve(path)
	if err != nil {
		return moejs.Undefined(), err
	}
	unlock := LockFile(abs)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return moejs.Undefined(), throwErr("创建目录失败: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return moejs.Undefined(), throwErr("写入失败: %v", err)
	}
	return moejs.Int(int64(len(content))), nil
}

func (fs *jsFS) glob(rt *moejs.Runtime, pattern string) (moejs.Value, error) {
	if pattern == "" {
		return moejs.Undefined(), throwErr("glob: 缺少 pattern")
	}
	if filepath.IsAbs(pattern) {
		return moejs.Undefined(), throwErr("glob: pattern %q 不能是绝对路径（相对 fs.root）", pattern)
	}
	// 先挡住明显的越界；符号链接逃逸由下面的 Resolve 兜住。
	pat := filepath.ToSlash(pattern)
	if strings.Contains(pat, "..") {
		return moejs.Undefined(), throwErr("glob: pattern %q 不能包含 ..", pattern)
	}
	matches, err := fs.globMatches(pat)
	if err != nil {
		return moejs.Undefined(), throwErr("glob: %s", err)
	}
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(out) >= jsFSMaxGlob {
			break
		}
		// 校验符号链接没有指到 root 外；越界的静默跳过。
		if _, err := fs.policy.Resolve(m); err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(fs.policy.Rel(m)))
	}
	if err := fs.charge(0); err != nil {
		return moejs.Undefined(), err
	}
	return rt.FromGo(out)
}

// resolve 解析路径并做越界检查；失败时作为 JS 异常抛出（脚本可以 try/catch）。
func (fs *jsFS) resolve(path string) (string, error) {
	abs, err := fs.policy.Resolve(path)
	if err != nil {
		return "", &moejs.Exception{Value: moejs.String(err.Error())}
	}
	return abs, nil
}

// charge 记账并检查本次执行的累计预算。
func (fs *jsFS) charge(n int64) error {
	fs.files++
	fs.bytes += n
	if fs.files > jsFSMaxFiles || fs.bytes > jsFSMaxBytes {
		return throwErr(
			"本次脚本读取超过预算（%d 个文件 / %d 字节），请缩小范围或分步执行",
			jsFSMaxFiles, jsFSMaxBytes)
	}
	return nil
}

func (fs *jsFS) readRaw(op, path string) ([]byte, error) {
	abs, err := fs.resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, throwErr("%s: %s: %s", op, path, errText(err))
	}
	if info.IsDir() {
		return nil, throwErr("%s: %s: 是目录（用 readDir）", op, path)
	}
	limit := fs.policy.MaxReadBytes
	if limit <= 0 {
		limit = jsFSDefaultReadLimit
	}
	if info.Size() > limit {
		return nil, throwErr(
			"%s: %s 大小 %d 字节，超过上限 %d（大文件请先用 read 工具分页）",
			op, path, info.Size(), limit)
	}
	if err := fs.charge(info.Size()); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, throwErr("%s: %s: %s", op, path, errText(err))
	}
	return data, nil
}

// globMatches 展开 pattern，支持 `**`（匹配任意层目录）。
//
// filepath.Glob 不认识 `**`，只按一层目录处理；模型写 `src/**/*.go` 是常态，
// 所以这里自己走一遍 WalkDir + 正则匹配。
func (fs *jsFS) globMatches(pattern string) ([]string, error) {
	root := fs.policy.Root
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	var out []string
	walkErr := filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil // 无权限/竞态删除：跳过这个子树
		}
		if d.IsDir() {
			return nil // 目录本身不进结果（与 shell glob 的常见用法一致）
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if re.MatchString(filepath.ToSlash(rel)) {
			out = append(out, p)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// jsArg 取第 i 个字符串参数（缺失或非字符串时返回空串）。
func jsArg(args []moejs.Value, i int) string {
	if i >= len(args) || args[i].IsNullish() {
		return ""
	}
	return args[i].String()
}

// throwErr 构造一个「抛字符串」的异常，返回给 moejs 的宿主函数。
//
// 抛字符串而不是 Error 对象：模型看到的输出更短，`catch (e) { e }` 直接就是
// 一句人话。moejs 的宿主函数返回 *Exception 时按原样抛出它的 Value。
func throwErr(format string, a ...any) error {
	msg := format
	if len(a) > 0 {
		msg = fmt.Sprintf(format, a...)
	}
	return &moejs.Exception{Value: moejs.String(msg)}
}
