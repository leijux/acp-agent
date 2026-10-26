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

// 本文件在 `segmenter` tag 下编译。**必须同时带 gse 的 `ne` tag**：
// 否则 gse 自己 embed 的简/繁/日词典与 idf 表（约 31MB）会被 linker 保留，
// 二进制从 22MB 涨到 52MB。统一入口是 `task build:full`（TAGS="segmenter ne"）。

// buildSegmenter 按配置构造中文分词器；返回 nil 表示用 kernel 默认的
// RuneSegmenter（单字 + CJK bigram 回退），不额外占内存。
//
// 默认（配置为空）就是 gse：分词质量直接决定 BM25 召回，没有理由把好分词
// 变成需要手动打开的选项。要降级的唯一途径是 config.json / ACP_AGENT_SEGMENTER
// 里的 segmenter=rune（子代理就这么干）；命令行没有开关。
//
// 为什么这件事必须在启动阶段做：kernel 是进程级共享实例，bundle 与选项
// 只在第一次初始化时生效（见 agent.SetKernelOptions），之后再换分词器
// 对已有会话无效。
func buildSegmenter(cfg Config) (kernel.Segmenter, error) {
	switch mode := strings.ToLower(strings.TrimSpace(cfg.Segmenter)); mode {
	case "", "default", "gse":
		return newGSE(cfg)
	case "rune", "off":
		return nil, nil
	case "gse_full", "gse-full":
		// 简体词表已取代双词表：旧配置当 gse 用，不报错（报错会让整个分词器
		// 回退到 rune，比降级更差）。
		return newGSE(cfg)
	case "sego":
		return nil, errors.New("segmenter=sego 已移除（它需要外部分发词典，且 LoadDictionary 在坏词典上会 log.Fatalf）；用 gse")
	default:
		return nil, fmt.Errorf("未知 segmenter %q（可选：gse / rune）", cfg.Segmenter)
	}
}

// newGSE 加载内置简体词表。expand 默认开：kernel 的 cjkRunTokens 只要 run
// 内有多字词就丢掉全部单字段，直通分词结果会丢跨词边界/繁体的召回（见
// segmenter.WithExpandBigram）。
func newGSE(cfg Config) (kernel.Segmenter, error) {
	var opts []segmenter.GSEOption
	if expandEnabled(cfg.SegmenterExpand) {
		opts = append(opts, segmenter.WithExpandBigram())
	}
	seg, err := segmenter.NewGSE(opts...)
	if err != nil {
		return nil, err
	}
	return seg, nil
}

// expandEnabled 解析 segmenterExpand 三态：空/on/true → 开；off/false/0 → 关。
func expandEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0", "no":
		return false
	default:
		return true
	}
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
