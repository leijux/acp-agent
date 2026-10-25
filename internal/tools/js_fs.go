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

	"github.com/dop251/goja"
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

// installFS 注入全局 fs 对象（只读）。
func installFS(vm *goja.Runtime, p Policy) error {
	if p.Root == "" {
		p = DefaultPolicy()
	}
	fs := &jsFS{policy: p}
	obj := vm.NewObject()

	methods := map[string]func(goja.FunctionCall) goja.Value{
		"readFile":       fs.readFile(vm),
		"readFileBytes":  fs.readFileBytes(vm),
		"readFileBase64": fs.readFileBase64(vm),
		"readDir":        fs.readDir(vm),
		"stat":           fs.stat(vm),
		"exists":         fs.exists(vm),
		"glob":           fs.glob(vm),
	}
	names := make([]string, 0, len(methods))
	for name := range methods {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := obj.Set(name, methods[name]); err != nil {
			return err
		}
	}
	// fs.root 让脚本能自己拼绝对路径（日志/输出里也更可读）。
	if err := obj.Set("root", p.Root); err != nil {
		return err
	}
	return vm.Set("fs", obj)
}

// resolve 解析路径并做越界检查；失败时直接抛 JS 异常（脚本可以 try/catch）。
func (fs *jsFS) resolve(vm *goja.Runtime, path string) string {
	abs, err := fs.policy.Resolve(path)
	if err != nil {
		throwJS(vm, err.Error())
	}
	return abs
}

// charge 记账并检查本次执行的累计预算。
func (fs *jsFS) charge(vm *goja.Runtime, n int64) {
	fs.files++
	fs.bytes += n
	if fs.files > jsFSMaxFiles || fs.bytes > jsFSMaxBytes {
		throwJS(vm, fmt.Sprintf(
			"本次脚本读取超过预算（%d 个文件 / %d 字节），请缩小范围或分步执行",
			jsFSMaxFiles, jsFSMaxBytes))
	}
}

func (fs *jsFS) readRaw(vm *goja.Runtime, op, path string) []byte {
	abs := fs.resolve(vm, path)
	info, err := os.Stat(abs)
	if err != nil {
		throwJS(vm, fmt.Sprintf("%s: %s: %s", op, path, errText(err)))
	}
	if info.IsDir() {
		throwJS(vm, fmt.Sprintf("%s: %s: 是目录（用 readDir）", op, path))
	}
	limit := fs.policy.MaxReadBytes
	if limit <= 0 {
		limit = jsFSDefaultReadLimit
	}
	if info.Size() > limit {
		throwJS(vm, fmt.Sprintf(
			"%s: %s 大小 %d 字节，超过上限 %d（大文件请先用 read 工具分页）",
			op, path, info.Size(), limit))
	}
	fs.charge(vm, info.Size())
	data, err := os.ReadFile(abs)
	if err != nil {
		throwJS(vm, fmt.Sprintf("%s: %s: %s", op, path, errText(err)))
	}
	return data
}

func (fs *jsFS) readFile(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(string(fs.readRaw(vm, "readFile", jsArg(call, 0))))
	}
}

// readFileBytes 返回 ArrayBuffer：脚本用 `new Uint8Array(b)` 就能按字节处理。
//
// ArrayBuffer 而不是 Uint8Array：交给 JS 自己选视图（Uint8Array/DataView），
// 也方便直接喂给 crypto.sha256。
func (fs *jsFS) readFileBytes(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		data := fs.readRaw(vm, "readFileBytes", jsArg(call, 0))
		return vm.ToValue(vm.NewArrayBuffer(data))
	}
}

// readFileBase64 返回 base64 字符串：只想知道大小/校验值、或要把内容嵌进文本时用。
func (fs *jsFS) readFileBase64(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		data := fs.readRaw(vm, "readFileBase64", jsArg(call, 0))
		return vm.ToValue(base64.StdEncoding.EncodeToString(data))
	}
}

func (fs *jsFS) readDir(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		path := jsArg(call, 0)
		if strings.TrimSpace(path) == "" {
			path = "."
		}
		abs := fs.resolve(vm, path)
		entries, err := os.ReadDir(abs)
		if err != nil {
			throwJS(vm, fmt.Sprintf("readDir: %s: %s", path, errText(err)))
		}
		fs.charge(vm, 0)
		out := make([]any, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/" // 和 ls 工具保持一致，目录带斜杠
			}
			out = append(out, name)
		}
		return vm.ToValue(out)
	}
}

func (fs *jsFS) stat(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		path := jsArg(call, 0)
		abs := fs.resolve(vm, path)
		info, err := os.Stat(abs)
		if err != nil {
			throwJS(vm, fmt.Sprintf("stat: %s: %s", path, errText(err)))
		}
		return vm.ToValue(map[string]any{
			"path":    fs.policy.Rel(abs),
			"size":    info.Size(),
			"isDir":   info.IsDir(),
			"mode":    info.Mode().String(),
			"mtimeMs": info.ModTime().UnixMilli(),
			"mtime":   info.ModTime().Format(time.RFC3339),
		})
	}
}

func (fs *jsFS) exists(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		path := jsArg(call, 0)
		abs, err := fs.policy.Resolve(path)
		if err != nil {
			return vm.ToValue(false) // 越界也算「不存在」，不抛错
		}
		_, statErr := os.Stat(abs)
		return vm.ToValue(statErr == nil)
	}
}

func (fs *jsFS) glob(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		pattern := jsArg(call, 0)
		if pattern == "" {
			throwJS(vm, "glob: 缺少 pattern")
		}
		if filepath.IsAbs(pattern) {
			throwJS(vm, fmt.Sprintf("glob: pattern %q 不能是绝对路径（相对 fs.root）", pattern))
		}
		// 先挡住明显的越界；符号链接逃逸由下面的 Resolve 兜住。
		pat := filepath.ToSlash(pattern)
		if strings.Contains(pat, "..") {
			throwJS(vm, fmt.Sprintf("glob: pattern %q 不能包含 ..", pattern))
		}
		matches, err := fs.globMatches(pat)
		if err != nil {
			throwJS(vm, fmt.Sprintf("glob: %s", err))
		}
		sort.Strings(matches)
		out := make([]any, 0, len(matches))
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
		fs.charge(vm, 0)
		return vm.ToValue(out)
	}
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
func jsArg(call goja.FunctionCall, i int) string {
	if i >= len(call.Arguments) {
		return ""
	}
	v := call.Arguments[i]
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	return v.String()
}

// throwJS 把消息作为 JS 异常抛出（脚本可以 try/catch）。
//
// 用字符串而不是 Error 对象：模型看到的输出更短，`catch (e) { e }
// 直接就是一句人话。
func throwJS(vm *goja.Runtime, msg string) {
	panic(vm.ToValue(msg))
}
