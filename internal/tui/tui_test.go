package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func newTestModel(w, h int) (*Model, *UI) {
	ui := &UI{submit: make(chan string, 8), opts: Options{FPS: 30}}
	m := newModel(ui, Options{Header: "hdr", FPS: 30})
	// 默认假时钟每次按键走 100ms：模拟人打字（远大于 pasteBurstGap），
	// 否则连同一条测试里背靠背的 send 都会被当成粘贴突发。
	// 要测粘贴就自己换一个步长更小的时钟（见 paste_test.go）。
	fake := time.Time{}
	m.now = func() time.Time { fake = fake.Add(100 * time.Millisecond); return fake }
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, ui
}

// fakeClock 让 m 的按键时间按每次 step 推进（step 小于 pasteBurstGap 即模拟粘贴）。
func fakeClock(m *Model, step time.Duration) {
	cur := time.Time{}
	m.now = func() time.Time { cur = cur.Add(step); return cur }
}

func send(m *Model, msg tea.Msg) { m.Update(msg) }

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func viewLines(m *Model) []string { return strings.Split(m.View(), "\n") }

// TestCollapseThinkingMsg 回放历史后一键收起已有思考块。
func TestCollapseThinkingMsg(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindThinking, Text: "甲"})
	send(m, LineMsg{Kind: KindText, Text: "答复"})
	send(m, LineMsg{Kind: KindThinking, Text: "乙"})
	send(m, CollapseThinkingMsg{})
	if !m.blocks[0].Collapsed || !m.blocks[2].Collapsed {
		t.Fatalf("两个思考块都该折叠: %+v", m.blocks)
	}
	if m.blocks[1].Collapsed {
		t.Fatal("正文块不该被折叠")
	}
	if !strings.Contains(ansi.Strip(m.View()), "答复") {
		t.Fatal("正文应从视图里消失了吗: " + ansi.Strip(m.View()))
	}
}

// TestAutoCollapseAfterTurn：思考块在 turn 结束时自动收起（用户不用手动折叠）。
func TestAutoCollapseAfterTurn(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, RunningMsg{On: true})
	send(m, LineMsg{Kind: KindThinking, Text: "想一想"})
	send(m, LineMsg{Kind: KindThinking, Text: "再想"})
	send(m, LineMsg{Kind: KindText, Text: "答"})
	if m.blocks[0].Collapsed {
		t.Fatal("流式中不应该一开始就折叠")
	}
	send(m, RunningMsg{On: false})
	if !m.blocks[0].Collapsed {
		t.Fatal("turn 结束后思考块应自动收起")
	}
	if m.blocks[0].Started.IsZero() {
		t.Fatal("思考块应记录起始时间（标题要显示耗时）")
	}
	// 注：不断言 Duration>0 —— Windows 上同一时钟滴答内 time.Since 会是 0。
	// 正文不受影响
	send(m, key(tea.KeyCtrlT))
	if m.blocks[0].Collapsed {
		t.Fatal("Ctrl+T 应能重新展开")
	}
}

// TestViewHeightIsExact 视图行数必须严格等于终端高度，否则行内重绘会错位。
func TestViewHeightIsExact(t *testing.T) {
	m, _ := newTestModel(40, 12)
	for _, h := range []int{8, 12, 20} {
		send(m, tea.WindowSizeMsg{Width: 40, Height: h})
		if got := len(viewLines(m)); got != h {
			t.Fatalf("height=%d 时 View 有 %d 行", h, got)
		}
	}
}

// TestSubmitSendsText：输入 + 回车应把文本送去给宿主，并回显成 user 块。
func TestSubmitSendsText(t *testing.T) {
	m, ui := newTestModel(60, 16)
	for _, r := range "你好 world" {
		send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.in.text(); got != "你好 world" {
		t.Fatalf("输入行 = %q", got)
	}
	send(m, key(tea.KeyEnter))
	select {
	case got := <-ui.submit:
		if got != "你好 world" {
			t.Fatalf("提交 = %q", got)
		}
	default:
		t.Fatal("回车没有把输入交给宿主")
	}
	if len(m.blocks) == 0 || m.blocks[0].Kind != KindUser {
		t.Fatalf("用户输入应回显成 user 块: %+v", m.blocks)
	}
	if !strings.Contains(m.View(), "你好 world") {
		t.Fatalf("视图里看不到用户输入:\n%s", m.View())
	}
}

// TestThinkingCollapse 思考块可折叠：折叠后只剩标题行，再按恢复。
func TestThinkingCollapse(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindThinking, Text: "第一步"})
	send(m, LineMsg{Kind: KindThinking, Text: "第二步"})
	send(m, LineMsg{Kind: KindText, Text: "回答"})

	before := len(m.blocks[0].render(60))
	send(m, key(tea.KeyCtrlT))
	collapsed := m.blocks[0].render(60)
	if len(collapsed) != 1 {
		t.Fatalf("折叠后应只剩标题行，实际 %d 行", len(collapsed))
	}
	if !strings.Contains(ansi.Strip(collapsed[0]), "思考") {
		t.Fatalf("标题行没说明是思考: %q", collapsed[0])
	}
	if !strings.Contains(ansi.Strip(collapsed[0]), "2 行") {
		t.Fatalf("标题行应显示行数: %q", collapsed[0])
	}
	// 折叠不能影响正文块
	if strings.Contains(ansi.Strip(m.View()), "第一步") {
		t.Fatal("折叠后正文仍在屏幕上")
	}
	send(m, key(tea.KeyCtrlT))
	if got := len(m.blocks[0].render(60)); got != before {
		t.Fatalf("展开后行数 = %d，期望 %d", got, before)
	}
}

// TestClickTogglesThinking：鼠标点在思考块上等价于 Ctrl+T。
// 点击 = 按下 + 松开（没拖动）；折叠在松开时才发生，这样才不会把拖选当成点击。
func TestClickTogglesThinking(t *testing.T) {
	m, _ := newTestModel(60, 20)
	send(m, LineMsg{Kind: KindThinking, Text: "想一想"})
	m.View() // 触发 rebuild，填好 owners

	// 第 0 行是 header；思考块标题在第 1 行（0-based）。
	click := []tea.MouseMsg{
		{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 3, Y: 1},
		{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 3, Y: 1},
	}
	for _, msg := range click {
		send(m, msg)
	}
	if !m.blocks[0].Collapsed {
		t.Fatalf("点击没有折叠思考块（owners=%v）", m.owners)
	}
	for _, msg := range click {
		send(m, msg)
	}
	if m.blocks[0].Collapsed {
		t.Fatal("再次点击没有展开")
	}
}

// TestJumpBetweenAnswers：Ctrl+↑/↓ 在「问/答」锚点间跳。
func TestJumpBetweenAnswers(t *testing.T) {
	m, _ := newTestModel(60, 12)
	send(m, LineMsg{Kind: KindUser, Text: "问题一"})
	send(m, KindMsg{})
	send(m, LineMsg{Kind: KindText, Text: "回答一"})
	send(m, LineMsg{Kind: KindUser, Text: "问题二"})
	send(m, LineMsg{Kind: KindText, Text: "回答二"})
	m.View()

	if len(m.anchors) < 4 {
		t.Fatalf("锚点太少: %+v", m.anchors)
	}
	send(m, key(tea.KeyCtrlUp))
	if m.selBlock < 0 {
		t.Fatal("Ctrl+↑ 没有选中任何块")
	}
	first := m.selBlock
	send(m, key(tea.KeyCtrlUp))
	if m.selBlock >= first {
		t.Fatalf("第二次 Ctrl+↑ 应向上跳：%d → %d", first, m.selBlock)
	}
	// 跳转后目标行必须可见：视口偏移不应超过该锚点行
	for _, a := range m.anchors {
		if a.block == m.selBlock && m.vp.YOffset > a.line {
			t.Fatalf("跳转后目标行不可见: offset=%d anchor=%d", m.vp.YOffset, a.line)
		}
	}
	// 到顶后继续按不应回绕
	for i := 0; i < 5; i++ {
		send(m, key(tea.KeyCtrlUp))
	}
	if m.selBlock != 0 {
		t.Fatalf("应停在最上面一条，实际 block=%d", m.selBlock)
	}
	send(m, key(tea.KeyCtrlDown))
	if m.selBlock == 0 {
		t.Fatal("Ctrl+↓ 没有向下跳")
	}
}

// TestWrapWideLines：CJK 行必须按显示宽度折行（2 列/字），否则会被截断。
func TestWrapWideLines(t *testing.T) {
	line := strings.Repeat("中", 20) // 40 列
	got := wrapLine(line, 10)
	if len(got) != 4 {
		t.Fatalf("20 个汉字在 10 列下应折成 4 行，实际 %d", len(got))
	}
	for _, ln := range got {
		if w := ansi.StringWidth(ln); w > 10 {
			t.Fatalf("折行后仍有 %d 列: %q", w, ln)
		}
	}
	// ASCII 短行不折
	if got := wrapLine("abc", 10); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("短行被折了: %v", got)
	}
	// ANSI 颜色不能被截断逻辑吃掉（只断言没有被截短）
	colored := "\x1b[31m" + strings.Repeat("x", 30) + "\x1b[0m"
	if got := wrapLine(colored, 10); len(got) != 3 {
		t.Fatalf("ANSI 行折行数 = %d: %v", len(got), got)
	}
}

// TestInputLineWideBackspace：宽字符退格删一个字、不是半个字节。
func TestInputLineWideBackspace(t *testing.T) {
	var in inputLine
	in.insertString("a中b")
	in.backspace() // 删 b
	if in.text() != "a中" {
		t.Fatalf("退格后 = %q", in.text())
	}
	in.backspace() // 删 中
	if in.text() != "a" {
		t.Fatalf("宽字符退格后 = %q", in.text())
	}
	in.left()
	in.insertString("X")
	if in.text() != "Xa" {
		t.Fatalf("左移后插入 = %q", in.text())
	}
	in.killToEnd()
	if in.text() != "X" {
		t.Fatalf("Ctrl+K 后 = %q", in.text())
	}
}

// TestInputViewKeepsCaretVisible：长输入时光标必须留在可见范围内。
func TestInputViewKeepsCaretVisible(t *testing.T) {
	var in inputLine
	in.insertString(strings.Repeat("长", 50))
	got := in.view("› ", 20)
	if w := ansi.StringWidth(ansi.Strip(got)); w > 20 {
		t.Fatalf("输入行宽 %d 超过 20: %q", w, ansi.Strip(got))
	}
	if !strings.Contains(got, "\x1b[7m") {
		t.Fatal("没有渲染光标")
	}
}

// TestSinkSplitsLines：字节流切成整行 + 未完成尾巴，且带当前块类型。
func TestSinkSplitsLines(t *testing.T) {
	ui := New(Options{Header: "x"})
	ui.SetBlockKind(KindThinking)
	s := ui.Sink()
	if _, err := s.Write([]byte("第一行\n第二")); err != nil {
		t.Fatal(err)
	}
	s = ui.Sink() // 新的 sink 不应继承旧缓冲
	if _, err := s.Write([]byte("行\n")); err != nil {
		t.Fatal(err)
	}
	var lines []LineMsg
	var partials []PartialMsg
	for _, p := range ui.pending {
		switch msg := p.(type) {
		case LineMsg:
			lines = append(lines, msg)
		case PartialMsg:
			partials = append(partials, msg)
		}
	}
	if len(lines) == 0 || lines[0].Text != "第一行" || lines[0].Kind != KindThinking {
		t.Fatalf("第一条应是 thinking 整行: %+v", lines)
	}
	if len(partials) != 1 || partials[0].Text != "第二" {
		t.Fatalf("应有一个未完成行「第二」: %+v", partials)
	}
}

// TestAskPermission answer 写回通道；非 y/a/n/d 的按键被忽略。
func TestAskPermissionKeys(t *testing.T) {
	m, _ := newTestModel(60, 16)
	reply := make(chan string, 1)
	send(m, AskMsg{Tool: "bash", Reply: reply})
	if m.perm == nil {
		t.Fatal("没有记录待确认请求")
	}
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	select {
	case c := <-reply:
		t.Fatalf("无关按键不应回答问题: %q", c)
	default:
	}
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	select {
	case c := <-reply:
		if c != "a" {
			t.Fatalf("回答 = %q", c)
		}
	default:
		t.Fatal("按键没有写回回答")
	}
	if m.perm != nil {
		t.Fatal("回答后应清除确认状态")
	}
}

// TestEscClearsInputThenQuits 语义：空闲 Esc 清输入；Ctrl+C 先清输入再退出。
func TestEscClearsInput(t *testing.T) {
	m, _ := newTestModel(60, 16)
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	send(m, key(tea.KeyEsc))
	if !m.in.empty() {
		t.Fatalf("Esc 没清空输入: %q", m.in.text())
	}
	cmd := m.onKey(key(tea.KeyCtrlC))
	if cmd == nil {
		t.Fatal("输入为空时 Ctrl+C 应退出")
	}
}
