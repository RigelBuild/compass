# microVM Runner V3 — egress-in-guest

Status: PROPOSED — details the V3 milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V3,
microvm-runner.md:492-506) and its frozen networking decision D6
(microvm-runner.md:733-750).

Ledger impact: none. V3 fills behavior the parent's own D6 already ratified
(the in-guest nft arm, guestd-as-root, agent-without-NET_ADMIN); nothing here
is a new cross-cutting decision, and `docs/designs/DECISIONS.md` is untouched.

## Problem / Intent

V2b built the gate but not the arm: guestd's `Provision` transitions
`ready → provisioned` and opens the exec gate, but a non-empty `nft_script` is
a hard `CodeUnimplemented` error ("nft egress arm is V3",
`go/internal/guestd/supervisor.go:146-149`), and `MicroVMRuntime.Start` sends
`ProvisionRequest` with only `default_exec_uid` + `base_env` — no script
(`go/internal/runtime/microvm_lifecycle.go:304-310`). So today a microVM
session boots with **open egress**: passt does no filtering (D6,
microvm-runner.md:739-741), and nothing arms nft inside the guest.

V3 closes that: deliver `EgressPolicy.NftScript()` (consumed unchanged,
`go/internal/runtime/egress.go:71-107`) to guestd, run it **as guest root
before the exec gate opens**, fail the boot when arming fails, and make the
host's arm path backend-correct while leaving the podman path byte-identical
(parent § V3, microvm-runner.md:494-496). The proto surface already exists —
`ProvisionRequest.nft_script = 1` was seeded for exactly this
(`proto/compass/v1/guest_control.proto:177-180`) — so V3 is a behavior fill,
not a wire change (§(e)).

## Approach

Each subsection resolves one fork the parent's V3 plan leaves to detailing.
The load-bearing arm-routing fork is (a)-(c); every resolution is also listed
in `## Open Questions` for the pre-freeze batch, and the body designs against
the recommended option.

### (a) How `NftScript()` reaches the backend: `ContainerSpec.Egress`

Today the policy stops at the `AgentRuntime` layer: `AgentSpec.Egress`
(`go/internal/runtime/agent.go:40-42`) is consumed only by
`AgentRuntime.armEgress`, which execs the script into the running container
(`agent.go:303-309`). `ContainerSpec` — the only thing a `ContainerRuntime`
backend ever sees (`go/internal/runtime/podman.go:88-114`) — carries no egress
field. The V2b record already recorded this exact gap as V3's inheritance:
"the intended data path is `ContainerSpec` growing an egress field captured at
Create and delivered by Start's Provision call"
(microvm-v2b-guest-supervisor-exec.md:214-226).

**Resolution: `ContainerSpec` grows `Egress EgressPolicy`.**

- `AgentRuntime.createAndStart` sets it from `spec.Egress` when assembling the
  `ContainerSpec` (`agent.go:262-272`).
- `PodmanCLI` **ignores** the field entirely: `createArgs` is untouched, so the
  podman argv — and the whole podman path — stays byte-identical. Podman keeps
  arming via the post-start `armEgress` exec as before ((c)).
- `MicroVMRuntime.Create` records `spec.Egress.NftScript()` on the
  `microvmSession` (beside the `uid`/`env` it already records "for the
  Provision RPC Start issues", `microvm_lifecycle.go:96-98,164-172`).

The alternative — a distinct host→guest arm call after Start — is rejected in
(b); a distinct *host-side* delivery channel (e.g. a file in the workspace
share) is rejected outright: the script is host-assembled trusted input for a
guest-root shell, and the vsock control channel is the only surface with the
right trust direction (parent §(c), microvm-runner.md:144-151).

### (b) Who arms: `MicroVMRuntime.Start`, intrinsically — the load-bearing fork

Podman's shape is a **post-start root-capable exec**: `AgentRuntime.provision`
runs `armEgress` first among the post-start steps, exec-ing
`sh -c NftScript()` as the image default user (uid 1000) holding the
container's `CAP_NET_ADMIN` grant (`agent.go:288-309`, the grant at
`agent.go:266`). The microVM backend cannot reuse that seam verbatim: the
guest supervisor refuses uid-0 execs before any spawn
(`guest_control.proto:99-102`, `supervisor.go:428-441`), every spawned child
gets an empty capability set (`supervisor.go:55-61`), and `spec.CapAdd` is
ignored — CAP_NET_ADMIN is never granted to the workload boundary
(`microvm_lifecycle.go:138-143`; D6, microvm-runner.md:741-743). The arm must
run as **guest root inside guestd**, before the gate opens.

Two candidate owners for issuing the arm:

- **Option 1 (recommended): arming is intrinsic to `Start`.**
  `MicroVMRuntime.Start` already issues the `Provision` RPC as its final
  transactional step (`microvm_lifecycle.go:269-310`); V3 adds
  `NftScript: session.nftScript` to that same request. One RPC provisions
  *and* arms; the gate opens only when both succeed.
  - Pro: the `ContainerRuntime` contract identity holds — on podman,
    `Start` then `Exec` works with no intermediate call, and the V2b contract
    suite asserts exactly that identity on both backends
    (contract_microvm_test.go:5-9, microvm_lifecycle_test.go's
    Create→Start→Exec shape). Any design where the gate opens *outside* Start
    breaks that: an `Exec` right after `Start` would be gate-refused on
    microVM only.
  - Pro: fail-closed teardown comes for free — Start's existing
    tear-down-on-any-failure posture (`microvm_lifecycle.go:286-295`) makes a
    failed arm tear the VM down with no new code path (§(d)).
  - Con: the arm's *timing* moves relative to podman (during Start, not during
    `AgentRuntime.provision`). This is strictly earlier, so "armed before any
    exec" is preserved with margin; flagged against the parent's phrasing in
    OQ-1.
- **Option 2 (rejected): a distinct arm call between Start and the first
  exec** — e.g. `Provision` split into a gate RPC and an arm RPC, or the arm
  riding a second host call. This keeps the podman *sequence* (start, then
  arm) but leaves a window where the gate design must choose between (i) gate
  open before arm — violating the parent's "Only after a successful arm does
  the supervisor accept exec requests" (microvm-runner.md:152-155) — or (ii)
  gate closed until the extra call — breaking the Start→Exec contract identity
  above. Both horns lose; rejected.

### (c) What the host `armEgress` does on the microVM backend: probe-and-skip

With (b), the arm has already happened by the time `AgentRuntime.provision`
runs. `armEgress`'s exec — nil `User`, so the session-default uid 1000
(`podman.go:119-121`, `microvm_lifecycle.go:432-438`) with an empty capability
set on this backend (`supervisor.go:55-61`) — would not just be redundant, it
would **fail** (nft as a capability-less uid), failing every microVM provision.
It
must not run on this backend. Three candidates:

- **Option A (rejected): grow `ContainerRuntime` with an
  `ArmEgress(ctx, id, EgressPolicy) error` verb** (podman impl = today's exec
  moved verbatim; microVM impl = no-op). Clean in the abstract, but it
  violates the interface's freeze discipline — the surface was deliberately
  completed at S1 so "no interface change lands after S1"
  (`podman.go:379-388`, the `Resize` reservation) — and it touches every fake
  and the shared contract suite for a verb one backend no-ops. Largest blast
  radius of the three.
- **Option B (rejected): hoist arming into both backends' `Start`** (podman
  gains a session table to carry the spec from Create to Start, and issues the
  same exec from inside `PodmanCLI.Start`). This deletes `armEgress` entirely
  and makes always-armed a property of every backend — but `PodmanCLI` is
  stateless today and the podman path is under an explicit
  changed-nothing constraint (parent § V3 "the podman path unchanged",
  microvm-runner.md:496). Restructuring podman internals to satisfy a microVM
  milestone is the opposite of small blast radius; rejected.
- **Option C (recommended): a backend capability probe in
  `AgentRuntime.provision`.** `MicroVMRuntime` gains one exported marker
  method, `EgressArmedInGuest() bool` (returns true), NOT on the
  `ContainerRuntime` interface. `AgentRuntime.provision` type-asserts an
  unexported single-method interface and skips `armEgress` when the backend
  self-arms:

  ```go
  // in agent.go
  type inGuestEgressArmer interface{ EgressArmedInGuest() bool }

  func (r *AgentRuntime) provision(ctx context.Context, id ContainerID, spec AgentSpec) error {
      if armer, ok := r.runtime.(inGuestEgressArmer); !ok || !armer.EgressArmedInGuest() {
          if err := r.armEgress(ctx, id, spec.Egress); err != nil {
              return err
          }
      }
      // installCredentials, ensureCheckoutDir unchanged (agent.go:294-297)
  ```

  `PodmanCLI` does not implement the probe, so the assertion fails and the
  existing `armEgress` exec runs byte-identically (`agent.go:300-309`); fakes
  don't implement it either, so every existing hermetic test (e.g.
  `TestEgressIsArmedAsRootNotTheAgentUser`, agent_test.go:213-216) keeps
  passing unchanged. The frozen interface is untouched. Smallest blast radius:
  one marker method + one guarded call site.

### (d) The guest arm: guestd runs the script as root, fail-closed

`Provision` (`supervisor.go:141-169`) replaces its `CodeUnimplemented` branch
with the real arm. Handler order (all before the `stateProvisioned`
transition):

1. Validate `default_exec_uid` non-zero (existing, `supervisor.go:150-153`).
2. Check state: still refuse when not `stateReady`
   (`supervisor.go:161-163`) — this also guarantees the arm's preconditions:
   `stateReady` means net bringup completed, so the guest IP, default route,
   and `/etc/resolv.conf` exist (`go/internal/guestd/net.go:24-34`), which the
   script requires (it parses resolv.conf for the DNS carve-out and resolves
   allowlisted hosts via `getent`, `egress.go:71-79,109-115`; D6,
   microvm-runner.md:747-748).
3. **Arm** when `nft_script` is non-empty: spawn `/bin/sh -c <script>` as a
   direct child **with no `syscall.Credential`** — it inherits guestd's own
   identity, guest root with full capabilities. This is deliberately a
   separate spawn path from exec children: it never passes through
   `resolveUID`/`newCredential` (`supervisor.go:53-61,428-441`), is never
   entered in the exec table, and is not reachable from the wire as an exec.
   The child is bounded by a 120 s deadline (mirroring podman's per-command
   `defaultCommandTimeout` that bounds the same script today,
   `podman.go:391-394`) and by the RPC context. `nft`, `getent`, and `awk`
   ship in the guest rootfs (`guest-image/default.nix:321-323`); the `/bin/sh`
   the arm spawns is added as a W1 deliverable (not currently linked — the
   toolchain provides `/bin/bash`, so always-arm §(e) makes an explicit
   `/bin/sh` link load-bearing; see W1).
4. On a non-zero exit, timeout, or spawn failure: return a
   `connect.CodeInternal` error carrying the script's exit status and a
   bounded stderr tail, and **do not transition state** — the gate stays
   `stateReady`, so `requireProvisioned` keeps refusing every exec
   (`supervisor.go:415-423`). The script's own `set -eu` makes partial
   installs abort non-zero (`egress.go:76-83`), so a failed arm never leaves a
   half-open ruleset presented as success.
5. Only after a successful arm (or an empty script, §(e) hermetic carve-out):
   `stateProvisioned`, record uid/env, respond OK.

**The end-to-end fail-closed contract**, restating the parent's
(microvm-runner.md:144-155) in as-built terms: a non-empty `nft_script` that
fails to arm ⇒ `Provision` returns an error ⇒ `MicroVMRuntime.Start` returns
an error and its deferred `vm.Shutdown` tears the freshly booted VM down
(`microvm_lifecycle.go:286-295,305-310`) ⇒ `AgentRuntime.createAndStart`'s
caller sees a failed launch and the partial container is removed
(`agent.go:274-284`, proven by `TestFailedProvisionRemovesThePartialContainer`,
agent_test.go:256-259) ⇒ session start fails. At no point does an armed-less
VM serve an exec: the gate is closed the entire time, and `Provision`'s
already-provisioned refusal (`supervisor.go:157-160`) means a successful arm
can never be re-run or replaced from the wire — the re-arm surface the V8
escalation probe attacks stays closed (microvm-runner.md:618-621).

Holding `supervisor.mu` across the arm is acceptable and simplest: `Provision`
is once-per-session and Health is lock-free (`supervisor.go:122-125`). An exec
arriving mid-arm blocks on `s.mu` in `requireProvisioned`
(`supervisor.go:415-423`) and, after a successful arm, proceeds — it serializes
behind the once-only arm rather than being refused; a graceful `Signal`/Stop
also takes `s.mu` (`supervisor.go:558-563`), so it blocks for up to `armTimeout`
behind a wedged arm, bounded by the host Stop escalating to a hard VMM kill
(OQ-4).

### (e) Always-arm: the zero-value `EgressPolicy` is default-deny

`EgressPolicy` is fail-closed by construction: the zero value's `NftScript()`
is the full default-drop base ruleset with an empty allowlist
(`egress.go:29-34,109-115`) — there is no "no policy" representation.
`MicroVMRuntime.Start` therefore **always** sends
`session.nftScript` (never empty for a session created through
`ContainerSpec`), and every microVM session boots default-deny even when a
direct `ContainerRuntime` caller never set `Egress`. That is a deliberate
divergence from podman, where a caller that skips `armEgress` gets an
unfirewalled container: on this backend a silent open-egress VM is
structurally impossible, which is the stronger reading of the parent's
integrity model (microvm-runner.md:141-151).

Consequences, handled explicitly:

- The V2b conceded-divergences list (microvm-v2b-guest-supervisor-exec.md:
  580-593) gains row (7): "microVM sessions are always armed default-deny at
  Start; podman containers are armed only by `AgentRuntime`." Asserted by a
  contract-suite row like the other six.
- Existing KVM-gated suites (the contract suite, the lifecycle e2e, the boot
  benchmark — contract_microvm_test.go:28-32,73-80) will now boot armed
  guests. Their exec traffic is loopback/vsock-only, which the base ruleset
  carves out (`egress.go:109-115` loopback accept; vsock is not IP,
  microvm-runner.md:166-171), so no row regresses; W3 verifies this claim on
  hardware rather than assuming it (OQ-3).
- **Always-arm makes first-boot netfilter module autoload load-bearing.** The
  first `nft` invocation post-boot triggers on-demand netfilter kernel-module
  autoload, which the guest image supports via its `/lib/modules` tree
   (`guest-image/default.nix:338`). Because every microVM Start now arms, a
  broken autoload path fails *every* microVM launch, not only egress-using
  ones — always-arm converts a latent packaging bug into a total-backend
  outage. W3(1)/(4) is the first hardware exercise of this path and is exactly
  the test that proves the assumption holds (OQ-3).
- guestd still *accepts* an empty `nft_script` (skip the arm, provision the
  gate) — the hermetic V2b supervisor tests and any non-Linux harness drive
  `Provision` without an in-guest nft. The host production path never sends
  empty; the acceptance is a test seam, documented in the proto comment (§(f)).
  Because it opens the gate with no arm, it is a deliberate, test-only
  divergence from the parent's arm-before-gate sentence — flagged for the
  pre-freeze ruling in **OQ-7**, not resolved silently here.

### (f) Proto: no wire change — confirmed

`Provision(ProvisionRequest) returns (ProvisionResponse)` and
`ProvisionRequest{nft_script = 1, default_exec_uid = 2, base_env = 3}` exist
as seeded (`guest_control.proto:76-80,177-188`). V3 adds **no field, no RPC,
no semantic change to any existing field** — only the doc-comments that
currently say "a non-empty script is unimplemented in V2b"
(`guest_control.proto:77-79,178-180`) are updated to describe the arm.
Comment-only, `buf breaking`-safe by construction, internal-go lane only, no
regeneration semantics change.

## Global Constraints

Every task below inherits these.

- **Fail-closed arm.** A non-empty `nft_script` that does not arm successfully
  fails `Provision`, leaves the supervisor in `stateReady` (exec refused,
  `supervisor.go:415-423`), fails `Start`, and tears the VM down
  (`microvm_lifecycle.go:286-295`). No code path may open the exec gate
  before a requested arm has succeeded (parent, microvm-runner.md:152-155).
- **The agent never holds NET_ADMIN.** The arm runs as guestd's own root
  identity via a spawn path unreachable from the exec surface; every wire exec
  is non-root with an empty capability set (`supervisor.go:55-61`,
  `guest_control.proto:99-102`; D6, microvm-runner.md:741-743). `spec.CapAdd`
  stays ignored on this backend (`microvm_lifecycle.go:140-143`).
- **The podman path is byte-identical.** No change to `createArgs`, to
  `armEgress`'s exec (`agent.go:300-309`), or to any podman argv; `PodmanCLI`
  ignores `ContainerSpec.Egress` and does not implement the (c) probe. The
  existing podman suites run unchanged.
- **`EgressPolicy`/`NftScript()` consumed unchanged** (`egress.go:71-107`) —
  same script on both backends, per the parent's V3 Interfaces
  (microvm-runner.md:498-502).
- **No proto wire change.** Doc-comment updates only (§(f)); `buf lint` +
  `buf breaking` green; internal-go lane only.
- **Frozen `ContainerRuntime` interface untouched.** The (c) probe is a marker
  method on `MicroVMRuntime` + an unexported assertion in `AgentRuntime`,
  never an interface verb (`podman.go:379-388` discipline).
- **KVM-gated vs hermetic split** (V2b GC, microvm-v2b-guest-supervisor-exec.md:
  605-615): everything booting a VM carries the microvm build tag and
  `microvmtest.Require(t)`; the guestd arm logic, the spec→request threading,
  and the probe/skip logic are tested hermetically.
- **External-reference gate.** Compass-tracked files only; no private names
  beyond RIG-NNN.

## Plan

W1 (guestd arm) and W2 (host threading) are independent until W3 integrates
them; both are hermetically testable. W3 (KVM-gated egress integration) is the
milestone's acceptance gate and consumes W1+W2.

### W1 — guestd: arm `nft_script` as guest root, fail-closed

Replace the `CodeUnimplemented` branch in `supervisor.Provision`
(`supervisor.go:146-149`) with the §(d) arm: root spawn seam, 120 s bound,
`CodeInternal` + stderr tail on failure, state untouched on failure,
`stateProvisioned` only after success. Update the proto doc-comments (§(f))
and the `Provision` handler comment (`supervisor.go:137-140`).

- **Interfaces:** produces the arm seam on the supervisor,
  `armFunc func(ctx context.Context, script string) error` (a struct field
  beside `newCredential credentialFunc`, `supervisor.go:96-98`), with the
  production implementation
  `runNftScript(ctx context.Context, script string) error` spawning
  `exec.CommandContext(ctx, "/bin/sh", "-c", script)` with **no**
  `SysProcAttr.Credential` (inherits guestd's root), `CombinedOutput`
  captured into the returned error, ctx bounded by
  `armTimeout = 120 * time.Second`. Consumes
  `ProvisionRequest.nft_script` (`guest_control.proto:177-180`) unchanged.
  Handler contract: empty script ⇒ no arm (hermetic seam, §(e)); non-empty ⇒
  arm before the state transition; arm error ⇒
  `connect.NewError(connect.CodeInternal, …)` and state stays `stateReady`.
- **Test cycle (hermetic, no KVM):** with an injected `armFunc` — (1)
  non-empty script invokes the seam with the exact script bytes, and success
  transitions to `stateProvisioned`; (2) seam error ⇒ Provision returns
  `CodeInternal`, `requireProvisioned` still refuses Exec/ExecStream, and a
  retried Provision may run (state still `stateReady`); (3) empty script skips
  the seam (the V2b hermetic behavior, preserved). **This task REPLACES the
  existing non-empty→`CodeUnimplemented` assertion** (`supervisor_test.go:114-121`):
  post-W1 a non-empty script invokes `armFunc`, so that row is rewritten into
  the seam-invoked/`CodeInternal` assertions of (1)/(2) — W1 must not leave the
  old `CodeUnimplemented` expectation, which would fail against the new arm.
  (4) already-provisioned refusal still holds after a successful arm (no wire
  re-arm). Plus a real-`/bin/sh` row: `armFunc = runNftScript` with an
  `exit 7`-style script ⇒ error carries the exit status (no root or nft
  needed to prove the failure path; the host's `/bin/sh` always exists, so
  this row cannot guard the *guest*'s `/bin/sh` — only W3/KVM can, which is
  why the guest link below is a MUST, not a test-guarded SHOULD). **W1 MUST
  also add the `/bin/sh` contract link to the guest image** beside the
  `nft`/`getent`/`awk` links (`guest-image/default.nix:321-323`):
  `ln -sf ${pkgs.bashInteractive}/bin/sh $out/bin/sh` (bashInteractive is
  already in the rootfs closure via the toolchain, so zero added closure — W1
  verifies it provides `bin/sh`; else the busybox `sh` the initrd already uses,
  `guest-image/default.nix:221`). Under always-arm (§(e)) EVERY microVM Start
  spawns `/bin/sh -c <script>`, so a missing guest `/bin/sh` is a
  total-backend outage, not an egress-only one — the same hazard class as the
  netfilter-autoload assumption, so the link is a load-bearing W1 deliverable.

### W2 — host: thread `spec.Egress` to `ProvisionRequest.nft_script`; probe-and-skip `armEgress`

The §(a)+(c) host half: `ContainerSpec.Egress`, the session capture, the
Start-intrinsic delivery, the `AgentRuntime` probe.

- **Interfaces:** produces
  - `ContainerSpec.Egress EgressPolicy` (new field, `podman.go:88-114`;
    doc-comment states podman ignores it — the podman arm rides
    `AgentRuntime.armEgress`);
  - `microvmSession.nftScript string` recorded in `MicroVMRuntime.Create` as
    `spec.Egress.NftScript()` (`microvm_lifecycle.go:164-172`);
  - `MicroVMRuntime.Start`'s Provision call gaining
    `NftScript: session.nftScript` (`microvm_lifecycle.go:305-308`);
  - **launch + client seams on `MicroVMRuntime` so `Start` is hermetically
    testable** — two unexported fields mirroring the supervisor seam pattern
    (`newCredential credentialFunc`, `supervisor.go:96-98`; W1's `armFunc`):
    - `launchFunc func(context.Context, microvm.BootConfig) (guestVM, error)`
      defaulting to a thin adapter over `microvm.Launch` (real signature
      `func(context.Context, microvm.BootConfig) (*microvm.VM, error)`,
      `microvm/launch.go:111` — NOT `*MicroVMConfig`). `guestVM` is a new
      unexported interface. Its method set is **not** just what `Start` calls —
      it retypes the shared field `microvmSession.vm` (`*microvm.VM` →
      `guestVM`), so it MUST cover every method invoked on that field across the
      package (across ALL build tags, incl. `//go:build microvm` tests). That
      set is **four**: `Health(context.Context) (*compassv1.HealthResponse,
      error)` and `Shutdown(context.Context) error` (called by
      `Start`/`awaitHealthy`, `microvm/launch.go:335,350`); `WaitVMMExit(time.Duration)
      bool` (`microvm/launch.go:413`), which `MicroVMRuntime.Stop` calls on
      `session.vm` before `Shutdown` (`microvm_lifecycle.go:531`); **and**
      `PSS() (map[string]int64, error)` (`microvm/launch.go:456`), which the
      Q-budget contract test calls as `session.vm.PSS()`
      (`contract_microvm_test.go:108`, `//go:build microvm && unix`, package
      `runtime`) — omitting either breaks that caller's compile (the same
      field-retype hazard for both, one in prod `Stop`, one in a build-tagged
      test). `*microvm.VM` satisfies all four as-is.
      `awaitHealthy`'s parameter also retypes `vm *microvm.VM` → `vm guestVM`
      (`microvm_lifecycle.go:334`),
      since `Start` passes the `launchFunc`-returned `guestVM` into it (it calls
      only `vm.Health`, already in the interface). The interface is required,
      not cosmetic: `vm.Health` itself re-dials via `GuestClient`
      (`microvm/launch.go:336`), and `*microvm.VM` has all-unexported fields
      with no exported test constructor — so a fake handle behind the interface
      is the only way to answer `Health` from package `runtime`;
    - `newGuestClient func(socket string, port uint32) compassv1internalconnect.GuestControlClient`
      defaulting to `microvm.GuestClient` (`microvm/dial.go:116`, already returns
      the interface), routing the Provision client `Start` builds from
      `session.cfg` (`microvm_lifecycle.go:304`) so a fake client answers
      `Provision` with no real vsock dial. Scope: this seams `Start`'s Provision
      client only; `Stop`'s own `GuestClient` dial (`microvm_lifecycle.go:525`,
      for the graceful `stopGuest` Signal) stays a direct real dial, out of W2's
      hermetic-Start scope — a future hermetic Stop test would seam it then.
    Together the fake `guestVM` + fake `GuestControlClient` make row (4)
    build and run with no real VMM and no real dial. This is the fold for the
    BLOCKER: today `Start` unconditionally spawns a real cloud-hypervisor and
    re-dials the guest, so test row (4) below is not buildable without both
    seams. Rejected alternative: move row (4) to W3 KVM — that surrenders W2's
    independent hermetic proof of the script-delivery + fail-Start contract, so
    the seams are preferred;
  - `func (m *MicroVMRuntime) EgressArmedInGuest() bool { return true }`
    (marker, NOT on `ContainerRuntime`);
  - the unexported probe in `agent.go`:
    `type inGuestEgressArmer interface{ EgressArmedInGuest() bool }`, checked
    at the top of `AgentRuntime.provision` (`agent.go:290-293`) to skip
    `armEgress` when satisfied; `armEgress` itself unchanged
    (`agent.go:300-309`); plus a one-line pointer comment beside the
    `ContainerRuntime` freeze note (`podman.go:379-388`) naming
    `inGuestEgressArmer`, so a future backend — or a `ContainerRuntime`
    decorator, which would otherwise swallow the marker and silently re-enable
    `armEgress` on the microVM backend (a loud but hard-to-diagnose launch
    failure) — discovers the probe;
  - `AgentRuntime.createAndStart` setting `Egress: spec.Egress` in the
    `ContainerSpec` literal (`agent.go:263-272`).
  Consumes `EgressPolicy`/`NftScript()` unchanged.
- **Test cycle (hermetic):** (1) a fake runtime WITHOUT the marker still
  receives the `armEgress` exec (existing
  `TestEgressIsArmedAsRootNotTheAgentUser` and
  `TestLaunchOrdersStagesEgressBeforeCheckoutDir`, agent_test.go:146-149,
  213-216, green unchanged — the podman-shape regression guard); (2) a fake
  runtime WITH the marker receives NO nft exec and provision proceeds to
  credentials/checkout; (3) `MicroVMRuntime.Create` records the zero-value
  policy's full default-deny script (never empty, §(e)); (4) via the
  `launchFunc` + `newGuestClient` seams — a fake `guestVM` whose `Health`
  reports ready with the minted nonce, and a fake `GuestControlClient` that
  records the `ProvisionRequest` — a `Start` test asserts the `ProvisionRequest`
  carries the recorded script verbatim and that a Provision error fails `Start`
  with the fake handle's `Shutdown` called; hermetic, no real VMM and no real
  vsock dial (NOT modelled on `serveFakeGuest`, which returns a `*GuestExec`
  over a plain unix listener that does not speak the vsock CONNECT preamble and
  cannot answer `Health`); (5) podman argv snapshot: `createArgs` output for a
  spec with `Egress` set is byte-identical to before the field existed.

### W3 — KVM-gated in-guest egress integration + V8 alignment

The parent's V3 test cycle verbatim (microvm-runner.md:503-506) plus the §(e)
always-arm verification, as a `//go:build microvm`-tagged suite beside
`microvm_lifecycle_microvm_test.go` / `contract_microvm_test.go`, each test
opening with `microvmtest.Require(t)`.

- **Interfaces:** consumes W1+W2 through the public `MicroVMRuntime` /
  `AgentRuntime` surfaces and the `microvm` package's direct-dial harness
  (`microvm.Launch` + `GuestClient`, the boot_microvm_test.go:155-159
  pattern) — no new production code. Produces the KVM-gated test files only.
- **Test cycle (KVM-gated):**
  1. **Allowlisted reachable / non-allowlisted blocked, both families:** boot
     a session whose `ContainerSpec.Egress` allowlists one real host; in-guest
     execs (agent uid) show the allowlisted host connects and a
     non-allowlisted raw IPv4 and IPv6 destination time out — mirroring the
     podman lifecycle proof (lifecycle_test.go:137-140) inside the guest
     netns.
  2. **Arm-failure ⇒ teardown ⇒ start fails:** drive guestd directly
     (`microvm.Launch` + `GuestClient`) with a Provision whose script is
     `exit 1`: the RPC errors, a follow-up Exec is gate-refused; then at the
     runtime layer assert a failed Provision propagates out of Start with the
     VM torn down (no VMM process, socket dir cleaned — the
     Start-teardown assertions of microvm_lifecycle_microvm_test.go:5-9).
  3. **Post-arm agent-uid exec cannot alter the ruleset:** `nft flush ruleset`
     as the agent uid exits non-zero (empty capability set,
     `supervisor.go:55-61`); the allow/deny behavior of (1) still holds
     afterward.
  4. **Always-arm holds and regresses nothing:** a session created with the
     zero-value policy boots default-deny (external egress blocked), and the
     existing KVM contract suite + lifecycle e2e stay green over armed guests
     (the §(e)/OQ-3 verification); the V2b divergence list gains its row (7)
     contract-suite assertion.
  5. **V8 alignment:** confirm rows (2) and (8) of the parent's V8 cycle
     (microvm-runner.md:609-621) are satisfiable by these tests — (2) is this
     suite run under the full backend; (8)'s re-arm half is covered by W1's
     already-provisioned refusal plus the peer-CID listener V2b built. No new
     V8 scope lands here; W3 leaves a pointer comment in the suite header.

## Tasks

- [ ] W1 — guestd `Provision` arms `nft_script` as guest root (replaces
      `CodeUnimplemented`), fail-closed, gate stays closed on failure
- [ ] W2 — `ContainerSpec.Egress` threaded Create→Start→`ProvisionRequest`;
      `AgentRuntime.provision` probe-and-skips `armEgress` on self-arming
      backends (podman path byte-identical)
- [ ] W3 — KVM-gated in-guest egress integration suite (allow/deny both
      families, arm-failure teardown, agent-uid `nft flush` refused,
      always-arm verification) + V8 alignment

## Open Questions

Batched for the pre-freeze ruling; the body designs against each
recommendation.

- **OQ-1 (load-bearing) — arm timing vs the parent's phrasing.** The parent
  describes V3 as "`AgentRuntime.provision`'s arm path routed through it
  [the Provision step] on the microVM backend" (microvm-runner.md:494-496),
  which could be read as the arm happening at `AgentRuntime.provision` *time*
  (post-Start), as on podman. As built, V2b buried the gate-opening Provision
  RPC inside `Start` (`microvm_lifecycle.go:304-310`), and the contract suite
  freezes Start→Exec identity across backends — so the only consistent
  completion is arming **inside Start** and `provision` skipping `armEgress`
  (§(b) Option 1, §(c) Option C). This *strengthens* the parent's
  "armed before exec acceptance" (microvm-runner.md:152-155); it is flagged
  because the parent record is frozen and the literal routing differs.
  **Recommendation:** ratify Start-intrinsic arming as the correct reading.
- **OQ-2 (load-bearing) — the (c) probe mechanism.** Marker-method probe
  (recommended, §(c) Option C) vs growing the frozen `ContainerRuntime`
  interface (Option A). The probe keeps the interface frozen and the blast
  radius at one call site; the interface verb is the more discoverable shape
  but contradicts the S1 no-interface-change discipline
  (`podman.go:379-388`) and touches every fake. **Recommendation:** Option C.
- **OQ-3 (load-bearing) — always-arm on the microVM backend (§(e)).** Every
  microVM Start arms at least default-deny, including direct
  `ContainerRuntime` callers (the KVM contract/e2e suites), a conceded
  divergence (7) from podman. Risk: an existing KVM row that needs external
  egress would start failing — believed none (exec traffic is
  loopback/vsock), verified on hardware by W3(4) before freeze is exercised.
  Fallback if a row genuinely needs egress: that row sets an allowlisting
  `Egress` on its spec, not a bypass. **Recommendation:** always-arm.
- **OQ-4 (non-load-bearing) — holding `supervisor.mu` across the arm.**
  Simplest and safe (§(d)): Provision is once-per-session and Health is
  lock-free (`supervisor.go:122-125`). Two precise notes on the lock's reach,
  neither changing the tag: an exec arriving mid-arm blocks on `s.mu` in
  `requireProvisioned` (`supervisor.go:415-423`) and, after a *successful* arm,
  proceeds — it serializes behind the once-only arm rather than being "refused
  anyway"; and a graceful `Signal`/Stop taking `s.mu` (`supervisor.go:558-563`)
  blocks for up to `armTimeout` behind a wedged arm, bounded by the host Stop
  escalating to a hard VMM kill. The alternative (an intermediate
  `stateArming`) adds a state for no observable benefit. **Recommendation:**
  hold the lock.
- **OQ-5 (non-load-bearing) — arm bound = 120 s.** Mirrors podman's
  `defaultCommandTimeout` bounding the same script today
  (`podman.go:391-394`); the dominant cost is `getent` DNS resolution of the
  allowlist against passt's resolver. Also bounded by Start's ctx/boot
  deadline. **Recommendation:** `armTimeout = 120 * time.Second`, a guestd
  const.
- **OQ-6 (non-load-bearing) — error code for a failed arm.**
  `CodeInternal` with exit status + bounded stderr tail (distinct from the
  `CodeInvalidArgument` uid refusal and `CodeFailedPrecondition` gate/state
  errors, `supervisor.go:150-163`). The host wraps it into Start's error
  either way. **Recommendation:** `CodeInternal`.
- **OQ-7 (load-bearing) — the empty-`nft_script` gate-open seam vs the
  parent's arm-before-gate sentence (§(e)).** guestd still *accepts* an empty
  `nft_script` — skipping the arm and opening the exec gate with no ruleset —
  as a hermetic test seam (the V2b supervisor tests, non-Linux harnesses).
  This literally contradicts the frozen parent's "Only after a successful arm
  does the supervisor accept exec requests" (microvm-runner.md:152-155). It is
  production-safe: the host path never sends an empty script (`NftScript()` has
  no empty representation — it always emits at least the default-deny base
  ruleset, `egress.go:88-90`), so no real session opens the gate unarmed. The
  contradiction is therefore test-seam-only and deliberate, but under the
  RIG-2675 posture a detailing record must flag any contradiction with the
  frozen parent rather than resolve it in body prose. **Recommendation:**
  ratify the empty-script acceptance as a test-only carve-out (production stays
  always-armed), decided alongside OQ-1's timing reading.

### Ledger assessment

`Ledger impact: none.` The egress substrate (in-guest nft, guestd-as-root arm,
no NET_ADMIN on the workload, userspace net backend) is D6, already frozen in
the parent (microvm-runner.md:733-750); this record fills its behavior and
resolves implementation-shaped forks (OQ-1..3, OQ-7) that stay inside the microVM
record's own decision numbering. No new cross-cutting decision;
`docs/designs/DECISIONS.md` untouched (verified: it has no microvm/egress
rows).
