//go:build windows

package gopty

import (
	"context"
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
