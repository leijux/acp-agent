package agent

// 压缩终态事件：发了 CompactionStartEvent 就必须发一个终态（End 或 Abort）。
//
// 回归点：以前四条失败路径（摘要失败 / kernel 拒绝 / 镜像块失败 / 区间已被覆盖的
// noop）都是「发过 Start 就直接 return error」，订阅方（ACP 的 compaction_update）
// 于是永远停在 in_progress。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/leijux/acp-agent/internal/tools"
)

// collectEvents 挂一个收集器。
func collectEvents(a *Agent) func() []Event {
	var mu sync.Mutex
	var out []Event
	a.On(func(ev Event) {
		mu.Lock()
		out = append(out, ev)
		mu.Unlock()
	})
	return func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), out...)
	}
}

// compactionTrail 把压缩相关事件压成 "start" / "end" / "abort:phase" 序列。
func compactionTrail(events []Event) []string {
	var out []string
	for _, ev := range events {
		switch e := ev.(type) {
		case CompactionStartEvent:
			out = append(out, "start")
		case CompactionEndEvent:
			out = append(out, "end")
		case CompactionAbortEvent:
			out = append(out, "abort:"+e.Phase)
		}
	}
	return out
}

// TestCompactSummarizeFailureEmitsAbort 摘要模型报错 → start + abort(summarize)。
func TestCompactSummarizeFailureEmitsAbort(t *testing.T) {
	p := NewScriptedProvider(func(req Request) (*AssistantMessage, error) {
		if req.Purpose == PurposeSummarize {
			return nil, errors.New("摘要模型炸了")
		}
		return reply("x"), nil
	})
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	fillContext(t, a, 40)
	trail := collectEvents(a)

	if _, err := a.Compact(context.Background(), "", "manual"); err == nil {
		t.Fatal("摘要失败时 Compact 应报错")
	}

	got := compactionTrail(trail())
	want := []string{"start", "abort:summarize"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("压缩事件序列 = %v, want %v", got, want)
	}
}

// TestCompactNothingToCompactLeavesNoStart 「没东西可压」在 Start **之前** 就返回，
// 不会给订阅方留下一个 forever in_progress 的实体。
//
// （Start 之后的 noop 路径——区间已被块覆盖——已由 compact.go 的 abort 闭包
// 覆盖；它在实际流程里几乎不可达，所以这里只钉住更容易踩的这条。）
func TestCompactNothingToCompactLeavesNoStart(t *testing.T) {
	p := NewScriptedProvider(reply("x"))
	reg := tools.NewRegistry()
	a := newTestAgent(t, p, func(o *Options) {
		o.Registry = reg
		o.ContextWindow = 60_000
	})
	// 只塞一点点历史：不够 kernel 的可压单元下限。
	a.Push(TextMessage("短历史"))
	trail := collectEvents(a)

	_, err := a.Compact(context.Background(), "", "manual")
	if !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("应返回 ErrNothingToCompact，实际 %v", err)
	}
	if got := compactionTrail(trail()); len(got) != 0 {
		t.Fatalf("没有可压区间时不应发任何压缩事件（会有裸 start），实际 %v", got)
	}
}
