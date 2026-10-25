//go:build !segmenter

package main

import (
	"fmt"
	"os"
	"strings"
)

// installSegmenter 在未编译分词器的默认构建里只负责解释为什么没生效。
//
// 为什么要用 build tag 隔开：gse + sego 的词典会被 embed 进二进制，
// 体积从 20MB 涨到 52MB（-s -w 后）。这对绝大多数用户不值得，所以默认
// 不编进去；需要时用 `go build -tags segmenter` 构建。
func installSegmenter(cfg Config) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Segmenter))
	if mode == "" || mode == "rune" || mode == "default" || mode == "off" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"警告: 该二进制未编译第三方分词器（segmenter=%q 被忽略，中文检索回退到单字 + bigram）。"+
			"需要时用 `go build -tags segmenter` 重新构建。\n", cfg.Segmenter)
}
