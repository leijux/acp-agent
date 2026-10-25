package provider

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// sseEvent 是 Server-Sent Events 流里的一个事件。
type sseEvent struct {
	// Event 是 "event:" 字段（OpenAI 兼容接口通常不带，Anthropic 用 message_start 等）。
	Event string
	// Data 是 "data:" 字段，多行会以 "\n" 拼接。
	Data string
	// ID 是 "id:" 字段。
	ID string
}

// sseReader 解析 text/event-stream。
//
// 只实现 spec 里服务端真正会发的那部分：`data:` 多行拼接、空行分隔、
// 忽略注释行（以 ":" 开头）与未知字段。HTTP chunk 边界、CRLF 换行都由
// bufio.Scanner 吞掉。
type sseReader struct {
	sc *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	// 单行可能很长（一条 assistant 消息的完整 JSON），默认 64KB 不够。
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &sseReader{sc: sc}
}

// Next 返回下一个事件。流结束时返回 io.EOF。
func (s *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data []string
	for {
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return ev, err
			}
			// 流在没有终止空行的情况下结束：把残存数据交出去。
			if len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return ev, io.EOF
		}

		line := strings.TrimSuffix(s.sc.Text(), "\r")
		if line == "" {
			// 空行 = 事件结束。
			if len(data) == 0 && ev.Event == "" && ev.ID == "" {
				continue // 心跳里连续空行，跳过
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释 / keep-alive
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}

		switch field {
		case "data":
			data = append(data, value)
		case "event":
			ev.Event = value
		case "id":
			ev.ID = value
		}
	}
}

// ErrSSEUnexpectedEOF 表示流在协议要求的事件之前断开。
var ErrSSEUnexpectedEOF = errors.New("provider: SSE 流意外结束")
