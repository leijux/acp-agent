# 上游 issue 草稿：mvdan/sh `Runner.Run` panics on Windows exit codes ≥ 256

> 用途：可直接贴到 https://github.com/mvdan/sh/issues 。仓库里留这份是为了以后
> 复查「我们已经上报过、也自己兜住了」。复现代码见本文末尾，本机 `tmp/repro-exitstatus/`
> 里也有一份（`tmp/` 不进仓库）。
>
> 现状：v3.14.1 复现；本地已在 `internal/tools/shell_builtin.go` 的 `normalizeExecErr`
> 兜住（见 docs/tools.md「退出码只有低 8 位」）。

---

**Title:** `interp`: `Runner.Run` panics when a process exits with a status that truncates to 0 (Windows 32-bit exit codes)

## Environment

- mvdan.cc/sh/v3 v3.14.1（同时在 v3.13.1 复现；v3.12.0 不 panic，见下）
- windows/amd64, go1.27.3

## Summary

On Windows, `DefaultExecHandler` converts a child's exit code with
`ExitStatus(err.ExitCode())`, but `ExitStatus` is a `uint8` and Windows exit codes are
32-bit. Any non-zero code that is a multiple of 256 (`256`, `512`, `0xC0000000`, …)
truncates to `ExitStatus(0)`, which is a **non-nil error whose code is zero**. That
violates the invariant asserted at the end of `Runner.Run` and the library panics:

```
panic: ended up with a non-nil exitStatus.err but a zero exitStatus.code
```

The panic is unrecoverable in practice for a library user: it happens on the goroutine
that called `Runner.Run`, and for programs that drive the interpreter from a worker
goroutine (in-process shells, agents, REPLs) it takes down the whole process.

## Minimal repro (Windows)

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func main() {
	file, _ := syntax.NewParser().Parse(strings.NewReader(`cmd /c exit 256`), "")
	r, _ := interp.New(
		interp.StdIO(nil, os.Stdout, os.Stderr),
		interp.Env(expand.ListEnviron(os.Environ()...)),
	)
	fmt.Println("run:", r.Run(context.Background(), file))
}
```

```
panic: ended up with a non-nil exitStatus.err but a zero exitStatus.code
goroutine 1 [running]:
mvdan.cc/sh/v3/interp.(*Runner).Run(...)
	mvdan.cc/sh/v3@v3.14.1/interp/api.go:1030 +0x4a5
main.main()
	./main.go:30 +0x19a
```

Any of these trigger it: `cmd /c exit 256`, `cmd /c exit 512`, `cmd /c exit 768`.
(`cmd /c exit 300` is fine on the panic front but silently reports `44`.)

## Root cause

1. `interp/handler.go:199` — `DefaultExecHandler`:

   ```go
   case *exec.ExitError:
       if status, ok := err.Sys().(waitStatus); ok && status.Signaled() { … }
       return ExitStatus(err.ExitCode())   // int -> uint8: 256 becomes 0
   ```

   `waitStatus` is a no-op on Windows/Plan9 (as the comment above it says), so every
   Windows exit goes through this truncating conversion.

2. `interp/api.go:963` — `type ExitStatus uint8`; `ExitStatus(0)` is a non-nil `error`.

3. `interp/api.go:244-246` — `exitStatus.fromHandlerError` accepts it as-is:

   ```go
   } else if es, ok := errors.AsType[ExitStatus](err); ok {
       e.err = err          // non-nil
       e.code = uint8(es)   // 0
   }
   ```

   which builds the state the struct comment on api.go:190 forbids
   ("When code is zero, err must be nil").

4. `interp/api.go:1026-1030` — `Runner.Run` detects the broken invariant and panics.

Note the panic assertion itself is recent:

| 版本 | `cmd /c exit 256` 的结果 |
|---|---|
| v3.12.0 | **`run: exit status 0`** —— 静默当成成功（更坏：失败被报成成功） |
| v3.13.1 | panic（api.go:937） |
| v3.14.1 | panic（api.go:1030） |

三个版本都在跑同一个 20 行复现程序。

## Second trigger (not Windows-specific)

Any `ExecHandlerFunc` that returns `interp.NewExitStatus(0)` instead of `nil` hits the
same panic — an easy mistake, since `NewExitStatus` is *documented* as the way to set a
status and its `uint8` parameter happily accepts `0`:

```go
r, _ := interp.New(interp.ExecHandler(func(ctx context.Context, args []string) error {
	return interp.NewExitStatus(0) // "the command succeeded"
}))
r.Run(context.Background(), file) // panic: same message
```

Handler contract says "Returning a nil error means a zero exit status", so this is a
user error — but it should not panic a whole process from inside a library.

## Suggested fix

Two independent hardening points; either alone stops the panic, both are cheap.

**A. Don't let a truncation turn failure into zero** (`handler.go`, `DefaultExecHandler`):

```go
case *exec.ExitError:
	if status, ok := err.Sys().(waitStatus); ok && status.Signaled() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ExitStatus(128 + status.Signal())
	}
	// Windows exit codes are 32-bit, ExitStatus is a POSIX 8-bit status.
	// A non-zero code must never truncate to 0 (= success).
	if code := uint8(err.ExitCode()); code != 0 {
		return ExitStatus(code)
	}
	return ExitStatus(1)
```

**B. Treat `ExitStatus(0)` as success** (`api.go`, `fromHandlerError`):

```go
} else if es, ok := errors.AsType[ExitStatus](err); ok {
	if code := uint8(es); code != 0 {
		e.err = err
		e.code = code
	}
	// ExitStatus(0) == success; leave the zero value alone instead of
	// building a state where err != nil but code == 0.
```

(Optionally also make `NewExitStatus(0)` return `nil`, so the footgun disappears at the
source.)

## Why it matters

Windows is a first-class target for this library (`waitStatus` no-op, `LookPathDir`
PATHEXT handling, `runScriptENOEXEC`). Every Windows program that shells out in-process
and can run a user-supplied command — editors, agents, CI runners — is one
`cmd /c exit 256` away from a crash, and the only workaround on the caller side is to
replace/wrap `DefaultExecHandler` and post-process its error, which loses the original
exit code either way.

Happy to test a patch on Windows if that helps.
