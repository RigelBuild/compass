# Design: Runner gateway/operator-fault error sentinels

Status: Active

Linear: RIG-1442. Approach ruled by Matt (full fix across all three lanes:
proto enum value + server mapping arm + runner sentinels); this record
documents and decomposes the decided approach — it does not re-open the
choice.

## Problem / Intent

An operator misconfiguration inside the Runner — canonically an agent socket
path over the AF_UNIX limit — is returned to the admin as Connect `Internal`,
because the Runner has no wire code for "your deployment is misconfigured".
`errorResult` (`go/internal/runner/dispatch.go:455-462`) maps only two
sentinels and defaults everything else to INTERNAL:

> ```go
> func errorResult(id string, err error) *compassv1internal.SessionsRequest {
>   code := compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL
>   switch {
>   case errors.Is(err, errAlreadyRunning):
>     code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING
>   case errors.Is(err, errSessionUnknown):
>     code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND
>   }
> ```

and the Server's `runnerErrorToConnect`
(`go/internal/runnerhub/commands.go:206-215`) likewise defaults to
`CodeInternal`:

> ```go
> switch e.GetCode() {
> case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING:
>   code = connect.CodeAlreadyExists
> case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND:
>   code = connect.CodeNotFound
> default:
>   code = connect.CodeInternal
> }
> ```

The canonical operator-fault path is the socket-path-too-long check, a bare
`fmt.Errorf` with no sentinel (`go/internal/runner/gateway/socket.go:140-141`):

> ```go
> if len(path) > sunPathMax {
>   return nil, fmt.Errorf("agent socket path %q is %d bytes, over the %d-byte AF_UNIX limit: shorten the Runner's --runtime-dir or the agent account id", path, len(path), sunPathMax)
> }
> ```

The diagnostic *message* survives the relay (`RunnerError.message` is carried;
`commands.go:216` — `fmt.Errorf("runner: %s", e.GetMessage())`), but the
*classification* is lost: the admin sees `Internal`, which reads as a Compass
bug rather than a knob to turn. Per RIG-1442, fix this as ONE change covering
the sibling operator-fault paths, not a one-off for the too-long check.

## Global Constraints

- **Repo/module layout**: RigelBuild/compass; Go module at `go/`, proto at
  `proto/compass/v1/`. Generated internal Go code under
  `go/internal/gen/compass/v1/` (`runner.pb.go` carries the enum today,
  `go/internal/gen/compass/v1/runner.pb.go:69`).
- **Wire change is additive-only and buf-safe.** `runner.proto`'s header
  states the posture (`proto/compass/v1/runner.proto:16-18`): "Additive and
  buf-breaking-safe: a new file in the owned `compass.v1` package, not a
  change to the public messages. `buf lint`/`buf breaking` still cover it".
  This change adds ONE enum value (tag 5) to `RunnerErrorCode` — nothing else
  on the wire moves. RunnerService is INTERNAL-ONLY (`runner.proto:5-8`),
  generated only into internal Go consumers, so no public TS client is
  affected.
- **Enum precedent to mirror exactly**: `RUNNER_ERROR_CODE_RESOURCE_EXHAUSTED
  = 4` added for RIG-1569 (`runner.proto:418-422`) — a new value with a doc
  comment naming its Connect mapping ("-> Connect ResourceExhausted") and the
  distinguishing rationale.
- **`go test -race` is the merge gate** (CGO_ENABLED=1); new tests must be
  race-clean.
- **Cross-lane ownership**: the proto file and the runner code are the
  compass-runner lane's; `go/internal/runnerhub/` (the mapping arm + its
  test) is compass-server's lane — T2 below is a coordinated hand-off.
- **Red-green discipline** (rule://red-green-testing): each lane's test lands
  red against the pre-change behavior (new code falls to INTERNAL /
  CodeInternal) and goes green with the change.
- Commit identity per repo convention (seal + Matt co-author trailer);
  squash-merge; no issue-id metadata in code comments — code comments cite
  this record by path.

## Approach

Matt-ruled, three coordinated changes across three lanes:

### (1) Proto: one new `RunnerErrorCode` value (tag 5)

`enum RunnerErrorCode` today (`proto/compass/v1/runner.proto:408-423`) holds
UNSPECIFIED=0, ALREADY_RUNNING=1, NOT_FOUND=2, INTERNAL=3,
RESOURCE_EXHAUSTED=4. Add tag 5 for the operator-fault class, mirroring the
RESOURCE_EXHAUSTED precedent (value + doc comment naming its Connect mapping
and why it is distinguished).

**Ruled value** (Matt, 2026-08-05): `RUNNER_ERROR_CODE_FAILED_PRECONDITION = 5`
→ Connect `FailedPrecondition`. Recorded rationale:

- The client's *arguments* are fine. A provision that trips the socket-path
  guard carries a well-formed `ProvisionAgentWorkspaceRequest`; the overlong
  path is built Runner-side from the operator-supplied `--runtime-dir` plus
  the minted account id (`socket.go:119-122`: "RuntimeDir is the
  operator-supplied variable: --runtime-dir is unbounded on every hop").
  `InvalidArgument` semantically indicts the request — retrying with a
  "fixed" request cannot help, because no request fixes it.
- `FailedPrecondition` is defined as "the system is not in a state required
  for the operation's execution" — exactly an environment-not-in-a-state-to-
  serve condition. The remedy is an operator knob (shorten `--runtime-dir`,
  fix directory permissions), i.e. fix the *system state*, then retry the
  *same* request — the precise retry semantics FailedPrecondition signals and
  InvalidArgument denies.
- **In-repo precedent**: FailedPrecondition is already this codebase's
  "deployment misconfiguration" vocabulary, so this choice aligns the Runner
  with the classification its own relay already uses. `errNotAgentAccount`
  maps to `CodeFailedPrecondition` NOT `CodeInvalidArgument`, and the comment
  states the difference is "load-bearing at the relay": InvalidArgument is a
  routine per-frame refusal, FailedPrecondition a contract defect logged as a
  misconfiguration (`go/internal/comms/agent_caller.go:71-79`). The no-secrets
  / no-config-surface servers answer `CodeFailedPrecondition` for absent
  deployment state (`go/internal/runnerhub/handler.go:52-56`, and the fail-
  closed arms at `:253-254`, `:288-292`, `:324-326`), and the comms
  request-error mapper maps `store.ErrFailedPrecondition` →
  `CodeFailedPrecondition` (`go/internal/comms/context.go:52`). Choosing InvalidArgument would
  put the Runner's operator-fault class at odds with the repo's own
  comms-relay classification.
- Naming the enum after the Connect code (not e.g. `OPERATOR_FAULT`) follows
  the existing vocabulary: every current value names its wire semantics and
  lets the doc comment carry the classification rationale, as
  RESOURCE_EXHAUSTED does.

### (2) Server: `runnerErrorToConnect` arm (compass-server lane, cross-lane)

Add one `case` arm to the switch at `go/internal/runnerhub/commands.go:208`
mapping the new value to the chosen Connect code, plus one row in the
table-driven `TestRunnerErrorToConnectCodeMapping`
(`go/internal/runnerhub/commands_test.go:25-35`, whose rows are
`{name, code, want connect.Code}` and which also asserts "The Runner's
message is surfaced, not swallowed", `commands_test.go:42-43`). The relay
call site needs no change: `relay` already routes every `RunnerError` through
this function (`commands.go:199-201`).

**This task is owned by compass-server and must be coordinated** — the
compass-runner lane owner hands it off. It is mechanically independent of the
runner change (an unmapped code falls to the default `CodeInternal` arm, so
lanes can land in either order after T1) but the end-to-end fix requires both.

### (3) Runner: an operator-fault sentinel + `errorResult` arm

**Sentinel placement.** The operator-fault paths live in package `gateway`
(`go/internal/runner/gateway/socket.go`); the mapping lives in package
`runner` (`dispatch.go`). The existing sentinels are package-private to
`runner` (`dispatch.go:69-73`: "Sentinel errors the host returns, mapped to
RunnerErrorCode on the wire"), so a new *exported* sentinel goes in the
package that raises it:

```go
// package gateway
// ErrOperatorConfig marks a failure whose remedy is an operator knob
// (runtime-dir length, socket-dir permissions/ownership) rather than a
// Compass bug. errorResult (runner/dispatch.go) maps it to
// RUNNER_ERROR_CODE_FAILED_PRECONDITION instead of INTERNAL.
var ErrOperatorConfig = errors.New("operator-fault runner configuration")
```

Qualifying paths wrap it: `fmt.Errorf("...: %w", ErrOperatorConfig)` appended
to the existing diagnostic (the messages already name the knobs). Note the
sentinel's *text* ("operator-fault runner configuration") is appended to
`err.Error()`, which is exactly `RunnerError.message`
(`go/internal/runner/dispatch.go:465-467` — `Message: err.Error()`) and thus
the diagnostic the admin reads (`go/internal/runnerhub/commands.go:216` —
`fmt.Errorf("runner: %s", e.GetMessage())`). So the wire message is not
verbatim: the sentinel phrase is appended. This is acceptable and
self-documenting — the appended phrase names the fault class — and existing
socket tests use `strings.Contains` (`socket_test.go:449-458`) so they stay
green. The chain survives to `errorResult` intact: `serveSocket` wraps
with `%w` (`go/internal/runner/host.go:829-831` — `fmt.Errorf("serving agent
socket for container %q: %w", containerName, err)`), `gateway.Serve` returns
`listenAgentSocket`'s error unwrapped (`gateway.go:277-281`), and Provision
returns `serveSocket`'s error directly (`host.go:172-175`), landing at
`errorResult(id, err)` via the Provision arm (`dispatch.go:380-383`). The
`runner` package already imports `gateway` (`host.go:828` calls
`gateway.Serve`), so no import-direction issue.

`errorResult` gains one arm:

```go
case errors.Is(err, gateway.ErrOperatorConfig):
  code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION
```

**Which socket.go paths qualify.** Classification rule: a failure is
operator-fault when it is *deterministic given the deployment configuration*
and its remedy is an operator knob; it stays INTERNAL when it is an
unexpected OS failure after all preconditions were verified.

Operator-fault (wrap `ErrOperatorConfig`):

- **Path-too-long** (`socket.go:140-141`, quoted in Problem). Deterministic
  from `--runtime-dir`; the message names both knobs. `validateRuntimeDir`
  (`go/internal/runner/run.go:59-78`) already refuses an over-budget dir at
  boot, so this guard is residual defense-in-depth — but when it does fire
  (e.g. a budget-model skew), it is by construction an operator condition.
- **MkdirAll of the socket dir** (`socket.go:145-147` — `fmt.Errorf("creating
  agent socket dir %q: %w", dir, err)`). The dir lives under the
  operator-supplied runtime dir; failure here is EACCES/EROFS-class —
  wrong ownership, read-only mount, i.e. deployment state. (MkdirAll can also
  fail ENOSPC — disk-full, arguably transient rather than a config knob — but
  there is no cheap errno split that pays for itself, and FailedPrecondition's
  "fix system state, then retry" still reads correctly for disk-full.)
- **Chmod of the socket dir** (`socket.go:150-152` — `fmt.Errorf("securing
  agent socket dir %q: %w", dir, err)`). Same substrate: "A pre-existing dir
  may carry a looser mode (umask, or a prior op)" (`socket.go:148-149`) —
  a pre-existing dir the Runner cannot secure is deployment state.
- **`reclaimStaleSocket` fail-closed refusals** (`socket.go:197-198`
  non-socket occupying the path — "refusing to remove"; `socket.go:204-205`
  wrong-owner socket — "owned by uid %d, not the Runner uid %d; refusing to
  remove"). Both are deliberate refusals over on-disk state only an operator
  can clear ("it is never an abandoned Runner socket, so deleting it would be
  destroying an unrelated inode", `socket.go:185-186`). The remedy is
  operator action on the filesystem → FailedPrecondition fits exactly.
  (The `Lstat`/ownership-read errors at `socket.go:192-194` and
  `socket.go:200-203` stay INTERNAL: unexpected inspection failures, not
  classified deployment state.)
- **`ensureRoot` MkdirAll + Chmod** (`go/internal/runner/config_materialize.go:205-210`
  — `fmt.Errorf("ensuring config root %q: %w", ...)` and `fmt.Errorf("pinning
  config root %q mode: %w", ...)`). This is the same operator-supplied
  runtime-dir substrate as the socket dir: `configMaterializerFor` roots the
  materializer at `<RuntimeDir>/containers/<name>/config`, explicitly
  "mirroring the per-container agent-socket layout"
  (`go/internal/runner/host.go:804-812`). Its error `%w`-chains to
  `errorResult` via the Provision arm: `Materialize` failure returns
  `fmt.Errorf("materializing agent config: %w", err)`
  (`go/internal/runner/host.go:184-190`), which lands at `errorResult(id, err)`
  through the same Provision path. The sentinel is importable there — package
  `runner` already imports `gateway` (`go/internal/runner/host.go:828` calls
  `gateway.Serve`). Indistinguishable in kind from the socket-dir
  MkdirAll/Chmod, so it wraps `ErrOperatorConfig` on the same footing.
  (The staging/unpack MkdirAlls at `config_materialize.go:401` and `:407`
  stay INTERNAL — bundle-content-driven, murkier; see OQ-4.)

Genuine-internal (unchanged, default INTERNAL):

- **Listen** (`socket.go:159-161` — `fmt.Errorf("listening on agent socket
  %q: %w", path, err)`). Path length is pre-checked and a stale socket was
  reclaimed; a bind failure past those guards is an unexpected OS condition
  with no single operator knob.
- **Chmod of the socket file** (`socket.go:165-167` — `fmt.Errorf("securing
  agent socket %q: %w", path, err)`). Chmod of a file the Runner itself just
  created failing is an OS anomaly, not deployment configuration.

**Local logging (secondary ask — recommended: yes, and for every error
result).** Today the failing-command path emits no Runner-side log: every
`execute` error arm returns `errorResult(id, err)` silently
(`dispatch.go:363,377,382,390,398,407,416,449`), so the diagnostic exists
*only* in the relayed `RunnerError.message` — its survival depends entirely
on relay fidelity, and a dropped/rewrapped message upstream erases it. The
dispatcher already carries a logger (`dispatch.go:78` — `log *slog.Logger`;
used at `dispatch.go:442` for the ConfigVersion signal), and `agentHost` logs
operational faults via `h.log` throughout (`host.go:273,344,852`).
Recommendation: convert `errorResult` to a dispatcher method that logs once
before returning —

```go
func (d *dispatcher) errorResult(ctx context.Context, id string, err error) *compassv1internal.SessionsRequest {
  // classify code as today, then log at a level chosen by class:
  //   context.Canceled / context.DeadlineExceeded -> skip (or Debug)
  //   INTERNAL                                     -> Error
  //   classified operator/client-fault codes       -> Warn
  // build and return the result frame
}
```

— logging error results, but at a **level chosen by class** rather than a flat
`ErrorContext` for all. The survives-independent-of-relay argument applies to
every command failure, and special-casing only the new code would leave
INTERNAL diagnostics (the ones hardest to debug from the admin side) unlogged.
But a flat Error level over-logs two noise cases (detailed in OQ-2): the
Provision-semaphore ctx.Done arm returns `errorResult(id, ctx.Err())`
(`dispatch.go:376-377`), so every provision queued at shutdown would emit an
ERROR "session command failed" for a routine cancel; and `errSessionUnknown` /
`errAlreadyRunning` are expected client-fault outcomes that would flatten
severity. So: Error for INTERNAL, Warn for the classified operator/client-fault
codes, skip/Debug for `context.Canceled` / `context.DeadlineExceeded`.
Concurrency is clean: all eight call sites are inside `dispatch.go`'s `execute`,
the dispatcher already logs from these per-command goroutines, and slog handlers
are concurrency-safe. Recorded as OQ-2 for Matt since the issue framed it as
a question.

## Plan

Sequencing: **T1 (proto) is the shared prerequisite** — T2 and T3 both
consume the generated `RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION`
constant. After T1 lands, T2 and T3 are order-independent (an unmapped code
degrades to today's CodeInternal, never breaks).

### T1 — proto: add the enum value (compass-runner lane; prerequisite)

Add to `enum RunnerErrorCode` (`proto/compass/v1/runner.proto:408-423`),
mirroring the RESOURCE_EXHAUSTED precedent (`runner.proto:418-422`):

```proto
  // A command failed on an operator-fault condition in the Runner's
  // deployment (a socket path over the AF_UNIX limit, socket-dir
  // permissions/ownership, an occupied socket path) -> Connect
  // FailedPrecondition. Distinguished from INTERNAL so an admin sees a
  // fix-the-deployment signal rather than a Compass bug.
  RUNNER_ERROR_CODE_FAILED_PRECONDITION = 5;
```

Then regenerate (`go/internal/gen/compass/v1/runner.pb.go` gains the
constant; the enum maps at `runner.pb.go:72-88` update mechanically).

- Interfaces: produces
  `compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION
  RunnerErrorCode = 5` in `go/internal/gen/compass/v1`.
- Acceptance: `buf lint` and `buf breaking` pass (additive value in the owned
  package, per `runner.proto:16-18`); generated code compiles; no other proto
  change in the diff.
- **Proto doc comment must disambiguate DIRECTION.** This new code flows
  Runner→Server→admin (an outbound operator-fault classification). It is
  distinct from the INBOUND Server→Runner `CodeFailedPrecondition` the Runner
  treats as a benign no-secrets / no-config surface signal
  (`go/internal/runner/host.go:340-343`,
  `go/internal/runner/config_materialize.go:150-153`). A maintainer grepping
  `CodeFailedPrecondition` finds both meanings, so the doc comment names its
  direction to keep them apart.

### T2 — server: mapping arm + test row (compass-server lane; CROSS-LANE)

Owned by compass-server; hand off after T1. Two edits:

1. `go/internal/runnerhub/commands.go:208-215` — add
   `case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION:
   code = connect.CodeFailedPrecondition` to the switch.
2. `go/internal/runnerhub/commands_test.go:30-35` — add the row
   `{"failed precondition", ...FAILED_PRECONDITION, connect.CodeFailedPrecondition}`
   to `TestRunnerErrorToConnectCodeMapping`.

- Interfaces: consumes the T1 constant; produces no new API — the existing
  `runnerErrorToConnect(e *compassv1internal.RunnerError) error`
  (`commands.go:206`) signature is unchanged.
- Acceptance (red-green): the test row added first is RED (the new code falls
  to the `default` → `CodeInternal` arm, `commands.go:213-214`); GREEN after
  the case arm. `go test -race ./internal/runnerhub/...` passes.

### T3 — runner: sentinel, wrapping, `errorResult` arm, logging (compass-runner lane)

1. Declare `var ErrOperatorConfig = errors.New(...)` (exported) in package
   `gateway` (`go/internal/runner/gateway/socket.go`, beside `sunPathMax`).
2. Wrap it (`%w`) into the qualifying error paths (Approach (3)):
   `socket.go:140-141` (too-long), `:145-147` (MkdirAll), `:150-152` (dir
   Chmod), `:197-198` (non-socket occupant), `:204-205` (wrong-owner socket),
   plus `config_materialize.go:205-210` (`ensureRoot`'s MkdirAll + Chmod, the
   same runtime-dir substrate — Approach (3)). The wrap appends the sentinel's
   TEXT to the diagnostic message the admin reads (`dispatch.go:465-467`
   `Message: err.Error()` → `commands.go:216`); this is not verbatim but is
   acceptable/self-documenting. Listen (`:159-161`), socket-file Chmod
   (`:165-167`), the reclaim inspection errors (`:192-194`, `:200-203`), and
   the staging/unpack MkdirAlls (`config_materialize.go:401,407`) stay
   unwrapped/INTERNAL.
3. Add the `errors.Is(err, gateway.ErrOperatorConfig)` arm in `errorResult`
   (`dispatch.go:455-462`) mapping to the T1 code, and convert `errorResult`
   to the logging dispatcher method per Approach (3) (all eight call sites,
   `dispatch.go:363-449`, become `d.errorResult(ctx, id, err)`).
4. Update the sentinel doc comment at `dispatch.go:69-70` ("Sentinel errors
   the host returns...") to note the gateway-owned sentinel as well.

- Interfaces: consumes the T1 constant and produces
  `gateway.ErrOperatorConfig error` (exported); `errorResult` becomes
  `func (d *dispatcher) errorResult(ctx context.Context, id string, err error) *compassv1internal.SessionsRequest`.
- Acceptance (red-green):
  - Unit RED-first: a dispatch-level test driving a `SessionHost` stub whose
    `Provision` returns
    `fmt.Errorf("serving agent socket for container %q: %w", name, gateway.ErrOperatorConfig)`
    asserts the result frame carries `RUNNER_ERROR_CODE_FAILED_PRECONDITION`
    (RED today: `errorResult` defaults to INTERNAL) and that the message text
    is preserved.
  - Gateway-side: extend the existing white-box socket tests
    (`socket_test.go:435-455` already asserts the too-long diagnostic text)
    to assert `errors.Is(err, ErrOperatorConfig)` on each wrapped path, and
    its *absence* on every deliberately-unwrapped path — not just Listen
    (`socket.go:159-161`), but also the socket-file Chmod (`:165-167`) and the
    two reclaim INSPECTION errors (`:192-194`, `:200-203`). All are deliberately
    unwrapped and all deserve the negative assertion: a future blanket-wrap
    refactor is exactly the over-classification bug this guard catches.
  - Logging: assert the level-by-class contract (per Matt's OQ-2 ruling) via a
    test `slog.Handler` — one record per error result at the class-appropriate
    level, and specifically PIN the shutdown/`ctx.Err()` behavior (the
    Provision-semaphore `ctx.Done` arm, `dispatch.go:376-377`) so the
    noise posture is a contract, not an accident (pattern availability:
    dispatcher tests already inject `d.log`).
  - `go test -race ./internal/runner/...` passes.

### T4 — end-to-end assertion (either lane, after T2+T3)

One integration-level test proving the full chain: a Runner whose provision
fails on a wrapped operator-fault error surfaces to the Hub caller as
`connect.CodeFailedPrecondition` with the diagnostic text intact — the
end-to-end contract RIG-1442 names (RED before the pair lands, GREEN after).
Candidate home: the runnerhub test harness that already drives real
dispatch/relay round-trips (e.g. beside `deliveryarm_test.go`, which asserts
RunnerError codes over the relay at
`go/internal/runnerhub/deliveryarm_test.go:414-416`).

- Interfaces: consumes both lanes' changes; produces no API.
- Acceptance: `connect.CodeOf(err) == connect.CodeFailedPrecondition` and the
  message contains the socket diagnostic (which now also carries the appended
  operator-fault sentinel phrase, per Approach (3)). If the e2e harness observes the
  Runner-side log, it may also assert the shutdown/`ctx.Err()` noise posture
  end-to-end (per OQ-2's ruling).

### Ledger scope — this record is not ledgered

The `DL-<n>` design-decision ledger (`docs/designs/product/DECISIONS.md`) and
its `design-ledger-gate` CI check govern the **product** design corpus only:
the gate globs `docs/designs/product/**` and resolves every Record-cell link
relative to `docs/designs/product/`, rejecting a `..` climb-out
(`tools/design-ledger-gate/index.ts`). This is a **platform** implementation
record (`docs/designs/platform/`, alongside `compass-runner-concurrent-dispatch`
and `compass-runner-arbitrary-uid`), so it owes no ledger row and carries no
`Ledger-impact:` line — matching every prior platform runner record, none of
which is ledgered.

## Tasks

- [ ] T1 (compass-runner, prerequisite): `RUNNER_ERROR_CODE_FAILED_PRECONDITION = 5`
      in `proto/compass/v1/runner.proto` + regen; buf lint/breaking green.
- [ ] T2 (compass-server, CROSS-LANE hand-off): `runnerErrorToConnect` case
      arm → `connect.CodeFailedPrecondition` + red-green row in
      `TestRunnerErrorToConnectCodeMapping`.
- [ ] T3 (compass-runner): `gateway.ErrOperatorConfig` sentinel; wrap the
      operator-fault paths in `socket.go` plus `config_materialize.go`
      `ensureRoot` (MkdirAll + Chmod); `errors.Is` arm in `errorResult`;
      `errorResult` → level-by-class logging dispatcher method; red-green unit
      + gateway (incl. over-classification negatives) + logging tests, `-race`
      clean.
- [ ] T4 (post T2+T3): end-to-end red-green test — operator-fault provision
      surfaces as `CodeFailedPrecondition` with diagnostic intact.

## Open Questions

- **OQ-1 — enum value + Connect code. RULED (Matt, 2026-08-05): FAILED_PRECONDITION → CodeFailedPrecondition.**
  `RUNNER_ERROR_CODE_INVALID_ARGUMENT` → Connect `InvalidArgument` vs
  `RUNNER_ERROR_CODE_FAILED_PRECONDITION` → Connect `FailedPrecondition`.
  **Recommendation: FAILED_PRECONDITION.** The client's request is
  well-formed; the fault is deployment state (`--runtime-dir` length, dir
  permissions/ownership, an occupied socket path — `socket.go:119-122` names
  RuntimeDir as "the operator-supplied variable"). FailedPrecondition's
  contract — fix the system state, then retry the same request — matches the
  remedy exactly; InvalidArgument would tell the caller to fix a request that
  has nothing wrong with it. **In-repo precedent seals it**: FailedPrecondition
  is already this codebase's deployment-misconfiguration vocabulary —
  `errNotAgentAccount` maps to `CodeFailedPrecondition` NOT
  `CodeInvalidArgument` because "the difference is load-bearing at the relay"
  (InvalidArgument = routine per-frame refusal, FailedPrecondition = a contract
  defect logged as misconfiguration, `go/internal/comms/agent_caller.go:71-79`);
  the no-secrets / no-config-surface handlers answer `CodeFailedPrecondition`
  for absent deployment state (`go/internal/runnerhub/handler.go:52-56`,
  `:253-254`, `:288-292`, `:324-326`); and `store.ErrFailedPrecondition` maps
  to `CodeFailedPrecondition` (`go/internal/comms/context.go:52`).
  Choosing InvalidArgument would put the Runner's operator-fault class at odds
  with the repo's own comms-relay classification. Dismissed third options:
  reusing `RESOURCE_EXHAUSTED = 4` is semantically wrong AND collides with the
  delivery-refusal counter (`go/internal/runnerhub/router.go:50-54` counts
  RESOURCE_EXHAUSTED among refused delivers); `CodeUnavailable` is wrong —
  nothing here is transient. An `OPERATOR_FAULT` name is also rejected: every
  current value names its wire semantics and lets the doc comment carry the
  rationale. The record is written against
  FAILED_PRECONDITION throughout; a ruling for INVALID_ARGUMENT changes only
  the value name, the Connect code in T1/T2, and the doc comments.
- **OQ-2 — local logging scope + level. RULED (Matt, 2026-08-05): log ALL error
  results, level-by-class.** The issue asks whether the Runner should log the operator
  diagnostic locally so it survives independent of relay fidelity.
  Recommendation: yes, and for *every* error result, not just operator-fault
  ones — the errorResult path is entirely unlogged today (all eight arms return
  silently, `dispatch.go:363-449`) while the dispatcher already carries
  `log *slog.Logger` (`dispatch.go:78`); the survives-independent-of-relay
  argument applies to every command failure, and INTERNAL failures are the ones
  an admin can least diagnose from the relayed message alone. **But a flat
  `ErrorContext` for all over-logs two noise cases**, so the recommendation is
  LEVEL-BY-CLASS, not a flat level:
  - *Shutdown noise*: the Provision-semaphore arm returns
    `errorResult(id, ctx.Err())` on `ctx.Done` (`dispatch.go:376-377`), so
    every provision queued at shutdown would emit an ERROR-level "session
    command failed" for a routine cancel. Skip or Debug
    `context.Canceled` / `context.DeadlineExceeded`.
  - *Severity flattening*: `errSessionUnknown` / `errAlreadyRunning` are
    expected client-fault outcomes (e.g. a Status probe for a gone session);
    ErrorContext for those pollutes the very signal this OQ exists to create.
  Recommended levels: **Error** for INTERNAL, **Warn** for the classified
  operator/client-fault codes, **skip/Debug** for context-cancellation.
  Concurrency is clean: all eight call sites are inside `dispatch.go`'s
  `execute`, the dispatcher already logs from these per-command goroutines, and
  slog handlers are concurrency-safe. The ruling adopts log-all
  level-by-class as specified above.
- **OQ-3 — `reclaimStaleSocket` refusals in scope? RULED (Matt, 2026-08-05): INCLUDE both.** The brief names
  MkdirAll/Chmod/Listen as the sibling paths; this record additionally
  classifies the two fail-closed reclaim refusals (`socket.go:197-198`,
  `:204-205`) as operator-fault, since both are deliberate refusals over
  on-disk state only an operator can clear.
- **OQ-4 — `ensureRoot` in the wrapped set? RULED (Matt, 2026-08-05): INCLUDE.**
  Beyond socket.go there is a same-class
  operator-fault sibling: `ensureRoot`'s MkdirAll + Chmod of
  `<RuntimeDir>/containers/<name>/config`
  (`go/internal/runner/config_materialize.go:205-210`) operates on the SAME
  operator-supplied runtime-dir substrate as the socket dir
  (`configMaterializerFor`, `go/internal/runner/host.go:804-812` —
  "mirroring the per-container agent-socket layout"), and its error
  `%w`-chains to `errorResult` via the Provision arm
  (`go/internal/runner/host.go:184-190` — `fmt.Errorf("materializing agent
  config: %w", err)`). The sentinel is importable there (package `runner`
  already imports `gateway`, `go/internal/runner/host.go:828`).
  **Ruled: INCLUDE** ensureRoot's two paths on the same footing as the
  socket-dir MkdirAll/Chmod — leaving it out violates the record's own
  classification rule, since ensureRoot is indistinguishable in kind. The
  staging/unpack MkdirAlls (`config_materialize.go:401,407`) stay INTERNAL
  (bundle-content-driven, murkier).
