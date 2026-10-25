package provider

import "time"

// SetNowFuncForTest 替换内部时钟并返回还原函数。
// 仅供 provider_test 包使用（本文件只在测试构建时参与编译）。
func SetNowFuncForTest(f func() time.Time) func() {
	orig := nowFunc
	nowFunc = f
	return func() { nowFunc = orig }
}

// SetDeepSeekHolidaysForTest 替换节假日表并返回还原函数。
func SetDeepSeekHolidaysForTest(days map[string]bool) func() {
	orig := deepSeekHolidays
	deepSeekHolidays = days
	return func() { deepSeekHolidays = orig }
}
