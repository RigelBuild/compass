//go:build livegithub

package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scriptedReads replays one canned result per call, for the read-after-write gates.
type scriptedReads struct {
	errs   []error
	states []string
	calls  int
	cancel context.CancelFunc
}

func (s *scriptedReads) GetIssue(context.Context, string, uint64) (Issue, error) {
	err := s.next()
	if err != nil {
		if s.cancel != nil {
			s.cancel()
		}
		return Issue{}, err
	}

	issue := Issue{Number: 7}
	if s.calls <= len(s.states) {
		issue.State = s.states[s.calls-1]
	}
	return issue, nil
}

// ListIssues returns the target only once the script is exhausted; a nil
// scripted entry before that models the index still lagging.
func (s *scriptedReads) ListIssues(context.Context, string, IssueFilter) ([]Issue, error) {
	err := s.next()
	if s.calls <= len(s.errs) {
		if err != nil && s.cancel != nil {
			s.cancel()
		}
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
	errNotFound      = &StatusError{Status: http.StatusNotFound, Message: "no issue RIG-7"}
	errUpstream      = fmt.Errorf("forge: linear: %w", &StatusError{Status: http.StatusServiceUnavailable, Message: "upstream connect error"})
	errConnReset     = fmt.Errorf("do request: %w", syscall.ECONNRESET)
	errForbidden     = &StatusError{Status: http.StatusForbidden, Message: "forbidden"}
	errClientTimeout = fmt.Errorf("do request: %w", &net.DNSError{IsTimeout: true, Err: "timeout"})
	errDeadline      = fmt.Errorf("do request: %w", context.DeadlineExceeded)
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

func TestIsReadTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"client timeout", errClientTimeout, true},
		{"wrapped deadline", errDeadline, true},
		{"503", errUpstream, true},
		{"connection reset", errConnReset, true},
		{"404", errNotFound, false},
		{"403", errForbidden, false},
		{"500", &StatusError{Status: http.StatusInternalServerError}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadTransient(tc.err); got != tc.want {
				t.Errorf("isReadTransient(%v) = %v, want %v", tc.err, got, tc.want)
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
	t.Run("timeout then visible", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errClientTimeout}}
		if err := awaitIssueVisible(context.Background(), g, "RIG", 7, zeroDelays(2)); err != nil {
			t.Fatalf("awaitIssueVisible = %v, want nil", err)
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
	t.Run("transient error then canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := &scriptedReads{errs: []error{errClientTimeout}, cancel: cancel}
		err := awaitIssueVisible(ctx, g, "RIG", 7, zeroDelays(3))
		if !errors.Is(err, errClientTimeout) {
			t.Fatalf("awaitIssueVisible = %v, want last read error", err)
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
	t.Run("timeout then found", func(t *testing.T) {
		l := &scriptedReads{errs: []error{errClientTimeout}}
		row, _, err := findListedIssueWithBackoff(context.Background(), l, "RIG", IssueFilter{}, 7, zeroDelays(2))
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
	t.Run("transient error then canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		l := &scriptedReads{errs: []error{errClientTimeout}, cancel: cancel}
		_, _, err := findListedIssueWithBackoff(ctx, l, "RIG", IssueFilter{}, 7, zeroDelays(3))
		if !errors.Is(err, errClientTimeout) {
			t.Fatalf("findListedIssueWithBackoff = %v, want last read error", err)
		}
	})
}

func TestAwaitIssueState(t *testing.T) {
	t.Run("stale state then wanted", func(t *testing.T) {
		g := &scriptedReads{states: []string{stateClosed, stateOpen}}
		got, err := awaitIssueState(context.Background(), g, "RIG", 7, stateOpen, zeroDelays(2))
		if err != nil || got.State != stateOpen {
			t.Fatalf("awaitIssueState = %+v, %v; want state %q", got, err, stateOpen)
		}
		if g.calls != 2 {
			t.Errorf("calls = %d, want 2", g.calls)
		}
	})
	t.Run("never reaches wanted state", func(t *testing.T) {
		g := &scriptedReads{states: []string{stateClosed, stateClosed}}
		_, err := awaitIssueState(context.Background(), g, "RIG", 7, stateOpen, zeroDelays(2))
		if err == nil || !strings.Contains(err.Error(), "RIG-7") || !strings.Contains(err.Error(), stateClosed) || !strings.Contains(err.Error(), stateOpen) || !strings.Contains(err.Error(), "2 polls") {
			t.Fatalf("awaitIssueState error = %v, want repo-number, last state, target, and poll count", err)
		}
	})
	t.Run("forbidden stops after one call", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errForbidden}}
		_, err := awaitIssueState(context.Background(), g, "RIG", 7, stateOpen, zeroDelays(3))
		if !errors.Is(err, errForbidden) || g.calls != 1 {
			t.Fatalf("awaitIssueState = %v after %d calls, want the 403 after 1", err, g.calls)
		}
	})
	t.Run("not found and timeout tolerated", func(t *testing.T) {
		g := &scriptedReads{errs: []error{errNotFound, errClientTimeout}, states: []string{"", "", stateOpen}}
		got, err := awaitIssueState(context.Background(), g, "RIG", 7, stateOpen, zeroDelays(3))
		if err != nil || got.State != stateOpen {
			t.Fatalf("awaitIssueState = %+v, %v; want state %q", got, err, stateOpen)
		}
	})
	t.Run("transient error then canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := &scriptedReads{errs: []error{errClientTimeout}, cancel: cancel}
		_, err := awaitIssueState(ctx, g, "RIG", 7, stateOpen, zeroDelays(3))
		if !errors.Is(err, errClientTimeout) {
			t.Fatalf("awaitIssueState = %v, want last read error", err)
		}
	})
}

// A gateway error or reset can follow an accepted write, so a create must not
// re-issue on it: a second call would duplicate the artifact.
func TestCreateWithBackoffDoesNotRetryUpstreamTransient(t *testing.T) {
	for _, transient := range []error{errUpstream, errConnReset} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, err := createWithBackoff(ctx, func() (Issue, error) {
			calls++
			cancel() // a reintroduced retry returns ctx.Err() at once, not after 30s
			return Issue{}, transient
		})
		if calls != 1 || !errors.Is(err, transient) {
			t.Errorf("createWithBackoff(%v): %d calls, err %v; want 1 call and the original error", transient, calls, err)
		}
	}
}

// fakeTracker is an issue store whose creates commit and then may time out.
// visible[i] is how many committed issues the i-th find sees (list lag).
type fakeTracker struct {
	issues    []Issue
	timeouts  int
	visible   []int
	creates   int
	finds     int
	discarded []uint64
}

func (f *fakeTracker) find(context.Context) ([]Issue, error) {
	n := len(f.issues)
	if f.finds < len(f.visible) {
		n = f.visible[f.finds]
	}
	f.finds++
	return f.issues[:n], nil
}

func (f *fakeTracker) create() (Issue, error) {
	f.creates++
	issue := Issue{Number: uint64(len(f.issues) + 1)}
	f.issues = append(f.issues, issue)
	if f.creates <= f.timeouts {
		return Issue{}, errClientTimeout
	}
	return issue, nil
}

func (f *fakeTracker) run(t *testing.T) (Issue, error) {
	t.Helper()
	createRetryDelay = 0
	t.Cleanup(func() { createRetryDelay = 30 * time.Second })
	var got Issue
	var err error
	t.Run("leg", func(t *testing.T) {
		got, err = createIssueWithBackoff(context.Background(), t, f.find, f.create,
			func(n uint64) { f.discarded = append(f.discarded, n) })
	})
	return got, err
}

// A create that times out after the provider committed it must not leak: the
// re-issue is kept and only the committed first attempt is discarded.
func TestCreateIssueWithBackoffDiscardsCommittedTimeout(t *testing.T) {
	f := &fakeTracker{timeouts: 1}
	got, err := f.run(t)
	if err != nil || got.Number != 2 {
		t.Fatalf("createIssueWithBackoff = %+v, %v; want issue 2", got, err)
	}
	if f.creates != 2 || len(f.discarded) != 1 || f.discarded[0] != 1 {
		t.Fatalf("creates=%d discarded=%v; want 2 creates and only issue 1 discarded", f.creates, f.discarded)
	}
}

// Both attempts can time out after committing, and list lag can reveal them one
// poll apart; the sweep keeps polling until both are discarded.
func TestCreateIssueWithBackoffDiscardsBothUnderLag(t *testing.T) {
	saved := listLagDelays
	listLagDelays = zeroDelays(4)
	t.Cleanup(func() { listLagDelays = saved })
	f := &fakeTracker{timeouts: 2, visible: []int{0, 1, 1}}
	if _, err := f.run(t); !errors.Is(err, errClientTimeout) {
		t.Fatalf("err = %v; want the second timeout", err)
	}
	if len(f.discarded) != 2 || f.discarded[0] != 1 || f.discarded[1] != 2 {
		t.Fatalf("discarded=%v after %d finds; want [1 2] once each", f.discarded, f.finds)
	}
}

// A clean create pays no extra list call.
func TestCreateIssueWithBackoffNoTimeoutNoSweep(t *testing.T) {
	f := &fakeTracker{}
	if _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if f.finds != 0 || len(f.discarded) != 0 {
		t.Fatalf("finds=%d discarded=%v; want no sweep", f.finds, f.discarded)
	}
}
