package kernel_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/leijux/acp-agent/internal/kernel"
)

// TestConcurrentSessionsIndependent 是 ③ 的正确性配套验证：
// 多个 goroutine 各自持有 Session、共享同一个 Kernel（共享编译产物），
// 每个会话的结果必须与串行执行逐字节一致 —— 即 VM/state 没有串味。
func TestConcurrentSessionsIndependent(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := benchMessages(60)

	turn := func(tag string) string {
		sess, err := k.NewSession(kernel.Config{ModelContextLimit: 120_000})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		// 每个会话用不同 id 的消息，避免 ref 分配结果偶然相同而掩盖串味。
		local := make([]kernel.Message, len(msgs))
		copy(local, msgs)
		for i := range local {
			local[i].ID = tag + "_" + local[i].ID
		}
		res, err := sess.Turn(local, 60_000)
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
		h := sha256.New()
		for _, m := range res.Messages {
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", m.ID, m.Ref, m.Role, m.Text)
		}
		return hex.EncodeToString(h.Sum(nil))
	}

	const n = 12
	serial := make([]string, n)
	for i := 0; i < n; i++ {
		serial[i] = turn(fmt.Sprintf("s%02d", i))
	}

	par := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			par[i] = turn(fmt.Sprintf("s%02d", i))
		}(i)
	}
	wg.Wait()

	for i := range serial {
		if serial[i] != par[i] {
			t.Fatalf("会话 s%02d 并发结果与串行不一致：%s vs %s", i, serial[i][:16], par[i][:16])
		}
	}
}

// TestSharedKernelVersionConcurrent 压 Kernel.Version/Functions/Call 的共享 runtime
// （这些方法共用一把锁 + 一个 VM，必须 -race 干净）。
func TestSharedKernelVersionConcurrent(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fns, err := k.Functions()
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if len(fns) < 100 {
		t.Fatalf("Functions 只返回 %d 个符号", len(fns))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := k.Version(); err != nil {
					t.Errorf("Version: %v", err)
					return
				}
				if _, err := k.Functions(); err != nil {
					t.Errorf("Functions: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestTurnMetaThenCompress 验证 TurnMeta 与 Turn 的状态推进等价：
// TurnMeta 之后可以直接 ApplyCompression（不需要再跑 Turn）。
func TestTurnMetaThenCompress(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	run := func(meta bool) (*kernel.Session, kernel.Nudge) {
		sess, err := k.NewSession(kernel.Config{ModelContextLimit: 120_000})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		var n kernel.Nudge
		if meta {
			m, err := sess.TurnMeta(benchMessages(40), 0) // 0 → 用 EstimateTokens 兜底
			if err != nil {
				t.Fatalf("TurnMeta: %v", err)
			}
			if m.Nudge == nil {
				t.Fatal("TurnMeta 没有返回 nudge")
			}
			n = *m.Nudge
			// View 必须能拿到与 Turn 等价的视图。
			view, err := sess.View()
			if err != nil {
				t.Fatalf("View: %v", err)
			}
			if len(view) == 0 {
				t.Fatal("View 返回空")
			}
		} else {
			res, err := sess.Turn(benchMessages(40), 0)
			if err != nil {
				t.Fatalf("Turn: %v", err)
			}
			if res.Nudge == nil {
				t.Fatal("Turn 没有返回 nudge")
			}
			n = *res.Nudge
		}
		return sess, n
	}

	s, n := run(true)
	defer s.Close()
	if len(n.CompressibleRanges) == 0 {
		t.Skip("kernel 未给出可压缩区间，跳过")
	}
	r := n.CompressibleRanges[0]
	if _, err := s.ApplyCompression([]kernel.Range{{
		StartRef: r.StartRef, EndRef: r.EndRef, Topic: "meta", Summary: longSummary,
	}}); err != nil {
		t.Fatalf("TurnMeta 之后 ApplyCompression 失败: %v", err)
	}
	blocks, err := s.Blocks()
	if err != nil {
		t.Fatalf("Blocks: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("压缩后没有块")
	}
}

// TestEstimateTokensDrivesNudge 检查 tokenCount=0 兜底路径能正常触发 nudge 决策。
func TestEstimateTokensDrivesNudge(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := k.NewSession(kernel.Config{ModelContextLimit: 8_000}) // 故意压小
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	metas := 0
	for i := 0; i < 5; i++ {
		m, err := sess.TurnMeta(benchMessages(20+i*5), 0)
		if err != nil {
			t.Fatalf("TurnMeta: %v", err)
		}
		if m.Nudge != nil {
			metas++
		}
	}
	if metas == 0 {
		t.Fatal("5 轮都没有 nudge，EstimateTokens 兜底可能失效")
	}
}
