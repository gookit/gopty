//go:build windows

package gopty

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStartKeepsExtraEnvOverInherited pins the env layering Spec.Env's callers depend
// on: the caller's variables are appended AFTER the inherited environment, so for a
// duplicated name the caller's value must reach the child — exactly what os/exec does
// on unix and Windows (dedupEnvCase, "last occurrence wins").
//
// The ConPTY binding passes the block straight to CreateProcess, which keeps the FIRST
// occurrence, so pty.Start de-duplicates before handing it over. Without that, a pty
// child kept the parent's PATH (and any other variable the parent carried) instead of
// the value the caller had just set.
func TestStartKeepsExtraEnvOverInherited(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}

	env := append(os.Environ(), "PTY_DUP_TEST=inherited", "PTY_DUP_TEST=extra")
	p, err := Start(Spec{
		Command: "cmd.exe",
		Args:    []string{"/c", "echo PTY_DUP_TEST=%PTY_DUP_TEST%"},
		Env:     env,
		Cols:    100,
		Rows:    30,
	})
	if err != nil {
		skipIfHostBlocked(t, err)
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Close() }()

	out := &ptySink{}
	go func() { _, _ = io.Copy(out, p) }()

	exitCh := make(chan exitResult, 1)
	go func() {
		code, werr := p.Wait(context.Background())
		exitCh <- exitResult{code: code, err: werr}
	}()

	got, exited := waitForMarker(out, "PTY_DUP_TEST=", exitCh)
	switch {
	case got != "":
		if !strings.Contains(got, "PTY_DUP_TEST=extra") {
			t.Fatalf("the child did not get the caller's value:\n%q", got)
		}
		if strings.Contains(got, "inherited") {
			t.Fatalf("the inherited value reached the child:\n%q", got)
		}
	case exited != nil:
		skipIfHostBlocked(t, exited.err)
		if IsBlockedExitCode(exited.code) {
			t.Skipf("a ConPTY child cannot run in this process context (job object): exit code 0x%x",
				uint32(exited.code))
		}
		t.Fatalf("the child exited (%d, %v) before printing the marker: %q", exited.code, exited.err, out.String())
	default:
		t.Fatalf("no %q in the child's output within the deadline: %q", "PTY_DUP_TEST=", out.String())
	}
}

// TestCloseAfterExitKeepsConPTYOutput is the regression test for output lost at Close:
// the child prints a long burst and exits right away, and everything it printed must
// still arrive even though Close tears the pty down as soon as the exit is known.
//
// ConPTY does not hand the child's bytes to the parent directly: conhost renders the
// console buffer and writes it to the output pipe asynchronously, so at the moment the
// child is reaped a good part of the burst is still in conhost/the pipe. Closing the
// pseudo-console there (what Close did before) discards it — a writer lost the last
// lines it had already printed. The reader is paced rather than greedy so the amount
// Close has not seen yet is a property of the backend, not of the scheduler.
func TestCloseAfterExitKeepsConPTYOutput(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}

	const lines = 400
	// ~55 bytes per line, so the burst (~22 KiB) fits in the output pipe (~64 KiB): the
	// child is not slowed down by its own output and exits right after writing it.
	filler := strings.Repeat("x", 40)
	burst := fmt.Sprintf("for /L %%i in (1,1,%d) do @echo PTYDRAIN-%%i-%s", lines, filler)
	wantTail := fmt.Sprintf("PTYDRAIN-%d-%s", lines, filler)

	p, err := Start(Spec{Command: "cmd.exe", Args: []string{"/c", burst}, Cols: 200, Rows: 50})
	if err != nil {
		skipIfHostBlocked(t, err)
		t.Fatalf("Start: %v", err)
	}

	// The pump reads while the child runs, the way a real consumer does.
	out := &ptySink{}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		copyPaced(out, p, 1024, 5*time.Millisecond)
	}()

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, werr := p.Wait(waitCtx)
	if werr != nil {
		_ = p.Close()
		skipIfHostBlocked(t, werr)
		t.Fatalf("Wait: %v", werr)
	}
	if IsBlockedExitCode(code) {
		_ = p.Close()
		t.Skipf("a ConPTY child cannot run in this process context (job object): exit code 0x%x", uint32(code))
	}

	_ = p.Close()
	waitClosed(t, pumpDone, 30*time.Second, "the pump to stop after Close")

	got := out.String()
	t.Logf("exit %d, %d bytes read, %d/%d burst lines", code, len(got), strings.Count(got, "PTYDRAIN-"), lines)
	if !strings.Contains(got, wantTail) {
		t.Fatalf("the tail of the child's output was lost by Close: %d/%d burst lines arrived, missing up to %q; output ends with %q",
			strings.Count(got, "PTYDRAIN-"), lines, wantTail, tailOf(got, 160))
	}
}

// TestCloseWithoutReaderIsBoundedAfterExit: with the child already gone and nobody
// reading the pipe, Close has nothing to drain and must still return promptly — the
// drain of an exited child is bounded by drainGrace, it never waits for a reader that
// does not exist.
func TestCloseWithoutReaderIsBoundedAfterExit(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}

	p, err := Start(Spec{Command: "cmd.exe", Args: []string{"/c", "echo unread"}, Cols: 80, Rows: 24})
	if err != nil {
		skipIfHostBlocked(t, err)
		t.Fatalf("Start: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, werr := p.Wait(waitCtx)
	if werr != nil {
		_ = p.Close()
		skipIfHostBlocked(t, werr)
		t.Fatalf("Wait: %v", werr)
	}
	if IsBlockedExitCode(code) {
		_ = p.Close()
		t.Skipf("a ConPTY child cannot run in this process context (job object): exit code 0x%x", uint32(code))
	}

	closed := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(closed)
		_ = p.Close()
	}()
	waitClosed(t, closed, drainGrace+10*time.Second, "Close to return with no reader")
	t.Logf("Close with no reader returned after %s (drainGrace %s)", time.Since(start).Round(time.Millisecond), drainGrace)
}

// copyPaced copies r into w in small chunks with a gap between reads: it stands in for
// a consumer that does not drain the pipe in one gulp, which keeps the amount of output
// still in flight when Close runs a property of the backend instead of a race with the
// scheduler.
func copyPaced(w io.Writer, r io.Reader, chunk int, gap time.Duration) {
	buf := make([]byte, chunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			return
		}
		time.Sleep(gap)
	}
}

// waitClosed fails the test when ch is not closed within d.
func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %s", d, what)
	}
}

// tailOf returns the last n bytes of s, for a readable failure message.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// exitResult is one Wait outcome.
type exitResult struct {
	code int
	err  error
}

// skipIfHostBlocked skips the test when the OS refused the ConPTY child in this
// process context (see pty.IsHostBlocked) instead of failing on an error code a
// reader cannot act on.
func skipIfHostBlocked(t *testing.T, err error) {
	t.Helper()
	if IsHostBlocked(err) {
		t.Skipf("this process context cannot host a ConPTY child (job object): %v", err)
	}
}

// ptySink is a concurrency-safe sink for the pty pump (the reader goroutine writes
// while the test polls).
type ptySink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *ptySink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *ptySink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForMarker polls the pump until the accumulated output carries needle. A ConPTY
// stream arrives in escape-sequence-wrapped chunks, so polling for the marker is the
// honest signal. It also watches the child's exit: a child that dies without printing
// (the job-object case) is reported instead of waiting out the deadline.
func waitForMarker(out *ptySink, needle string, exitCh <-chan exitResult) (string, *exitResult) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := out.String(); strings.Contains(got, needle) {
			return got, nil
		}
		select {
		case res := <-exitCh:
			return "", &res
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", nil
}
