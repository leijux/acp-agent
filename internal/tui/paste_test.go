package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pasteMsg 构造一次括号粘贴（bubbletea 用 Paste 标记整段输入）。
func pasteMsg(s string) tea.Msg {
	return tea.PasteMsg{Content: s}
}

// 多行粘贴折叠成 [txt×N行]，提交时展开成原文。
func TestPasteMultilineFoldsAndExpands(t *testing.T) {
	m, _ := newTestModel(80, 20)
	src := "第一行\n第二行\n第三行"

	send(m, pasteMsg(src))

	if got := string(m.in.buf); got != "[txt×3行]" {
		t.Fatalf("输入框显示 = %q，期望 [txt×3行]", got)
	}
	if got := m.in.text(); got != src {
		t.Fatalf("展开结果 = %q，期望原文", got)
	}
	// 视图里不能出现裸换行，否则输入行会顶成两行、整屏错位。
	if got := m.in.view(prompt, 80); strings.Contains(got, "\n") {
		t.Fatalf("输入行渲染含换行: %q", got)
	}
}

// CRLF（Windows 复制的文本）也要按 3 行算。
func TestPasteCRLFLines(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, pasteMsg("a\r\nb\r\nc"))
	if got := string(m.in.buf); got != "[txt×3行]" {
		t.Fatalf("输入框显示 = %q，期望 [txt×3行]", got)
	}
	if got := m.in.text(); got != "a\nb\nc" {
		t.Fatalf("展开结果 = %q，期望 a\\nb\\nc", got)
	}
}

// 单行过长也折叠，标注字符数；短的单行粘贴保持原样。
func TestPasteLongSingleLineFolds(t *testing.T) {
	m, _ := newTestModel(80, 20)
	long := strings.Repeat("字", pasteFoldRunes+1)
	send(m, pasteMsg(long))
	if got := string(m.in.buf); got != fmt.Sprintf("[txt×%d字]", len([]rune(long))) {
		t.Fatalf("输入框显示 = %q", got)
	}
	if got := m.in.text(); got != long {
		t.Fatal("展开结果与原文不一致")
	}

	m2, _ := newTestModel(80, 20)
	send(m2, pasteMsg("普通的短文本"))
	if got := string(m2.in.buf); got != "普通的短文本" {
		t.Fatalf("短粘贴不该折叠: %q", got)
	}
}

// 两个内容不同但行数相同的粘贴不能共用一个占位符（否则展开会串）。
func TestPasteTokensAreDistinct(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, pasteMsg("a\nb"))
	send(m, pasteMsg("c\nd"))

	if got := string(m.in.buf); got != "[txt×2行][txt×2行·2]" {
		t.Fatalf("输入框显示 = %q，期望两个不同占位符", got)
	}
	// 第二个 token 不能把第一个当前缀，否则展开会串（这正是序号放括号里的原因）。
	if got := m.in.text(); got != "a\nbc\nd" {
		t.Fatalf("展开结果 = %q，期望 \"a\\nbc\\nd\"", got)
	}
}

// 退格/删除把占位符当一个整体删，不会留下半个 [txt×…]。
func TestPasteTokenDeletedAsOneUnit(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, pasteMsg("a\nb\nc"))
	send(m, key(tea.KeyBackspace))

	if got := string(m.in.buf); got != "" {
		t.Fatalf("退格后 = %q，期望清空", got)
	}
	if got := m.in.text(); got != "" {
		t.Fatalf("退格后展开 = %q，期望空", got)
	}

	// 光标在占位符左侧时 Delete 也整体删。
	m2, _ := newTestModel(80, 20)
	send(m2, pasteMsg("a\nb"))
	send(m2, key(tea.KeyHome))
	send(m2, key(tea.KeyDelete))
	if got := string(m2.in.buf); got != "" {
		t.Fatalf("Delete 后 = %q，期望清空", got)
	}
}

// 方向键整体跨过占位符，而不是停在 token 中间。
func TestPasteTokenNavigation(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, pasteMsg("a\nb"))
	send(m, key(tea.KeyHome))
	send(m, key(tea.KeyRight))
	if m.in.cur != len("[txt×2行]") {
		t.Fatalf("右移后光标 = %d，期望跳到占位符末尾 %d", m.in.cur, len("[txt×2行]"))
	}
	send(m, key(tea.KeyLeft))
	if m.in.cur != 0 {
		t.Fatalf("左移后光标 = %d，期望 0", m.in.cur)
	}
}

// 粘贴 + 手打混排，提交时按出现位置展开。
func TestPasteMixedWithTyping(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, runes("请分析："))
	send(m, pasteMsg("行1\n行2"))
	if got := m.in.text(); got != "请分析：行1\n行2" {
		t.Fatalf("展开 = %q", got)
	}
}

// Alt+Enter 手动换行：真实换行留在 buf（提交时是多行），但画出来是 ⏎。
func TestAltEnterNewlineRendersAsSymbol(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, runes("第一行"))
	send(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	send(m, runes("第二行"))

	if got := m.in.text(); got != "第一行\n第二行" {
		t.Fatalf("提交文本 = %q，期望含真实换行", got)
	}
	view := m.in.view(prompt, 80)
	if strings.Contains(view, "\n") {
		t.Fatalf("渲染不应含换行: %q", view)
	}
	if !strings.Contains(view, "⏎") {
		t.Fatalf("渲染应显示 ⏎: %q", view)
	}
}

// 终端没启括号粘贴时，一次多行粘贴会变成 runes+Enter+runes… 一串事件。
// 每个换行都不能当成提交，否则一条粘贴会变成好几条消息发给模型；
// 而且停下来以后要跟括号粘贴一样折成 [txt×N行]。
func TestLoosePasteMultilineIsOneMessage(t *testing.T) {
	m, ui := newTestModel(80, 20)
	fakeClock(m, time.Millisecond) // 粘贴：所有字符几乎是同一时刻到达的

	send(m, runes("第一行"))
	send(m, key(tea.KeyEnter)) // 粘贴里的换行（\n）
	send(m, runes("第二行"))
	send(m, key(tea.KeyEnter)) // 粘贴里的 \r\n：\r 和 \n 各解析成一个 KeyEnter
	send(m, key(tea.KeyEnter))

	select {
	case got := <-ui.submit:
		t.Fatalf("粘贴中途就提交了: %q", got)
	default:
	}

	// 粘完松手：收尾定时器到点，攒的整段折叠成占位符。
	send(m, loosePasteMsg{seq: m.looseSeq})
	if got := m.in.text(); got != "第一行\n第二行" {
		t.Fatalf("展开 = %q，期望两行且 \\r\\n 只算一个换行", got)
	}
	if got := string(m.in.buf); got != "[txt×2行]" {
		t.Fatalf("输入行 = %q，期望整段折成一个 [txt×2行]", got)
	}

	// 粘贴结束后停下来，再按回车才是真的提交，而且只提交一条。
	fakeClock(m, 200*time.Millisecond)
	send(m, key(tea.KeyEnter))

	select {
	case got := <-ui.submit:
		if got != "第一行\n第二行" {
			t.Fatalf("提交 = %q", got)
		}
	default:
		t.Fatal("粘贴结束后回车没有提交")
	}
	select {
	case got := <-ui.submit:
		t.Fatalf("一次粘贴发出了多条消息，多出: %q", got)
	default:
	}
	if n := len(m.blocks); n != 1 {
		t.Fatalf("用户块 = %d 个，期望 1 个", n)
	}
}

// 打字（键间隔远大于突发窗口）后回车必须照旧提交，不能误判成粘贴。
func TestTypingThenEnterStillSubmits(t *testing.T) {
	m, ui := newTestModel(80, 20)
	fakeClock(m, 80*time.Millisecond)

	send(m, runes("帮我看看"))
	send(m, key(tea.KeyEnter))

	select {
	case got := <-ui.submit:
		if got != "帮我看看" {
			t.Fatalf("提交 = %q", got)
		}
	default:
		t.Fatalf("回车没有提交，输入行还留着 %q", m.in.text())
	}
}

// 输入法上屏也是一条多字符消息，但它不是粘贴：不能延迟显示、不能折叠。
func TestIMERunesInsertDirectly(t *testing.T) {
	m, _ := newTestModel(80, 20)
	fakeClock(m, 100*time.Millisecond)

	send(m, runes("你好世界"))
	if got := string(m.in.buf); got != "你好世界" {
		t.Fatalf("输入行 = %q，期望直接落字", got)
	}
	if len(m.in.pastes) != 0 {
		t.Fatalf("上屏不该被当成粘贴: %v", m.in.pastes)
	}
}

// 提交后占位符状态要清干净（否则下一行会带着上一行的展开表）。
func TestPasteStateClearedOnSubmit(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, pasteMsg("a\nb"))
	send(m, key(tea.KeyEnter))

	if len(m.in.pastes) != 0 || len(m.in.buf) != 0 {
		t.Fatalf("提交后残留: buf=%q pastes=%v", m.in.buf, m.in.pastes)
	}
	// 用户块应该是展开后的多行内容
	if got := m.blocks[0].Lines; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("用户块 = %q，期望两行 a/b", got)
	}
}

// Ctrl+Enter 换行。终端把它发成 LF 时是 KeyCtrlJ；Windows 的 coninput 会把它
// 报成 KeyRunes("\n")，两条路径都要能换行（而不是提交、也不是被过滤掉）。
func TestCtrlEnterInsertsNewline(t *testing.T) {
	t.Run("KeyCtrlJ", func(t *testing.T) {
		m, _ := newTestModel(80, 20)
		send(m, runes("第一行"))
		send(m, ctrlKey('j'))
		send(m, runes("第二行"))
		if got := m.in.text(); got != "第一行\n第二行" {
			t.Fatalf("提交文本 = %q，期望含真实换行", got)
		}
		if view := m.in.view(prompt, 80); strings.Contains(view, "\n") || !strings.Contains(view, "⏎") {
			t.Fatalf("渲染应为单行显示 ⏎: %q", view)
		}
	})

	t.Run("KeyRunes 裸换行", func(t *testing.T) {
		m, _ := newTestModel(80, 20)
		send(m, runes("a"))
		send(m, tea.KeyPressMsg{Text: "\n"})
		send(m, runes("b"))
		if got := m.in.text(); got != "a\nb" {
			t.Fatalf("提交文本 = %q，期望含真实换行", got)
		}
	})

	// v2 起按键带修饰位：支持增强键盘的终端直接上报 Enter+Ctrl。
	t.Run("Ctrl+Enter（v2 修饰位）", func(t *testing.T) {
		m, ui := newTestModel(80, 20)
		send(m, runes("a"))
		send(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
		send(m, runes("b"))
		if got := m.in.text(); got != "a\nb" {
			t.Fatalf("提交文本 = %q，期望含真实换行", got)
		}
		select {
		case got := <-ui.submit:
			t.Fatalf("Ctrl+Enter 不该提交，收到 %q", got)
		default:
		}
	})

	// 换行不能把输入提交掉（否则多行输入根本敲不出来）
	t.Run("不触发提交", func(t *testing.T) {
		m, ui := newTestModel(80, 20)
		send(m, runes("x"))
		send(m, ctrlKey('j'))
		select {
		case got := <-ui.submit:
			t.Fatalf("Ctrl+Enter 不该提交，收到 %q", got)
		default:
		}
	})
}

// 功能键不能被当成文本插进输入框。
//
// v2 里特殊键的 Code 是 iota 私有值（KeyF1…KeyF32），早先的实现有
// 「Text 为空就用 string(Code) 兜底」，结果按 F1 会插入一个私用区乱码字符。
func TestSpecialKeysDoNotInsertText(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, runes("ab"))
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyF1},
		{Code: tea.KeyF5},
		{Code: tea.KeyInsert},
		{Code: tea.KeyTab},
	} {
		send(m, k)
	}
	if got := m.in.text(); got != "ab" {
		t.Fatalf("特殊键不该插入字符，实际 %q", got)
	}
}

// abort 后退回的排队消息要能放回输入框（Pi 语义：放回编辑器）。
func TestPrefillReplacesInput(t *testing.T) {
	m, _ := newTestModel(80, 20)
	send(m, runes("旧内容"))
	send(m, PrefillMsg{Text: "顺便看下 README"})
	if got := m.in.text(); got != "顺便看下 README" {
		t.Fatalf("输入框 = %q，期望被退回的消息覆盖", got)
	}
	// 多行退回也要能显示（折叠成 ⏎），且不提交
	if view := m.in.view(prompt, 80); strings.Contains(view, "\n") {
		t.Fatalf("输入行不该含裸换行: %q", view)
	}
	send(m, PrefillMsg{Text: "第一行\n第二行"})
	if got := m.in.text(); got != "第一行\n第二行" {
		t.Fatalf("多行退回 = %q", got)
	}
}

// 权限确认期间的粘贴不能进输入框（否则剪贴板内容会灌进正在询问的界面）。
func TestPasteIgnoredDuringPermission(t *testing.T) {
	m, _ := newTestModel(80, 20)
	reply := make(chan string, 1)
	send(m, AskMsg{Tool: "write", Reply: reply})
	send(m, pasteMsg("不该进来"))
	if got := m.in.text(); got != "" {
		t.Fatalf("权限询问期间粘贴被写进输入框: %q", got)
	}
	// 回答之后粘贴恢复正常
	send(m, key('y'))
	send(m, pasteMsg("正常"))
	if got := m.in.text(); got != "正常" {
		t.Fatalf("回答后粘贴应恢复，实际 %q", got)
	}
}
