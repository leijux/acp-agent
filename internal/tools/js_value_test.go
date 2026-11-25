package tools

import (
	"strings"
	"testing"
)

// BigInt 不能被渲染成 Go 类型名（`<big.Int>`）——模型要的是数字本身。
func TestJSValue_BigInt(t *testing.T) {
	r := runJS(t, Args{"script": "2n ** 100n"})
	want := "1267650600228229401496703205376"
	if got := r.Text(); !strings.Contains(got, want) {
		t.Fatalf("结果 = %q，期望含 %s", got, want)
	}
}

// Promise 不能被渲染成空对象 `{}`：要给出状态与结果。
func TestJSValue_PromiseFulfilled(t *testing.T) {
	r := runJS(t, Args{"script": "Promise.resolve(1)"})
	if got := r.Text(); !strings.Contains(got, "Promise { 1 }") {
		t.Fatalf("结果 = %q", got)
	}
}

func TestJSValue_PromiseStringResultQuoted(t *testing.T) {
	r := runJS(t, Args{"script": "Promise.resolve('abc')"})
	if got := r.Text(); !strings.Contains(got, `Promise { "abc" }`) {
		t.Fatalf("结果 = %q", got)
	}
}

func TestJSValue_PromisePending(t *testing.T) {
	r := runJS(t, Args{"script": "new Promise(() => {})"})
	if got := r.Text(); !strings.Contains(got, "Promise { <pending> }") {
		t.Fatalf("结果 = %q", got)
	}
}

func TestJSValue_PromiseRejected(t *testing.T) {
	r := runJS(t, Args{"script": "Promise.reject(new Error('boom'))"})
	got := r.Text()
	if !strings.Contains(got, "<rejected>") || !strings.Contains(got, "boom") {
		t.Fatalf("结果 = %q", got)
	}
}

// 自引用 promise：展开结果必须收敛（否则栈溢出，整个进程没了）。
func TestJSValue_SelfReferentialPromise(t *testing.T) {
	r := runJS(t, Args{"script": "const p = Promise.resolve(1).then(() => p); p"})
	if got := r.Text(); !strings.Contains(got, "Promise {") {
		t.Fatalf("结果 = %q", got)
	}
}
