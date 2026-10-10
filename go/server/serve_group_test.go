//go:build unix

package server

import (
	"context"
	"errors"
	"testing"
)

// refreshRecorder captures the ctx key refresh was started under.
type refreshRecorder struct{ ctx context.Context }

func (r *refreshRecorder) Start(ctx context.Context) { r.ctx = ctx }

// A door that fails while the caller's ctx is still live must still stop key
// refresh: the refresh ctx ends when the serve group does, not when the caller does.
func TestServeGroupStopsKeyRefreshWhenServeReturns(t *testing.T) {
	rec := &refreshRecorder{}
	g, _ := newServeGroup(t.Context(), rec)
	if rec.ctx == nil {
		t.Fatal("key refresh was not started")
	}
	g.Go(func() error { return errors.New("door failed") })
	if err := g.Wait(); err == nil {
		t.Fatal("Wait = nil, want the door error")
	}
	if t.Context().Err() != nil {
		t.Fatal("test ctx ended; the case needs a live parent")
	}
	select {
	case <-rec.ctx.Done():
	default:
		t.Fatal("key refresh ctx still live after Serve's group returned")
	}
}

func TestServeGroupWithoutVerifierStartsNothing(t *testing.T) {
	g, _ := newServeGroup(t.Context(), nil)
	if err := g.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}
