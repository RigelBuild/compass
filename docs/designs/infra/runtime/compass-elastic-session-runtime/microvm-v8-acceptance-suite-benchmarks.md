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
   is *partly* covered — main already asserts that a Start against a bad
   rootfs tears down its partial boot with no orphan processes and leaves
   the session not-started (`microvm_lifecycle_microvm_test.go:58-61`) — so
   V8's delta there is narrower and specific: process *identity* verified
   against V7's pidfiles (a recycled pid must not satisfy the check) and the
   caller-deadline mid-boot cancellation, which nothing covers.

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
suite ran.

The eight RIG-2499 cycles map onto tasks as follows. Cycles marked *escalate*
consume a merged or in-flight lower-milestone suite and re-prove it at the
assembled-backend layer; cycles marked *new* have no existing coverage.

| Cycle | Property | Status on main | Task |
| --- | --- | --- | --- |
| 1 | Inter-tenant probe (volume, vsock, host fs, host metadata/net) | *escalate + new* — PR #912 already boots two sessions for the volume surface (`microvm_isolation_microvm_test.go:391-395`, `TestMicroVMCrossSessionVolumeUnreachable`) and confines a single session's traversal (`:302-304`); net-new are the host-network legs and the vsock leg (OQ-8) | W1 |
| 2 | Egress fail-closed inside the guest netns | *escalate* — `egress_inguest_microvm_test.go:37-42` already runs under the full backend and names itself V8 row (2) | W3 |
| 3 | S1 contract tests pass unchanged | *escalate* — `contract_microvm_test.go:34-69` covers `ContainerRuntime`; the `AgentRuntime.Launch` layer is podman-only (`lifecycle_test.go:1`) | W3 |
| 4 | Boot timeout killed + cleaned | *escalate* — `microvm_lifecycle_microvm_test.go:57-62` proves the corrupt-rootfs deadline AND orphan-free teardown (`:58-61`); V8's delta is pidfile-verified process *identity* during the boot window plus the caller-deadline cancel leg | W4 |
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
  non-vacuous: "no listener" is excluded by construction, so a failed
  connect is a blocked connect. Today the enforcing layer is the in-guest
  nft default-deny ruleset, because passt's launch argv
  (`launch.go:187-195`) does not pass `--no-map-gw`, and passt maps the
  host onto the guest-visible gateway address by default (passt(1):
  "If --no-map-gw or --map-host-loopback none are specified", the mapping
  is disabled — i.e. it is on otherwise). Being explicit about what this
  row measures: the session is armed with a *permissive-but-not-host*
  egress policy, so the deny that the probe observes **is the nft
  firewall** — the firewall is precisely what is under test, and the
  absence of any structural layer beneath it is what the mutation exposes.
  OQ-1 (load-bearing) asks whether to add `--no-map-gw` as that structural
  second layer; until it lands, this row certifies one layer, not two, and
  says so. Positive control: an allowlisted external raw IP is reachable
  through the same probe script. Proving mutation: arm the session with an
  egress policy that allowlists `10.0.2.2`; with no structural layer the
  connect then reaches the test's host listener and the row MUST go red —
  which is also the observation OQ-1's answer depends on.
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
`go/internal/runtime/testdata/vsockprobe/main.go`, built by the test with
`CGO_ENABLED=0 GOOS=linux go build`, copied into the session's workspace
volume (the virtio-fs share the guest already mounts), and exec'd inside the
guest as the agent uid. No production surface grows.

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
discrimination (126/127 for a missing binary, 3 for absent transport or
refused connect, 2 for accepted-then-closed). Proving mutation, per OQ-9's
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
  suite ran (see W7). W3 also adds the one missing leg: the same allow/deny
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
- **Boot timeout (cycle 4).** Two legs in W4, both `package runtime`:
  (i) the existing corrupt-rootfs deadline path escalated with
  *identity-verified* orphan assertions — main already proves orphan-free
  cleanup on this path ("a Start against a bad rootfs path must tear down
  its partial boot (no orphan processes) AND leave the session in a
  not-started state", `microvm_lifecycle_microvm_test.go:58-61`); V8's delta
  is that every recorded child pid is checked *by identity* against V7's
  per-session pidfiles (`<pid> <starttime> <bootid>`, PR #931 record §(a)),
  so a recycled pid cannot satisfy the check, and that the session runtime
  dir is removable; (ii) the genuinely uncovered leg — a caller-deadline
  mid-boot cancellation: Start with a ctx deadline shorter than a real boot;
  assert teardown kills the mid-boot VMM within a bound. Vacuity analysis:
  "no orphans" is vacuous if the check reads pids that were never written;
  the test first asserts the pidfiles exist and name live processes *during*
  the boot window (positive control). Proving mutation: comment out Start's
  deferred partial-boot teardown (`microvm_lifecycle.go:377-383`); the
  orphan check MUST go red.
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
- **How it runs.** `TestMicroVMBenchmark` (tagged `microvm && unix`,
  `microvmtest.Require(t)` first) runs a fixed warmup boot then N=5 measured
  iterations, recording every raw sample — never only an average, so outlier
  boots stay visible. The container baseline is `TestPodmanBaselineBenchmark`
  (tagged `podman`), gated on `podmanUsable()` (`lifecycle_test.go:56-60`).
- **Both legs run in ONE process, or the baseline is structurally absent.**
  The two backends live in different tag universes, and the lane's sweep is
  `go test -tags microvm -race -v -timeout 15m ./...` (`ci.yml:806`) — a
  `//go:build podman` file never compiles into that invocation. Left there,
  the baseline is not "occasionally missing" but *absent on every run*:
  cycle 7's "vs container baseline" (RIG-2499 acceptance row 7) permanently
  unmet, the report carrying `baseline: absent` forever, and nothing red
  anywhere. Nor can two invocations be merged after the fact —
  `writeBenchReport` writes a whole `BenchReport`, so a second process
  clobbers rather than merges. The benchmark therefore runs as its own
  step, one process spanning both tag universes:
  `-tags "microvm podman" -run
  "TestMicroVMBenchmark|TestPodmanBaselineBenchmark"` — the scoped `-run`
  keeps the rest of the podman suite out of the KVM lane, and one process
  means one `writeBenchReport` call carrying both backends' iterations.
  This requires podman to be usable on the ephemeral KVM runner, which W7's
  dry run must **verify, not assume** (`podmanUsable()` merely runs a
  container). Where the baseline genuinely cannot run, the report carries an
  explicit `baseline: absent:<reason>` marker rather than a silent
  microVM-only report — and OQ-3 (regraded load-bearing, precisely because
  presence is now achievable) rules on whether that reds the lane.
  Mechanical consequence: `writeBenchReport` and the report types must be
  visible to both tag universes, so they live in an untagged `_test.go`
  (W6 Interfaces notes the unused-in-default-tags wrinkle).
- **Report emission.** The benchmark writes one JSON document to the path in
  `COMPASS_MICROVM_BENCH_OUT` (unset ⇒ `t.Logf` only, so dev-box runs stay
  zero-config): schema
  `{schema: 1, host: {...}, iterations: [{backend, boot_ms, exec_ms, pss_kb: {vmm, virtiofsd, passt}}], baseline: "present"|"absent:<reason>"}`.
- **Where it lands.** The CI lane writes the JSON to the workspace, renders a
  markdown table of per-iteration numbers plus medians into
  `$GITHUB_STEP_SUMMARY`, and uploads the JSON as a build artifact
  (`microvm-bench-report`) — the durable series the Q-budget ruling reads.
  The workflow today uploads no artifacts anywhere (checked this session:
  no `upload-artifact` use in `ci.yml`), so the upload step is new and is
  pinned by commit SHA like the existing actions (`ci.yml:649,655`).
- **Metrics cross-check.** The benchmark installs
  `sdkmetric.NewManualReader()` as the global meter provider *before*
  constructing the runtime (the established pattern,
  `go/internal/delivery/trace_test.go:209-220`) and asserts V7's
  `compass.microvm.boot.duration` histogram (PR #931 record §(d) metric
  table) recorded one `outcome=ok` point per boot through that runtime —
  which is **6**, not 5: the warmup boot runs `Start` through the same
  runtime, constructed after the reader was installed, and V7's instrument
  fires per `Start`. The assertion carries a comment naming the warmup as
  the sixth point, so a future change to the warmup count cannot quietly
  turn the check into an off-by-one. V8 *consumes* V7's instrument set and
  defines no metric of its own.

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
be verified before the suites it runs exist. W3-W7 land AFTER both V6
(PR #912) and V7 (PR #931) merge, and W1 after V6 — they consume those
milestones' symbols (`isolationSession`,
`TestMicroVMCrossSessionVolumeUnreachable`, pidfiles, death errors,
metrics) and must not fork them. **W2 is the one full exemption:** it needs
nothing off main (the guestd peer-CID gate is on main,
`go/internal/guestd/vsock.go:25-29`), so it may land before either merge —
land-order freedom is the point of its independence, and the plan orders by
independent-first. Each task's own **Depends** line is authoritative.

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
  | the host is unreachable from A at the gateway address `10.0.2.2` | the connect failed for want of any listener rather than being blocked | the test itself opens the host-bound listener it dials, so "no listener" is excluded by construction; mutation: allowlist `10.0.2.2` in the session's egress policy — the connect MUST reach the test's listener and this row MUST go red |
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
  no path; built via
  `exec.Command("go", "build", "-o", ...)` with `CGO_ENABLED=0 GOOS=linux`
  env, staged into the session workspace, exec'd via
  `Exec(ctx, id, NewExecSpec("/workspace/vsockprobe", cid, port))` as the
  session's non-root uid. Consumes `guestVsockPort` (1024,
  `microvm_lifecycle.go:52`) as the dialed port.
- **Test cycle:** CID-2 dial exits 0 (positive control); the CID-1 dial's
  asserted exit code is **pending OQ-9** — drafted as exit 2
  (refused-after-accept, guestd's close-before-first-byte,
  `go/internal/guestd/vsock.go:57-59`), but on today's guest image the dial
  cannot reach guestd's `Accept` at all for want of the `vsock_loopback`
  transport, so exit 2 is red and exit **3** is the observable (and
  strictly stronger) outcome. Do not implement this row until OQ-9 rules;
  under either ruling the row asserts one specific exit code, never
  "non-zero". Vacuity: covered by the CID-2 control plus the exit-code
  discrimination (a missing probe binary is 126/127, an absent transport or
  refused connect is 3, an accepted-then-closed connection is 2 — all
  distinct). Proving mutation, per OQ-9's ruling: under (a) a guest image
  built WITH `vsock_loopback` must flip the probe from 3 to 2 — the exit-3
  assertion MUST go red; under (b), with the transport present, a guest
  image whose `peerAllowed` returns `true` unconditionally
  (`guestd/vsock.go:36-38`) makes CID-1 exit 0 — the exit-2 assertion MUST
  go red.
- **Depends:** none beyond main (the guestd gate is on main,
  `go/internal/guestd/vsock.go:25-29`); independent of W1 and of V6/V7 —
  this is the Plan preamble's one exemption, and W2's land-order freedom is
  the point of its independence.

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
- **Test cycle:** the five podman-leg properties (session boots via Launch;
  checkout dir owned by uid 1000; exec runs as uid 1000; egress allow/deny
  holds; teardown removes) plus: the gateway socket accepts a connection
  while the session lives and stops accepting after teardown. Vacuity +
  mutations: the uid assertion could pass if the exec never ran — the probe
  echoes `id -u` output and asserts `1000` literally (mutation: force
  guestd's exec uid to 0 in a local guest build — MUST go red on the uid
  echo AND on the contract suite's `refusesRootExec` row); the egress deny
  could pass on an unreachable host — the paired allow probe through the
  same script is the control (mutation: drop the deny rule from
  `NftScript()` output in a local build — deny probe MUST go red); the
  gateway-liveness assertion could pass on a stale socket file — it asserts
  a completed accept while the session lives AND a refused connect after
  teardown (mutation: skip the listener's `Close` on teardown — the
  post-teardown leg MUST go red). Cycle 2's ratification: the V3 suite
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
- **Test cycle:**

  | Assertion | Could pass while false when… | Proving mutation |
  | --- | --- | --- |
  | wedged boot killed at deadline, no orphans | pidfiles were never written, so "no live pids" is trivially true | assert pids live DURING boot first; then comment out Start's deferred teardown (`microvm_lifecycle.go:377-383`) — MUST go red |
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
- **Depends:** none; fully hermetic. Independent of W1-W4.

### W6 — boot-latency + RSS benchmark and report (cycle 7)

`microvm_bench_microvm_test.go` plus the podman baseline leg, per
§ Approach (g).

- **Interfaces:** produces `TestMicroVMBenchmark(t *testing.T)` (tagged
  `microvm && unix`) and `TestPodmanBaselineBenchmark(t *testing.T)` (tagged
  `podman`); the report writer
  `writeBenchReport(path string, r BenchReport) error` with
  `type BenchReport struct { Schema int; Host HostInfo; Iterations []BenchIteration; Baseline string }`
  and
  `type BenchIteration struct { Backend string; BootMillis int64; ExecMillis int64; PSSKB map[string]int64 }`
  (unexported to the test files — no production surface). The writer and
  both report types live in an **untagged** `microvm_bench_report_test.go`,
  because they must be visible to both tag universes (§ Approach (g)); note
  the wrinkle that a default-tag lint/vet run then sees them with no
  in-universe caller, so the file carries the two legs' shared helper
  entry points (or a small untagged self-test of the writer's
  zero-iteration refusal) to keep it non-dead under every tag set.
  Consumes `VM.PSS()` via the `guestVM` seam
  (`microvm_lifecycle.go:105-113`), the env knob
  `COMPASS_MICROVM_BENCH_OUT`, and V7's `compass.microvm.boot.duration`
  instrument for the metrics cross-check (`trace_test.go:209-220` harness
  pattern). Both legs are run in ONE process by the invocation W7 owns
  (`-tags "microvm podman" -run
  "TestMicroVMBenchmark|TestPodmanBaselineBenchmark"`), which is what lets
  a single `writeBenchReport` call carry both backends' iterations.
- **Test cycle:** 1 warmup + 5 measured boots per backend; every raw sample
  in the report (the warmup's sample is excluded from the report's
  `iterations`); JSON written iff the env knob is set; the manual-reader
  cross-check sees exactly **6** `outcome=ok` boot-duration points, and the
  assertion carries a comment naming the sixth — the warmup boot also runs
  `Start` through the same runtime, and the reader is installed as the
  global provider *before* that runtime is constructed, so V7's per-`Start`
  instrument records the warmup too. (Asserting 5 would be red on the first
  honest run; the alternative — a throwaway runtime for the warmup,
  constructed before the reader is installed — is rejected to keep one
  runtime across warmup and measurement, which is the point of warming up.)
  Vacuity: a benchmark cannot "fail-open" on its numbers, but it CAN report
  vacuously — a zero-iteration report, or a baseline that silently vanished
  (F-shape: the report exists, parses, uploads, and certifies nothing about
  the comparison). The writer refuses (`error`) a report with zero
  iterations, `Baseline` is a mandatory enum (`present` / `absent:<reason>`)
  and never empty, and the single-process invocation above is what makes
  `present` reachable at all. Proving mutations: skip the measured loop
  (N=0) — the writer MUST error and the test MUST go red; and run the bench
  invocation with `-tags microvm` alone — `Baseline` MUST come back
  `absent:*`, which is the assertion that would have caught the
  structurally-absent baseline (whether that reds the lane is OQ-3).
- **Depends:** V7 merged (the metric instrument). Independent of W1-W5.

### W7 — the CI lane: acceptance sweep + benchmark report (all cycles)

Extends the existing `microvm` job (`ci.yml:624-850`) per § Approach (h).

- **Interfaces:** consumes the existing job's steps (KVM udev + userns
  sysctl `ci.yml:741-746`; `go test -tags microvm` sweep `ci.yml:806`;
  package-ran guard `ci.yml:836-849`) and W6's report contract. Produces:
  a benchmark step running both bench legs in ONE process
  (`-tags "microvm podman" -run
  "TestMicroVMBenchmark|TestPodmanBaselineBenchmark"`, § Approach (g)) with
  `COMPASS_MICROVM_BENCH_OUT=$RUNNER_TEMP/microvm-bench.json` exported; a
  render step appending the per-iteration markdown table + medians to
  `$GITHUB_STEP_SUMMARY`; an `actions/upload-artifact` step (SHA-pinned,
  matching the workflow's pinning discipline, `ci.yml:649,655`) publishing
  `microvm-bench-report`; re-validated `timeout-minutes` for the grown
  suite; and a **per-test presence check** step.

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
  `^func Test`, then asserts each name appears as `--- PASS: <name>` (or at
  minimum `=== RUN   <name>`) in `/tmp/microvm.log` — the file the sweep
  redirects its `-v` output into and which the existing guard step already
  reads back (the sweep is
  `go test -tags microvm -race -v -timeout 15m ./... >/tmp/microvm.log 2>&1`
  at `ci.yml:806`; the guard greps the same path, `ci.yml:831,844`), so the
  check is a third step over the same capture and needs no change to the
  sweep. The derived name list is scoped to the files the sweep's tag set
  actually compiles (the `microvm && unix` acceptance files plus W5's
  untagged rows); the podman baseline leg runs in W7's separate
  single-process bench invocation and is covered by that step's own report
  assertions, not by this log. Failure lists the missing names.
- **Test cycle:** a dry-run dispatch of the extended job on a branch: every
  named V8 acceptance `Test` function appears as run-and-passed in the
  captured log (the per-test presence check above); the artifact exists and
  parses against the schema; the step summary renders; the
  `podmanUsable()` probe reports true on the runner (OQ-3 — verify, do not
  assume). Vacuity: the lane could green while asserting nothing if a new
  test never compiled in. `COMPASS_REQUIRE_MICROVM: '1'` plus the skip-text
  guard (`ci.yml:825-835`) close the *skipped* shape, and the package-ran
  guard closes the *package never ran* shape — but neither closes the
  dropped-file shape, which is why the presence check exists (rationale
  under the deliverable above). Proving mutation: misspell one new test
  file's build tag (`//go:build microvm && linux` for
  `microvm && unix`, or `//go:build microvmm && unix`) so the file silently
  leaves the sweep — every other test still passes, the package still
  prints `ok`, the package-ran guard stays green, and the **per-test
  presence check MUST go red** on that file's missing test names. Running
  the same mutation against the package-ran guard alone is the control that
  shows why the check is needed: the guard does not move.
- **Depends:** W1-W6 (it runs them).

## Tasks

- [ ] W1 — inter-tenant probe (`package runtime`): two concurrent sessions
      on one runtime; volume/host-fs legs as a delta over PR #912's
      `TestMicroVMCrossSessionVolumeUnreachable`; host gateway probe against
      a test-opened listener; metadata probe gated on its host-side
      precondition; **vsock leg blocked on OQ-8**; per-assertion positive
      controls + recorded proving mutations
- [ ] W2 — in-guest escalation probe: static `vsockprobe` binary staged via
      the workspace share; CID-2 positive control; CID-1 assertion's exit
      code (**3 vs 2**) and its mutation **blocked on OQ-9**; independent of
      V6/V7 — may land first
- [ ] W3 — S1 contract at the assembled layer: `AgentRuntime.Launch` e2e on
      microVM in `package runtime` (five podman-leg properties), egress
      allow/deny through the launched session, gateway-liveness leg in
      `package runner` beside the existing vsock-gateway e2e; V3-suite
      ratification carried by W7's per-test presence check
- [ ] W4 — failure modes: wedged-boot deadline kill with during-boot pid
      liveness control + pidfile-identity no-orphan check and the
      caller-deadline cancel leg (`package runtime`); mid-session VMM
      SIGKILL in `package runner` with typed death error, bounded
      stream/drain end (own `vm.Shutdown`-suppression mutation), peer
      teardown (own mutation), idempotent Remove
- [ ] W5 — KVM-absent hard-fail: two composed tests — D3 capability-naming
      error asserted on the value returned by the injected-`openKVM` seam
      (`package runtime`), and the startup gate's propagation of a fake's
      sentinel (`package main`); KVM-present control on the tagged twin
- [ ] W6 — benchmark: 1 warmup + 5 measured boots, raw-sample JSON report
      behind `COMPASS_MICROVM_BENCH_OUT`, podman baseline in the SAME
      process (or explicit `absent:<reason>`), writer + types in an untagged
      file, V7 boot-duration cross-check asserting **6** `outcome=ok`
      points (warmup included); no thresholds
- [ ] W7 — CI lane: extend the `microvm` job with the single-process
      benchmark step (`-tags "microvm podman"`, scoped `-run`), a per-test
      presence check over `/tmp/microvm.log` with the name list derived from
      source, `$GITHUB_STEP_SUMMARY` table, SHA-pinned artifact upload of
      `microvm-bench-report`; re-validate timeouts; dry-run dispatch that
      verifies `podmanUsable()` on the runner and proves the presence check
      via the build-tag-typo mutation

## Global Constraints

- **Go module root is `go/`.** All `go test`/`go build` invocations run from
  `<workspace>/go`.
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
- **Darwin cross-check with CGO off:**
  `CGO_ENABLED=0 GOOS=darwin go build ./internal/runtime/` must stay green —
  untagged files must not grow unix-only imports.
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
  lane is only a free deferral once the baseline *can* be present. Options:
  (i) **one process, hard-fail on absent** —
  `-tags "microvm podman" -run "TestMicroVMBenchmark|TestPodmanBaselineBenchmark"`,
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
  first report's data. Deferred to that data.
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
  record therefore names the *contracts* and not the identifiers: an
  `errors.As`-matchable exported type distinguishing VMM death from
  transport errors (PR #931 record §(c)); one pidfile per peer daemon
  recording `<pid> <starttime> <bootid>` (PR #931 record §(a)); a per-`Start`
  boot-duration histogram carrying an `outcome` attribute (PR #931 record
  §(d)). The executor binds each of them to whatever V7 merges.
  Non-load-bearing because every one of those assertion shapes is fixed
  either way.
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
