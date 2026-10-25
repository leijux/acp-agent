package web

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// metaCharsetRe 从 HTML 头部抓 <meta charset="gbk"> 或
// <meta http-equiv="Content-Type" content="text/html; charset=gb2312">。
var metaCharsetRe = regexp.MustCompile(`(?i)<meta[^>]*charset\s*=\s*["']?\s*([a-z0-9_\-]+)`)

// decodeCharset 把 body 按 charset 解码成 UTF-8 字符串。
//
// 决策顺序：
//  1. charset 已知且不是 utf-8 → 按它转码；
//  2. charset 未知 → 先试 UTF-8；
//  3. UTF-8 非法 → 从 HTML 头部的 <meta charset> 里猜；
//  4. 还是不知道 → 按 GB18030 解（中文站点最常见的非 UTF-8 编码）。
//
// 这样不解码失败就直接报错：宁可给出带少量乱码的正文，也比「抓取失败」有用。
func decodeCharset(body []byte, charset string) (string, error) {
	// BOM 比 charset 声明更可信：不少站点声称 utf-8，实际发 UTF-16LE。
	if len(body) >= 2 {
		switch {
		case body[0] == 0xFF && body[1] == 0xFE:
			return decodeWith(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), body[2:])
		case body[0] == 0xFE && body[1] == 0xFF:
			return decodeWith(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), body[2:])
		}
	}
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	charset = strings.ToLower(strings.TrimSpace(charset))
	if charset == "utf-8" || charset == "utf8" || charset == "us-ascii" || charset == "ascii" {
		return toValidUTF8(body), nil
	}
	if charset != "" {
		if enc := lookupEncoding(charset); enc != nil {
			return decodeWith(enc, body)
		}
	}
	if utf8.Valid(body) {
		return string(body), nil
	}
	if m := metaCharsetRe.FindSubmatch(headBytes(body, 4096)); m != nil {
		if enc := lookupEncoding(strings.ToLower(string(m[1]))); enc != nil {
			if s, err := decodeWith(enc, body); err == nil {
				return s, nil
			}
		}
	}
	if enc := lookupEncoding("gb18030"); enc != nil {
		if s, err := decodeWith(enc, body); err == nil {
			return s, nil
		}
	}
	return toValidUTF8(body), nil
}

func decodeWith(enc encoding.Encoding, body []byte) (string, error) {
	out, _, err := transform.Bytes(enc.NewDecoder(), body)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func lookupEncoding(name string) encoding.Encoding {
	enc, err := htmlindex.Get(name)
	if err != nil {
		return nil
	}
	return enc
}

// toValidUTF8 把非法字节替换成 U+FFFD，保证结果是合法字符串。
func toValidUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune('\uFFFD')
			b = b[1:]
			continue
		}
		sb.WriteRune(r)
		b = b[size:]
	}
	return sb.String()
}

func headBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// stripBOM 去掉 UTF-8 BOM。
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
