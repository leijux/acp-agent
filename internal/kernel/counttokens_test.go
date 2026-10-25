package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCountTokensMatchesUpstream 用上游的 JS 实现逐字对比 Go 版结果。
// 这是「用 Go 替掉 JS 正则」这一优化的正确性前提：必须完全等价。
func TestCountTokensMatchesUpstream(t *testing.T) {
	rt, err := newRuntime(testProgram(t), Options{})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	// 上游 defaultCountTokens 的表达式（与 kernel.dist.cjs:491 一致）。
	const expr = `(function(text){
		var m = text.match(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g);
		var n = m ? m.length : 0;
		return n + Math.ceil((text.length - n) / 4);
	})(%s)`

	cases := []string{
		"",
		"hello world",
		"CJK 中文测试",
		"日本語のテストとカタカナ",
		"한국어 테스트",
		"emoji 😀🎉 mixed 中文 andEnglish12345",
		`<acp type="file" path="D:/x.go">func main() {}</acp>`,
		strings.Repeat("上下文压缩 ", 50),
		strings.Repeat("a", 1000),
		strings.Repeat("中", 333),
		"𠀀𠀁 增补平面字符（不在上游字符类内）",
	}
	for _, c := range cases {
		lit, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		v, err := rt.vm.RunString(strings.Replace(expr, "%s", string(lit), 1))
		if err != nil {
			t.Fatalf("js countTokens(%q): %v", truncForTest(c, 30), err)
		}
		want := int(v.ToInteger())
		if got := CountTokens(c); got != want {
			t.Errorf("CountTokens(%q) = %d, js = %d", truncForTest(c, 40), got, want)
		}
	}
}

// TestEstimateTokensConsistent 检查 EstimateTokens 与逐条 CountTokens 之和一致。
func TestEstimateTokensConsistent(t *testing.T) {
	msgs := []Message{
		{ID: "a", Text: "hello world"},
		{ID: "b", Text: "中文内容", ThinkingTokens: 42},
		{ID: "c", Text: ""},
	}
	want := CountTokens("hello world") + CountTokens("中文内容") + 42
	if got := EstimateTokens(msgs); got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
}

func truncForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
