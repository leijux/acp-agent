package kernel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
)

// testProgram 编译默认 bundle 的 wrapper（与 Kernel.program 等价）。
func testProgram(t *testing.T) *moejs.Script {
	t.Helper()
	src := "(function(module, exports, require){" + string(defaultBundle) + "})"
	prog, err := moejs.CompileScript("test.wrapper.js", src)
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

// TestSegmenterWired 检查宿主注入的分词器是否真的被 kernel 使用。
func TestSegmenterWired(t *testing.T) {
	n := 0
	rt, err := newRuntime(testProgram(t), Options{Segmenter: spySeg{n: &n, inner: RuneSegmenter{}}})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}

	v, err := rt.call("searchBlocks",
		rt.val([]map[string]any{{"kind": "block", "ref": "b1", "text": "压缩上下文 cache 摘要", "title": "t"}}),
		rt.val("压缩"))
	if err != nil {
		t.Fatalf("searchBlocks: %v", err)
	}
	t.Logf("searchBlocks = %s (calls=%d)", v.String(), n)
	if n == 0 {
		t.Error("segmenter never invoked by searchBlocks")
	}

	// 再确认 segment 的结果形状与 kernel 期望一致。
	raw, err := rt.evalString(`(function(){
		var s = new Intl.Segmenter("zh", {granularity:"word"}).segment("上下文 cache");
		var o = [];
		for (var i = 0; i < s.length; i++) o.push(s[i].segment);
		return o.join("|");
	})()`)
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
	cfgV, _ := rt.call("defaultConfig", rt.val(200000))
	stV, _ := rt.call("createInitialState")

	msgs := make([]any, 0, 12)
	for i := 0; i < 12; i++ {
		msgs = append(msgs, map[string]any{
			"id":          fmt.Sprintf("x_%02d", i),
			"role":        "user",
			"contentType": "text",
			"text":        strings.Repeat("上下文压缩 context window budget 讨论。", 60),
		})
	}
	out, err := rt.coreCall("processTurn", rt.val(map[string]any{
		"messages":   rt.val(msgs),
		"state":      stV,
		"config":     cfgV,
		"tokenCount": 90000,
	}))
	if err != nil {
		t.Fatal(err)
	}
	view := rt.prop(out, "messages")
	ac, err := rt.coreCall("applyCompression", rt.val(map[string]any{
		"ranges": rt.val([]map[string]any{{
			"startRef": "m00001", "endRef": "m00006",
			"summary": strings.Repeat("压缩摘要 summary of earlier turns 关于上下文预算的讨论。", 3),
		}}),
		"messages": view,
		"state":    rt.prop(out, "state"),
		"config":   cfgV,
	}))
	if err != nil {
		t.Fatalf("applyCompression: %v", err)
	}

	out3, err := rt.coreCall("processTurn", rt.val(map[string]any{
		"messages":   view,
		"state":      rt.prop(ac, "state"),
		"config":     cfgV,
		"tokenCount": 40000,
	}))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := rt.toJSON(rt.prop(out3, "messages"))
	var view2 []map[string]any
	_ = json.Unmarshal(data, &view2)
	for i, m := range view2 {
		if i > 2 {
			break
		}
		b, _ := json.Marshal(m)
		t.Logf("msg[%d] = %s", i, b)
	}
	nd, _ := rt.toJSON(rt.prop(out, "nudge"))
	t.Logf("nudge = %s", truncate(string(nd), 700))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
