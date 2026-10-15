// Package ptytest drives a program inside a pty from ordinary Go tests: start it,
// wait for the prompt, type, assert on the output, then shut it down.
//
// It is the Go counterpart of a "pty smoke script" (the same semantics a Python
// pty driver or a Windows-console harness provides), built on github.com/gookit/gopty
// so the same test code runs on unix and on Windows (where the pty backend is
// ConPTY). It is a *test* package: it takes a *testing.T, fails/skips it with a
// useful message, and registers its own cleanup.
//
// Typical use:
//
//	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)
//	mark := s.Mark()
//	if !s.Expect("name\\?", mark) {
//		t.Fatalf("no prompt, output:\n%s", s.Text())
//	}
//	s.SendLine("tom")
//	if !s.Expect(`got=tom`, mark) {
//		t.Fatalf("no answer, output:\n%s", s.Text())
//	}
//	if code, err := s.WaitExit(); err != nil || code != 0 {
//		t.Fatalf("exit = (%d, %v)", code, err)
//	}
//
// Rules of thumb (learned the hard way, see the README):
//   - assert on output produced after a Mark, never on everything: earlier turns
//     would satisfy the check by accident;
//   - wait for the prompt (Expect) before typing: a program that clears pending
//     input before asking drops too-early keystrokes;
//   - Enter is "\r", Ctrl-C is 0x03, Ctrl-D is 0x04 (see the Send* helpers);
//   - Text strips ANSI escapes, RawText keeps them (colour-only output).
package ptytest

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gookit/gopty"
)

// Terminal keys a test usually needs to send.
const (
	Enter = "\r"
	CtrlC = "\x03"
	CtrlD = "\x04"
	CtrlU = "\x15"
)

// defaultTimeout bounds every wait when the caller does not pass one.
const defaultTimeout = 10 * time.Second

// ansiCSI matches the escape sequences Text strips: CSI ... final-byte.
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// Session is one program running under a pty with its output collected.
type Session struct {
	t *testing.T
	p gopty.Pty

	mu         sync.Mutex
	buf        bytes.Buffer
	raw        bytes.Buffer
	reaped     bool // the child has been waited for
	code       int
	werr       error

	exitOnce   sync.Once
	exited     chan struct{} // closed when the child has been reaped
	readerDone chan struct{} // closed when the reader stopped draining the master
}

// Start runs command+args under a pty, collects its output in the background and
// registers the cleanup (Close). It skips the test when no pty backend is available
// on this host, and when the OS refuses to host a pty child (a Windows job object).
//
// Environment and working directory are inherited; use StartSpec to set them.
func Start(t *testing.T, command string, args ...string) *Session {
	t.Helper()
	return StartSpec(t, gopty.Spec{Command: command, Args: args, Cols: 120, Rows: 40})
}

// StartSpec is Start with a full gopty.Spec (env, dir, initial size).
func StartSpec(t *testing.T, spec gopty.Spec) *Session {
	t.Helper()
	if !gopty.IsAvailable() {
		t.Skipf("no pty backend on this host (%s)", runtime.GOOS)
	}

	p, err := gopty.Start(spec)
	if err != nil {
		if gopty.IsHostBlocked(err) {
			t.Skipf("this process context cannot host a pty child: %v", err)
		}
		t.Fatalf("ptytest: start %s: %v", spec.Command, err)
	}

	s := &Session{t: t, p: p, exited: make(chan struct{}), readerDone: make(chan struct{})}
	go s.pump()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// pump starts the reaper and then reads the master until the stream ends. The two
// run in parallel because the end of the stream is not a portable "child exited"
// signal: on Windows (ConPTY) the output pipe does not report EOF when the child
// exits, so a fixture that reaped only after Read returned would wait forever.
// The reaper therefore closes the master once the child is gone — the reader then
// drains the last output and stops, which is what lets WaitExit/Text be reliable.
func (s *Session) pump() {
	go func() {
		code, werr := s.p.Wait(context.Background())
		s.mu.Lock()
		s.reaped, s.code, s.werr = true, code, werr
		s.mu.Unlock()
		_ = s.p.Close()
		s.exitOnce.Do(func() { close(s.exited) })
	}()

	defer close(s.readerDone)
	buf := make([]byte, 4096)
	for {
		n, err := s.p.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.raw.Write(buf[:n])
			s.buf.WriteString(ansiCSI.ReplaceAllString(string(buf[:n]), ""))
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Mark returns the current end of the output. Assert only on what comes after it —
// earlier output would satisfy a check by accident.
func (s *Session) Mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// Text returns everything received so far, with ANSI escapes stripped.
func (s *Session) Text() string { return s.Since(0) }

// Since returns the output received after mark, with ANSI escapes stripped.
func (s *Session) Since(mark int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buf.Bytes()
	if mark < 0 || mark > len(b) {
		mark = 0
	}
	return string(b[mark:])
}

// RawText returns everything received so far, escape codes kept (colour-only output
// such as a highlighted input line is invisible to Text).
func (s *Session) RawText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw.String()
}

// Contains reports whether the output after mark matches pattern (a regexp; a plain
// string matches literally).
func (s *Session) Contains(pattern string, mark int) bool { return s.matches(pattern, mark) }

// Count returns how often pattern matches in the output after mark.
func (s *Session) Count(pattern string, mark int) int {
	return len(regexp.MustCompile(pattern).FindAllString(s.Since(mark), -1))
}

// Expect waits until the output after mark matches pattern, and reports whether it
// did within the timeout (defaultTimeout when none is given).
//
// A pty child that the host blocked (Windows job object) never prints anything: that
// case skips the test instead of burning the timeout, so the reason is visible.
func (s *Session) Expect(pattern string, mark int, timeout ...time.Duration) bool {
	d := firstOr(timeout, defaultTimeout)
	deadline := time.Now().Add(d)
	for {
		if s.matches(pattern, mark) {
			return true
		}
		if s.Code() != nil {
			// The child is gone: let the reader finish the tail, then decide once.
			s.waitReader()
			if s.matches(pattern, mark) {
				return true
			}
			s.skipIfBlocked()
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Send writes text to the child (no newline added).
func (s *Session) Send(text string) {
	s.t.Helper()
	if _, err := s.p.Write([]byte(text)); err != nil {
		s.t.Fatalf("ptytest: send %q: %v", text, err)
	}
}

// SendLine writes text followed by Enter, as a keyboard does.
func (s *Session) SendLine(text string) { s.Send(text + Enter) }

// SendCtrlC interrupts the child (the byte a terminal sends for Ctrl-C).
func (s *Session) SendCtrlC() { s.Send(CtrlC) }

// WaitExit waits for the child to exit and returns its exit code and reap error.
//
// Before returning it waits (bounded) for the reader to drain the master, so a
// Text() call after WaitExit still carries the output the child wrote right before
// exiting.
func (s *Session) WaitExit(timeout ...time.Duration) (int, error) {
	d := firstOr(timeout, defaultTimeout)
	select {
	case <-s.exited:
		s.waitReader()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.werr != nil {
			return s.code, s.werr
		}
		return s.code, nil
	case <-time.After(d):
		s.t.Fatalf("ptytest: the child did not exit within %s, output:\n%s", d, s.Text())
		return -1, errors.New("timeout")
	}
}

// waitReader waits (bounded) until the reader has drained the master.
func (s *Session) waitReader() {
	select {
	case <-s.readerDone:
	case <-time.After(time.Second):
	}
}

// Code returns the child's exit code when it has already been reaped, else nil.
func (s *Session) Code() *int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reaped {
		return nil
	}
	code := s.code
	return &code
}

// Close kills the child (if alive) and releases the gopty. It is registered by Start
// as the test cleanup; call it directly to tear down earlier.
func (s *Session) Close() error {
	err := s.p.Close()
	s.exitOnce.Do(func() { close(s.exited) })
	return err
}

// matches reports whether the output after mark matches pattern.
func (s *Session) matches(pattern string, mark int) bool {
	return regexp.MustCompile(pattern).MatchString(s.Since(mark))
}

// skipIfBlocked skips the test when the child was refused by the host (Windows job
// object) rather than failing on a bare exit code.
func (s *Session) skipIfBlocked() {
	s.mu.Lock()
	code, werr := s.code, s.werr
	s.mu.Unlock()
	if werr != nil && gopty.IsHostBlocked(werr) || gopty.IsBlockedExitCode(code) {
		s.t.Skipf("this process context cannot host a pty child (job object): exit code %d, err %v", code, werr)
	}
}

func firstOr(ds []time.Duration, def time.Duration) time.Duration {
	if len(ds) > 0 && ds[0] > 0 {
		return ds[0]
	}
	return def
}
