//go:build segmenter

package main

import "testing"

// TestBuildSegmenterModes 覆盖配置到分词器的映射与错误分支。
// 不测 gse/sego 的实际切分 —— 那是 internal/segmenter 包的职责，
// 这里只保证接线正确（错误不被吞掉）。
func TestBuildSegmenterModes(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
		wantNil bool
	}{
		{name: "默认", cfg: Config{}, wantNil: true},
		{name: "rune", cfg: Config{Segmenter: "rune"}, wantNil: true},
		{name: "off", cfg: Config{Segmenter: "OFF"}, wantNil: true},
		{name: "未知", cfg: Config{Segmenter: "jieba"}, wantErr: true},
		{name: "sego 缺词典", cfg: Config{Segmenter: "sego"}, wantErr: true},
		{
			name:    "sego 词典不存在",
			cfg:     Config{Segmenter: "sego", SegmenterDict: t.TempDir() + "/missing.txt"},
			wantErr: true,
		},
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
		})
	}
}

// TestSplitDictPaths 覆盖逗号分隔与空白清理。
func TestSplitDictPaths(t *testing.T) {
	got := splitDictPaths(" a.txt , ,b.txt,")
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Fatalf("got %v", got)
	}
	if len(splitDictPaths("  ")) != 0 {
		t.Fatal("空串应返回空切片")
	}
}
