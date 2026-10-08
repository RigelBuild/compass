//go:build livegithub

package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// scriptedReads replays one canned result per call, for the read-after-write gates.
type scriptedReads struct {
	errs  []error
	calls int
}

func (s *scriptedReads) GetIssue(context.Context, string, uint64) (Issue, error) {
	if err := s.next(); err != nil {
		return Issue{}, err
	}
	return Issue{Number: 7}, nil
}

// ListIssues returns the target only once the script is exhausted; a nil
// scripted entry before that models the index still lagging.
func (s *scriptedReads) ListIssues(context.Context, string, IssueFilter) ([]Issue, error) {
	err := s.next()
	if s.calls <= len(s.errs) {
		return []Issue{{Number: 1}}, err
	}
	return []Issue{{Number: 1}, {Number: 7}}, nil
}

func (s *scriptedReads) next() error {
	s.calls++
	if s.calls > len(s.errs) {
		return nil
	}
	return s.errs[s.calls-1]
}

func zeroDelays(n int) []time.Duration { return make([]time.Duration, n) }

var (
	errNotFound  = &StatusError{Status: http.StatusNotFound, Message: "no issue RIG-7"}
	errUpstream  = fmt.Errorf("forge: linear: %w", &StatusError{Status: http.StatusServiceUnavailable, Message: "upstream connect error"})
	errConnReset = fmt.Errorf("do request: %w", syscall.ECONNRESET)
	errForbidden = &StatusError{Status: http.StatusForbidden, Message: "forbidden"}
)

func TestIsUpstreamTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped 503", errUpstream, true},
		{"502", &StatusError{Status: http.StatusBadGateway}, true},
		{"504", &StatusError{Status: http.StatusGatewayTimeout}, true},
		{"connection reset", errConnReset, true},
		{"500 is a server bug, not a gateway blip", &StatusError{Status: http.StatusInternalServerError}, false},
		{"404", errNotFound, false},
		{"403", errForbidden, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUpstreamTransient(tc.err); got != tc.want {
				t.Errorf("isUpstreamTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAwaitIssueVisible(t *testing.T) {
	t.Run("lag then visible", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errNotFound, errUpstream, errNotFound}}
		if err := awaitIssueVisible(context.Background(), g, "RIG", 7, zeroDelays(4)); err != nil {
			t.Fatalf("awaitIssueVisible = %v, want nil", err)
		}
		if g.calls != 4 {
			t.Errorf("calls = %d, want 4", g.calls)
		}
	})
	t.Run("never visible fails loud", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errNotFound, errNotFound, errNotFound}}
		err := awaitIssueVisible(context.Background(), g, "RIG", 7, zeroDelays(3))
		if !isNotFound(err) {
			t.Fatalf("awaitIssueVisible = %v, want the last 404 wrapped", err)
		}
	})
	t.Run("non-transient error stops at once", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errForbidden}}
		err := awaitIssueVisible(context.Background(), g, "RIG", 7, zeroDelays(3))
		if !errors.Is(err, errForbidden) || g.calls != 1 {
			t.Fatalf("awaitIssueVisible = %v after %d calls, want the 403 after 1", err, g.calls)
		}
	})
	t.Run("canceled ctx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := awaitIssueVisible(ctx, &scriptedReads{}, "RIG", 7, zeroDelays(1))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("awaitIssueVisible = %v, want context.Canceled", err)
		}
	})
}

func TestFindListedIssueWithBackoff(t *testing.T) {
	t.Run("upstream blip and lag then found", func(t *testing.T) {
		l := &scriptedReads{errs: []error{errUpstream, nil, errConnReset}}
		row, _, err := findListedIssueWithBackoff(context.Background(), l, "RIG", IssueFilter{}, 7, zeroDelays(4))
		if err != nil || row.Number != 7 {
			t.Fatalf("found row %d err %v, want row 7 and nil", row.Number, err)
		}
	})
	t.Run("never listed returns no row and no error", func(t *testing.T) {
		l := &scriptedReads{errs: []error{nil, nil, nil}}
		row, n, err := findListedIssueWithBackoff(context.Background(), l, "RIG", IssueFilter{}, 7, zeroDelays(3))
		if err != nil || row.Number != 0 || n != 1 {
			t.Fatalf("got row %d, %d rows, err %v; want no row, 1 row seen, nil", row.Number, n, err)
		}
	})
	t.Run("transient outlasting the bound is returned", func(t *testing.T) {
		l := &scriptedReads{errs: []error{errUpstream, errUpstream}}
		_, _, err := findListedIssueWithBackoff(context.Background(), l, "RIG", IssueFilter{}, 7, zeroDelays(2))
		if !isUpstreamTransient(err) {
			t.Fatalf("err = %v, want the last upstream transient", err)
		}
	})
	t.Run("non-transient error stops at once", func(t *testing.T) {
		l := &scriptedReads{errs: []error{errForbidden}}
		_, _, err := findListedIssueWithBackoff(context.Background(), l, "RIG", IssueFilter{}, 7, zeroDelays(3))
		if !errors.Is(err, errForbidden) || l.calls != 1 {
			t.Fatalf("err = %v after %d calls, want the 403 after 1", err, l.calls)
		}
	})
}
