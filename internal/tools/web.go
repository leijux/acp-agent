package tools

import (
	"context"
	"fmt"
	"strings"
)

// ---- 外部能力注入点 -------------------------------------------------------
//
// 网络与子代理能力不由本包实现，而是通过函数注入：
// 这样 internal/tools 保持零外部依赖、可测试，接入方自由选择 provider。

// SearchHit 是一条搜索结果。
type SearchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// SearchProvider 执行搜索。由宿主注入（Brave/Tavily/Exa/自建…）。
type SearchProvider func(ctx context.Context, query string, maxResults int) ([]SearchHit, error)

// FetchProvider 抓取 URL 正文（markdown 或纯文本）。由宿主注入。
type FetchProvider func(ctx context.Context, url string) (string, error)

// ---------------------------------------------------------------------------
// web_search
// ---------------------------------------------------------------------------

// WebSearchTool 让模型能检索网络。
type WebSearchTool struct{ Provider SearchProvider }

// NewWebSearchTool 创建工具；provider 为空时调用返回「未配置」错误。
func NewWebSearchTool(p SearchProvider) *WebSearchTool { return &WebSearchTool{Provider: p} }

func (*WebSearchTool) Name() string { return "web_search" }
func (*WebSearchTool) Description() string {
	return "搜索网络获取最新信息。需要多个角度时一次给出多条 query，而不是反复调用单条。" +
		"需要全文时再用 web_fetch。"
}
func (*WebSearchTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"query":       Prop("string", "单条查询"),
		"queries":     PropArray("string", "多条不同角度的查询（优先）"),
		"numResults":  PropInt("每条查询返回结果数", 5),
		"recencyDays": PropInt("只保留最近 N 天的结果（0 不限）", 0),
	})
}
func (*WebSearchTool) ConcurrencySafe() bool { return true }
func (*WebSearchTool) Mutates() bool         { return false }

func (t *WebSearchTool) Execute(ctx context.Context, a Args) (Result, error) {
	if t.Provider == nil {
		return ErrorResult("web_search 未配置 provider（宿主需注入 tools.SearchProvider）"), nil
	}
	queries := a.StrSlice("queries")
	if q, ok := a.String("query"); ok && strings.TrimSpace(q) != "" {
		queries = append(queries, q)
	}
	if len(queries) == 0 {
		return ErrorResult("需要 query 或 queries"), nil
	}
	if len(queries) > 4 {
		queries = queries[:4]
	}
	n := int(a.Int("numResults", 5))
	if n <= 0 || n > 20 {
		n = 5
	}

	type res struct {
		q    string
		hits []SearchHit
		err  error
	}
	out := make([]res, len(queries))
	var wg = make(chan struct{}, len(queries))
	for i, q := range queries {
		i, q := i, q
		go func() {
			hits, err := t.Provider(ctx, q, n)
			out[i] = res{q: q, hits: hits, err: err}
			wg <- struct{}{}
		}()
	}
	for range queries {
		select {
		case <-wg:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	var b strings.Builder
	var allHits []SearchHit
	failed := 0
	for _, r := range out {
		fmt.Fprintf(&b, "## %s\n", r.q)
		if r.err != nil {
			failed++
			fmt.Fprintf(&b, "(搜索失败: %v)\n\n", r.err)
			continue
		}
		if len(r.hits) == 0 {
			b.WriteString("(无结果)\n\n")
			continue
		}
		for i, h := range r.hits {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
			if h.Snippet != "" {
				fmt.Fprintf(&b, "   %s\n", clip(strings.ReplaceAll(h.Snippet, "\n", " "), 300))
			}
			allHits = append(allHits, h)
		}
		b.WriteString("\n")
	}
	text, trunc := Truncate(b.String(), 0, 0)
	// 全部查询都失败 → 标记为工具错误，让 agent loop / UI 看到失败，
	// 而不是把一段「搜索失败」文字当成正常结果。部分失败仍算成功（正文已注明）。
	if failed == len(out) && failed > 0 {
		return Result{
			Content: []Block{{Kind: KindText, Text: text}},
			Details: map[string]any{"queries": queries, "results": 0, "failed": failed},
			IsError: true,
		}, nil
	}
	return Result{
		Content:   []Block{{Kind: KindText, Text: text}},
		Truncated: trunc,
		Details:   map[string]any{"queries": queries, "results": len(allHits)},
	}, nil
}

// ---------------------------------------------------------------------------
// web_fetch
// ---------------------------------------------------------------------------

// WebFetchTool 抓取网页正文。
type WebFetchTool struct{ Provider FetchProvider }

// NewWebFetchTool 创建工具。
func NewWebFetchTool(p FetchProvider) *WebFetchTool { return &WebFetchTool{Provider: p} }

func (*WebFetchTool) Name() string { return "web_fetch" }
func (*WebFetchTool) Description() string {
	return "抓取 URL 正文并返回可读文本（自动截断）。用于读取 web_search 找到的页面。"
}
func (*WebFetchTool) Parameters() map[string]any {
	return Schema(map[string]any{
		"url":     Prop("string", "要抓取的 URL"),
		"maxChar": PropInt("最多返回字符数", 20000),
	}, "url")
}
func (*WebFetchTool) ConcurrencySafe() bool { return true }
func (*WebFetchTool) Mutates() bool         { return false }

func (t *WebFetchTool) Execute(ctx context.Context, a Args) (Result, error) {
	if t.Provider == nil {
		return ErrorResult("web_fetch 未配置 provider（宿主需注入 tools.FetchProvider）"), nil
	}
	u, err := a.Required("url")
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ErrorResult("只支持 http/https URL"), nil
	}
	body, err := t.Provider(ctx, u)
	if err != nil {
		return ErrorResult("抓取失败：%v", err), nil
	}
	maxChar := int(a.Int("maxChar", 20000))
	if maxChar <= 0 {
		maxChar = 20000
	}
	out, trunc := Truncate(body, 0, maxChar)
	return Result{
		Content:   []Block{{Kind: KindText, Text: out}},
		Truncated: trunc,
		Details:   map[string]any{"url": u, "chars": len(body)},
	}, nil
}
