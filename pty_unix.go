//go:build unix

package pty

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	creack "github.com/creack/pty"
)

// IsAvailable is always true on unix: creack/pty opens /dev/ptmx at Start time
// and a failure surfaces there (probing here would just duplicate that).
func IsAvailable() bool { return true }

// IsHostBlocked is always false on unix: no platform mechanism blocks a pty child
// the way a Windows job object does (see the windows file).
func IsHostBlocked(error) bool { return false }

// IsBlockedExitCode is always false on unix: a unix pty child always runs.
func IsBlockedExitCode(int) bool { return false }

// drainGrace bounds how long Close, after the child has exited on its own, waits
// for the reader to drain the pty to EOF before closing the master. A reader that
// keeps up hits EOF almost at once; the bound only matters when nobody reads, or
// when a grandchild that outlived the child still holds the terminal (EOF then
// never comes and the master is closed anyway, as before).
var drainGrace = 2 * time.Second

// Start runs spec under a freshly allocated pty (unix: creack/pty).
//
// Unlike creack.Start, the parent keeps its own slave fd until the child has been
// reaped. On darwin the child's exit would otherwise be the LAST close of the slave,
// and the BSD tty layer flushes output the master has not read yet on that close —
// a child that printed and exited before the reader caught up lost its output, all
// of it for a short-lived one. Holding the slave makes the reaper's close the last
// one; it runs only after the exit is known, and the reader drains the buffer to EOF
// before Close tears the master down (see Close).
func Start(spec Spec) (Pty, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env // nil env => inherit os.Environ (exec default)
	}

	ptmx, tty, err := creack.Open()
	if err != nil {
		return nil, err
	}
	if spec.Cols > 0 && spec.Rows > 0 {
		// Set the window size BEFORE exec so a size-sensitive program reads the
		// right dimensions from its first tcgetwinsize.
		err = creack.Setsize(ptmx, &creack.Winsize{Rows: uint16(spec.Rows), Cols: uint16(spec.Cols)})
		if err != nil {
			_ = ptmx.Close()
			_ = tty.Close()
			return nil, err
		}
	}
	// Same child wiring as creack.Start: the tty is stdio, and the child leads a
	// new session with the tty (its fd 0) as controlling terminal.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, err
	}

	p := &unixPty{ptmx: ptmx, cmd: cmd, exited: make(chan struct{}), readEnd: make(chan struct{})}
	go p.reap(tty)
	return p, nil
}

// unixPty wraps the pty master fd + the child cmd.
type unixPty struct {
	ptmx *os.File
	cmd  *exec.Cmd

	exited   chan struct{} // closed once the child is reaped; code/waitErr valid after
	code     int
	waitErr  error
	readEnd  chan struct{} // closed when a Read first fails (EOF/EIO/closed)
	readOnce sync.Once
}

// reap is the single cmd.Wait. It publishes the exit, then releases the parent's
// slave fd: that close is now the last one, so the master reads the rest of the
// buffer and then EOF (linux: EIO). On darwin a blocking last close waits for the
// master to drain the output queue, which the reader is doing — and Close's
// bounded drain wait ends it regardless by closing the master.
func (p *unixPty) reap(tty *os.File) {
	werr := p.cmd.Wait()
	switch {
	case werr == nil:
	case errors.As(werr, new(*exec.ExitError)):
		p.code = p.cmd.ProcessState.ExitCode() // clean process exit (maybe non-zero); no error
	default:
		p.code, p.waitErr = -1, werr // genuine reap failure
	}
	close(p.exited)
	_ = tty.Close()
}

func (p *unixPty) Read(b []byte) (int, error) {
	n, err := p.ptmx.Read(b)
	if err != nil {
		p.readOnce.Do(func() { close(p.readEnd) })
	}
	return n, err
}

func (p *unixPty) Write(b []byte) (int, error) { return p.ptmx.Write(b) }

func (p *unixPty) Resize(cols, rows int) error {
	return creack.Setsize(p.ptmx, &creack.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Close closes the master fd and kills the child. It is the "close master +
// kill child" primitive; the caller's state machine decides WHEN to call it.
//
// When the child has already exited on its own, Close first waits (bounded by
// drainGrace) for the reader to reach EOF: the output the child wrote right before
// exiting may still sit in the pty, and closing the master under the reader would
// discard it. A live child (cancel) is torn down at once, as before.
func (p *unixPty) Close() error {
	select {
	case <-p.exited:
		timer := time.NewTimer(drainGrace)
		select {
		case <-p.readEnd:
		case <-timer.C:
		}
		timer.Stop()
	default:
	}
	cerr := p.ptmx.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill() // a no-op error once reaped
	}
	return cerr
}

// Wait returns the child's exit code once it has been reaped (the reap itself runs
// in the background from Start, so the slave can be released as soon as the child
// is gone even if nobody calls Wait yet). A REAL process exit — even a non-zero or
// signalled one — returns (code, nil): the exit code carries the outcome (parity
// with the windows conpty backend). Only a genuine reap failure returns a non-nil
// error. A ctx cancellation returns early with ctx.Err(); the caller kills via
// Close and calls Wait again (or relies on its own first call) to observe the exit.
func (p *unixPty) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-p.exited:
		return p.code, p.waitErr
	}
}
