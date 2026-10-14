//go:build windows

package ptytest_test

import (
	"os"
	"testing"

	"github.com/inhere/pty"
	"github.com/inhere/pty/ptytest"
)

// TestPromptRoundTripWindows is the Windows twin of the unix round trip: the
// fixtures must work on a ConPTY too. `cmd /v:on` is needed because `%n%` would be
// expanded before `set /p` runs; the prompt text is what Expect waits for.
func TestPromptRoundTripWindows(t *testing.T) {
	s := ptytest.Start(t, "cmd.exe", "/v:on", "/c", `set /p n=name? & echo got=!n!`)

	mark := s.Mark()
	if !s.Expect(`name\?`, mark) {
		t.Fatalf("the prompt never appeared, output:\n%s", s.Text())
	}

	s.SendLine("tom")
	if !s.Expect(`got=tom`, mark) {
		t.Fatalf("the answer was not used, output:\n%s", s.Text())
	}

	code, err := s.WaitExit()
	if err != nil || code != 0 {
		t.Fatalf("WaitExit = (%d, %v), output:\n%s", code, err, s.Text())
	}
}

// TestStartSpecEnvOnWindows pins the Spec knobs on the ConPTY backend: the caller's
// Env reaches the child (with os.Environ() as the base, so the shell has PATH) and
// the initial window size is applied.
func TestStartSpecEnvOnWindows(t *testing.T) {
	s := ptytest.StartSpec(t, pty.Spec{
		Command: "cmd.exe",
		Args:    []string{"/c", "echo env=%PTY_FIXTURE% & mode con"},
		Env:     append(os.Environ(), "PTY_FIXTURE=yes"),
		Cols:    120,
		Rows:    40,
	})

	mark := s.Mark()
	if !s.Expect(`env=yes`, mark) {
		t.Fatalf("env was not passed through, output:\n%s", s.Text())
	}
	if !s.Expect(`Columns:\s+120`, mark) {
		t.Fatalf("initial size was not applied, output:\n%s", s.Text())
	}
	if code, err := s.WaitExit(); err != nil || code != 0 {
		t.Fatalf("WaitExit = (%d, %v), output:\n%s", code, err, s.Text())
	}
}
