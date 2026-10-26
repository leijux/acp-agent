//go:build segmenter

package main

import (
	"testing"

	"github.com/leijux/acp-agent/internal/kernel"
)

// TestBuildSegmenterModes 覆盖配置到分词器的映射与错误分支。
// 不测 gse 的实际切分 —— 那是 internal/segmenter 包的职责，
// 这里只保证接线正确（错误不被吞掉）。
func TestBuildSegmenterModes(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
		wantNil bool
	}{
		// 默认就是 gse：好分词不需要用户手动打开。
		{name: "默认 gse", cfg: Config{}},
		{name: "default 别名", cfg: Config{Segmenter: "default"}},
		{name: "gse", cfg: Config{Segmenter: "gse"}},
		{name: "rune 降级", cfg: Config{Segmenter: "rune"}, wantNil: true},
		{name: "off", cfg: Config{Segmenter: "OFF"}, wantNil: true},
		{name: "未知", cfg: Config{Segmenter: "jieba"}, wantErr: true},
		// gse_full 已合并到 gse（只带简体），旧配置降级而不是报错。
		{name: "gse_full 降级", cfg: Config{Segmenter: "gse_full"}},
		// sego 已移除，但旧配置/环境变量里可能还写着 —— 必须报错而不是静默降级。
		{name: "sego 已移除", cfg: Config{Segmenter: "sego"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seg, err := buildSegmenter(c.cfg)
			if c.wantErr {
				if err == nil {
					t.Fatal("应返回错误")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if c.wantNil && seg != nil {
				t.Fatalf("期望 nil 分词器，得到 %T", seg)
			}
			if !c.wantNil && seg == nil {
				t.Fatalf("期望非 nil 分词器（%v）", c.cfg.Segmenter)
			}
		})
	}
}

// TestBuildSegmenterExpand 盯住 segmenterExpand 三态：默认开，off 关闭，
// 且关闭后同一个句子不再含跨词 bigram。
func TestBuildSegmenterExpand(t *testing.T) {
	const text = "上下文压缩引擎"

	on, err := buildSegmenter(Config{})
	if err != nil {
		t.Fatalf("默认: %v", err)
	}
	if !hasSeg(on, text, "缩引") {
		t.Fatalf("默认应展开 bigram，得到 %v", segTexts(on, text))
	}

	off, err := buildSegmenter(Config{SegmenterExpand: "off"})
	if err != nil {
		t.Fatalf("gse + expand=off: %v", err)
	}
	if hasSeg(off, text, "缩引") {
		t.Fatalf("expand=off 不应出现跨词 bigram，得到 %v", segTexts(off, text))
	}

	if !expandEnabled("") || !expandEnabled("on") || expandEnabled("off") || expandEnabled("false") {
		t.Fatal("expandEnabled 三态解析不对")
	}
}

func segTexts(seg kernel.Segmenter, text string) []string {
	var out []string
	for _, s := range seg.Segment(text) {
		out = append(out, s.Text)
	}
	return out
}

func hasSeg(seg kernel.Segmenter, text, want string) bool {
	for _, s := range segTexts(seg, text) {
		if s == want {
			return true
		}
	}
	return false
}
