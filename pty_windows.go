//go:build windows

package gopty

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"

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
	return &winPty{c: c}, nil
}

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
}

func (p *winPty) Read(b []byte) (int, error)  { return p.c.Read(b) }
func (p *winPty) Write(b []byte) (int, error) { return p.c.Write(b) }
func (p *winPty) Resize(cols, rows int) error { return p.c.Resize(cols, rows) }

func (p *winPty) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.c.Close() })
	return p.closeErr
}

func (p *winPty) Wait(ctx context.Context) (int, error) {
	code, err := p.c.Wait(ctx)
	return int(code), err
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
