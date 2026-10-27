package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/leijux/acp-agent/internal/agent"
)

func TestFmtTokens(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		7:         "7",
		999:       "999",
		1000:      "1k",
		1234:      "1.2k",
		12_345:    "12.3k",
		99_949:    "99.9k",
		123_456:   "123k",
		999_499:   "999k",
		1_234_567: "1.2M",
	}
	for in, want := range cases {
		if got := fmtTokens(in); got != want {
			t.Errorf("fmtTokens(%d) = %q, 期望 %q", in, got, want)
		}
	}
}

// 一轮结束时那行用量：数字带人类可读单位，不再是一串原始位数。
func TestPrintUsageHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	term := newTerminal(&buf, true) // 无颜色，便于断言
	term.printUsage(agent.Usage{
		Input:     12_345,
		Output:    678,
		CacheRead: 8_100,
		Reasoning: 320,
	}, 1500*time.Millisecond)

	got := buf.String()
	for _, want := range []string{"in 12.3k", "out 678", "cache_read 8.1k", "reasoning 320", "1.5s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("用量行缺少 %q: %q", want, got)
		}
	}
	if strings.Contains(got, "12345") {
		t.Fatalf("用量行仍在打原始位数: %q", got)
	}
}
