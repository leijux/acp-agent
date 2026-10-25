package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"
)

func mustRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func main() {
	// 优先 $KERNEL_BUNDLE，否则用仓库内的产物（它本来就是 script/build-cjs.sh 的产物），
	// 最后回退到早期调试用的临时路径。
	bundlePath := os.Getenv("KERNEL_BUNDLE")
	if bundlePath == "" {
		bundlePath = filepath.Join("internal", "kernel", "kernel.dist.cjs")
		if _, err := os.Stat(bundlePath); err != nil {
			bundlePath = "/tmp/kernel.bundle.cjs"
		}
	}
	src := mustRead(bundlePath)

	t0 := time.Now()
	vm := goja.New()

	// --- console shim ---
	console := vm.NewObject()
	_ = console.Set("log", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		fmt.Println("[js]", strings.Join(parts, " "))
		return goja.Undefined()
	})
	_ = console.Set("warn", console.Get("log"))
	_ = console.Set("error", console.Get("log"))
	_ = console.Set("info", console.Get("log"))
	_ = vm.Set("console", console)

	// --- Intl.Segmenter shim (goja has no Intl) ---
	if os.Getenv("SKIP_INTL") == "" {
		if _, err := vm.RunString(segmenterShim); err != nil {
			panic(fmt.Sprintf("shim failed: %v", err))
		}
	}

	// --- require shim ---
	require := newRequire(vm)

	// --- CJS wrapper ---
	module := vm.NewObject()
	exports := vm.NewObject()
	_ = module.Set("exports", exports)

	fnVal, err := vm.RunString("(function(module, exports, require){" + src + "\n})")
	if err != nil {
		fmt.Printf("PARSE/EVAL ERROR: %v\n", err)
		os.Exit(1)
	}
	fn, _ := goja.AssertFunction(fnVal)
	if _, err := fn(goja.Undefined(), module, exports, require); err != nil {
		fmt.Printf("MODULE INIT ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("1) bundle loaded OK in %v\n", time.Since(t0).Round(time.Millisecond))

	mod := module.Get("exports").ToObject(vm)
	keys := mod.Keys()
	fmt.Printf("2) exports: %d symbols\n", len(keys))

	// --- create core/state/config ---
	coreFn, ok := goja.AssertFunction(mod.Get("createCore"))
	if !ok {
		fmt.Println("createCore not callable")
		os.Exit(1)
	}
	core, err := coreFn(goja.Undefined())
	if err != nil {
		fmt.Printf("createCore ERROR: %v\n", err)
		os.Exit(1)
	}
	stateFn, _ := goja.AssertFunction(mod.Get("createInitialState"))
	state, err := stateFn(goja.Undefined())
	if err != nil {
		fmt.Printf("createInitialState ERROR: %v\n", err)
		os.Exit(1)
	}
	cfgFn, _ := goja.AssertFunction(mod.Get("defaultConfig"))
	cfg, err := cfgFn(goja.Undefined(), vm.ToValue(200000))
	if err != nil {
		fmt.Printf("defaultConfig ERROR: %v\n", err)
		os.Exit(1)
	}

	// --- build messages ---
	msgs := vm.NewArray()
	n := 40
	for i := 0; i < n; i++ {
		m := vm.NewObject()
		_ = m.Set("id", fmt.Sprintf("msg_%03d", i))
		role := "user"
		ctype := "text"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = m.Set("role", role)
		_ = m.Set("contentType", ctype)
		if i%7 == 3 {
			_ = m.Set("role", "tool")
			_ = m.Set("contentType", "tool-result")
			_ = m.Set("toolName", "read")
			_ = m.Set("toolCallId", fmt.Sprintf("call_%d", i))
			_ = m.Set("text", fmt.Sprintf("file contents chunk %d: func main() { /* 中文注释，缓存与压缩 */ }; ", i)+strings.Repeat("line of code and prose about context compression 压缩上下文 ", 20))
		} else {
			_ = m.Set("text", fmt.Sprintf("turn %d: 请分析 billion-context 的压缩策略 cache & compression behaviour, line %d; ", i, i+100)+strings.Repeat("详细讨论 context window, token budget, summary tier 以及缓存命中率。", 15))
		}
		_ = msgs.Set(fmt.Sprintf("%d", i), m)
	}

	// --- processTurn ---
	coreObj := core.ToObject(vm)
	processTurn, ok := goja.AssertFunction(coreObj.Get("processTurn"))
	if !ok {
		fmt.Println("processTurn not callable")
		os.Exit(1)
	}
	input := vm.NewObject()
	_ = input.Set("messages", msgs)
	_ = input.Set("state", state)
	_ = input.Set("config", cfg)
	_ = input.Set("tokenCount", 90000)

	t1 := time.Now()
	res, err := processTurn(core, input)
	if err != nil {
		fmt.Printf("processTurn ERROR: %v\n", err)
		os.Exit(1)
	}
	resObj := res.ToObject(vm)
	outMsgs := resObj.Get("messages").ToObject(vm)
	fmt.Printf("3) processTurn OK in %v: messages=%d, nudge=%v\n",
		time.Since(t1).Round(time.Millisecond), outMsgs.Get("length").ToInteger(), !goja.IsUndefined(resObj.Get("nudge")))

	nextState := resObj.Get("state").ToObject(vm)
	refs := nextState.Get("messageRefs").ToObject(vm)
	byRaw := refs.Get("byRaw").ToObject(vm)
	fmt.Printf("   refs assigned: %d (sample: %v)\n", len(byRaw.Keys()), byRaw.Get("msg_000"))

	// --- applyCompression ---
	apply, _ := goja.AssertFunction(coreObj.Get("applyCompression"))
	rangeObj := vm.NewObject()
	_ = rangeObj.Set("startRef", "m00005")
	_ = rangeObj.Set("endRef", "m00020")
	_ = rangeObj.Set("summary", "SUMMARY: earlier turns condensed — 压缩摘要覆盖 m00005..m00020 compressed context")
	ranges := vm.NewArray()
	_ = ranges.Set("0", rangeObj)

	acInput := vm.NewObject()
	_ = acInput.Set("ranges", ranges)
	_ = acInput.Set("messages", resObj.Get("messages"))
	_ = acInput.Set("state", resObj.Get("state"))
	_ = acInput.Set("config", cfg)

	t2 := time.Now()
	acRes, err := apply(core, acInput)
	if err != nil {
		fmt.Printf("applyCompression ERROR: %v\n", err)
		os.Exit(1)
	}
	acObj := acRes.ToObject(vm)
	acState := acObj.Get("state").ToObject(vm)
	blocks := acState.Get("blocks").ToObject(vm)
	acResult := acObj.Get("result").ToObject(vm)
	fmt.Printf("4) applyCompression OK in %v: blocks=%d created=%v compressedTokens=%v\n", time.Since(t2).Round(time.Millisecond),
		blocks.Get("length").ToInteger(), acResult.Get("blocksCreated"), acResult.Get("tokensCompressed"))
	fmt.Printf("   errors=%v warnings=%v notes=%v\n", acResult.Get("errors"), acResult.Get("warnings"), acResult.Get("notes"))

	// --- second processTurn (pruned view) ---
	input2 := vm.NewObject()
	_ = input2.Set("messages", resObj.Get("messages"))
	_ = input2.Set("state", acState)
	_ = input2.Set("config", cfg)
	_ = input2.Set("tokenCount", 30000)
	res2, err := processTurn(core, input2)
	if err != nil {
		fmt.Printf("processTurn#2 ERROR: %v\n", err)
		os.Exit(1)
	}
	r2 := res2.ToObject(vm)
	m2 := r2.Get("messages").ToObject(vm)
	fmt.Printf("5) processTurn#2 OK: messages=%d (folded)\n", m2.Get("length").ToInteger())

	// --- search ---
	searchFn, _ := goja.AssertFunction(coreObj.Get("search"))
	t3 := time.Now()
	sres, err := searchFn(core, vm.ToValue("压缩 摘要"), r2.Get("state"))
	if err != nil {
		fmt.Printf("search ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("6) search OK in %v: hits=%d\n", time.Since(t3).Round(time.Millisecond), sres.ToObject(vm).Get("length").ToInteger())

	// --- status ---
	statusFn, _ := goja.AssertFunction(coreObj.Get("status"))
	st, err := statusFn(core, r2.Get("state"), vm.ToValue(30000), cfg)
	if err != nil {
		fmt.Printf("status ERROR: %v\n", err)
		os.Exit(1)
	}
	stObj := st.ToObject(vm)
	fmt.Printf("7) status OK: contextUsage=%v tokensCompressed=%v activeBlocks=%v totalBlocks=%v\n",
		stObj.Get("contextUsage"), stObj.Get("tokensCompressed"), stObj.Get("activeBlocks"), stObj.Get("totalBlocks"))

	// --- decompress round-trip ---
	decFn, _ := goja.AssertFunction(coreObj.Get("decompress"))
	blk0 := blocks.ToObject(vm).Get("0")
	if !goja.IsUndefined(blk0) {
		bid := blk0.ToObject(vm).Get("blockId").String()
		dec, err := decFn(core, vm.ToValue(bid), r2.Get("state"))
		if err != nil {
			fmt.Printf("decompress ERROR: %v\n", err)
		} else if goja.IsUndefined(dec) {
			fmt.Printf("8) decompress(%s) -> undefined (inactive)\n", bid)
		} else {
			d := dec.ToObject(vm)
			fmt.Printf("8) decompress(%s) OK: summary=%d chars, directMessageIds=%v\n", bid, len(d.Get("summary").String()), d.Get("directMessageIds").ToObject(vm).Get("length"))
		}
	}

	// --- benchmark: 200-message turn ---
	big := vm.NewArray()
	for i := 0; i < 200; i++ {
		m := vm.NewObject()
		_ = m.Set("id", fmt.Sprintf("big_%04d", i))
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = m.Set("role", role)
		_ = m.Set("contentType", "text")
		_ = m.Set("text", fmt.Sprintf("turn %d: 上下文压缩上下文压缩 context window management discussion with details about tiering, summarisation and cache behaviour. ", i)+strings.Repeat("abcdefghij klmnopqrst uvwxyz 0123456789 中文内容测试 ", 8))
		_ = big.Set(fmt.Sprintf("%d", i), m)
	}
	st2Fn, _ := goja.AssertFunction(mod.Get("createInitialState"))
	st2, _ := st2Fn(goja.Undefined())
	bin := vm.NewObject()
	_ = bin.Set("messages", big)
	_ = bin.Set("state", st2)
	_ = bin.Set("config", cfg)
	_ = bin.Set("tokenCount", 150000)
	tb := time.Now()
	bres, err := processTurn(core, bin)
	if err != nil {
		fmt.Printf("bench processTurn ERROR: %v\n", err)
	} else {
		fmt.Printf("9) bench processTurn(200 msgs) OK in %v: out=%d\n", time.Since(tb).Round(time.Millisecond), bres.ToObject(vm).Get("messages").ToObject(vm).Get("length"))
	}
	jsonObj := vm.Get("JSON").ToObject(vm)
	stringify, _ := goja.AssertFunction(jsonObj.Get("stringify"))
	msgsJSON, _ := stringify(goja.Undefined(), r2.Get("messages"))
	sum := sha256.Sum256([]byte(msgsJSON.String()))
	fmt.Printf("messagesHash %x\n", sum[:8])
	blocksJSON, _ := stringify(goja.Undefined(), r2.ToObject(vm).Get("state").ToObject(vm).Get("blocks"))
	bsum := sha256.Sum256([]byte(blocksJSON.String()))
	fmt.Printf("blocksHash %x\n", bsum[:8])
	_ = vm.Set("__state", r2.ToObject(vm).Get("state"))
	bnt, _ := vm.RunString(`(function(){ var b = __state.blocks[0]; var c = {}; for (var k in b) c[k] = b[k]; c.createdAt = 0; return JSON.stringify(c); })()`)
	bntSum := sha256.Sum256([]byte(bnt.String()))
	fmt.Printf("blockNoTimeJSON %s\n", bnt.String())
	fmt.Printf("blockNoTimeHash %x\n", bntSum[:8])

	// searchBlocks (lexical BM25/hybrid pipeline, not core.search)
	sbFn, _ := goja.AssertFunction(mod.Get("searchBlocks"))
	bdFn, _ := goja.AssertFunction(mod.Get("blockDocs"))
	mdFn, _ := goja.AssertFunction(mod.Get("messageDocs"))
	bdocs, err := bdFn(goja.Undefined(), r2.ToObject(vm).Get("state"))
	if err == nil {
		ts := time.Now()
		hits, err := sbFn(goja.Undefined(), bdocs, vm.ToValue("压缩 摘要"))
		if err != nil {
			fmt.Printf("searchBlocks ERROR: %v\n", err)
		} else {
			fmt.Printf("10) searchBlocks OK in %v: hits=%d\n", time.Since(ts).Round(time.Millisecond), hits.ToObject(vm).Get("length"))
		}
	}
	_ = mdFn

	fmt.Printf("TOTAL %v\n", time.Since(t0).Round(time.Millisecond))
}

const segmenterShim = `
(function(){
  if (typeof Intl === "undefined") { globalThis.Intl = {}; }
  if (typeof Intl.Segmenter === "function") return;
  var CJK = "\\u3400-\\u4dbf\\u4e00-\\u9fff\\uf900-\\ufaff\\u3040-\\u30ff\\uac00-\\ud7af";
  Intl.Segmenter = function Segmenter(locale, opts) {
    this.granularity = (opts && opts.granularity) || "word";
  };
  Intl.Segmenter.prototype.segment = function segment(input) {
    var s = String(input);
    var re = new RegExp("[" + CJK + "]" + "|[A-Za-z0-9_]+|\\s+|[^\\sA-Za-z0-9_" + CJK + "]+", "g");
    var out = [], m;
    while ((m = re.exec(s)) !== null) {
      out.push({ segment: m[0], index: m.index, input: s });
      if (m[0].length === 0) re.lastIndex++;
    }
    return out;
  };
})();
`

func newRequire(vm *goja.Runtime) goja.Value {
	cache := map[string]goja.Value{}

	pathMod := vm.NewObject()
	joinFn := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		return vm.ToValue(filepath.Join(parts...))
	})
	dirFn := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.Dir(call.Argument(0).String()))
	})
	baseFn := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.Base(call.Argument(0).String()))
	})
	extFn := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.Ext(call.Argument(0).String()))
	})
	_ = pathMod.Set("join", joinFn)
	_ = pathMod.Set("resolve", joinFn)
	_ = pathMod.Set("dirname", dirFn)
	_ = pathMod.Set("basename", baseFn)
	_ = pathMod.Set("extname", extFn)
	_ = pathMod.Set("normalize", vm.ToValue(func(call goja.FunctionCall) goja.Value { return call.Argument(0) }))
	_ = pathMod.Set("isAbsolute", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(strings.HasPrefix(call.Argument(0).String(), "/") || filepath.IsAbs(call.Argument(0).String()))
	}))
	_ = pathMod.Set("sep", vm.ToValue(string(filepath.Separator)))
	_ = pathMod.Set("default", pathMod)

	cryptoMod := vm.NewObject()
	_ = cryptoMod.Set("createHash", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		alg := strings.ToLower(call.Argument(0).String())
		var h hash.Hash
		switch alg {
		case "sha256":
			h = sha256.New()
		default:
			panic(vm.ToValue("unsupported hash: " + alg))
		}
		obj := vm.NewObject()
		_ = obj.Set("update", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			h.Write([]byte(call.Argument(0).String()))
			return obj
		}))
		_ = obj.Set("digest", vm.ToValue(func(call goja.FunctionCall) goja.Value {
			sum := h.Sum(nil)
			if call.Argument(0).String() == "base64" {
				return vm.ToValue(base64.StdEncoding.EncodeToString(sum))
			}
			return vm.ToValue(hex.EncodeToString(sum))
		}))
		return obj
	}))

	moduleMod := vm.NewObject()
	_ = moduleMod.Set("createRequire", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(func(call goja.FunctionCall) goja.Value {
			panic(vm.ToValue("module not found: " + call.Argument(0).String()))
		})
	}))

	fsMod := vm.NewObject()
	notAvail := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		panic(vm.ToValue("fs not available in goja sandbox"))
	})
	for _, k := range []string{"readFileSync", "readdirSync", "existsSync", "writeFileSync", "mkdirSync", "statSync", "rmSync", "openSync", "readSync", "closeSync"} {
		_ = fsMod.Set(k, notAvail)
	}

	cache["node:path"] = pathMod
	cache["path"] = pathMod
	cache["node:crypto"] = cryptoMod
	cache["crypto"] = cryptoMod
	cache["node:module"] = moduleMod
	cache["module"] = moduleMod
	cache["node:fs"] = fsMod
	cache["fs"] = fsMod

	return vm.ToValue(func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		if v, ok := cache[name]; ok {
			return v
		}
		panic(vm.ToValue("Cannot find module '" + name + "'"))
	})
}
