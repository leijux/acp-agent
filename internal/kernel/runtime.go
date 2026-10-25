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

	"github.com/dop251/goja"
)

// runtime 是一个 goja 实例 + 已加载的 kernel bundle。
// 非并发安全，由 Kernel.mu 串行化。
type runtime struct {
	vm        *goja.Runtime
	exports   *goja.Object
	core      goja.Value
	stringify goja.Callable
	parse     goja.Callable
	opts      Options
}

// newRuntime 在一个新 VM 上执行（已预编译的）bundle wrapper。
// prog 来自 Kernel.program()，可被任意多个 VM 复用。
func newRuntime(prog *goja.Program, opts Options) (*runtime, error) {
	vm := goja.New()
	vm.SetFieldNameMapper(goja.UncapFieldNameMapper())

	r := &runtime{vm: vm, opts: opts}
	r.injectConsole()
	if err := r.injectIntl(); err != nil {
		return nil, err
	}
	require := r.newRequire()

	module := vm.NewObject()
	exports := vm.NewObject()
	_ = module.Set("exports", exports)

	fnVal, err := vm.RunProgram(prog)
	if err != nil {
		return nil, fmt.Errorf("kernel: load bundle: %w", wrapJS(err))
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, errors.New("kernel: bundle did not evaluate to a function")
	}
	if _, err := fn(goja.Undefined(), module, exports, require); err != nil {
		return nil, fmt.Errorf("kernel: init module: %w", wrapJS(err))
	}

	exp, ok := module.Get("exports").(*goja.Object)
	if !ok {
		return nil, errors.New("kernel: module.exports is not an object")
	}
	r.exports = exp

	createCore, ok := goja.AssertFunction(exp.Get("createCore"))
	if !ok {
		return nil, errors.New("kernel: createCore is not a function")
	}
	var ports goja.Value
	if opts.CountTokens != nil {
		ports = vm.ToValue(map[string]any{
			"countTokens": vm.ToValue(func(call goja.FunctionCall) goja.Value {
				return vm.ToValue(opts.CountTokens(call.Argument(0).String()))
			}),
		})
	} else {
		ports = goja.Undefined()
	}
	core, err := createCore(goja.Undefined(), ports)
	if err != nil {
		return nil, fmt.Errorf("kernel: createCore: %w", wrapJS(err))
	}
	r.core = core

	jsonObj := vm.Get("JSON").ToObject(vm)
	r.stringify, _ = goja.AssertFunction(jsonObj.Get("stringify"))
	r.parse, _ = goja.AssertFunction(jsonObj.Get("parse"))
	return r, nil
}

func (r *runtime) injectConsole() {
	log := r.opts.Logger
	if log == nil {
		return
	}
	obj := r.vm.NewObject()
	mk := func(level string) goja.Value {
		return r.vm.ToValue(func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, a.String())
			}
			log(level, strings.Join(parts, " "))
			return goja.Undefined()
		})
	}
	_ = obj.Set("log", mk("info"))
	_ = obj.Set("info", mk("info"))
	_ = obj.Set("warn", mk("warn"))
	_ = obj.Set("error", mk("error"))
	_ = r.vm.Set("console", obj)
}

// newRequire 提供 kernel 需要的宿主模块：node:crypto / node:path / node:fs / node:module。
func (r *runtime) newRequire() goja.Value {
	vm := r.vm
	cache := map[string]goja.Value{}

	// --- path（POSIX 语义，与 Node 在 Linux 上一致）---
	pathMod := vm.NewObject()
	join := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, a := range call.Arguments {
			if s := a.String(); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return vm.ToValue(".")
		}
		return vm.ToValue(path.Join(parts...))
	})
	_ = pathMod.Set("join", join)
	_ = pathMod.Set("resolve", join)
	_ = pathMod.Set("normalize", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(path.Clean(call.Argument(0).String()))
	}))
	_ = pathMod.Set("dirname", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(path.Dir(call.Argument(0).String()))
	}))
	_ = pathMod.Set("basename", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(path.Base(call.Argument(0).String()))
	}))
	_ = pathMod.Set("extname", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(path.Ext(call.Argument(0).String()))
	}))
	_ = pathMod.Set("isAbsolute", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(path.IsAbs(call.Argument(0).String()))
	}))
	_ = pathMod.Set("sep", vm.ToValue("/"))
	_ = pathMod.Set("delimiter", vm.ToValue(":"))
	_ = pathMod.Set("default", pathMod)

	// --- crypto ---
	cryptoMod := vm.NewObject()
	_ = cryptoMod.Set("createHash", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		alg := strings.ToLower(call.Argument(0).String())
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
			panic(vm.ToValue("kernel: unsupported hash algorithm: " + alg))
		}
		obj := vm.NewObject()
		_ = obj.Set("update", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			h.Write([]byte(call.Argument(0).String()))
			return obj
		}))
		_ = obj.Set("digest", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			sum := h.Sum(nil)
			switch call.Argument(0).String() {
			case "base64":
				return vm.ToValue(base64.StdEncoding.EncodeToString(sum))
			case "base64url":
				return vm.ToValue(base64.RawURLEncoding.EncodeToString(sum))
			default:
				return vm.ToValue(hex.EncodeToString(sum))
			}
		}))
		return obj
	}))
	_ = cryptoMod.Set("createHmac", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		alg := strings.ToLower(call.Argument(0).String())
		key := []byte(call.Argument(1).String())
		var h func() hash.Hash
		switch alg {
		case "sha1":
			h = sha1.New
		case "sha256":
			h = sha256.New
		case "sha512":
			h = sha512.New
		default:
			panic(vm.ToValue("kernel: unsupported hmac algorithm: " + alg))
		}
		mac := hmac.New(h, key)
		obj := vm.NewObject()
		_ = obj.Set("update", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			mac.Write([]byte(call.Argument(0).String()))
			return obj
		}))
		_ = obj.Set("digest", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			sum := mac.Sum(nil)
			if call.Argument(0).String() == "base64" {
				return vm.ToValue(base64.StdEncoding.EncodeToString(sum))
			}
			return vm.ToValue(hex.EncodeToString(sum))
		}))
		return obj
	}))
	_ = cryptoMod.Set("randomUUID", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(vm.ToValue("kernel: randomUUID: " + err.Error()))
		}
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return vm.ToValue(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
	}))
	_ = cryptoMod.Set("default", cryptoMod)

	// --- fs（可选，映射到 io/fs.FS；packs.ts 的读取失败会被上游 try/catch 吞掉）---
	fsMod := vm.NewObject()
	notAvail := func(op string) goja.Value {
		return vm.ToValue(func(call goja.FunctionCall) goja.Value {
			panic(vm.ToValue("kernel: fs." + op + " unavailable: no FS configured (kernel.WithFS)"))
		})
	}
	_ = fsMod.Set("readFileSync", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if r.opts.FS == nil {
			panic(vm.ToValue("kernel: fs.readFileSync unavailable: no FS configured (kernel.WithFS)"))
		}
		name := cleanFSPath(call.Argument(0).String())
		data, err := fs.ReadFile(r.opts.FS, name)
		if err != nil {
			panic(vm.ToValue("kernel: fs.readFileSync " + name + ": " + err.Error()))
		}
		return vm.ToValue(string(data))
	}))
	_ = fsMod.Set("readdirSync", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if r.opts.FS == nil {
			panic(vm.ToValue("kernel: fs.readdirSync unavailable: no FS configured (kernel.WithFS)"))
		}
		name := cleanFSPath(call.Argument(0).String())
		entries, err := fs.ReadDir(r.opts.FS, name)
		if err != nil {
			panic(vm.ToValue("kernel: fs.readdirSync " + name + ": " + err.Error()))
		}
		arr := vm.NewArray()
		for i, e := range entries {
			_ = arr.Set(fmt.Sprintf("%d", i), e.Name())
		}
		return arr
	}))
	_ = fsMod.Set("existsSync", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if r.opts.FS == nil {
			return vm.ToValue(false)
		}
		_, err := fs.Stat(r.opts.FS, cleanFSPath(call.Argument(0).String()))
		return vm.ToValue(err == nil)
	}))
	for _, k := range []string{"writeFileSync", "mkdirSync", "rmSync", "statSync", "openSync", "readSync", "closeSync"} {
		_ = fsMod.Set(k, notAvail(k))
	}
	_ = fsMod.Set("default", fsMod)
	promises := vm.NewObject()
	_ = promises.Set("readFile", notAvail("promises.readFile"))
	_ = promises.Set("writeFile", notAvail("promises.writeFile"))
	_ = promises.Set("mkdir", notAvail("promises.mkdir"))
	_ = fsMod.Set("promises", promises)

	// --- module ---
	moduleMod := vm.NewObject()
	var requireFn goja.Value
	_ = moduleMod.Set("createRequire", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return requireFn
	}))
	_ = moduleMod.Set("default", moduleMod)

	cache["node:path"] = pathMod
	cache["path"] = pathMod
	cache["path/posix"] = pathMod
	cache["node:crypto"] = cryptoMod
	cache["crypto"] = cryptoMod
	cache["node:fs"] = fsMod
	cache["fs"] = fsMod
	cache["node:fs/promises"] = promises
	cache["fs/promises"] = promises
	cache["node:module"] = moduleMod
	cache["module"] = moduleMod

	requireFn = vm.ToValue(func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		if v, ok := cache[name]; ok {
			return v
		}
		panic(vm.ToValue("kernel: cannot find module '" + name + "'"))
	})
	return requireFn
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

func (r *runtime) call(fnName string, args ...goja.Value) (goja.Value, error) {
	fn, ok := goja.AssertFunction(r.exports.Get(fnName))
	if !ok {
		return nil, fmt.Errorf("kernel: %s is not exported", fnName)
	}
	return r.callFn(fn, args...)
}

func (r *runtime) callFn(fn goja.Callable, args ...goja.Value) (goja.Value, error) {
	v, err := fn(goja.Undefined(), args...)
	if err != nil {
		return nil, wrapJS(err)
	}
	return v, nil
}

func (r *runtime) coreCall(method string, args ...goja.Value) (goja.Value, error) {
	fn, ok := goja.AssertFunction(r.core.ToObject(r.vm).Get(method))
	if !ok {
		return nil, fmt.Errorf("kernel: core.%s is not a function", method)
	}
	return r.callFn(fn, args...)
}

// toJSON 把 JS 值序列化成原始 JSON。
func (r *runtime) toJSON(v goja.Value) ([]byte, error) {
	out, err := r.stringify(goja.Undefined(), v)
	if err != nil {
		return nil, wrapJS(err)
	}
	if goja.IsUndefined(out) {
		return nil, nil
	}
	return []byte(out.String()), nil
}

func (r *runtime) fromJSON(data []byte) (goja.Value, error) {
	v, err := r.parse(goja.Undefined(), r.vm.ToValue(string(data)))
	if err != nil {
		return nil, wrapJS(err)
	}
	return v, nil
}

func wrapJS(err error) error {
	if err == nil {
		return nil
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return fmt.Errorf("%s", ex.String())
	}
	return err
}
