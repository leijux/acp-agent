package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestBundleDigest 确认仓库里的 kernel.dist.cjs 就是 script/build-cjs.sh 打出来的那份。
//
// 光比对 BundleVersion 是自证（Version() 就是返回那个常量），挡不住最常见的错：
// 换了 bundle 却忘了重跑脚本（版本号/指纹对不上）。
func TestBundleDigest(t *testing.T) {
	if len(defaultBundle) == 0 {
		t.Fatal("没有嵌入 bundle")
	}
	if BundleDigest == "" {
		t.Fatal("BundleDigest 为空：version_gen.go 没由 build-cjs.sh 生成？")
	}
	sum := sha256.Sum256(defaultBundle)
	if got := hex.EncodeToString(sum[:]); got != BundleDigest {
		t.Fatalf("bundle sha256 = %s, BundleDigest = %s\n"+
			"说明 kernel.dist.cjs 被换过但没重跑 script/build-cjs.sh（或反过来）", got, BundleDigest)
	}
	t.Logf("bundle: acp-kernel %s, esbuild %s, sha256 %s", BundleVersion, BundleEsbuild, BundleDigest[:16]+"…")
}
