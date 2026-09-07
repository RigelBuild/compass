package ingest

// Unit acceptance for the head_sha->PR-number resolution step (RIG-2869): the
// router's step 0 (a check_suite webhook carries a head SHA but no artifact
// number) and the TTL cache in front of it. context.Background() here is the
// test root — the sanctioned F-ttsr exemption (mirrors notify_router_test.go).

import (
	"context"
	"errors"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
)

// checkSuiteEvent is the shape parseGitHubCheckSuite produces: a CHECKS event
// carrying a head SHA and NO artifact number.
func checkSuiteEvent(headSHA string) forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com", Repo: "o/r", Kind: kindPR, Number: 0,
		URL: "https://gh/o/r/pull/7", Change: chChecks, HeadSHA: headSHA,
	}
}

// TestRouteResolvesPullNumberForCheckSuite tables the step-0 arms: which events
// consult the resolver, and what each resolution outcome does to the route.
func TestRouteResolvesPullNumberForCheckSuite(t *testing.T) {
	tests := []struct {
		name string
		ev   forge.ForgeEvent
		// pulls nil means NO resolver wired (the pre-RIG-2869 shape).
		pulls *fakePullNumbers

		wantErr      bool
		wantCalls    int
		wantSHA      string
		wantDispatch int
		wantNumber   uint64
	}{
		{
			name:         "check_suite resolves and routes",
			ev:           checkSuiteEvent("abc123"),
			pulls:        &fakePullNumbers{number: 77},
			wantCalls:    1,
			wantSHA:      "abc123",
			wantDispatch: 1,
			wantNumber:   77,
		},
		{
			name:      "no pull request for the sha fails closed",
			ev:        checkSuiteEvent("orphan"),
			pulls:     &fakePullNumbers{err: forge.ErrNoPullRequestForSHA},
			wantErr:   true,
			wantCalls: 1,
			wantSHA:   "orphan",
		},
		{
			name:      "resolver infrastructure error propagates",
			ev:        checkSuiteEvent("abc123"),
			pulls:     &fakePullNumbers{err: errors.New("boom: 502 from the forge")},
			wantErr:   true,
			wantCalls: 1,
			wantSHA:   "abc123",
		},
		{
			name:    "nil resolver keeps the legacy zero-number rejection",
			ev:      checkSuiteEvent("abc123"),
			pulls:   nil,
			wantErr: true,
		},
		{
			name: "non-CHECKS zero-number event is rejected without resolving",
			ev: forge.ForgeEvent{
				Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
				Host:     "github.com", Repo: "o/r", Kind: kindIssue, Number: 0,
				URL: "u", Change: chComment, HeadSHA: "abc123",
				Comment: ghComment("u", "hi", "octocat"),
			},
			pulls:     &fakePullNumbers{number: 77},
			wantErr:   true,
			wantCalls: 0,
		},
		{
			name:      "CHECKS with no head sha is rejected without resolving",
			ev:        checkSuiteEvent(""),
			pulls:     &fakePullNumbers{number: 77},
			wantErr:   true,
			wantCalls: 0,
		},
		{
			name: "CHECKS that already carries a number never resolves",
			ev: forge.ForgeEvent{
				Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
				Host:     "github.com", Repo: "o/r", Kind: kindPR, Number: 12,
				URL: "u", Change: chChecks, HeadSHA: "abc123",
			},
			pulls:        &fakePullNumbers{number: 77},
			wantCalls:    0,
			wantDispatch: 1,
			wantNumber:   12,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "sub-1", AgentAccountID: "acct-1"}}}
			d := &fakeDispatcher{}
			var resolver PullNumberResolver
			if tc.pulls != nil {
				resolver = tc.pulls
			}
			r := newRouterWithPulls(t, st, d, &fakeChecksRoller{}, resolver)

			err := r.Route(context.Background(), tc.ev)
			if tc.wantErr && err == nil {
				t.Fatal("Route: nil error, want the route to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Route: %v", err)
			}
			if tc.pulls != nil && tc.pulls.calls != tc.wantCalls {
				t.Errorf("resolver calls = %d, want %d", tc.pulls.calls, tc.wantCalls)
			}
			if tc.wantSHA != "" && tc.pulls.lastSHA != tc.wantSHA {
				t.Errorf("resolved sha = %q, want %q", tc.pulls.lastSHA, tc.wantSHA)
			}
			if len(d.sent) != tc.wantDispatch {
				t.Fatalf("dispatched %d, want %d", len(d.sent), tc.wantDispatch)
			}
			if tc.wantDispatch > 0 && d.sent[0].GetNumber() != tc.wantNumber {
				t.Errorf("notified Number = %d, want %d", d.sent[0].GetNumber(), tc.wantNumber)
			}
		})
	}
}

// TestRouteResolvedNumberKeysEveryLaterStep: the resolved number is not just
// stamped on the notification — it is the coordinate the roll-up read and the
// cursor upsert key on. A number that reached only the wire would leave the
// snapshot filed under #0.
func TestRouteResolvedNumberKeysEveryLaterStep(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "sub-1", AgentAccountID: "acct-1"}}}
	d := &fakeDispatcher{}
	roller := &fakeChecksRoller{res: forge.ConditionalResult[forge.Checks]{
		V:    forge.Checks{HeadSHA: "abc123", State: "success"},
		ETag: `"ck1"`,
	}}
	r := newRouterWithPulls(t, st, d, roller, &fakePullNumbers{number: 77})

	if err := r.Route(context.Background(), checkSuiteEvent("abc123")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if roller.calls != 1 || roller.lastNum != 77 || roller.lastHead != "abc123" {
		t.Errorf("roll-up called %d times with #%d@%s, want 1 with #77@abc123",
			roller.calls, roller.lastNum, roller.lastHead)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("cursor upserts = %d, want 1", len(st.upserts))
	}
	if got := st.upserts[0].Number; got != 77 {
		t.Errorf("cursor filed under #%d, want the resolved #77", got)
	}
}

// ---- the TTL cache ----

// countingResolver counts underlying resolutions and scripts a per-SHA answer,
// so a cache test can prove a hit cost NO underlying call.
type countingResolver struct {
	bySHA map[string]uint64
	err   error
	calls int
}

func (c *countingResolver) PullNumberForSHA(_ context.Context, _, headSHA string) (uint64, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	num, ok := c.bySHA[headSHA]
	if !ok {
		return 0, forge.ErrNoPullRequestForSHA
	}
	return num, nil
}

// fakeClock is an injectable now func whose time only moves when a test moves
// it — the cachedWebhookSecret pattern. No sleeps.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestCachedPullNumberResolver(t *testing.T) {
	const ttl = time.Minute
	base := &countingResolver{bySHA: map[string]uint64{"abc123": 77, "def456": 88}}
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	c := newCachedPullNumberResolver(base, ttl, clk.now)
	ctx := context.Background()

	// Two resolutions inside the TTL cost ONE underlying call.
	for i := range 2 {
		num, err := c.PullNumberForSHA(ctx, "o/r", "abc123")
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if num != 77 {
			t.Errorf("resolve %d = %d, want 77", i, num)
		}
	}
	if base.calls != 1 {
		t.Fatalf("underlying calls = %d, want 1 (the second resolve is a cache hit)", base.calls)
	}

	// A different SHA is a different key: its own call, and it does not evict.
	if num, err := c.PullNumberForSHA(ctx, "o/r", "def456"); err != nil || num != 88 {
		t.Fatalf("resolve(def456) = %d, %v, want 88, nil", num, err)
	}
	if base.calls != 2 {
		t.Fatalf("underlying calls = %d, want 2 (a distinct SHA is a distinct key)", base.calls)
	}
	if _, err := c.PullNumberForSHA(ctx, "o/r", "abc123"); err != nil {
		t.Fatalf("resolve(abc123) after a sibling key: %v", err)
	}
	if base.calls != 2 {
		t.Errorf("underlying calls = %d, want 2 (abc123 still cached)", base.calls)
	}

	// Past the TTL the entry is stale: the next resolve calls through again.
	clk.add(ttl)
	if num, err := c.PullNumberForSHA(ctx, "o/r", "abc123"); err != nil || num != 77 {
		t.Fatalf("resolve after expiry = %d, %v, want 77, nil", num, err)
	}
	if base.calls != 3 {
		t.Errorf("underlying calls = %d, want 3 (the clock passed the TTL)", base.calls)
	}
}

// TestCachedPullNumberResolverCachesSentinel: the no-PR-for-this-SHA answer is
// cached too — a push to a PR-less branch fires a check_suite per installed App,
// so it is exactly where the duplicate-call burst is worst.
func TestCachedPullNumberResolverCachesSentinel(t *testing.T) {
	base := &countingResolver{bySHA: map[string]uint64{}} // every SHA -> sentinel
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	c := newCachedPullNumberResolver(base, time.Minute, clk.now)
	ctx := context.Background()

	for i := range 2 {
		num, err := c.PullNumberForSHA(ctx, "o/r", "orphan")
		if !errors.Is(err, forge.ErrNoPullRequestForSHA) {
			t.Fatalf("resolve %d err = %v, want ErrNoPullRequestForSHA replayed from the cache", i, err)
		}
		if num != 0 {
			t.Errorf("resolve %d = %d, want 0 alongside the sentinel", i, num)
		}
	}
	if base.calls != 1 {
		t.Errorf("underlying calls = %d, want 1 (the sentinel is cached)", base.calls)
	}
}

// TestCachedPullNumberResolverNeverCachesInfraError: an infrastructure fault is
// returned, never cached, so a rate-limited or 5xx resolve retries on the next
// event instead of being pinned for a whole TTL.
func TestCachedPullNumberResolverNeverCachesInfraError(t *testing.T) {
	boom := errors.New("502 bad gateway")
	base := &countingResolver{err: boom}
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	c := newCachedPullNumberResolver(base, time.Minute, clk.now)
	ctx := context.Background()

	for i := range 2 {
		if _, err := c.PullNumberForSHA(ctx, "o/r", "abc123"); !errors.Is(err, boom) {
			t.Fatalf("resolve %d err = %v, want the infrastructure error", i, err)
		}
	}
	if base.calls != 2 {
		t.Errorf("underlying calls = %d, want 2 (an infra error is never cached)", base.calls)
	}
}

// TestNewCachedPullNumberResolverNilBase: a nil base stays nil rather than
// becoming a cache over nothing, so a lane with no resolver reaches the router
// as the nil the router tolerates.
//
// The assertion goes through the SEAM, not the concrete type, and that is the
// whole point. A constructor returning a concrete *CachedPullNumberResolver
// would satisfy `got != nil` while producing a TYPED NIL once assigned into a
// PullNumberResolver: the interface value is non-nil, so Route's nil check
// passes and the first call dereferences a nil receiver, panicking the notify
// drain goroutine — which has no recover, so the whole lane dies. Comparing the
// concrete return would pass either way and prove nothing.
func TestNewCachedPullNumberResolverNilBase(t *testing.T) {
	// Passed through a func taking the interface, so the value genuinely crosses
	// the seam boundary. An inline `var x PullNumberResolver = ...` would say
	// the same thing, but staticcheck (ST1023) strips the annotation as
	// inferable — and inferring the CONCRETE type is exactly the mistake this
	// test exists to catch.
	assertNilSeam := func(t *testing.T, seam PullNumberResolver) {
		t.Helper()
		if seam != nil {
			t.Fatalf("NewCachedPullNumberResolver(nil) as a PullNumberResolver = %v, want a true nil", seam)
		}
	}
	assertNilSeam(t, NewCachedPullNumberResolver(nil))

	// The contract that matters: a router handed that value takes the untouched
	// pre-resolution path instead of panicking.
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}}}
	d := &fakeDispatcher{}
	r := newRouterWithPulls(t, st, d, &fakeChecksRoller{}, NewCachedPullNumberResolver(nil))
	if err := r.Route(context.Background(), checkSuiteEvent("abc123")); err == nil {
		t.Error("Route with a nil resolver = nil error, want the zero-number guard to reject")
	}
	if len(d.sent) != 0 {
		t.Errorf("dispatched %d, want 0", len(d.sent))
	}
}

// TestCachedResolverDrivesRouteStep0: the cache and the router compose — two
// check_suite events for the SAME head (the one-per-installed-App burst) both
// route, and cost ONE underlying resolution.
func TestCachedResolverDrivesRouteStep0(t *testing.T) {
	base := &countingResolver{bySHA: map[string]uint64{"abc123": 77}}
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	cache := newCachedPullNumberResolver(base, time.Minute, clk.now)

	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "sub-1", AgentAccountID: "acct-1"}}}
	d := &fakeDispatcher{}
	r := newRouterWithPulls(t, st, d, &fakeChecksRoller{}, cache)

	for i := range 2 {
		if err := r.Route(context.Background(), checkSuiteEvent("abc123")); err != nil {
			t.Fatalf("Route %d: %v", i, err)
		}
	}
	if base.calls != 1 {
		t.Errorf("underlying resolutions = %d, want 1 for two same-head check_suites", base.calls)
	}
	if len(d.sent) != 2 {
		t.Fatalf("dispatched %d, want 2 (both events route)", len(d.sent))
	}
	for i, n := range d.sent {
		if n.GetNumber() != 77 {
			t.Errorf("notification %d Number = %d, want 77", i, n.GetNumber())
		}
	}
}

// TestCachedPullNumberResolverEvictsExpired pins the memory bound the TTL alone
// does not give. The cache is keyed on a head SHA, so its key space is every
// commit ever pushed; a lookup that only IGNORED an expired entry would leave
// one resident entry per commit in a process that runs for weeks. Two evictions
// are asserted separately because they are separate mechanisms: the read path
// drops the key it looks up, and a store sweeps the keys nothing looks up again
// (the leak that matters, since a coordinate is usually seen once).
func TestCachedPullNumberResolverEvictsExpired(t *testing.T) {
	const ttl = time.Minute
	base := &countingResolver{bySHA: map[string]uint64{"aaa": 1, "bbb": 2, "ccc": 3}}
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	c := newCachedPullNumberResolver(base, ttl, clk.now)
	ctx := context.Background()

	// Two coordinates seen once each, then never again.
	for _, sha := range []string{"aaa", "bbb"} {
		if _, err := c.PullNumberForSHA(ctx, "octo/repo", sha); err != nil {
			t.Fatalf("resolve %s: %v", sha, err)
		}
	}
	if got := c.size(); got != 2 {
		t.Fatalf("resident = %d, want 2 before expiry", got)
	}

	// Past the TTL those two are dead weight: nothing will look them up again.
	// A third, unrelated resolve must not let them stay resident.
	clk.add(ttl + time.Second)
	if _, err := c.PullNumberForSHA(ctx, "octo/repo", "ccc"); err != nil {
		t.Fatalf("resolve ccc: %v", err)
	}
	if got := c.size(); got != 1 {
		t.Errorf("resident = %d, want 1 (a store sweeps the expired keys nothing re-reads)", got)
	}

	// The read path evicts the key it looks up even when no store follows. Cache
	// a sentinel, expire it, then make the re-resolve fail: an infrastructure
	// fault stores nothing, so the only thing that can remove the dead key is
	// the read itself. ccc stays resident throughout — its own TTL restarted
	// when it was stored, which is why this asserts a delta, not an empty map.
	if _, err := c.PullNumberForSHA(ctx, "octo/repo", "zzz"); !errors.Is(err, forge.ErrNoPullRequestForSHA) {
		t.Fatalf("resolve zzz = %v, want the no-PR sentinel", err)
	}
	if got := c.size(); got != 2 {
		t.Fatalf("resident = %d, want 2 (ccc + the zzz sentinel)", got)
	}
	clk.add(ttl + time.Second)
	base.err = context.Canceled // an infrastructure fault is never cached
	if _, err := c.PullNumberForSHA(ctx, "octo/repo", "zzz"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve zzz after expiry = %v, want the infrastructure error", err)
	}
	if got := c.size(); got != 1 {
		t.Errorf("resident = %d, want 1: the read evicted the expired zzz and the failed resolve stored nothing, leaving only ccc", got)
	}
}

// slowResolver answers a scripted sequence, so a test can interleave two
// resolves for one key and control which one observed the world later.
type slowResolver struct {
	answers []func() (uint64, error)
	calls   int
}

func (s *slowResolver) PullNumberForSHA(_ context.Context, _, _ string) (uint64, error) {
	i := s.calls
	s.calls++
	if i >= len(s.answers) {
		return 0, forge.ErrNoPullRequestForSHA
	}
	return s.answers[i]()
}

// TestCachedPullNumberResolverLastObservedWins pins that the cache is ordered by
// what each resolve SAW, not by which one returned last.
//
// The burst this cache exists for makes the race routine: one push fires a
// check_suite per installed App, so two resolves for the same head overlap. If
// the slow one started before the PR was opened and the fast one found it, the
// slow one landing last would pin "no PR" for a full TTL and fail every
// check_suite for that head until it expired — the cache actively serving a
// wrong answer over a right one it already held.
func TestCachedPullNumberResolverLastObservedWins(t *testing.T) {
	const ttl = 10 * time.Minute
	clk := &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	base := &slowResolver{}
	c := newCachedPullNumberResolver(base, ttl, clk.now)
	ctx := context.Background()

	// The FAST resolve observes the world at 12:00 and finds PR 77.
	base.answers = []func() (uint64, error){func() (uint64, error) { return 77, nil }}
	if num, err := c.PullNumberForSHA(ctx, "octo/repo", "abc123"); err != nil || num != 77 {
		t.Fatalf("fresh resolve = (%d, %v), want (77, nil)", num, err)
	}

	// The SLOW resolve began EARLIER (at 11:59, before the PR existed) and only
	// now returns the sentinel. Storing it would overwrite the fresher answer.
	stale := pullNumberEntry{
		noPull:     true,
		expires:    clk.t.Add(ttl),
		observedAt: clk.t.Add(-time.Minute),
	}
	c.store(pullNumberKey{repo: "octo/repo", sha: "abc123"}, stale)

	num, err := c.PullNumberForSHA(ctx, "octo/repo", "abc123")
	if err != nil {
		t.Fatalf("after the stale store = %v, want the fresher 77 to survive", err)
	}
	if num != 77 {
		t.Errorf("number = %d, want 77 (a resolve that observed an EARLIER world must not overwrite a later one)", num)
	}
	if base.calls != 1 {
		t.Errorf("underlying calls = %d, want 1 (the surviving entry is still a cache hit)", base.calls)
	}

	// A LATER observation still wins — the guard orders writes, it does not
	// freeze the entry.
	clk.add(time.Minute)
	fresher := pullNumberEntry{number: 88, expires: clk.now().Add(ttl), observedAt: clk.now()}
	c.store(pullNumberKey{repo: "octo/repo", sha: "abc123"}, fresher)
	if num, err := c.PullNumberForSHA(ctx, "octo/repo", "abc123"); err != nil || num != 88 {
		t.Errorf("after a later observation = (%d, %v), want (88, nil)", num, err)
	}
}
