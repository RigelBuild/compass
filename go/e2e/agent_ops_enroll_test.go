//go:build podman

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestClassifyEnrollProbe pins the pure classification of a StopAgentSession
// enrollment probe result into (ready, retry, cerr) without a live client.
func TestClassifyEnrollProbe(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantReady bool
		wantRetry bool
		wantErr   bool
	}{
		{
			name:      "nil error means enrolled",
			err:       nil,
			wantReady: true,
			wantRetry: false,
			wantErr:   false,
		},
		{
			name:      "unavailable no runner enrolled retries",
			err:       connect.NewError(connect.CodeUnavailable, "unavailable: no runner enrolled to serve session"),
			wantReady: false,
			wantRetry: true,
			wantErr:   false,
		},
		{
			name:      "unavailable sessions stream not attached retries",
			err:       connect.NewError(connect.CodeUnavailable, `no live runner sessions stream for command "x"`),
			wantReady: false,
			wantRetry: true,
			wantErr:   false,
		},
		{
			name:      "internal error is surfaced",
			err:       connect.NewError(connect.CodeInternal, "boom"),
			wantReady: false,
			wantRetry: false,
			wantErr:   true,
		},
		{
			name:      "unavailable with other message is surfaced not retried",
			err:       connect.NewError(connect.CodeUnavailable, "some other unavailable"),
			wantReady: false,
			wantRetry: false,
			wantErr:   true,
		},
		{
			name:      "deadline exceeded is surfaced not retried",
			err:       context.DeadlineExceeded,
			wantReady: false,
			wantRetry: false,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, retry, cerr := classifyEnrollProbe(tt.err)
			if ready != tt.wantReady {
				t.Errorf("ready = %v, want %v", ready, tt.wantReady)
			}
			if retry != tt.wantRetry {
				t.Errorf("retry = %v, want %v", retry, tt.wantRetry)
			}
			if tt.wantErr {
				if cerr == nil {
					t.Fatalf("cerr = nil, want non-nil")
				}
				if !errors.Is(cerr, tt.err) {
					t.Errorf("cerr = %v, want to wrap %v", cerr, tt.err)
				}
				if !strings.Contains(cerr.Error(), "runner enrollment probe (StopAgentSession)") {
					t.Errorf("cerr = %q, want probe prefix", cerr.Error())
				}
			} else if cerr != nil {
				t.Errorf("cerr = %v, want nil", cerr)
			}
		})
	}
}

// TestWaitRunnerEnrolledBudgetTimeout drives the budget-timeout branch of
// waitRunnerEnrolled through the injectable clock seam: the fake clock jumps
// past the deadline on the loop-top check, so the poll returns the clean
// budget-exhausted error without ever firing a live probe (a bare Fixture with
// only now set — Compass() is never reached).
func TestWaitRunnerEnrolledBudgetTimeout(t *testing.T) {
	base := time.Now()
	calls := 0
	f := &Fixture{
		now: func() time.Time {
			calls++
			if calls == 1 {
				return base // deadline = base + enrollPollBudget
			}
			return base.Add(enrollPollBudget + time.Second) // past the deadline
		},
	}
	err := f.waitRunnerEnrolled(context.Background())
	if err == nil {
		t.Fatal("waitRunnerEnrolled() = nil, want budget-timeout error")
	}
	if !strings.Contains(err.Error(), "did not attach within") {
		t.Errorf("err = %q, want budget-timeout message", err.Error())
	}
}

// TestClassifySeedSettle pins the pure not-yet-vs-real-error branching of the
// seed-settle probe (RIG-2403) without a live store. The two inputs are the
// ordered store outcomes: handleErr from resolving the supervisor by handle, and
// placementErr from reading its placement (consulted only when handleErr is nil).
// A store.ErrNotFound in EITHER is a "still seeding" not-yet (settled=false, no
// error, poll on); any other error surfaces wrapped (abort); both nil means the
// placement resolved (settled=true). A regression that mapped the placement
// ErrNotFound to a hard error would abort the gate mid-seed and redden the
// "unplaced is not-yet" case; one that swallowed a real error would hang the poll
// to the budget and redden the "real error surfaces" cases.
func TestClassifySeedSettle(t *testing.T) {
	realErr := errors.New("connection refused")
	tests := []struct {
		name         string
		handleErr    error
		placementErr error
		wantSettled  bool
		wantErr      bool
	}{
		{
			name:        "both nil is settled",
			wantSettled: true,
		},
		{
			name:        "handle not-found is not-yet",
			handleErr:   store.ErrNotFound,
			wantSettled: false,
		},
		{
			name:         "placement not-found is not-yet",
			placementErr: store.ErrNotFound,
			wantSettled:  false,
		},
		{
			name:        "real handle error surfaces",
			handleErr:   realErr,
			wantSettled: false,
			wantErr:     true,
		},
		{
			name:         "real placement error surfaces",
			placementErr: realErr,
			wantSettled:  false,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settled, cerr := classifySeedSettle(tt.handleErr, tt.placementErr)
			if settled != tt.wantSettled {
				t.Errorf("settled = %v, want %v", settled, tt.wantSettled)
			}
			if tt.wantErr {
				if cerr == nil {
					t.Fatalf("cerr = nil, want non-nil")
				}
				if !errors.Is(cerr, realErr) {
					t.Errorf("cerr = %v, want to wrap %v", cerr, realErr)
				}
			} else if cerr != nil {
				t.Errorf("cerr = %v, want nil", cerr)
			}
		})
	}
}

// enrollProbeClient fails every StopAgentSession with the retryable no-runner
// error and records the probe ctx deadline; other methods are unused.
type enrollProbeClient struct {
	compassServiceClient
	deadline time.Time
	hasDL    bool
}

func (c *enrollProbeClient) StopAgentSession(ctx context.Context, _ *compassv1.StopAgentSessionRequest) (*compassv1.StopAgentSessionResponse, error) {
	c.deadline, c.hasDL = ctx.Deadline()
	return nil, connect.NewError(connect.CodeUnavailable, runnerhub.ErrMsgNoRunnerEnrolled+" to serve session")
}

// TestRunnerEnrolledProbeBound pins each probe's timeout to
// min(rpcTimeout, deadline-now), clamped to 1ms once the budget is spent.
func TestRunnerEnrolledProbeBound(t *testing.T) {
	base := time.Now()
	budgetEnd := base.Add(rpcTimeout + 10*time.Second)
	var offset time.Duration
	client := &enrollProbeClient{}
	f := &Fixture{compass: client, now: func() time.Time { return base.Add(offset) }}
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		deadline time.Time
		want     time.Duration
	}{
		{"no deadline uses rpcTimeout", 0, time.Time{}, rpcTimeout},
		{"remaining above rpcTimeout uses rpcTimeout", 0, budgetEnd, rpcTimeout},
		{"remaining equal to rpcTimeout", 10 * time.Second, budgetEnd, rpcTimeout},
		{"remaining below rpcTimeout uses remaining", 30 * time.Second, budgetEnd, 10 * time.Second},
		{"nearly spent uses remaining", rpcTimeout + 10*time.Second - 3*time.Millisecond, budgetEnd, 3 * time.Millisecond},
		{"spent clamps to 1ms", rpcTimeout + 10*time.Second, budgetEnd, time.Millisecond},
		{"overrun clamps to 1ms", rpcTimeout + 20*time.Second, budgetEnd, time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset = tc.offset
			*client = enrollProbeClient{}
			before := time.Now()
			ready, err := f.runnerEnrolledProbe(t.Context(), tc.deadline)
			after := time.Now()
			if ready || err != nil {
				t.Fatalf("runnerEnrolledProbe() = (%v, %v), want (false, nil) on a retryable no-runner error", ready, err)
			}
			if !client.hasDL {
				t.Fatal("probe ctx has no deadline")
			}
			// WithTimeout reads the real clock, so the timeout lies in [before, after] + want.
			if lo, hi := before.Add(tc.want), after.Add(tc.want); client.deadline.Before(lo) || client.deadline.After(hi) {
				t.Errorf("probe deadline %s after call start, want %s", client.deadline.Sub(before), tc.want)
			}
		})
	}
}
