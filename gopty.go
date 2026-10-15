// Package gopty is a small, platform-neutral pty (pseudo terminal) backend: it
// starts a child process attached to a pty master and hands the caller a raw
// bidirectional byte stream, plus resize and exit-code wait.
//
// Backends (see pty_unix.go / pty_windows.go):
//
//   - unix: github.com/creack/pty, whose master file is the stream.
//   - windows: a vendored copy of UserExistsError/conpty (./conpty, MIT), Win10 1809+.
//     ConPTY takes ONE command-line string, so Start joins Command+Args into a safely
//     quoted line (windows.EscapeArg); a caller-supplied Env is de-duplicated with the
//     os/exec rule ("last occurrence wins") because CreateProcess keeps the first.
//
// Both backends satisfy the same Pty contract, so a program that runs pty children
// compiles and behaves the same on a unix worker and on Windows.
//
// A ConPTY child must be created through the ConPTY API rather than os/exec
// (golang/go#62708), and some Windows process contexts refuse it: suga's console harness
// (scripts/wincons) reported CreateProcess failing with STATUS_DLL_INIT_FAILED
// (0xC0000142) inside a gofer job, while this repo's own probe in a gofer job starts a
// ConPTY child fine (cmd.exe, exit 0) — so the outcome is environment-dependent. Treat it
// as possible: check IsHostBlocked / IsBlockedExitCode instead of assuming either way.
// When it is blocked, drive a real console for interactive work instead.
//
// Interactive (test) children are best driven through the sibling package
// github.com/gookit/gopty/ptytest, which adds expect/send helpers on this contract.
package gopty

import (
	"context"
	"io"
)

// Spec is a platform-neutral request to start a child under a pty.
type Spec struct {
	Command string   // executable to run
	Args    []string // args (not including argv[0])
	Env     []string // full process env (KEY=VALUE); nil = inherit os.Environ
	Dir     string   // working directory; "" = inherit
	Cols    int      // initial terminal width (columns); 0 = platform default
	Rows    int      // initial terminal height (rows); 0 = platform default
}

// Pty is a running child attached to a pty master. Read/Write carry the raw byte
// stream to/from the child (stdout+stderr multiplexed on Read, stdin on Write);
// Resize sets the terminal window size; Wait blocks for the child to exit and
// returns its exit code. Close releases the master fd (and, on unix, kills the
// child) — teardown ORDER is owned by the caller's state machine, this type only
// provides the primitives.
type Pty interface {
	io.ReadWriteCloser
	// Resize sets the pty window size in character cells.
	Resize(cols, rows int) error
	// Wait blocks until the child exits (or ctx is cancelled) and returns the
	// process exit code. A ctx cancellation returns a non-nil error without
	// reaping; the caller kills via Close and calls Wait once.
	Wait(ctx context.Context) (exitCode int, err error)
}

// Start launches spec.Command under a pty and returns the live Pty. Start and
// IsAvailable are provided by the platform files so the common contract above
// stays free of any platform symbol.
//
// (declared per-platform: pty_unix.go / pty_windows.go)

// IsAvailable reports whether a pty backend is usable on this build/host. On
// unix it is always true; on windows it probes the ConPTY API (Win10 1809+).
//
// (declared per-platform: pty_unix.go / pty_windows.go)

// IsHostBlocked reports whether err means "this process context refuses to host a
// pty child", i.e. the pty API itself is available but the OS blocked the start —
// on Windows, a ConPTY child launched inside a job object fails with
// STATUS_DLL_INIT_FAILED (0xC0000142) because the job does not allow breakaway.
// Callers can use it to degrade gracefully (skip a test, report a clearer error)
// instead of surfacing a bare error code. Always false on unix.
//
// (declared per-platform: pty_unix.go / pty_windows.go)
