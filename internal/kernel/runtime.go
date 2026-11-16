package kernel

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"path"
	"strings"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// runtime 是一个 moejs runtime + 已加载的 kernel bundle。
// 非并发安全，由 Kernel.mu 串行化。
type runtime struct {
	rt      *moejs.Runtime
	exports moejs.Value
	core    moejs.Value
	opts    Options
}

// newRuntime 在一个新 runtime 上执行（已预编译的）bundle wrapper。
// script 来自 Kernel.program()，可被任意多个 runtime 复用。
func newRuntime(script *moejs.Script, opts Options) (*runtime, error) {
	mopts := moejs.Options{}
	// 只有宿主显式给了分词器才需要可变内建：换掉 Intl.Segmenter 是写内建对象，
	// 而 moejs 默认把内建（含 Intl）做成进程共享 + 冻结，写它会抛 TypeError。
	// 默认路径保持共享内建（更省内存、启动更快），直接用它自带的 Segmenter。
	if opts.Segmenter != nil {
		mopts.MutableIntrinsics = true
	}
	rt := moejs.NewRuntime(mopts)

	r := &runtime{rt: rt, opts: opts}
	if err := r.injectConsole(); err != nil {
		return nil, err
	}
	if err := r.injectIntl(); err != nil {
		return nil, err
	}
	require, err := r.newRequire()
	if err != nil {
		return nil, err
	}

	module, err := r.newObject()
	if err != nil {
		return nil, err
	}
	exports, err := r.newObject()
	if err != nil {
		return nil, err
	}
	if err := r.setProp(module, "exports", exports); err != nil {
		return nil, err
	}

	fnVal, err := rt.RunScript(script)
	if err != nil {
		return nil, fmt.Errorf("kernel: load bundle: %w", wrapJS(err))
	}
	if _, err := r.callFn(fnVal, module, exports, require); err != nil {
		return nil, fmt.Errorf("kernel: init module: %w", err)
	}

	exp := r.prop(module, "exports")
	if !exp.IsObject() {
		return nil, errors.New("kernel: module.exports is not an object")
	}
	r.exports = exp

	createCore := r.prop(exp, "createCore")
	ports := moejs.Undefined()
	if opts.CountTokens != nil {
		count := opts.CountTokens
		ports = r.val(map[string]any{
			"countTokens": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
				return moejs.Int(int64(count(jsString(args, 0)))), nil
			}),
		})
	}
	core, err := r.callFn(createCore, ports)
	if err != nil {
		return nil, fmt.Errorf("kernel: createCore: %w", err)
	}
	r.core = core
	return r, nil
}

// ---------------------------------------------------------------------------
// 宿主 API 小工具
// ---------------------------------------------------------------------------

// val 把 Go 值转成 JS 值。转换失败退回 undefined：
// 这些值都是 JSON 形态的简单数据，转不动说明宿主传了不支持的类型；
// kernel 处在压缩链路上，不该为此让整个会话失败。
func (r *runtime) val(x any) moejs.Value {
	v, err := r.rt.FromGo(x)
	if err != nil {
		return moejs.Undefined()
	}
	return v
}

// newObject 建一个普通 JS 对象（带 Object.prototype，可写、可扩展）。
func (r *runtime) newObject() (moejs.Value, error) {
	return r.rt.ParseJSONString("{}")
}

// prop 读对象属性（不是对象或读不到时返回 undefined）。
func (r *runtime) prop(v moejs.Value, key string) moejs.Value {
	if !v.IsObject() {
		return moejs.Undefined()
	}
	x, err := r.rt.Get(v, key)
	if err != nil {
		return moejs.Undefined()
	}
	return x
}

// setProp 给对象写一个属性。
//
// moejs 的顶层 API 只提供「读属性」和「装全局」，写对象属性要下到 engine：
// InternKey + Object.SetProp。用它而不是 FromGo(map) 的原因是后者是**惰性**
// Go 容器视图，而且没法表达 `mod.default = mod` 这种自引用。
func (r *runtime) setProp(obj moejs.Value, key string, v moejs.Value) error {
	if !obj.IsObject() {
		return fmt.Errorf("kernel: cannot set property %q on a non-object", key)
	}
	return obj.AsObject().SetProp(r.rt.Realm(), engine.InternKey(key), v)
}

// callFn 调用一个 JS 函数值。
func (r *runtime) callFn(fn moejs.Value, args ...moejs.Value) (moejs.Value, error) {
	if !fn.IsObject() {
		return moejs.Undefined(), errors.New("kernel: value is not a function")
	}
	v, err := r.rt.CallFunction(fn, moejs.Undefined(), args...)
	if err != nil {
		return moejs.Undefined(), wrapJS(err)
	}
	return v, nil
}

// module 用一张方法表造一个对象，并把它设成自己的 default
// （CJS/ESM 互操作：esbuild 产出的 __toESM 会读 .default）。
func (r *runtime) module(fields map[string]any) (moejs.Value, error) {
	obj, err := r.newObject()
	if err != nil {
		return moejs.Undefined(), err
	}
	for k, v := range fields {
		if err := r.setProp(obj, k, r.val(v)); err != nil {
			return moejs.Undefined(), err
		}
	}
	if err := r.setProp(obj, "default", obj); err != nil {
		return moejs.Undefined(), err
	}
	return obj, nil
}

func jsString(args []moejs.Value, i int) string {
	if i >= len(args) || args[i].IsNullish() {
		return ""
	}
	return args[i].String()
}

func (r *runtime) injectConsole() error {
	log := r.opts.Logger
	if log == nil {
		return nil
	}
	mk := func(level string) moejs.NativeFunc {
		return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			parts := make([]string, 0, len(args))
			for _, a := range args {
				parts = append(parts, a.String())
			}
			log(level, strings.Join(parts, " "))
			return moejs.Undefined(), nil
		}
	}
	return r.rt.SetGlobal("console", map[string]any{
		"log":   mk("info"),
		"info":  mk("info"),
		"warn":  mk("warn"),
		"error": mk("error"),
	})
}

// newRequire 提供 kernel 需要的宿主模块：node:crypto / node:path / node:fs / node:module。
func (r *runtime) newRequire() (moejs.Value, error) {
	// --- path（POSIX 语义，与 Node 在 Linux 上一致）---
	joinFn := moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			if s := a.String(); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return moejs.String("."), nil
		}
		return moejs.String(path.Join(parts...)), nil
	})
	pathMod, err := r.module(map[string]any{
		"join":       joinFn,
		"resolve":    joinFn,
		"normalize":  unaryPath(path.Clean),
		"dirname":    unaryPath(path.Dir),
		"basename":   unaryPath(path.Base),
		"extname":    unaryPath(path.Ext),
		"isAbsolute": boolPath(path.IsAbs),
		"sep":        "/",
		"delimiter":  ":",
	})
	if err != nil {
		return moejs.Undefined(), err
	}

	// --- crypto ---
	cryptoMod, err := r.module(map[string]any{
		"createHash": r.nativeCreateHash(),
		"createHmac": r.nativeCreateHmac(),
		"randomUUID": r.nativeRandomUUID(),
	})
	if err != nil {
		return moejs.Undefined(), err
	}

	// --- fs（可选，映射到 io/fs.FS；packs.ts 的读取失败会被上游 try/catch 吞掉）---
	fsFields := map[string]any{
		"readFileSync": r.nativeReadFileSync(),
		"readdirSync":  r.nativeReaddirSync(),
		"existsSync":   r.nativeExistsSync(),
	}
	for _, name := range []string{"writeFileSync", "mkdirSync", "rmSync", "statSync", "openSync", "readSync", "closeSync"} {
		fsFields[name] = notAvailable(name)
	}
	promises, err := r.module(map[string]any{
		"readFile":  notAvailable("promises.readFile"),
		"writeFile": notAvailable("promises.writeFile"),
		"mkdir":     notAvailable("promises.mkdir"),
	})
	if err != nil {
		return moejs.Undefined(), err
	}
	fsFields["promises"] = promises
	fsMod, err := r.module(fsFields)
	if err != nil {
		return moejs.Undefined(), err
	}

	// --- module ---
	requireFn := moejs.Undefined()
	moduleMod, err := r.module(map[string]any{
		"createRequire": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			return requireFn, nil
		}),
	})
	if err != nil {
		return moejs.Undefined(), err
	}

	cache := map[string]moejs.Value{
		"node:path":        pathMod,
		"path":             pathMod,
		"path/posix":       pathMod,
		"node:crypto":      cryptoMod,
		"crypto":           cryptoMod,
		"node:fs":          fsMod,
		"fs":               fsMod,
		"node:fs/promises": promises,
		"fs/promises":      promises,
		"node:module":      moduleMod,
		"module":           moduleMod,
	}

	requireFn = r.val(moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		name := jsString(args, 0)
		if v, ok := cache[name]; ok {
			return v, nil
		}
		return moejs.Undefined(), fmt.Errorf("kernel: cannot find module '%s'", name)
	}))
	return requireFn, nil
}

func notAvailable(op string) moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), fmt.Errorf("kernel: fs.%s unavailable: no FS configured (kernel.WithFS)", op)
	}
}

// unaryPath 把一个 path 函数包装成单参数的宿主函数。
func unaryPath(fn func(string) string) moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return moejs.String(fn(jsString(args, 0))), nil
	}
}

func boolPath(fn func(string) bool) moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return moejs.Bool(fn(jsString(args, 0))), nil
	}
}

// hashObj 造一个 {update, digest} 对象（createHash/createHmac 的返回值）。
func (r *runtime) hashObj(write func([]byte), sum func() []byte) (moejs.Value, error) {
	obj, err := r.newObject()
	if err != nil {
		return moejs.Undefined(), err
	}
	if err := r.setProp(obj, "update", r.val(moejs.NativeFunc(
		func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			write([]byte(jsString(args, 0)))
			return obj, nil
		}))); err != nil {
		return moejs.Undefined(), err
	}
	if err := r.setProp(obj, "digest", r.val(moejs.NativeFunc(
		func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			raw := sum()
			switch jsString(args, 0) {
			case "base64":
				return moejs.String(base64.StdEncoding.EncodeToString(raw)), nil
			case "base64url":
				return moejs.String(base64.RawURLEncoding.EncodeToString(raw)), nil
			default:
				return moejs.String(hex.EncodeToString(raw)), nil
			}
		}))); err != nil {
		return moejs.Undefined(), err
	}
	return obj, nil
}

func (r *runtime) nativeCreateHash() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		alg := strings.ToLower(jsString(args, 0))
		var h hash.Hash
		switch alg {
		case "md5":
			h = md5.New()
		case "sha1":
			h = sha1.New()
		case "sha256":
			h = sha256.New()
		case "sha512":
			h = sha512.New()
		default:
			return moejs.Undefined(), fmt.Errorf("kernel: unsupported hash algorithm: %s", alg)
		}
		return r.hashObj(func(b []byte) { h.Write(b) }, func() []byte { return h.Sum(nil) })
	}
}

func (r *runtime) nativeCreateHmac() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		alg := strings.ToLower(jsString(args, 0))
		key := []byte(jsString(args, 1))
		var newHash func() hash.Hash
		switch alg {
		case "sha1":
			newHash = sha1.New
		case "sha256":
			newHash = sha256.New
		case "sha512":
			newHash = sha512.New
		default:
			return moejs.Undefined(), fmt.Errorf("kernel: unsupported hmac algorithm: %s", alg)
		}
		mac := hmac.New(newHash, key)
		return r.hashObj(func(b []byte) { mac.Write(b) }, func() []byte { return mac.Sum(nil) })
	}
}

func (r *runtime) nativeRandomUUID() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return moejs.Undefined(), fmt.Errorf("kernel: randomUUID: %w", err)
		}
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return moejs.String(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])), nil
	}
}

func (r *runtime) nativeReadFileSync() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if r.opts.FS == nil {
			return moejs.Undefined(), errors.New("kernel: fs.readFileSync unavailable: no FS configured (kernel.WithFS)")
		}
		name := cleanFSPath(jsString(args, 0))
		data, err := fs.ReadFile(r.opts.FS, name)
		if err != nil {
			return moejs.Undefined(), fmt.Errorf("kernel: fs.readFileSync %s: %w", name, err)
		}
		return moejs.String(string(data)), nil
	}
}

func (r *runtime) nativeReaddirSync() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if r.opts.FS == nil {
			return moejs.Undefined(), errors.New("kernel: fs.readdirSync unavailable: no FS configured (kernel.WithFS)")
		}
		name := cleanFSPath(jsString(args, 0))
		entries, err := fs.ReadDir(r.opts.FS, name)
		if err != nil {
			return moejs.Undefined(), fmt.Errorf("kernel: fs.readdirSync %s: %w", name, err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return r.val(names), nil
	}
}

func (r *runtime) nativeExistsSync() moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if r.opts.FS == nil {
			return moejs.Bool(false), nil
		}
		_, err := fs.Stat(r.opts.FS, cleanFSPath(jsString(args, 0)))
		return moejs.Bool(err == nil), nil
	}
}

// cleanFSPath 把 Node 风格路径转成 io/fs 路径（去掉前导 /、统一分隔符）。
func cleanFSPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "/")
	if p == "." || p == "" {
		return "."
	}
	return p
}

func (r *runtime) call(fnName string, args ...moejs.Value) (moejs.Value, error) {
	fn := r.prop(r.exports, fnName)
	if !fn.IsObject() {
		return moejs.Undefined(), fmt.Errorf("kernel: %s is not exported", fnName)
	}
	return r.callFn(fn, args...)
}

func (r *runtime) coreCall(method string, args ...moejs.Value) (moejs.Value, error) {
	fn := r.prop(r.core, method)
	if !fn.IsObject() {
		return moejs.Undefined(), fmt.Errorf("kernel: core.%s is not a function", method)
	}
	return r.callFn(fn, args...)
}

// toJSON 把 JS 值序列化成原始 JSON。
func (r *runtime) toJSON(v moejs.Value) ([]byte, error) {
	if v.IsNullish() {
		return nil, nil
	}
	out, err := r.rt.AppendJSON(nil, v)
	if err != nil {
		return nil, wrapJS(err)
	}
	if len(out) == 0 || string(out) == "undefined" {
		return nil, nil
	}
	return out, nil
}

// parse 把一个 JSON 文本变成 JS 值（失败时返回 undefined）。
func (r *runtime) parse(data []byte) moejs.Value {
	v, err := r.rt.ParseJSON(data)
	if err != nil {
		return moejs.Undefined()
	}
	return v
}

func (r *runtime) fromJSON(data []byte) (moejs.Value, error) {
	v, err := r.rt.ParseJSON(data)
	if err != nil {
		return moejs.Undefined(), wrapJS(err)
	}
	return v, nil
}

// evalString 在一个临时脚本里执行代码（测试与诊断用）。
func (r *runtime) evalString(src string) (moejs.Value, error) {
	s, err := moejs.CompileScript("acp-kernel.eval.js", src)
	if err != nil {
		return moejs.Undefined(), err
	}
	return r.rt.RunScript(s)
}

func wrapJS(err error) error {
	if err == nil {
		return nil
	}
	var ex *moejs.Exception
	if errors.As(err, &ex) {
		return fmt.Errorf("%s", ex.String())
	}
	return err
}
