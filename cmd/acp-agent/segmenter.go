//go:build segmenter

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/leijux/acp-agent/internal/agent"
	"github.com/leijux/acp-agent/internal/kernel"
	"github.com/leijux/acp-agent/internal/segmenter"
)

// buildSegmenter 按配置构造中文分词器；返回 nil 表示用 kernel 默认的
// RuneSegmenter（单字 + CJK bigram 回退），不额外占内存。
//
// 为什么这件事必须在启动阶段做：kernel 是进程级共享实例，bundle 与选项
// 只在第一次初始化时生效（见 agent.SetKernelOptions），之后再换分词器
// 对已有会话无效。
func buildSegmenter(cfg Config) (kernel.Segmenter, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Segmenter)) {
	case "", "rune", "default", "off":
		return nil, nil
	case "gse":
		// 默认只用简体词典：繁体文本会自动退化成单字（kernel 随之回退到
		// bigram），召回不丢，但启动快一倍、内存少一三分之一。
		seg, err := segmenter.NewGSE()
		if err != nil {
			return nil, err
		}
		return seg, nil
	case "gse_full", "gse-full":
		// 需要繁体词精度时选这个（加载约 811ms、分配 295MB）。
		seg, err := segmenter.NewGSE(segmenter.DictZhHansHant)
		if err != nil {
			return nil, err
		}
		return seg, nil
	case "sego":
		files := splitDictPaths(cfg.SegmenterDict)
		if len(files) == 0 {
			return nil, errors.New("segmenter=sego 需要 segmenterDict（sego 不自带词典，可从 " +
				"https://github.com/huichen/sego 的 data/dictionary.txt 获取）")
		}
		seg, err := segmenter.NewSego(files...)
		if err != nil {
			return nil, err
		}
		return seg, nil
	default:
		return nil, fmt.Errorf("未知 segmenter %q（可选：rune / gse / gse_full / sego）", cfg.Segmenter)
	}
}

// splitDictPaths 解析逗号分隔的词典路径。
func splitDictPaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// installSegmenter 构造并注入分词器。
//
// 失败不致命：分词器只影响中文检索召回，退化成 RuneSegmenter 仍然能用，
// 所以这里只打警告，不阻断启动。
func installSegmenter(cfg Config) {
	seg, err := buildSegmenter(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: %v（中文检索回退到单字 + bigram）\n", err)
		return
	}
	if seg == nil {
		return
	}
	agent.SetKernelOptions(kernel.WithSegmenter(seg))
}
