package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
)

// ---- DuckDuckGo（无需 key） -------------------------------------------------
//
// 用 html.duckduckgo.com 的表单端点。它的结果是服务端渲染的 HTML，
// 这里只抓 result__a（标题+链接）与 result__snippet（摘要）。
// 注：DDG 会对可疑流量返回验证页，此时返回「无结果」而不是崩溃。

func (c *Client) searchDuckDuckGo(ctx context.Context, query string, max int) ([]Hit, error) {
	endpoint := c.cfg.BaseURL
	if endpoint == "" {
		endpoint = "https://html.duckduckgo.com"
	}
	form := url.Values{}
	form.Set("q", query)
	form.Set("kl", "wt-wt")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.fcfg.UserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	body, err := c.doText(req, 4<<20)
	if err != nil {
		return nil, err
	}
	hits := parseDuckDuckGo(body)
	if len(hits) == 0 && looksLikeDuckDuckGoChallenge(body) {
		return nil, errors.New("duckduckgo 触发了人机校验，暂时拿不到结果：换 --web-search tavily/brave/exa，或配 searxng 自建实例")
	}
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits, nil
}

// looksLikeDuckDuckGoChallenge 识别 DDG 的限流/验证页。
//
// 它不返回 4xx，而是一个看似正常的 HTML 页面，所以只能看内容。
// 不识别出来的话，模型会以为「真的没有结果」而反复改词重试。
func looksLikeDuckDuckGoChallenge(page string) bool {
	lower := strings.ToLower(page)
	for _, marker := range []string{"anomaly-modal", "challenge-form", "unusual traffic", "captcha", "are you a robot"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseDuckDuckGo 从 DDG 的 HTML 里抽结果。
func parseDuckDuckGo(page string) []Hit {
	var hits []Hit
	rest := page
	for {
		i := strings.Index(rest, "result__a")
		if i < 0 {
			break
		}
		rest = rest[i:]
		href := attrValue(rest, "href")
		text, after := tagInner(rest)
		rest = after
		if href == "" || text == "" {
			continue
		}
		hit := Hit{
			Title: normalizeSpace(htmlUnescape(text)),
			URL:   unwrapDuckDuckGoURL(href),
		}
		// 摘要紧跟在标题后面，可包含 <b> 高亮，所以取到 </a> 为止再去标签。
		if j := strings.Index(rest, "result__snippet"); j >= 0 && j < 4000 {
			if snip, ok := anchorText(rest[j:]); ok {
				hit.Snippet = normalizeSpace(strings.TrimSpace(stripTags(htmlUnescape(snip))))
			}
			rest = rest[j:]
		}
		if hit.URL == "" || strings.HasPrefix(hit.URL, "javascript:") {
			continue
		}
		hits = append(hits, hit)
		if len(hits) >= 30 {
			break
		}
	}
	return hits
}

// unwrapDuckDuckGoURL 还原 DDG 的跳转链接 //duckduckgo.com/l/?uddg=<encoded>。
func unwrapDuckDuckGoURL(href string) string {
	href = htmlUnescape(strings.TrimSpace(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		if dec, err := url.QueryUnescape(target); err == nil {
			return dec
		}
		return target
	}
	return href
}

// ---- SearxNG（需 baseUrl） --------------------------------------------------

func (c *Client) searchSearxNG(ctx context.Context, query string, max int) ([]Hit, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("language", "auto")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.fcfg.UserAgent)
	req.Header.Set("Accept", "application/json")

	body, err := c.doJSON(req, 4<<20)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("searxng 返回的不是 JSON（该实例可能没开 format=json）：%w", err)
	}
	hits := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.URL == "" {
			continue
		}
		hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: normalizeSpace(r.Content)})
		if len(hits) >= max {
			break
		}
	}
	return hits, nil
}

// ---- Tavily -----------------------------------------------------------------

func (c *Client) searchTavily(ctx context.Context, query string, max int) ([]Hit, error) {
	endpoint := c.cfg.BaseURL
	if endpoint == "" {
		endpoint = "https://api.tavily.com"
	}
	payload := map[string]any{
		"query":        query,
		"max_results":  max,
		"search_depth": "basic",
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/search", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	body, err := c.doJSON(req, 4<<20)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("tavily 响应解析失败：%w", err)
	}
	hits := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.URL == "" {
			continue
		}
		hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: normalizeSpace(r.Content)})
	}
	return hits, nil
}

// ---- Brave ------------------------------------------------------------------

func (c *Client) searchBrave(ctx context.Context, query string, max int) ([]Hit, error) {
	endpoint := c.cfg.BaseURL
	if endpoint == "" {
		endpoint = "https://api.search.brave.com"
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprint(max))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/res/v1/web/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", c.cfg.APIKey)

	body, err := c.doJSON(req, 4<<20)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("brave 响应解析失败：%w", err)
	}
	hits := make([]Hit, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		if r.URL == "" {
			continue
		}
		hits = append(hits, Hit{
			Title:   normalizeSpace(stripTags(htmlUnescape(r.Title))),
			URL:     r.URL,
			Snippet: normalizeSpace(stripTags(htmlUnescape(r.Description))),
		})
	}
	return hits, nil
}

// ---- Exa --------------------------------------------------------------------

func (c *Client) searchExa(ctx context.Context, query string, max int) ([]Hit, error) {
	endpoint := c.cfg.BaseURL
	if endpoint == "" {
		endpoint = "https://api.exa.ai"
	}
	payload := map[string]any{
		"query":      query,
		"numResults": max,
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": 500}},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/search", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)

	body, err := c.doJSON(req, 8<<20)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("exa 响应解析失败：%w", err)
	}
	hits := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.URL == "" {
			continue
		}
		hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: normalizeSpace(r.Text)})
	}
	return hits, nil
}

// ---- HTTP 辅助 --------------------------------------------------------------

// doJSON 发请求并把非 2xx 转成带响应片段的错误。
func (c *Client) doJSON(req *http.Request, limit int64) ([]byte, error) {
	body, status, err := c.do(req, limit)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("%s %s 返回 HTTP %d：%s", req.Method, req.URL.Host, status, clipRunes(strings.TrimSpace(string(body)), 300))
	}
	return body, nil
}

// doText 同 doJSON，但把响应体当文本返回。
func (c *Client) doText(req *http.Request, limit int64) (string, error) {
	body, status, err := c.do(req, limit)
	if err != nil {
		return "", err
	}
	text, derr := decodeCharset(body, "")
	if derr != nil {
		text = string(body)
	}
	if status >= 400 {
		return "", fmt.Errorf("%s %s 返回 HTTP %d", req.Method, req.URL.Host, status)
	}
	return text, nil
}

func (c *Client) do(req *http.Request, limit int64) ([]byte, int, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, humanizeNetErr(err)
	}
	defer resp.Body.Close()
	body, _, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// ---- HTML 小工具 ------------------------------------------------------------

// attrValue 从标签文本里取属性值（取第一个匹配的属性）。
func attrValue(tag, name string) string {
	lower := strings.ToLower(tag)
	idx := strings.Index(lower, strings.ToLower(name)+"=")
	if idx < 0 {
		return ""
	}
	rest := tag[idx+len(name)+1:]
	if rest == "" {
		return ""
	}
	var quote byte
	rest = strings.TrimLeft(rest, " ")
	if rest[0] == '"' || rest[0] == '\'' {
		quote = rest[0]
		rest = rest[1:]
		if j := strings.IndexByte(rest, quote); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	j := strings.IndexAny(rest, " \t\r\n>")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// tagInner 找到下一个 '>' 之后的文本直到下一个 '<'，返回文本与剩余串。
func tagInner(s string) (string, string) {
	gt := strings.IndexByte(s, '>')
	if gt < 0 {
		return "", ""
	}
	rest := s[gt+1:]
	if strings.HasPrefix(rest, "<") {
		// 标签之间没有文字（例如图标 + span）。
		return "", rest
	}
	lt := strings.IndexByte(rest, '<')
	if lt < 0 {
		return rest, ""
	}
	return rest[:lt], rest[lt:]
}

// anchorText 返回从标签起点到其 </a> 之间的全部内容（含内层标签）。
func anchorText(s string) (string, bool) {
	gt := strings.IndexByte(s, '>')
	if gt < 0 {
		return "", false
	}
	rest := s[gt+1:]
	if j := strings.Index(strings.ToLower(rest), "</a>"); j >= 0 {
		return rest[:j], true
	}
	return rest, true
}

// stripTags 去掉字符串里的标签（用于 Brave 的 title/description）。
func stripTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '<' {
			if j := strings.IndexByte(s[i:], '>'); j >= 0 {
				i += j + 1
				continue
			}
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func htmlUnescape(s string) string { return html.UnescapeString(s) }
