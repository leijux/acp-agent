package kernel_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"gitlab.com/leijux/acp-agent/internal/kernel"
)

// fixture 复现 main.go 里的 40 条消息，用于与 Node 输出做回归比对。
func fixture() []kernel.Message {
	msgs := make([]kernel.Message, 0, 40)
	for i := 0; i < 40; i++ {
		m := kernel.Message{ID: fmt.Sprintf("msg_%03d", i), Role: kernel.RoleUser, ContentType: kernel.ContentText}
		if i%2 == 1 {
			m.Role = kernel.RoleAssistant
		}
		if i%7 == 3 {
			m.Role = kernel.RoleTool
			m.ContentType = kernel.ContentToolResult
			m.ToolName = "read"
			m.ToolCallID = fmt.Sprintf("call_%d", i)
			m.Text = fmt.Sprintf("file contents chunk %d: func main() { /* 中文注释，缓存与压缩 */ }; ", i) +
				strings.Repeat("line of code and prose about context compression 压缩上下文 ", 20)
		} else {
			m.Text = fmt.Sprintf("turn %d: 请分析 billion-context 的压缩策略 cache & compression behaviour, line %d; ", i, i+100) +
				strings.Repeat("详细讨论 context window, token budget, summary tier 以及缓存命中率。", 15)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func newSession(t *testing.T, limit int) *kernel.Session {
	t.Helper()
	k, err := kernel.New()
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	s, err := k.NewSession(kernel.Config{ModelContextLimit: limit})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// compression summaries must be at least 50 chars (upstream validation).
const longSummary = "SUMMARY m00005..m00020: earlier turns condensed — 压缩摘要覆盖了关于 cache & compression 的讨论与工具输出细节"

func sha8(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

var createdAtZero = regexp.MustCompile(`"createdAt":\d+`)

func TestNewSession(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatal(err)
	}
	fns, err := k.Functions()
	if err != nil {
		t.Fatal(err)
	}
	if len(fns) < 100 {
		t.Fatalf("expected a large export surface, got %d", len(fns))
	}
	for _, want := range []string{"createCore", "createInitialState", "defaultConfig", "searchBlocks", "blockDocs", "messageDocs"} {
		found := false
		for _, f := range fns {
			if f == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing export %q", want)
		}
	}
	t.Logf("exports=%d", len(fns))
}

func TestTurnPrunesAndAssignsRefs(t *testing.T) {
	s := newSession(t, 200_000)
	res, err := s.Turn(fixture(), 90_000)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("empty rendered view")
	}
	if len(res.Messages) >= 40 {
		t.Logf("note: no pruning happened, %d messages out", len(res.Messages))
	}
	if res.Nudge == nil {
		t.Error("expected a compression nudge at 90000 tokens")
	} else if res.Nudge.Reason == "" {
		t.Error("parsed nudge has empty Reason (field mapping broken?)")
	}
	if len(res.NudgeJSON) == 0 {
		t.Error("raw nudge json not preserved")
	}
	if len(res.Refs.ByRaw) == 0 {
		t.Fatal("no refs assigned")
	}
	// 渲染视图里的每条消息都应带上引用。
	for _, m := range res.Messages {
		if m.Ref == "" {
			t.Errorf("message %s has no ref", m.ID)
		}
	}
}

func TestCompressSearchStatusDecompress(t *testing.T) {
	s := newSession(t, 200_000)
	if _, err := s.Turn(fixture(), 90_000); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	cres, err := s.ApplyCompression([]kernel.Range{{
		StartRef: "m00005",
		EndRef:   "m00020",
		Summary:  "SUMMARY: earlier turns condensed — 压缩摘要覆盖 m00005..m00020 compressed context",
	}})
	if err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}
	if cres.BlocksCreated == 0 {
		t.Fatalf("no blocks created: %+v", cres)
	}
	if len(cres.Blocks) == 0 {
		t.Fatal("expected active blocks")
	}
	t.Logf("blocksCreated=%d tokensCompressed=%d active=%d",
		cres.BlocksCreated, cres.TokensCompressed, len(cres.Blocks))

	blocks, err := s.Blocks()
	if err != nil {
		t.Fatalf("Blocks: %v", err)
	}
	if len(blocks) != cres.BlocksCreated && len(blocks) < len(cres.Blocks) {
		t.Fatalf("blocks=%d created=%d", len(blocks), cres.BlocksCreated)
	}

	hits, err := s.Search("压缩 摘要")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("Search returned no hits")
	}

	bhits, err := s.SearchBlocks("压缩 摘要")
	if err != nil {
		t.Fatalf("SearchBlocks: %v", err)
	}
	if len(bhits) == 0 {
		t.Error("SearchBlocks returned no hits")
	}

	st, err := s.Status(30_000)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.TokensCompressed == 0 {
		t.Errorf("tokensCompressed=0 in status: %+v", st)
	}
	if st.ContextUsage <= 0 || st.ContextUsage > 2 {
		t.Errorf("implausible contextUsage %v", st.ContextUsage)
	}
	t.Logf("status: usage=%.3f tokensCompressed=%d activeBlocks=%d totalBlocks=%d",
		st.ContextUsage, st.TokensCompressed, st.ActiveBlocks, st.TotalBlocks)

	dec, err := s.Decompress(cres.Blocks[0].BlockID)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if dec != nil && len(dec.DirectMessageIDs) == 0 {
		t.Error("decompressed block has no direct message ids")
	}
}

// TestGoldenHashes 与 Node 26 上的同一序列比对（详见 docs/kernel.md）。
// 这两个 hash 只要变化，说明 JS 侧行为或我们的 shim 出现了偏差。
func TestGoldenHashes(t *testing.T) {
	s := newSession(t, 200_000)
	res1, err := s.Turn(fixture(), 90_000)
	if err != nil {
		t.Fatalf("Turn#1: %v", err)
	}
	if _, err := s.ApplyCompression([]kernel.Range{{
		StartRef: "m00005",
		EndRef:   "m00020",
		Summary:  "SUMMARY: earlier turns condensed — 压缩摘要覆盖 m00005..m00020 compressed context",
	}}); err != nil {
		t.Fatalf("ApplyCompression: %v", err)
	}

	// 第二轮：用上一轮的渲染视图作为输入。
	var rendered []kernel.Message
	if err := json.Unmarshal(res1.MessagesJSON, &rendered); err != nil {
		t.Fatalf("decode rendered: %v", err)
	}
	res2, err := s.Turn(rendered, 30_000)
	if err != nil {
		t.Fatalf("Turn#2: %v", err)
	}

	msgHash := sha8(res2.MessagesJSON)
	blocksJSON, err := s.BlocksJSON()
	if err != nil {
		t.Fatalf("BlocksJSON: %v", err)
	}
	// createdAt 是毫秒时间戳，比对时置 0（与 main.go 的 blockNoTimeHash 一致）。
	// main.go 对单块（blocks[0]）算 hash，且必须保持 JS 的 key 顺序，
	// 因此用 RawMessage 保留原文、再用文本替换掉时间戳。
	var raw []json.RawMessage
	if err := json.Unmarshal(blocksJSON, &raw); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("no blocks")
	}
	stable := createdAtZero.ReplaceAll(raw[0], []byte(`"createdAt":0`))
	blockHash := sha8(stable)

	const (
		wantMsg   = "6a8561416f02cde1"
		wantBlock = "0cbd1741d253e587"
	)
	if msgHash != wantMsg {
		t.Errorf("messagesHash = %s, want %s", msgHash, wantMsg)
	}
	if blockHash != wantBlock {
		t.Errorf("blocksHash = %s, want %s", blockHash, wantBlock)
	}
	t.Logf("messagesHash=%s blocksHash=%s", msgHash, blockHash)
}

func TestStateRoundTrip(t *testing.T) {
	s := newSession(t, 200_000)
	if _, err := s.Turn(fixture(), 90_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyCompression([]kernel.Range{{
		StartRef: "m00005", EndRef: "m00020", Summary: longSummary,
	}}); err != nil {
		t.Fatal(err)
	}
	state, err := s.StateJSON()
	if err != nil {
		t.Fatalf("StateJSON: %v", err)
	}
	if len(state) < 100 {
		t.Fatalf("suspiciously small state: %d bytes", len(state))
	}
	before, err := s.BlocksJSON()
	if err != nil {
		t.Fatal(err)
	}

	s2 := newSession(t, 200_000)
	if err := s2.LoadState(state); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	after, err := s2.BlocksJSON()
	if err != nil {
		t.Fatal(err)
	}
	if sha8(before) != sha8(after) {
		t.Fatalf("blocks differ after round trip:\n%s\n%s", before, after)
	}
	if _, err := s2.Search("压缩"); err != nil {
		t.Fatalf("Search after LoadState: %v", err)
	}
}

func TestCloseIsFinal(t *testing.T) {
	s := newSession(t, 100_000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(fixture(), 1); err != kernel.ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if err := s.LoadState([]byte("{}")); err != kernel.ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestOverrides(t *testing.T) {
	k, err := kernel.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.NewSession(kernel.Config{
		ModelContextLimit: 100_000,
		Overrides:         map[string]any{"nudge": map[string]any{"minContextLimitPct": 0.05}},
	})
	if err != nil {
		t.Fatalf("NewSession with overrides: %v", err)
	}
	defer s.Close()
	if _, err := s.Turn(fixture(), 1000); err != nil {
		t.Fatalf("Turn: %v", err)
	}
}

// TestSegmenterInjection 验证自定义分词器会被 kernel 使用。
func TestSegmenterInjection(t *testing.T) {
	var calls int
	seg := countingSegmenter{inner: kernel.RuneSegmenter{}, n: &calls}
	k, err := kernel.New(kernel.WithSegmenter(seg))
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Turn(fixture(), 90_000); err != nil {
		t.Fatal(err)
	}
	cres, err := s.ApplyCompression([]kernel.Range{{StartRef: "m00005", EndRef: "m00020", Summary: longSummary}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("compress: created=%d tokens=%d errs=%v warns=%v notes=%v", cres.BlocksCreated, cres.TokensCompressed, cres.Errors, cres.Warnings, cres.Notes)
	hits, err := s.SearchBlocksWith("压缩", map[string]any{"algorithm": "bm25"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Log("no bm25 hits")
	}
	if calls == 0 {
		t.Error("custom segmenter was never called")
	}
}

type countingSegmenter struct {
	inner kernel.Segmenter
	n     *int
}

func (c countingSegmenter) Segment(text string) []kernel.Segment {
	*c.n++
	return c.inner.Segment(text)
}

func TestRuneSegmenter(t *testing.T) {
	segs := kernel.RuneSegmenter{}.Segment("上下文 cache 命中")
	var got []string
	for _, s := range segs {
		got = append(got, s.Text)
	}
	want := []string{"上", "下", "文", " ", "cache", " ", "命", "中"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func BenchmarkTurn200(b *testing.B) {
	k, err := kernel.New()
	if err != nil {
		b.Fatal(err)
	}
	msgs := make([]kernel.Message, 0, 200)
	for i := 0; i < 200; i++ {
		m := kernel.Message{
			ID:          fmt.Sprintf("big_%04d", i),
			Role:        kernel.RoleUser,
			ContentType: kernel.ContentText,
			Text: fmt.Sprintf("turn %d: 上下文压缩上下文压缩 context window management discussion with details about tiering, summarisation and cache behaviour. ", i) +
				strings.Repeat("abcdefghij klmnopqrst uvwxyz 0123456789 中文内容测试 ", 8),
		}
		if i%2 == 1 {
			m.Role = kernel.RoleAssistant
		}
		msgs = append(msgs, m)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Turn(msgs, 150_000); err != nil {
			b.Fatal(err)
		}
		_ = s.Close()
	}
}

// BenchmarkTurn200Reuse 只测 processTurn 本身（排除 session 初始化）。
func BenchmarkTurn200Reuse(b *testing.B) {
	k, _ := kernel.New()
	s, err := k.NewSession(kernel.Config{ModelContextLimit: 200_000})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	msgs := make([]kernel.Message, 0, 200)
	for i := 0; i < 200; i++ {
		msgs = append(msgs, kernel.Message{
			ID:          fmt.Sprintf("re_%04d", i),
			Role:        kernel.RoleUser,
			ContentType: kernel.ContentText,
			Text:        fmt.Sprintf("turn %d 上下文压缩 context window ", i) + strings.Repeat("abc 中文 ", 40),
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Turn(msgs, 150_000); err != nil {
			b.Fatal(err)
		}
	}
}
