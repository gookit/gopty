//go:build windows

package gopty

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gookit/gopty/conpty"
	"golang.org/x/sys/windows"
)

// IsAvailable probes the ConPTY API (Win10 1809+ / kernel32 pseudo-console procs).
func IsAvailable() bool { return conpty.IsConPtyAvailable() }

// statusDLLInitFailed is STATUS_DLL_INIT_FAILED: the code a ConPTY child comes back
// with when the process context (a Windows job object) refuses it, and the one
// CreateProcess-wrapped-in-a-job reports through the API in the same situation.
const statusDLLInitFailed = 0xC0000142

// IsHostBlocked reports whether err means the OS refused a ConPTY child in this
// process context rather than the pty API being unusable: being inside a job object
// that does not allow breakaway makes CreateProcess fail (ERROR_ACCESS_DENIED) or
// the child die at load with STATUS_DLL_INIT_FAILED. Tests use it to skip with a
// clear reason (see ptytest) instead of reporting a bare error code.
func IsHostBlocked(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.EACCES) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uint32(errno) == statusDLLInitFailed {
		return true
	}
	return false
}

// IsBlockedExitCode reports whether a child exit code means the host refused the
// ConPTY child instead of the command running (STATUS_DLL_INIT_FAILED): what a job
// object produces for a child it blocks. See IsHostBlocked.
func IsBlockedExitCode(code int) bool { return uint32(code) == statusDLLInitFailed }

// Start runs spec under a ConPTY. Unlike creack (which takes an *exec.Cmd), the
// conpty API takes ONE command-line string, so (Command,Args) is joined into a
// safely-quoted command line via windows.EscapeArg (no shell involved).
func Start(spec Spec) (Pty, error) {
	opts := make([]conpty.ConPtyOption, 0, 3)
	if spec.Cols > 0 && spec.Rows > 0 {
		opts = append(opts, conpty.ConPtyDimensions(spec.Cols, spec.Rows))
	}
	if spec.Dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(spec.Dir))
	}
	if len(spec.Env) > 0 {
		opts = append(opts, conpty.ConPtyEnv(dedupEnvLast(spec.Env)))
	}
	c, err := conpty.Start(buildCommandLine(spec.Command, spec.Args), opts...)
	if err != nil {
		return nil, err
	}
	p := &winPty{c: c, exited: make(chan struct{}), readEnd: make(chan struct{})}
	go p.reap()
	return p, nil
}

// drainQuiet is how long the windows drain accepts silence on the output pipe as "the
// child's output has been delivered". ConPTY hands the child's bytes to the parent
// through conhost, and that pipe does NOT report EOF while the pseudo console is open
// — ClosePseudoConsole is what ends the stream — so EOF cannot be the drain's signal
// the way it is on unix. Silence is: output arrives in chunks, and the render lag
// observed on this host is a few hundred bytes to a few KiB, far inside this window.
var drainQuiet = 250 * time.Millisecond

// drainPoll is how often the drain re-checks the pipe while it waits.
const drainPoll = 20 * time.Millisecond

// statusStillActive is STILL_ACTIVE (259): what Wait reports for a cancelled context,
// matching what the underlying conpty.Wait answered for it.
const statusStillActive = 259

// winPty adapts *conpty.ConPty to the Pty interface.
type winPty struct {
	c *conpty.ConPty
	// closeOnce guards the close of the underlying handles: conpty.Close closes the
	// process/thread/pseudo-console and the four pipe handles unconditionally, and a
	// second close would close handle values that Windows may have reused since —
	// observed to take the test binary down silently. Close is expected to be safe to
	// call twice (defer + a fixture's cleanup both do it).
	closeOnce sync.Once
	closeErr  error

	exited  chan struct{} // closed once the child is known to have exited; code/waitErr valid after
	code    int
	waitErr error

	lastRead atomic.Int64 // UnixNano of the last Read that returned data; 0 = none yet
	readEnd  chan struct{} // closed when a Read first fails (EOF/error/closed)
	readOnce sync.Once
}

// reap publishes the child's exit. ConPTY has no exit notification, and the only
// handle to the process lives inside the ConPty value, so the exit is taken on its own
// goroutine — conpty.Wait blocks in WaitForSingleObject and wakes as soon as the
// process is signalled. Close has to know whether the child exited on its own (even
// when the caller never called Wait) before it decides to drain.
func (p *winPty) reap() {
	code, err := p.c.Wait(context.Background())
	p.code, p.waitErr = int(code), err
	close(p.exited)
}

func (p *winPty) Read(b []byte) (int, error) {
	n, err := p.c.Read(b)
	if n > 0 {
		p.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil {
		p.readOnce.Do(func() { close(p.readEnd) })
	}
	return n, err
}

func (p *winPty) Write(b []byte) (int, error) { return p.c.Write(b) }
func (p *winPty) Resize(cols, rows int) error { return p.c.Resize(cols, rows) }

// Close releases the pseudo-console and the pipes.
//
// When the child has already exited on its own, Close first drains (see drain): the
// bytes it wrote right before exiting may still be in conhost on their way to the
// output pipe, and closing the pseudo-console discards whatever has not been handed
// over yet. A live child (cancel) is torn down at once, as before.
func (p *winPty) Close() error {
	p.closeOnce.Do(func() {
		p.drain()
		p.closeErr = p.c.Close()
	})
	return p.closeErr
}

// drain waits for the output the child wrote before exiting to reach the reader, so
// that closing the pseudo-console does not throw it away. It returns as soon as the
// pipe has been quiet for drainQuiet — no Read returned data for that long, i.e. the
// reader has everything conhost had — and at the latest at drainGrace after the call,
// so a reader that lags or does not exist cannot make Close hang (the unix backend
// drains to EOF with the same bound; see gopty.drainGrace).
func (p *winPty) drain() {
	select {
	case <-p.exited:
	default:
		return // child still running: the caller is tearing it down on purpose
	}
	// The quiet window is measured from the start of the drain as well as from the last
	// read, so output a reader is about to pick up still gets its window even if nothing
	// has read yet, and Close stays bounded either way.
	quietSince := time.Now()
	deadline := quietSince.Add(drainGrace)
	for time.Now().Before(deadline) {
		if last := time.Unix(0, p.lastRead.Load()); last.After(quietSince) {
			quietSince = last
		}
		if time.Since(quietSince) >= drainQuiet {
			return
		}
		select {
		case <-p.readEnd:
			return // the stream already ended: nothing is left to drain
		default:
		}
		time.Sleep(drainPoll)
	}
}

// Wait returns the child's exit code once it has exited (the reaper runs from Start). A
// real process exit is (code, nil); a cancelled context reports "no exit code yet"
// (STILL_ACTIVE) without reaping, the way the underlying conpty.Wait did.
func (p *winPty) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.exited:
		return p.code, p.waitErr
	case <-ctx.Done():
		return statusStillActive, fmt.Errorf("wait canceled: %v", ctx.Err())
	}
}

// buildCommandLine joins the command + args into a single Windows command line,
// quoting each token per the CommandLineToArgvW rules (windows.EscapeArg) so a
// space/quote in an arg cannot inject an extra token.
func buildCommandLine(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, windows.EscapeArg(command))
	for _, a := range args {
		parts = append(parts, windows.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

// dedupEnvLast keeps the LAST occurrence of each name, preserving order — the rule
// os/exec applies on both platforms (dedupEnvCase) and the rule Spec.Env's callers
// layer by: the caller's own variables are appended after the inherited environment, so
// a duplicated name must resolve to the caller's value.
//
// The ConPTY binding hands the block straight to CreateProcess, which keeps the FIRST
// occurrence instead (undocumented but observable), so without this a pty child silently
// keeps the parent's PATH — and any other variable the parent happened to carry —
// rather than the value the caller had just set. Names are compared case-insensitively:
// Windows environment variable names are.
func dedupEnvLast(env []string) []string {
	if len(env) < 2 {
		return env
	}
	out := make([]string, 0, len(env))
	seen := make(map[string]bool, len(env))
	for i := len(env) - 1; i >= 0; i-- {
		name, _, ok := strings.Cut(env[i], "=")
		if !ok {
			name = env[i]
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, env[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
