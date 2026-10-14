//go:build unix

package pty

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStartSizeAndExit proves the whole unix contract in one shot: Start applies
// the initial window size (a size-sensitive program reads it), Read carries the
// child's output, and Wait returns a clean exit code.
func TestStartSizeAndExit(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}
	// `stty size` prints "<rows> <cols>"; we start it at 40x120.
	p, err := Start(Spec{
		Command: "sh",
		Args:    []string{"-c", "stty size; echo done"},
		Cols:    120,
		Rows:    40,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	out := readAll(t, p, 2*time.Second)
	if !strings.Contains(out, "40 120") {
		t.Fatalf("stty size did not reflect initial 40x120, got %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("missing child output, got %q", out)
	}

	code, werr := p.Wait(context.Background())
	if werr != nil {
		t.Fatalf("Wait err: %v", werr)
	}
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestResizeAndIO proves Resize succeeds on a live pty and that input written to
// the master is echoed back on Read (a real interactive round-trip through cat).
func TestResizeAndIO(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}
	p, err := Start(Spec{Command: "cat", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if err := p.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if _, err := p.Write([]byte("hello-pty\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// cat echoes the line back through the pty (terminal echo + program echo).
	got := readUntil(t, p, "hello-pty", 2*time.Second)
	if !strings.Contains(got, "hello-pty") {
		t.Fatalf("input not echoed, got %q", got)
	}
}

// TestKillAndWait proves the teardown primitive: a live child is killed by Close
// and Wait still reports an exit code for it.
func TestKillAndWait(t *testing.T) {
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}
	p, err := Start(Spec{Command: "cat", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitUntil(t, 5*time.Second, "Wait to return after Close", func() bool {
		_, werr := p.Wait(context.Background())
		return werr == nil
	})
}

// readAll reads to EOF (bounded by a deadline via a watchdog Close).
func readAll(t *testing.T, r io.ReadCloser, d time.Duration) string {
	t.Helper()
	done := make(chan struct{})
	timer := time.AfterFunc(d, func() { _ = r.Close() })
	defer timer.Stop()
	var sb strings.Builder
	go func() {
		_, _ = io.Copy(&sb, r)
		close(done)
	}()
	<-done
	return sb.String()
}

// readUntil reads until the accumulated output contains needle or the deadline
// elapses.
func readUntil(t *testing.T, r io.Reader, needle string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	var sb strings.Builder
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		type rr struct {
			n   int
			err error
		}
		ch := make(chan rr, 1)
		go func() { n, err := r.Read(buf); ch <- rr{n, err} }()
		select {
		case res := <-ch:
			if res.n > 0 {
				sb.Write(buf[:res.n])
				if strings.Contains(sb.String(), needle) {
					return sb.String()
				}
			}
			if res.err != nil {
				return sb.String()
			}
		case <-time.After(time.Until(deadline)):
			return sb.String()
		}
	}
	return sb.String()
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestOutputReadableAfterReap: the child prints and is REAPED before a single byte
// is read; the output must still be readable in full and then end in EOF. The
// parent holds its own slave fd until the reap so the child's exit is not the
// slave's last close (darwin flushes unread output on that close), and releases it
// right after — without the release the read below would never see EOF.
func TestOutputReadableAfterReap(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// On darwin a session leader's exit waits for its terminal's output to drain
		// (BSD exit -> ttywait), so a child cannot exit before somebody reads the
		// master: the "exited, nobody read yet" state this test builds never happens
		// there and the test would deadlock. Real sessions always read concurrently.
		t.Skip("darwin: a session leader's exit waits for the pty output to be read")
	}
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}
	p, err := Start(Spec{Command: "sh", Args: []string{"-c", "printf 'tail-of-output\\n'"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if code, werr := p.Wait(context.Background()); werr != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v), want (0, nil)", code, werr)
	}

	var (
		mu  sync.Mutex
		out strings.Builder
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			n, rerr := p.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if rerr != nil {
				return
			}
		}
	}()
	waitUntil(t, 10*time.Second, "the read reaches EOF after the reap", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out.String(), "tail-of-output") {
		t.Fatalf("output written before exit was lost: %q", out.String())
	}
}

// TestCloseAfterExitWithoutReaderIsBounded: Close after a natural exit waits for
// the reader to drain, but only up to drainGrace — with nobody reading it still
// returns.
func TestCloseAfterExitWithoutReaderIsBounded(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// On darwin a session leader's exit waits for its terminal's output to drain
		// (BSD exit -> ttywait), so a child cannot exit before somebody reads the
		// master: the "exited, nobody read yet" state this test builds never happens
		// there and the test would deadlock. Real sessions always read concurrently.
		t.Skip("darwin: a session leader's exit waits for the pty output to be read")
	}
	if !IsAvailable() {
		t.Skip("pty backend not available")
	}
	old := drainGrace
	drainGrace = 50 * time.Millisecond
	t.Cleanup(func() { drainGrace = old })

	p, err := Start(Spec{Command: "sh", Args: []string{"-c", "printf unread"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, werr := p.Wait(context.Background()); werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	closed := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closed)
	}()
	waitUntil(t, 10*time.Second, "Close returns with no reader", func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	})
}
