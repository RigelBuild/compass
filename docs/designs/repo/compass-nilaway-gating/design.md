# Nilaway: advisory → gating

Status: Draft

Tracker: RIG-1534. Placement: `docs/designs/repo/` — the nilaway lane is
repo-wide Go tooling posture (the same bucket as `compass-renovate-migration.md`
and `compass-drop-proto.md`), not a CI-substrate change: the gate mechanism
(`moon run :ci` sweeping `go/moon.yml`'s `ci` aggregate) already exists and is
untouched; only the task's exit-code contract changes. `infra/` was considered
and rejected on that ground.

## Problem / Intent

`compass-go:nilaway` runs on every CI pass but cannot fail it: the task script
ends in `|| true` (`go/moon.yml:137`), a deliberate advisory posture from when
the lane was introduced (`go/moon.yml:119`: "promoting this lane to gating
means dropping it"). Advisory means every one of its findings — including real
nil-panic paths in shipped code — scrolls by unread. Burn the baseline down to
zero and drop the `|| true` so a new nil-flow finding fails the PR that
introduces it.

## Baseline, measured

Re-verified in this workspace at `main` (nilaway 0-unstable-2025-03-07 from the
pinned devenv shell), running the exact task command from `go/moon.yml:137` in
`go/`:

```text
nilaway -include-pkgs=github.com/RigelBuild/compass/go -test=false \
  -exclude-errors-in-files="gen/compass/v1/,internal/gen/compass/v1/" ./...
```

**41 top-level errors across 22 files**, in two classes. (Each error also lists
secondary "same nil source" deref sites — 53 of them, 94 flagged derefs total;
fixing the top-level flow clears its secondaries.)

### Class A — proto-getter false positives (12 errors, ~57 flagged derefs)

Generated oneof/message getters (`GetFrame`/`GetCall`/`GetCommand`/`GetOp`/
`GetBlock`/`GetPayload`) contain a literal `return nil` on the nil-receiver
path (e.g. `gen/compass/v1/runner.pb.go:477`, `agent_gateway.pb.go:1277`),
so nilaway traces a nil into every deref — but at these sites the wire
contract guarantees presence (a validated stream frame or dispatched call
always carries its variant). The sites:

| File | Top-level error | Flagged derefs | Getter |
| --- | --- | --- | --- |
| `internal/runner/dispatch.go:372` | 1 | 10 | `GetCommand()` |
| `server/forge.go:192` | 1 | 10 | `GetCall()` |
| `internal/comms/subscribe.go:289` | 1 | 8 | `GetPayload()` |
| `internal/runnerhub/relay_comms.go:416` | 1 | 8 | `GetCall()` |
| `internal/runnerhub/hub.go:560` | 1 | 4 (incl. `relay_comms.go:369`) | `GetFrame()` |
| `internal/runtime/microvm/exec.go:361` | 1 | 4 | `GetFrame()` |
| `internal/comms/mapping.go:352` | 1 | 4 | `GetBlock()` |
| `internal/comms/comms.go:670` | 1 | 3 | `GetOp()` |
| `internal/runner/config_fetch.go:75` | 1 | 2 | `GetFrame()` |
| `internal/runnerhub/relay_lifecycle.go:107` | 1 | 2 | `GetCall()` |
| `internal/runnerhub/relay_board.go:108` | 1 | 1 | `GetCall()` |
| `internal/runner/gateway/publish.go:62` | 1 | 1 | `GetFrame()` |

### Class B — the remaining 29 findings (reachable-nil bugs + guard-needed FPs)

> Not all 29 are reachable bugs. Several (`agent.go` ×9, `supervisor.go`) are
> **guard-needed false positives**: the code already `ok`/`err`-guards, but
> nilaway can't correlate the guard bool/err with the pointer, so a defensive
> nil check (same shape as Class A) is the fix — not a source-logic change.
> Others (`messages.go`/`mapping.go` `byID` deep-reads, `agent_transcripts.go`
> `entries`, `config_materialize.go` variadic, `spawn.go`, `events.go`) are
> plausibly reachable and fixed at the source. Each row is re-checked for
> reachability before its lane implements the fix.

| Site | Flow (from the nilaway trace) |
| --- | --- |
| `internal/runtime/agent.go` ×9 (`:68,:71,:75×2,:79,:82×2,:86,:90`) | `registry.Resolve()` (`internal/runtime/registry.go:40-44`) already returns `(*AgentHandle, bool)` and the `host.go` call sites (`:572,:682,:857`) already `ok`-guard — but nilaway does not correlate the `ok` bool with the pointer's nilness, so the handle reaches the `AgentHandle` receiver still flagged nil. **Guard-needed FP**, not a reachable bug. |
| `internal/comms/agent_caller.go` ×8 (`:147,:176,:272,:294×2,:316×2,:338`) | server methods (`comms/comms.go:188,:235,:244,:262,:272,:395,:419,:624`) return `(nil, err)` where nilaway can't prove err non-nil on all paths; caller derefs `resp.Msg` after only `err != nil` check |
| `internal/store/messages.go:766,:767` | unguarded map deep-read `byID[...]` (assigned `:765`) then field access |
| `internal/runner/gateway/control.go:388,:733` | `existingSession()` (`control.go:477`) returns unguarded `p.sessions[...]`; nil `ops` field (`control.go:275`) sliced |
| `internal/store/agent_transcripts.go:540` | `entries` sliced while unassigned on a path |
| `internal/comms/comms.go:557` | `resolveHandles()` returns literal nil (`comms/resolve.go:36`), result sliced |
| `internal/comms/mapping.go:339` | unguarded map deep-read `byID` dereferenced |
| `internal/runner/agent_exec.go:360` | `bufio.Reader.ReadSlice()` result sliced without guard |
| `internal/runner/config_materialize.go:557` | variadic param `exts` sliced directly |
| `internal/guestd/supervisor.go:550-551` | `os.Stat()` err IS already checked (`return err == nil && info.Mode()…`), but nilaway doesn't model os.Stat's (info non-nil when err==nil) contract, so `info.Mode()` is flagged. **Guard-needed FP** — needs an explicit `info != nil`, not an err guard. |
| `server/spawn.go:224` | `joinOrBeginSpawn()` result (`spawn.go:111`) passed unguarded to `settleSpawn()`, `.resp` deref'd |
| `events/events.go:214` | nil assigned deeply into `subscribers` (`events.go:227`), field `lagged` read |

## Approach

**Guard each proto-getter deref; really fix the genuine findings; then drop
`|| true`.** Decided by Matt (guard-each-getter ruling); the record documents
why the alternatives lose.

Nilaway offers no per-site suppression: it ignores `//nolint` directives, and
its only opt-out is file-granular (`exclude-file-docstrings`, an upstream
config flag — cited from the upstream repo, not the pinned stripped binary) —
which would blind the tool on
exactly the wire-dispatch files where a *real* new nil bug is most likely.
Per-getter guards are honest defensive code: a nil check on a proto getter is
semantically true (the getter *can* return nil on a malformed frame), costs one
branch, and turns a would-be panic into an explicit error path.

Guard shapes, by site shape:

- **Direct deref** — hoist and check:
  `if f := resp.GetFrame(); f == nil { return <error/skip> } else { …use f… }`
  or the early-return form when the function can error.
- **Oneof type-switch** (`dispatch.go:370`, `forge.go:190`, `relay_*`) — the
  switch's `default:`/`nil` case already handles a nil oneof at the switch
  head; nilaway still flags the wrapper-field access in each `case` arm
  (`c.CreateIssue` etc.). The fix is a nil guard on the wrapper field at the
  arm's entry, or hoisting `call.GetCall()` into a checked variable —
  whichever clears the finding with the smallest diff. Each lane verifies its
  choice against nilaway per package (see task test cycles) rather than
  assuming one shape silences the tool.
- **Guard-needed FPs** (nilaway can't discharge an existing `ok`/`err`
  correlation) get an explicit pointer/`info` nil-check, same shape as Class A:
  `registry.Resolve()` already returns `(*AgentHandle, bool)` and `host.go`
  already `ok`-guards, so add an explicit `handle == nil` guard at the three
  call sites; `supervisor.go:550-551` already checks `err == nil`, so add an
  explicit `info != nil`.
- **Reachable-nil bugs** get real fixes at the source: `ok`-check the
  unguarded `byID[...]` map reads (`messages.go`, `mapping.go`), check `err`
  before `resp.Msg` (or guard `resp`) in `agent_caller.go`, initialize
  `entries` (`agent_transcripts.go`), guard the variadic index
  (`config_materialize.go`).

The error message for a failed guard should name the violated wire contract
(e.g. `"sessions stream: frame missing command variant"`) so a tripped guard in
production is diagnosable — these guards are assertions on the protocol, not
dead code.

## Alternatives considered

### Nilaway declaration annotations (`nilable(…)` / `nonnil(…)`)

Nilaway parses `nilable`/`nonnil` keyword annotations out of the doc-comment
group of a declaration (`nilableKeyword`/`nonNilKeyword` /
`nilabilityFromCommentGroup` in nilaway's upstream `annotation/map.go` — cited
from the upstream repo, not verifiable against the pinned source-stripped nix
binary). Marking the getters' results `nonnil` would erase Class A wholesale.
**Loses because the annotation must
live on the declaration it describes, and those declarations are generated
code**: `go/gen` and `go/internal/gen` are protoc/connect output that AGENTS.md
forbids hand-editing and buf's `clean: true` wipes on regeneration
(`go/moon.yml:122-124`) — any annotation there is deleted on the next `gen`.
There is no external/sidecar annotation file mechanism. Annotating the *caller*
side is not supported for this flow shape.

### File docstring opt-out (`exclude-file-docstrings`)

A marker string in a file's docstring excludes the whole file from analysis.
Would silence `dispatch.go`, `forge.go`, `subscribe.go`, `relay_comms.go`, … —
the files that parse untrusted/wire input and where nilaway's future coverage
is worth the most. Loses on coverage: it trades 12 false positives for zero
detection on the highest-risk files, permanently (`go/moon.yml:134-136` already
records this judgment).

### Narrower `-exclude-errors-in-files` additions

Same coverage objection at file granularity, without even the self-documenting
marker in the file. Also drifts silently as files move. Rejected.

## Plan

### Global Constraints

1. **Scope is frozen during burn-down**: the task command stays byte-identical
   to `go/moon.yml:137` (`-include-pkgs=github.com/RigelBuild/compass/go
   -test=false -exclude-errors-in-files="gen/compass/v1/,internal/gen/compass/v1/"`)
   until T8. No new exclusions may be added to make a lane pass — a lane that
   can't clear a finding escalates instead.
2. **`//nolint` does not work on nilaway** — never add one for it. The only
   opt-outs are file-granular and are prohibited by this design (Alternatives).
3. **Guard pattern**: hoist the getter result, check nil, fail toward the
   function's existing error path with a message naming the violated wire
   contract. Never a silent `return nil`/skip that swallows a malformed frame
   unless the surrounding loop already skips unknown frames (then log at the
   loop's existing level).
4. **`-test=false` stays**: nilaway has no per-linter test relaxation dial
   (`go/moon.yml:130-133`); fixes must not rely on test-only callers.
5. **Per-lane verification is one targeted run**, from `go/`:
   `nilaway -include-pkgs=github.com/RigelBuild/compass/go -test=false -exclude-errors-in-files="gen/compass/v1/,internal/gen/compass/v1/" ./...`
   filtered to the lane's files, plus `go test ./<lane pkgs>/...`. (The full
   `./...` run is cheap — seconds — so lanes run the real command, not a
   package subset that would miss cross-package flows like
   `hub.go:560` → `relay_comms.go:369`.)
   When T1 and T5 run in parallel, each lane's `./...` run still reports the
   other's not-yet-fixed findings in the shared `internal/runner` package —
   verify by filtering to the lane's own sites; T8's zero-at-merged-tip is the
   authoritative gate.
6. **Behavioral invariant**: guards on Class A sites must be pure
   never-taken-in-practice branches — no change to happy-path behavior, no new
   allocation on the hot path. Class B fixes may change behavior only from
   "panics on nil" to "errors on nil".
7. **F1 posture**: any `.golangci.yml` or `go/moon.yml` comment touched keeps
   the disable-with-reason style already in place.

### T1 — runner lane (`internal/runner`, `internal/runner/gateway`)

Clears 8 errors (18 flagged derefs).

- Class A: `dispatch.go:372` (`GetCommand()` type-switch, 10 derefs),
  `config_fetch.go:75` (`GetFrame()`, 2), `gateway/publish.go:62`
  (`GetFrame().ControlAck`, 1).
- Class B: `gateway/control.go:388,:733` (`existingSession()` unguarded
  `p.sessions[...]` read at `control.go:476-477` + nil `ops` slice),
  `agent_exec.go:360` (`ReadSlice` result), `config_materialize.go:557`
  (variadic `exts` index).

Interfaces: consumes `*compassv1internal.SessionsResponse.GetCommand()`
(`internal/gen/compass/v1/runner.pb.go:477`) and the sessions-stream dispatch
contract in `dispatch.go:369-472`; `existingSession(id string) *session` in
`gateway/control.go:476` (may become `(*session, bool)` — internal to the
package, callers at `:333`, `:636`). No exported-API changes. Test cycle: GC5.

### T2 — runnerhub lane (`internal/runnerhub`)

Clears 4 errors (~15 derefs), all Class A: `hub.go:560` (`GetFrame()`, 4
derefs incl. the cross-file `relay_comms.go:369`), `relay_comms.go:416`
(`GetCall()`, 8), `relay_lifecycle.go:107` (`GetCall()`, 2),
`relay_board.go:108` (`GetCall()`, 1).

Interfaces: consumes `agent.pb.go:106` `GetFrame()` and
`agent_gateway.pb.go:185,:759,:2616` `GetCall()` variants; the relay dispatch
switches in `relay_comms.go:410-480`, `relay_lifecycle.go:100-120`,
`relay_board.go:100-115`. No exported-API changes. Test cycle: GC5.

### T3 — comms lane (`internal/comms`)

Clears 13 errors. The biggest and most mixed lane.

- Class A: `subscribe.go:289` (`GetPayload()`, 8 derefs), `mapping.go:352`
  (`GetBlock()`, 4), `comms.go:670` (`GetOp()`, 3).
- Class B: `agent_caller.go` ×8 — derefs of `resp.Msg` where the callee
  (`comms.go:188,:235,:244,:262,:272,:395,:419,:624`) has `(nil, …)` return
  paths nilaway can't prove carry a non-nil error. Fix at the source where a
  callee truly can return `(nil, nil)`; otherwise guard `resp` at the caller.
  Plus `comms.go:557` (`resolveHandles` nil-slice return from
  `resolve.go:36`), `mapping.go:339` (unguarded `byID` map deep-read).

Interfaces: the `agentCaller` → server-method call surface in
`agent_caller.go:143-338`; `resolveHandles(...) ([]string, error)` in
`comms/resolve.go`; subscription fan-out `subscribe.go:280-325`. No exported-API
changes. Test cycle: GC5.

### T4 — store lane (`internal/store`)

Clears 3 errors, all Class B: `messages.go:766,:767` (`ok`-check the
`byID[...]` read at `:765`), `agent_transcripts.go:540-541` (`entries` used
while unassigned on a path in `maybeSafetyValve`, `agent_transcripts.go:472+`).

Interfaces: internal to `Store`; no signature changes. Test cycle: GC5 (store
has an existing test suite — run it).

### T5 — runtime lane (`internal/runtime`, `internal/runtime/microvm`)

Clears 10 errors.

- Class B (guard-needed FP): `agent.go` ×9. `Registry.Resolve()`
  (`internal/runtime/registry.go:40-44`) already returns `(*AgentHandle, bool)`
  and the three `host.go` call sites (`:572,:682,:857`) already `ok`-guard with
  an early return on a miss — so there is NO Resolve signature change to make
  and NO reachable nil. nilaway flags these because it does not correlate the
  `ok` bool with the pointer, letting the (statically non-nil) handle reach the
  `AgentHandle` receiver in `agent.go` still marked nil. Fix: add an explicit
  `handle == nil` guard at the three `host.go` sites (before the handle is used
  as a receiver), which nilaway discharges directly. The lane verifies the
  guard clears all nine per GC5 before finishing.
- Class A: `microvm/exec.go:361` (`GetFrame()` on `guest_control.pb.go:572`,
  4 derefs).

Interfaces: no signature change — `Registry.Resolve` is already
`(*AgentHandle, bool)`. T5 edits the three `runner/host.go` call sites
(`:572,:682,:857`) to add the explicit nil guard; those sites live in the
`internal/runner` package T1 also works, but T1 does not touch them (disjoint
site ownership). Test cycle: GC5.

### T6 — server lane (`server/`)

Clears 2 errors (~13 derefs): `forge.go:192` (Class A, `GetCall()` type-switch
over 10 forge variants, `forge.go:190-213`), `spawn.go:224` (Class B —
`joinOrBeginSpawn` result from `spawn.go:104-111` unguarded into
`settleSpawn`, `.resp`/`.done` derefs at `:224,:225,:230`).

Interfaces: `forgeService.ExecuteForgeCallAsAccount` (`forge.go:180`),
`joinOrBeginSpawn`/`settleSpawn` internal pair. No exported-API changes. Test
cycle: GC5.

### T7 — guestd + events lane (`internal/guestd`, `events/`)

Clears 2 errors, both small: `supervisor.go:550-551` (guard-needed FP — the
`os.Stat` err is already checked, `return err == nil && info.Mode()…`; add an
explicit `info != nil` since nilaway doesn't model os.Stat's info/err
correlation), `events.go:214` (nil assigned deeply into `subscribers` at
`:227` then `lagged` read at `:214,:218,:221,:340` — guard or stop storing
nil). Bundled as one task because each alone is below the right-size bar.
Interfaces: internal only. Test cycle: GC5.

### T8 — cutover (LAST, strictly ordered after T1–T7)

Drop `|| true` from `go/moon.yml:137` and rewrite the surrounding comment
block (`go/moon.yml:117-136`) to describe the gating posture: baseline is
zero, new findings fail the introducing PR, the guard rules (GC2/GC3) are the
prescribed response, and this record is the citation. Verification: the full
task command exits 0 with zero findings at the merged tip of T1–T7.

**Ordering is load-bearing**: T8 merged before any lane's findings are cleared
turns `main` red for every PR. T1–T7 are mutually independent (disjoint files —
T5's `host.go` ownership carve-out noted above) and can land in any order or in
parallel; T8 depends on all of them.

### Future findings (the forcing function)

Once T8 lands, any PR whose diff introduces a nil flow fails `compass-go:ci`
via the `nilaway` dep (`go/moon.yml:219`). This is intended. The playbook for
the failing author:

- **New proto-getter deref**: guard it per GC3 — same as the baseline burn-down.
- **Genuine flow**: fix it at the source.
- **Believed false positive with no guardable shape**: escalate to Matt; never
  a file opt-out or scope-narrowing (GC1/GC2 survive the cutover as standing
  policy, restated in the T8 comment block).
- **New generated tree** (e.g. `compass/v2`): extend `-exclude-errors-in-files`
  — the one sanctioned exclusion edit, already documented at
  `go/moon.yml:129-130`.

## Tasks

- [ ] T1 runner lane: clear 8 errors in `internal/runner{,/gateway}`
- [ ] T2 runnerhub lane: clear 4 errors in `internal/runnerhub`
- [ ] T3 comms lane: clear 13 errors in `internal/comms`
- [ ] T4 store lane: clear 3 errors in `internal/store`
- [ ] T5 runtime lane: clear 10 errors in `internal/runtime{,/microvm}` (+ 3 `runner/host.go` call sites)
- [ ] T6 server lane: clear 2 errors in `server/`
- [ ] T7 guestd+events lane: clear 2 errors in `internal/guestd`, `events/`
- [ ] T8 cutover: drop `|| true` at `go/moon.yml:137`, rewrite comment — only after T1–T7 are merged

## Open Questions

All load-bearing; for Matt via the driver.

1. **Ratify guard-each-getter over the annotation route.** Matt already ruled
   guard-each-getter; this record adds the investigated grounds: nilaway's
   `nilable`/`nonnil` annotations exist (`annotation/map.go`, upstream repo)
   but must
   sit on the generated declarations, which AGENTS.md forbids editing and buf
   regeneration wipes — so the annotation route is infeasible, not merely
   inferior. Recommendation: confirm and close.
2. **Do the Class B (genuine) fixes ship ahead as their own wave?** They are
   real bug fixes with standalone value regardless of the gate. Recommendation:
   no separate sequencing — each lane task already carries both classes for its
   files, and splitting by class would double the PR count over the same files.
   But if Matt wants the genuine fixes expedited, T4/T7 and the Class B halves
   of T1/T3/T5/T6 could be pulled forward.
3. **Gate-flip mechanics: once at the end (recommended) vs per-lane
   toggling.** Recommendation: the gate flips exactly once, in T8, after all
   lanes merge. Per-lane drop-and-restore of `|| true` would make `main`'s gate
   posture oscillate and create merge races between lanes. The residual risk of
   flip-once — a regression landing between a lane's merge and T8 — is bounded
   by T8's own verification run (it must observe zero findings before the flip
   merges; any interloper finding blocks T8, not `main`).
4. **`agent_caller.go` fix style (T3).** The 8 findings are `(resp, err)`
   flows where the server method has a `return nil, <maybe-nil err>` path.
   Options: (a) fix the callees so every nil-resp path provably returns
   non-nil error (source fix, nilaway-clean, no caller churn); (b) guard
   `resp`/`resp.Msg` at each caller (defensive, 8 small diffs).
   Recommendation: (a) where the callee's nil-err path is a real bug, (b) for
   the rest — decided per finding by the T3 implementer, but the *default*
