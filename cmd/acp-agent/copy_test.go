package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/leijux/acp-agent/internal/tui"
)

// OSC 52 序列里必须是原文的 base64；tmux 下要包 DCS 透传。
func TestOsc52SeqEncodesText(t *testing.T) {
	text := "acp-agent 复制测试 ✅"
	seq := osc52Seq(text).String()
	if !strings.Contains(seq, base64.StdEncoding.EncodeToString([]byte(text))) {
		t.Fatalf("OSC 52 序列不含 base64 正文: %q", seq)
	}
	if !strings.HasPrefix(seq, "\x1b]52;c;") {
		t.Fatalf("OSC 52 序列前缀不对: %q", seq)
	}

	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	if wrapped := osc52Seq(text).String(); !strings.Contains(wrapped, "\x1bPtmux;") {
		t.Fatalf("tmux 下应包 DCS 透传: %q", wrapped)
	}
}

func TestCopyToClipboardRejectsEmpty(t *testing.T) {
	if _, err := copyToClipboard("   \n", true); err == nil {
		t.Fatal("空内容应报错，而不是把剪贴板清空")
	}
}

// 交互界面下不允许 OSC 52（stdout 归渲染器独占，终端不支持时会假报成功）。
// 本机命令都不可用时应该直接报错，而不是回一个“已复制 via OSC 52”。
func TestCopyTextNoOSC52InTUI(t *testing.T) {
	a := &app{ui: tui.New(tui.Options{Header: "h"})}
	// 把平台命令全挪走，逼它只剩 OSC 52 这条路。
	t.Setenv("PATH", t.TempDir())
	how, err := a.copyText("abc")
	if err == nil {
		t.Fatalf("TUI 下不该用 OSC 52 假报成功（how=%q）", how)
	}
}

// 剪贴板命令的临时文件路径要能安全嵌进 PowerShell 单引号串：
// 路径里的 ' 必须写两遍，否则脚本语法错误（“复制不了”且没有原因）。
func TestPsClipboardScriptEscapesQuotes(t *testing.T) {
	script := psClipboardScript(`C:\Users\a'b\clip.txt`)
	want := `Set-Clipboard -Value ([IO.File]::ReadAllText('C:\Users\a''b\clip.txt', [Text.Encoding]::UTF8))`
	if script != want {
		t.Fatalf("脚本 = %q, 期望 %q", script, want)
	}
}

// 复制提示不能粘在流式半行后面（TUI 的 printf 共用同一个行缓冲）。
// 复现路径：思考内容逐 token 写入且没有换行 → 此时拖选复制。
func TestNoticeBreaksPendingLine(t *testing.T) {
	ui := tui.New(tui.Options{Header: "h"})
	term := &terminal{out: ui.Sink(), errOut: io.Discard}
	a := &app{ui: ui, term: term}

	term.mu.Lock()
	a.setKind(kindThinking)       // 真实路径：repl.go 在每个思考增量前声明 kind
	fmt.Fprint(term.out, "正在思考…") // 思考增量：无换行
	term.mu.Unlock()

	a.notice("已复制选区（1 行 / 3 字符）via x")

	var got []string
	for _, msg := range ui.Pending() {
		if lm, ok := msg.(tui.LineMsg); ok {
			got = append(got, fmt.Sprintf("%v|%s", lm.Kind, lm.Text))
		}
	}
	want := []string{
		fmt.Sprintf("%v|%s", tui.KindThinking, "正在思考…"),
		fmt.Sprintf("%v|%s", tui.KindSystem, "已复制选区（1 行 / 3 字符）via x"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("行序列 = %q\n期望 %q", got, want)
	}
}
