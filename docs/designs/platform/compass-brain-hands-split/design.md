# Compass brain/hands split — separate the agent from on-demand compute

> **Design record.** This designs the brain/hands split for Compass agents —
> separating the agent loop from on-demand compute on vendor-neutral
> primitives. Every `go/internal/*`, `agent-image/*`, and `docs/*` citation
> below is a path in the **`RigelBuild/compass`** monorepo at HEAD `13a43cec`
> (line numbers drift as the code evolves; resolve them against that commit).
> It lives in the sealed design corpus (`docs/designs/platform/`) because that
> is where the wave's design records freeze. Frozen on merge; executing agents
> read this as the contract for RIG-1717.

Status: Draft
Tracking: RIG-1717

## Problem / Intent

Today's architecture fuses brain and hands: **one rootless-podman container per
agent, and the agent binary runs inside it.** `AgentRuntime.Launch`
(`go/internal/runtime/agent.go:177-198`) creates + starts the container, arms
the default-deny egress firewall as root (`armEgress`,
`go/internal/runtime/agent.go:300-309`, running `EgressPolicy.NftScript()`,
`go/internal/runtime/egress.go:87`), installs scoped credentials, and creates
the checkout dir; the Runner then execs a bare `compass-agent` argv inside the
container (`agentCommand`, `go/internal/runner/agent_exec.go:32`;
`AgentEnv.execSpec` `:77-96`; `StartAgent` `:136`). That `compass-agent` is a
`bun build --compile` standalone bundle (`agent-image/entrypoint.nix:5-29`,
wrapper `:227-229`) — an I/O-bound LLM loop that is already portable in
principle, yet today it can only run welded to the container that also serves
as its compute environment.

The fusion means every agent pays for a full container for its whole lifetime,
even though ~90% of what an agent does — read, edit, reason, call the model —
needs no compute environment at all. It also welds two decisions together that
should be independent: *where the agent loop runs* and *where agent-generated
code runs*. RIG-1717 separates them: the **brain** (the agent loop — LLM
calls, reasoning, read/write over a shared virtual FS; lightweight, I/O-bound,
portable) from the **hands** (compute for running model-written code —
compile, test, dev server; heavy, native, security-sensitive), with hands spun
up **on demand** and torn down after. The pattern is adopted on
**vendor-neutral primitives** — never Cloudflare libraries; Workers is at most
one swappable brain-placement backend, not a foundation. Sequencing:
Beta-phase, explicitly **after** the Dogfood substrate
(`docs/designs/platform/compass-dogfood-loop/design.md`), landing when hosted
Compass makes infra cost first-order. Nothing in Dogfood blocks on this.

## Approach

### The split

- **Brain** = the agent loop: LLM calls, reasoning, tool dispatch, reads and
  writes against a shared virtual FS. A Bun process today (the same
  OMP-derived loop that ships in `agent-image/entrypoint.nix`), a Go goroutine
  later (SEA-1719), or optionally a Worker isolate at a managed edge. Because
  the brain is portable, "where the brain runs" and "where the hands run" are
  independent decisions — that independence is what defeats lock-in.
- **Hands** = an on-demand compute environment for agent-generated code. Booted
  only when a task needs it, FUSE-mounting the same virtual FS the brain has
  been editing (edits already present — no copy in, no copy out), torn down
  after.

### Tiered execution — reach for compute only when needed

Target: **<10% of operations hit a microVM.**

1. **Brain over virtual FS** — write/edit/read/reason. No compute environment
   at all. ~90% of a PR's operations.
2. **Light in-isolate exec** — grep, small deterministic shell ops, run as
   shell-to-JS over the virtual FS (the `just-bash` trick) inside the brain's
   own process. No VM, no container.
3. **Heavy/native exec in a microVM** — compile, test, dev server. Only here
   does a Firecracker microVM boot (~125ms), FUSE-mounts the shared virtual
   FS, runs the command, and is reclaimed.

### Vendor-neutral primitives (the self-hostable stack)

- **Shared virtual FS → Turso AgentFS** (MIT,
  github.com/tursodatabase/agentfs): a POSIX filesystem inside one SQLite
  `.db`, FUSE/NFS-mountable, copy-on-write per-agent branching, SQL-queryable
  audit trail. Cloudflare's `dofs` minus the Durable Object — runs on one
  Linux box, open format. Its SQL-auditable property directly serves
  SEA-1718's "human-auditable, not a black box" constraint.
- **On-demand isolation → Firecracker** (Apache-2.0): ~125ms boot, ~5MB
  overhead, thousands of microVMs per box, hardware/KVM isolation. With
  **bubblewrap/namespaces** as a lighter tier for *trusted* worker tasks;
  untrusted (model-written) code escalates to a microVM.
- **Brain → our own OMP-derived loop** (JS today; Go later, SEA-1719 — note
  Go→WASM runs single-threaded in a Worker isolate, so goroutine density is
  inert on Workers; Go pays off only as a real OS process on boxes we run,
  which is why the portable path stays JS).

### How this maps onto the existing seam

The package layering (`go/internal/runtime/podman.go:10-20`) already isolates
the engine behind the `ContainerRuntime` interface
(`go/internal/runtime/podman.go:286-324`) — "everything above depends on the
interface, so a libpod-REST backend can replace it without touching a caller."
The split reuses that discipline rather than replacing it:

- **`ContainerRuntime` stays untouched.** It remains the engine seam for the
  container-shaped things that survive the split (the gVisor/container
  fallback tier, and the fused path during migration).
- **A new sibling seam, `HandsRuntime`, sits beside it** (not a rewrite of
  `AgentRuntime`): a backend-selecting exec API in the Cloudflare Computer
  `exec(cmd, {backend})` shape —
  `Exec(ctx, HandsSpec) (ExecOutput, error)` where `HandsSpec` carries the
  command, cwd, env, and the branch id. **The isolation `Backend`
  (`isolate` | `sandbox` | `microvm` | `container`) is assigned Runner-side
  by policy, never authored by the brain** — a model-driven caller may pass
  at most an *upgrade-only hint* policy can raise but never lower (Global
  Constraint 9); "the Runner routes each exec to a tier, callers never name
  Firecracker or podman." `Exec` is completion-shaped; a long-lived variant
  (streaming stdio + kill/wait handle, in the `ContainerRuntime.ExecStreaming`
  shape, `go/internal/runtime/podman.go:299-307`) is reserved in H1 for
  SEA-1720's agent-launched dev servers (OQ7).
- **`AgentRuntime` narrows from "the agent's world" to "one brain-placement
  backend."** Its `Launch` (`go/internal/runtime/agent.go:173-198`) —
  container + in-container agent — becomes the *fused/container* placement,
  kept working throughout the migration. A new host-process placement runs the
  brain as a supervised bun process against the AgentFS mount, no container.
  The `AgentSpec` fields (`go/internal/runtime/agent.go:32-57`) split along
  the same line: `Persona`/`Role`/`AgentAccountID`/`Model` are brain
  concerns; `Image`/`Mounts` are hands concerns; `Workspace` is replaced by a
  virtual-FS branch; `Egress` splits in two (next bullet).
- **Egress splits.** Today one `EgressPolicy`
  (`go/internal/runtime/egress.go:32-34`) covers the whole container. Post-
  split the brain needs LLM/API endpoints and the hands need default-deny
  plus build-time hosts — different allowlists, same `NftScript()`-style
  fail-closed mechanism (`go/internal/runtime/egress.go:87`) applied per
  placement (nft in the microVM / sandbox netns for hands; the brain's policy
  enforced by its own placement).
- **The virtual FS is a new infra seam, `VirtualFS`:**
  `Mount(ctx, branch BranchID) (mountpoint string, unmount func() error, err error)`,
  `Branch(ctx, parent BranchID) (BranchID, error)`, and
  `Audit(ctx, branch BranchID, since time.Time) ([]AuditEntry, error)` over an
  AgentFS `.db`. The brain's placement and every hands backend consume the
  same mount, which is what makes tier 3's "edits already present, no copy"
  true — **as a measured default, not an assumption:** heavy execs over a
  FUSE→virtio-fs mount are a known worst case (many-small-file `go test` /
  `bun install`), so V1/H2 benchmark it and the design keeps a
  materialize-the-branch-into-guest-storage-then-write-back-the-diff escape
  hatch (OQ5) for the case where no-copy loses to the copy it saves.
- **The Runner's substrate stays runtime-agnostic.** `SpecBuilder`
  (`go/internal/runner/host.go:46-48`) keeps assembling specs from operator
  defaults + the provision request; the dogfood loop moves under the split
  unchanged, per the sequencing constraint.

`HandsRuntime` is the one genuinely new abstraction, and it is justified by a
capability no existing seam carries: `ContainerRuntime` models *a container's
lifecycle* (create/start/exec/stop against a `ContainerID`,
`go/internal/runtime/podman.go:286-324`), while hands need *an exec routed to
a tier that may not be a container at all* (in-process isolate, bubblewrap,
microVM). Forcing microVMs to impersonate `ContainerID`s would corrupt the
existing seam's meaning; a sibling interface keeps both honest.

## Alternatives considered

### Cloudflare Computer / Workers as the foundation — rejected

`@cloudflare/computer` (github.com/cloudflare/computer) is the closest
blueprint — its `dofs`/`computerd` FUSE-sync and backend-selecting
`exec(cmd, {backend})` are exactly the shapes this record adopts as a
*pattern*. But as a foundation it fails twice: it is Durable-Object-bound and
preview-only (a lock-in plus a stability risk), and Workers/V8 isolates cannot
be hands at all — no POSIX, no subprocess, no `go test`. Treating a Worker as
a compute environment is a category error, not a tradeoff. Cloudflare Computer
stays a **reading reference only**; Workers remains at most an optional,
swappable brain-placement backend.

### Container-only hands (podman/gVisor, no microVM) — fallback, not default

Keeping hands as containers would reuse `ContainerRuntime`
(`go/internal/runtime/podman.go:286-324`) wholesale. But hands run
model-written code: we want a hardware boundary, not a shared kernel.
gVisor-hardened containers narrow the syscall surface but still interpose a
userspace kernel on the host kernel. Kept as the **fallback tier where KVM is
unavailable** (nested-virt-less VMs, some CI); Firecracker is the default
where KVM exists.

### e2b open infra (Nomad/Consul orchestration) — too heavy

e2b (Apache-2.0, github.com/e2b-dev/E2B) is the reference for the
Firecracker-orchestration glue — spawn/mount/run/teardown, warm pool — but its
Nomad/Consul cluster machinery is sized for multi-tenant fleets. Single-box
first: we take the glue patterns, not the cluster weight.

### The microVM / container / Worker tradeoff (why Firecracker for hands)

| | Firecracker microVM | Container (podman / gVisor) | Worker / V8 isolate |
| --- | --- | --- | --- |
| Isolation boundary | Hardware/KVM — own kernel | Shared host kernel (gVisor: userspace kernel shim) | V8 sandbox — no OS at all |
| Can run `go test`, compilers, dev servers | Yes — full Linux | Yes | **No — no POSIX, no subprocess (category error)** |
| Cold start | ~125ms | ~100ms–1s | ~5ms |
| Per-instance overhead | ~5MB | ~10–100MB | ~KBs |
| Density | Thousands/box | Hundreds/box | Tens of thousands |
| Trust level for model-written code | Strong (hardware boundary) | Weaker (kernel attack surface; gVisor mitigates) | N/A as hands |
| Lock-in | None (Apache-2.0, any Linux/KVM box) | None | Cloudflare-shaped |

A ~125ms boot is noise against a seconds-to-minutes compile/test run, and the
hardware boundary is the right posture for untrusted code — so **microVM wins
for hands**, containers are the KVM-less fallback, and isolates are relevant
only as an optional brain placement.

## Global Constraints

1. **Vendor neutrality (hard rule).** No Cloudflare foundation dependency
   anywhere in the stack — no `@cloudflare/computer`, no Durable Objects, no
   Workers-required path. Cloudflare Computer is a reading reference only.
   Every primitive must be self-hostable on one Linux box: AgentFS (MIT),
   Firecracker (Apache-2.0), bubblewrap, our own brain loop.
2. **Linux/KVM for the microVM tier.** Firecracker requires KVM
   (`/dev/kvm`). Hosts without it (or without nested virt) get the
   gVisor-hardened-container fallback tier; the `HandsRuntime` backend
   selector must degrade explicitly, never silently run untrusted code in a
   plain container.
3. **Single-box first.** All orchestration targets one Linux box (the
   dogfood/self-host shape). No Nomad, no Consul, no cluster scheduler.
   Multi-box is a later record.
4. **The substrate stays runtime-agnostic.** The Runner's provision flow —
   the `SpecBuilder` seam (`go/internal/runner/host.go:46-48`), the
   dispatch/session plumbing (`go/internal/runner/agent_exec.go`,
   `dispatch.go`, `host.go`) — keeps its interfaces; the split slots in as new
   implementations behind existing seams plus the three new ones
   (`HandsRuntime`, `VirtualFS`, `BrainPlacement`). The dogfood loop
   (`docs/designs/platform/compass-dogfood-loop/design.md`) must keep passing
   unchanged at every increment.
5. **Sequencing: Beta-phase, after Dogfood.** Nothing in the Dogfood milestone
   blocks on this record; no task here may become a Dogfood dependency. Work
   starts when hosted/managed Compass is being stood up.
6. **The fused path stays green during migration.** `AgentRuntime.Launch`
   (`go/internal/runtime/agent.go:173-198`) remains a working brain placement
   until the split path carries a full session end to end; increments are
   additive behind seams, never a big-bang cutover.
7. **Security floors carry over.** Default-deny egress (fail-closed,
   `go/internal/runtime/egress.go:71-107` semantics) applies to every
   placement of both brain and hands; model-written code never executes
   outside a hardware boundary (microVM) or the explicitly-degraded gVisor
   fallback; secrets keep the 0600/stdin-not-argv discipline
   (`go/internal/runtime/agent.go:238-246`).
8. **Version floors.** Firecracker ≥ 1.7, AgentFS pinned to a vetted release
   (see OQ2), Linux ≥ 5.10 with FUSE3, existing podman ≥ 4.3 floor unchanged
   (`go/internal/runtime/podman.go:405-413`).
9. **Tier selection fails closed; the agent can never lower isolation.** The
   isolation tier for any hands exec is chosen by Runner policy, not by the
   model-driven brain: a command not on the tier-2 in-isolate allowlist
   escalates to tier 3 (an unknown/unclassified command is tier 3 by default),
   and the `ExecHands` RPC carries no authoritative `Backend` from the agent —
   at most an upgrade-only hint policy may raise. This is the invariant the
   whole hardware-boundary posture rests on: a classifier that fails *open*
   would put model-chosen argv inside the brain's own process, which holds the
   LLM credentials, the scoped-`$HOME` secrets, and the brain's (wider) egress
   — strictly weaker than today, where every agent exec runs in the
   default-deny container (`go/internal/runtime/egress.go:71-107`).

## Plan

Lanes: **virtual FS = infra**, **hands = runner/infra**, **brain =
compass-agent**. Dependency order is the task numbering within each lane plus
the explicit `Depends:` lines; V1 → H1 is the critical path, brain tasks
parallelize once V1 and H1 freeze their interfaces.

### M0 — baseline the tier mix from real sessions (lane: compass-agent, parallel pre-task)

Before three backends get built, ground the ~90%-brain / <10%-microVM figures:
classify the tool-calls in existing dogfood-loop session logs (the substrate
already emits tool-call telemetry) into tier 1 / 2 / 3 and report the real
distribution. Cheap, no dependency; informs whether the <10% target is an
acceptance bar or needs adjustment before E1 measures it live.

- **Interfaces:** consumes existing dogfood session tool-call logs; produces a
  one-off tier-mix report committed in this record's directory. No seam.
- **Depends:** nothing (runs in parallel with V1).
- **Test cycle:** the report itself; a classification pass rerunnable as sessions accrue.

### V1 — AgentFS vetting spike + the `VirtualFS` seam (lane: infra)

Vet Turso AgentFS against our load: FUSE-mount a `.db`, run a representative
repo checkout + edit burst + concurrent branch reads, measure latency and
crash behavior, and confirm the SQL audit trail captures every write. Freeze
the Go seam the rest of the design consumes. Outcome includes a written
go/no-go against OQ2 (fallback if AgentFS is not production-ready).

- **Interfaces:** produces `go/internal/vfs.VirtualFS`:
  `Mount(ctx, branch BranchID) (mountpoint string, unmount func() error, err error)`,
  `Branch(ctx, parent BranchID) (BranchID, error)`,
  `Audit(ctx, branch BranchID, since time.Time) ([]AuditEntry, error)`.
  Consumes: an AgentFS release pin; nothing in-repo.
- **Depends:** nothing (first task).
- **Test cycle:** spike benchmarks committed as a report in this record's
  directory; interface lands with a fake impl + contract tests.

### V2 — per-agent branch lifecycle in provision (lane: infra)

Wire `VirtualFS` into the Runner's provision flow: a provision request creates
a copy-on-write branch for the agent; teardown retires it; the audit query is
exposed for SEA-1718. `Workspace` (`go/internal/runtime/agent.go:39-40`)
gains a virtual-FS variant without breaking the fused clone-based path.

- **Interfaces:** consumes `vfs.VirtualFS` (V1) and the `SpecBuilder` seam
  (`go/internal/runner/host.go:46-48`); produces a `WorkspaceSource` variant
  on `AgentSpec` (clone-dir today | vfs-branch new) the placements read.
- **Depends:** V1.
- **Test cycle:** provision→branch→teardown round-trip against a real AgentFS
  `.db` in an integration test; fused path regression suite stays green.

### H1 — the `HandsRuntime` seam + tier policy (lane: runner/infra)

Define the backend-selecting exec API and the routing policy. No real backend
yet: land the interface, the policy (which commands go to which tier, the
fail-closed default of Global Constraint 9, the explicit KVM-absent
degradation), and a fake backend for tests.

- **Interfaces:** produces `go/internal/hands.HandsRuntime`:
  `Exec(ctx, HandsSpec) (runtime.ExecOutput, error)` with
  `HandsSpec{Command []string, Dir string, Env map[string]string, Branch vfs.BranchID, Timeout time.Duration}`
  — note `HandsSpec` carries **no** authoritative `Backend`: the Runner's tier
  policy assigns it (`Backend ∈ {BackendIsolate, BackendSandbox, BackendMicroVM, BackendContainer}`),
  a caller hint may only upgrade. Reserve a long-lived variant
  (`ExecStreaming`-shaped: live stdio + kill/wait handle, mirroring
  `go/internal/runtime/podman.go:299-307`) in the seam now for SEA-1720 (OQ7),
  even if unimplemented, so freezing H1 does not force a breaking change later.
  Reuses `runtime.ExecOutput` (`go/internal/runtime/podman.go:139-146`).
  Sibling of `ContainerRuntime`, never a replacement for it.
- **Depends:** V1 (for `vfs.BranchID` in the spec type).
- **Test cycle:** unit tests on the tier-routing policy incl. the fail-closed
  default (unknown command → tier 3) and the KVM-absent degradation being
  explicit (error or logged fallback per policy, never silent).

### H2 — Firecracker backend (lane: runner/infra)

The microVM tier: a minimal kernel + rootfs image carrying the devenv
toolchain, spawn/FUSE-mount(branch)/run/teardown glue in the e2b shape (minus
Nomad/Consul), and a small warm pool so boot latency hides behind the exec.
Hands-side egress: default-deny nft inside the microVM, allowlist from the
hands policy (H4).

- **Interfaces:** implements `hands.HandsRuntime` for `BackendMicroVM`;
  consumes `vfs.VirtualFS.Mount` (V1) exported into the guest over
  virtio-fs/FUSE; consumes the Firecracker API socket (machine config, drives,
  vsock). Produces the rootfs image build (nix, sibling of
  `agent-image/`).
- **Depends:** V1, H1.
- **Test cycle:** integration test on a KVM box — boot, mount a branch with
  pre-made edits, `go test` a tiny module, assert edits were visible with no
  copy step, teardown reclaims the VM; boot-latency budget asserted (<500ms
  p95 warm).

### H3 — sandbox + container fallback backends (lane: runner/infra)

The lighter tiers: bubblewrap/namespaces (`BackendSandbox`) for *trusted*
worker tasks, and the gVisor-hardened container (`BackendContainer`) as the
KVM-absent fallback, reusing `ContainerRuntime`
(`go/internal/runtime/podman.go:286-324`) unchanged underneath.

- **Interfaces:** implements `hands.HandsRuntime` for `BackendSandbox` and
  `BackendContainer`; consumes `runtime.ContainerRuntime` (container path)
  and the AgentFS mountpoint (bind-mounted into the sandbox).
- **Depends:** H1; V1 for the mount.
- **Test cycle:** sandbox denies net + escapes by default (probe tests);
  container fallback runs the same H2 smoke suite behind gVisor.

### H4 — egress split: brain policy vs hands policy (lane: runner/infra)

Split the single `EgressPolicy` (`go/internal/runtime/egress.go:29-34`) into
two derivations in the Runner: the brain allowlist (LLM/API endpoints, git
hosts) and the hands allowlist (default-deny plus build-time hosts), both
fail-closed in the `NftScript()` shape (`go/internal/runtime/egress.go:71-107`).
Applied per placement: nft in the microVM/sandbox netns for hands; the brain's
policy enforced by whichever placement hosts it.

- **Interfaces:** consumes `runtime.EgressPolicy`/`NftScript`; produces
  `BrainEgress`/`HandsEgress` fields where `AgentSpec.Egress`
  (`go/internal/runtime/agent.go:41-42`) is today, derived in `SpecBuilder`.
- **Depends:** H1 (to know where hands policies attach); parallel with H2/H3.
- **Test cycle:** existing egress probe tests extended per placement (blocked
  host unreachable from hands VM; LLM endpoint reachable from brain only).

### B1 — brain tools over the virtual FS + tier-2 in-isolate exec (lane: compass-agent)

Point the agent's read/write/edit tools at the AgentFS mountpoint (tier 1),
and add the tier-2 light exec: shell-to-JS (`just-bash`-style) for grep-class
deterministic ops inside the brain's own bun process — no VM, no container.
Because tier 2 runs *inside* the credential-holding brain process, its
containment is load-bearing (Global Constraint 9): a **fixed builtin command
set** (JS implementations, no shell-out), **no subprocess/FFI**, FS access
**confined to the branch mount**, **no network**, and a **resource ceiling**
(wall-clock + memory) so a misclassified heavy op degrades to a killed tier-2
call, never a wedged brain. Anything outside the builtin set is not tier 2 —
it escalates to tier 3 (B2).

- **Interfaces:** consumes the mountpoint path handed to the agent via env
  (extending `AgentEnv.execSpec`'s env contract,
  `go/internal/runner/agent_exec.go:77-96` — a `COMPASS_VFS_ROOT` variable in
  the same style as `COMPASS_WORKDIR`); produces the tier-2 exec module in
  `packages/compass-agent`.
- **Depends:** V1 (a mountable branch to develop against); independent of H*.
- **Test cycle:** agent-side unit tests over a real FUSE mount incl. the
  containment probes (a subprocess/net attempt from tier 2 is refused; an
  over-budget op is killed); a golden session transcript showing tier-1/2 ops
  never leave the process.

### B2 — brain→hands escalation (lane: compass-agent)

The brain's exec tool classifies a command (tier 2 vs escalate) and sends
tier-3 work to the Runner over the existing per-container/per-agent socket RPC,
carrying the command + branch id. **The brain never names a backend and never
talks to Firecracker directly:** the Runner assigns the isolation tier by
policy (Global Constraint 9); the classifier fails closed (unknown → escalate).

- **Interfaces:** consumes a new Runner RPC `ExecHands(ExecHandsRequest) →
  ExecOutput` where the request carries the command, cwd, env, and branch id
  but **no authoritative `Backend`** (an optional upgrade-only hint at most);
  the Go side derives the tier and dispatches to `hands.HandsRuntime`. Produces
  the agent-side classification policy + tool wiring.
- **Depends:** H1 (interface), B1; H2/H3 for end-to-end.
- **Test cycle:** fake-HandsRuntime integration test — a `go test` command
  escalates, a `grep` does not, an unknown command escalates (fail-closed);
  a hint requesting a weaker tier does not lower it; classification table
  unit-tested.

### B3 — host-process brain placement (lane: runner/infra, with compass-agent)

A second brain placement beside the fused one: the `compass-agent` bundle
(`agent-image/entrypoint.nix:227-229`) runs as a supervised host process
against the AgentFS mount — no per-agent container. `AgentRuntime.Launch`
(`go/internal/runtime/agent.go:173-198`) becomes one of two placements behind a
`BrainPlacement` seam; registry, session RPC resolution
(`go/internal/runtime/registry.go`) and `StartAgent`
(`go/internal/runner/agent_exec.go:136`) resolve either. **The host placement
must reproduce the two guarantees the container gave structurally**, which is
real scope, not a wrapper: (1) **egress** — today default-deny is armed via the
container's netns + `NET_ADMIN` entrypoint running nft
(`go/internal/runtime/agent.go:300-309`, `egress.go:71-107`); a host process has
neither, so it gets its own netns (pasta/slirp + nft, the recommended
mechanism — OQ6) or bwrap network confinement, applying H4's `BrainEgress`
fail-closed; (2) **secrets** — every credential materializer is a container
exec today (`WriteAgentFile` `go/internal/runtime/agent.go:239-260`,
`secrets_materialize.go`), so B3 provides a host-side equivalent honoring the
same 0600 / stdin-not-argv floor.

- **Interfaces:** produces `BrainPlacement` with
  `Launch(ctx, AgentSpec) (BrainHandle, error)` / `Teardown`, implemented by
  the existing container path and the new host-process path; consumes
  `vfs.VirtualFS` (mount), H4's `BrainEgress`, and a host-side secrets
  materializer (0600, stdin-not-argv).
- **Depends:** V2, B1, H4.
- **Test cycle:** dogfood-loop session driven end to end on the host-process
  placement; egress probe (blocked host unreachable, LLM endpoint reachable)
  and a 0600-perms assertion on the materialized secrets; fused-placement
  regression suite stays green (Global Constraint 6).

### E1 — end-to-end split session + tier-mix telemetry (lane: runner/infra)

Drive a full real session on the split path: host-process brain, tier-1/2 ops
in-process, one compile/test escalation into a Firecracker microVM, teardown
reclaims everything. Instrument the tier mix so the <10%-microVM target is
measured, not asserted.

- **Interfaces:** consumes everything above; produces a per-session tier-mix
  counter surfaced through the Runner's session status (the
  `AgentSessionStatus` attribution path, `go/internal/runtime/agent.go:52-56`
  comment trail).
- **Depends:** B2, B3, H2.
- **Test cycle:** the dogfood-loop e2e extended with a split-path variant;
  tier-mix assertion in the harness.

### G1 — orphan reclamation / crash reconciliation (lane: runner/infra)

`AgentRuntime` is deliberately stateless about container existence — the
container engine is the source of truth and `Exists` reconciles after a Runner
crash (`go/internal/runtime/agent.go:1-11`). Firecracker VMs, virtiofsd/FUSE
mounts, and vfs CoW branches have **no** such engine, so a Runner crash leaks
them. Preserve the statelessness property: a persisted inventory of live
VMs/mounts/branches + a startup reconciliation pass that reaps orphans.

- **Interfaces:** consumes `hands.HandsRuntime` (VM handles), `vfs.VirtualFS`
  (branch inventory); produces a reconciliation pass invoked on Runner startup,
  beside the existing container reconcile.
- **Depends:** H2, V2.
- **Test cycle:** kill the Runner mid-exec, restart, assert no leaked VM /
  virtiofsd / mount / branch remains.

## Tasks

Checklist mirroring the plan (owning lane in brackets; order = dependency
order):

- [ ] **M0** [compass-agent] — baseline the tier mix from real dogfood
      session logs (parallel pre-task, no deps).
- [ ] **V1** [infra] — AgentFS vetting spike + freeze `vfs.VirtualFS`
      (`Mount`/`Branch`/`Audit`); go/no-go vs OQ2 fallback.
- [ ] **H1** [runner/infra] — `hands.HandsRuntime` seam + tier-routing policy
      + explicit KVM-absent degradation (depends: V1).
- [ ] **V2** [infra] — per-agent CoW branch lifecycle in provision;
      `WorkspaceSource` variant on `AgentSpec` (depends: V1).
- [ ] **B1** [compass-agent] — brain tools over the AgentFS mount +
      tier-2 in-isolate shell-to-JS exec (depends: V1).
- [ ] **H2** [runner/infra] — Firecracker backend: rootfs image, warm pool,
      mount/run/teardown glue (depends: V1, H1).
- [ ] **H3** [runner/infra] — bubblewrap sandbox + gVisor container fallback
      backends (depends: H1, V1).
- [ ] **H4** [runner/infra] — egress split into `BrainEgress`/`HandsEgress`
      derivations, per-placement fail-closed arming (depends: H1).
- [ ] **B2** [compass-agent] — fail-closed tier classification + `ExecHands`
      escalation RPC (Runner-assigned tier, no agent-authored backend) from
      brain to Runner (depends: H1, B1; e2e needs H2/H3).
- [ ] **B3** [runner/infra + compass-agent] — `BrainPlacement` seam +
      host-process brain placement beside the fused container placement
      (depends: V2, B1, H4).
- [ ] **G1** [runner/infra] — orphan reclamation for microVMs / virtiofsd
      mounts / vfs branches after a Runner crash (depends: H2, V2).
- [ ] **E1** [runner/infra] — full split-path session end to end + tier-mix
      telemetry proving the <10%-microVM target (depends: B2, B3, H2).

## Open Questions

Each tagged **load-bearing** (an executor hits real ambiguity — blocks merge;
the caller asks Matt) or **non-load-bearing** (deferred with rationale). Each
carries a recommendation; the record is drafted against the recommendation as
a stated assumption.

1. **[load-bearing] Firecracker vs gVisor as the default tier-3 boundary for
   the single-box dogfood milestone.** Firecracker is the designed default
   (hardware boundary for model-written code), but the first self-host boxes
   may lack KVM/nested-virt, making gVisor the *de facto* first tier shipped.
   **Recommendation:** Firecracker is the contract default; H3's gVisor
   fallback may land and even ship first where KVM is absent, but H2 remains
   the acceptance bar for E1 — E1 does not close on a gVisor-only box.
2. **[load-bearing] AgentFS production-maturity risk + fallback.** AgentFS is
   young (MIT, Turso); the tier-1/3 "no copy" story leans on it. V1 is the
   vetting spike; the fallback if it fails needs a ruling, and the honest fork
   is between two imperfect options: (a) plain host directory + overlayfs CoW
   branches with the audit trail reconstructed by **writing back the branch
   diff** on teardown (complete, but not real-time), or (b) an fsnotify/inotify
   journal (real-time, but **lossy** — queue overflow drops events, no ordering
   or content capture). Either way the SQL-auditable property that serves
   SEA-1718's "human-auditable, not a black box" constraint **degrades** — this
   is an explicit audit downgrade needing a SEA-1718 ruling, not a shim that
   preserves it. **Recommendation:** (a) overlayfs + branch-diff write-back;
   keep `vfs.VirtualFS` narrow enough that the swap is impl-only. Flag the
   downgrade to SEA-1718 if V1 goes no-go.
3. **[load-bearing] Does the brain↔hands seam extend `AgentRuntime` or sit as
   a new tier below/beside it?** This record designs it as a **sibling**:
   `hands.HandsRuntime` beside `runtime.ContainerRuntime`, with `AgentRuntime`
   narrowing to the fused brain placement behind a new `BrainPlacement` seam.
   The alternative — growing `AgentRuntime` into the split façade — keeps one
   entry point but muddles its container-lifecycle contract
   (`go/internal/runtime/agent.go:150-158`; `ContainerRuntime` is keyed on
   `ContainerID`, `podman.go:286-325`). **Recommendation:** sibling seams as
   designed; the existing seams' signatures stay honest.
4. **[load-bearing] Is increment #1 a full split, or hands-on-demand behind
   the current fused path?** The earlier "bind-mount the existing clone into
   the microVM" idea is **not executable** as-is: the clone is created *inside*
   the container (`go/internal/runtime/workspace.go:3-5`, `podman.go:1-8`),
   there is no host path to bind into a Firecracker guest, and reaching into
   rootless-podman overlay upperdirs of a live container is the shared-mutable
   hack this design refuses. So the real fork is: (a) **increment #1 includes
   V1** — the fused container mounts an AgentFS branch as its checkout
   (V1 → H1 → H2 → B2), hands-on-demand lands on the vfs from day one; or (b)
   add a **host-volume-workspace prerequisite** (a new `W0` moving the clone to
   a host-backed volume, with userns uid-mapping + credential-scoping
   consequences — itself a nontrivial change to the fused path) so hands can
   bind the workspace without vfs. **Recommendation:** (a) — accept the vfs
   dependency in increment #1; it is smaller than `W0` and avoids a
   throwaway host-volume detour. Only E1's host-process-brain acceptance
   belongs to increment #2 (V2/B1/B3).
5. **[load-bearing] FUSE→virtio-fs throughput for heavy builds — does the
   no-copy pillar hold?** Tier 3 mounts the AgentFS branch (host FUSE) and
   re-exports it into the guest over virtio-fs; many-small-file workloads
   (`go test`, `bun install`, git) are the classic FUSE worst case and the
   compile could slow by more than the copy it saves. V1 benchmarks a tier-1
   workload only; nothing benchmarks a **compile inside a microVM over the
   mounted branch** until E1 (last task, after every backend is built).
   **Recommendation:** add a compile-workload benchmark to V1/H2 (go test over
   FUSE→virtio-fs vs a materialized tmpfs/reflink copy), and name the escape
   hatch — for heavy execs, materialize the branch into guest-local storage and
   write back the diff — so "no copy" is a measured default with a fallback,
   decided before three backends are built, not a pillar that can't bend.
6. **[load-bearing] Host-process brain egress + secrets mechanism (blocks
   B3).** Global Constraint 7 promises default-deny for *every* placement, but
   today's enforcement is structurally container-shaped (netns + `NET_ADMIN`
   entrypoint arming nft, `agent.go:300-309`, `egress.go:71-107`; secrets via
   container-exec `WriteAgentFile` `agent.go:239-260`). A supervised host bun
   process has neither. **Recommendation:** give the host placement its own
   netns via **pasta/slirp + nft** (closest to today's mechanism, keeps
   `NftScript()` reusable) with bwrap network confinement as the alternative,
   plus a host-side 0600 / stdin-not-argv secrets materializer — named in B3's
   scope. Matt to confirm the netns primitive.
7. **[load-bearing] `HandsRuntime` long-lived / streaming exec shape
   (SEA-1720).** `Exec` is completion-shaped; SEA-1720's agent-launched dev
   servers need a long-lived process with a live handle/port.
   `ContainerRuntime` itself splits `Exec` vs `ExecStreaming` for this
   (`podman.go:299-307`). If H1 freezes a completion-only seam, SEA-1720 forces
   a breaking change post-freeze. **Recommendation:** reserve a streaming
   variant in the H1 interface now (even unimplemented); defer the dev-server
   port-exposure wiring itself to SEA-1720.
8. **[non-load-bearing] Tier-2 command classification table.** The safety
   invariant (unknown → tier 3; the agent can never lower the tier) now lives
   in **Global Constraint 9**, so the table *contents* are a policy detail
   iterated behind B1/B2's unit-tested allowlist — a wrong classification
   escalates to a slower tier, never opens a hole. Deferred.
9. **[non-load-bearing] Warm-pool sizing and reclaim policy for H2.**
   Pure tuning; measurable once E1's telemetry exists. Deferred — start with
   a pool of 2 and a 60s idle reclaim.
10. **[non-load-bearing] Concurrent-writer coherence on a shared branch**
    (the brain edits while hands compile on the same mount). Assumed fine: the
    single FUSE daemon serializes writes; stated here so an executor does not
    assume independent caches. Revisit only if a backend bypasses the daemon.
11. **[non-load-bearing] Worker-isolate brain placement.** Explicitly optional
    edge; nothing in this plan builds it. Deferred until a managed-edge
    requirement exists — the `BrainPlacement` seam (B3) is where it would plug
    in.
12. **[non-load-bearing] Go-brain density work (SEA-1719).** Composes with but
    is not part of this record; the split makes it possible (an idle goroutine
    brain is near-zero cost, hands in Firecracker regardless), tracked on
    SEA-1719.
