// Package web 给 web_search / web_fetch 提供可用的后端。
//
// 它不依赖 internal/tools：返回自己的 Hit 类型，由宿主适配成
// tools.SearchHit / tools.FetchProvider。这样 tools 包保持零网络依赖。
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Provider 名字。
const (
	ProviderDuckDuckGo = "duckduckgo" // 无需 key
	ProviderSearxNG    = "searxng"    // 需要 baseUrl（自建或公共实例）
	ProviderTavily     = "tavily"     // 需要 apiKey
	ProviderBrave      = "brave"      // 需要 apiKey
	ProviderExa        = "exa"        // 需要 apiKey
	ProviderOff        = "off"        // 关闭：Search 直接报「未启用」
)

// Defaults。
const (
	DefaultNumResults = 5
	DefaultTimeout    = 20 * time.Second
	DefaultMaxBytes   = 5 << 20
	// DefaultMaxChars 是提取后的正文字符上限：5MiB 的 HTML 展开成纯文本
	// 可能有十几万字，直接吞掉模型上下文，所以必须单独限一次。
	DefaultMaxChars  = 40000
	DefaultUserAgent = "Mozilla/5.0 (compatible; acp-agent/0.1; +https://gitlab.com/leijux/acp-agent)"
)

// Config 是 web 能力的配置。
type Config struct {
	Search SearchConfig `json:"search"`
	Fetch  FetchConfig  `json:"fetch"`
}

// SearchConfig 配置搜索后端。
type SearchConfig struct {
	// Provider 取 duckduckgo / searxng / tavily / brave / exa / off，缺省 duckduckgo。
	Provider string `json:"provider"`
	// APIKey 用于 tavily / brave / exa。
	APIKey string `json:"apiKey"`
	// BaseURL 用于 searxng（形如 https://searx.example.com），也可覆盖其它后端。
	BaseURL string `json:"baseUrl"`
	// NumResults 是单次查询默认返回条数（1..20）。
	NumResults int `json:"numResults"`
	// TimeoutMs 单次请求超时。
	TimeoutMs int `json:"timeoutMs"`
}

// FetchConfig 配置抓取行为。
type FetchConfig struct {
	// TimeoutMs 单次抓取超时，缺省 20s。
	TimeoutMs int `json:"timeoutMs"`
	// MaxBytes 响应体上限，缺省 5MiB。超过部分被丢弃并标记截断。
	MaxBytes int64 `json:"maxBytes"`
	// UserAgent 覆盖默认 UA。
	UserAgent string `json:"userAgent"`
	// AllowPrivate 允许抓取回环/私网/链路本地地址，缺省 false。
	//
	// 默认拒绝的理由：web_fetch 的入参来自模型，而模型可能被网页里的提示注入
	// 操纵，把「读这个网页」变成「读 http://169.254.169.254/latest/meta-data/」
	// 或「读 http://localhost:8080/admin」。bash 也能 curl，但 web_fetch 看起来
	// 无害、更容易被当成数据通道。
	AllowPrivate bool `json:"allowPrivate"`
	// MaxChars 提取后的文本上限（按 rune 计），缺省 40000。
	// 字节上限只能防住「下载太多」，防不住「HTML 展开成几十万字的正文」——
	// 后者会直接把模型上下文撑爆，所以两者都要有。
	MaxChars int `json:"maxChars"`
}

// Hit 是一条搜索结果。
type Hit struct {
	Title   string
	URL     string
	Snippet string
}

// Client 实现搜索与抓取。
type Client struct {
	cfg  SearchConfig
	fcfg FetchConfig
	// hc 用于搜索后端：自建 searxng 常常就在 localhost，不能拦内网。
	hc *http.Client
	// fetchHC 用于 web_fetch：额外装了内网地址过滤（含 DNS rebinding 防护）。
	fetchHC *http.Client
}

// New 校验配置并构造 Client。
//
// provider=off 时仍返回可用对象（Search 会报未启用），方便宿主统一接线。
func New(cfg Config) (*Client, error) {
	sc := cfg.Search
	sc.Provider = strings.ToLower(strings.TrimSpace(sc.Provider))
	if sc.Provider == "" {
		sc.Provider = ProviderDuckDuckGo
	}
	switch sc.Provider {
	case ProviderDuckDuckGo, ProviderOff:
	case ProviderSearxNG:
		if strings.TrimSpace(sc.BaseURL) == "" {
			return nil, errors.New("searxng 需要配置 baseUrl")
		}
	case ProviderTavily, ProviderBrave, ProviderExa:
		if strings.TrimSpace(sc.APIKey) == "" {
			return nil, fmt.Errorf("%s 需要配置 apiKey", sc.Provider)
		}
	default:
		return nil, fmt.Errorf("未知的搜索 provider %q（可用：duckduckgo/searxng/tavily/brave/exa/off）", sc.Provider)
	}
	if sc.NumResults <= 0 || sc.NumResults > 20 {
		sc.NumResults = DefaultNumResults
	}
	if sc.BaseURL != "" {
		sc.BaseURL = strings.TrimRight(sc.BaseURL, "/")
	}

	fc := cfg.Fetch
	if fc.TimeoutMs <= 0 {
		fc.TimeoutMs = int(DefaultTimeout / time.Millisecond)
	}
	if fc.MaxBytes <= 0 {
		fc.MaxBytes = DefaultMaxBytes
	}
	if fc.UserAgent == "" {
		fc.UserAgent = DefaultUserAgent
	}
	if fc.MaxChars <= 0 {
		fc.MaxChars = DefaultMaxChars
	}

	timeout := time.Duration(fc.TimeoutMs) * time.Millisecond
	return &Client{
		cfg:  sc,
		fcfg: fc,
		hc: &http.Client{
			Timeout:       timeout,
			CheckRedirect: redirectLimit,
		},
		fetchHC: &http.Client{
			Timeout:       timeout,
			CheckRedirect: redirectLimit,
			Transport:     newGuardedTransport(fc.AllowPrivate, timeout),
		},
	}, nil
}

// redirectLimit 限制重定向次数（防止恶意站点把 agent 拖进长链）。
func redirectLimit(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("重定向超过 5 次")
	}
	return nil
}

// newGuardedTransport 构造带内网地址过滤的 Transport。
//
// 两道关：
//  1. fetch() 里先做一次「URL 解析后判 IP」，给模型一个明确原因；
//  2. 这里在 DialContext 里对**解析出的 IP** 再判一次 —— URL 里的域名可能
//     先解析到公网、真正连接时再解析到内网（DNS rebinding），只查 URL 拦不住。
func newGuardedTransport(allowPrivate bool, timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if allowPrivate {
		return &http.Transport{DialContext: dialer.DialContext}
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				if !isPublicIP(ip.IP) {
					lastErr = fmt.Errorf("拒绝连接 %s：解析到内网/回环地址 %s", host, ip.IP)
					continue
				}
				conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if derr == nil {
					return conn, nil
				}
				lastErr = derr
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("无法解析 %s", host)
			}
			return nil, lastErr
		},
	}
}

// isPublicIP 判断地址是否允许抓取：回环、私网、链路本地、未指定、组播一律拒绝。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// 云元数据服务（169.254.169.254）已在 LinkLocalUnicast 覆盖；
	// 这里再挡掉 NAT64/6to4 里嵌的回环与私网。
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1]&0xC0 == 64: // 100.64.0.0/10 CGNAT
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return false
		}
	}
	return true
}

// Provider 返回生效的搜索 provider 名。
func (c *Client) Provider() string { return c.cfg.Provider }

// Search 执行一次查询。max <= 0 时用配置里的默认条数。
func (c *Client) Search(ctx context.Context, query string, max int) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query 不能为空")
	}
	if max <= 0 || max > 20 {
		max = c.cfg.NumResults
	}
	switch c.cfg.Provider {
	case ProviderOff:
		return nil, errors.New("web 搜索未启用（web.search.provider=off）")
	case ProviderDuckDuckGo:
		return c.searchDuckDuckGo(ctx, query, max)
	case ProviderSearxNG:
		return c.searchSearxNG(ctx, query, max)
	case ProviderTavily:
		return c.searchTavily(ctx, query, max)
	case ProviderBrave:
		return c.searchBrave(ctx, query, max)
	case ProviderExa:
		return c.searchExa(ctx, query, max)
	}
	return nil, fmt.Errorf("未实现的 provider %q", c.cfg.Provider)
}

// Fetch 抓取 URL 并转成可读文本。
func (c *Client) Fetch(ctx context.Context, rawURL string) (string, error) {
	res, err := c.fetch(ctx, rawURL)
	if err != nil {
		return "", err
	}
	text, cut := res.Text, false
	if max := c.fcfg.MaxChars; max > 0 && utf8.RuneCountInString(text) > max {
		text, cut = clipRunes(text, max), true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[URL] %s\n[Status] %d  [Type] %s  [Bytes] %d\n\n",
		res.URL, res.StatusCode, res.ContentType, res.Bytes)
	b.WriteString(text)
	if cut || res.Truncated {
		fmt.Fprintf(&b, "\n\n……（已截断：响应 %d 字节，正文上限 %d 字符）", res.Bytes, c.fcfg.MaxChars)
	}
	return b.String(), nil
}

// FetchResult 是抓取结果（含截断标记，供宿主展示）。
type FetchResult struct {
	URL         string
	StatusCode  int
	ContentType string
	Text        string
	Bytes       int64
	Truncated   bool
}

// FetchResult 抓取并返回完整信息。
func (c *Client) FetchResult(ctx context.Context, rawURL string) (FetchResult, error) {
	return c.fetch(ctx, rawURL)
}

func (c *Client) fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return FetchResult{}, errors.New("url 不能为空")
	}
	lower := strings.ToLower(rawURL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return FetchResult{}, errors.New("只支持 http/https URL")
	}

	if !c.fcfg.AllowPrivate {
		if err := checkPublicURL(rawURL); err != nil {
			return FetchResult{}, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", c.fcfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := c.fetchHC.Do(req)
	if err != nil {
		return FetchResult{}, humanizeNetErr(err)
	}
	defer resp.Body.Close()

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	body, truncated, err := readLimited(resp.Body, c.fcfg.MaxBytes)
	if err != nil {
		return FetchResult{}, err
	}
	out := FetchResult{
		URL:         resp.Request.URL.String(), // 重定向后的最终地址
		StatusCode:  resp.StatusCode,
		ContentType: ct,
		Bytes:       int64(len(body)),
		Truncated:   truncated,
	}

	if resp.StatusCode >= 400 {
		// 错误页也把正文带回一部分：模型常需要从 404 页面里判断真实地址。
		out.Text = fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), clipRunes(string(body), 2000))
		return out, nil
	}

	text, err := DecodeBody(ct, body)
	if err != nil {
		return out, err
	}
	if text == "" {
		text = "(空响应)"
	}
	out.Text = text
	return out, nil
}

// checkPublicURL 在发起请求前判一次目标地址，给出比 DialContext 更清楚的原因。
//
// 这一步拦不住 DNS rebinding（域名解析发生在连接时），所以 DialContext 里还有
// 第二道同样的检查 —— 两者是配合关系，不是重复。
func checkPublicURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("拒绝抓取内网/回环地址 %s（如需允许，配置 web.fetch.allowPrivate）", host)
		}
		return nil
	}
	// 域名交给 DialContext 判（这里解析一次只是为了尽早给出提示）。
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return humanizeNetErr(err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("拒绝抓取 %s（解析到内网/回环地址 %s）", host, ip.IP)
		}
	}
	return nil
}

// DecodeBody 按 Content-Type 把响应体转成文本。
func DecodeBody(contentType string, body []byte) (string, error) {
	mediaType, charset := splitContentType(contentType)
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		s, err := decodeCharset(body, charset)
		if err != nil {
			return "", err
		}
		return ExtractText(s), nil
	case strings.HasPrefix(mediaType, "text/"),
		mediaType == "application/json",
		mediaType == "application/xml",
		mediaType == "application/ld+json",
		mediaType == "application/javascript",
		mediaType == "application/rss+xml",
		mediaType == "application/atom+xml",
		mediaType == "":
		return decodeCharset(body, charset)
	case mediaType == "application/pdf":
		return "", errors.New("PDF 暂不支持（需要文本型页面）；可以换一个 HTML 来源")
	case strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
		return "", fmt.Errorf("不支持 %s 内容", mediaType)
	}
	// 未知类型：如果是合法 UTF-8 就照原样给，否则报错。
	if s, err := decodeCharset(body, charset); err == nil && looksTexty(s) {
		return s, nil
	}
	return "", fmt.Errorf("不支持的内容类型 %s", mediaType)
}

func splitContentType(ct string) (mediaType, charset string) {
	parts := strings.Split(ct, ";")
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(strings.ToLower(p), "charset=") {
			charset = strings.Trim(strings.TrimSpace(p[len("charset="):]), `"'`)
		}
	}
	return mediaType, charset
}

// looksTexty 用「可打印字符占比」粗判二进制。
func looksTexty(s string) bool {
	if s == "" {
		return true
	}
	if strings.ContainsRune(s, 0) {
		return false
	}
	bad := 0
	total := 0
	for _, r := range s {
		total++
		if total > 4096 {
			break
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			bad++
		}
	}
	return bad*20 < total
}

// readLimited 读到 limit 字节，返回是否被截断。
func readLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, humanizeNetErr(err)
	}
	if int64(len(buf)) > limit {
		return buf[:limit], true, nil
	}
	return buf, false, nil
}

func humanizeNetErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("请求超时：%w", err)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("域名解析失败（%s）：%w", dnsErr.Name, err)
	}
	return err
}

// clipRunes 按 rune 截断，避免切坏多字节字符。
func clipRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
