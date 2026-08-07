//go:build unix && gtk3

package main

// T4.2 quit-lifecycle gate. The explicit "Quit and stop stack" orchestration is
// exercised through quitController with INJECTED seams — a recording stackDown
// stub and a counting quit — so the down argv, the happy-path quit, and the
// quit-anyway-on-failure default (the parked-fork tonight behavior) are all
// verified deterministically, with no real compass-stack exec and no display.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestStackDownArgs: the pure argv builder emits the exact `compass-stack down`
// invocation (down + --state-dir + --image + --socket) and OMITS both --database
// (compass-stack recomputes the default DSN from --state-dir) and --linger (down
// is not lingerable).
func TestStackDownArgs(t *testing.T) {
	args := stackDownArgs(baseParams)

	want := []string{
		"down",
		"--state-dir", baseParams.stateDir,
		"--image", baseParams.image,
		"--socket", baseParams.socket,
	}
	if !slices.Equal(args, want) {
		t.Errorf("stackDownArgs = %v, want %v", args, want)
	}
	if slices.Contains(args, "--database") {
		t.Errorf("argv carries --database, want it omitted (compass-stack defaults the DSN): %v", args)
	}
	if slices.Contains(args, "--linger") {
		t.Errorf("argv carries --linger, want it omitted (down is not lingerable): %v", args)
	}
}

// TestStopStackAndQuitHappyPath: a successful teardown runs down with EXACTLY
// the stackDownArgs(params) argv and then quits the app exactly once.
func TestStopStackAndQuitHappyPath(t *testing.T) {
	var gotArgs []string
	quitCount := 0
	c := quitController{
		stackDown: func(_ context.Context, args []string) error {
			gotArgs = args
			return nil
		},
		params:  baseParams,
		quit:    func() { quitCount++ },
		timeout: stackDownTimeout,
	}

	c.stopStackAndQuit(context.Background())

	if want := stackDownArgs(baseParams); !slices.Equal(gotArgs, want) {
		t.Errorf("stackDown argv = %v, want %v", gotArgs, want)
	}
	if quitCount != 1 {
		t.Errorf("quit called %d times, want exactly 1", quitCount)
	}
}

// TestStopStackAndQuitQuitsAnywayOnDownFailure: the parked-fork tonight default.
// When compass-stack down FAILS, the app STILL quits exactly once (a lingering
// stack is the safe failure; trapping the user in a live window is worse).
func TestStopStackAndQuitQuitsAnywayOnDownFailure(t *testing.T) {
	quitCount := 0
	c := quitController{
		stackDown: func(_ context.Context, _ []string) error {
			return errors.New("down boom")
		},
		params:  baseParams,
		quit:    func() { quitCount++ },
		timeout: stackDownTimeout,
	}

	c.stopStackAndQuit(context.Background())

	if quitCount != 1 {
		t.Errorf("quit called %d times on a down failure, want exactly 1 (quit-anyway default)", quitCount)
	}
}

// TestStopStackAndQuitBoundsTheDownContext: stopStackAndQuit derives a bounded
// context off the caller's, so the injected down seam sees a deadline rather
// than an unbounded context.
func TestStopStackAndQuitBoundsTheDownContext(t *testing.T) {
	hadDeadline := false
	c := quitController{
		stackDown: func(ctx context.Context, _ []string) error {
			_, hadDeadline = ctx.Deadline()
			return nil
		},
		params:  baseParams,
		quit:    func() {},
		timeout: 30 * time.Second,
	}

	c.stopStackAndQuit(context.Background())

	if !hadDeadline {
		t.Error("down context had no deadline, want it bounded by timeout")
	}
}
