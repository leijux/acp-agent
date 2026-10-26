//go:build !segmenter

package main

import (
	"fmt"
	"os"
	"strings"
)

// installSegmenter 在未编译分词器的构建里只负责说明为什么没生效。
//
// 为什么要用 build tag 隔开：gse 自带 ~31MB 的简/繁/日词典 embed，即使我们
// 只用简体也会被 linker 全留下（52MB）。轻量构建不带它；需要词级分词时：
//
//	task build:full          # = go build -tags "segmenter ne"
//
// 注意 `ne` 不能漏：没有它 gse 自己的 embed 会被保留，体积白涨 30MB。
//
// 默认配置（空 = gse）在轻量构建里是**静默**降级的：每次启动都刷一行警告
// 只会变成噪音（`go run` 出来的就是轻量版）。只有用户显式要求 gse 时才提醒 ——
// 那才是「配了却没生效」。
func installSegmenter(cfg Config) {
	switch mode := strings.ToLower(strings.TrimSpace(cfg.Segmenter)); mode {
	case "", "rune", "default", "off":
		return
	case "gse", "gse_full", "gse-full":
		fmt.Fprintf(os.Stderr,
			"警告: 该二进制未编译中文分词词典（segmenter=%q 被忽略，中文检索回退到单字 + bigram）。"+
				"需要时用 `task build:full`（即 -tags \"segmenter ne\"）重新构建。\n", mode)
	default:
		fmt.Fprintf(os.Stderr, "警告: 未知 segmenter=%q（可选：gse / rune），中文检索用单字 + bigram\n", mode)
	}
}
