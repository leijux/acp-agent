package acp

// 压缩终态：compaction_update 实体必须能被关掉。
//
// 回归点：之前客户端只会在 compactionEnd 收到 completed；任何失败路径
// （摘要失败 / kernel 拒绝 / 区间已被覆盖）都是「发过 in_progress 就没了」，
// 于是客户端永远显示"压缩中"。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

// compactionHarness 起一对 conn，把 client 侧收到的 compaction_update 收集起来。
type compactionHarness struct {
	u      *updates
	close  func()
	notifs func() []map[string]any
}

func newCompactionHarness(t *testing.T, caps ClientCapabilities) *compactionHarness {
	t.Helper()
	cr, sr := net.Pipe()
	clientConn := NewConn(cr, cr, nil)

	var mu sync.Mutex
	var got []map[string]any
	clientConn.HandleNotify(MethodSessionUpdate, func(_ context.Context, params json.RawMessage) {
		var p SessionUpdateParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("解析 session/update 失败: %v", err)
			return
		}
		raw, _ := json.Marshal(p.Update)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("解析 update 失败: %v", err)
			return
		}
		if m["sessionUpdate"] != SessionUpdateCompaction {
			return
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	go clientConn.Serve()

	serverConn := NewConn(sr, sr, nil)
	go serverConn.Serve()

	h := &compactionHarness{
		u: newUpdates(serverConn, "s1", caps),
		close: func() {
			_ = clientConn.Close()
			_ = serverConn.Close()
		},
		notifs: func() []map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return append([]map[string]any(nil), got...)
		},
	}
	t.Cleanup(h.close)
	return h
}

// waitFor 等到收齐 n 条 compaction_update。
func (h *compactionHarness) waitFor(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.notifs(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("只收到 %d 条 compaction_update（期望 %d）：%v", len(h.notifs()), n, h.notifs())
	return nil
}

func compactionCaps() ClientCapabilities {
	return ClientCapabilities{Session: &SessionCapabilities{Compaction: map[string]any{}}}
}

// TestCompactionAbortClosesInProgress 失败终态必须复用同一个 compactionId。
func TestCompactionAbortClosesInProgress(t *testing.T) {
	h := newCompactionHarness(t, compactionCaps())

	h.u.Handle(agent.CompactionStartEvent{Reason: "threshold", TokensBefore: 90_000})
	h.u.Handle(agent.CompactionAbortEvent{
		Reason: "threshold", Phase: "summarize", Err: errors.New("摘要模型炸了"),
	})

	got := h.waitFor(t, 2)
	start, abort := got[0], got[1]
	if start["status"] != "in_progress" || abort["status"] != "failed" {
		t.Fatalf("状态序列不对: %v → %v", start["status"], abort["status"])
	}
	if start["compactionId"] != abort["compactionId"] {
		t.Fatalf("终态用了另一个 id: %v vs %v", start["compactionId"], abort["compactionId"])
	}
	msg, _ := abort["error"].(string)
	if msg == "" || !strings.Contains(msg, "摘要模型炸了") || !strings.Contains(msg, "summarize") {
		t.Fatalf("error 应带上失败原因与阶段，实际 %q", abort["error"])
	}
}

// TestCompactionEndReusesStartID 正常完成也必须复用 id（之前靠 seq 反推）。
func TestCompactionEndReusesStartID(t *testing.T) {
	h := newCompactionHarness(t, compactionCaps())

	h.u.Handle(agent.CompactionStartEvent{Reason: "manual", TokensBefore: 100_000})
	h.u.Handle(agent.CompactionEndEvent{Summary: "摘要", TokensBefore: 100_000, TokensAfter: 40_000})

	got := h.waitFor(t, 2)
	if got[1]["status"] != "completed" {
		t.Fatalf("应为 completed: %v", got[1]["status"])
	}
	if got[0]["compactionId"] != got[1]["compactionId"] {
		t.Fatalf("completed 用了另一个 id: %v vs %v", got[0]["compactionId"], got[1]["compactionId"])
	}
	// 连续两次压缩要用不同 id。
	h.u.Handle(agent.CompactionStartEvent{Reason: "manual"})
	got = h.waitFor(t, 3)
	if got[2]["compactionId"] == got[0]["compactionId"] {
		t.Fatalf("第二次压缩复用了旧 id: %v", got[2]["compactionId"])
	}
}

// TestCompactionAbortWithoutStartNoEntity 没有在飞的实体时不要凭空发 failed
// （客户端会看到一个从未开始过的压缩）。
func TestCompactionAbortWithoutStartNoEntity(t *testing.T) {
	h := newCompactionHarness(t, compactionCaps())
	h.u.Handle(agent.CompactionAbortEvent{Phase: "apply", Err: errors.New("炸了")})
	time.Sleep(100 * time.Millisecond)
	if got := h.notifs(); len(got) != 0 {
		t.Fatalf("不应发出 compaction_update，实际 %v", got)
	}
}

// TestCompactionSkipDoesNotOpenEntity 没东西可压时 Start 根本没发，
// skip 事件不能凭空造一个 in_progress 出来。
func TestCompactionSkipDoesNotOpenEntity(t *testing.T) {
	h := newCompactionHarness(t, compactionCaps())
	h.u.Handle(agent.CompactionSkipEvent{Reason: "threshold", Usage: 0.93})
	time.Sleep(100 * time.Millisecond)
	if got := h.notifs(); len(got) != 0 {
		t.Fatalf("skip 不应产生 compaction_update，实际 %v", got)
	}
}

// TestCompactionFallbackWithoutCaps 客户端不支持 compaction 时，走 notice/消息，
// 不再发 compaction_update 实体。
func TestCompactionFallbackWithoutCaps(t *testing.T) {
	h := newCompactionHarness(t, ClientCapabilities{})
	h.u.Handle(agent.CompactionStartEvent{Reason: "manual", TokensBefore: 1})
	h.u.Handle(agent.CompactionAbortEvent{Phase: "apply", Err: errors.New("炸了")})
	time.Sleep(100 * time.Millisecond)
	if got := h.notifs(); len(got) != 0 {
		t.Fatalf("不支持 compaction 时不该发 compaction_update，实际 %v", got)
	}
}
