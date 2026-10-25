package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// web_search / web_fetch 只有一个壳：验证壳本身的参数解析与错误处理，
// 真正的网络实现在 internal/web（那里有自己的测试）。

func TestWebSearchTool_InjectedProvider(t *testing.T) {
	var gotQuery string
	var gotMax int
	tool := NewWebSearchTool(func(ctx context.Context, q string, max int) ([]SearchHit, error) {
		gotQuery, gotMax = q, max
		return []SearchHit{
			{Title: "Go 文档", URL: "https://go.dev", Snippet: "官方文档"},
			{Title: "无摘要", URL: "https://example.com"},
		}, nil
	})

	r, err := tool.Execute(context.Background(), Args{"query": "go 文档", "numResults": 7})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("意外错误：%s", r.Text())
	}
	if gotQuery != "go 文档" || gotMax != 7 {
		t.Fatalf("provider 收到 query=%q max=%d", gotQuery, gotMax)
	}
	for _, want := range []string{"Go 文档", "https://go.dev", "官方文档", "https://example.com"} {
		if !strings.Contains(r.Text(), want) {
			t.Fatalf("结果缺少 %q：\n%s", want, r.Text())
		}
	}
}

func TestWebSearchTool_NoProvider(t *testing.T) {
	r, err := NewWebSearchTool(nil).Execute(context.Background(), Args{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || !strings.Contains(r.Text(), "未配置 provider") {
		t.Fatalf("r = %+v", r)
	}
}

func TestWebSearchTool_ProviderError(t *testing.T) {
	tool := NewWebSearchTool(func(ctx context.Context, q string, max int) ([]SearchHit, error) {
		return nil, errors.New("quota exceeded")
	})
	r, err := tool.Execute(context.Background(), Args{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || !strings.Contains(r.Text(), "quota exceeded") {
		t.Fatalf("全部查询失败应标记为工具错误：%+v", r)
	}
}

func TestWebSearchTool_EmptyResults(t *testing.T) {
	tool := NewWebSearchTool(func(ctx context.Context, q string, max int) ([]SearchHit, error) {
		return nil, nil
	})
	r, err := tool.Execute(context.Background(), Args{"query": "无结果查询"})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("空结果是正常情况，不该是错误：%s", r.Text())
	}
}

func TestWebFetchTool_InjectedProvider(t *testing.T) {
	var gotURL string
	tool := NewWebFetchTool(func(ctx context.Context, u string) (string, error) {
		gotURL = u
		return "[URL] " + u + "\n\n正文", nil
	})

	r, err := tool.Execute(context.Background(), Args{"url": "https://example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError || gotURL != "https://example.com/x" {
		t.Fatalf("r=%+v url=%q", r, gotURL)
	}
	if !strings.Contains(r.Text(), "正文") {
		t.Fatalf("r = %s", r.Text())
	}
}

func TestWebFetchTool_Errors(t *testing.T) {
	if r, _ := NewWebFetchTool(nil).Execute(context.Background(), Args{"url": "https://x"}); !r.IsError {
		t.Fatal("未配置 provider 应报错")
	}

	tool := NewWebFetchTool(func(ctx context.Context, u string) (string, error) {
		return "", errors.New("连接被拒绝")
	})
	r, _ := tool.Execute(context.Background(), Args{"url": "https://x"})
	if !r.IsError || !strings.Contains(r.Text(), "连接被拒绝") {
		t.Fatalf("r = %+v", r)
	}
}
