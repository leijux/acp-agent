package kernel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// testProgram 编译默认 bundle 的 wrapper（与 Kernel.program 等价）。
func testProgram(t *testing.T) *goja.Program {
	t.Helper()
	src := "(function(module, exports, require){" + string(defaultBundle) + "})"
	prog, err := goja.Compile("test.wrapper.js", src, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return prog
}

type spySeg struct {
	n     *int
	inner Segmenter
}

func (s spySeg) Segment(text string) []Segment {
	*s.n++
	return s.inner.Segment(text)
}

// TestSegmenterWired 检查 Intl.Segmenter 注入是否真的被 kernel 使用。
func TestSegmenterWired(t *testing.T) {
	n := 0
	rt, err := newRuntime(testProgram(t), Options{Segmenter: spySeg{n: &n, inner: RuneSegmenter{}}})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}

	v, err := rt.call("searchBlocks",
		rt.vm.ToValue([]map[string]any{{"kind": "block", "ref": "b1", "text": "压缩上下文 cache 摘要", "title": "t"}}),
		rt.vm.ToValue("压缩"))
	if err != nil {
		t.Fatalf("searchBlocks: %v", err)
	}
	t.Logf("searchBlocks = %s (calls=%d)", v.String(), n)
	if n == 0 {
		t.Error("segmenter never invoked by searchBlocks")
	}

	// 再确认 segment 的结果形状与 kernel 期望一致。
	raw, err := rt.vm.RunString(`(function(){var s=new Intl.Segmenter("zh",{granularity:"word"}).segment("上下文 cache");var o=[];for(var i=0;i<s.length;i++)o.push(s[i].segment);return o.join("|");})()`)
	if err != nil {
		t.Fatalf("segmenter probe: %v", err)
	}
	t.Logf("segments = %s", raw.String())
}

// TestDumpView 打印渲染视图里 summary 消息的字段形状（供 demo/客户端参考）。
func TestDumpView(t *testing.T) {
	n := 0
	rt, err := newRuntime(testProgram(t), Options{Segmenter: spySeg{n: &n, inner: RuneSegmenter{}}})
	if err != nil {
		t.Fatal(err)
	}
	cfgV, _ := rt.call("defaultConfig", rt.vm.ToValue(200000))
	stV, _ := rt.call("createInitialState")

	msgs := rt.vm.NewArray()
	for i := 0; i < 12; i++ {
		o := rt.vm.NewObject()
		_ = o.Set("id", fmt.Sprintf("x_%02d", i))
		_ = o.Set("role", "user")
		_ = o.Set("contentType", "text")
		_ = o.Set("text", strings.Repeat("上下文压缩 context window budget 讨论。", 60))
		_ = msgs.Set(fmt.Sprintf("%d", i), o)
	}
	in := rt.vm.NewObject()
	_ = in.Set("messages", msgs)
	_ = in.Set("state", stV)
	_ = in.Set("config", cfgV)
	_ = in.Set("tokenCount", 90000)
	out, err := rt.coreCall("processTurn", in)
	if err != nil {
		t.Fatal(err)
	}
	obj := out.ToObject(rt.vm)
	view := obj.Get("messages")
	in2 := rt.vm.NewObject()
	_ = in2.Set("ranges", rt.vm.ToValue([]map[string]any{{
		"startRef": "m00001", "endRef": "m00006",
		"summary": strings.Repeat("压缩摘要 summary of earlier turns 关于上下文预算的讨论。", 3),
	}}))
	_ = in2.Set("messages", view)
	_ = in2.Set("state", obj.Get("state"))
	_ = in2.Set("config", cfgV)
	ac, err := rt.coreCall("applyCompression", in2)
	if err != nil {
		t.Fatalf("applyCompression: %v", err)
	}
	acObj := ac.ToObject(rt.vm)

	in3 := rt.vm.NewObject()
	_ = in3.Set("messages", view)
	_ = in3.Set("state", acObj.Get("state"))
	_ = in3.Set("config", cfgV)
	_ = in3.Set("tokenCount", 40000)
	out3, err := rt.coreCall("processTurn", in3)
	if err != nil {
		t.Fatal(err)
	}
	v3 := out3.ToObject(rt.vm).Get("messages")
	data, _ := rt.toJSON(v3)
	var any []map[string]any
	_ = json.Unmarshal(data, &any)
	for i, m := range any {
		if i > 2 {
			break
		}
		b, _ := json.Marshal(m)
		t.Logf("msg[%d] = %s", i, b)
	}
	nd, _ := rt.toJSON(out.ToObject(rt.vm).Get("nudge"))
	b, _ := json.Marshal(json.RawMessage(nd))
	_ = b
	t.Logf("nudge = %s", truncate(string(nd), 700))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
