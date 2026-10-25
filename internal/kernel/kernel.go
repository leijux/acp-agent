// Package kernel 把 billion-context 的 acp-kernel 封装成 Go 库。
//
// 用法：
//
//	k, err := kernel.New()
//	sess, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
//	defer sess.Close()
//
//	res, err := sess.Turn(msgs, tokenCount)
//	// res.Messages 是渲染后的视图（已折叠压缩块、已打 <acp> 标签）
//	sess.Compress([]kernel.Range{{StartRef: "m00005", EndRef: "m00020", Summary: "..."}})
//
// 依赖的 JS 产物由 go:embed 带入；重新生成见 build-cjs.sh。
package kernel

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"github.com/dop251/goja"
)

//go:embed kernel.dist.cjs
var defaultBundle []byte

// Options 用于定制 Kernel 行为。
type Options struct {
	// Bundle 覆盖内置的 CJS 产物（一般不需要）。
	Bundle []byte
	// Logger 接收 kernel 的 console 输出。
	Logger func(level, msg string)
	// Segmenter 替换默认分词器。
	Segmenter Segmenter
	// FS 提供 node:fs 语义（packs/persist 用）；nil 时相关调用会抛错，
	// kernel 内部的 try/catch 会把它们降级为“无 pack”。
	FS fs.FS
	// CountTokens 覆盖 token 计数。
	// 默认已是一个与上游逐字节等价的 Go 实现（快两个数量级），
	// 只有想用真 BPE 分词器时才需要覆盖。
	CountTokens func(text string) int
}

// Option 是函数式选项。
type Option func(*Options)

// WithLogger 设置 console 输出接收器。
func WithLogger(fn func(level, msg string)) Option {
	return func(o *Options) { o.Logger = fn }
}

// WithSegmenter 设置分词器。
func WithSegmenter(s Segmenter) Option {
	return func(o *Options) { o.Segmenter = s }
}

// WithFS 提供 node:fs 能力（只读）。
func WithFS(fsys fs.FS) Option {
	return func(o *Options) { o.FS = fsys }
}

// WithCountTokens 覆盖 token 计数。
//
// 默认值已与上游 defaultCountTokens 等价且快两个数量级；只有当你想换成
// 真正的 BPE 分词器（如 tiktoken 的 Go 端）时才需要设置。
func WithCountTokens(fn func(text string) int) Option {
	return func(o *Options) { o.CountTokens = fn }
}

// WithBundle 使用自定义 bundle。
func WithBundle(b []byte) Option {
	return func(o *Options) { o.Bundle = b }
}

// Kernel 是 bundle 的持有者；本身无状态，可为多个 Session 共享。
//
// bundle 只在首次需要时编译一次（goja.Program），之后的每个 Session
// 都用 vm.RunProgram 复用编译产物 —— 这是 NewSession 从 ~35ms 降下来的关键。
type Kernel struct {
	opts Options

	progOnce sync.Once
	prog     *goja.Program
	progErr  error

	// shared 是 Version/Functions/Call 共用的只读 runtime（避免每次重建）。
	sharedMu sync.Mutex
	shared   *runtime
}

// New 创建一个 Kernel。
func New(opts ...Option) (*Kernel, error) {
	o := Options{}
	for _, fn := range opts {
		fn(&o)
	}
	if o.Bundle == nil {
		if len(defaultBundle) == 0 {
			return nil, errors.New("kernel: no bundle embedded; run build-cjs.sh or use WithBundle")
		}
		o.Bundle = defaultBundle
	}
	// 关键优化：kernel 的默认 countTokens 是 JS 正则（goja 下走回溯引擎，
	// 占 Turn 七成以上分配）。用 Go 的等价实现替掉，语义不变。
	if o.CountTokens == nil {
		o.CountTokens = CountTokens
	}
	return &Kernel{opts: o}, nil
}

// program 编译（并缓存）bundle 的 wrapper。进程内只编译一次。
func (k *Kernel) program() (*goja.Program, error) {
	k.progOnce.Do(func() {
		src := "(function(module, exports, require){" + string(k.opts.Bundle) + "\n})"
		k.prog, k.progErr = goja.Compile("acp-kernel.wrapper.js", src, false)
		if k.progErr != nil {
			k.progErr = fmt.Errorf("kernel: compile bundle: %w", k.progErr)
		}
	})
	return k.prog, k.progErr
}

// sharedRuntime 返回一个进程级共享的只读 runtime，用于 Version/Functions/Call。
// runtime 非并发安全，调用方必须持有 sharedMu。
func (k *Kernel) sharedRuntime() (*runtime, error) {
	if k.shared != nil {
		return k.shared, nil
	}
	prog, err := k.program()
	if err != nil {
		return nil, err
	}
	rt, err := newRuntime(prog, k.opts)
	if err != nil {
		return nil, err
	}
	k.shared = rt
	return rt, nil
}

// NewSession 创建会话。每个 Session 持有独立 goja runtime；
// bundle 的编译产物由 Kernel 复用，初始化成本主要是 VM 分配与 createCore。
func (k *Kernel) NewSession(cfg Config) (*Session, error) {
	prog, err := k.program()
	if err != nil {
		return nil, err
	}
	rt, err := newRuntime(prog, k.opts)
	if err != nil {
		return nil, err
	}
	s := &Session{rt: rt, limit: cfg.ModelContextLimit}
	if err := s.init(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// Functions 列出 bundle 的导出符号（调试用）。
func (k *Kernel) Functions() ([]string, error) {
	k.sharedMu.Lock()
	defer k.sharedMu.Unlock()
	rt, err := k.sharedRuntime()
	if err != nil {
		return nil, err
	}
	return rt.exports.Keys(), nil
}

// Call 直接调用 bundle 的导出函数，返回原始 JSON（逃生门：覆盖尚未封装的 API）。
func (k *Kernel) Call(name string, args ...any) ([]byte, error) {
	k.sharedMu.Lock()
	defer k.sharedMu.Unlock()
	rt, err := k.sharedRuntime()
	if err != nil {
		return nil, err
	}
	vals := make([]goja.Value, 0, len(args))
	for _, a := range args {
		vals = append(vals, rt.vm.ToValue(a))
	}
	v, err := rt.call(name, vals...)
	if err != nil {
		return nil, err
	}
	return rt.toJSON(v)
}

// Version 返回 kernel bundle 的版本（若导出）。
func (k *Kernel) Version() (string, error) {
	k.sharedMu.Lock()
	defer k.sharedMu.Unlock()
	rt, err := k.sharedRuntime()
	if err != nil {
		return "", err
	}
	v := rt.exports.Get("KERNEL_VERSION")
	if v == nil || goja.IsUndefined(v) {
		v = rt.exports.Get("version")
	}
	if v == nil || goja.IsUndefined(v) {
		return "", nil
	}
	return v.String(), nil
}

func (o Options) String() string {
	return fmt.Sprintf("Options{logger=%v,fs=%v,segmenter=%v}", o.Logger != nil, o.FS != nil, o.Segmenter != nil)
}
