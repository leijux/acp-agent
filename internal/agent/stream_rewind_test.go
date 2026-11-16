package agent

import (
	"context"
	"testing"
)

// probeProvider 把收到的 sink 交给测试观察（stubProvider 拿不到 sink）。
type probeProvider struct {
	fn func(sink Sink) (*AssistantMessage, error)
}

func (p *probeProvider) Stream(ctx context.Context, req Request, sink Sink) (*AssistantMessage, error) {
	return p.fn(sink)
}

// 默认不向 provider 暴露 Reset（ACP 客户端撤回不了已发出的增量）；
// SetStreamRewind(true) 之后才暴露，provider 才能在重发前撤销半截内容。
func TestSetStreamRewindExposesResetter(t *testing.T) {
	var resettable []bool
	prov := &probeProvider{fn: func(sink Sink) (*AssistantMessage, error) {
		_, ok := sink.(Resetter)
		resettable = append(resettable, ok)
		return reply("ok"), nil
	}}
	a := newTestAgent(t, prov)
	ctx := context.Background()

	if _, err := a.Run(ctx, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	a.SetStreamRewind(true)
	if _, err := a.Run(ctx, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(resettable) != 2 {
		t.Fatalf("请求次数 = %d", len(resettable))
	}
	if resettable[0] {
		t.Error("默认不该暴露 Reset：客户端撤回不了已发出的增量")
	}
	if !resettable[1] {
		t.Error("SetStreamRewind(true) 之后应暴露 Reset")
	}
}

// Reset 必须清空半截内容并发出 MessageRewindEvent，让下游把屏幕上那段擦掉；
// 之后重发的增量重新从 MessageStartEvent 开始。
func TestStreamRewindClearsPartialAndEmitsEvent(t *testing.T) {
	prov := &probeProvider{fn: func(sink Sink) (*AssistantMessage, error) {
		sink.Text("半截")
		sink.Thinking("想想")
		rs, ok := sink.(Resetter)
		if !ok {
			t.Fatal("Reset 能力未暴露")
		}
		rs.Reset()
		sink.Text("答复")
		return reply("答复"), nil
	}}
	a := newTestAgent(t, prov)
	a.SetStreamRewind(true)

	var (
		starts  int
		rewinds int
		deltas  []string
	)
	a.On(func(ev Event) {
		switch e := ev.(type) {
		case MessageStartEvent:
			starts++
		case MessageRewindEvent:
			rewinds++
			// 回滚时 partial 已被清空：下游拿到的是空消息，只用于定位。
			if am, ok := e.Message.(*AssistantMessage); ok && len(am.Content) > 0 {
				t.Errorf("回滚事件里的内容应已清空: %+v", am.Content)
			}
		case MessageUpdateEvent:
			deltas = append(deltas, e.Delta)
		}
	})

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rewinds != 1 {
		t.Errorf("MessageRewindEvent 次数 = %d, want 1", rewinds)
	}
	if starts != 2 {
		t.Errorf("MessageStartEvent 次数 = %d, want 2（重发后重新开始）", starts)
	}
	if len(deltas) != 3 {
		t.Fatalf("增量 = %v", deltas)
	}
}
