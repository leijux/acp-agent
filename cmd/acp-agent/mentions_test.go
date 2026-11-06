package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureModel 记录每次请求体，并直接回一句话结束这一轮。
func captureModel(t *testing.T, bodies *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(
			`{"id":"r1","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"收到。"}}]}`,
			`{"id":"r1","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"r1","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// @文件 必须在进模型之前展开成 <file> 块；原文（含 @token）要保留。
func TestRunOnceExpandsMentions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("你好，世界\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := captureModel(t, &bodies)

	cfg := Config{Provider: "deepseek", Model: "deepseek-flash", Root: dir}
	a, _ := newTestApp(t, cfg, srv.URL)
	if err := a.runOnce(context.Background(), "看下 @hello.txt 里写了什么"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if len(bodies) == 0 {
		t.Fatal("模型没收到请求")
	}
	body := bodies[0]
	// 请求体是 JSON，`<` 会被转义成 `\u003c`、引号会变成 `\"`，所以只断言不受转义影响的片段。
	if !strings.Contains(body, `path=\"hello.txt\"`) {
		t.Errorf("请求体里没有 file 块:\n%s", body)
	}
	if !strings.Contains(body, "attachments") {
		t.Errorf("请求体里没有 attachments 包装:\n%s", body)
	}
	if !strings.Contains(body, "你好，世界") {
		t.Errorf("请求体里没有文件内容:\n%s", body)
	}
	if !strings.Contains(body, "@hello.txt") {
		t.Errorf("正文里的 @token 原文应保留:\n%s", body)
	}
}

// --no-mentions 是逃生口：一个字都不该动。
func TestRunOnceNoMentions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("机密\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := captureModel(t, &bodies)

	cfg := Config{Provider: "deepseek", Model: "deepseek-flash", Root: dir, NoMentions: true}
	a, _ := newTestApp(t, cfg, srv.URL)
	if err := a.runOnce(context.Background(), "看下 @hello.txt"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if len(bodies) == 0 {
		t.Fatal("模型没收到请求")
	}
	if strings.Contains(bodies[0], "<attachments>") || strings.Contains(bodies[0], "机密") {
		t.Errorf("--no-mentions 下不应展开:\n%s", bodies[0])
	}
}

// 不存在的路径只给警告，prompt 原样发出去（不能让整个任务失败）。
func TestRunOnceMentionMissingStillRuns(t *testing.T) {
	var bodies []string
	srv := captureModel(t, &bodies)
	cfg := Config{Provider: "deepseek", Model: "deepseek-flash", Root: t.TempDir()}
	a, buf := newTestApp(t, cfg, srv.URL)
	if err := a.runOnce(context.Background(), "看下 @nope.go"); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !strings.Contains(buf.String(), "不存在") {
		t.Errorf("应提示 @nope.go 不存在: %q", buf.String())
	}
	if len(bodies) == 0 || !strings.Contains(bodies[0], "@nope.go") {
		t.Errorf("prompt 应原样发出:\n%v", bodies)
	}
}
