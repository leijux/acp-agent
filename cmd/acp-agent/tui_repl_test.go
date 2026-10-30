package main

import "testing"

// 运行中的普通输入必须变成 steering（插话），而不是排到下一轮。
//
// 这条语义坏了不会有任何报错：界面上照常回显、消息也照常成轮，只是延迟了
// 一整轮才生效 —— 所以单独测这个决策。
func TestDecideSubmit(t *testing.T) {
	cases := []struct {
		name  string
		input string
		busy  bool
		want  submitAction
	}{
		{"空闲普通输入开新一轮", "帮我改个 bug", false, submitTurn},
		{"运行中普通输入插话", "顺便看下 README", true, submitSteer},
		{"空闲命令就地执行", "/model deepseek-flash", false, submitCommand},
		{"运行中命令仍按命令处理", "/exit", true, submitCommand},
		{"斜杠开头的普通文本也是命令", "/tmp 在哪个盘", false, submitCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideSubmit(tc.input, tc.busy); got != tc.want {
				t.Fatalf("decideSubmit(%q, busy=%v) = %v, want %v", tc.input, tc.busy, got, tc.want)
			}
		})
	}
}
