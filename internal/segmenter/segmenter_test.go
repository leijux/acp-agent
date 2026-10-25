package segmenter

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

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

// TestSegoRequiresDictionary 确认坏路径返回错误，而不是让 sego 内部
// log.Fatalf 把整个进程带走。
func TestSegoRequiresDictionary(t *testing.T) {
	if _, err := NewSego(); err == nil {
		t.Fatal("没有词典时应报错")
	}
	if _, err := NewSego(t.TempDir() + "/missing.txt"); err == nil {
		t.Fatal("词典不存在时应报错")
	}
	if _, err := NewSego(t.TempDir()); err == nil {
		t.Fatal("传目录时应报错")
	}
}

// TestSegoWithDictionary 用一份最小词典验证接缝（无真实词典也能跑）。
func TestSegoWithDictionary(t *testing.T) {
	dir := t.TempDir()
	dict := dir + "/dict.txt"
	if err := os.WriteFile(dict, []byte("上下文 10 n\n压缩 8 v\n引擎 6 n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seg, err := NewSego(dict)
	if err != nil {
		t.Fatalf("NewSego: %v", err)
	}
	got := seg.Segment("上下文压缩")
	if len(got) == 0 {
		t.Fatal("没有切出任何段")
	}
	var texts []string
	for _, s := range got {
		texts = append(texts, s.Text)
	}
	// 词典里的词应当作为整体出现（sego 走最短路径，至少覆盖整段文本）。
	joined := strings.Join(texts, "")
	if joined != "上下文压缩" {
		t.Fatalf("切分结果丢字: %v", texts)
	}
	t.Logf("sego: %s", strings.Join(texts, "|"))
}
