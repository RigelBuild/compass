package ingest

// The head_sha->PR-number TTL cache (RIG-2869), the hot-path guard on Route's
// step 0. It lives next to the PullNumberResolver seam it both consumes and
// satisfies (a decorator, not a lane detail), so go/server's wiring is one
// constructor call and the cache's contract is unit-tested against the same
// seam the router drives.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// defaultPullNumberTTL is how long a resolved (repo, head sha) -> PR number
// mapping is held. It is generous because the mapping is near-immutable: a
// commit's association to a PR changes only when a PR is opened for, or
// re-targeted onto, an already-pushed head. The TTL exists to bound staleness
// for exactly those cases, not to track a moving value.
const defaultPullNumberTTL = 10 * time.Minute

// CachedPullNumberResolver is a TTL cache in front of a PullNumberResolver,
// keyed on (repo, head sha) — the shape of the traffic it defends against. ONE
// push produces one check_suite per installed GitHub App, so a repo with several
// CI Apps routes several CHECKS events carrying the SAME head SHA within
// seconds; uncached, each pays its own commits/{sha}/pulls GET against the one
// shared App rate budget (the gate the reconciler's ErrBudgetExhausted rides).
// The cache collapses that burst to one call.
//
// It mirrors cachedWebhookSecret (go/server/serve.go): a mutex, a ttl, and an
// INJECTABLE now func so a test drives expiry by clock, never by sleeping.
//
// BOTH outcomes are cached — a resolved number AND forge.ErrNoPullRequestForSHA.
// Caching the sentinel is the point rather than a concession: a push to a branch
// with no PR (or straight to a default branch) still fires a check_suite per
// App, so the PR-less case is exactly where the duplicate-call burst is most
// wasteful and least useful. The staleness it admits — a PR opened for an
// already-pushed head is not routable for up to one TTL — is bounded and healed:
// the reconcile sweep re-notifies any subscriber left behind on the durable gap
// once the PR exists as a target. An INFRASTRUCTURE error is never cached
// (mirroring cachedWebhookSecret's resolve fault), so a rate-limited or 5xx
// resolve retries on the next event instead of being pinned for a TTL.
//
// UNLIKE cachedWebhookSecret, which holds ONE value, this cache is a map keyed
// on a head SHA — an unbounded value space in a process that runs for weeks. A
// lookup that merely ignores an expired entry would leak one entry per commit
// ever pushed, so an expired entry is DELETED on the read that finds it, and a
// store sweeps the other expired keys. The live set is therefore bounded by the
// coordinates actually seen within one TTL, not by the repo's whole history.
type CachedPullNumberResolver struct {
	base PullNumberResolver
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[pullNumberKey]pullNumberEntry
}

// pullNumberKey is one cached coordinate: the repo and the head SHA. The repo is
// part of the key because a SHA is only unique within a repository.
type pullNumberKey struct {
	repo string
	sha  string
}

// pullNumberEntry is one cached outcome: either a resolved number, or the
// no-PR-for-this-SHA answer (number 0, noPull true). It never holds an
// infrastructure error.
//
// observedAt is when the resolve that produced this entry STARTED, not when it
// finished. Two resolves for one key overlap whenever a burst misses (the cache
// resolves outside the lock on purpose), and the useful answer is the one that
// looked at the world most recently — not the one that happened to return last.
// A slow resolve that began before a PR existed must never overwrite a fast
// later resolve that found it.
type pullNumberEntry struct {
	number     uint64
	noPull     bool
	expires    time.Time
	observedAt time.Time
}

// NewCachedPullNumberResolver wraps base in a TTL cache over the default TTL and
// the wall clock. A nil base yields a nil RESOLVER, so a lane with no resolver
// stays one the router tolerates rather than becoming a cache over nothing.
//
// It returns the INTERFACE, not the concrete type, and that is load-bearing: a
// nil *CachedPullNumberResolver assigned into a PullNumberResolver would be a
// typed nil — an interface value that is NOT nil, so the router's nil check
// would pass and the first call would dereference a nil receiver. Returning the
// interface makes the nil a true nil at every callsite.
func NewCachedPullNumberResolver(base PullNumberResolver) PullNumberResolver {
	if base == nil {
		return nil
	}
	return newCachedPullNumberResolver(base, defaultPullNumberTTL, time.Now)
}

// newCachedPullNumberResolver is the injectable-clock constructor the tests
// drive; production goes through NewCachedPullNumberResolver.
func newCachedPullNumberResolver(base PullNumberResolver, ttl time.Duration, now func() time.Time) *CachedPullNumberResolver {
	return &CachedPullNumberResolver{
		base:    base,
		ttl:     ttl,
		now:     now,
		entries: make(map[pullNumberKey]pullNumberEntry),
	}
}

// PullNumberForSHA serves an unexpired cached outcome, else resolves through
// base and caches the result. A cached no-PR outcome is replayed as
// forge.ErrNoPullRequestForSHA so the router's step 0 cannot tell a cache hit
// from a fresh resolve.
func (c *CachedPullNumberResolver) PullNumberForSHA(ctx context.Context, repo, headSHA string) (uint64, error) {
	key := pullNumberKey{repo: repo, sha: headSHA}

	now := c.now()
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && !now.Before(entry.expires) {
		// Expired: drop it here rather than merely ignoring it, so a key never
		// seen again cannot outlive its TTL as resident memory.
		delete(c.entries, key)
		ok = false
	}
	c.mu.Unlock()
	if ok {
		if entry.noPull {
			return 0, forge.ErrNoPullRequestForSHA
		}
		return entry.number, nil
	}

	// Resolved outside the lock: a commits/{sha}/pulls GET must never serialize
	// every other coordinate's cache lookup behind it. Two concurrent misses on
	// one key can both call base — the cost is one duplicate GET, which is
	// strictly better than holding the mutex across network I/O. observedAt is
	// captured BEFORE the call so store can order the two by what each one saw,
	// not by which finished first.
	observedAt := c.now()
	num, err := c.base.PullNumberForSHA(ctx, repo, headSHA)
	switch {
	case errors.Is(err, forge.ErrNoPullRequestForSHA):
		c.store(key, pullNumberEntry{noPull: true, expires: c.now().Add(c.ttl), observedAt: observedAt})
		return 0, err
	case err != nil:
		return 0, err // infrastructure fault: never cached, retried next event
	}
	c.store(key, pullNumberEntry{number: num, expires: c.now().Add(c.ttl), observedAt: observedAt})
	return num, nil
}

// store records one outcome under the cache lock, sweeping entries that expired
// meanwhile. The sweep is what bounds the map: a read only evicts the key it
// looks up, so a coordinate seen exactly once would otherwise stay resident
// forever. Every store is already behind a network resolve, so walking the live
// set is far cheaper than the call that got us here.
//
// The write is LAST-OBSERVED-WINS, not last-to-finish-wins. During the burst
// this cache exists for, two resolves for one head overlap; if a slow one began
// before the PR was opened and a fast one afterwards found it, letting the slow
// one land would pin "no PR" for a full TTL and fail every check_suite for that
// head until it expired. So an entry whose resolve started EARLIER never
// replaces a live entry that saw the world later.
func (c *CachedPullNumberResolver) store(key pullNumberKey, entry pullNumberEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if cur, ok := c.entries[key]; ok && now.Before(cur.expires) && cur.observedAt.After(entry.observedAt) {
		return
	}
	c.entries[key] = entry
}

// size is the number of entries currently resident, expired or not. It exists
// so a test can assert the map does not grow without bound — the property the
// TTL alone does not give, since an ignored expired entry still occupies a key.
func (c *CachedPullNumberResolver) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
