package agent

import "errors"

// OverflowDetector 由 provider 适配层的错误类型实现，用来识别「请求超过上下文窗口」。
//
// agent 不能直接依赖 provider 包（provider 依赖 agent），所以这里只约定一个
// 方法：错误类型实现 ContextOverflow() bool 即视为可识别。
type OverflowDetector interface {
	ContextOverflow() bool
}

// IsContextOverflow 报告 err 是否是「上下文超限」。
//
// 用途：Run 循环里据此触发「压缩 + 重试一次」（见 RunMessages），而不是把
// 上游的 400 原样落进历史 —— 那条错误消息会把上下文再撑大一点，让下一次
// 请求必然复现同一个失败。
func IsContextOverflow(err error) bool {
	var d OverflowDetector
	return errors.As(err, &d) && d.ContextOverflow()
}
