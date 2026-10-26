package segmenter

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/leijux/acp-agent/internal/kernel"
)

// texts 取段的文本序列，方便断言。
func texts(segs []kernel.Segment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.Text)
	}
	return out
}

func hasToken(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// subsequence 判断 sub 是否是 seq 的子序列（保持相对顺序）。
func subsequence(seq, sub []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(sub) && s == sub[i] {
			i++
		}
	}
	return i == len(sub)
}

// TestGSEProducesWords 是「真分词器接进来了」的核心断言：
// 默认 RuneSegmenter 全是一字一段，这里必须能切出多字词。
func TestGSEProducesWords(t *testing.T) {
	seg, err := NewGSE()
	if err != nil {
		t.Fatalf("NewGSE: %v", err)
	}

	const text = "上下文压缩引擎会把历史折叠成摘要"
	got := seg.Segment(text)
	if len(got) == 0 {
		t.Fatal("没有切出任何段")
	}

	var texts []string
	words := 0
	last := -1
	for _, s := range got {
		if s.Text == "" {
			t.Errorf("出现空段: %+v", s)
		}
		if s.Index < last {
			t.Errorf("Index 回退: %d 在 %d 之后（%v）", s.Index, last, texts)
		}
		if s.Index < 0 || s.Index > utf8.RuneCountInString(text) {
			t.Errorf("Index 越界: %d（文本 %d 个 rune）", s.Index, utf8.RuneCountInString(text))
		}
		last = s.Index
		texts = append(texts, s.Text)
		if utf8.RuneCountInString(s.Text) >= 2 {
			words++
		}
	}
	if words == 0 {
		t.Fatalf("gse 没切出任何多字词（退化成单字？）: %v", texts)
	}
	t.Logf("gse: %s", strings.Join(texts, "|"))
}

// TestGSEDictionaryLoaded 盯住词表本身：条数与最长词长。
// 词表是生成物，gse 升级或重新裁剪后这里会先失败，提醒复核切分 golden。
func TestGSEDictionaryLoaded(t *testing.T) {
	seg, err := NewGSE()
	if err != nil {
		t.Fatalf("NewGSE: %v", err)
	}
	dict := seg.seg.Dictionary()
	if got := dict.NumTokens(); got != HansDictWords {
		t.Fatalf("简体词表条数 = %d，期望 %d（没走 LoadDictStr？或词表被改坏）", got, HansDictWords)
	}
	if maxLen := dict.MaxTokenLen(); maxLen < 2 {
		t.Fatalf("MaxTokenLen = %d，词表里没有多字词", maxLen)
	}
	t.Logf("简体词表 tokens=%d maxTokenLen=%d totalFreq=%.0f",
		dict.NumTokens(), dict.MaxTokenLen(), dict.TotalFreq())

	// 只带简体：繁体文本切不出繁体词，只能靠单字 + bigram 兜。
	if got := texts(seg.Segment("壓縮引擎")); hasToken(got, "壓縮") {
		t.Fatalf("只应该有简体词表，却切出了繁体词「壓縮」：%v", got)
	}
}

// TestGSEGolden 固定几句典型中文的切分结果。
// 词表/gse 版本变化导致召回漂移时，这里最先炸 —— 确认新结果可接受后再更新。
func TestGSEGolden(t *testing.T) {
	seg, err := NewGSE()
	if err != nil {
		t.Fatalf("NewGSE: %v", err)
	}
	cases := map[string][]string{
		"上下文压缩引擎会把历史折叠成摘要": {"上下文", "压缩", "引擎", "会", "把", "历史", "折叠", "成", "摘要"},
		"缓存命中率下跌":         {"缓存", "命中率", "下跌"},
		"帮我看看数据库连接池的实现":   {"帮", "我", "看看", "数据库", "连接", "池", "的", "实现"},
	}
	for text, want := range cases {
		got := texts(seg.Segment(text))
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s\n got: %v\nwant: %v", text, got, want)
		}
	}
}

// TestGSEExpandBigram 是召回回归测试，对应 kernel 的 cjkRunTokens 语义：
// 只要 run 内有多字词就只留词、丢掉单字。展开后必须补回 bigram。
func TestGSEExpandBigram(t *testing.T) {
	plain, err := NewGSE()
	if err != nil {
		t.Fatalf("NewGSE: %v", err)
	}
	exp, err := NewGSE(WithExpandBigram())
	if err != nil {
		t.Fatalf("NewGSE(WithExpandBigram): %v", err)
	}

	// 繁体：简体词表切不出「壓縮」，只有「引擎」是多字词，于是 kernel 只剩「引擎」。
	got := texts(plain.Segment("壓縮引擎"))
	t.Logf("plain 壓縮引擎 = %v", got)
	if hasToken(got, "壓縮") {
		t.Errorf("简体词表不该切出繁体词，得到 %v", got)
	}
	gotExp := texts(exp.Segment("壓縮引擎"))
	t.Logf("expand 壓縮引擎 = %v", gotExp)
	for _, want := range []string{"引擎", "壓縮", "縮引"} {
		if !hasToken(gotExp, want) {
			t.Errorf("展开后应含 %q，得到 %v", want, gotExp)
		}
	}

	// 跨词边界：kernel 的 BM25 通道在直通模式下查不到「缩引」。
	plainTokens := texts(plain.Segment("上下文压缩引擎"))
	if hasToken(plainTokens, "缩引") {
		t.Fatalf("直通不该出现跨词 bigram，得到 %v", plainTokens)
	}
	if expTokens := texts(exp.Segment("上下文压缩引擎")); !hasToken(expTokens, "缩引") {
		t.Errorf("展开后应含跨词 bigram「缩引」，得到 %v", expTokens)
	}

	// 全单字 run 不展开（否则会跟 kernel 自己的 bigram 回退重复计数）。
	// 「壓縮引擎」的简体词典切分是 [壓 縮 引擎]，已含多字词，属展开分支；
	// 这里用纯未登录字验证不重复。
	rare := texts(exp.Segment("贇鼒"))
	if len(rare) != 2 {
		t.Errorf("全单字 run 不应展开，得到 %v", rare)
	}

	// 展开不能改变原有段：plain 的每一段都得出现在 expand 里（顺序一致）。
	plainAll := texts(plain.Segment("上下文压缩引擎会把历史折叠成摘要"))
	expAll := texts(exp.Segment("上下文压缩引擎会把历史折叠成摘要"))
	if !subsequence(expAll, plainAll) {
		t.Errorf("展开把原有段弄丢了\nplain: %v\nexpand: %v", plainAll, expAll)
	}
}

// TestGSEEmpty 确认空串不会 panic。
func TestGSEEmpty(t *testing.T) {
	seg, err := NewGSE()
	if err != nil {
		t.Fatalf("NewGSE: %v", err)
	}
	if got := seg.Segment(""); len(got) != 0 {
		t.Fatalf("空串应返回空切片，得到 %v", got)
	}
}
