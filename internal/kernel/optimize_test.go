package kernel

import "testing"

// TestDefaultCountTokensIsGo 锁定优化：默认必须走 Go 计数实现，
// 而不是回落到 kernel 的 JS 正则（goja 下走 regexp2，慢两个数量级）。
func TestDefaultCountTokensIsGo(t *testing.T) {
	k, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if k.opts.CountTokens == nil {
		t.Fatal("默认未注入 Go 版 countTokens —— 性能会退化约 3 倍")
	}
	// 行为等价性由 TestCountTokensMatchesUpstream 保证。
	if got, want := k.opts.CountTokens("中文 abc"), CountTokens("中文 abc"); got != want {
		t.Fatalf("默认 CountTokens = %d, want %d", got, want)
	}
	if !equalFunc(k.opts.CountTokens, CountTokens) {
		t.Log("默认 countTokens 被替换（WithCountTokens 生效）")
	}
}

func equalFunc(a, b func(string) int) bool {
	// 函数不可比较；用几个样本探测是否为同一实现。
	for _, s := range []string{"", "abc", "中文", "😀x"} {
		if a(s) != b(s) {
			return false
		}
	}
	return true
}

// TestProgramCompiledOnce 确认 bundle 只编译一次（NewSession 依赖它）。
func TestProgramCompiledOnce(t *testing.T) {
	k, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p1, err := k.program()
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	p2, err := k.program()
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	if p1 != p2 {
		t.Fatal("program 被重复编译")
	}
	// 多个 Session 必须复用同一 program，但各自持有独立 VM。
	s1, err := k.NewSession(Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s1.Close()
	s2, err := k.NewSession(Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s2.Close()
	if s1.rt.vm == s2.rt.vm {
		t.Fatal("两个 Session 共享了同一个 VM（并发不安全）")
	}
}
