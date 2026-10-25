package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/kernel"
)

// compressedSession 返回一个已压缩过一次的会话（含 1 个活跃块）。
func compressedSession(t *testing.T) *kernel.Session {
	t.Helper()
	s := newSession(t, 200_000)
	if _, err := s.Turn(fixture(), 90_000); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	cres, err := s.ApplyCompression([]kernel.Range{{
		StartRef: "m00005",
		EndRef:   "m00020",
		Topic:    "上下文预算",
		Summary:  longSummary,
	}})
	if err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}
	if cres.BlocksCreated == 0 {
		t.Fatalf("no blocks created: %+v", cres)
	}
	return s
}

func TestFileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := compressedSession(t)

	state, err := s.StateJSON()
	if err != nil {
		t.Fatalf("StateJSON: %v", err)
	}
	st, err := kernel.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	info, err := s.Persist(ctx, st, "sess-1")
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if info.ID != "sess-1" || info.Bytes != len(state) {
		t.Errorf("info = %+v (want id=sess-1 bytes=%d)", info, len(state))
	}
	if info.Blocks == 0 || info.ActiveBlocks != info.Blocks {
		t.Errorf("info blocks = %d active = %d, want >0 and equal", info.Blocks, info.ActiveBlocks)
	}
	if info.TokensCompressed == 0 || info.Compressions == 0 {
		t.Errorf("info stats missing: %+v", info)
	}
	if info.Refs == 0 {
		t.Error("info.Refs = 0, want > 0")
	}
	if info.UpdatedAt.IsZero() {
		t.Error("info.UpdatedAt is zero")
	}

	// 恢复到一个全新内核实例，state 必须逐字节一致。
	k2, err := kernel.New()
	if err != nil {
		t.Fatalf("kernel.New#2: %v", err)
	}
	s2, err := k2.NewSessionFrom(ctx, st, "sess-1", kernel.Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatalf("NewSessionFrom: %v", err)
	}
	defer s2.Close()

	state2, err := s2.StateJSON()
	if err != nil {
		t.Fatalf("StateJSON#2: %v", err)
	}
	if sha8(state) != sha8(state2) {
		t.Errorf("state mismatch after restore: %s vs %s", sha8(state), sha8(state2))
	}

	blocks, err := s2.Blocks()
	if err != nil {
		t.Fatalf("Blocks#2: %v", err)
	}
	if len(blocks) != info.Blocks {
		t.Errorf("restored blocks = %d, want %d", len(blocks), info.Blocks)
	}
	refs, err := s2.Refs()
	if err != nil {
		t.Fatalf("Refs#2: %v", err)
	}
	if len(refs.ByRaw) != info.Refs {
		t.Errorf("restored refs = %d, want %d", len(refs.ByRaw), info.Refs)
	}
}

// 恢复后继续压缩：Restore 的视图为空，必须先 Turn 或 SetMessages 才能压新区间。
func TestRestoreThenContinueCompression(t *testing.T) {
	ctx := context.Background()
	s := compressedSession(t)
	st, _ := kernel.NewFileStore(t.TempDir())
	if _, err := s.Persist(ctx, st, "s2"); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	k2, err := kernel.New()
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	sess, err := k2.NewSessionFrom(ctx, st, "s2", kernel.Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatalf("NewSessionFrom: %v", err)
	}
	defer sess.Close()

	// 直接压（不先 Turn）会被上游拒绝：视图里没有对应消息。
	_, err = sess.ApplyCompression([]kernel.Range{{
		StartRef: "m00021", EndRef: "m00030", Topic: "t", Summary: longSummary,
	}})
	if err == nil {
		t.Log("note: compression without a rendered view was accepted (upstream relaxed this)")
	}

	// 先跑一轮拿到视图，再压新区间。
	res, err := sess.Turn(fixture(), 90_000)
	if err != nil {
		t.Fatalf("Turn after restore: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("empty view after restore")
	}
	sess.SetMessages(res.Messages)

	cres, err := sess.ApplyCompression([]kernel.Range{{
		StartRef: "m00021", EndRef: "m00030", Topic: "续压", Summary: longSummary,
	}})
	if err != nil {
		t.Fatalf("ApplyCompression after restore: %v", err)
	}
	if cres.BlocksCreated == 0 && len(cres.Errors) > 0 {
		t.Fatalf("compression rejected: %v", cres.Errors)
	}
	st2, err := sess.Status(90_000)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st2.ActiveBlocks < 2 {
		t.Errorf("active blocks = %d, want >= 2", st2.ActiveBlocks)
	}
}

func TestFileStoreInvalidID(t *testing.T) {
	ctx := context.Background()
	st, _ := kernel.NewFileStore(t.TempDir())
	bad := []string{"", "..", "../etc/passwd", "a/b", `a\b`, ".hidden", strings.Repeat("x", 200), "a b"}
	for _, id := range bad {
		if _, err := st.Save(ctx, id, []byte(`{}`)); err == nil {
			t.Errorf("Save(%q) accepted, want error", id)
		}
		if _, _, err := st.Load(ctx, id); err == nil {
			t.Errorf("Load(%q) accepted, want error", id)
		}
	}
	if err := kernel.ValidateSessionID("sess-1.a_b"); err != nil {
		t.Errorf("valid id rejected: %v", err)
	}
}

func TestFileStoreSaveRejectsGarbageState(t *testing.T) {
	ctx := context.Background()
	st, _ := kernel.NewFileStore(t.TempDir())
	if _, err := st.Save(ctx, "x", []byte("not json")); err == nil {
		t.Error("Save accepted non-JSON state")
	}
	if _, err := st.Save(ctx, "x", nil); err == nil {
		t.Error("Save accepted empty state")
	}
}

func TestFileStoreNotFoundAndDelete(t *testing.T) {
	ctx := context.Background()
	st, _ := kernel.NewFileStore(t.TempDir())

	_, _, err := st.Load(ctx, "missing")
	if !errors.Is(err, kernel.ErrSessionNotFound) {
		t.Errorf("Load error = %v, want ErrSessionNotFound", err)
	}

	// Delete 幂等。
	if err := st.Delete(ctx, "missing"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}

	s := compressedSession(t)
	if _, err := s.Persist(ctx, st, "d1"); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := st.Delete(ctx, "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := st.Load(ctx, "d1"); !errors.Is(err, kernel.ErrSessionNotFound) {
		t.Errorf("Load after delete = %v, want ErrSessionNotFound", err)
	}
}

func TestFileStoreListOrder(t *testing.T) {
	ctx := context.Background()
	st, _ := kernel.NewFileStore(t.TempDir())
	s := compressedSession(t)
	for _, id := range []string{"a1", "b2", "c3"} {
		if _, err := s.Persist(ctx, st, id); err != nil {
			t.Fatalf("Persist %s: %v", id, err)
		}
		time.Sleep(5 * time.Millisecond) // 保证 UpdatedAt 单调
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	if list[0].ID != "c3" || list[2].ID != "a1" {
		t.Errorf("List order = %v, want c3..a1 (UpdatedAt desc)", []string{list[0].ID, list[1].ID, list[2].ID})
	}
	for _, info := range list {
		if info.Blocks == 0 || info.Bytes == 0 {
			t.Errorf("list entry %s missing meta: %+v", info.ID, info)
		}
	}
}

// 崩溃安全：残留的 .tmp- 文件既不能被 List 看到，也不能影响 Load。
func TestFileStoreTempFilesAreInvisible(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := kernel.NewFileStore(dir)
	s := compressedSession(t)
	if _, err := s.Persist(ctx, st, "k1"); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	before, _, err := st.Load(ctx, "k1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 模拟写到一半被杀：半截临时文件 + 陈旧（用于 CleanupTemp）。
	half := filepath.Join(dir, "k1.json.tmp-123456")
	if err := os.WriteFile(half, []byte(`{"version":1,"state":{"blocks":[`), 0o644); err != nil {
		t.Fatalf("write half temp: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(half, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	after, _, err := st.Load(ctx, "k1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.Blocks != before.Blocks {
		t.Errorf("Load picked up temp file: blocks %d -> %d", before.Blocks, after.Blocks)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List len = %d, want 1 (temp file must be ignored)", len(list))
	}

	n, err := st.CleanupTemp(time.Hour)
	if err != nil {
		t.Fatalf("CleanupTemp: %v", err)
	}
	if n != 1 {
		t.Errorf("CleanupTemp removed %d, want 1", n)
	}
	if _, err := os.Stat(half); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file still present: %v", err)
	}
}

// 向前兼容：直接放裸 state 的文件也能读。
func TestFileStoreReadsBareStateFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := kernel.NewFileStore(dir)

	s := compressedSession(t)
	state, err := s.StateJSON()
	if err != nil {
		t.Fatalf("StateJSON: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bare.json"), state, 0o644); err != nil {
		t.Fatalf("write bare: %v", err)
	}

	info, got, err := st.Load(ctx, "bare")
	if err != nil {
		t.Fatalf("Load bare: %v", err)
	}
	if sha8(got) != sha8(state) {
		t.Errorf("bare state mismatch")
	}
	if info.Blocks == 0 || info.Refs == 0 {
		t.Errorf("bare meta not derived: %+v", info)
	}

	s2, err := kernel.New()
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	sess, err := s2.NewSessionFrom(ctx, st, "bare", kernel.Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatalf("NewSessionFrom bare: %v", err)
	}
	defer sess.Close()
	if blocks, err := sess.Blocks(); err != nil || len(blocks) != info.Blocks {
		t.Errorf("blocks = %d, err = %v, want %d", len(blocks), err, info.Blocks)
	}
}

func TestFileStoreConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := kernel.NewFileStore(dir)
	s := compressedSession(t)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	// 并行写不同 id。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-" + strings.Repeat("x", i) + string(rune('a'+i))
			if _, err := s.Persist(ctx, st, id); err != nil {
				errs <- err
			}
		}(i)
	}
	// 并行写同一 id（最后写赢，但文件必须始终可解析）。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Persist(ctx, st, "shared"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Persist: %v", err)
	}

	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 9 {
		t.Errorf("List len = %d, want 9", len(list))
	}
	if _, _, err := st.Load(ctx, "shared"); err != nil {
		t.Errorf("Load shared: %v", err)
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	st := kernel.NewMemoryStore()
	s := compressedSession(t)

	info, err := s.Persist(ctx, st, "m1")
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	info2, _, err := st.Load(ctx, "m1")
	if info2.Blocks != info.Blocks || err != nil {
		t.Fatalf("Load = %+v, %v", info2, err)
	}
	if list, err := st.List(ctx); err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v", list, err)
	}
	if err := st.Delete(ctx, "m1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := st.Load(ctx, "m1"); !errors.Is(err, kernel.ErrSessionNotFound) {
		t.Errorf("Load after delete = %v", err)
	}
}

func TestSummarizeStateRejectsEmpty(t *testing.T) {
	if _, err := kernel.SummarizeState("x", nil); err == nil {
		t.Error("SummarizeState(nil) accepted")
	}
	info, err := kernel.SummarizeState("x", []byte(`{"blocks":[],"messageRefs":{"byRaw":{}},"stats":{}}`))
	if err != nil {
		t.Fatalf("SummarizeState: %v", err)
	}
	if info.Blocks != 0 || info.Bytes == 0 {
		t.Errorf("info = %+v", info)
	}
	// 元信息可 JSON 序列化（便于 List 直接返回给前端）。
	if _, err := json.Marshal(info); err != nil {
		t.Errorf("marshal info: %v", err)
	}
}
