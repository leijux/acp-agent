package tools

// js 的哈希桥（读 + 校验这对需求是连着的：读二进制最常见的目的就是算校验值）。
//
// 引擎里没有 crypto/SubtleCrypto，也没有 Buffer；脚本要算 sha256 只能调宿主。
// 这里接的是 Go 标准库，参数既接受字符串（按 UTF-8）也接受 ArrayBuffer
// （`fs.readFileBytes` 的返回值）或 Uint8Array —— moejs 的 ToGo 把这两种都
// 还原成 []byte。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/MoeForge/moejs"
)

// installCrypto 注入全局 crypto 对象。
func installCrypto(rt *moejs.Runtime) error {
	return rt.SetGlobal("crypto", map[string]any{
		"sha256": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			data, ok := jsBytes(rt, args, 0)
			if !ok {
				return moejs.Undefined(), errors.New("crypto.sha256: 参数必须是字符串、ArrayBuffer 或 Uint8Array")
			}
			sum := sha256.Sum256(data)
			return moejs.String(hex.EncodeToString(sum[:])), nil
		}),
	})
}

// jsBytes 把第 i 个参数当成字节序列取出来。
func jsBytes(rt *moejs.Runtime, args []moejs.Value, i int) ([]byte, bool) {
	if i >= len(args) || args[i].IsNullish() {
		return nil, false
	}
	x, err := rt.ToGo(args[i])
	if err != nil {
		return nil, false
	}
	switch b := x.(type) {
	case string:
		return []byte(b), true
	case []byte:
		return b, true
	}
	return nil, false
}
