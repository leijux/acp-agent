package tools

// js 的哈希桥（读 + 校验这对需求是连着的：读二进制最常见的目的就是算校验值）。
//
// goja 没有 crypto/SubtleCrypto，也没有 Buffer；脚本要算 sha256 只能调宿主。
// 这里接的是 Go 标准库，参数既接受字符串（按 UTF-8）也接受 ArrayBuffer
// （`fs.readFileBytes` 的返回值）或 Uint8Array。

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/dop251/goja"
)

// installCrypto 注入全局 crypto 对象。
func installCrypto(vm *goja.Runtime) error {
	obj := vm.NewObject()
	if err := obj.Set("sha256", func(call goja.FunctionCall) goja.Value {
		data, ok := jsBytes(call, 0)
		if !ok {
			throwJS(vm, "crypto.sha256: 参数必须是字符串、ArrayBuffer 或 Uint8Array")
		}
		sum := sha256.Sum256(data)
		return vm.ToValue(hex.EncodeToString(sum[:]))
	}); err != nil {
		return err
	}
	return vm.Set("crypto", obj)
}

// jsBytes 把第 i 个参数当成字节序列取出来。
func jsBytes(call goja.FunctionCall, i int) ([]byte, bool) {
	if i >= len(call.Arguments) {
		return nil, false
	}
	v := call.Arguments[i]
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, false
	}
	switch x := v.Export().(type) {
	case string:
		return []byte(x), true
	case goja.ArrayBuffer:
		return x.Bytes(), true
	case []byte: // 也覆盖 []uint8（同一个类型）；Uint8Array 的 Export 会落到这里
		return x, true
	}
	return nil, false
}
