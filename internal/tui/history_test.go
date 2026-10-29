package tui

import (
	"fmt"
	"testing"
)

func TestHistoryPushFilters(t *testing.T) {
	var h History
	h.Push("")
	h.Push("   ")      // 纯空白：不记
	h.Push("  ls -la") // 以空格开头：不记（bash 惯例）
	h.Push("echo hi")  // 记
	if h.Len() != 1 {
		t.Fatalf("只该记 1 条，实际 %d: %+v", h.Len(), h.items)
	}
	if got, _ := h.At(0); got != "echo hi" {
		t.Fatalf("内容不对: %q", got)
	}
}

func TestHistoryDedupMovesToLatest(t *testing.T) {
	var h History
	h.Push("a")
	h.Push("b")
	h.Push("a") // 重复：移到最新
	if h.Len() != 2 {
		t.Fatalf("重复项应合并，实际 %d: %+v", h.Len(), h.items)
	}
	if got, _ := h.At(0); got != "b" {
		t.Fatalf("旧的 a 应被摘掉，首位应为 b: %q", got)
	}
	if got, _ := h.At(1); got != "a" {
		t.Fatalf("a 应在最新位置: %q", got)
	}
}

func TestHistoryCap(t *testing.T) {
	var h History
	for i := 0; i < historyMax+50; i++ {
		h.Push(fmt.Sprintf("cmd-%d", i))
	}
	if h.Len() != historyMax {
		t.Fatalf("上限应为 %d，实际 %d", historyMax, h.Len())
	}
	// 最旧的被丢掉，最新的一定在
	if got, _ := h.At(0); got != "cmd-50" {
		t.Fatalf("首位应为 cmd-50，实际 %q", got)
	}
	if got, _ := h.At(h.Len() - 1); got != fmt.Sprintf("cmd-%d", historyMax+49) {
		t.Fatalf("末位应为最后一条，实际 %q", got)
	}
}

func TestHistoryPrevNext(t *testing.T) {
	var h History
	h.Push("git status")
	h.Push("ls")
	h.Push("git commit -m x")

	// 从「未进历史」开始：取最新一条
	got, pos, ok := h.Prev(-1, "")
	if !ok || got != "git commit -m x" || pos != 2 {
		t.Fatalf("Prev(-1) = %q,%d,%v", got, pos, ok)
	}
	// 继续向前
	got, pos, ok = h.Prev(pos, "")
	if !ok || got != "ls" || pos != 1 {
		t.Fatalf("Prev(2) = %q,%d,%v", got, pos, ok)
	}
	// 向后
	got, pos, ok = h.Next(pos, "")
	if !ok || got != "git commit -m x" || pos != 2 {
		t.Fatalf("Next(1) = %q,%d,%v", got, pos, ok)
	}
	// 已在最新：调用方应回草稿
	if _, _, ok := h.Next(pos, ""); ok {
		t.Fatal("已是最新一条时 Next 应返回 false")
	}
	// 再往前到顶：ok=false，pos 不变
	pos = 0
	if _, newPos, ok := h.Prev(pos, ""); ok || newPos != 0 {
		t.Fatal("到顶后 Prev 应返回 false 且不改变 pos")
	}
}

func TestHistoryPrefixFilter(t *testing.T) {
	var h History
	h.Push("git status")
	h.Push("ls -la")
	h.Push("git commit -m x")
	h.Push("echo hi")

	// 只翻以 "git " 开头的
	got, pos, ok := h.Prev(-1, "git ")
	if !ok || got != "git commit -m x" {
		t.Fatalf("前缀过滤失败: %q,%d,%v", got, pos, ok)
	}
	got, pos, ok = h.Prev(pos, "git ")
	if !ok || got != "git status" {
		t.Fatalf("前缀过滤第二条失败: %q,%d,%v", got, pos, ok)
	}
	if _, _, ok := h.Prev(pos, "git "); ok {
		t.Fatal("没有更多匹配时应返回 false")
	}
	// 向后回到匹配项
	got, _, ok = h.Next(-1+1, "git ")
	if !ok || got != "git commit -m x" {
		t.Fatalf("向后前缀过滤失败: %q,%v", got, ok)
	}

	// 没有匹配项：不动
	if _, _, ok := h.Prev(-1, "npm "); ok {
		t.Fatal("无匹配前缀时应返回 false")
	}
}

func TestHistoryNextWithoutNav(t *testing.T) {
	var h History
	h.Push("a")
	if _, _, ok := h.Next(-1, ""); ok {
		t.Fatal("pos=-1（未进历史）时 Next 应返回 false")
	}
}
