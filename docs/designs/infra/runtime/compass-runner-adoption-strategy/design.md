# Compass self-host runner topology and adoption strategy

Status: Active
Tracking: RIG-3070
Owner: compass-obs (design) → compass-runner (impl, runtime/sequencing)

## Problem / Intent

The runtime corpus froze a microVM trajectory (DL-259 self-host KVM stack,
the KVM-only no-fallback amendment, DL-235 client-only app) while podman
remains the shipping production default, and the adoption question — how a
new user actually gets onto Compass — was never written down as a contract.
Matt has ruled the topology and the adoption strategy (2026-08, amended
2026-08-31); this record documents that ruling as a frozen-on-merge
contract: **the security boundary follows the trust model, not the
deployment uniformly**. Untrusted multi-tenant operation runs code from
mutually-distrusting tenants and requires the microVM hardware isolation
boundary; self-host single-tenant deployments get podman as a permanent,
supported entry tier requiring no `/dev/kvm`, with microVM recommended but
not required; and embedded-local mode is revived as the cross-OS
(macOS/Linux/Windows-WSL) podman-backed developer front door, whose
app-architecture reversal is designed in the compass-native lane's
embedded-mode-revival record (`docs/designs/ui/compass-native-embedded-revival/design.md`,
DL-319). How a hosted multi-tenant service adopts and
sequences the microVM-only boundary is a managed-plane concern, out of
scope here (`docs/concepts/self-host-and-managed.md`).

## Approach

### The ruled topology

**The runner end state splits by trust model.** Untrusted multi-tenant
operation runs code from mutually-distrusting tenants and needs the
hardware isolation boundary: microVM (cloud-hypervisor/KVM) is required
there. A self-host single-tenant deployment runs the operator's
own agents on their own code on their own box — there is no untrusted
tenant to isolate from — so the KVM hardware boundary is optional there:
podman is a permanent, supported self-host entry tier, and microVM is the
recommended (not required) self-host upgrade, for defense-in-depth or for
an operator who runs untrusted code or shares the box.

This RATIFIES the stack SHAPE of one frozen record and PARTIAL-SUPERSEDES its
KVM-machine clause (DL-259), records the deferred reversal of a second
(DL-235), and AMENDS a third:

- **DL-259: stack shape ratified, KVM-machine clause partial-superseded by
  citation.** The self-host stack stays a host-level bring-up, no
  compose/Swarm packaging (`docs/designs/DECISIONS.md`, DL-259: "The self-host
  stack stays a host-level bring-up on a KVM-capable Linux machine
  (`compass-stack up`; microVM D3 hard-fail consumed, no compose/Swarm
  packaging)"). That stack-shape half is RATIFIED — the carve-out adds a
  podman tier beside the KVM stack rather than repackaging it. DL-259's
  "KVM-capable Linux machine" clause is PARTIAL-SUPERSEDED BY CITATION for the
  podman entry tier, which runs on a KVM-absent box or VPS — the same
  treatment DL-319 already applied to that clause ("partial-supersedes
  DL-259's KVM-floor clause by citation"); DL-259 stays Active and its
  microVM-path stack shape is unchanged.
- **DL-235 is being REVERSED** — its client-only charter retired embedded
  supervision (`docs/designs/DECISIONS.md`, DL-235: "The Compass native
  app is CLIENT-ONLY: `compass-app` retires embedded mode entirely
  (supervisor invocation, host preflight, UDS bridge target, embedded
  config arm) and connects exclusively over the authenticated TLS door to
  a headless Compass stack"). Under the 2026-08-31 embedded-revival
  ruling that charter is reversed by the compass-native lane's
  embedded-revival record; THIS record records only the topology
  direction that motivates the reversal — podman is permanent for
  self-host single-tenant, and a single-tenant local box has no untrusted
  tenant to isolate, so embedded-local is a legitimate deployment of the
  same podman tier — and defers the whole app-architecture reversal
  (un-retiring supervision, config, bundle) to that record.
- **The KVM-only amendment is AMENDED with a self-host carve-out.** This
  record reopens a frozen decision, and says so honestly: the amendment
  ruled that "the runtime is KVM-only" with no degrade-to-container
  fallback, and that "A KVM-absent host does not get a lesser boundary; it
  does not run" (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-kvm-only-amendment.md:96-97`).
  That posture is RATIFIED for untrusted multi-tenant operation, where the
  boundary isolates untrusted tenants from each other, and AMENDED for
  self-host single-tenant deployments, where the operator is the only
  tenant: there, podman is a first-class permanent runtime choice, not a
  fallback and not a lesser boundary imposed on an unwitting tenant. The
  self-host carve-out is the net-new ruling in this record (Matt,
  2026-08-31: "keep podman for the entry tier, say in docs that microVM is
  recommended even on selfhost, but podman/container is usable for users
  who don't want to pay a kvm premium"). A pointer-amendment in the
  KVM-only amendment's own directory
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-self-host-carveout-amendment.md`)
  records this carve-out beside the frozen amendment it amends, so a reader
  grounding there is not left with a silent absolute.

The elastic-session record already froze the transitional container path
that an untrusted-multi-tenant deployment eventually removes in favor of
microVM-only
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:403-405`:
"**Transitional container path, then microVM-only (D2).** The rootless
container remains the running boundary through Dogfood + trusted-tenant Beta
… and is then **removed**: microVM is the sole runtime"). Under the
trust-model split that removal scopes to untrusted multi-tenant operation;
the podman backend stays shipped for self-host, permanently.

### Decision — podman: permanent for self-host; both backends ship behind the seam

This section applies the trust-model split ruled above to the shipped
runtime.

**The core ships both runner backends behind one constructor-time seam.**
podman is the production default today; the microVM backend is opt-in and
maturing (the V-series work). The engineering invariant is that Compass
never ships without a working runner — the proven backend stays the floor
until a replacing backend clears a stated production-readiness bar (OQ-1).

**An untrusted-multi-tenant deployment ends at microVM-only.** The frozen
corpus already fixes that end state (`microvm-runner.md:403-405`, quoted
above); when and how a hosted multi-tenant service sequences its move off
the transitional container path is a managed-plane rollout decision, out of
scope here. This record fixes only the core capability — both backends
ship, and the seam supports pinning microVM behind a startup hard gate for
any deployment that requires the hardware boundary.

**Self-host: podman is permanent.** The entry tier stays supported
indefinitely — it is not dropped at the OQ-1 bar or any later milestone.
The honest cost is stated up front: the byte-identical-behavior constraint
and the two-runtime-shape maintenance surface, which the prior draft
treated as a transitional burden that retires at the drop, are now
PERMANENT for self-host. What makes the permanent split cheap is that both
backends already ship behind one constructor-time seam — this decision
KEEPS a working backend rather than building one.

The same permanent podman path is also what backs the revived
embedded-local front door: a single-tenant local box selects the podman
backend through the same `SelectBackend` seam, so embedded-local rides
the self-host entry tier rather than adding a third runtime shape.

Grounding the current state:

- Podman IS the production default today. `SelectBackend` defaults to it
  (`go/internal/runtime/microvm.go:119-120`:
  `case "", "podman": return NewPodmanCLI(), nil`), and its doc states the
  posture verbatim (`go/internal/runtime/microvm.go:110-113`: "During the
  transitional period both backends ship and the default is podman: the
  proven container path stays the floor while the microVM backend is brought
  up, so an unset backend never silently switches an operator onto the
  unfinished path"). The microVM backend is opt-in via
  `BackendConfig.Backend = "microvm"`
  (`go/internal/runtime/microvm.go:64-66`: "Backend names the runtime
  backend: \"podman\" (or empty, the transitional default) or \"microvm\"")
  and is maturing through the V-series per the `microvm-v2*` and
  `microvm-ci-dev-enablement` records in
  `docs/designs/infra/runtime/compass-elastic-session-runtime/`:
  V2a/V2b/V3/V4/V5 have landed (guest image + boot, in-guest supervisor +
  vsock exec, egress-in-guest, gateway-over-vsock, preflight + boot canary)
  and the KVM CI lane is live (`.github/workflows/ci.yml`, the `microvm`
  job); V6-V8 and the OQ-1 readiness bar remain.
- The seam is exactly what makes the permanent split cheap. The podman
  implementation is explicitly a thin seam
  (`go/internal/runtime/podman.go:10-13`: "podman.go — a thin
  ContainerRuntime over the podman CLI: the only place a subprocess is
  spawned. Everything above depends on the interface"), backend selection is
  constructor-time (`go/internal/runtime/microvm.go:117`:
  `func SelectBackend(cfg BackendConfig) (ContainerRuntime, error)`), and
  the frozen record pins byte-identical container behavior during
  coexistence
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:397-402`:
  "While both backends coexist (D2), selecting the container backend yields
  behavior byte-identical to today's podman path … This constraint retires
  when the container path is removed"). Under the trust-model split the
  container path is never removed from the shipped seam, so that parity
  constraint becomes permanent for self-host rather than retiring.
- The seam doc's post-drop shape (`go/internal/runtime/microvm.go:113-116`:
  "Once the microVM backend is the sole runtime, the default collapses to
  microVM guarded by a VerifyMicroVMSupport hard gate at startup — a legible
  refusal when the host cannot run microVMs, with no fallback to the
  container path") predates this amendment: under the trust-model split it
  describes the microVM-only *configuration* — a deployment that requires
  the hardware boundary pins the microVM backend behind the hard gate — not
  a deletion of the podman backend from the shipped seam.

### Embedded-local: the developer front door (dual runners accepted)

Embedded-local mode is REVIVED as the zero-setup, cross-OS developer
front door (Matt, 2026-08-31: "with us bringing back the podman runtime
and committing to the double runtimes — we just bring back the full
embedded stack mode, that you can run on macOS (can use podman), linux,
and windows (via wsl), and then our front door is easy — just brew
install the app, launch it, sign in with your Claude Code/Codex account,
and you are off to the races, same as if you had installed OMP or another
harness"). The trust-model split is what makes this viable: podman is
permanent for self-host single-tenant, and a developer's own laptop is
the single-tenant case in its purest form — there is no untrusted tenant
to isolate, so the podman boundary that is legitimate on a self-host box
is equally legitimate locally. And on the user's own box, restricted-tier
subscription sign-in is allowed (their box, their IP, their risk, the
frozen RIG-3050 posture) — a zero-friction on-ramp for a user who signs in
with an existing subscription.

The always-on-server argument survives as the GRADUATION motivation, not
an argument against embedded. Compass is fundamentally an always-on
server — agents keep working while you are away — and a personal laptop
sleeps; so embedded-local is the try-it-on-your-box on-ramp, and a user
who wants always-on operation graduates to a self-host stack on a
dedicated box or VPS, or to managed. The funnel: embedded-local (front
door, your box) → self-host stack (always-on, dedicated box) → managed
(hosted always-on).

The embedded supervision subsystem was deleted under DL-235
(`docs/designs/ui/compass-native-client-only/design.md:42-43`: "The
work here is DELETION of built, merged, working embedded code") and is
being RE-INTRODUCED by the compass-native lane's embedded-revival record,
which owns the whole app-architecture reversal — un-retiring supervisor
invocation, the embedded config arm, the thin-client→embedded bundle
change, mode selection, macOS podman-machine provisioning — none of which
is designed here. In this record "dual runtimes" still means podman and
microVM as STACK runner backends behind `SelectBackend`, both of which
already exist; embedded-local runs the same podman backend locally, not a
third runtime.

Permanent dual runners were rejected in the prior draft on
maintenance-doubling grounds (a fix on one backend can break the other).
Under the 2026-08-31 ruling that cost is now the ACCEPTED tradeoff for
self-host, for two reasons: the podman backend already exists and works, so
the cost is KEEPING a proven backend rather than building one; and it
removes the KVM premium at the self-host front door — cheap VPS tiers
mostly do not expose `/dev/kvm`, and a single-tenant operator gains little
from a hardware boundary that exists to isolate untrusted tenants. The
standing two-backend maintenance surface is the acknowledged price, bounded
by the frozen `ContainerRuntime` seam and the now-permanent byte-identical
parity constraint.

### Guided onboarding: embedded-local front door, then self-host

The adoption funnel starts before self-host: the zero-setup front door is
embedded-local — brew install the app, launch it, sign in with your own
subscription, and agents run locally on the podman backend (the
app-architecture that delivers this is the compass-native lane's
embedded-revival record, not this record). Self-host is the graduation
tier for always-on operation, and its bring-up must be near-one-command
on the user's own Linux box or a VPS. The entrypoint already exists:
`compass-stack` dispatches
`up|down|status|preflight` (`go/cmd/compass-stack/main.go:8-13`: "up: bring
the embedded stack to Ready (or attach to a live one) … preflight: check the
host's KVM/podman/microVM prerequisites"), and DL-259's install surface is
"the flake + preflight + self-host doc" (`docs/designs/DECISIONS.md`,
DL-259). Under the trust-model split, self-host no longer requires a
KVM-capable box: the podman entry tier runs on ANY cheap VPS or Linux
machine with no `/dev/kvm`. One caveat the onboarding guide must respect:
today's `compass-stack preflight` reports the KVM probe and the microVM
host-floors unconditionally (`go/cmd/compass-stack/preflight.go`), so on a
zero-KVM podman box it exits non-zero — a green-preflight experience for the
podman entry tier is part of T1's deliverable, and T2's guide must not
instruct a `preflight` run on the podman tier before then. This record adds
the adoption framing on top:
an onboarding guide (T2) that walks the funnel — the embedded-local front
door first, then both self-host graduation paths: the zero-KVM podman
path on any VPS or box (the entry tier), and the recommended microVM path
on a KVM-capable box or nested-virt-enabled instance (the docs recommend
microVM even on self-host; podman remains fully supported for users who
don't want the KVM premium). The specific VPS provider recommendation is
deferred to doc-writing time (OQ-2). The guide content itself is an impl
task (T2), not frozen prose here.

### macOS reality (designed against, not designed around)

- The microVM VMM is cloud-hypervisor, a KVM-backed VMM
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:80-81`:
  "Per D1, the VMM is **cloud-hypervisor**. The design relies only on the
  **virtio-fs-preserving shape** — a KVM-backed VMM"), which needs Linux
  `/dev/kvm`. No Mac has `/dev/kvm`. Running the Compass microVM on macOS is
  only possible NESTED (macOS → a Linux VM via Apple
  Virtualization.framework → cloud-hypervisor inside), which requires
  hardware nested virtualization available only on M3+/macOS 15 — bleeding
  edge, not a supported path. The distribution record is blunt
  (`docs/designs/infra/release/compass-distribution/design.md:111-113`: "every
  stack install channel targets **Linux x86_64 with KVM**; there is no macOS
  or no-KVM stack story, ever, in this record").
- Podman on macOS also runs a Linux VM (podman-machine) but is
  containers-in-a-VM: mature, works on ANY Mac, no nested virt needed. The
  local-dev record already runs the whole runner inside such a VM on macOS
  (`docs/designs/infra/ci/compass-local-dev/design.md:194-196`: "The runner
  cannot run natively on macOS against a remote podman connection: the
  per-container agent sockets are AF_UNIX bind-mounts whose source must be
  local to the container host"; `:210`: "VM engine: `podman machine`
  (recommended over colima; OQ1)").
- Implication: under the trust-model split, a Mac self-host user can
  PERMANENTLY run the stack via podman-machine — no nested virtualization
  needed, works on any Mac — since podman is a permanent self-host tier,
  not a transitional window. This same podman-machine path is now ALSO
  the embedded-local front-door path on Mac ("can run on macOS (can use
  podman)", Matt 2026-08-31), not only a self-host option. The microVM
  path on a Mac remains nested-only/bleeding-edge and unsupported.
  Alternatively the Mac user
  points the client app at a remote Linux stack — the DL-235
  client-only posture already supports exactly this
  (`docs/designs/DECISIONS.md`, DL-235: "connects exclusively over the
  authenticated TLS door to a headless Compass stack — normally on a
  dedicated KVM-capable machine"). One open technical unknown is scoped as
  a spike, not a blocker: whether the AF_UNIX per-session socket
  cross-boundary limitation documented for the runner-in-a-VM shape
  (`compass-local-dev/design.md:194-199`) also affects podman-machine
  bind-mounts for a Mac self-host stack (OQ-3).

## Alternatives considered

- **Option 1 — permanent dual podman+microVM + revive embedded
  (macOS/Linux/WSL).** Previously rejected; now, in substance, the
  ADOPTED posture (Matt, 2026-08-31), refined by two splits: the
  trust-model split (untrusted multi-tenant stays microVM-only; podman is permanent only
  where there is no untrusted tenant) and the direction/architecture
  split (this record fixes the direction — embedded-local as the
  podman-backed cross-OS front door — while the compass-native lane's
  embedded-revival record designs the app-architecture reversal of the
  DL-235 deletion,
  `docs/designs/ui/compass-native-client-only/design.md:43-44`).
- **Option 1-lite — permanent dual runners, no embedded.** Briefly the
  chosen interim posture; now subsumed by Option 1 (embedded-local adds
  the front door on the same podman tier). The
  prior draft rejected it for the permanent two-runtime maintenance cost
  and for forfeiting the clean single-runtime end state the KVM-only
  amendment argued for (`microvm-kvm-only-amendment.md:95-96`: "it splits
  every downstream path (C3 burst, D4 density) into two runtime shapes
  forever"). The split preserves that argument where it bites — the
  untrusted multi-tenant operation, whose burst/density paths stay single-runtime microVM —
  and accepts the two-shape cost only for self-host, where podman already
  ships and removes the KVM premium at the front door.
  Honesty note: the KVM-only amendment's OWN alternatives already
  considered and rejected a self-host carve-out ("Keep the
  degrade-to-container path as a self-host / KVM-absent convenience",
  `docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-kvm-only-amendment.md:91-97`,
  rejected 2026-08-23 as "a standing hole in the security posture"). That
  rejection was about the untrusted-multi-tenant microVM boundary, where
  a shared-kernel fallback is a real hole in the isolation an
  untrusted-multi-tenant boundary provides; this record's carve-out is self-host single-tenant,
  where there is no untrusted tenant — a different case, not a reversal
  of that specific finding.
- **Uniform KVM-only everywhere (the prior draft's ruling).** Reconsidered
  by Matt (2026-08-31): it taxed self-host single-tenant users for a
  hardware isolation boundary that exists to isolate untrusted tenants
  they do not have, and priced them onto KVM-capable hosts when cheap VPS
  tiers mostly cannot expose `/dev/kvm`. The boundary now follows the
  trust model instead.
- **KVM-mandatory-everywhere immediately (drop podman now).** Not chosen
  either: the microVM backend has not yet cleared the OQ-1 readiness bar
  (V6-V8 and the dogfood soak remain), and dropping podman before that bar
  would violate the never-without-a-working-runner invariant. Podman stays
  the floor while microVM matures — exactly the `SelectBackend` posture
  (`go/internal/runtime/microvm.go:110-113`) — and, per the trust-model
  split, stays permanently for self-host.
- **Other runtime backends behind the seam — deferred, not declined-forever.**
  The `ContainerRuntime` interface is frozen precisely so a new backend is one
  `SelectBackend` case plus an implementation, no caller churn
  (`go/internal/runtime/podman.go`: "Everything above depends on the
  interface, so a libpod-REST backend can replace it without touching a
  caller"). Three candidates were raised and are scoped OUT of this record's
  freeze, each to its own follow-up:
  - **Apple `container` for the embedded macOS front door** — Apple's
    OCI-image runtime boots a dedicated lightweight VM per container over
    Virtualization.framework (no nested virt, sub-second boot), a stronger
    boundary than podman-machine's shared Linux VM and a natural fit for the
    most-used front door (a native ADE-style local experience on Mac). It is
    Apple-silicon/macOS-only and reached 1.0 recently (as of 2026-09), and
    is a distinct non-podman CLI (a new backend implementation, not a
    `WithProgram` swap). Investigation starts now in the compass-native
    lane; today's shipping Mac paths (podman-machine permanent +
    remote-Linux client) are unaffected.
  - **Quadlet vs. hand-rolled supervision for the self-host stack services**
    (server/runner/gateway/postgres/NATS). Quadlet is declarative
    systemd-native container supervision — a candidate to replace the
    hand-rolled DL-183/DL-262 pgid teardown for the LONG-LIVED stack services,
    a separate concern from this record's per-session runner backend and
    Linux/systemd-only (no bearing on the macOS front door). Its own
    follow-up.
  - **A generic Docker socket backend** — declined for now, not frozen out:
    it is the daemon model against this substrate's hard rootless/no-daemon
    invariant (`podman.go`: "Rootless is a hard requirement … no daemon, no
    root, no rootful fallback"), and Docker has no `--userns=keep-id:uid=`
    equivalent for the host-user→agent-uid remap Create depends on
    (`VerifyUsernsRemapSupport`), so it is a real port, not a drop-in. Folded
    into the Quadlet/backend-breadth follow-up as an explicitly-considered
    alternative.

## Global Constraints

1. **KVM/Linux floor — the microVM path only.** Any untrusted-multi-tenant
   runtime and any self-host deployment that selects the microVM
   backend must expose `/dev/kvm`; on the microVM path, KVM-absent
   hard-fails with no silent degrade
   (`docs/designs/infra/release/compass-distribution/design.md:108-110`, quoting
   microvm-runner D3: "KVM-absent ⇒ hard-fail (D3): with no container
   fallback, `/dev/kvm` absence (or any preflight failure) aborts Runner
   startup"). The self-host podman entry tier has NO KVM floor — it runs
   on any box or VPS without `/dev/kvm`; the universal KVM floor the
   distribution record consumed is amended for that tier by this record's
   trust-model split.
2. **Always-on-server invariant → the graduation motivation.** Compass is
   an always-on server; no design may assume an always-on deployment
   lives on a machine that sleeps. Embedded-local is the on-ramp — the
   always-on invariant is the reason a user graduates from it to a
   self-host stack or managed, not an argument against its existence.
3. **Embedded-local is the developer front door.** Its app-architecture
   reversal (reversing DL-235's client-only charter) is designed in the
   compass-native lane's embedded-revival record, not here; this record
   fixes only the direction and the topology rationale.
4. **Never without a working runner.** A move to microVM-only happens only
   after the microVM production-readiness bar (OQ-1) is met; until then
   podman remains the default and the byte-identical-behavior constraint of
   `microvm-runner.md:397-402` holds. When and how a hosted
   multi-tenant service sequences that move is a managed-plane rollout
   decision, out of scope here. Self-host is unaffected: it keeps the
   podman backend permanently, so the parity constraint is permanent there
   rather than retiring.
5. **Enrollment policy is consumed, not redesigned.** The RIG-3050
   consumption-eligibility matrix
   (`docs/designs/server/compass-gateway-oauth-enrollment/design.md:66-87`) governs which
   credential kinds each mode offers; this record layers adoption framing
   on top of it.

## Plan

The embedded-local app-architecture (un-retiring supervision, config,
bundle; reversing DL-235) is designed and implemented in the
compass-native lane's embedded-mode-revival record
(`docs/designs/ui/compass-native-embedded-revival/design.md`, DL-319) — deliberately NOT a task
in this record.

### T1 — microVM production-readiness bar + the microVM-only pinning path

- **Owner:** compass-runner (runtime lane).
- **Do:** freeze the checklist (proposed bar in OQ-1) that certifies the
  microVM backend production-ready. The microVM-only pinning path this
  record called for has since landed in the runtime lane and is **consumed
  here, not built here**: `verifyBackendPreflight`
  (`go/cmd/compass-runner/main.go:212`) runs the selected engine's
  static host-capability probe at startup, dispatching the microVM backend
  to `VerifyMicroVMSupport`
  (`go/internal/runtime/microvm_preflight.go:79`) fail-closed — so selecting
  `BackendConfig.Backend = "microvm"` is already pinned behind the startup
  hard gate the seam doc named
  (`go/internal/runtime/microvm.go:113-116`), and `compass-stack preflight`
  already splits the podman prerequisites (`decidePodman`) from the microVM
  trio floors (`hostcheck.MicroVMFloors`) via the shared
  `go/internal/hostcheck` package (`go/cmd/compass-stack/preflight.go`).
  What remains net-new here is the constraint, not the mechanism: the podman
  backend, `PodmanCLI`, and the `"podman"` backend value are NOT deleted —
  they remain the permanent self-host entry tier per the trust-model
  split; the prior draft's deletion step is cancelled. When and how a
  hosted multi-tenant service adopts microVM-only is a managed-plane
  rollout decision, not this task.
  Also net-new here: make `compass-stack preflight` backend-aware — the
  microVM trio floors (`hostcheck.MicroVMFloors`) and the KVM probe report as
  advisory (non-failing) when the podman entry tier is selected or targeted,
  so a zero-KVM podman box gets a green preflight, while the KVM/microVM
  checks stay hard-failing on the microVM path
  (`go/cmd/compass-stack/preflight.go`). This is the green-preflight
  deliverable that §Guided onboarding names as T1's, and on which T2's
  podman-tier `preflight` instructions are blocked until it lands.
- **Interfaces:** consumes the frozen `ContainerRuntime` interface and
  `SelectBackend(cfg BackendConfig) (ContainerRuntime, error)`
  (`go/internal/runtime/microvm.go:117`); consumes the landed startup
  preflight surface (`verifyBackendPreflight`, above) and the microVM e2e/CI
  suites
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-ci-dev-enablement.md`).
  Produces the frozen OQ-1 readiness bar.
- **Deps:** the microVM V-series tasks in
  `compass-elastic-session-runtime/` reaching the OQ-1 bar — the
  pinning/preflight waves owned by
  `compass-elastic-session-runtime/microvm-v5-preflight-boot-canary.md`
  (V5 W1 `hostcheck` extraction, W2 `verifyBackendPreflight` startup
  dispatch, and W3 `BootCanary`/`CanaryReport` + its startup wiring) have
  all landed and are consumed above; what remains for OQ-1 criterion 4 is
  the dogfood soak that consumes the canary, not the canary itself. Blocks
  T2's podman-tier preflight instructions (the backend-aware preflight
  above); T2's remaining content is unblocked.

### T2 — Guided self-host onboarding

- **Owner:** distribution/docs lane (extends DL-259's T9 self-host doc).
- **Do:** the onboarding guide, opening with the embedded-local front
  door (brew install the app → launch → sign in with your own
  subscription; the delivering app-architecture is the compass-native
  lane's embedded-revival record, not this task), then near-one-command
  bring-up documentation and polish around `compass-stack up` covering
  BOTH self-host graduation paths: the zero-KVM podman
  entry tier on any cheap VPS or Linux box (no `/dev/kvm` needed), and the
  recommended microVM path with KVM-capable instance types
  (nested-virt-enabled instances) — stating the recommendation that
  microVM is preferred even on self-host while podman remains fully
  supported. Also covers the dedicated-Linux-box path, the one-box
  localhost-TLS path (client-only OQ-6 ruling), and the Mac paths:
  podman-machine as a PERMANENT supported option on any Mac, or the client
  app pointed at a remote Linux stack per the DL-235 client-only posture
  (`docs/designs/ui/compass-native-client-only/design.md:28-31`).
  Provider picks are decided at doc-writing time (OQ-2).
- **Interfaces:** consumes `compass-stack up|preflight`
  (`go/cmd/compass-stack/main.go:8-13`) and the DL-259 install surface
  (flake + preflight + `docs/self-host.md`,
  `docs/designs/DECISIONS.md`, DL-259). Produces the onboarding
  guide.
- **Deps:** DL-259 T6/T9 (flake, preflight, self-host doc). T1's
  backend-aware `compass-stack preflight` — required before the guide may
  instruct a `preflight` run on the podman tier; the rest of T2 may proceed
  in parallel.

## Tasks

- [ ] **T1** Readiness bar frozen (OQ-1 ruled). The microVM-only pinning
  path (backend pinned behind the `VerifyMicroVMSupport` startup hard gate)
  and the podman/microVM preflight split have already landed in the runtime
  lane — consumed here, not built here; podman backend retained permanently
  for self-host. Net-new: make `compass-stack preflight` backend-aware so the
  podman entry tier gets a green preflight (microVM/KVM checks advisory on the
  podman path, hard-failing on the microVM path); T2's podman-tier
  `preflight` instructions are blocked on this, the rest of T2 parallel.
- [ ] **T2** Onboarding guide: embedded-local front door, then self-host
  graduation — zero-KVM podman path (any VPS/box) and recommended microVM
  path (KVM instance types), dedicated-box, one-box localhost-TLS, and
  Mac paths (podman-machine permanent; Mac→remote-Linux).

## Open Questions

- **OQ-1 [load-bearing for T1's execution, not for this record's freeze] —
  the microVM production-readiness bar.** The bar certifies the microVM
  backend production-ready, so its criteria must be ruled before T1
  executes; the topology decision itself is already ruled and does not wait
  on it.
  **Recommendation** (a concrete bar to ratify or amend):
  1. the microVM e2e/acceptance suites green in CI (the
     `docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-ci-dev-enablement.md` lane) including the gateway suites over
     the hybrid-vsock transport;
  2. the guest supervisor exec path (V2b) and session teardown proven
     under the same acceptance suite the podman backend is judged by —
     this record's parity bar, composing with (not derived from) the
     byte-identical-coexistence constraint of `microvm-runner.md:397-402`,
     which constrains the CONTAINER backend during coexistence;
  3. a documented self-host bring-up path: `compass-stack preflight` green
     on a clean KVM host → `up` → one agent session end-to-end (the DL-259
     T9 test cycle shape);
  4. a dogfood soak: the microVM backend as the opt-in default on the
     dogfood stack; the soak-window length and the boundary-regression
     definition are quantified in the bar-ruling artifact (below).

  **Ruling vehicle:** OQ-1 is ruled either as a short amendment in
  `compass-elastic-session-runtime/` (the directory's established
  amendment mechanism) or as its own ledger row when the microVM backend
  is certified — that artifact is what "OQ-1 ruled" concretely looks like
  for compass-runner, and it carries the soak window and regression
  definition criterion 4 delegates.
- **OQ-2 [deferral] — which VPS provider(s) the onboarding guide
  recommends.** Doc content, decided at T2 writing time. With the podman
  entry tier, KVM-capable instance types are a requirement only for the
  recommended microVM path, not for self-host as such; the guide
  recommends providers for both tiers.
- **OQ-3 [deferral, spike] — macOS podman-machine socket-mount
  feasibility.** Whether the AF_UNIX per-session socket cross-boundary
  limitation (`compass-local-dev/design.md:194-199`, written for the
  runner-in-a-VM dev shape) also affects a podman-machine-hosted stack on
  macOS. This now matters MORE than a permanent self-host convenience:
  podman-machine is the embedded-local front-door path on Mac ("can use
  podman", Matt 2026-08-31), so the spike gates the Mac front door. Still
  a spike, not a blocker for this record's freeze: the Mac user always
  has the remote-Linux client path either way.

## Ledger delta (for the coordinator to encode at freeze)

One decision in this record is net-new and covered by no existing DL row —
DL-259 and DL-235 cover the KVM stack and the client-only app, and DL-319
(embedded-revival) cites this split as its rationale rather than ruling it,
not the runtime split or the adoption framing. Recommended row, mirroring the
freeze-time delta shape the directory's amendments use
(`microvm-kvm-only-amendment.md:112-117`):

1. **Permanent trust-model runtime split; embedded-local revived on the
   podman tier.** The security boundary follows the trust model:
   untrusted multi-tenant operation requires the microVM hardware boundary
   (unchanged); self-host single-tenant deployments keep podman as a
   permanent, supported entry tier requiring no `/dev/kvm`, with microVM
   the recommended (not required) upgrade. When and how a hosted
   multi-tenant service sequences its move to microVM-only is a
   managed-plane rollout decision, out of scope here. The permanent
   self-host podman tier is also what enables embedded-local as the revived
   cross-OS (macOS/Linux/Windows-WSL) developer front door; the
   app-architecture reversal that delivers it (reversing DL-235's
   client-only charter) is designed in the compass-native lane's
   embedded-revival record and carries its own ledger row there. AMENDS
   the frozen KVM-only amendment (`microvm-kvm-only-amendment.md:96-97`)
   with the self-host carve-out; the `ContainerRuntime` interface stays
   frozen.

This stanza is human-readable guidance for the freeze coordinator; the
ledger row is encoded in `DECISIONS.md` in the same PR at freeze time.
