package main

import (
	"context"
	"fmt"
	"io"

	"gitlab.com/leijux/acp-agent/internal/tools"
	"gitlab.com/leijux/acp-agent/internal/web"
)

// buildWebProviders 依据配置构造 web_search / web_fetch 的后端。
//
// 返回 usable=false 的情况：配置为 off，或配置非法（例如 tavily 缺 key）。
// 两种情况下工具都应该从注册表里摘掉 —— 让模型看到一个「调用必失败」的
// 工具只会诱发无效重试。
func buildWebProviders(cfg Config, out io.Writer) (search tools.SearchProvider, fetch tools.FetchProvider, usable bool) {
	if cfg.Web == nil {
		return nil, nil, false
	}
	if cfg.Web.Search.Provider == web.ProviderOff {
		return nil, nil, false
	}
	client, err := web.New(*cfg.Web)
	if err != nil {
		fmt.Fprintf(out, "web 能力已关闭：%v\n", err)
		return nil, nil, false
	}
	if client.Provider() == web.ProviderOff {
		return nil, nil, false
	}

	search = func(ctx context.Context, query string, max int) ([]tools.SearchHit, error) {
		hits, err := client.Search(ctx, query, max)
		if err != nil {
			return nil, err
		}
		out := make([]tools.SearchHit, 0, len(hits))
		for _, h := range hits {
			out = append(out, tools.SearchHit{Title: h.Title, URL: h.URL, Snippet: h.Snippet})
		}
		return out, nil
	}
	fetch = func(ctx context.Context, url string) (string, error) {
		return client.Fetch(ctx, url)
	}
	return search, fetch, true
}

// registryDisable 汇总要从注册表里摘掉的工具名。
func registryDisable(cfg Config, webUsable bool) []string {
	out := append([]string(nil), cfg.DisableTools...)
	if !webUsable {
		out = append(out, "web_search", "web_fetch")
	}
	return out
}
