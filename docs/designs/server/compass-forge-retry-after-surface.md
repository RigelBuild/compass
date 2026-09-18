# Design: Forge retry-after surface (RIG-2255)

Ledger: DL-276 (this PR), Comms & tools. Freezes on merge; no rows superseded.

## Problem / Intent

`ForgeCallError.retry_after_ms` (proto/compass/v1/agent_gateway.proto:268-272, "0
when the forge gave no hint") is structurally always 0: neither forge error type
carries the provider's rate-limit reset out of the client, so the forge-call
chokepoint's mapping (`mapForgeError`, go/server/forge.go:673-696) has nothing to
populate it from — its own doc comment says so (go/server/forge.go:667-672).
This is NOT write-only: the same mapper serves the agent's read ops
(`get_issue` forge.go:542, `get_pull_request` :557, `list_issues` :572) as well
as the writes, and the client's fail-fast rate gate arms per-request regardless
of direction — a rate-limited read surfaces the same hint-less error. The
emission sites below are correspondingly direction-agnostic (a GET wrap at
github.go:642, a list-page wrap at :158, a POST wrap at :854, plus the shared
response classifier at :1035). Both clients COMPUTE a real reset instant and
then discard it on the error path. Intent: carry the reset hint on the
budget-exhausted error surface, from both emission sites in both providers,
through to `retry_after_ms`, for reads and writes alike.

## Global Constraints

- Go, stdlib-only forge clients — no go-github / GraphQL lib, matching the
  existing no-dependency posture of `github.go` / `linear.go`.
- `errors.Is(err, forge.ErrBudgetExhausted)` MUST keep working at every current
  call site. Enumerated non-test sites: emission `go/internal/forge/github.go:158,642,854,1035`
  and `go/internal/forge/linear.go:315,369`; consumption
  `go/internal/ingest/driver.go:168` (poll-driver skip) and
  `go/server/forge.go:681` (resource_exhausted arm). Test sites relying on the
  sentinel: `github_test.go:195,228,234,274,311,605,613,835,898,1165`,
  `linear_test.go:376,383,398,554,602,622`, `driver_test.go:468`,
  `forge_test.go:756`. None may regress.
- Both providers (GitHub + Linear) covered symmetrically across their mirror
  seams; Linear is issues-only (go/internal/forge/linear.go:10-12) but shares the
  identical gate shape.
- Injectable clock exists on both clients (`g.now()` / `l.now()`); the server has
  none — all clock reads happen client-side at error construction.
- Red-first tests per rule://red-green-testing: failing tests land before the
  implementation in each task.
- The proto contract `retry_after_ms == 0` ⇒ "no hint" is preserved.

## Approach

**Option A: a dedicated typed rate-limit error, `*forge.RateLimitError`, that
replaces the bare `ErrBudgetExhausted` wrap at every emission site while
remaining `errors.Is`-compatible via `Unwrap`.**

```go
// RateLimitError is a rate-limit skip carrying the retry hint. It unwraps to
// ErrBudgetExhausted, so every existing errors.Is site keeps matching.
type RateLimitError struct {
    // RetryAfter is how long until the budget gate re-opens, computed at
    // error construction from the client's injectable clock. Zero = no hint.
    RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "forge: rate budget exhausted" }
func (e *RateLimitError) Unwrap() error { return ErrBudgetExhausted }
```

`errors.Is` walks `Unwrap`, so the driver's skip contract
(go/internal/ingest/driver.go:168), the server arm (go/server/forge.go:681), and
every test above keep working unchanged; `errors.As` recovers the hint at the
chokepoint. The type lives beside `StatusError` in
`go/internal/forge/provider.go` (the existing error-type home,
provider.go:249-263).

**Carried value: `time.Duration` (retry-after), not `time.Time` (absolute
reset).** Load-bearing for testability: both clients have an injectable clock
(`g.now()` / `l.now()`), the server does not. A duration is computed at error
construction with the client's clock, so the server conversion is pure
arithmetic (`RetryAfter.Milliseconds()`, clamped to [0, math.MaxUint32]) with no
clock read. An absolute `time.Time` would force the server to read a wall clock
it cannot inject, making the mapping untestable deterministically and skewed by
queue delay.

**Emission sites (six across both providers — GitHub has four: three fail-fast
gate call sites + one live-response classifier; Linear has two: one fail-fast +
one live):**

1. GitHub live-response classifier (`mapErrorResponse`,
   go/internal/forge/github.go:1024-1046): compute `reset :=
   g.rateLimitReset(resp)` (github.go:1068-1078) BEFORE arming; arm the gate as
   today (`armGate`, github.go:998-1003); wrap `&RateLimitError{RetryAfter:
   hint}` where `hint = reset.Sub(g.now())` clamped ≥ 0 if `reset` is non-zero,
   else 0 (no header ⇒ no hint; the gate still self-arms with `defaultSkip`
   internally, github.go:107).
2. GitHub fail-fast gate (`gateBlocked`, github.go:831-842; call sites
   github.go:157-158, 641-642, 853-854): the gate is armed, so `g.resetAt` is
   known — the hint is `g.resetAt.Sub(g.now())`. `gateBlocked` changes signature
   to return the remaining duration so call sites can construct the error.
3. Linear live-response classifier (`handleResponse` rate-limit arm,
   go/internal/forge/linear.go:365-370, with `rateLimitReset`
   linear.go:461-471): same as (1).
4. Linear fail-fast gate (`gateBlocked`, linear.go:412-423; call site
   `doGraphQL` linear.go:314-316): same as (2).

**Fail-fast hint provenance (decided):** at the fail-fast sites the hint is
surfaced even when `resetAt` originated from the synthetic `defaultSkip`
fallback (github.go:995-1003, linear.go:425-432) or from `recordBudget` on a
success response (linear.go:434-455): once armed, `resetAt` is BY CONSTRUCTION
the instant the client will next issue a request, so `resetAt − now()` is a
truthful retry hint regardless of provenance — no provenance tracking needed.
At the LIVE site, an absent/unusable header surfaces 0 ("no hint"), preserving
the proto contract; the synthetic fallback only shapes the internal gate. This
yields a deliberate asymmetry for a headerless rate-limit window: the live
error carries 0, then the immediately-following gated call carries up to the
bounded `defaultSkip` (≤60s) for the SAME window — not a bug: the second error
genuinely knows the armed gate the first response only implied.

**Server mapping (`mapForgeError`, go/server/forge.go:673-696):** the
`ErrBudgetExhausted` arm (forge.go:681-682) additionally does `errors.As(err,
&rle)` and sets `RetryAfterMs` from `rle.RetryAfter.Milliseconds()` clamped to
`[0, math.MaxUint32]` BEFORE the `uint32` cast (a negative or >MaxUint32 value
would otherwise wrap — see T4 Interfaces for the guard). The `*StatusError{429}` arm
(forge.go:691-692) stays hint-less at 0. Precisely: this arm is reached by a
429 that `isRateLimited` did NOT classify as a rate-limit skip
(github.go:1048-1061) — a header-carrying rate-limited 429 becomes
`RateLimitError` on the dominant path (github.go:1030-1036; Linear
short-circuits EVERY 429 into the rate-limit arm, linear.go:365-370, so a
Linear `StatusError{429}` is unreachable). One narrow GitHub sub-case is a
deliberate out-of-scope drop: a 429 carrying ONLY `X-Ratelimit-Reset` (no
`Retry-After`, remaining absent/nonzero) fails `isRateLimited` and falls
through to `StatusError{429}` while `rateLimitReset` COULD read a usable reset
— its hint is dropped to 0. Widening `isRateLimited` to treat a present
`X-Ratelimit-Reset` as a rate-limit discriminator would recover it, but that
changes the poll driver's skip/abort CLASSIFICATION (`driver.go:168-177`),
a behavior change beyond "fill the field"; it is left to a follow-up, not this
slice (GitHub sends remaining=0 alongside reset on real limits, so exposure is
narrow). The stale always-0 doc note (forge.go:667-672) is rewritten; T4 also
updates the `retry_after_ms` proto comment (agent_gateway.proto:271), whose
"0 when the forge gave no hint" now under-describes the field — the value is
"time until a retry stops fail-fasting," a superset of provider-stated hints
(see Fail-fast hint provenance).

## Alternatives considered

**(B) Widen `StatusError` with a reset field and route rate limits through
`*StatusError{429, ..., reset}` instead of `ErrBudgetExhausted` — rejected.**
Grounded break: the poll driver distinguishes `ErrBudgetExhausted` (warn +
clean skip, retry next tick, go/internal/ingest/driver.go:168-171) from every
other error (Error-level log + abort, driver.go:176-177). Re-routing rate
limits onto `StatusError` either breaks that skip contract (rate limits start
logging as errors) or forces `StatusError` to grow an `Is` method matching the
sentinel only when `Status == 429` — a conditional identity that muddies the
taxonomy (`StatusError` is the "forge said HTTP N" carrier, provider.go:249-257,
not a rate-limit signal). It also does nothing for the DOMINANT carrier: a real
rate-limited 403/429 never becomes `StatusError{429}` today
(github.go:1030-1036), so widening `StatusError` alone leaves `retry_after_ms`
at 0 on the main path. A is strictly smaller and breaks nothing.

**Carrying `time.Time` instead of `time.Duration` — rejected** (see Approach:
the server has no injectable clock; conversion must happen where the clock
lives).

## Plan

Each task is red-first: tests land failing, then the smallest implementation
turns them green. T2/T3 depend on T1; T4 depends on T1 (and is exercised
end-to-end once T2/T3 land).

### T1 — `forge.RateLimitError` type

Add the typed error beside `StatusError` in `go/internal/forge/provider.go`
(after provider.go:263).

- Red: table test in a new `provider_test.go` (or the existing forge test file
  housing error-shape tests) asserting `errors.Is(&RateLimitError{}, ErrBudgetExhausted)`
  is true, `errors.As` recovers the value through an `fmt.Errorf("...: %w", ...)`
  wrap, and `Error()` renders the stable message.
- Green: the type + methods.

Interfaces:

```go
// consumes: forge.ErrBudgetExhausted (github.go:22)
// produces:
type RateLimitError struct {
    RetryAfter time.Duration // 0 = no hint
}
func (e *RateLimitError) Error() string // "forge: rate budget exhausted"
func (e *RateLimitError) Unwrap() error // returns ErrBudgetExhausted
```

### T2 — GitHub client emits the hint (both sites)

- Red (extends `go/internal/forge/github_test.go` using the existing scripted
  RoundTripper + injectable clock harness):
  - live site: a 403 with `Retry-After: 60` yields an error where
    `errors.As → *RateLimitError` with `RetryAfter == 60*time.Second`, and
    `errors.Is(err, ErrBudgetExhausted)` still true (extend the
    github_test.go:827 mapping test).
  - live site, no usable headers (403 + `X-Ratelimit-Remaining: 0` only, no
    reset header): `RetryAfter == 0` while the gate still arms with
    `defaultSkip`.
  - fail-fast site: after arming, advance the clock partway; the gated call's
    error carries `RetryAfter == resetAt − now()` (e.g. 1s remaining of a 60s
    window, extending github_test.go:227-236).
- Green: change `gateBlocked` to return the remaining duration; construct
  `&RateLimitError{...}` at github.go:158, 642, 854 (fail-fast) and
  github.go:1035 (live, via `reset := g.rateLimitReset(resp)` computed once,
  passed to both `armGate` and the hint).

Interfaces:

```go
// changed (unexported): reports gate state AND the remaining wait when blocked.
func (g *GitHub) gateBlocked() (time.Duration, bool)
// emission shape at all four sites:
fmt.Errorf("...: %w", &RateLimitError{RetryAfter: hint})
```

### T3 — Linear client emits the hint (both sites)

Mirror of T2 on the Linear seams.

- Red (extends `go/internal/forge/linear_test.go`): a 429 with
  `Retry-After: 60` → `RetryAfter == 60s` (extend linear_test.go:365-401); an
  `X-Ratelimit-Requests-Reset` epoch-ms fallback (linear_test.go:610-626 shape)
  → the epoch-ms-derived duration; a header-less RATELIMITED GraphQL rejection
  → `RetryAfter == 0` with the gate armed; the gated fail-fast call →
  `resetAt − now()`.
- Green: `gateBlocked` (linear.go:412-423) returns the remaining duration;
  construct `&RateLimitError{...}` at linear.go:315 (fail-fast) and
  linear.go:369 (live, reset computed once via `l.rateLimitReset(resp)`,
  linear.go:461-471).

Interfaces:

```go
func (l *Linear) gateBlocked() (time.Duration, bool)
// emission shape identical to T2.
```

### T4 — server mapping populates `retry_after_ms`

- Red (extends `go/server/forge_test.go`, the
  TestForgeBudgetExhaustedAnd429MapToResourceExhausted harness,
  forge_test.go:741-759): a scripted
  `fmt.Errorf("x: %w", &forge.RateLimitError{RetryAfter: 90 * time.Second})` →
  `resource_exhausted` with `RetryAfterMs == 90000`; the bare sentinel and a
  zero-hint `RateLimitError` → `RetryAfterMs == 0`; a negative `RetryAfter` →
  0; `*StatusError{429}` stays 0.
- Green: in `mapForgeError`'s sentinel arm (go/server/forge.go:681-682),
  `errors.As` for `*forge.RateLimitError` and set the clamped milliseconds;
  rewrite the stale always-0 doc note (forge.go:667-672) AND the
  `retry_after_ms` proto comment (agent_gateway.proto:271) to the widened
  "time until a retry stops fail-fasting" semantics.

Interfaces:

```go
// consumes: *forge.RateLimitError (T1), compassv1internal.ForgeCallError.RetryAfterMs (uint32)
// unchanged signature:
func mapForgeError(err error, op forgeOp) *compassv1internal.ForgeCallError
// clamp: ms := rle.RetryAfter.Milliseconds(); ms < 0 → 0; ms > math.MaxUint32 → math.MaxUint32
```

### T5 — poll-driver regression pin

- Red-then-green in one step (behavior is unchanged; the test pins it): extend
  `go/internal/ingest/driver_test.go` (the driver_test.go:467-471 harness) so a
  scripted `*forge.RateLimitError` — not the bare sentinel — still takes the
  budget-exhausted skip branch (driver.go:168-171), proving no consumer of the
  sentinel regressed. No driver code changes.

Interfaces: none (test-only; consumes T1's type and the existing
`pageResult{err: ...}` scripting).

## Tasks

- [ ] T1: `forge.RateLimitError` (Unwrap → `ErrBudgetExhausted`) + shape tests
- [ ] T2: GitHub — hint at live classifier + fail-fast gate (3 call sites), red-first
- [ ] T3: Linear — hint at live classifier + fail-fast gate, red-first
- [ ] T4: `mapForgeError` populates clamped `RetryAfterMs`; stale doc note rewritten
- [ ] T5: poll-driver skip-contract regression pin

## Resolved decisions

- **Fork option A over B** — B breaks the poll driver's sentinel skip contract
  (driver.go:168-171) and misses the dominant carrier; see Alternatives.
- **`time.Duration` over `time.Time`** — the clock lives client-side; the
  server conversion is clockless arithmetic; see Approach.
- **Fail-fast hints surface even for synthetic/`recordBudget`-armed gates;
  live-site hints are 0 when no usable header** — `resetAt` is truthful once
  armed regardless of provenance; see Approach. If Matt prefers the synthetic
  `defaultSkip` window to also surface 0 (strict "provider-stated hints only"),
  the change is confined to the fail-fast emission and does not alter the
  design's shape — flagged here rather than as a blocking question.
- **`X-Ratelimit-Reset`-only 429 hint dropped — out of scope (design-critic
  finding).** A GitHub 429 carrying only `X-Ratelimit-Reset` (no `Retry-After`,
  remaining absent/nonzero) is not classified as a rate-limit skip by
  `isRateLimited` (github.go:1048-1061), reaches the `StatusError{429}` arm, and
  its readable reset drops to 0. Recovering it needs widening `isRateLimited`,
  which changes the poll driver's skip/abort classification (driver.go:168-177)
  — a behavior change beyond this slice's "fill the field." Left to a follow-up;
  narrow real-world exposure (GitHub sends remaining=0 with reset on real limits).
- **Proto-comment semantic widening (design-critic finding).** `retry_after_ms`
  becomes "time until a retry stops fail-fasting," a superset of provider-stated
  hints; T4 updates the proto comment (agent_gateway.proto:271) accordingly. The
  live/fail-fast asymmetry for a headerless window is documented in Approach, not
  a bug.

Design-critic red-team (pre-freeze): 0 blocking, 0 load-bearing forks for Matt;
1 medium + 3 low folded above; core choices (option A, `time.Duration`,
provenance-agnostic fail-fast hint) all ratified.
