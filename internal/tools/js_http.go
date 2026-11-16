package tools

// js 的网络桥（fetch）。
//
// 设计取向与 fs 桥一致：补的是「怎么把外部数据拿进来」，但边界必须写清楚，
// 因为这个口子一旦没约束，一个脚本就能把整个内网扫一遍。
//
//   - **同步**：`js` 工具里没有事件循环（见 docs/tools.md「仅支持同步代码」），
//     所以 fetch 直接返回结果对象，不是 Promise。没有 await，`r.json` 是属性
//     而不是方法。一旦哪天要 async，这个签名就得整体换掉 —— 不要偷偷塞一个
//     thenable 让脚本 `await` 一个 undefined。
//
//   - **有界**：单请求响应体、单次脚本的请求数与累计字节数都有上限；单请求超时
//     不超过脚本剩余时间。原因很实际：`Interrupt` 只在解释器执行 opcode 时
//     检查中断，打不断阻塞在 socket 上的 Go 调用 —— 超时必须由 http 层自己兜住，
//     否则一个慢站点就能把工具调用挂到远超 timeout 的地方。
//
//   - **受 Policy 管**：只读模式（`permissions=readonly`）只放行 GET/HEAD。
//     理由和 web_fetch 一致：只读模式本来就允许「读网」（web_search/web_fetch
//     在 readonly 下照常可用），但不允许借 js 去改远端状态。
//
//   - **不做 SSRF 过滤**：bash 本来就能 curl localhost / 内网，在 js 上单独加
//     域名白名单只会把模型赶回 shell，是"看着安全"的假边界。真正的门禁是
//     permissions —— readonly 拦写，改动型权限本来就要用户确认。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
)

const (
	// jsHTTPMaxRequests / jsHTTPMaxTotalBytes 是一次脚本的累计预算。
	// 单请求上限挡不住 `for (const id of ids) fetch(...)` —— 1000 个 id 就是
	// 1000 次真实请求，所以次数必须单独记。
	jsHTTPMaxRequests    = 16
	jsHTTPMaxTotalBytes  = 8 << 20
	jsHTTPDefaultMaxBody = 1 << 20
	jsHTTPMaxBodyCap     = 8 << 20
	// jsHTTPDefaultTimeout 是单请求默认超时；实际取 min(它, 脚本剩余时间)。
	jsHTTPDefaultTimeout = 15 * time.Second
	jsHTTPMaxTimeout     = 60 * time.Second
	// jsHTTPUserAgent 默认带上，部分站点会直接拒绝空 UA 的请求。
	jsHTTPUserAgent = "acp-agent-js (fetch)"
)

// jsHTTPClient 共用连接池；超时靠每个请求自己的 ctx，不用 Client.Timeout
// （后者会把复用中的连接一起取消）。
var jsHTTPClient = &http.Client{}

// jsHTTP 是一次脚本执行的网络上下文（预算 + 截止时间）。
type jsHTTP struct {
	policy Policy
	ctx    context.Context
	reqs   int
	bytes  int64
}

// installFetch 注入全局函数 fetch(url[, options])。
func installFetch(rt *moejs.Runtime, p Policy, h *jsHTTP) error {
	if h == nil {
		h = &jsHTTP{policy: p, ctx: context.Background()}
	}
	h.policy = p
	return rt.SetGlobal("fetch", h.fetch(rt))
}

// fetchOptions 是 fetch 的第二个参数解析结果。
type fetchOptions struct {
	method   string
	headers  map[string]string
	body     string
	hasBody  bool
	maxBytes int64
	timeout  time.Duration
}

func (h *jsHTTP) fetch(rt *moejs.Runtime) moejs.NativeFunc {
	return func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		rawURL := strings.TrimSpace(jsArg(args, 0))
		if rawURL == "" {
			return moejs.Undefined(), throwErr("fetch: 缺少 url")
		}
		lower := strings.ToLower(rawURL)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			return moejs.Undefined(), throwErr("fetch: 只支持 http/https URL（拿到 %q）", clip(rawURL, 80))
		}
		short := clip(rawURL, 80)
		opts, err := parseFetchOptions(rt, jsArgValue(args, 1))
		if err != nil {
			return moejs.Undefined(), err
		}

		write := opts.method != http.MethodGet && opts.method != http.MethodHead
		if h.policy.ReadOnly && write {
			return moejs.Undefined(), throwErr(
				"fetch: 只读模式（permissions=readonly）只允许 GET/HEAD，收到 %s。要改远端状态请用 bash", opts.method)
		}
		if !h.policy.AllowNetworkWrite && write {
			// 不是改动型工具，拦不住就不该放行：ask 模式下同样的事走 bash 会弹确认。
			return moejs.Undefined(), throwErr(
				"fetch: 当前权限模式不允许非 GET/HEAD 的请求（收到 %s）。要写远端请用 bash 发命令（会走确认），或在 permissions=auto 下运行", opts.method)
		}

		h.reqs++
		if h.reqs > jsHTTPMaxRequests {
			return moejs.Undefined(), throwErr(
				"fetch: 单次脚本最多 %d 次网络请求（已发 %d 次）。要批量抓请用一条 curl，或把任务拆成多次 js 调用",
				jsHTTPMaxRequests, jsHTTPMaxRequests)
		}
		if h.bytes >= jsHTTPMaxTotalBytes {
			return moejs.Undefined(), throwErr(
				"fetch: 本次脚本累计已读 %d 字节，超过上限 %d", h.bytes, jsHTTPMaxTotalBytes)
		}

		timeout := opts.timeout
		if dl, ok := h.ctx.Deadline(); ok {
			remain := time.Until(dl)
			if remain <= 0 {
				return moejs.Undefined(), throwErr("fetch %s: 脚本已超时/取消", short)
			}
			if remain < timeout {
				timeout = remain
			}
		}
		ctx, cancel := context.WithTimeout(h.ctx, timeout)
		defer cancel()

		var bodyReader io.Reader
		if opts.hasBody {
			bodyReader = strings.NewReader(opts.body)
		}
		req, err := http.NewRequestWithContext(ctx, opts.method, rawURL, bodyReader)
		if err != nil {
			return moejs.Undefined(), throwErr("fetch %s: %s", short, err.Error())
		}
		for k, v := range opts.headers {
			req.Header.Set(k, v)
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", jsHTTPUserAgent)
		}

		resp, err := jsHTTPClient.Do(req)
		if err != nil {
			return moejs.Undefined(), throwErr("fetch %s: %s", short, netErrText(err, timeout))
		}
		defer resp.Body.Close()

		limit := opts.maxBytes
		if limit <= 0 {
			limit = jsHTTPDefaultMaxBody
		}
		if limit > jsHTTPMaxBodyCap {
			limit = jsHTTPMaxBodyCap
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if rerr != nil && len(data) == 0 {
			return moejs.Undefined(), throwErr("fetch %s: 读响应体失败: %s", short, errText(rerr))
		}
		truncated := int64(len(data)) > limit
		if truncated {
			data = data[:limit]
		}
		h.bytes += int64(len(data))

		return jsFetchResult(rt, resp, string(data), truncated)
	}
}

// parseFetchOptions 把第二个参数（对象或 undefined）解析成结构化选项。
//
// 支持：method / headers / body / json（自动 JSON.stringify 并补 Content-Type）
// / timeout（毫秒）/ maxBytes。未知字段忽略 —— 手滑写错一个键不该让整段脚本失败。
func parseFetchOptions(rt *moejs.Runtime, v moejs.Value) (fetchOptions, error) {
	out := fetchOptions{method: http.MethodGet, timeout: jsHTTPDefaultTimeout}
	if v.IsNullish() {
		return out, nil
	}
	if !v.IsObject() {
		return out, throwErr("fetch: 第二个参数必须是对象，如 {method:'POST', json:{...}}")
	}

	if m := strings.TrimSpace(jsPropString(rt, v, "method")); m != "" {
		out.method = strings.ToUpper(m)
	}
	switch out.method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return out, throwErr("fetch: 不支持的 method %q", out.method)
	}

	if hv := jsProp(rt, v, "headers"); !hv.IsNullish() {
		if hm, ok := jsToGo(rt, hv).(map[string]any); ok {
			out.headers = make(map[string]string, len(hm))
			for k, val := range hm {
				out.headers[k] = strings.TrimSpace(fmt.Sprint(val))
			}
		} else {
			return out, throwErr("fetch: headers 必须是对象，如 {headers:{'X-Token':'abc'}}")
		}
	}

	if jv := jsProp(rt, v, "json"); !jv.IsNullish() {
		body, err := jsJSONStringify(rt, jv)
		if err != nil {
			return out, err
		}
		out.body = body
		out.hasBody = true
		if out.headers == nil {
			out.headers = map[string]string{}
		}
		if !hasHeaderFold(out.headers, "Content-Type") {
			out.headers["Content-Type"] = "application/json"
		}
	} else if bv := jsProp(rt, v, "body"); !bv.IsNullish() {
		out.body = bv.String()
		out.hasBody = true
	}

	if ms := jsPropNumber(rt, v, "timeout"); ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		if d > jsHTTPMaxTimeout {
			d = jsHTTPMaxTimeout
		}
		out.timeout = d
	}
	if n := jsPropNumber(rt, v, "maxBytes"); n > 0 {
		out.maxBytes = n
	}
	return out, nil
}

// jsFetchResult 把响应包装成普通 JS 对象。
//
// 刻意不是 Response 类的克隆：这里没有 body 流、没有 thenable，模型拿到的是
// 一份已经读完的快照 —— `{status, ok, url, contentType, headers, size,
// truncated, text, json}`。`json` 只在响应体是合法 JSON 时才存在。
//
// 非 2xx 不抛错（HTTP 语义 + 让脚本自己判 `ok`/`status`），抛错的只有传输层
// 失败、URL 非法、超预算、超时这几类。
func jsFetchResult(rt *moejs.Runtime, resp *http.Response, bodyText string, truncated bool) (moejs.Value, error) {
	headers := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		// 全小写：HTTP header 不区分大小写，统一成小写省得脚本猜 `X-Trace` 还是 `x-trace`。
		headers[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	out := map[string]any{
		"status":      resp.StatusCode,
		"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
		"url":         resp.Request.URL.String(),
		"contentType": resp.Header.Get("Content-Type"),
		"headers":     headers,
		"size":        len(bodyText),
		"truncated":   truncated,
		"text":        bodyText,
	}
	if v, err := rt.ParseJSONString(bodyText); err == nil {
		out["json"] = v
	}
	return rt.FromGo(out)
}

// netErrText 给网络错误补一句人话。
//
// 超时是最常见的一类，而 Go 的原文是 "context deadline exceeded" —— 模型看到
// 它不一定联想到「这个站点在这段时间内没响应」。原文保留在后面，方便排查。
func netErrText(err error, timeout time.Duration) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout") {
		return fmt.Sprintf("在 %s 内没有完成（%s）。可调大 timeout，或确认该地址是否可访问", timeout, msg)
	}
	return msg
}

// jsJSONStringify 用引擎自带的 JSON.stringify 序列化，保证与脚本语义一致。
func jsJSONStringify(rt *moejs.Runtime, v moejs.Value) (string, error) {
	res, err := rt.AppendJSON(nil, v)
	if err != nil {
		return "", throwErr("fetch: JSON.stringify 失败: %v", err)
	}
	if len(res) == 0 || string(res) == "undefined" {
		return "null", nil
	}
	return string(res), nil
}

// jsProp 读对象属性（不是对象或读不到时返回 undefined）。
func jsProp(rt *moejs.Runtime, obj moejs.Value, key string) moejs.Value {
	if !obj.IsObject() {
		return moejs.Undefined()
	}
	v, err := rt.Get(obj, key)
	if err != nil {
		return moejs.Undefined()
	}
	return v
}

// jsPropString 读对象里的字符串字段（缺失返回空串）。
func jsPropString(rt *moejs.Runtime, obj moejs.Value, key string) string {
	v := jsProp(rt, obj, key)
	if v.IsNullish() {
		return ""
	}
	return v.String()
}

// jsPropNumber 读对象里的数字字段（缺失/非数字返回 0）。
//
// 字符串也收（`timeout: "5000"` 是模型常见的写法）：旧实现用 ToInteger()
// 会隐式转换，换成引擎之后得自己补这一步，否则会静默回落成默认值。
func jsPropNumber(rt *moejs.Runtime, obj moejs.Value, key string) int64 {
	v := jsProp(rt, obj, key)
	if v.IsNumber() {
		return int64(v.AsNumber())
	}
	if v.IsString() {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64); err == nil {
			return int64(n)
		}
	}
	return 0
}

// jsToGo 把 JS 值转成 Go 值（失败返回 nil）。
func jsToGo(rt *moejs.Runtime, v moejs.Value) any {
	x, err := rt.ToGo(v)
	if err != nil {
		return nil
	}
	return x
}

// jsArgValue 取第 i 个参数（越界返回 undefined）。
func jsArgValue(args []moejs.Value, i int) moejs.Value {
	if i >= len(args) {
		return moejs.Undefined()
	}
	return args[i]
}

// hasHeaderFold 大小写不敏感地判断 header 是否已存在。
func hasHeaderFold(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
