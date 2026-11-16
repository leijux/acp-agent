package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 内存上限：分配型脚本必须被拦住并报一条可读的错误，而不是把整个进程吃到
// 内核 OOM kill（那种死法连 error 都拿不到，工具层的 recover 也救不回来）。
func TestJSTool_MemoryLimit(t *testing.T) {
	jt := NewJSToolWithPolicy(testPolicy(t))
	res, err := jt.Execute(context.Background(), Args{
		"script":  `const a = []; for (;;) a.push(new Array(1 << 16).fill(0));`,
		"timeout": 30000,
	})
	if err != nil {
		t.Fatalf("Execute 返回 Go error: %v", err)
	}
	got := res.Text()
	if !res.IsError {
		t.Fatalf("内存超限应报错，实际: %q", got)
	}
	if !strings.Contains(got, "内存超限") {
		t.Fatalf("错误文案应说明内存超限: %q", got)
	}
	// 别退化成脚本超时或「内部错误」：前者会误导模型去查死循环，后者是 panic 兜底。
	if strings.Contains(got, "超时") || strings.Contains(got, "内部错误") {
		t.Fatalf("被当成了超时/内部错误: %q", got)
	}
}

// 大量**临时**分配但留得很少的脚本必须照常跑完：moejs 在越过阈值时先测量
// 「真正留住的量」再决定是否打断，否则这个上限会把逐行扫文件这类正常用法打死。
func TestJSTool_MemoryLimitAllowsGarbage(t *testing.T) {
	jt := NewJSToolWithPolicy(testPolicy(t))
	// 2000 × 64KiB = 128MB 累计分配，同时只留一个字符串。
	res, err := jt.Execute(context.Background(), Args{
		"script": `let n = 0;
			for (let i = 0; i < 2000; i++) { const s = "x".repeat(1 << 16); n += s.length; }
			n`,
		"timeout": 30000,
	})
	if err != nil {
		t.Fatalf("Execute 返回 Go error: %v", err)
	}
	got := res.Text()
	if res.IsError {
		t.Fatalf("大量垃圾但低留存的脚本不该被拦: %q", got)
	}
	if !strings.Contains(got, fmt.Sprint(2000*(1<<16))) {
		t.Fatalf("结果不对: %q", got)
	}
}

// 上限之内的正经大计算必须正常返回（别把预算设得太紧）。
func TestJSTool_MemoryLimitAllowsBigResult(t *testing.T) {
	jt := NewJSToolWithPolicy(testPolicy(t))
	res, err := jt.Execute(context.Background(), Args{
		"script":  `const a = new Array(1 << 19).fill(7); a.reduce((x, y) => x + y, 0)`,
		"timeout": 30000,
	})
	if err != nil {
		t.Fatalf("Execute 返回 Go error: %v", err)
	}
	if got := res.Text(); res.IsError || !strings.Contains(got, fmt.Sprint(7*(1<<19))) {
		t.Fatalf("524288 个元素的数组应能算出来: %q", got)
	}
}

// 内建 goja 命令走同一套上限（同一个引擎、同一个进程风险）。
func TestBuiltinGojaMemoryLimit(t *testing.T) {
	out := runGojaBuiltin(t, `goja -t 30s -e 'const a=[];for(;;)a.push(new Array(1<<16).fill(0))' || echo "EXIT=$?"`)
	if !strings.Contains(out, "内存超限") {
		t.Fatalf("内建 goja 未触发内存上限: %q", out)
	}
	if !strings.Contains(out, "EXIT=1") {
		t.Fatalf("内存超限应退出码 1: %q", out)
	}
}
