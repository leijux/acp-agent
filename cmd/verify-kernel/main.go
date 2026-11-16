// verify-kernel 是全链路自检：编译并跑仓库里嵌入的那份 kernel bundle，
// 走过 Turn / ApplyCompression / Search / Status / Decompress 全流程，最后
// 打印与上游 Node 比对用的三个 hash。
//
// 它现在走 internal/kernel 的公开 API（以前是手写一套 goja 宿主绑定）。这样：
//   - 校验的是**真实使用路径**（wrapper + createCore + core 方法），而不是另抄一份；
//   - 绑定代码只有一份，不会这边改了那边忘（历史上两份 shim 就漂移过）。
//
// 想跑别的 bundle：KERNEL_BUNDLE=/path/to/kernel.dist.cjs。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/leijux/acp-agent/internal/kernel"
)

const (
	msgCount  = 40
	benchMsgs = 200
	limit     = 200_000
	summary   = "SUMMARY: earlier turns condensed — 压缩摘要覆盖 m00005..m00020 compressed context"
)

func main() {
	t0 := time.Now()

	k, err := newKernel()
	if err != nil {
		fail("kernel init", err)
	}
	fmt.Printf("1) bundle loaded OK in %v\n", time.Since(t0).Round(time.Millisecond))

	names, err := k.Functions()
	if err != nil {
		fail("Functions", err)
	}
	fmt.Printf("2) exports: %d symbols\n", len(names))

	sess, err := k.NewSession(kernel.Config{ModelContextLimit: limit})
	if err != nil {
		fail("NewSession", err)
	}
	defer sess.Close()

	t1 := time.Now()
	res, err := sess.Turn(fixture(), 90_000)
	if err != nil {
		fail("processTurn", err)
	}
	fmt.Printf("3) processTurn OK in %v: messages=%d, nudge=%v\n",
		time.Since(t1).Round(time.Millisecond), len(res.Messages), res.Nudge != nil)

	refs, err := sess.Refs()
	if err != nil {
		fail("Refs", err)
	}
	fmt.Printf("   refs assigned: %d (sample: %s)\n", len(refs.ByRaw), refs.ByRaw["msg_000"])

	t2 := time.Now()
	ac, err := sess.ApplyCompression([]kernel.Range{{
		StartRef: "m00005",
		EndRef:   "m00020",
		Summary:  summary,
	}})
	if err != nil {
		fail("applyCompression", err)
	}
	fmt.Printf("4) applyCompression OK in %v: blocks=%d created=%d compressedTokens=%d\n",
		time.Since(t2).Round(time.Millisecond), len(ac.Blocks), ac.BlocksCreated, ac.TokensCompressed)
	fmt.Printf("   errors=%v warnings=%v notes=%v\n", ac.Errors, ac.Warnings, ac.Notes)

	// 第二轮：用上一轮的渲染视图作为输入（压缩块已被折叠成摘要）。
	res2, err := sess.Turn(res.Messages, 30_000)
	if err != nil {
		fail("processTurn#2", err)
	}
	fmt.Printf("5) processTurn#2 OK: messages=%d (folded)\n", len(res2.Messages))

	t3 := time.Now()
	hits, err := sess.Search("压缩 摘要")
	if err != nil {
		fail("search", err)
	}
	fmt.Printf("6) search OK in %v: hits=%d\n", time.Since(t3).Round(time.Millisecond), len(hits))

	st, err := sess.Status(30_000)
	if err != nil {
		fail("status", err)
	}
	fmt.Printf("7) status OK: contextUsage=%v tokensCompressed=%v activeBlocks=%v totalBlocks=%v\n",
		st.ContextUsage, st.TokensCompressed, st.ActiveBlocks, st.TotalBlocks)

	if len(ac.Blocks) > 0 {
		bid := ac.Blocks[0].BlockID
		blk, err := sess.Decompress(bid)
		switch {
		case err != nil:
			fmt.Printf("decompress ERROR: %v\n", err)
		case blk == nil:
			fmt.Printf("8) decompress(%s) -> nil (inactive)\n", bid)
		default:
			fmt.Printf("8) decompress(%s) OK: summary=%d chars, directMessageIds=%d\n",
				bid, len(blk.Summary), len(blk.DirectMessageIDs))
		}
	}

	// 200 条消息的一轮：确认耗时仍在可接受范围（历史数据见 docs/kernel.md）。
	bench, err := k.NewSession(kernel.Config{ModelContextLimit: limit})
	if err != nil {
		fail("NewSession(bench)", err)
	}
	defer bench.Close()
	tb := time.Now()
	bres, err := bench.Turn(benchFixture(), 150_000)
	if err != nil {
		fmt.Printf("bench processTurn ERROR: %v\n", err)
	} else {
		fmt.Printf("9) bench processTurn(%d msgs) OK in %v: out=%d\n",
			benchMsgs, time.Since(tb).Round(time.Millisecond), len(bres.Messages))
	}

	fmt.Printf("messagesHash %s\n", sha8(res2.MessagesJSON))
	blocksJSON, err := sess.BlocksJSON()
	if err != nil {
		fail("BlocksJSON", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(blocksJSON, &raw); err != nil {
		fail("decode blocks", err)
	}
	fmt.Printf("blocksHash %s\n", sha8(blocksJSON))
	if len(raw) > 0 {
		stable := createdAtZero.ReplaceAll(raw[0], []byte(`"createdAt":0`))
		fmt.Printf("blockNoTimeJSON %s\n", stable)
		fmt.Printf("blockNoTimeHash %s\n", sha8(stable))
	}

	ts := time.Now()
	sbHits, err := sess.SearchBlocks("压缩 摘要")
	if err != nil {
		fmt.Printf("searchBlocks ERROR: %v\n", err)
	} else {
		fmt.Printf("10) searchBlocks OK in %v: hits=%d\n", time.Since(ts).Round(time.Millisecond), len(sbHits))
	}

	fmt.Printf("TOTAL %v\n", time.Since(t0).Round(time.Millisecond))
}

// newKernel 装配 kernel：$KERNEL_BUNDLE 覆盖内嵌产物（脚本重建后先试跑用）。
func newKernel() (*kernel.Kernel, error) {
	if p := os.Getenv("KERNEL_BUNDLE"); p != "" {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return kernel.New(kernel.WithBundle(src))
	}
	if _, err := os.Stat(filepath.Join("internal", "kernel", "kernel.dist.cjs")); err != nil {
		return nil, fmt.Errorf("找不到 internal/kernel/kernel.dist.cjs（当前目录 %s），或用 KERNEL_BUNDLE 指定", wd())
	}
	return kernel.New()
}

// fixture 复现 kernel_test 里的 40 条消息（与上游 Node 比对用的同一批输入）。
func fixture() []kernel.Message {
	msgs := make([]kernel.Message, 0, msgCount)
	for i := 0; i < msgCount; i++ {
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
			msgs = append(msgs, m)
			continue
		}
		m.Text = fmt.Sprintf("turn %d: 请分析 billion-context 的压缩策略 cache & compression behaviour, line %d; ", i, i+100) +
			strings.Repeat("详细讨论 context window, token budget, summary tier 以及缓存命中率。", 15)
		msgs = append(msgs, m)
	}
	return msgs
}

// benchFixture 是 200 条消息的压测输入。
func benchFixture() []kernel.Message {
	msgs := make([]kernel.Message, 0, benchMsgs)
	for i := 0; i < benchMsgs; i++ {
		m := kernel.Message{
			ID:          fmt.Sprintf("big_%04d", i),
			Role:        kernel.RoleUser,
			ContentType: kernel.ContentText,
		}
		if i%2 == 1 {
			m.Role = kernel.RoleAssistant
		}
		m.Text = fmt.Sprintf("turn %d: 上下文压缩上下文压缩 context window management discussion with details about tiering, summarisation and cache behaviour. ", i) +
			strings.Repeat("abcdefghij klmnopqrst uvwxyz 0123456789 中文内容测试 ", 8)
		msgs = append(msgs, m)
	}
	return msgs
}

var createdAtZero = regexp.MustCompile(`"createdAt":\d+`)

func sha8(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func wd() string {
	d, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return d
}

func fail(stage string, err error) {
	fmt.Printf("%s ERROR: %v\n", stage, err)
	os.Exit(1)
}
