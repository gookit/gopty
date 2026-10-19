# gopty

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/gookit/gopty?style=flat-square)
[![GitHub tag (latest SemVer)](https://img.shields.io/github/tag/gookit/gopty)](https://github.com/gookit/gopty)
[![Unit-Tests](https://github.com/gookit/gopty/actions/workflows/go.yml/badge.svg)](https://github.com/gookit/gopty/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/gookit/gopty.svg)](https://pkg.go.dev/github.com/gookit/gopty)

> [English](./README.md) | [简体中文](./README.zh-CN.md)

A small, platform-neutral pty (pseudo terminal) backend for Go, plus an interactive test fixture built
on top of it. Start a child process on a pty master and read/write its byte stream, resize the terminal
and wait for its exit code — the same code on Linux, macOS and Windows (ConPTY).

## Packages

| Package | Use it for |
| --- | --- |
| `github.com/gookit/gopty` | Product code: `Spec` + `Start` → `Pty` (`Read` / `Write` / `Resize` / `Wait` / `Close`), `IsAvailable()`, `IsHostBlocked(err)`, `IsBlockedExitCode(code)` |
| `github.com/gookit/gopty/ptytest` | Test code: drive a program inside a pty from `go test` — `Start` / `StartSpec`, `Mark`, `Expect` / `Contains` / `Count`, `Text` (ANSI stripped) / `RawText`, `Send` / `SendLine` / `SendCtrlC`, `WaitExit`, and an automatic `Close` cleanup |

## Install

```bash
go get github.com/gookit/gopty
go get github.com/gookit/gopty/ptytest   # the test fixture
```

Requires Go 1.23 or newer.

## Backends

| GOOS | Backend | Dependency |
| --- | --- | --- |
| `linux`, `macOS`, `*BSD` | [creack/pty](https://github.com/creack/pty) (the master file is the stream) | `github.com/creack/pty` |
| `windows` (Win10 1809+) | a vendored [ConPTY](https://github.com/UserExistsError/conpty) copy in `./conpty`, MIT | `golang.org/x/sys` |

Both backends satisfy the same `Pty` contract, so a program that runs pty children compiles and behaves
the same on a unix worker and on Windows.

> The Windows side deliberately keeps `golang.org/x/sys` as its only dependency: it does not pull in
`hcsshim` and its indirect chain the way `aymanbagabas/go-pty`
does.

## Usage

### Run a child on a pty

```go
import "github.com/gookit/gopty"

p, err := gopty.Start(gopty.Spec{Command: "bash", Args: []string{"-i"}, Cols: 100, Rows: 30})
if err != nil {
	return err
}
defer p.Close()

_, _ = p.Write([]byte("echo hi\n"))
out, _ := io.ReadAll(p)          // stdout + stderr, multiplexed
code, err := p.Wait(ctx)         // blocks until the child exits (or ctx is cancelled)
```

`Spec` fields:

| Field | Meaning |
| --- | --- |
| `Command`, `Args` | executable + args (argv[0] not included). On Windows they are joined into one safely-quoted command line (`windows.EscapeArg`), since ConPTY takes a command line, not an argv |
| `Env` | full process env; `nil` inherits the parent. On Windows duplicates are de-duplicated "last wins" (the `os/exec` rule) because `CreateProcess` keeps the first |
| `Dir` | working directory; `""` inherits |
| `Cols`, `Rows` | initial terminal size; `0` uses the platform default |

`Pty` contract: `Read` carries stdout+stderr, `Write` is stdin, `Resize` sets the window size in cells,
`Wait(ctx)` returns the exit code (a real process exit is `(code, nil)`, only a genuine reap failure
returns an error), `Close` releases the master and — on unix — kills the child. Teardown *order* belongs
to the caller's state machine; this package only provides the primitives.

### Drive an interactive program from a test

```go
import "github.com/gookit/gopty/ptytest"

func TestAskName(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)
	mark := s.Mark()
	if !s.Expect(`name\?`, mark) {
		t.Fatalf("no prompt, output:\n%s", s.Text())
	}
	s.SendLine("tom")
	if !s.Expect(`got=tom`, mark) {
		t.Fatalf("no answer, output:\n%s", s.Text())
	}
}
```

It is the Go counterpart of a "pty smoke script" (what a Python pty driver or a Windows console harness
used to provide), so the same test code runs on every platform.

Rules of thumb:

- assert only on output produced after a `Mark` — earlier turns would satisfy the check by accident;
- wait for the prompt (`Expect`) before typing: a program that clears pending input before asking drops
  too-early keystrokes;
- `Enter` is `"\r"`, Ctrl-C is `0x03` (use `SendCtrlC`), Ctrl-D is `0x04`;
- `Text()` strips ANSI escapes, `RawText()` keeps them (colour-only output is invisible to `Text`);
- `Start` skips the test with a reason when no pty backend is available or the host refuses a pty child,
  and registers its own `Close` cleanup.

## Platform notes

### Windows (ConPTY)

- ConPTY needs Win10 1809+; `IsAvailable()` probes it and `ptytest.Start` skips when it is missing.
- Some Windows process contexts refuse a ConPTY child: inside a job object that does not allow
  breakaway, `CreateProcess` fails with `ERROR_ACCESS_DENIED`, or the child dies at load with
  `STATUS_DLL_INIT_FAILED` (`0xC0000142`). Use `IsHostBlocked(err)` / `IsBlockedExitCode(code)` to
  detect it and degrade; `ptytest` skips with the reason instead of reporting a bare error code. The
  outcome is environment-dependent — measured both ways inside the same agent job (`cmd.exe` started
  fine, another harness was blocked) — so do not assume either result.
- When the host blocks ConPTY and you still need to test interactive prompts, drive a real console:
  `AllocConsole`, child stdin = `CONIN$`, then send keys with `WriteConsoleInput`.
- After the child has exited on its own, `Close()` first drains what conhost is still forwarding, so the
  last lines a short-lived child printed are not discarded: ConPTY's output pipe does not report EOF while
  the pseudo console is open (closing it is what ends the stream), so the drain waits for the pipe to go
  **quiet** (250ms without a read) instead of for EOF, and gives up at 2s (`drainGrace`, the same bound the
  unix backend uses). A live child (cancel) is torn down at once.
- `Close()` is idempotent. The vendored `conpty.Close()` closes four pipe handles and the pseudo console
  unconditionally, and a second close would release handle values Windows may have reused since
  (observed to take the test binary down silently); the adapter guards it with `sync.Once`.
- A synthetic Ctrl-C written to the input (`0x03`) is not a real `CTRL_C_EVENT`; the child's console
  mode decides how it is read.

### unix

- The parent keeps its own slave fd until the child has been reaped, so the reaper's close is the last
  one. On darwin the BSD tty layer flushes master-unread output on the last close of the slave, which
  would lose everything a short-lived child printed before the reader caught up.
- After the child has exited, `Close()` first waits (bounded, 2s) for the reader to reach EOF, so the
  output written right before exit is not discarded; a live child (cancel) is torn down at once.

## Development & testing

```bash
go test ./...                  # host platform
go test -race ./...            # unix
GOOS=windows go build ./...    # cross-compile check (also GOOS=darwin, GOOS=linux)
go vet ./...
```

CI ([.github/workflows/go.yml](./.github/workflows/go.yml)) runs the tests on ubuntu + windows for Go
1.23 / 1.24 / 1.25 / stable, plus macOS on 1.23 and stable, and the race detector on the ubuntu/stable
leg. On a runner whose session cannot host a ConPTY child, the Windows pty cases skip and say so in the
log.

## Consumers

- inhere/gofer — pty runner for interactive agent sessions (`internal/runner/pty`);
- gookit/gcli — interactive end-to-end tests of prompts and shared options (`e2e_pty_test.go`).

## License

MIT, see [LICENSE](./LICENSE). The vendored `./conpty` package keeps its own copy in
[conpty/LICENSE](./conpty/LICENSE).
