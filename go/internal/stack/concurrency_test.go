//go:build unix

package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type upResult struct {
	stack *Stack
	err   error
}

// Two concurrent Ups on the same state dir must yield exactly one spawning
// stack: the O_EXCL lockfile closes the probe→spawn TOCTOU. The winner is gated
// inside its first (postgres) spawn while it holds the lock; the loser, finding
// the lock held and the server not yet answering, must error-as-contended rather
// than spawn a second chain. Deterministic — the winner is pinned at the gate,
// so the loser's outcome is observed before any spawn can complete; no sleeps,
// no retries.
func TestTwoConcurrentUpsExactlyOneSpawns(t *testing.T) {
	cfg, h := newHarness(t)

	// Gate the winner inside postgres Start: `entered` closes when a spawner
	// reaches it, `gate` holds it there (still holding the lock) until we release.
	entered := make(chan struct{})
	gate := make(chan struct{})
	h.sup.entered[ComponentPostgres] = entered
	h.sup.gate[ComponentPostgres] = gate

	results := make(chan upResult, 2)
	up := func() {
		s, err := Up(context.Background(), cfg, h.deps)
		results <- upResult{s, err}
	}
	go up()
	go up()

	// One goroutine won the lock and is now blocked inside postgres Start; the
	// other could not acquire the lock and runs to completion first.
	<-entered

	loser := <-results
	if loser.err == nil {
		t.Fatalf("loser Up() = nil error; want contended failure (a second spawn happened)")
	}
	if errors.Is(loser.err, ErrVersionMismatch) {
		t.Fatalf("loser Up() = version mismatch; want contended failure")
	}
	if loser.stack != nil {
		t.Fatal("loser Up() returned a Stack; want nil")
	}

	// Release the winner; it completes its single spawn chain.
	close(gate)
	winner := <-results
	if winner.err != nil {
		t.Fatalf("winner Up() = %v, want nil", winner.err)
	}
	if winner.stack == nil || winner.stack.attached {
		t.Fatalf("winner should own a freshly spawned (non-attached) stack, got %+v", winner.stack)
	}

	// Exactly one postgres spawn happened across both Ups — never two.
	if n := countEvent(h.rec.snapshot(), "start postgres"); n != 1 {
		t.Fatalf("start postgres happened %d times; want exactly 1", n)
	}

	// The winner still holds the lock; Down releases it.
	if err := winner.stack.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	assertLockFree(t, cfg.StateDir)
}

// A pre-existing STALE lockfile (holder pid gone, as a crashed prior Up leaves)
// plus two concurrent Ups must still yield exactly one spawn. Without the
// acquire guard, both racers pass the stale check and both remove-then-recreate
// the lockfile — the second clobbering the first's fresh lock — and both spawn a
// full stack against one state dir. The guard serializes the reclaim so only one
// racer consumes the stale file; the other observes the winner's fresh live lock
// and attaches-contended instead of spawning.
func TestConcurrentUpsOverStaleLockExactlyOneSpawns(t *testing.T) {
	cfg, h := newHarness(t)

	// Seed a stale lockfile naming a pid that cannot be live, so both racers enter
	// acquireLock's reclaim branch.
	stalePID := deadPID(t)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, lockFileName),
		[]byte(strconv.Itoa(stalePID)), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	// Gate the winner inside postgres Start (still holding the lock) so the
	// loser's outcome is observed before any spawn can complete.
	entered := make(chan struct{})
	gate := make(chan struct{})
	h.sup.entered[ComponentPostgres] = entered
	h.sup.gate[ComponentPostgres] = gate

	results := make(chan upResult, 2)
	up := func() {
		s, err := Up(context.Background(), cfg, h.deps)
		results <- upResult{s, err}
	}
	go up()
	go up()

	<-entered

	loser := <-results
	if loser.err == nil {
		t.Fatalf("loser Up() = nil error; want contended failure (a second spawn happened over the stale lock)")
	}
	if loser.stack != nil {
		t.Fatal("loser Up() returned a Stack; want nil")
	}

	close(gate)
	winner := <-results
	if winner.err != nil {
		t.Fatalf("winner Up() = %v, want nil", winner.err)
	}
	if winner.stack == nil || winner.stack.attached {
		t.Fatalf("winner should own a freshly spawned (non-attached) stack, got %+v", winner.stack)
	}

	if n := countEvent(h.rec.snapshot(), "start postgres"); n != 1 {
		t.Fatalf("start postgres happened %d times; want exactly 1 (stale-lock reclaim double-spawn)", n)
	}

	if err := winner.stack.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	assertLockFree(t, cfg.StateDir)
}

func countEvent(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}
