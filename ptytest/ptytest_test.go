//go:build unix

package ptytest_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inhere/pty"
	"github.com/inhere/pty/ptytest"
)

// TestPromptRoundTrip is the point of the fixtures: wait for a prompt, type an
// answer, assert on what the program printed back, and check the exit code.
func TestPromptRoundTrip(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)

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

// TestMarkOnlySeesNewOutput pins the assertion rule: a Mark taken after the first
// output must not satisfy a check for that earlier text.
func TestMarkOnlySeesNewOutput(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "first-line\n"; printf "name? "; read n; echo got=$n`)

	mark := s.Mark()
	if !s.Expect(`first-line`, mark) {
		t.Fatalf("the first line never appeared, output:\n%s", s.Text())
	}

	later := s.Mark()
	if s.Contains(`first-line`, later) {
		t.Fatalf("output before Mark leaked into the check:\n%q", s.Since(later))
	}

	s.SendLine("x")
	if !s.Expect(`got=x`, later) {
		t.Fatalf("no answer after Mark, output:\n%s", s.Since(later))
	}
	if s.Count(`first-line`, later) != 0 {
		t.Fatalf("Count looked before Mark:\n%q", s.Since(later))
	}
}

// TestCtrlCInterrupts proves the key bytes reach the child: Ctrl-C ends an
// interactive read, and the shell reports the signal through a non-zero status.
func TestCtrlCInterrupts(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)

	mark := s.Mark()
	if !s.Expect(`name\?`, mark) {
		t.Fatalf("the prompt never appeared, output:\n%s", s.Text())
	}

	s.SendCtrlC()
	code, err := s.WaitExit(5 * time.Second)
	if err != nil {
		t.Fatalf("WaitExit: %v, output:\n%s", err, s.Text())
	}
	if code == 0 {
		t.Fatalf("Ctrl-C did not interrupt the read (exit %d), output:\n%s", code, s.Text())
	}
	if s.Contains(`got=`, mark) {
		t.Fatalf("the read completed anyway:\n%s", s.Since(mark))
	}
}

// TestStripsANSI proves Text is assertable while RawText still carries the escapes.
func TestStripsANSI(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "\033[32mgreen\033[0m\n"`)

	mark := s.Mark()
	if !s.Expect(`green`, mark) {
		t.Fatalf("no coloured line, output:\n%s", s.Text())
	}
	if strings.Contains(s.Text(), "\x1b") {
		t.Fatalf("Text kept ANSI escapes: %q", s.Text())
	}
	if !strings.Contains(s.RawText(), "\x1b[32m") {
		t.Fatalf("RawText lost ANSI escapes: %q", s.RawText())
	}
}

// TestSendEnvAndSize uses StartSpec to pin the Spec knobs the fixtures pass through.
func TestSendEnvAndSize(t *testing.T) {
	s := ptytest.StartSpec(t, pty.Spec{
		Command: "sh",
		Args:    []string{"-c", `stty size; echo env=$PTY_FIXTURE`},
		Env:     append(os.Environ(), "PTY_FIXTURE=yes"),
		Cols:    120,
		Rows:    40,
	})

	mark := s.Mark()
	if !s.Expect(`env=yes`, mark) {
		t.Fatalf("env was not passed through, output:\n%s", s.Text())
	}
	if !s.Expect(`40 120`, mark) {
		t.Fatalf("initial size was not applied, output:\n%s", s.Text())
	}
	if code, err := s.WaitExit(); err != nil || code != 0 {
		t.Fatalf("WaitExit = (%d, %v), output:\n%s", code, err, s.Text())
	}
}
