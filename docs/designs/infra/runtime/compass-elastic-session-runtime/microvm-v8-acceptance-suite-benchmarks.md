# microVM Runner V8 — isolation / contract / failure-mode acceptance suite + benchmarks

Status: PROPOSED — details the V8 milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V8,
microvm-runner.md:600-621; § Approach (c) peer-CID authentication,
microvm-runner.md:158-164; (e) preflight/hard-fail, microvm-runner.md:211-236;
(h) boot-latency/RSS budget, microvm-runner.md:278-287) and the frozen D3
hard-fail decision (microvm-runner.md:693-703).
Authoritative issue scope: RIG-2499.

Ledger impact: none. V8 is the terminal detailing record of the V-spine: it
proves properties the parent already froze (D3 hard-fail, §(c) peer-CID
refusal, §(d) isolation, §(f) failure handling) and measures the Q-budget the
parent explicitly defers to data ("Set from V2a/V8 measurements on real
hardware, not invented here", microvm-runner.md:839-842). Nothing here is a new
cross-cutting decision; `docs/designs/DECISIONS.md` is untouched.

Dependency position: V6 (RIG-2497, PR #912) and V7 (RIG-2498, PR #931) are in
flight, not merged. V8 is strictly after both: it re-runs V6's isolation suite
under the full backend, escalates V7's death tests through the session
lifecycle, and asserts V7's metric set. Citation convention: an unprefixed
`file.go:N` cites main; V6-branch and V7-branch claims carry `PR #912` /
`PR #931` prefixes.

## Problem / Intent

Every V-milestone so far proves its own slice in isolation; nothing yet proves
the assembled backend. The parent demands exactly that: "The proving suite the
parent's I1 test cycle demanded (design.md:596-601), run against the real
backend on KVM hardware. This is the task that PROVES inter-tenant isolation
rather than exercising the happy path" (microvm-runner.md:602-604), producing
"the CI job (KVM-labeled runner) and the benchmark report feeding the
boot-budget measurement (Q-budget)" (microvm-runner.md:606-608).

Three gaps V8 closes:

1. **The isolation properties are proven per-slice, and cross-tenant only
   for the volume surface.** V6 proves one guest cannot escape its own
   volume (PR #912 `microvm_isolation_microvm_test.go:302-304`,
   `TestMicroVMVolumeTraversalConfined`) and already boots **two** sessions
   to prove A cannot reach B's volume (PR #912 `:391-395`,
   `TestMicroVMCrossSessionVolumeUnreachable`, with own-canary positive
   controls at `:410-421`). What no test covers is the rest of the A→B
   surface: B's vsock sockets, and the host metadata/network from guest A
   (cycle 1) — so V8's cycle-1 work is an escalation of V6's volume leg plus
   genuinely new network and vsock legs, not a first two-session test. The
   in-guest escalation probe (cycle 8) has its guest-side gate landed —
   guestd refuses any non-host peer CID ("The supervisor accepts control
   connections ONLY from the host; any other peer CID (including the
   in-guest loopback CID 1, VMADDR_CID_LOCAL) is refused before a single
   HTTP byte is read", `go/internal/guestd/vsock.go:25-29`) — but only
   hermetically (`guestd/supervisor_test.go:712-715` table-tests
   `peerAllowed`, never a real agent-uid dial over AF_VSOCK inside a booted
   guest).

2. **The contract and failure-mode proofs stop below the session lifecycle.**
   The shared `ContainerRuntime` contract suite runs on the microVM backend
   (`contract_microvm_test.go:34-69`), but the S1-frozen seam contract the
   parent's design demands — "a session and a burst both boot on the microVM
   runtime and pass the S1/C3 contract tests unchanged" (design.md:596-597)
   — is not exercised through `AgentRuntime.Launch` and the gateway on this
   backend (the podman analog, `lifecycle_test.go:5-16`, is
   `//go:build podman`). Likewise V7 designs mid-session VMM-death handling
   (PR #931 record §(c)) at the runtime layer; nothing proves the gateway
   streams above it break cleanly (cycle 5). The wedged-boot path (cycle 4)
   is covered *less* than its doc comment suggests. Main's test body
   (`microvm_lifecycle_microvm_test.go:62-118`) asserts exactly three
   things: `Start` returns an error (`:98-100`), `session.guestExec` is nil
   (`:108-110`), and `session.runtimeDir` is gone after `Remove`
   (`:111-116`). That is **fail-closed Start**, not orphan-freedom: the
   words "no orphan processes" appear only in the DOC COMMENT
   (`:57-61`), and the body asserts nothing about any process — no pid is
   read, no liveness is checked. So V8's cycle-4 delta is wider than a
   drafting of this record previously claimed: W4 must ADD **the
   orphan-freedom assertion itself, pidfile-identity-verified** (a recycled
   pid must not satisfy the check), on top of the caller-deadline mid-boot
   cancellation, which nothing covers either.

3. **The Q-budget has no benchmark or report.** `TestMicroVMQBudget` logs boot
   latency and PSS informally ("INFORMATIONAL spike output, NOT a boot gate",
   `contract_microvm_test.go:71-77`), but there is no container-baseline
   comparison, no machine-readable report, and no CI artifact an operator can
   set the Q-budget from (cycle 7). The record invents no numbers — it
   specifies how they are measured, emitted, and land.

The central design risk is **vacuity**: a proving suite whose negative
assertions can pass while the property is false certifies isolation that is
not there. Three instances of exactly that shape occurred in PR #912's review
cycle (a guest `grep` that does not exist, so the sweep never searched; an `ls`
row asserting output `ls` never prints; assertions matching text on either
side of a corrupted message). Every negative assertion in this plan therefore
carries (a) its could-pass-while-false analysis and (b) the mutation that
proves it cannot — per cycle, as a first-class deliverable.

## Approach

V8 adds no production feature: it is tests, a benchmark, and a CI lane. Every
subsection resolves one concern the parent's V8 plan leaves to detailing;
every genuine fork is also listed in `## Open Questions`. Where a fork has a
workable default the body designs against the recommended option; where it
does not — OQ-8's vsock-identity shape and OQ-9's cycle-8 exit code, both of
which turn out to have **no** implementable drafting on today's tree — the
affected assertion is marked pending that OQ instead, because writing a
plausible-looking assertion that cannot pass is how a proving suite ends up
certifying nothing.

### (a) Suite placement and the cycle map

The acceptance tests live where every existing KVM suite lives — files tagged
`//go:build microvm && unix`, opening with `microvmtest.Require(t)` (the
two-tier rule the harness package states: "an integration test that touches
KVM/cloud-hypervisor/virtiofsd/passt or the guest image carries
`//go:build microvm && unix` and calls microvmtest.Require(t) first",
`go/internal/microvmtest/microvmtest.go:22-24`). **No new package** — the
suite drives production types where they are defined — but that means *two*
existing packages, not one, and which one is forced by the import graph
rather than chosen:

- **`package runtime`** for everything that drives `MicroVMRuntime` /
  `AgentRuntime` directly: W1's isolation legs, W3's lifecycle + egress
  legs, W4's boot-timeout legs, W6's benchmark.
- **`package runner`** for any leg consuming `internal/runner` or
  `internal/runner/gateway` symbols: W3's gateway-liveness leg and W4's
  cycle-5 gateway/`AgentStream` death legs. This is not a preference. Both
  packages import `internal/runtime` — `gateway/socket.go:31` imports
  `"github.com/RigelBuild/compass/go/internal/runtime"`, and runner's
  `AgentStream` is built on `runtime.StreamingExecSpec`
  (`go/internal/runner/agent_exec.go:77-78`) — so a `package runtime` test
  file importing either is an **import cycle and will not compile**. The leg
  also belongs there conceptually: `AgentRuntime.Launch` never serves the
  gateway; the runner host does
  (`listener, err := gateway.Serve(ctx, endpoint, name, deps)`,
  `go/internal/runner/host.go:824`), so a runtime-package test has no
  gateway socket to probe even in principle. The precedent is settled — the
  existing gateway-over-vsock e2e is already `package runner`
  (`go/internal/runner/e2e_vsock_gateway_microvm_test.go:3`), and the new
  legs sit beside it. (An external `runtime_test` package is the other
  compile-legal route, but it forfeits `e2eConfig`/`isolationSession`/
  `guestSh`, which W1 needs.)

The existing CI package-ran guard (`.github/workflows/ci.yml:836-849`) then
picks up both packages automatically, because it derives the package set from
`microvmtest.Require` call sites ("the package list is the set that actually
calls microvmtest.Require", `ci.yml:821-822`). **What that guard does not
do** — load-bearing for every "the guard enforces it" claim below — is see
individual tests: it is package-granular, and `internal/runtime` and
`internal/runner` both already hold many `Require` callers, so a V8 file that
silently leaves the sweep (a misspelled build tag) removes neither its
package from the set nor its package's `ok` line. The guard stays green while
the acceptance property was never asserted. W7 therefore adds a **per-test
presence check**, and it — not the guard — is what ratifies that a named
suite ran. Because that check is the sole ratifier for cycles 2 and 3, it
must not itself be skip-blind: it requires `--- PASS: <name>` and REJECTS
`--- SKIP:` at any depth under a listed name, against a small allowlist of
rows permitted to skip (today: W1's metadata leg). A `=== RUN`-level
acceptance would credit a skipped test, and a parent-only `--- PASS` would
credit a parent whose every subtest skipped — both verified shapes of
`go test -v` output; W7 carries the mechanism and its mutations.

The eight RIG-2499 cycles map onto tasks as follows. Cycles marked *escalate*
consume a merged or in-flight lower-milestone suite and re-prove it at the
assembled-backend layer; cycles marked *new* have no existing coverage.

| Cycle | Property | Status on main | Task |
| --- | --- | --- | --- |
| 1 | Inter-tenant probe (volume, vsock, host fs, host metadata/net) | *escalate + new* — PR #912 already boots two sessions for the volume surface (`microvm_isolation_microvm_test.go:391-395`, `TestMicroVMCrossSessionVolumeUnreachable`) and confines a single session's traversal (`:302-304`); net-new are the host-network legs and the vsock leg (OQ-8) | W1 |
| 2 | Egress fail-closed inside the guest netns | *escalate* — `egress_inguest_microvm_test.go:37-42` already runs under the full backend and names itself V8 row (2) | W3 |
| 3 | S1 contract tests pass unchanged | *escalate* — `contract_microvm_test.go:34-69` covers `ContainerRuntime`; the `AgentRuntime.Launch` layer is podman-only (`lifecycle_test.go:1`) | W3 |
| 4 | Boot timeout killed + cleaned | *escalate + new* — `microvm_lifecycle_microvm_test.go:62-118` proves the corrupt-rootfs deadline is **fail-closed** (Start errors `:98-100`, no exec client `:108-110`, runtime dir removable `:111-116`); it asserts NOTHING about processes — "no orphan processes" is doc-comment text only (`:57-61`), read by no assertion. V8's delta is therefore the orphan-freedom assertion *itself*, pidfile-identity-verified, plus the caller-deadline cancel leg | W4 |
| 5 | Mid-session VMM death under the session lifecycle | *new* at this layer — V7 (PR #931 §(c)) designs runtime-layer detection; gateway streams above it are unproven | W4 |
| 6 | KVM-absent hard-fail (D3) | *escalate* — `microvm_preflight_test.go:82-88` unit-tests the axis; no acceptance-level assertion of the capability-naming error text | W5 |
| 7 | Boot-latency + RSS benchmark vs container baseline | *new* — `TestMicroVMQBudget` logs one uncompared sample (`contract_microvm_test.go:71-77`); the comparison needs the single-process bench invocation of § Approach (g), without which the baseline is structurally absent from the KVM lane | W6 |
| 8 | In-guest escalation probe refused (peer-CID) | *new* — guestd's gate is hermetic-only (`go/internal/guestd/vsock.go:25-29`, `supervisor_test.go:712-715`); which property the live probe can assert is OQ-9 | W2 |

### (b) The anti-vacuity discipline: positive control + proving mutation

Every negative assertion in this suite follows one two-part discipline, stated
here once and instantiated per task:

1. **Positive control.** A negative probe ("A cannot reach X") must be paired
   with a positive discriminator run through the *same mechanism* ("A CAN
   reach its own X the same way"), so a probe that silently broke — a missing
   guest binary exiting 127, a dead socket, a timeout for an unrelated reason
   — fails the control instead of passing the negative. The guest toolbox
   makes this mandatory, not optional: the image ships bash 5.3, gawk, ls,
   cat, stat and **no `grep`, `find`, or `sed`** (the rootfs closure links
   exactly `nft`, `getent`, `awk`, `sh` beyond the toolchain set,
   `guest-image/default.nix:325-328`), so a guest script written from host
   reflexes dies with exit 127 having probed nothing.
2. **Proving mutation.** For each negative assertion the task names the
   specific production-code or harness mutation under which the test MUST
   fail — break the property, watch the test go red, revert. Mutations are
   run once during the task's development and recorded in the PR description
   (a checklist per assertion), not installed as a permanent CI mutation
   harness (see OQ-7).

Guest-side scripting rules, inherited by every task (also in
`## Global Constraints`): batched gawk scans must open with
`BEGINFILE { if (ERRNO) { nextfile } }` and must not blanket-redirect stderr,
because gawk treats an unopenable input as fatal — it aborts the invocation,
skips `END`, and exits 2, silently abandoning the rest of the batch;
filenames a test reasons about positionally are zero-padded (`f%03d.txt`)
because bash globs sort lexicographically.

### (c) Cycle 1 — the inter-tenant probe boots two real sessions

W1 boots **two concurrent sessions A and B** on one `MicroVMRuntime` with
disjoint workspace volumes, then probes every A→B surface from inside guest
A. Two-session probing is not new — PR #912 already does it for the volume
surface (see below); W1's net-new surfaces are the host network and the
vsock leg, and its delta on the rest is the single-runtime topology.

- **B's volume — an escalation of V6's leg, not a new probe.** PR #912
  already boots two sessions for exactly this property:
  `TestMicroVMCrossSessionVolumeUnreachable` asserts "two live sessions, and
  guest A cannot reach guest B's volume by any path, nor write into it"
  (PR #912 `microvm_isolation_microvm_test.go:391-395`), with
  A-reads-own-canary and B-reads-own-secret positive controls (PR #912
  `:410-421`) and an awk content sweep covering B's volume parent (PR #912
  `:446-464`). W1 consumes and extends it rather than re-deriving it (the
  Plan's preamble forbids forking V6's symbols, and the test already takes
  caller-chosen names via `isolationSession(t, env, name)`, PR #912 `:85`).
  The delta W1 adds: the one-runtime topology, and the symlink-in-A's-volume
  shape (V6's sharpest probe form, PR #912 `:317-321`). Pass condition
  unchanged — B's canary body appears in no output AND a host-side snapshot
  of B's volume is unchanged after A's write attempts, with the same `cat`
  reading A's own canary through the same script path as the control.
  Vacuity analysis: the probe could pass while isolation is broken if the
  script never ran (127) or probed the wrong path — both fail the positive
  control. Proving mutation: launch B's virtiofsd rooted at the common
  parent directory of both volumes (one-line harness change to the
  shared-dir argument); A's read of B's canary then succeeds and the test
  MUST go red.
- **B's vsock surface — pending OQ-8 (load-bearing).** Under
  cloud-hypervisor's hybrid vsock a guest dial terminates at its *own*
  VMM's socket muxer: the host end is a per-session AF_UNIX path ("the
  hybrid transport addresses by socket path, not CID",
  `go/internal/runtime/microvm_lifecycle.go:44-45`; the guest-reachable
  listener is `vsockSocket + "_" + port`,
  `go/internal/runtime/microvm/config.go:46-48`), and both guests are given
  the same CID (`const guestVsockCID uint32 = 3`,
  `microvm_lifecycle.go:46`) — so cross-VM vsock is structurally absent,
  and B has **no vsock address by which A could name it**. That makes
  "probe B's vsock port" a question about interpretation rather than a test
  to write, and the first drafting of this leg — dial CID 2 on the control
  port (1024), issue `Health`, assert the call succeeds and the echoed
  `boot_nonce` is A's — is not implementable: nothing binds the host path
  `vsock.sock_1024` (port 1024 is direction-reversed — guestd listens
  in-guest and the host dials it, `go/internal/guestd/vsock.go:75-77`,
  `go/internal/runtime/microvm/dial.go:51`; the only host-side listener at a
  suffixed path is the gateway at 1025,
  `go/internal/runner/e2e_vsock_gateway_microvm_test.go:24`), bash has no
  AF_VSOCK vehicle for an h2c `Health` RPC, and the drafted mutation
  (pointing A's config at B's `VsockSocket`) reddens main's host-side nonce
  check during setup (`microvm_lifecycle.go:441-445`) rather than the
  in-guest probe. **OQ-8 carries the fork** — re-aim at CID 2:1025 where a
  host listener exists and assert A reaches A's own gateway (recommended),
  or concede the structural argument and assert only that A's dial surface
  is bounded. W1 implements neither shape until it rules; the structural
  facts above stand either way.
- **Host filesystem.** V6's traversal probes re-run in the two-session
  topology (dot-dot, absolute path, symlink escape), asserting from both
  sides (guest output + host snapshot). Mutation: same virtiofsd-root
  widening as above.
- **Host network — the gateway address.** From A, bounded `/dev/tcp`
  connects (the V3 probe mechanism,
  `egress_inguest_microvm_test.go:21-26`) to the passt gateway address
  `10.0.2.2` (`go/internal/runtime/microvm/launch.go:33`) on a host-bound
  listener port **the test itself opens**, which is what makes the row
  non-vacuous in its *primary* failure mode: "no listener" is excluded by
  construction, so a failed connect is a blocked connect. The session is
  armed with a *permissive-but-not-host* egress policy, so the deny the
  probe observes **is the in-guest nft default-deny ruleset** — passt's
  launch argv (`launch.go:187-195`) passes no `--no-map-gw`, and passt maps
  the host onto the guest-visible gateway address by default.

  **What this row certifies, stated as what it proves.** The claim is
  *"the nft default-deny rule is armed and enforcing on the guest's
  outbound path to `10.0.2.2`"* — **not** "the host is unreachable from the
  guest". The stronger sentence would need a layer beneath the firewall,
  and there is none today; OQ-1 (load-bearing, Matt's) asks whether to add
  `--no-map-gw` as that structural second layer, and only OQ-1 option (i)
  would deliver host-unreachability. Until it rules, this row certifies one
  layer, and says which one.

  **Could pass while false — two hazards, the second dominant for the
  mutation.** (1) The no-listener hazard, closed by construction: the test
  opens the listener it dials, so a refused connect cannot be
  want-of-a-listener. (2) The host-mapping hazard, which is *not* closed by
  construction and which the row must not assume away: whether a guest
  connect to `10.0.2.2` can reach a host listener at all depends on passt's
  gateway mapping, and `--no-map-gw` is **implied** — not merely available
  — under conditions this record cannot assert about a CI runner. The
  pinned passt (2025_09_19) documents both halves: `--no-map-gw` is
  "Don't map gateway address to host" (`passt --help`, run this session),
  and it is "Implied if there is no gateway on the selected default route,
  or if there is no default route, for any of the enabled address families"
  (passt(1)). On a runner whose default route differs, the mapping is off
  without the flag, the negative assertion passes for a reason unrelated to
  nft, and — worse — the proving mutation below cannot redden either.

  **Proving mutation, made self-verifying so it cannot be recorded as
  passed when it never discriminated.** Arm the session with an egress
  policy allowlisting `10.0.2.2`, and — because the accept rule that
  produces is destination-address-only, not port-scoped
  (`nft add rule inet compass_egress output ip daddr @allow4 accept`,
  `internal/runtime/egress.go:130`, fed by an arm-time in-guest
  `getent ahostsv4 %s | awk '{print $1}'` resolution, `egress.go:88-103`) —
  keep the dialed port the single test-opened one so the observation stays
  scoped to it. The mutation run then asserts **two** things, in order:
  1. the allowlisted connect **reaches the test's listener** (the harness
     observes the accepted connection on the host side, not merely a
     non-error in the guest) — this is the mutation's own positive control,
     and it is what proves host-mapping is in effect on this box;
  2. the row's assertion goes red.

  If (1) fails the mutation is recorded as **"could not discriminate on
  this host — passt host-mapping not in effect"**, never as a passed
  mutation and never as a green row: an implied-`--no-map-gw` runner
  surfaces as a declared gap, the same posture the metadata row takes for
  its precondition. Recording the mutation as passed without observing (1)
  is exactly the failure this clause exists to prevent — the connect can
  fail for a reason that has nothing to do with the allowlist. Observation
  (1) is also the datum OQ-1's answer depends on.
- **Host network — the metadata endpoint.** The `169.254.169.254:80` probe
  needs its own vacuity analysis, because it fails the test the row above
  passes. A guest connect to the metadata IP rides passt's ordinary
  outbound path as a host-originated connect, so on any box with no
  metadata service — every dev box, and this one — "must fail" passes **for
  want of a listener even with the entire nft ruleset deleted**. That is
  the classic no-listener vacuity, and the named positive control does not
  discriminate it: an allowlisted external IP proves egress works, not that
  the metadata path could have succeeded. Nor can the test create the
  listener, as it can at `10.0.2.2` — the address is the runner's, not the
  test's. The row therefore runs **only when its host-side precondition
  holds**: the test probes `169.254.169.254:80` from the host first, and
  where it does not answer the leg skips with that reason recorded, because
  a declared gap beats a vacuous pass. Where it does answer (a cloud
  runner), the mutation is the same allowlist-widening as the gateway row,
  aimed at `169.254.169.254`: the connect MUST then succeed and the row
  MUST go red. On boxes where it skips, metadata protection rests on the
  default-deny the gateway row proves plus OQ-1's structural layer — stated
  as a corollary rather than asserted vacuously.

### (d) Cycle 8 — the escalation probe needs a real AF_VSOCK dial

The property (§ Approach (c) of the parent): "an agent-uid process inside the
guest dials the supervisor vsock port over loopback (CID 1) and is refused"
(microvm-runner.md:618-621). guestd's gate exists and is hermetically proven
("any other peer CID (including the in-guest loopback CID 1, VMADDR_CID_LOCAL)
is refused before a single HTTP byte is read",
`go/internal/guestd/vsock.go:25-29`), but no test performs the dial from a
real agent-uid process inside a booted guest.

Bash cannot open an AF_VSOCK socket (`/dev/tcp` is AF_INET only), so the
probe needs a vehicle. Per OQ-2 the design uses a **test-built static probe
binary**: a tiny Go program under
`go/internal/runtime/testdata/vsockprobe/main.go`, built by the test,
staged into the session's workspace share, and exec'd inside the guest as
the agent uid. No production surface grows. Three parts of that contract are
load-bearing enough to state rather than leave to the executor, because each
is a wall on first run:

- **Built by explicit path only — and therefore invisible to every
  repo-wide gate.** The Go tool **ignores `testdata` directories when
  matching package patterns**, so the probe is not a package `./...` can
  see. Verified this session in a scratch module: `go list ./...` omits it
  entirely, `go list ./internal/runtime/testdata/...` reports
  `matched no packages`, and `go build ./...` / `go vet ./...` therefore
  build and vet nothing there — while the explicit-path build succeeds:

  ```sh
  CGO_ENABLED=0 GOOS=linux go build -o <dst> ./internal/runtime/testdata/vsockprobe
  ```

  `testdata` is still the right home (it keeps a test-only binary's source
  out of the shipped package graph), but the consequence must be recorded:
  `go vet ./...`, the moon battery, and golangci-lint never see this file,
  so it can rot to non-compiling and **nothing reds until the KVM lane
  runs**. W2 therefore carries a cheap compile guard in its **untagged**
  half — running the same explicit-path `go build` into `t.TempDir()` and
  failing on a compile error — so rot reds in the default lane, on a
  KVM-less box, within seconds. § Global Constraints records the carve-out.
  `CGO_ENABLED=0` is what makes the binary static (no dynamic loader in the
  guest's minimal rootfs to satisfy) and `GOOS=linux` is required because
  the host may not be linux; both are part of the contract, not defaults.
- **Staged through the ONE `/workspace` share, never a second mount.** The
  backend refuses any spec whose single mount targets a `ContainerPath`
  other than `/workspace`, refuses a read-only mount, and refuses more than
  one mount outright with `UnsupportedMountError`
  (`microvm_lifecycle.go:313-330`). So the probe is staged by **writing the
  built binary into the existing `/workspace` host directory (mode 0755)
  before `Start`** — it rides the share the guest already mounts. An
  executor reaching for a dedicated read-only mount for a test-only binary
  gets `UnsupportedMountError` at `Create`.
- **A missing or unstartable probe is an `Exec` ERROR, not exit 126/127.**
  guestd does not surface an unresolvable program as an exit code:
  `resolveProgram`'s error is wrapped into
  `connect.NewError(connect.CodeInternal, …)` by `buildChild`
  (`internal/guestd/supervisor.go:600-614`), and a path-form argv like
  `/workspace/vsockprobe` bypasses PATH resolution entirely
  (`resolveProgram`: a name containing a slash "is used as given",
  `supervisor.go:643-646`) and then fails at `cmd.Start()` — also
  `CodeInternal`. Host-side that is a returned error, not an
  `ExecOutput.ExitCode`: `Exec` maps a transport/refusal failure to
  `ExecOutput{}, err` (`microvm_lifecycle.go:486-496`, whose doc states "a
  guest refusal or transport failure is an error" while "a non-zero exit is
  a SUCCESSFUL call", `:470-475`). W2 therefore asserts **`err == nil`
  first** — a transport/spawn error is a harness fault and must `t.Fatal`,
  matching V6's `guestSh` posture ("A transport/refusal error is fatal; a
  NON-ZERO EXIT IS NOT", PR #912
  `microvm_isolation_microvm_test.go:111-123`) — and only then
  discriminates `0`/`2`/`3` on `ExecOutput.ExitCode`. The exit-code
  contract accordingly drops its 126/127 arm: those codes are not
  observable on this path, and a missing probe is caught by the `err == nil`
  assertion instead.

The test asserts, in order:

1. **Positive control:** the probe dials CID 2 (host) on the control port
   and the connection completes — proving the probe binary, the vsock
   device, and the agent-uid dial path all work.
2. **The refusal — pending OQ-9.** The probe dials CID 1 (loopback) on the
   same port. Exit codes distinguish connected-and-served (0) /
   connected-then-closed (2) / connect-refused-or-no-path (3), so the
   assertion pins the *mechanism* rather than "some failure" — but **which**
   code is the acceptance assertion is not settled here.

   Drafting it as exit 2 (guestd's refuse-before-first-byte) would be red on
   today's image, and for a reason that strengthens rather than weakens the
   isolation claim: a CID-1 (`VMADDR_CID_LOCAL`) connect needs the kernel's
   `vsock_loopback` transport, and the initrd's `bootModules` list loads
   only `virtio_pci`, `virtio_blk`, `erofs`, `overlay`, `virtio_net`,
   `virtiofs`, `vmw_vsock_virtio_transport`, `af_packet`
   (`guest-image/default.nix:153-162`), with no loopback entry in the
   checked `bootModuleConfigs` list (`guest-image/default.nix:171-180`).
   With no local transport registered the connect fails at the socket layer
   and never reaches guestd's `Accept` — exit 3, not 2.
   [INFERENCE: the exact errno; the transport-absence mechanism is standard
   `af_vsock` behavior — a connect to a local CID requires a registered
   local transport.] The guest cannot self-load the module either
   (`modprobe` needs root; the backend advertises `refusesRootExec: true`,
   `contract_microvm_test.go:49`). So the supervisor is today
   **structurally unreachable** from any in-guest process, which is strictly
   stronger than the parent's "refused" — and making the parent's literal
   wording observable would mean *adding* the transport, widening the very
   escalation surface this cycle certifies. OQ-9 (load-bearing) carries that
   fork; W2 implements whichever exit code it rules for.

Vacuity analysis: "dial fails" alone would pass if the vsock device were
missing, the port wrong, or the probe binary broken — all caught by the
positive control against CID 2, and further separated by the exit-code
discrimination: `3` for absent transport or refused connect, `2` for
accepted-then-closed, `0` for connected-and-served. A missing or unstartable
probe is deliberately **not** in that table: it never reaches an exit code,
surfacing instead as an `Exec` error (guestd's `CodeInternal`, see above),
which W2's `err == nil` assertion catches as a harness fault before any
exit-code comparison runs. Proving mutation, per OQ-9's
ruling: under (a) — asserting exit 3 — a guest image built WITH
`vsock_loopback` MUST flip the probe to exit 2, reddening the assertion and
simultaneously proving both the exit-code discrimination and that it is
guestd's gate closing the connection once a transport exists; under (b) —
asserting exit 2, transport present — a guest image whose `peerAllowed`
returns `true` unconditionally (`go/internal/guestd/vsock.go:36-38`) makes
the CID-1 dial complete an HTTP exchange (exit 0) and the test MUST go red.

### (e) Cycles 2-5 — contract and failure modes at the assembled layer

- **Egress (cycle 2)** is already proven at the runtime layer under the full
  backend — the V3 suite "satisfies V8 row (2)"
  (`egress_inguest_microvm_test.go:37-39`). W3 ratifies it into the
  acceptance set (no duplication: the cycle's deliverable is the assertion
  that the suite runs in the KVM lane). That ratification is carried by
  W7's **per-test presence check**, not by the CI package-ran guard: the
  guard is package-granular and `internal/runtime` already contains many
  `microvmtest.Require` callers, so it cannot tell whether any *particular*
  suite ran (see W7). Because the check IS this cycle's whole deliverable,
  it must reject a skip as well as an absence: it requires `--- PASS:` for
  the named test and fails on `--- SKIP:` at any depth beneath it, since a
  skipped test still prints `=== RUN` and an all-subtests-skipped parent
  still prints `--- PASS` (W7 carries the mechanism, the allowlist, and the
  mutations). W3 also adds the one missing leg: the same allow/deny
  probe pair run through `AgentRuntime.Launch`-provisioned sessions rather
  than direct `Create`/`Start` calls.
- **S1 contract (cycle 3).** The `ContainerRuntime` contract suite already
  runs with every divergence cap ON (`contract_microvm_test.go:47-55`). The
  missing layer is `AgentRuntime`: the podman lifecycle e2e
  (`lifecycle_test.go:5-16` — create, checkout-dir ownership, uid-1000 exec,
  egress hold, teardown) has no microVM sibling. W3 adds
  `TestAgentLifecycle_MicroVM` driving the production `AgentRuntime.Launch`
  path against a `MicroVMRuntime` engine, asserting the same five
  properties, plus a `package runner` gateway-liveness leg (§ Approach (a)
  — the gateway is served by the runner host, not by `Launch`). Vacuity
  analysis: a "passes unchanged" claim can be vacuous if rows silently
  skip; the contract suite's caps flags are asserted ON in the microVM leg,
  and W7's per-test presence check asserts each named acceptance test
  actually ran.
- **Boot timeout (cycle 4).** Two legs in W4, both `package runtime`.
  **What main actually proves, read off the body rather than the doc
  comment.** `TestMicroVMStartFailureLeavesNoState`'s body
  (`microvm_lifecycle_microvm_test.go:62-118`) makes exactly three
  assertions: `Start` returns an error (`:98-100`), `session.guestExec` is
  nil (`:108-110`), and `session.runtimeDir` does not exist after `Remove`
  (`:111-116`). That is **fail-closed Start** — error, no exec client,
  removable runtime dir — and nothing more. The phrase "no orphan
  processes" lives only in the doc comment (`:57-61`); the body reads no
  pid, checks no liveness, and signals nothing. **Main does not prove
  orphan-freedom.** So:
  (i) the corrupt-rootfs deadline path escalated with the orphan-freedom
  assertion **itself**, pidfile-identity-verified: the pids named by V7's
  per-session pidfiles (`<pid> <starttime> <bootid>`, PR #931 record §(a))
  are **captured during the boot window** and asserted all-dead once
  `Start` returns — dead *by identity*, the `<starttime>`/`<bootid>` triple
  matched against the captured values so a recycled pid cannot satisfy the
  check. Capturing rather than re-reading is load-bearing: V7's `Shutdown`
  removes all three pidfiles alongside the sockets (PR #931 record
  :194-196), so a post-`Start` re-read yields an empty pid set on the green
  path and "all pids dead" would pass vacuously. This is a base assertion
  W4 ADDS, not identity-verification layered over an existing orphan check;
  (ii) the other genuinely uncovered leg — a caller-deadline mid-boot
  cancellation: Start with a ctx deadline shorter than a real boot; assert
  teardown kills the mid-boot VMM within a bound.

  Vacuity analysis, per assertion. The new orphan assertion is vacuous if
  the check reads pids that were never written — "no live pids" is then
  trivially true — so the test first asserts the pidfiles exist and name
  live processes *during* the boot window (positive control), and reads the
  pids BEFORE teardown, because V7's `Shutdown` removes the pidfiles
  alongside the sockets (PR #931 record :194-196). The two assertions main
  *does* carry are vacuous in their own ways and W4 keeps them honest: "no
  exec client" passes trivially because `session.guestExec` is assigned only
  at `microvm_lifecycle.go:412`, past every error return on the failure path
  — it can never be non-nil here, so it discriminates a regression that
  moves the assignment earlier and nothing else; and "runtime dir gone"
  passes on any `Remove` at all, since `Remove`'s
  `os.RemoveAll(session.runtimeDir)` (`microvm_lifecycle.go:674-676`) runs
  unconditionally and independently of the VM's `Shutdown` (`:669-673`).
  Neither says anything about processes; both are retained as fail-closed
  coverage, not as orphan evidence.

  Proving mutation, re-derived so it reddens **this row's own property** —
  and it must reckon with **three** independent teardown paths, not just the
  deferred `Shutdown`, because the other two kill the VMM whether or not
  `Shutdown` ever runs. Two are defeatable from the test and the mutation
  defeats both; the third is not, which is what makes the
  could-not-discriminate outcome below mandatory rather than optional:

  1. **`Start`'s deferred `Shutdown`.** `booted := true`
     (`microvm_lifecycle.go:376`) with
     `defer func() { if booted { _ = vm.Shutdown(context.WithoutCancel(ctx)) } }()`
     (`:377-383`).
  2. **`exec.CommandContext` binding — ctx cancellation kills the children
     independently of `vm.Shutdown`.** All three are spawned bound to the
     boot ctx:
     `cmd: exec.CommandContext(ctx, vmmPath, vmmArgs(cfg, vm.consolePath, opts)...)`
     (`microvm/launch.go:220`), and identically virtiofsd (`:163`) and passt
     (`:187`). `CommandContext` "sets the command's Cancel function to
     invoke the Kill method on its Process, and leaves its WaitDelay unset"
     (go1.26.5 `src/os/exec/exec.go:481-483`, read this session), and package
     `microvm` overrides neither field (grepped this session: the only
     `cmd.Cancel`/`WaitDelay` hits under `internal/runtime/` are `PodmanCLI`'s,
     `podman.go:595-596`). The test's own shape cancels promptly — main's
     `TestMicroVMStartFailureLeavesNoState` derives
     `ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)` with
     `defer cancel()` (`microvm_lifecycle_microvm_test.go:96-97`) — and
     `bootPollContext` honors that caller deadline
     (`microvm_lifecycle.go:463-467`).
  3. **`Pdeathsig: SIGTERM`.** Every child carries the orphan guard,
     `return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}`
     (`microvm/orphanguard_pdeathsig.go:17-19`), installed at
     `c.cmd.SysProcAttr = orphanGuardSysProcAttr()` (`launch.go:299`). It
     fires on the spawning **thread's** death, and `startChild` locks that
     thread only across `cmd.Start` — `runtime.LockOSThread()` /
     `defer runtime.UnlockOSThread()` (`launch.go:301-302`) — so the lock is
     released the moment `startChild` returns and the Go runtime may retire
     that thread at any later point. This path is therefore **not
     defeatable from the test**, and the record does not pretend otherwise:
     it is best-effort and nondeterministic by its own design
     ("The real teardown guarantee is Shutdown, not Pdeathsig … this only
      shortens the window a wedged host leaves a VMM orphaned",
     `microvm/orphanguard_pdeathsig.go:10-12`, echoed at `launch.go:288-289`).

  So the honest mutation suppresses (1) **and** keeps the boot ctx
  uncancelled across the post-`Start` read: the mutation run passes a
  `context.WithoutCancel`-derived ctx into `m.launchFunc`
  (`microvm_lifecycle.go:367`; the production seam is wired by
  `installSeamDefaults`, `:131-137`), and the assertion reads the captured
  pids' liveness **before** the test's own `defer cancel()` fires. That
  defeats (1) and (2), the two deterministic killers. (3) cannot be defeated
  from the test at all, which is not a gap in the mutation but the reason the
  next paragraph is mandatory rather than a courtesy.

  **A green mutation run here is recorded as "could not discriminate — a
  competing killer reached the VMM first", never as a pass and never as a
  disproof of the assertion** — § Approach (b)'s primitive, used exactly as
  the gateway row uses it. This is spelled out because the failure is
  timing-dependent rather than a clean negative: whether a defer-only
  mutation discriminates depends on when the ctx is cancelled relative to
  the read, so it can redden on one run and not the next, and an executor
  reading the green run as evidence would weaken a correct assertion.

  (Recording why the previously-drafted mutation was a no-op, since the same
  trap is one edit away: against main's three assertions that same mutation
  reddens **nothing**. `Start` still errors because `awaitHealthy` still
  times out; `guestExec` is still nil because `:412` is past the error
  return; and the runtime dir still disappears because `Remove`'s
  `os.RemoveAll` at `:674-676` does not depend on `Shutdown` having run. The
  mutation only discriminates once an assertion actually reads a pid — which
  is exactly what leg (i) adds.)
- **Mid-session VMM death (cycle 5).** W4 boots a session through
  `AgentRuntime.Launch`, opens a live `ExecStreaming` and the gateway
  socket, then SIGKILLs the VMM pid (read from V7's `vmm.pid`). This leg
  consumes `internal/runner` symbols and therefore lives in
  `package runner`, not `package runtime` (§ Approach (a)). Asserts: the
  in-flight exec fails with the distinguishable death error V7 defines
  (PR #931 record §(c); the exact type name is consumed from V7 at
  implementation time — see the stated assumption in OQ-6); the
  `AgentStream` drains end **within a bound well under**
  `execDefaultTimeout` rather than wedge (the drain contract: "a read error
  is reported rather than swallowed",
  `go/internal/runner/agent_exec.go:277-279`); the peer daemons are torn
  down (pidfile liveness); `Remove` is idempotent. Vacuity analysis: an
  untyped "some error" assertion would pass on any transport hiccup — the
  typed-error assertion pins the death path; and an *unbounded* wait for
  stream end passes even on a wedged drain, because a wedge only delays,
  which is why the bound is part of the assertion. Proving mutations, one
  per property: suppress V7's monitor's error classification — the
  typed-error assertion MUST go red; skip the peer teardown on death — the
  pidfile-liveness assertion MUST go red; suppress V7's monitor's
  `vm.Shutdown` on death so the VMM is gone but peers and pipes are never
  torn down — the drain has no writer close to observe and the stream-end
  bound MUST trip.

### (f) Cycle 6 — KVM-absent tested by injecting the probe, not removing hardware

This box (and the CI leg) has working KVM, and `COMPASS_REQUIRE_MICROVM=1`
makes a KVM-absent skip a hard failure — so the KVM-absent path can never be
observed by disabling hardware. It is tested where the seam already exists:
`VerifyMicroVMSupport` runs its axes through the injected `preflightProbes`
struct ("so every failure axis is hermetically unit-testable without a real
/dev/kvm", `go/internal/runtime/microvm_preflight.go:26-30`), and the unit
row for a failing `openKVM` exists (`microvm_preflight_test.go:82-88`).

W5 escalates this to the acceptance contract D3 actually states — the error
*names the capability and the fix* (microvm-runner.md:693-697). That takes
**two** named tests plus a KVM-present control, because no single test can
drive the real chain to the D3 error: `verifyBackendPreflight` dispatches on
the concrete `microVMPreflighter` and calls `VerifyMicroVMSupport(ctx)`, which
hard-wires the real probes (`return m.verifyMicroVMSupport(ctx,
defaultPreflightProbes())`, `go/internal/runtime/microvm_preflight.go:79-81`)
— the injectable seam and the `preflightProbes` struct are unexported to
`package runtime` and unreachable from `package main`, and on a KVM-present
box the real type's gate passes anyway. So:

1. **The D3 error text, at the seam** (`package runtime`): drive
   `verifyMicroVMSupport(ctx, probes)` with an injected failing `openKVM`
   and assert the *returned* error value contains `/dev/kvm`, the word
   `KVM`, and the operator fix string — the full D3 sentence, asserted as
   three substrings of ONE error value, never of log output where
   neighbouring lines could satisfy them.
2. **The gate's refusal, at the startup boundary** (`package main`): a row
   extending `TestVerifyBackendPreflight` (`main_test.go:162-163`) with a
   fake preflighter returning a sentinel, asserting
   `verifyBackendPreflight` returns it — i.e. startup refuses rather than
   logging and continuing.

   The composition argument, stated explicitly so an executor does not burn
   a cycle rediscovering it: (1) proves the error *says* what D3 requires,
   (2) proves the gate *propagates* whatever the preflighter returns, and
   the seam between them is one trivial statement
   (`if err := pre.VerifyMicroVMSupport(ctx); err != nil { return err }`,
   `go/cmd/compass-runner/main.go:230-232`). The real chain end-to-end-to-
   failure is driven nowhere, and cannot be on a KVM-present box; the
   composition is what the acceptance claim rests on.
3. A KVM-*present* control on the same box: the real
   `defaultPreflightProbes()` pass end to end (this is the existing KVM-leg
   behavior, re-asserted so the failure test cannot drift onto a fake that
   no longer matches production wiring).

Vacuity analysis: an error-text assertion can pass while the gate is broken
if startup does not actually consult the probe result (error constructed but
ignored) — which is why row 2 asserts on `verifyBackendPreflight`'s
*return*, not on a log line. Conversely, row 2 alone would pass on a gate
that faithfully propagates a *wrong* error, which is why row 1 pins the D3
text on a real error value. Neither row is sufficient; the pair is, given
the one-statement propagation cited above. Proving mutation: make
`verifyMicroVMSupport` swallow the `openKVM` error (the
`hostcheck.DecideKVM(probes.openKVM())` arm,
`microvm_preflight.go:87-89`); row 1 MUST go red.

### (g) Cycle 7 — benchmark methodology and the report artifact

**No number in this record is a budget.** The benchmark produces the data the
Q-budget is set from (microvm-runner.md:839-842); methodology and reporting
are specified here, thresholds are not.

- **What is measured.** Per iteration: (i) boot-to-first-exec latency —
  wall-clock from `Create` through `Start` to the completion of one echo
  `Exec` (the same span `BootCanary` measures, `microvm_preflight.go:290-300`);
  (ii) steady-state memory — per-process PSS of the VMM/virtiofsd/passt trio
  via the existing `VM.PSS()` seam (`microvm_lifecycle.go:105-107`), read
  after the guest reports healthy and an exec has completed. The container
  baseline runs the same span on `PodmanCLI` (create + start + one exec, the
  `lifecycle_test.go` image recipe) with the container's cgroup memory read
  as its RSS analog.

  **The PSS reading is a known-incomplete lower bound, and the report says
  so rather than implying three values.** `VM.PSS()` is best-effort: a
  process whose `smaps_rollup` is unreadable is SKIPPED, leaving its key
  simply absent from the returned map —
  `if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) { continue }`,
  with the reason stated inline: "passt sets `PR_SET_DUMPABLE=0`, which
  reparents `/proc/<pid>/smaps_rollup` to root and denies the non-root
  reader" (`internal/runtime/microvm/launch.go:471-484`). Main's own
  consumer already records the consequence — the value "undercounts by the
  passt share on a healthy host … so treat it as a vmm+virtiofsd-dominated
  lower bound, not an exact footprint" (`microvm_preflight.go:277-287`). So
  on the CI runner the `passt` key will routinely be ABSENT, and a fixed
  three-value schema would publish a partial number with no marker. Two
  consequences the benchmark carries:
  - `pss_kb` is a **map whose missing keys are meaningful**, not a
    three-field record, and each iteration carries a companion
    `pss_incomplete: ["passt", …]` listing the processes whose PSS was
    unreadable — so a Q-budget reader sees a lower bound *labelled* as one.
  - W6 asserts the **required PSS key set** is present in **every** measured
    microVM iteration. That set is defined here, once, bound to its source
    rather than to prose: **the `cloud-hypervisor` and `virtiofsd` keys**,
    per `VM.PSS()`'s `out[c.name] = pss`
    (`internal/runtime/microvm/launch.go:485`) — the map is keyed by the
    child's `name` field, whose three literal values are `virtiofsd`
    (`launch.go:160`), `passt` (`:180`) and, for the VMM,
    `name: "cloud-hypervisor"` (`:217`). **There is no `vmm` key.** Main's
    existing consumer iterates exactly that literal set —
    `for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt"}`
    (`contract_microvm_test.go:112`). Every other site in this record (W6
    Interfaces, W6's vacuity paragraph and its proving mutation, OQ-4's scope
    note) points at **this** definition instead of restating the spelling, so
    a child rename reds in one place. `passt` is excluded from the required
    set because its drop-out is expected rather than faulty; the absence of
    either required key IS a real fault, which is what makes an incomplete
    sample visible instead of silently shrinking the number.

  **Two vocabularies over one trio of processes — recorded because V8 reads
  both surfaces and a drafting of this record already conflated them once.**

  | Surface | Vocabulary | Names | Source |
  | --- | --- | --- | --- |
  | `VM.PSS()`'s returned map key | **binary** name | `cloud-hypervisor`, `virtiofsd`, `passt` | `out[c.name] = pss` (`microvm/launch.go:485`), names at `:160`, `:180`, `:217` |
  | V7's `compass.microvm.guest.memory.pss` `process` attribute | **role** name | `vmm`, `virtiofsd`, `passt` | PR #931 record :757 — `Int64ObservableGauge`, `By`, `process` = `vmm`\|`virtiofsd`\|`passt` |
  | V7's per-session pidfile stems | **role** name | `vmm.pid`, `virtiofsd.pid`, `passt.pid` | PR #931 record :105-110 — the File/Process/Writer table, where `<id>/vmm.pid` is cloud-hypervisor's, written by the host |

  V7's gauge performs the `cloud-hypervisor` → `vmm` rename **at record
  time** — its own row derives the attribute from the map ("Sum over live
  sessions of per-process PSS via the existing `VM.PSS()`", PR #931 record
  :757) — so V8 inherits that rename invisibly unless it is written down.
  Which vocabulary each V8 deliverable uses: **W6's `PSSKB` keys and
  `PSSIncomplete` entries take the BINARY names** (they come straight out of
  `VM.PSS()`); **W4's pidfile reads take the ROLE names** (`vmm.pid` is
  cloud-hypervisor's file). Main's prose sits on the role side too — the
  "vmm+virtiofsd-dominated lower bound" quoted just above
  (`microvm_preflight.go:277-287`) is a *description of a sum*, not a key
  set, and reading it as one is precisely how a `vmm` PSS map key got into a
  drafting of this record. OQ-6 carries the pointer so the collision cannot
  silently recur when an executor binds V7's identifiers.

  The two PSS *views* are also not expected to agree, and neither supersedes
  the other: the JSON report's `pss_kb`/`pss_incomplete` is the **Q-budget's
  authoritative reading** (per-iteration, per-session, raw samples), while
  V7's gauge is the **operational series** (summed across live sessions,
  kB→bytes at record, PR #931 record :757). Divergence between them is a
  consequence of that difference in basis, not a defect in either.
- **How it runs.** `TestMicroVMBenchmark` (tagged `microvm && unix`,
  `microvmtest.Require(t)` first) runs a fixed warmup boot then N=5 measured
  iterations, recording every raw sample — never only an average, so outlier
  boots stay visible. The container baseline is `TestPodmanBaselineBenchmark`
  (tagged `podman`), gated on `podmanUsable()` (`lifecycle_test.go:56-60`).
- **Both legs run in ONE process — which means ONE PACKAGE — or the baseline
  is structurally absent.** The two backends live in different tag
  universes, and the lane's sweep is
  `go test -tags microvm -race -v -timeout 15m ./...` (`ci.yml:806`) — a
  `//go:build podman` file never compiles into that invocation. Left there,
  the baseline is not "occasionally missing" but *absent on every run*:
  cycle 7's "vs container baseline" (RIG-2499 acceptance row 7) permanently
  unmet, the report carrying `baseline: absent` forever, and nothing red
  anywhere. Nor can two invocations be merged after the fact —
  `writeBenchReport` writes a whole `BenchReport`, so a second process
  clobbers rather than merges. The benchmark therefore runs as its own
  step, one process spanning both tag universes:

  ```sh
  go test -tags "microvm podman" \
    -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark" \
    ./internal/runtime/
  ```

  **The package pattern is not optional and the "one process" premise
  depends on it.** Two separate points, both load-bearing:
  - *Without* a package pattern the command is not runnable at all. Run from
    the module root (`go/`, per § Global Constraints) it fails at setup —
    `# .` / `no Go files in <workspace>/compass/go` / `FAIL . [setup failed]`
    (executed this session) — because the module root holds no Go files. An
    executor building W7 from a pattern-less invocation hits a wall on the
    step's first run.
  - The obvious repair `./...` would run the command but **break the
    premise**: `go test` compiles and runs one test binary **per package**,
    so two legs in two packages are two processes, two `writeBenchReport`
    calls, and the second clobbers the first — `baseline: absent` forever,
    nothing red, the exact silent-and-permanent F-shape OQ-3 exists to
    prevent. Naming the single package is what makes "one process" true.

  Hence the co-location constraint, restated in W6's Interfaces and
  § Global Constraints: **both bench legs and the untagged report writer live
  in `internal/runtime`.** That is also where the podman leg's dependencies
  already are — `podmanUsable()` and the image recipe the baseline reuses are
  `package runtime` (`lifecycle_test.go:1-3` is `//go:build podman` /
  `package runtime`, `podmanUsable` at `:56-60`) — so co-location costs
  nothing and a leg drifting into `package runner` silently re-opens the
  hole. W6 additionally asserts the written report carries iterations for
  **both** backends (not merely `Baseline == "present"`), so a split-process
  regression reds instead of clobbering.

  The scoped `-run` keeps the rest of the podman suite out of the KVM lane.
  This requires podman to be usable on the ephemeral KVM runner, which W7's
  dry run must **verify, not assume** (`podmanUsable()` merely runs a
  container). Where the baseline genuinely cannot run, the report carries an
  explicit `baseline: absent:<reason>` marker rather than a silent
  microVM-only report — and OQ-3 (regraded load-bearing, precisely because
  presence is now achievable) rules on whether that reds the lane.
  Mechanical consequence: `writeBenchReport` and the report types must be
  visible to both tag universes, so they live in an untagged `_test.go` in
  that same package (W6 Interfaces notes the unused-in-default-tags wrinkle).
- **Report emission.** The benchmark writes one JSON document to the path in
  `COMPASS_MICROVM_BENCH_OUT` (unset ⇒ `t.Logf` only, so dev-box runs stay
  zero-config): schema
  `{schema: 1, host: {...}, iterations: [{backend, boot_ms, exec_ms, pss_kb: {<process>: kb, …}, pss_incomplete: [<process>, …]}], baseline: "present"|"absent:<reason>"}`.
  `pss_kb` is an open map, not a fixed `{vmm, virtiofsd, passt}` triple: a
  key is present iff that process's PSS was readable, and every unreadable
  process is named in `pss_incomplete` (see the undercount above). A reader
  computing medians therefore knows which processes each sample covered.
- **Where it lands.** The CI lane writes the JSON to the workspace, renders a
  markdown table of per-iteration numbers plus medians into
  `$GITHUB_STEP_SUMMARY`, and uploads the JSON as a build artifact
  (`microvm-bench-report`) — the durable series the Q-budget ruling reads.
  `ci.yml` uploads no artifacts anywhere today (checked this session: no
  `upload-artifact` use in it), so this is a new external action entering
  the lane and is specified to the record's usual grounding rather than left
  to the executor:
  - **Action + pin.** `actions/upload-artifact` **v7.0.1**, SHA-pinned in
    the `uses:` with the version in a trailing comment, matching both the
    workflow's own discipline (`ci.yml:649`
    `uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1`;
    `:655` `cachix/install-nix-action@630ae543ea3a38a9a4166f03376c02c50f408342 # v31`)
    and the repo's one existing use of this very action in the sibling
    workflow (`release.yml:417`
    `uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1`).
    That commit is the v7.0.1 pin to reuse; the executor re-resolves it at
    implementation and keeps the two workflows on one version.
  - **Retention.** An explicit `retention-days` — the series must outlive a
    default the org can change under it. 90 days is the ask: long enough for
    the Q-budget ruling to read a trend, short of the 90-day cap.
  - **`if-no-files-found: error`.** The benchmark step exports
    `COMPASS_MICROVM_BENCH_OUT`, so a missing JSON means the bench never
    wrote — which must red rather than upload nothing, the same posture
    `release.yml:421` takes. Defaulting to `warn` is precisely the
    report-exists-and-certifies-nothing F-shape.
  - **Job `permissions`.** `upload-artifact` v4+ needs no extra scope beyond
    the workflow-level `permissions: contents: read` (`ci.yml:124-125`) —
    it writes through the artifact backend, not the GitHub API — and the
    `microvm` job declares no `permissions:` block of its own, so it
    inherits that. **W7 must verify this on the dry run rather than assume
    it** (it is the same verify-don't-assume bar the `podmanUsable()` probe
    carries), and add a least-privilege job-level `permissions:` block only
    if the dry run shows one is needed — the precedent for declaring one
    explicitly is `release.yml:337-338`.
- **Metrics cross-check — the counted quantity is a `Count`, not a point
  count.** The benchmark installs `sdkmetric.NewManualReader()` as the
  global meter provider *before* constructing the runtime (the established
  pattern, `go/internal/delivery/trace_test.go:209-220`) and asserts V7's
  `compass.microvm.boot.duration` (PR #931 record :754 declares it a
  **`Float64Histogram`** with a single `outcome` = `ok`\|`error` attribute).
  A histogram does **not** emit one data point per recording: it aggregates
  every recording sharing one attribute set into a single point, whose
  `Count` is the number of recordings —
  `type HistogramDataPoint[N int64 | float64] struct { Attributes attribute.Set; …; Count uint64; …; Sum N; … }`, where `Attributes`
  are the values that "uniquely identify the timeseries" and `Count` is
  "the number of updates this histogram has been calculated with"
  (`go.opentelemetry.io/otel/sdk/metric@v1.46.0/metricdata/data.go:102-129`).
  Six `Start` calls all carrying `outcome=ok` therefore collapse into **one**
  point with `Count == 6` — an executor asserting
  `len(hist.DataPoints) == 6` gets 1 and a red test on the first honest run.

  So the assertion is: **exactly ONE**
  `metricdata.HistogramDataPoint[float64]` in
  `compass.microvm.boot.duration`'s `metricdata.Histogram[float64]` whose
  attribute set is *precisely* the single pair `outcome=ok`, and whose
  **`Count == 6`** — 6 *recordings*, not 6 points: 1 warmup + 5 measured,
  the warmup included because it runs `Start` through the same runtime,
  constructed after the reader was installed, and V7's instrument fires per
  `Start`. (Asserting `Count == 5` would be red on the first honest run.)
  The mandated protective comment is aimed at the `Count` and states the
  decomposition — "`Count` = 6 recordings = 1 warmup + 5 measured, all
  through the runtime constructed after the reader was installed" — so a
  future change to the warmup count cannot quietly turn the check into an
  off-by-one. Naming "the sixth point" would pin a quantity that never
  exists and would catch nothing.

  The attribute set is asserted exhaustively, not merely searched for, using
  the established `dispatchedCounts` idiom (`trace_test.go:253-256`
  asserts "op.kind is the ONLY attribute on every data point"): a stray
  attribute would split the timeseries into a second point, so "exactly one
  point, attributes exactly `{outcome: ok}`" is what makes the check
  distinguish 6 boots from 6 boots plus a stray recording on another
  attribute set — a bare `Count` lookup on a found point cannot. V8
  *consumes* V7's instrument set and defines no metric of its own.

### (h) The CI deliverable — extend the existing KVM leg, not a new job

The "KVM-labeled runner" lane already exists as the `microvm` job: a
peer job behind the rollup, KVM enabled on the ephemeral runner via the udev
rule + userns sysctl (`ci.yml:741-746`), running
`go test -tags microvm -race -v -timeout 15m ./...` with
`COMPASS_REQUIRE_MICROVM: '1'` so a KVM-withheld runner reds instead of
skipping (`ci.yml:750-756,805-808`), followed by the skipped-vs-ran guard
(`ci.yml:810-849`). W7 extends this job rather than adding a second one
(OQ-5): the new acceptance tests join the existing `./...` sweep
automatically, and two steps are added — the benchmark invocation (exporting
`COMPASS_MICROVM_BENCH_OUT`) and the report render/upload. The 15m test
timeout and 30m job timeout are re-examined in W7 against the measured
two-session suite cost; raising them is a W7 deliverable if the dry run
demands it, not a silent drift.

## Plan

Dependency order: W1 and W2 are independent of each other and come first (the
*new* isolation properties). W3-W5 are independent of each other and of W1/W2
at the code level but review in cycle order. W6 needs nothing from W1-W5 but
consumes V7's metric set. W7 is last: it wires everything into CI and cannot
be verified before the suites it runs exist.

Land-order, stated to match each task's own **Depends** line rather than
flattened into one milestone gate — the per-task line is authoritative, and
these agree with it:

| Task | Needs |
| --- | --- |
| W2 | nothing beyond main (**full exemption**) |
| W5 | nothing beyond main; fully hermetic (**full exemption**) |
| W1 | V6 (PR #912) merged |
| W3 | V6 + V7 merged |
| W4 | V7 merged (pidfiles + death error + monitor) |
| W6 | V7 merged (the metric instrument) |
| W7 | W1-W6 (it runs them) |

So there are **two** full exemptions, not one. W2 needs nothing off main
(the guestd peer-CID gate is on main,
`go/internal/guestd/vsock.go:25-29`) and W5 needs nothing off main either —
its subject is the KVM-less path and its Interfaces consume only
`preflightProbes`/`verifyMicroVMSupport`
(`microvm_preflight.go:79-85`) and `verifyBackendPreflight`
(`cmd/compass-runner/main.go:212`, exercised at `main_test.go:162`), all
already on main. Both may land before either merge; land-order freedom is
the point of their independence, and the plan orders by independent-first.
W6 needs V7 **only** — not V6. Everything that does consume a lower
milestone's symbols (`isolationSession`,
`TestMicroVMCrossSessionVolumeUnreachable`, pidfiles, death errors,
metrics) must not fork them.

### W1 — inter-tenant probe (cycle 1)

A new `microvm_intertenant_microvm_test.go` in `package runtime`
(`//go:build microvm && unix`) booting two concurrent sessions on one
`MicroVMRuntime` and probing every A→B surface per § Approach (c). Reuses
PR #912's `isolationSession`/`guestSh`/`snapshotTree` helpers where their
signatures allow a caller-chosen session name; otherwise extends them
additively in this file.

**Scope — the volume leg is a delta, not a new probe.** PR #912 already
ships a two-session volume probe: `TestMicroVMCrossSessionVolumeUnreachable`
asserts "two live sessions, and guest A cannot reach guest B's volume by any
path, nor write into it" (PR #912
`microvm_isolation_microvm_test.go:391-395`) with A-reads-own-canary and
B-reads-own-secret positive controls (PR #912 `:410-421`) and an awk content
sweep covering B's volume parent (PR #912 `:446-464`). Since the Plan's own
preamble forbids forking V6's symbols, W1 **consumes and extends** that test
— it already takes caller-chosen names via `isolationSession(t, env, name)`
(PR #912 `:85`) — rather than re-deriving four probes. W1's volume/host-fs
delta is the one-runtime topology and the symlink-in-A's-volume shape; its
net-new content is the host-network leg and the vsock leg (OQ-8).

- **Interfaces:** consumes `NewMicroVMRuntime(cfg MicroVMConfig)
  *MicroVMRuntime`, the `ContainerRuntime` verbs
  (`Create(ctx, ContainerSpec) (ContainerID, error)`, `Start`, `Exec`,
  `Remove`), `e2eConfig(t, env) MicroVMConfig`
  (`microvm_lifecycle_microvm_test.go:33-55`), and PR #912's isolation
  helpers including `TestMicroVMCrossSessionVolumeUnreachable`'s session
  fixture. Produces:
  `TestInterTenantVolumeUnreachable` (the V6 delta),
  `TestInterTenantHostFilesystemConfined`,
  `TestInterTenantHostNetworkUnreachable(t *testing.T)`, and — **pending
  OQ-8** — `TestInterTenantVsockIdentityBound`, whose assertion shape is not
  yet decided and which must not be implemented before that OQ rules.
- **Test cycle (per-assertion vacuity + mutation):**

  | Assertion | Could pass while false when… | Proving mutation |
  | --- | --- | --- |
  | A cannot read/write B's volume (delta over PR #912's leg: one runtime, symlink shape) | probe script never ran (exit 127) or probed a wrong path | run both guests' virtiofsd rooted at the volumes' common parent — MUST go red |
  | A's guest-side vsock dial is bound to A's own session — **pending OQ-8**, do not implement until it rules | as drafted the row was unimplementable, not merely vacuous: nothing binds host `vsock.sock_1024`, so the required "dial succeeds" can never hold, and the drafted mutation reddens main's host-side nonce check during setup instead of this row's property | supplied by OQ-8's ruling: under option (a) (dial CID 2:1025, assert A observes A's own gateway's per-session discriminator) the mutation is to swap A's and B's gateway listeners in the harness — A then observes B's discriminator and this row MUST go red |
  | host fs unreachable from A | write "succeeded" only inside the guest overlay and no host check ran | same virtiofsd-root widening; host snapshot MUST change and go red |
  | the in-guest nft default-deny rule is armed and enforcing on A's outbound path to the gateway address `10.0.2.2` — **the claim is the firewall's enforcement, NOT "the host is unreachable"** (only OQ-1 (i) would deliver that; there is no layer beneath the firewall today) | (a) the connect failed for want of any listener rather than being blocked; (b) **the no-listener hazard's twin, and not closed by construction:** the connect failed because passt never mapped the host onto `10.0.2.2` at all — `--no-map-gw` is "Implied if there is no gateway on the selected default route, or if there is no default route" (passt(1), pinned 2025_09_19), so on a runner with a different default route the row passes for a reason unrelated to nft, and the mutation below cannot redden either | (a) is excluded by construction: the test itself opens the host-bound listener it dials. For (b) the mutation is **self-verifying** — arm the session's egress policy allowlisting `10.0.2.2` (the accept rule is destination-only, `egress.go:130`, resolved at arm time in-guest, `egress.go:88-103`, so keep the dial on the single test-opened port), then assert IN ORDER: (1) the allowlisted connect is **observed accepted at the test's host listener** — the mutation's own positive control, and the proof host-mapping is in effect on this box; (2) the row's assertion MUST go red. If (1) fails, record **"could not discriminate on this host — passt host-mapping not in effect"**, never a passed mutation and never a green row |
  | the metadata endpoint `169.254.169.254:80` is unreachable from A — **runs only when the host-side precondition holds** | this is the row's dominant failure mode, not an edge case: a guest connect to the metadata IP rides passt's ordinary outbound path as a host-originated connect, so on any box with no metadata service (every dev box) "must fail" passes for want of a listener **even with the entire nft ruleset deleted** | the test first probes the endpoint host-side; if it does not answer the leg SKIPS with that reason recorded (a vacuous pass is worse than a declared gap). Where it does answer, mutation: the same `169.254.169.254` allowlist-widening as the gateway row — the connect MUST succeed and the row MUST go red |

  Positive controls in every test: A reads its own canary, and an
  allowlisted external raw IP connects — both through the identical
  script/binary path as the negatives. Note the external-IP control does
  *not* discriminate for the metadata row (it proves egress works, not that
  the metadata path could have succeeded), which is why that row carries the
  host-side precondition probe instead. The vsock row's positive
  discriminator is supplied by OQ-8's ruling.
- **Depends:** V6 merged — W1 extends
  `TestMicroVMCrossSessionVolumeUnreachable` and reuses its
  `isolationSession`/`guestSh`/`snapshotTree` helpers. No V7 symbol is
  consumed here (W1 asserts no pidfiles), so V7's merge does not gate it.
  Independent of W2.

### W2 — in-guest escalation probe (cycle 8)

`microvm_escalation_microvm_test.go` plus the probe source
`testdata/vsockprobe/main.go` per § Approach (d).

- **Interfaces:** produces
  `TestEscalationProbeRefusedOnLoopback(t *testing.T)` and the probe binary
  contract: `vsockprobe <cid> <port>` exits `0` = connected and received
  bytes, `2` = connected then closed with no bytes, `3` = connect refused/
  no path. Per § Approach (d), the three executability terms:
  - **Build.** `exec.Command` running, from the module root,
    `CGO_ENABLED=0 GOOS=linux go build -o <dst> ./internal/runtime/testdata/vsockprobe`
    — an **explicit path**, because the Go tool ignores `testdata` for
    package patterns (`go list ./internal/runtime/testdata/...` →
    `matched no packages`, verified this session), so `./...` matches nothing
    here and no repo-wide `vet`/lint/build ever compiles this file.
    `CGO_ENABLED=0` is what makes it static for the guest's minimal rootfs;
    `GOOS=linux` because the host need not be linux.
  - **A compile guard in the untagged half**, so the invisibility above
    cannot let the probe rot silently: an untagged test runs the same
    explicit-path build into `t.TempDir()` and fails on a compile error, so
    rot reds in the default lane on a KVM-less box rather than waiting for
    the KVM lane.
  - **Staging through the ONE `/workspace` share.** The built binary is
    written (mode 0755) into the existing `/workspace` host directory
    **before `Start`**; it must NOT get its own mount. `workspaceShare`
    refuses >1 mount, a read-only mount, or any single mount whose
    `ContainerPath != /workspace`, with `UnsupportedMountError`
    (`microvm_lifecycle.go:313-330`) — a second read-only mount for a
    test-only binary fails at `Create`.

  Exec'd via `Exec(ctx, id, NewExecSpec("/workspace/vsockprobe", cid, port))`
  as the session's non-root uid. Consumes `guestVsockPort` (1024,
  `microvm_lifecycle.go:52`) as the dialed port.
- **Test cycle:** `Exec` returns **`err == nil`** first — a transport or
  spawn failure is a harness fault and `t.Fatal`s, matching V6's `guestSh`
  posture ("A transport/refusal error is fatal; a NON-ZERO EXIT IS NOT",
  PR #912 `microvm_isolation_microvm_test.go:111-123`) — and only then is an
  exit code compared. Then: CID-2 dial exits 0 (positive control); the CID-1
  dial's asserted exit code is **pending OQ-9** — drafted as exit 2
  (refused-after-accept, guestd's close-before-first-byte,
  `go/internal/guestd/vsock.go:57-59`), but on today's guest image the dial
  cannot reach guestd's `Accept` at all for want of the `vsock_loopback`
  transport, so exit 2 is red and exit **3** is the observable (and
  strictly stronger) outcome. Do not implement this row until OQ-9 rules;
  under either ruling the row asserts one specific exit code, never
  "non-zero". Vacuity: covered by the CID-2 control plus the exit-code
  discrimination — `3` for an absent transport or refused connect, `2` for
  accepted-then-closed, `0` for connected-and-served, all distinct. **A
  missing or unstaged probe is NOT in that table:** guestd converts an
  unresolvable or unstartable program into
  `connect.NewError(connect.CodeInternal, …)`
  (`internal/guestd/supervisor.go:600-614`; a slash-bearing argv bypasses
  PATH entirely, `:643-646`), which `Exec` returns as
  `ExecOutput{}, err` (`microvm_lifecycle.go:486-496`) — so 126/127 can
  never be observed as an exit code on this path, and the `err == nil`
  assertion above is what catches a broken staging. Proving mutation, per
  OQ-9's ruling: under (a) a guest image built WITH `vsock_loopback` must
  flip the probe from 3 to 2 — the exit-3 assertion MUST go red; under (b),
  with the transport present, a guest image whose `peerAllowed` returns
  `true` unconditionally (`guestd/vsock.go:36-38`) makes CID-1 exit 0 — the
  exit-2 assertion MUST go red.
- **Depends:** none beyond main (the guestd gate is on main,
  `go/internal/guestd/vsock.go:25-29`); independent of W1 and of V6/V7 —
  one of the Plan preamble's **two** full exemptions (with W5), and W2's
  land-order freedom is the point of its independence.

### W3 — S1 contract + egress under the assembled lifecycle (cycles 2, 3)

The microVM sibling of the podman `TestPerAgentContainerLifecycle`
(`lifecycle_test.go:5-16`), driving `AgentRuntime.Launch` end to end on a
`MicroVMRuntime` engine, then the allow/deny egress probe pair through the
launched session.

- **Placement (see § Approach (a)).** The lifecycle + egress legs go in
  `microvm_agent_lifecycle_microvm_test.go` in `package runtime`. The
  **gateway-liveness leg** consumes `internal/runner/gateway` symbols and so
  lives in `package runner`, as an extension of the settled precedent
  `e2e_vsock_gateway_microvm_test.go` — `gateway/socket.go:31` imports
  `internal/runtime`, so a `package runtime` file importing it is an import
  cycle. It also has to be there conceptually: `AgentRuntime.Launch` never
  serves the gateway — the runner host does
  (`listener, err := gateway.Serve(ctx, endpoint, name, deps)`,
  `go/internal/runner/host.go:824`) — so a runtime-package test has no
  gateway socket to probe.
- **Interfaces:** consumes the production `AgentRuntime` launch surface
  (`go/internal/runtime/agent.go` — the provision sequence that on this
  backend skips host-side arming via the `EgressArmedInGuest()` marker,
  `microvm_lifecycle.go:418-424`) and the gateway serving path
  (`gateway.Serve` over `microvm.GatewaySocketPath(vsockSocket, 1025)`,
  `microvm/config.go:40-48`). Produces `TestAgentLifecycle_MicroVM` and
  `TestAgentLifecycleEgress_MicroVM` (`package runtime`) and
  `TestAgentGatewayLiveness_MicroVM` (`package runner`).

  **The `package runner` leg cannot reach `e2eConfig`, and must not fork a
  third short-runroot helper.** `e2eConfig` is a `package runtime` test
  symbol (`microvm_lifecycle_microvm_test.go:33-55`), unreachable from
  `package runner` — the same forfeiture the record notes for the rejected
  external-`runtime_test` option applies to the `package runner` legs
  actually chosen. The precedent file already solves it with its own
  private copy, `w3MicroVMConfig(t, env) runtime.MicroVMConfig`
  (`e2e_vsock_gateway_microvm_test.go:132-162`, carrying the identical
  short-runroot `sun_path` rationale: the widest per-session leaf is a
  57-byte tail and a `t.TempDir()` root overflows the 107-byte cap, so the
  bind fails `EINVAL` and the boot times out). W3's gateway-liveness leg
  **consumes that existing helper**; it does not add a third copy.
- **Test cycle:** the five podman-leg properties (session boots via Launch;
  checkout dir owned by uid 1000; exec runs as uid 1000; egress allow/deny
  holds; teardown removes) plus: the gateway socket accepts a connection
  while the session lives and stops accepting after teardown. Vacuity +
  mutations:
  - **The uid assertion** could pass if the exec never ran — the probe
    echoes `id -u` output and asserts `1000` literally. **The mutation must
    be reachable, which "force the exec uid to 0" is not.** uid-0 refusal is
    enforced at three independent layers, and a local *guest* build defeats
    at most the latter two: (1) **host-side, before Provision** —
    `if session.uid == 0 { return fmt.Errorf("microvm: session %s has a zero exec uid; Provision requires a non-zero default_exec_uid", id) }`
    (`microvm_lifecycle.go:389-391`); (2) **guest-side at Provision** —
    `if m.GetDefaultExecUid() == 0 { return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("default_exec_uid must be non-zero: the guest supervisor never runs an exec as root")) }`
    (`internal/guestd/supervisor.go:233-235`); (3) **guest-side per-exec in
    `resolveUID`** —
    `if u == 0 { return 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("exec uid 0 is refused: …")) }`
    (`supervisor.go:554-556`). Layer (1) is HOST code, so with a uid-0
    session the run never boots to an exec at all: `Start` fails during
    setup and the row reddens **for the wrong reason** — the same
    mistargeting this record diagnosed and rejected for the OQ-8 vsock leg.
    So the mutation is re-aimed at **this row's own property — that the exec
    runs as uid 1000 rather than as some other non-zero uid**: in a local
    guest build, make guestd's credential resolution ignore the requested
    uid and return a different NON-ZERO uid (hard-code `1001` in
    `linuxCredential`/`resolveUID`'s return, `supervisor.go:60-62`,
    `:545-559`). All three refusal layers stay intact, `Start` still
    succeeds, the session still boots, and the `id -u` echo assertion goes
    red **on the value** — which is exactly what this row asserts. The
    contract suite's `refusesRootExec` row is **pre-existing coverage, not
    part of W3's mutation**: it already runs in the microVM contract leg on
    main (`contract_microvm_test.go:49` sets `refusesRootExec: true`;
    `contract_suite_test.go:273-277` asserts
    `rt.Exec(…, NewExecSpec("id", "-u").AsUser("0"))` errors), so citing it
    adds no discriminator here.
  - **The egress deny** could pass on an unreachable host — the paired allow
    probe through the same script is the control (mutation: drop the deny
    rule from `NftScript()` output in a local build — deny probe MUST go
    red).
  - **The gateway-liveness assertion** could pass on a stale socket file —
    it asserts a completed accept while the session lives AND a refused
    connect after teardown (mutation: skip the listener's `Close` on
    teardown — the post-teardown leg MUST go red).

  Cycle 2's ratification: the V3 suite
  (`egress_inguest_microvm_test.go`) already runs in the KVM lane, and W7's
  **per-test presence check** — not the package-granular package-ran guard,
  which cannot see a single dropped file — is what asserts it by name. W3
  adds no duplicate of it.
- **Depends:** V6/V7 merged; independent of W1/W2.

### W4 — boot timeout + mid-session VMM death (cycles 4, 5)

`microvm_failure_microvm_test.go`: the failure-mode escalations of
§ Approach (e).

- **Placement (two files, two packages — see § Approach (a)).** The boot
  timeout legs are runtime-layer and stay in
  `microvm_failure_microvm_test.go` in `package runtime`. The cycle-5 death
  legs consume `internal/runner` symbols (`AgentRuntime.Launch`, the
  `AgentStream` drain contract) and therefore live in
  `e2e_vmm_death_microvm_test.go` in **`package runner`**, beside the
  existing `e2e_vsock_gateway_microvm_test.go` — a `package runtime` file
  importing `internal/runner` is an import cycle and will not compile.
- **Interfaces:** consumes V7's per-session pidfile layout
  (`<RunRoot>/microvm/<id>/{vmm.pid,virtiofsd.pid,passt.pid}`, PR #931
  record §(a) — settled records `<pid> <starttime> <bootid>`; the literal
  file names, like the death-error symbol and the metric names, are bound to
  V7's contract rather than to these identifiers — OQ-6), V7's
  distinguishable VM-death error (PR #931 record §(c); exact exported name
  consumed at implementation — OQ-6 states the assumption), the
  `AgentStream` drain contract (`agent_exec.go:98-124`), and
  `execDefaultTimeout` (120s, `microvm_lifecycle.go:97`). Produces
  `TestBootTimeoutKilledAndCleaned`, `TestStartDeadlineTearsDownMidBoot`
  (both `package runtime`) and `TestVMMDeathBreaksStreamsCleanly`
  (`package runner`).

  **The pid-identity comparison consumes V7's own predicate, not a second
  `/proc` parser.** The `<pid> <starttime> <bootid>` match is performed by
  the same pid-identity predicate V7's orphan reaper uses to decide a
  recorded pid is still its process: V7's W1 produces
  `type pidRecord struct { Intent bool; PID int; StartTime uint64; BootID string }`
  with `readPidfile(path) (pidRecord, error)` and
  `(pidRecord) alive() (bool, error)` — "boot-id short-circuit, then
  starttime-compared liveness (stale boot id ⇒ false, nil; ENOENT ⇒ false,
  nil)" (PR #931 record :1147-1152) — over `/proc/<pid>/stat` field 22
  (PR #931 record :131-134, and §(b)'s reaper at :344-345). W4 must NOT
  re-parse `/proc/<pid>/stat` itself: a second start-time parser beside V7's
  can drift from the one the reaper's kill decision rides on. The exact
  symbol is bound at implementation per OQ-6, which carries this predicate in
  its contract list — including the flag that V7 places these in
  `package microvm` and states no exported or test-reachable form for
  `package runtime` / `package runner`, where W4's legs live.

  The `package runner` death leg, like W3's gateway leg, **cannot reach
  `e2eConfig`** (a `package runtime` test symbol,
  `microvm_lifecycle_microvm_test.go:33-55`) and consumes the existing
  private helper `w3MicroVMConfig(t, env) runtime.MicroVMConfig`
  (`e2e_vsock_gateway_microvm_test.go:132-162`) beside which it lands. It
  must not fork a third short-runroot helper — the `sun_path` rationale is
  already recorded there.
- **Test cycle:**

  | Assertion | Could pass while false when… | Proving mutation |
  | --- | --- | --- |
  | wedged boot torn down orphan-free: the pids **captured from V7's pidfiles during the boot window** are all dead once `Start` returns, matched by the `<pid> <starttime> <bootid>` triple **via V7's own pid-identity predicate** (Interfaces above) so a recycled pid cannot satisfy it — **a base assertion W4 adds; main asserts nothing about processes** (`microvm_lifecycle_microvm_test.go:62-118`) | **a post-`Start` RE-READ of the pidfiles is vacuous by construction and must not be the assertion's input:** on the green path `Start`'s teardown calls `vm.Shutdown`, which removes all three pidfiles alongside the sockets (PR #931 record :194-196), so a re-read yields an EMPTY pid set and "all pids dead" passes trivially — the same shape as pidfiles never having been written. (`Remove`'s `os.RemoveAll(session.runtimeDir)`, `microvm_lifecycle.go:674-676`, would erase them too, but `Shutdown` gets there first.) | the pid values are **read and retained** during the boot window, where the positive control asserts the pidfiles exist and name LIVE processes; the post-`Start` assertion then tests THOSE retained pids, so an emptied pidfile directory cannot satisfy it. **Mutation — and THREE independent teardown paths kill the VMM here, so suppressing the deferred `Shutdown` alone does NOT discriminate (§ Approach (e) carries the full derivation): (1) `Start`'s `booted`/deferred-`Shutdown` block (`microvm_lifecycle.go:376-383`); (2) every child is `exec.CommandContext`-bound to the boot ctx (`microvm/launch.go:163`, `:187`, `:220`), and `CommandContext` "sets the command's Cancel function to invoke the Kill method on its Process" (go1.26.5 `src/os/exec/exec.go:481-483`) with no `Cancel`/`WaitDelay` override in package `microvm`, so ctx cancellation kills the VMM independently of `vm.Shutdown` — and the test's own ctx is a 30s `context.WithTimeout` with `defer cancel()` (`microvm_lifecycle_microvm_test.go:96-97`), honored by `bootPollContext` (`microvm_lifecycle.go:463-467`); (3) each child carries `Pdeathsig: syscall.SIGTERM` (`microvm/orphanguard_pdeathsig.go:17-19`, installed at `launch.go:299`), which fires on the spawning THREAD's death and is NOT defeatable from the test — `startChild` unlocks that thread as soon as it returns (`launch.go:301-302`), and the guard is best-effort by its own design (`orphanguard_pdeathsig.go:10-12`). The mutation therefore comments out (1) AND passes a `context.WithoutCancel`-derived ctx into `m.launchFunc` (`microvm_lifecycle.go:367`) so (2) cannot kill first, reading liveness BEFORE the test's own `defer cancel()` — which defeats the two DETERMINISTIC killers. Only then does the captured VMM pid stay LIVE for the assertion to read: MUST go red.** Because (3) remains undefeatable, a green mutation run is recorded as **"could not discriminate — a competing killer reached the VMM first"**, never as a pass and never as a disproof of the assertion (§ Approach (b)). (The same mutation reddens NOTHING against main's three assertions — `Start` still errors via `awaitHealthy`'s timeout, `guestExec` is still nil because its only assignment is at `:412`, and the runtime dir still disappears because `Remove`'s `os.RemoveAll` is independent of `Shutdown`. It discriminates only because this row reads a captured pid.) |
  | `Start` is fail-closed: it returns an error and leaves no exec client (retained from main, `:98-100`, `:108-110`) | the no-exec-client half is **structurally** near-vacuous: `session.guestExec` is assigned only at `microvm_lifecycle.go:412`, past every error return on this path, so it can never be non-nil here — it discriminates a regression that moves the assignment before the error returns, and nothing else | none added: W4 retains it as fail-closed coverage and does NOT credit it as orphan or teardown evidence. Its discriminating mutation is to hoist the `session.guestExec` assignment above the `awaitHealthy` error return (`:385-387`) — MUST go red |
  | the session runtime dir is removable after the failed `Start` (retained from main, `:111-116`) | passes on *any* successful `Remove`: `os.RemoveAll(session.runtimeDir)` (`microvm_lifecycle.go:674-676`) runs unconditionally, and on this path `session.vm` is nil (never assigned, `:411`), so `Remove` skips `Shutdown` (`:669-673`) entirely — the row is blind to whether any process died | none added; same posture as above. Mutation that does discriminate it: make `Remove` return before its `os.RemoveAll` — MUST go red |
  | in-flight exec fails with the typed death error | any transport error satisfies an untyped check | assert `errors.As` against V7's death type; mutation: suppress V7's death monitor's error classification — MUST go red |
  | the peer daemons are torn down after death | the pidfiles were never written, or the pids were already dead for an unrelated reason | assert each pidfile names a LIVE process pre-SIGKILL (positive control); mutation: skip the peer teardown on death — the post-death pidfile-liveness check MUST go red |
  | gateway/exec streams end promptly, drains do not wedge | a wedged drain only *delays*, so an unbounded wait eventually observes the end and passes anyway | bound the stream-end observation well under `execDefaultTimeout` (120s, `microvm_lifecycle.go:97`) and fail on the bound; mutation: suppress V7's monitor's `vm.Shutdown` on death in a local build, so the VMM is gone but peers and pipes are never torn down — the drain then has no writer close to observe, and the stream-end bound MUST trip |
  | `Remove` after death is idempotent | Remove silently no-ops on an unknown session, passing on a *different* bug | assert the session existed pre-death (positive control) and the runtime dir is gone post-Remove |

- **Depends:** V7 merged (pidfiles + death error + monitor). Independent of
  W1-W3.

### W5 — KVM-absent hard-fail acceptance (cycle 6)

Hermetic (untagged) tests per § Approach (f) — the one cycle that must NOT be
KVM-gated, since its subject is the KVM-less path.

- **Interfaces:** consumes `preflightProbes` and
  `(*MicroVMRuntime) verifyMicroVMSupport(ctx, probes)`
  (`microvm_preflight.go:79-85`) and `verifyBackendPreflight(ctx, engine)`
  (`main.go:212-217`). Produces `TestKVMAbsentHardFailNamesCapability` (in
  `package runtime`, `//go:build unix`) and a `package main` row extending
  `TestVerifyBackendPreflight` (`main_test.go`) asserting the microVM
  branch's error propagates to the startup gate verbatim.
- **Test cycle:** injected `openKVM` failure ⇒ returned error mentions
  `/dev/kvm`, `KVM`, and the fix text (the D3 sentence, asserted as three
  substrings of ONE error value — never of log output, where neighbouring
  lines could satisfy them); KVM-present control: `defaultPreflightProbes()`
  passes on the KVM leg (tagged twin, `microvmtest.Require(t)`). Vacuity:
  per § Approach (f) — the assertion is on the error *returned by the gate*,
  proving startup refuses, not that an error string was constructed
  somewhere. Proving mutation: swallow the `openKVM` error inside
  `verifyMicroVMSupport` — MUST go red.
- **Depends:** none beyond main; fully hermetic — it needs neither V6 nor
  V7, so it is the Plan preamble's **second full exemption** (with W2) and
  may land before either merge. Independent of W1-W4.

### W6 — boot-latency + RSS benchmark and report (cycle 7)

`microvm_bench_microvm_test.go` plus the podman baseline leg, per
§ Approach (g).

- **Interfaces:** produces `TestMicroVMBenchmark(t *testing.T)` (tagged
  `microvm && unix`) and `TestPodmanBaselineBenchmark(t *testing.T)` (tagged
  `podman`); the report writer
  `writeBenchReport(path string, r BenchReport) error` with
  `type BenchReport struct { Schema int; Host HostInfo; Iterations []BenchIteration; Baseline string }`
  and
  `type BenchIteration struct { Backend string; BootMillis int64; ExecMillis int64; PSSKB map[string]int64; PSSIncomplete []string }`
  (unexported to the test files — no production surface). The writer and
  both report types live in an **untagged** `microvm_bench_report_test.go`,
  because they must be visible to both tag universes (§ Approach (g)). A
  default-tag lint/vet run then sees them with no in-universe caller, and a
  drafting of this record left two ways of keeping the file non-dead as an
  unresolved `or`. **Resolved: the file carries a small untagged self-test of
  `writeBenchReport`'s zero-iteration refusal, and the two legs' unix-only
  helpers MUST NOT move into it.** Three reasons, in order of force:

  1. **Darwin.** The untagged file's imports stay `encoding/json`, `os`,
     `testing` — portable by construction, which is what § Global
     Constraints' Darwin cross-check requires of an untagged file. The
     rejected branch is the one that can break that gate: the microVM leg's
     helpers are inherently unix-only (they touch `VM.PSS()`, V7's pidfiles,
     `/proc`), so hoisting them into an untagged file drags unix-only surface
     into every `GOOS`. Verified this session that the hazard is real **and**
     that the gate must be `go vet` rather than `go build`: a scratch package
     whose untagged `_test.go` names
     `syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}` passes
     `CGO_ENABLED=0 GOOS=darwin go build` (which does not compile `_test.go`
     files at all) and fails `CGO_ENABLED=0 GOOS=darwin go vet` with
     `unknown field Pdeathsig in struct literal of type syscall.SysProcAttr`.
     § Global Constraints is corrected accordingly.
  2. **It earns its place as coverage, not as linter appeasement.** The
     zero-iteration refusal is already a required proving mutation of W6's
     vacuity paragraph below, so the self-test is that mutation's permanent
     positive form rather than a dead-code workaround.
  3. **It leaves the helpers where they already are.** The shared-helper
     branch would make the untagged file a real API surface both tagged legs
     compile against — a structural commitment graded by § Global
     Constraints' lint floors (golangci-lint 2.13.2; nilaway) — whereas the
     self-test is one hermetic test and changes no other file's shape.

  **Load-bearing placement constraint — one package is what makes one
  process.** BOTH bench legs and the untagged report writer MUST live in
  **`internal/runtime`**. `go test` compiles and runs one test binary *per
  package*, so a leg moved to `package runner` would be a second process
  with its own `writeBenchReport` call, and since the writer writes a whole
  `BenchReport` the second clobbers rather than merges — silently
  re-opening the absent-baseline hole OQ-3 exists to close. `internal/runtime`
  is also where the podman leg's dependencies already are: `podmanUsable()`
  and the image recipe the baseline reuses are `package runtime`
  (`lifecycle_test.go:1-3` is `//go:build podman` / `package runtime`,
  `podmanUsable` at `:56-60`). This is a constraint, not a preference — see
  § Global Constraints, which restates it, and § Approach (g), which the
  single-process premise rests on.

  Consumes `VM.PSS()` via the `guestVM` seam
  (`microvm_lifecycle.go:105-113`) — **best-effort by design**: it drops a
  process whose `smaps_rollup` is unreadable
  (`if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) { continue }`,
  `internal/runtime/microvm/launch.go:471-484`), so `PSSKB` is a map whose
  MISSING KEYS ARE MEANINGFUL and `PSSIncomplete` names them (see § Approach
  (g)). **`PSSKB`'s key domain IS `VM.PSS()`'s child-name set** —
  `out[c.name] = pss` (`launch.go:485`), i.e. `cloud-hypervisor`
  (`launch.go:217`), `virtiofsd` (`:160`) and `passt` (`:180`) — so `PSSKB`
  and `PSSIncomplete` carry **binary** names, never V7's role-name
  vocabulary (§ Approach (g)'s two-vocabularies table), and a child rename
  reds against that one definition. Also consumed: the env knob
  `COMPASS_MICROVM_BENCH_OUT`; and V7's
  `compass.microvm.boot.duration` instrument for the metrics cross-check,
  read as a `metricdata.Histogram[float64]` whose points are
  `metricdata.HistogramDataPoint[float64]` (naming the type so the shape is
  compile-checked rather than assumed —
  `metricdata/data.go:102-129`), via the `trace_test.go:209-220` harness
  pattern. Both legs are run in ONE process by the invocation W7 owns
  (`-tags "microvm podman" -run
  "TestMicroVMBenchmark|TestPodmanBaselineBenchmark" ./internal/runtime/`),
  which — together with the one-package constraint above — is what lets a
  single `writeBenchReport` call carry both backends' iterations.
- **Test cycle:** 1 warmup + 5 measured boots per backend; every raw sample
  in the report (the warmup's sample is excluded from the report's
  `iterations`); JSON written iff the env knob is set; the manual-reader
  cross-check asserts **exactly ONE**
  `metricdata.HistogramDataPoint[float64]` on
  `compass.microvm.boot.duration` whose attribute set is *precisely*
  `{outcome: ok}` and whose **`Count == 6`**. Six is a count of
  *recordings*, not of points: a histogram aggregates every recording
  sharing one attribute set into a single point carrying `Count`
  (`metricdata/data.go:102-129`), so `len(DataPoints)` here is 1 and
  asserting 6 points would be red on the first honest run. The protective
  comment is aimed at the `Count` and states its decomposition — "`Count`
  = 6 recordings = 1 warmup + 5 measured, all through the runtime
  constructed after the reader was installed" — so a change to the warmup
  count cannot quietly turn the check into an off-by-one; a comment naming
  "the sixth point" would pin a quantity that never exists. The warmup is
  counted because it runs `Start` through the same runtime, constructed
  after the reader was installed as the global provider, and V7's
  instrument fires per `Start`. (Asserting `Count == 5` would be red on the
  first honest run; the alternative — a throwaway runtime for the warmup,
  constructed before the reader is installed — is rejected to keep one
  runtime across warmup and measurement, which is the point of warming up.)
  The attributes are asserted exhaustively rather than searched, per the
  `dispatchedCounts` idiom (`trace_test.go:253-256`), so a stray attribute
  splits the timeseries into a second point and reds loudly instead of
  leaving a found-point `Count` lookup satisfied.

  Vacuity: a benchmark cannot "fail-open" on its numbers, but it CAN report
  vacuously — a zero-iteration report, a baseline that silently vanished, or
  a PSS sample silently missing a process (F-shape: the report exists,
  parses, uploads, and certifies nothing). The writer refuses (`error`) a
  report with zero iterations; `Baseline` is a mandatory enum (`present` /
  `absent:<reason>`) and never empty; every measured microVM iteration MUST
  carry **the required PSS key set § Approach (g) defines once** — the
  `cloud-hypervisor` and `virtiofsd` keys, per `VM.PSS()`'s
  `out[c.name] = pss` (`microvm/launch.go:485`) with those names at
  `launch.go:217` and `:160` — whose absence IS a real fault (unlike passt's
  expected drop-out, § Approach (g)), with any missing key listed in
  `PSSIncomplete`; and the written report MUST contain
  iterations for **both** backends — asserted on the backend set, not merely
  on `Baseline == "present"`, so a split-process regression reds instead of
  clobbering. Proving mutations: skip the measured loop (N=0) — the writer
  MUST error and the test MUST go red (the untagged writer self-test of
  Interfaces above is that same refusal in permanent positive form); run the
  bench invocation with `-tags microvm` alone — `Baseline` MUST come back
  `absent:*` AND the both-backends assertion MUST go red, which together is
  what would have caught the structurally-absent baseline (whether that reds
  the lane is OQ-3); and, for the required-key assertion, **run the drop
  mutation ONCE PER REQUIRED KEY — drop `cloud-hypervisor`, then drop
  `virtiofsd` — each MUST redden independently.** Per-key is not belt-and-
  braces: a single-key drop was this row's own defect, because a two-part
  assertion whose mutation exercises only one part reddens exactly as
  promised while the other part stays broken and the executor records the
  mutation as passed. Per-key binds the mutation to the whole key set rather
  than to half of it.

  Also in W6's cycle, because W6 is what introduces the untagged file:
  `CGO_ENABLED=0 GOOS=darwin go vet ./internal/runtime/` must stay green
  (§ Global Constraints — note it is `go vet`, not `go build`, that
  typechecks a `_test.go` file).
- **Depends:** V7 merged (the metric instrument) — **V7 only; V6 is not a
  dependency of this task**, matching the Plan preamble's table.
  Independent of W1-W5.

### W7 — the CI lane: acceptance sweep + benchmark report (all cycles)

Extends the existing `microvm` job (`ci.yml:624-850`) per § Approach (h).

- **Interfaces:** consumes the existing job's steps (KVM udev + userns
  sysctl `ci.yml:741-746`; `go test -tags microvm` sweep `ci.yml:806`;
  package-ran guard `ci.yml:836-849`) and W6's report contract. Produces:
  a benchmark step running both bench legs in ONE process **and one
  package**, with the package pattern spelled out because the invocation is
  otherwise not runnable (§ Approach (g)):

  ```sh
  go test -tags "microvm podman" \
    -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark" \
    ./internal/runtime/
  ```

  run from `go/` with
  `COMPASS_MICROVM_BENCH_OUT=$RUNNER_TEMP/microvm-bench.json` exported.
  Dropping `./internal/runtime/` makes the step fail at setup on its first
  execution (`no Go files in <workspace>/compass/go` / `FAIL . [setup failed]`,
  executed this session); widening it to `./...` would run but split the two
  legs across per-package test binaries, so the second
  `writeBenchReport` clobbers the first — which is why both legs are
  co-located in `internal/runtime` (§ Approach (g), W6 Interfaces,
  § Global Constraints). Then: a render step appending the per-iteration
  markdown table + medians to `$GITHUB_STEP_SUMMARY`; an
  `actions/upload-artifact` step publishing `microvm-bench-report` —
  **v7.0.1, SHA-pinned with the version in a trailing comment** per the
  workflow's discipline (`ci.yml:649,655`) and reusing the repo's existing
  pin for this action (`release.yml:417`), with an explicit
  `retention-days: 90` and `if-no-files-found: error`, and the job's
  `permissions` requirement verified on the dry run rather than assumed
  (full rationale in § Approach (g) "Where it lands"); re-validated
  `timeout-minutes` for the grown suite; and a **per-test presence check**
  step.

  The presence check is a W7 deliverable in its own right, because the
  existing package-ran guard cannot catch a dropped test file. That guard is
  package-granular: it derives its package set with
  `grep -rl 'microvmtest\.Require' … | xargs -r -n1 dirname | sort -u` and
  then requires a matching `^ok  …/$pkg` line per package
  (`ci.yml:836-849`). Every V8 file lands in `internal/runtime` or
  `internal/runner`, and both packages *already* contain many
  `microvmtest.Require` callers (`egress_inguest_microvm_test.go`,
  `contract_microvm_test.go`,
  `e2e_vsock_gateway_microvm_test.go`, …) — so a V8 file that silently
  drops out of the sweep removes neither its package from the set nor its
  package's `ok` line, and the guard stays green while the acceptance
  property was never asserted. The check closes that at test granularity:
  it derives the expected `Test` name list **from source** rather than from
  a hand-maintained literal — the same drift-proofing idiom the existing
  guard uses for its package set — by grepping the V8 acceptance files for
  `^func Test`, then checks each name against `/tmp/microvm.log`.

  **The acceptance condition is `--- PASS:`, and a skip is a failure — no
  `=== RUN` fallback.** An earlier drafting accepted `--- PASS: <name>` "or
  at minimum `=== RUN   <name>`", which is vacuous in two distinct ways,
  both verified against `go test -v` this session:

  ```text
  === RUN   TestAlpha
  --- SKIP: TestAlpha (0.00s)
  === RUN   TestBeta
  === RUN   TestBeta/sub
  --- SKIP: TestBeta/sub (0.00s)
  --- PASS: TestBeta (0.00s)
  ```

  A skipped test still prints `=== RUN`, so the fallback credits a test that
  ran zero assertions; and a parent whose every subtest skips prints
  `--- PASS` at the parent, so even the strict form credits a suite that
  asserted nothing. That matters here rather than theoretically: W1's
  metadata row is *designed* to skip when its host-side precondition does
  not hold (§ Approach (c)), so the mechanism distinguishing an intended
  skip from a whole dropped suite cannot itself be skip-blind. The
  `COMPASS_REQUIRE_MICROVM=1` skip-text guard does not cover this either: it
  greps only for `microvmtest.Require`'s specific
  `/dev/kvm is not openable…` message, `sed`-derived from `microvmtest.go`
  (`ci.yml:825-835`), so a `t.Skip` from any other cause — the metadata
  precondition, a missing env, a false `podmanUsable()` — is invisible to
  it. The check therefore:
  1. requires `--- PASS: <name>` for every derived name (the `=== RUN`
     fallback is dropped);
  2. FAILS on any `--- SKIP:` line naming a derived test **or any subtest
     beneath one** (`--- SKIP: <name>/…`, at any depth — that is what closes
     the all-subtests-skipped parent that still prints `--- PASS`);
  3. except for a small explicit **skip allowlist** — today exactly W1's
     metadata leg — so an intended skip is a declared, reviewed entry and
     every other skip is loud. An allowlisted name still must appear in the
     log (as `--- PASS:` or `--- SKIP:`), so a dropped file is caught even
     for the row permitted to skip.

  Equivalently for the subtest case, the check may assert that a listed
  parent has at least one passing subtest; the `--- SKIP:`-beneath-a-listed-name
  scan is the simpler shape over the same capture and is what W7 specifies.

  `/tmp/microvm.log` is the file the sweep redirects its `-v` output into
  and which the existing guard step already reads back (the sweep is
  `go test -tags microvm -race -v -timeout 15m ./... >/tmp/microvm.log 2>&1`
  at `ci.yml:806`; the guard greps the same path, `ci.yml:831,844`), so the
  check is a third step over the same capture and needs no change to the
  sweep.

  **The derived file list is pinned by name, not described.**
  `grep ^func Test` reads a file's text regardless of its build tags — the
  existing guard relies on exactly that ("grep reads file text regardless of
  build tags, so it finds the tagged canary too", `ci.yml:822-823`) — so the
  scoping is done by choosing WHICH FILES to grep. The V8 acceptance files
  W1-W6 name, enumerated: `internal/runtime/microvm_intertenant_microvm_test.go`
  (W1), `internal/runtime/microvm_escalation_microvm_test.go` (W2),
  `internal/runtime/microvm_agent_lifecycle_microvm_test.go` plus W3's
  addition to `internal/runner/e2e_vsock_gateway_microvm_test.go` (W3),
  `internal/runtime/microvm_failure_microvm_test.go` and
  `internal/runner/e2e_vmm_death_microvm_test.go` (W4), W5's two rows (its
  `package runtime` `//go:build unix` file and the `package main` row in
  `main_test.go`), and `internal/runtime/microvm_bench_microvm_test.go` (W6).
  The podman baseline leg is deliberately absent from the list: it runs in
  W7's separate single-process bench invocation and is covered by that step's
  own report assertions, not by this log.

  **Empty-list vacuity guard.** If the derivation yields an empty name list
  the step FAILS LOUDLY, mirroring the existing guard's own check ("no
  package calls microvmtest.Require — the harness moved and this guard is
  vacuous", `ci.yml:838-841`). Without it a bad file pattern derives no names
  and the check passes trivially — the same F-shape the check exists to
  close.

  **The OQ-8/OQ-9 interaction, stated because source-derivation would
  otherwise look like it reds the lane on the record's own gating.** The two
  gates bite at different granularities, and only the first touches this
  check:

  - **OQ-8 blocks a whole test function.** W1's
    `TestInterTenantVsockIdentityBound` "must not be implemented before that
    OQ rules" (W1 Interfaces), so until it does the function does not exist,
    **`grep ^func Test` does not derive it, and the check does not demand
    it** — a name present nowhere in source is not a missing `--- PASS:`.
  - **OQ-9 blocks a ROW inside a test that does exist.** W2's
    `TestEscalationProbeRefusedOnLoopback` is produced and its CID-2
    positive control runs; only the CID-1 exit-code row is gated ("Do not
    implement this row until OQ-9 rules"). So that name IS derived and MUST
    appear as `--- PASS:` — correctly, since the test really does run. The
    gated row must be **absent**, not `t.Skip`ped: a skipped subtest beneath
    a derived name is exactly what the depth-scoped `--- SKIP: <name>/…`
    scan reds, and W1's metadata leg is the only allowlisted skip.

  This is a reason the list must be source-derived
  rather than a hand-maintained literal of the record's *intended* test set:
  a literal would name OQ-8's blocked test and red the lane for honoring the
  gating the record deliberately imposes. It also gives the mechanism its
  payoff, worth stating because it is the point: **when OQ-8 rules and its
  test is added, source-derivation enrols it in the presence check
  automatically**, with no CI edit — so the post-ruling test cannot land and
  then silently drop out of the sweep. (The converse the record accepts:
  while OQ-8 is open, W1's Interfaces "Produces" list names a function that
  does not exist yet and nothing checks that it is ever added. That is the
  OQ's own gate to close on ruling, not this step's.)

  Failure lists the missing names, and separately the unexpectedly-skipped
  ones.
- **Test cycle:** a dry-run dispatch of the extended job on a branch: every
  named V8 acceptance `Test` function appears as run-and-passed in the
  captured log (the per-test presence check above); the artifact exists and
  parses against the schema; the step summary renders; the
  `podmanUsable()` probe reports true on the runner (OQ-3 — verify, do not
  assume); and the job's `permissions` suffice for
  `actions/upload-artifact` (LOW: verified here, not assumed — see
  § Approach (g) "Where it lands"). Vacuity: the lane could green while
  asserting nothing if a new test never compiled in, or if it compiled in
  and skipped. `COMPASS_REQUIRE_MICROVM: '1'` plus the skip-text guard
  (`ci.yml:825-835`) close only the *`microvmtest.Require` skipped* shape —
  that guard greps one specific `/dev/kvm is not openable…` message — and
  the package-ran guard closes the *package never ran* shape; neither
  closes the dropped-file shape nor a `t.Skip` from any other cause, which
  is why the presence check exists and why it rejects `--- SKIP:` outright
  (rationale under the deliverable above). Proving mutations, one per
  shape:
  - *dropped file* — misspell one new test file's build tag
    (`//go:build microvm && linux` for `microvm && unix`, or
    `//go:build microvmm && unix`) so the file silently leaves the sweep:
    every other test still passes, the package still prints `ok`, the
    package-ran guard stays green, and the **per-test presence check MUST go
    red** on that file's missing test names. Running the same mutation
    against the package-ran guard alone is the control that shows why the
    check is needed: the guard does not move.
  - *non-allowlisted skip* — insert a bare `t.Skip("mutation")` at the top
    of one non-allowlisted acceptance test. The sweep still prints `ok` for
    its package, and the skip-text guard does not match (the message is not
    `microvmtest.Require`'s), so the **presence check MUST go red** on the
    `--- SKIP:` line. Under the dropped `=== RUN` fallback this mutation
    would have passed — which is what makes it this row's own discriminator.
  - *all-subtests-skipped parent* — make every `t.Run` row of one listed
    parent skip. The parent still prints `--- PASS`, so the depth-scoped
    `--- SKIP: <name>/…` scan is what reds; a parent-only `--- PASS` check
    would not move.
- **Depends:** W1-W6 (it runs them).

## Tasks

- [ ] W1 — inter-tenant probe (`package runtime`): two concurrent sessions
      on one runtime; volume/host-fs legs as a delta over PR #912's
      `TestMicroVMCrossSessionVolumeUnreachable`; host gateway probe against
      a test-opened listener; metadata probe gated on its host-side
      precondition; **vsock leg blocked on OQ-8**; per-assertion positive
      controls + recorded proving mutations
- [ ] W2 — in-guest escalation probe: static `vsockprobe` built by
      **explicit path** (`CGO_ENABLED=0 GOOS=linux go build -o <dst>
      ./internal/runtime/testdata/vsockprobe` — `testdata` is invisible to
      `./...`, so an untagged compile guard catches rot in the default
      lane), staged by writing into the existing `/workspace` host dir
      before `Start` (never a second mount, `microvm_lifecycle.go:313-330`);
      `Exec` `err == nil` asserted BEFORE any exit code (a missing probe is
      a `CodeInternal` error, never 126/127); CID-2 positive control; CID-1
      assertion's exit code (**3 vs 2**) and its mutation **blocked on
      OQ-9**; independent of V6/V7 — may land first
- [ ] W3 — S1 contract at the assembled layer: `AgentRuntime.Launch` e2e on
      microVM in `package runtime` (five podman-leg properties) with the uid
      mutation **re-aimed at a reachable, on-property one** (guestd returns
      a different NON-ZERO uid; uid-0 is refused at three layers, one of
      them host-side at `microvm_lifecycle.go:389-391`, so a uid-0 mutation
      reddens setup instead), egress allow/deny through the launched
      session, gateway-liveness leg in `package runner` beside the existing
      vsock-gateway e2e **consuming its `w3MicroVMConfig` helper** (not
      `e2eConfig`, which is unreachable from that package); V3-suite
      ratification carried by W7's per-test presence check
- [ ] W4 — failure modes: wedged-boot deadline kill — **add the base
      orphan-freedom assertion** (main asserts only fail-closed Start,
      `microvm_lifecycle_microvm_test.go:62-118`), pidfile-identity-matched
      against pids **captured during the boot window** (a post-`Start`
      re-read is vacuous — `Shutdown` deletes the pidfiles, PR #931 record
      :194-196), with the during-boot pid-liveness positive
      control, matched via **V7's own pid-identity predicate** rather than a
      second `/proc` parser (OQ-6), proven by a mutation that defeats the
      **two deterministic** teardown paths: suppress the
      `booted`/deferred-`Shutdown` block (`microvm_lifecycle.go:376-383`)
      AND pass a `context.WithoutCancel`-derived ctx into `m.launchFunc`
      (`:367`) so the children's `exec.CommandContext` binding
      (`microvm/launch.go:163,187,220`) cannot kill the VMM first — the
      third path, `Pdeathsig: SIGTERM`
      (`microvm/orphanguard_pdeathsig.go:17-19`), is **not defeatable from
      the test** (`startChild` unlocks the spawning thread on return,
      `launch.go:301-302`), which is why a green mutation run is
      recorded "could not discriminate", never a pass; plus the
      caller-deadline cancel leg
      (`package runtime`); mid-session VMM SIGKILL in `package runner` with
      typed death error, bounded stream/drain end (own
      `vm.Shutdown`-suppression mutation), peer teardown (own mutation),
      idempotent Remove
- [ ] W5 — KVM-absent hard-fail: two composed tests — D3 capability-naming
      error asserted on the value returned by the injected-`openKVM` seam
      (`package runtime`), and the startup gate's propagation of a fake's
      sentinel (`package main`); KVM-present control on the tagged twin
- [ ] W6 — benchmark: 1 warmup + 5 measured boots, raw-sample JSON report
      behind `COMPASS_MICROVM_BENCH_OUT`, podman baseline in the SAME
      process **and the same package** `internal/runtime` (or explicit
      `absent:<reason>`), writer + types in an untagged file kept
      **Darwin-clean and non-dead by a small untagged self-test of the
      zero-iteration refusal — NOT by moving the legs' unix-only helpers into
      it**, the required-PSS-key assertion over § Approach (g)'s
      once-defined set (`cloud-hypervisor` + `virtiofsd`, per
      `out[c.name] = pss`, `microvm/launch.go:485`) with the drop mutation
      run ONCE PER KEY, V7
      boot-duration cross-check asserting **exactly ONE**
      `metricdata.HistogramDataPoint[float64]` whose sole attribute is
      `outcome=ok` and whose **`Count == 6`** (1 warmup + 5 measured — a
      histogram aggregates recordings into one point per attribute set, so
      6 is a `Count`, never a point count); no thresholds
- [ ] W7 — CI lane: extend the `microvm` job with the single-process
      benchmark step (`-tags "microvm podman"`, scoped `-run`, **package
      pattern `./internal/runtime/`** — pattern-less it fails at setup,
      `./...` splits it into two processes), a per-test presence check over
      `/tmp/microvm.log` with the name list derived from source over an
      **enumerated file list** (empty list ⇒ fail loudly, mirroring
      `ci.yml:838-841`; the OQ-8/OQ-9-blocked tests are unwritten and so
      underived, and adding one on ruling enrols it automatically) that
      **requires `--- PASS:` and rejects `--- SKIP:` at any depth** under a
      listed name (small skip allowlist: W1's metadata leg),
      `$GITHUB_STEP_SUMMARY` table, `actions/upload-artifact` v7.0.1
      SHA-pinned with `retention-days: 90` + `if-no-files-found: error`
      publishing `microvm-bench-report`; re-validate timeouts; dry-run
      dispatch that verifies `podmanUsable()` and the job's
      `upload-artifact` permissions on the runner, and proves the presence
      check via the build-tag-typo, non-allowlisted-`t.Skip`, and
      all-subtests-skipped mutations

## Global Constraints

- **Go module root is `go/`.** All `go test`/`go build` invocations run from
  `<workspace>/go`.
- **Every `go test` invocation carries a package pattern.** The module root
  holds no Go files, so a pattern-less invocation fails at setup rather than
  running anything — `go test -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark"`
  from `go/` yields `# .` / `no Go files in <workspace>/compass/go` /
  `FAIL . [setup failed]` (executed this session). Every command this record
  specifies for CI or a dry run names its packages explicitly.
- **One process means one package: both bench legs live in
  `internal/runtime`.** `go test` compiles and runs one test binary **per
  package**, so the single-process guarantee § Approach (g) rests on — one
  `writeBenchReport` call carrying both backends — holds only while
  `TestMicroVMBenchmark`, `TestPodmanBaselineBenchmark`, and the untagged
  report writer are all in `internal/runtime`. A leg moved to
  `package runner` becomes a second process whose `writeBenchReport`
  clobbers the first, silently restoring `baseline: absent` forever. The
  lane's bench step is therefore
  `go test -tags "microvm podman" -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark" ./internal/runtime/`
  and W6 asserts the written report carries both backends.
- **`testdata` is invisible to package patterns — carve-out for the probe.**
  The Go tool excludes `testdata` directories when matching patterns, so
  `go build ./...`, `go vet ./...`, the moon battery, and golangci-lint
  never see `internal/runtime/testdata/vsockprobe`
  (`go list ./internal/runtime/testdata/...` → `matched no packages`,
  verified this session). It is built by explicit path only
  (`CGO_ENABLED=0 GOOS=linux go build -o <dst> ./internal/runtime/testdata/vsockprobe`),
  and because the repo-wide lint floors below cannot cover it, W2 carries an
  untagged compile guard running that same build into `t.TempDir()` so rot
  reds in the default lane rather than only in the KVM lane.
- **Two-tier build tagging.** Every KVM-touching test file carries
  `//go:build microvm && unix`, calls `microvmtest.Require(t)` as its first
  statement, and is named `*_microvm_test.go`; hermetic tests are untagged or
  `//go:build unix` and must pass on a KVM-less box
  (`go/internal/microvmtest/microvmtest.go:19-25`).
- **The devenv wrapper on every gate.** The VMM toolchain (cloud-hypervisor
  53.0, virtiofsd 1.14.0, passt 2025_09_19) is on PATH only inside devenv:
  `cd <main-clone> && direnv exec . bash -c 'cd <workspace>/go && ...'`, with
  the KVM env sourced from `.microvm-test-env.sh` (exports
  `COMPASS_TEST_GUEST_{KERNEL,ROOTFS,INITRD}`, prepends VMM bins,
  `CGO_ENABLED=1`).
- **`COMPASS_REQUIRE_MICROVM=1`** for every KVM-lane run (local and CI): a
  KVM-absent SKIP becomes a hard failure, so a green run proves the suite
  booted (`microvmtest.go:37-43`).
- **Darwin cross-check with CGO off — and the gate is `go vet`, not
  `go build`, because only `vet` typechecks untagged TEST files:**
  `CGO_ENABLED=0 GOOS=darwin go vet ./internal/runtime/` must stay green —
  untagged files must not grow unix-only imports. `go build` does not compile
  `_test.go` files, so it cannot enforce that on the untagged
  `microvm_bench_report_test.go` W6 adds (W6 Interfaces). Verified this
  session: a scratch package whose untagged `_test.go` names
  `syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}` exits 0 under
  `CGO_ENABLED=0 GOOS=darwin go build` and fails
  `CGO_ENABLED=0 GOOS=darwin go vet` with
  `unknown field Pdeathsig in struct literal of type syscall.SysProcAttr`.
  Both commands are green on `./internal/runtime/` today (executed this
  session, go1.26.5), where `go list` reports 14 darwin-visible test files
  for `vet` to typecheck.
- **Lint floors:** golangci-lint 2.13.2; nilaway under
  `GOTOOLCHAIN=go1.27.1`.
- **Citation convention:** an unprefixed `file.go:N` cites main; any claim
  about V6-branch or V7-branch code carries its `PR #912` / `PR #931`
  prefix.
- **Guest scripting rules (anti-vacuity):** the guest image has no
  `grep`/`find`/`sed` — bash 5.3, gawk, ls, cat, stat only
  (`guest-image/default.nix:325-328`); batched gawk scans open with
  `BEGINFILE { if (ERRNO) { nextfile } }` and never blanket-`2>/dev/null`
  the awk call; positionally-significant filenames are zero-padded. Every
  negative assertion ships with a positive control through the same probe
  path and a recorded proving mutation.
- **V8 defines no metrics.** All microVM instruments are V7's (PR #931
  record §(d)); V8 consumes and asserts them via
  `sdkmetric.NewManualReader()` installed as the global provider before SUT
  construction (`go/internal/delivery/trace_test.go:209-220`). Watch
  cardinality: no per-session attributes in any assertion helper.
- **No production-code changes.** V8's Go deliverables are test files, test
  data, and CI workflow edits; the frozen `ContainerRuntime` interface and
  all backend behavior are untouched. Where a proof requires a mutation, the
  mutation is transient (local build), recorded in the PR description, and
  never merged.
- **No invented budgets.** The benchmark reports raw samples and medians;
  no latency/RSS threshold appears in code, CI, or this record (Q-budget,
  microvm-runner.md:839-842).

## Open Questions

Batched per the pre-freeze rule; each is graded **load-bearing** (an executor
hits real ambiguity; blocks freeze, goes to Matt) or **non-load-bearing**
(deferred with a rationale, resolved in implementation). Five are
load-bearing — OQ-1, OQ-2, OQ-3 (regraded), OQ-8, OQ-9 — and four are not:
OQ-4 through OQ-7. **OQ-8 and OQ-9 each block a specific
assertion**, not merely the freeze: the W1 vsock row and the W2 CID-1 exit
code have no implementable shape until they rule, and their W-rows say so
inline rather than pretending a decision was made.

- **OQ-1 (load-bearing) — should passt get `--no-map-gw`, making the
  host-network boundary structural rather than firewall-only?** Today the
  guest's only barrier to the host is the in-guest nft default-deny: passt's
  argv (`go/internal/runtime/microvm/launch.go:187-195`) does not pass
  `--no-map-gw`, and passt maps the host onto the guest-visible gateway
  address by default (passt(1) documents `--no-map-gw` as the opt-out of
  that default) — so a session armed with a policy that allowlists
  `10.0.2.2` could reach host-bound listeners. The cycle-1 host-network
  probe as designed measures this honestly (it runs under a policy that
  does not allowlist the gateway), but W1's proving mutation will expose
  the single-layer reality. Options: (i) **add `--no-map-gw` to the passt
  argv** — one flag, defense in depth, the host becomes unreachable even
  through a mis-armed or future user-extended allowlist; risk: it is a
  production-code change inside a docs-then-tests milestone, and anything
  that legitimately needs the host path (nothing today — the gateway rides
  vsock, not IP, `microvm-runner.md:169-171`) would break. (ii) Keep
  firewall-only and assert it in W1 as-is; the boundary then depends on
  every future egress policy never allowlisting the gateway IP. (iii) Add
  the flag in a one-line V8-adjacent PR with its own review, keeping this
  record docs-only. **Recommendation: (iii)** — the flag is right (the
  parent's defense-in-depth posture — an optional host-side egress layer is
  named future-acceptable, microvm-runner.md:748-750) but should land as its
  own reviewed change, and W1's probe then asserts the structural layer too.
- **OQ-2 (load-bearing) — vehicle for the cycle-8 AF_VSOCK dial.** Bash
  cannot speak AF_VSOCK, and the guest image ships no socat/python.
  Options: (i) **test-built static Go probe binary staged through the
  workspace share** (§ Approach (d)) — exercises exactly the attacker
  position (an agent-uid process in the workload mount); cost: the test
  builds a Linux binary at run time (~2s, cached). **Cost correction — the
  earlier "zero guest-image change" claim on this option is false, and its
  real cost is set by OQ-9.** The *vehicle* is image-free, but the CID-1
  *dial* it performs needs the kernel's `vsock_loopback` transport, and the
  initrd's `bootModules` list is exactly `virtio_pci`, `virtio_blk`,
  `erofs`, `overlay`, `virtio_net`, `virtiofs`,
  `vmw_vsock_virtio_transport`, `af_packet`
  (`guest-image/default.nix:153-162`) — no loopback transport, and no
  loopback entry in the checked `bootModuleConfigs` list
  (`guest-image/default.nix:171-180`). So option (i) is zero-guest-image
  only under OQ-9 option (a) (assert the stronger transport-level absence);
  under OQ-9 option (b) the image must grow the module. (ii) Bake a probe
  into the guest image — faster per-run but grows the image's shipped
  surface with a test-only tool, against the image's minimal-toolbox
  posture. (iii) Have guestd itself expose a self-probe — worst: the SUT
  would be probing itself. **Recommendation: (i)**, with its guest-image
  cost read off OQ-9's ruling rather than assumed zero.
- **OQ-3 (load-bearing — regraded) — can the container baseline actually be
  present in the KVM lane, and does an absent baseline red it?** Drafted
  non-load-bearing on the assumption that the baseline is normally present
  and `absent:<reason>` is the rare case. That assumption did not hold under
  the tag split as first drafted: the lane runs
  `go test -tags microvm -race -v -timeout 15m ./...` (`ci.yml:806`), and a
  `//go:build podman` file never compiles into that invocation — so the
  baseline was *structurally* absent on every run, cycle 7's "boot-latency +
  RSS benchmark vs container baseline" (RIG-2499 acceptance row 7)
  permanently unmet, and nothing red anywhere. § Approach (g) / W6 / W7 now
  specify the single-process mechanism that makes presence possible, which
  is exactly what turns the grading question live: whether `absent` reds the
  lane is only a free deferral once the baseline *can* be present.

  **The mechanism this regrade rests on, stated exactly** — the invocation
  is `-tags "microvm podman"` plus a scoped `-run` **plus the package
  pattern `./internal/runtime/`**, with **both legs and the report writer
  co-located in that one package**. Both halves are load-bearing to the
  regrade: pattern-less, the step fails at setup and the baseline is absent
  because nothing ran; pattern-`./...` with the legs in two packages, the
  step runs as two test binaries and the second `writeBenchReport`
  clobbers the first, so the baseline is absent *and the report says
  nothing about it*. Either way presence would remain unachievable and this
  OQ would collapse back to a non-question. The co-location constraint
  (§ Global Constraints, W6 Interfaces) is what makes "one process" true
  and therefore what makes this OQ a live grading decision rather than a
  structural impossibility. W6's both-backends report assertion is the
  guard that a future split re-opens the hole loudly.

  Options: (i) **one process, one package, hard-fail on absent** —
  `go test -tags "microvm podman" -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark" ./internal/runtime/`,
  and the benchmark step fails on `baseline: absent:*` once podman is
  *verified* usable on the ephemeral KVM runner; the comparison the parent
  asked for is then actually gated, at the cost of making the KVM lane
  depend on the baseline backend's health. (ii) Same mechanism,
  warn-and-report: the report carries `absent:<reason>`, an operator reads
  it, the lane stays green — cheap, but this is precisely the posture that
  let a permanently-absent baseline hide. (iii) Drop the in-lane comparison
  and set the Q-budget from the microVM numbers alone, recording the
  container baseline as an out-of-lane one-off measurement.
  **Recommendation: (i)**, gated on W7's dry run actually demonstrating
  `podmanUsable()` true on the KVM runner — that helper merely runs a
  container (`lifecycle_test.go:56-60`), so verify, do not assume. If the
  dry run shows podman unusable there, (iii) is the honest fallback; (ii) is
  the wrong default under either outcome.
- **OQ-4 (non-load-bearing) — benchmark iteration count.** N=5 measured
  boots (plus 1 warmup) per backend keeps the lane's added wall-clock under
  ~5 minutes at the proven ~1s health latency plus teardown cost, while
  making a single outlier visible. If the measured variance proves too wide
  to set a budget from, raising N is a one-constant change guided by the
  first report's data. Deferred to that data. (Scope note: this OQ is about
  the iteration *count*, not each sample's *reading completeness* — the
  known `passt` PSS drop-out is not an open question but a settled fact of
  `VM.PSS()`'s best-effort contract (`microvm/launch.go:471-484`,
  `microvm_preflight.go:277-287`), handled in § Approach (g) by the
  `pss_incomplete` field plus W6's **required-PSS-key assertion over the key
  set § Approach (g) defines once** (the `cloud-hypervisor` and `virtiofsd`
  keys, per `VM.PSS()`'s `out[c.name] = pss`, `microvm/launch.go:485`) —
  named there rather than respelled here, since the map's keys are
  `VM.PSS()`'s child names and not V7's role-name vocabulary. Raising N does
  not make a partial sample whole.)
- **OQ-5 (non-load-bearing) — one CI job or a separate `microvm-bench`
  job?** W7 extends the existing `microvm` job: the benchmark reuses the
  same runner relaxations, nix realizations, and rollup wiring, and a
  second KVM job would pay the whole nix bootstrap twice per PR. If the
  benchmark's runtime ever crowds the 30m job timeout, peeling it into a
  peer job is mechanical (copy the setup steps, add it to the rollup
  needs-list, `ci.yml:2224`). Deferred until the measured cost demands it.
- **OQ-6 (non-load-bearing, stated assumption) — V7's consumed identifier
  set.** W4 asserts V7's distinguishable VM-death error by type and reads
  V7's per-session pidfiles by name (`vmm.pid` / `virtiofsd.pid` /
  `passt.pid`); W6 asserts V7's `compass.microvm.boot.duration` instrument
  by name. V7's record is at its review cap and *every one* of those
  identifiers may still shift — its own OQ-1 already renegotiated the
  parent's file-name sketch once (`passt.pid` over `netbackend.pid`, no
  `vsock.port` file, PR #931 record §OQ-1), so the review pressure that can
  rename the death error can equally rename a metric or a pidfile. This
  record therefore names the *contracts* and not the identifiers:

  - an `errors.As`-matchable exported type distinguishing VMM death from
    transport errors (PR #931 record §(c));
  - one pidfile per peer daemon recording `<pid> <starttime> <bootid>`
    (PR #931 record §(a));
  - **a pid-identity predicate answering "is the process at this pid still
    the one this pidfile recorded"**, exported or otherwise test-reachable —
    the same one V7's reaper rides on (`readPidfile` plus
    `(pidRecord) alive()`, with its boot-id short-circuit and starttime
    compare, PR #931 record :1147-1152). W4 consumes it rather than forking a
    second `/proc/<pid>/stat` field-22 parser in a test. **Flag, while V7 is
    still open:** V7 places these in `package microvm` and states no exported
    or test-reachable form for `package runtime` / `package runner`, where
    W4's legs live (PR #931 record :1134-1152) — so if V7 merges with no
    reachable predicate, that is a request to V7, not something W4
    re-derives;
  - a per-`Start` boot-duration histogram carrying an `outcome` attribute
    (PR #931 record §(d));
  - **a per-process PSS gauge whose `process` attribute is the ROLE name,
    renamed from `VM.PSS()`'s binary-name map key** (`vmm` for
    `cloud-hypervisor`, PR #931 record :757). The *rename* is the contract
    here, not either spelling: § Approach (g)'s two-vocabularies table
    records both surfaces, W6's required-key assertion is bound to the map's
    binary names, and W4's pidfile reads use the role names. This entry
    exists so the collision is visible at the moment an executor binds V7's
    identifiers — reading role-name prose as a map key is exactly how a `vmm`
    PSS key got into a drafting of this record.

  The executor binds each of them to whatever V7 merges. Non-load-bearing
  because every one of those assertion shapes is fixed either way.
- **OQ-7 (non-load-bearing) — proving mutations: recorded once, or a
  permanent mutation harness?** The plan runs each proving mutation during
  task development and records it as a per-assertion checklist in the PR
  description (§ Approach (b)). A permanent in-CI mutation harness (build
  N deliberately-broken guests per run) would re-prove non-vacuity forever
  but multiplies the lane's nix-build and boot cost by the mutation count.
  Deferred: the checklist discipline catches the fail-open shape at review
  time; a standing harness can be revisited if a vacuity regression ever
  slips through. Default shipped: recorded mutations, no permanent harness.
- **OQ-8 (load-bearing) — what does "probe B's vsock port" mean when B has
  no vsock address?** RIG-2499 acceptance row 1 asks that guest A's attempt
  to reach "B's vsock port" fail. Under cloud-hypervisor's hybrid vsock
  there is no such addressable thing. Both guests are given the same CID
  ("the hybrid transport addresses by socket path, not CID, so nothing
  routes on it", `go/internal/runtime/microvm_lifecycle.go:44-45`;
  `const guestVsockCID uint32 = 3`, `:46`), and a
  guest dial of CID 2 port P lands at the host AF_UNIX path
  `vsockSocket + "_" + P` ("the host-side AF_UNIX listener path a guest
  reaches by dialing AF_VSOCK (CID 2, port): cloud-hypervisor's hybrid vsock
  connects the guest's dial to the launch-time `--vsock` socket path with an
  appended `_` and the guest-side port",
  `go/internal/runtime/microvm/config.go:40-48`) — i.e. at A's own VMM's
  muxer, per session. So A cannot name B at all, and the leg as drafted in
  § Approach (c) is not implementable, for three independent reasons:
  (1) **no listener** — nothing binds `vsock.sock_1024` on the host, because
  port 1024 is direction-reversed: guestd *listens* in-guest ("serveVsock …
  listens on AF_VSOCK at the guest CID and the given port",
  `go/internal/guestd/vsock.go:75-77`) and the *host* dials it with a
  `CONNECT 1024` preamble (`go/internal/runtime/microvm/dial.go:51`). The
  only host-side listener at a suffixed path is the gateway at 1025 ("HOST
  leg: the host serves gateway.Serve over `<runtimeDir>/vsock.sock_1025`",
  `go/internal/runner/e2e_vsock_gateway_microvm_test.go:24`) — so an
  in-guest CID-2:1024 dial is connection-refused and the drafted "the call
  **succeeds**" assertion is unconditionally red. (2) **No vehicle** — the
  leg needs an in-guest h2c Connect `Health` RPC and a protobuf nonce parse;
  bash has no AF_VSOCK, and W2's `vsockprobe` contract is byte-count exit
  codes, not an RPC client. (3) **Mutation mistargeted** — pointing A's
  config at B's `VsockSocket` redirects the *host's* Health dial to B's
  guestd, so `awaitHealthy` fails during setup against the already-proven
  host-side check ("`if !bytes.Equal(resp.GetBootNonce(), nonce) { return
  fmt.Errorf("microvm: boot nonce mismatch…")`",
  `go/internal/runtime/microvm_lifecycle.go:441-445`) — it reddens main's
  nonce binding, not the in-guest probe's discrimination.
  Options: (a) **Re-aim the probe at the port that has a host listener.**
  From A, via W2's `vsockprobe` extended with a small identity-echo mode,
  dial CID 2:1025 and assert the connection terminates at *A's own* gateway
  socket: the harness serves a distinct per-session discriminator byte
  string on each session's gateway path, and A must observe A's. That is a
  real positive discriminator with a mutation that reddens this row's own
  property — swap A's and B's gateway listeners in the harness and the
  assertion MUST go red. Cost: the probe binary grows a read-and-compare
  mode, and the leg proves "the guest's dial surface is bound to its own
  session" rather than the parent's literal wording. (b) **Concede the
  structural argument at the design layer.** Record that path-addressed
  hybrid vsock offers no cross-VM route at all, assert only that A's
  guest-side dial surface is *bounded* (CID 2 connects on the two muxed
  ports; nothing else connects), and let main's host-side nonce binding
  carry per-session identity. Cheaper and honest, but cycle 1's vsock half
  reduces to a structural argument plus a negative-space assertion, with no
  positive identity discriminator inside the guest. Either way RIG-2499 row
  1's wording ("B's vsock port … fails") needs an interpretation ruling,
  because under both options nothing named "B's vsock port" is ever dialed —
  no such address exists. **Recommendation: (a)** — it is the only shape
  with an honest positive discriminator and a proving mutation that reddens
  the property actually under test.
- **OQ-9 (load-bearing) — prove the peer-CID gate live, or pin the stronger
  absence?** Cycle 8 asks that an agent-uid in-guest process dial the
  supervisor over loopback (CID 1) "and is refused"
  (microvm-runner.md:618-621), and § Approach (d)/W2 assert exit 2
  (connected-then-closed by guestd's gate). That assertion cannot pass on
  today's image — and not because the gate is broken. A CID-1
  (`VMADDR_CID_LOCAL`) connect needs the kernel's `vsock_loopback`
  transport; the initrd's `bootModules` list is exactly `virtio_pci`,
  `virtio_blk`, `erofs`, `overlay`, `virtio_net`, `virtiofs`,
  `vmw_vsock_virtio_transport`, `af_packet`
  (`guest-image/default.nix:153-162`), with no loopback entry in the checked
  `bootModuleConfigs` list (`guest-image/default.nix:171-180`, where
  `vmw_vsock_virtio_transport` maps to `CONFIG_VIRTIO_VSOCKETS`,
  `:164-170`). With no local transport registered the `connect()` fails at
  the socket layer and never reaches guestd's `Accept`, so the probe exits
  **3** (no path) under W2's
  exit-code contract and the drafted exit-2 assertion is red.
  [INFERENCE: the exact errno; the transport-absence mechanism is standard
  `af_vsock` behavior — a connect to a local CID requires a registered local
  transport.] The guest cannot self-load the module: `modprobe` needs root
  and the backend advertises `refusesRootExec: true`
  (`contract_microvm_test.go:49`). So today the supervisor is
  **structurally unreachable** from any in-guest process — strictly stronger
  than "refused" — and proving the gate *live* would mean ADDING the
  transport, widening the very escalation surface this suite certifies.
  Options: (a) **Keep loopback absent and pin the stronger property.** The
  acceptance assertion becomes exit 3 (transport-level unreachability),
  documented as strictly stronger than the parent's "refused", with guestd's
  hermetic `peerAllowed` table
  (`go/internal/guestd/supervisor_test.go:709-719`) continuing to prove the
  gate itself as defense in depth. Its proving mutation is real and
  available: a guest image built *with* `vsock_loopback` MUST flip the probe
  to exit 2 — which reddens the exit-3 assertion and simultaneously proves
  the exit-code discrimination and that the gate is what closes the
  connection once a transport exists. (b) **Add `vsock_loopback` to the
  guest image** so the parent's literal sentence is exercised end to end
  (dial connects, gate closes it, exit 2). Cost: it enables the very channel
  the cycle exists to prove unreachable, trading a structural guarantee for
  a policy one on a multi-tenant box, and it is a guest-image change inside
  a docs-then-tests milestone. The fork is the parent's frozen wording
  against the structurally stronger reality, so the ruling is Matt's, not an
  executor's. **Recommendation: (a)** — never weaken an isolation surface to
  make a test match its prose; assert the stronger property and record the
  wording divergence. Under (a) OQ-2's option-(i) guest-image cost is
  genuinely zero; under (b) it is not (see OQ-2's correction).
