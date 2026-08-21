# Compass hosted agent platform — the elastic session runtime (end-state)

> **Design record.** This designs the hosted multi-tenant agent-platform
> end-state for Compass: the fused, colocated agent+compute session
> environment, hardened along three axes — elastic compute, a persistent
> session volume, and suspend-idle density. Every `go/internal/*`,
> `agent-image/*`, and `docs/*` citation below is a path in the
> **`RigelBuild/compass`** monorepo at HEAD `13a43cec` (line numbers drift as
> the code evolves; resolve them against that commit). It lives in the design
> corpus (`docs/designs/platform/`) because that is where the wave's design
> records freeze. Frozen on merge; executing agents read this as the contract
> for RIG-1717.

Status: Draft
Tracking: RIG-1717

## Problem / Intent

Compass is becoming a **hosted multi-tenant agent platform** — a managed
service built on an open-source core (see *OSS core and managed service*
below). Today's architecture already has the right shape: **one
rootless-podman container per agent, fusing the agent (LLM reasoning +
read/write/edit tools + toolchain + LSP) and its compute (compile/test/run) in
a single environment.**
`AgentRuntime.Launch` (`go/internal/runtime/agent.go:177-198`) creates + starts
the container, arms the default-deny egress firewall (`armEgress`,
`go/internal/runtime/agent.go:300-309`, running `EgressPolicy.NftScript()`,
`go/internal/runtime/egress.go:87`), installs scoped credentials, and creates
the checkout dir; the Runner then execs a bare `compass-agent` argv inside the
container (`agentCommand`, `go/internal/runner/agent_exec.go:32`;
`AgentEnv.execSpec` `:77-96`; `StartAgent` `:136`). That `compass-agent` is a
`bun build --compile` standalone bundle (`agent-image/entrypoint.nix:5-29`,
wrapper `:227-229`).

**The end-state KEEPS this fused, colocated model.** The agent never leaves
its environment; the filesystem stays with the compute; there is no
cross-machine tree transfer. What the fused model lacks today is hardening for
hosted-scale economics and durability, and that is exactly what this record
adds — three axes, nothing more:

1. **Elastic compute (the one genuinely new capability).** Heavy operations —
   full compile, whole test suite, dev server — either **resize the
   session's environment in place** (more cores/RAM) or **burst to a larger
   transient environment that shares the session's storage** (a shared network
   volume, or the same box). A performance/cost optimization: reach for big
   compute only when a task needs it, not for the whole session. It is not
   dispatch to separate hardware tiers and not a tree transfer — the
   filesystem stays with the compute layer.
2. **Persistent session storage.** The session working tree + derived state
   (`target/`, `node_modules`, build caches) live on a **persistent volume**
   that survives suspend / resume / eviction, mounted at a **stable absolute
   path** on every box the session lands on. A plain volume — not a virtual
   filesystem, not content-addressed.
3. **Density via suspend-idle.** Pack many sessions per box; **suspend idle
   sessions** — evict the idle environment (durable state is the session
   transcript, resumed via the `COMPASS_RESUME_SESSION_FILE` contract,
   `go/internal/runner/agent_exec.go:40-44,65-67,92-94`) — and relaunch on
   activity. This is the main density play.

A brain/hands split (separate reasoning box + compute box over an adopted
virtual FS) and a content-addressed VFS were seriously considered for this
end-state and **rejected** — the principle and the reasoning are recorded in
Alternatives considered, so the decision survives re-proposal.

Everything is adopted on **vendor-neutral primitives** — never a
cloud-provider foundation dependency. Sequencing: Beta-phase, explicitly
**after** the Dogfood substrate
(`docs/designs/platform/compass-dogfood-loop/design.md`), landing when hosted
Compass makes infra cost first-order. Nothing in Dogfood blocks on this.

### OSS core and managed service — one architecture, two products

Compass ships as two products over one shared core:

- **OSS core (AGPL, `RigelBuild/compass` — this repo).** The agent runtime and
  every seam this record touches: `AgentRuntime`, `ContainerRuntime`,
  `VirtualFS`, `ComputeRuntime`, the volume lifecycle, the microVM boundary.
  All of this record's `go/internal/*` and `agent-image/*` citations are
  public-repo paths.
- **Managed Compass (private monorepo, dual AGPL + commercial license).** The
  hosted multi-tenant service, which *reuses* the OSS core rather than forking
  it.

**This record designs a change to the OSS core.** All three axes and all seven
tasks land in the public repo — that is why the record lives here and every
code citation resolves against `RigelBuild/compass`. Nothing in the managed
control plane (tenant orchestration, billing, the hosted control surface, all
of which live in the private monorepo) is designed here.

**The motivation is primarily the managed service.** Hosted-scale density (pack
many tenants, suspend the idle ones), durability across eviction, and a
VM-class inter-tenant isolation boundary are managed-multi-tenant economics —
they are what make infra cost first-order and drive this work now.

**OSS self-host deployments benefit too.** The performance and durability axes
— elastic compute, a persistent session volume, fast suspend/resume — improve
any deployment, single-tenant included; the microVM boundary (I1) hardens the
host against model-written code whether or not a second tenant exists. Only the
*multi-tenant* framing (many tenants per box, inter-tenant isolation as a
product requirement) is managed-specific — the mechanisms are all OSS.

## Approach

### The Spine — one fused session environment, hardened three ways

The record's central thesis:

1. **The session environment is the unit.** One environment per session fuses
   the agent loop, the working tree, the toolchain, and the LSP servers. The
   agent runs *inside* it for the session's whole life; the environment is
   never compute-less, because the inner loop (LSP above all) needs real
   CPU/RAM continuously.
2. **Compute is elastic, temporally.** The session's environment is sized for
   the inner loop by default and grows — resize-in-place or burst to a bigger
   transient environment sharing the same storage — exactly for the duration
   of a heavy op, then shrinks/reclaims. Elasticity is *temporal* (grow on
   demand, reclaim after), never *topological* (no standing second tier).
3. **Storage is persistent and colocated.** The working tree + derived state
   live on a per-session persistent volume at a stable absolute path. Suspend,
   resume, and eviction never lose it; a heavy-op burst environment mounts the
   same volume, so there is no copy-in/copy-out and no tree transfer.
4. **Density comes from suspend-idle.** An idle session's environment is
   evicted entirely; the durable session state is the transcript
   (`COMPASS_RESUME_SESSION_FILE`, `go/internal/runner/agent_exec.go:40-44`)
   plus the persistent volume. Relaunch on activity. No always-resident cheap
   tier is needed to make idle sessions cheap — an evicted session costs
   storage only.
5. **Go-through-the-seams is the load-bearing discipline.** The elastic/burst
   and suspend-idle work lands incrementally behind three seams (below) with
   trivial fused-model configurations first, so each hardening axis is an
   implementation swap behind a frozen interface, never a big-bang rewrite.
   Every bypass — a direct `exec`, a raw-disk assumption outside the volume —
   deletes that migration path (Global Constraint 2).

### Tiered execution — how heavy an op is, not whether compute exists

The agent always runs inside a real environment with toolchain + LSP. Tiers
describe operation weight:

- **Inner loop (the default):** read / edit / reason / LSP (gopls,
  rust-analyzer, tsserver, pyright) / grep / small deterministic shell ops —
  all inside the session's own environment. The overwhelming majority of a
  PR's operations. LSP is load-bearing here: go-to-def, diagnostics, and
  formatters need the materialized tree + toolchain + real CPU/RAM
  continuously — this is *why* the environment is never compute-less.
- **Heavy op (the escalation):** full compile, whole test suite, dev server —
  resize-in-place or burst to a bigger transient environment
  sharing the session's storage. Target: a small fraction of operations; M0
  measures the real fraction rather than asserting one.

Routing between the two is Runner policy and **fails closed**: an
unknown/unclassified op escalates to the heavy path, and the agent can never
force a smaller or cheaper boundary than policy allows (Global Constraint 3).

### Code entry — parity with the customer's humans

The agent gets the **same working copy the customer's own humans get**:

- **Normal repos:** a plain git checkout on the persistent volume,
  volume-snapshotted so subsequent sessions skip the first clone (the
  Codespaces prebuild model).
- **Large repos:** **git sparse-checkout** — already supported because git is
  on the box; this is where Microsoft/Scalar landed.
- **Customer-owned VFS:** if a customer runs their own large-repo VFS
  (EdenFS/Cider-class), Compass **interops with it** as a mounted source of
  tree. Compass builds no VFS of its own — it will not serve
  Google/Meta-scale monorepos (organizations at that scale build their own
  layer), so the scale where a content-addressed VFS pays is exactly the
  scale that already owns one.

### The end-state topology

- **One environment per session, one box at a time.** The Server is the
  control plane: identity, persona/role, session lifecycle from the outside,
  secrets brokering, forge relay (the Server holds the sole forge write
  credential as a `server_only` declared secret, DL-052,
  `docs/designs/product/DECISIONS.md:75`). The Server places nothing and
  knows no internals of how sessions are packed.
- **The agent is a resident process per session** (the bun bundle,
  `agent-image/entrypoint.nix:227-229`): it hosts stateful in-process
  extensions (push-guard/git-guard as `tool_call` gates) and MCP servers that
  run session-long in-heap. Density comes from suspending idle sessions, not
  from shrinking the per-process footprint.
- **Heavy ops stay inside the session's trust and storage domain.** A burst
  environment is transient, mounts the session's volume at the same stable
  path, runs under the same session-scoped egress policy, and is reclaimed
  after the op. Edits made before the op are already present (same volume);
  artifacts written by the op are already present after (same volume).

### The three seams

The package layering (`go/internal/runtime/podman.go:10-20`) already isolates
the engine behind the `ContainerRuntime` interface
(`go/internal/runtime/podman.go:286-324`) — "everything above depends on the
interface, so a libpod-REST backend can replace it without touching a caller."
The hardening work reuses that discipline:

- **`ContainerRuntime` — existing verbs frozen, extended additively.** It
  remains the engine seam (create/start/exec/stop against a `ContainerID`,
  `go/internal/runtime/podman.go:286-324`), including `ExecStreaming`
  (`go/internal/runtime/podman.go:299-307`) for the long-lived agent process.
  Resize-in-place adds one verb — `Resize(ctx, id, ResourceLimits)` (a
  `podman update`-class cgroup limit change), naming a concrete cgroup-limit
  type distinct from the `ComputeSpec.Resources ResourceClass` policy enum
  below — so resize reaches the engine *through* the seam, never by shelling
  past it (Global Constraint 2). The verb is **frozen into the interface at
  S1** (additively reserved, the same discipline as the reserved streaming
  variant), so I1's microVM backend and every fake carry it from the start;
  C3 fills in only the resize *behavior* behind the already-frozen seam — no
  interface change lands after S1. "Frozen" means no existing verb's
  signature changes. (Rootless `podman update` needs cgroups v2 + systemd
  delegation; where the box lacks it, C3's resize backend falls back to burst
  — the capability hedge already in C3.)
- **`VirtualFS` — a thin source-of-tree seam.** It abstracts *where the
  working tree comes from*: plain checkout | git sparse-checkout | volume
  snapshot | a mounted customer VFS. It is deliberately minimal — not a
  content-addressed filesystem, no Merkle/hash-sync primitive, no
  branch-by-hash. It is the seam behind which "persistent volume now →
  interop-with-customer-VFS later" can swap without over-building today.
- **`ComputeRuntime` — the elastic-compute seam (the one genuinely new
  abstraction).** Named `ComputeRuntime` (over `ExecRuntime`) because what it
  abstracts is the compute *capacity* an exec runs against, not the exec
  mechanics `ContainerRuntime` already owns. It routes a heavy op to a
  backend: **run-in-place** (the session's own environment, optionally
  resized) vs **burst** (a bigger transient environment sharing the session's
  volume). It is justified by a capability no existing seam carries:
  `ContainerRuntime.Exec` (`go/internal/runtime/podman.go:286-324`) models an
  exec against a fixed, already-sized container, while a heavy op needs an
  exec whose *sizing and placement* are chosen by policy at call time.
  `Exec` is completion-shaped; a **streaming variant is reserved in the seam
  now** (live stdio + kill/wait handle, mirroring how `ContainerRuntime`
  splits `Exec`/`ExecStreaming`, `go/internal/runtime/podman.go:293-307`) for
  SEA-1720's agent-launched dev servers, even if unimplemented, so freezing
  the seam does not force a breaking change later.

There is no placement seam for the agent process itself: the agent runs in
the session environment, period. (A separate lightweight edit-only placement
for managed-fleet density remains at most a deferred, optional optimization
hook, explicitly out of scope for this record — no plan task builds toward
it.)

- **The Runner's substrate stays runtime-agnostic.** `SpecBuilder`
  (`go/internal/runner/host.go:46-48`) keeps assembling specs from operator
  defaults + the provision request; the dogfood loop moves under the hardened
  runtime unchanged, per the sequencing constraint. `AgentSpec`
  (`go/internal/runtime/agent.go:32-57` — fields: Name, Image, Workspace,
  Egress, Mounts, Persona, Role, AgentAccountID; the model selector is *not*
  here, it rides `AgentEnv.Model` at exec time,
  `go/internal/runner/agent_exec.go:58-59`) gains a `WorkspaceSource` variant
  for the volume-backed tree without breaking the clone-based path.

### Security — the session sandbox is the boundary

The security floors carry over unchanged, with one framing correction against
earlier drafts: **within a single tenant's session, the agent drives both
editing and exec — one trust domain.** There is no trusted-editing /
untrusted-exec boundary *inside* a session; the real isolation boundary is
**the session sandbox vs other tenants and the host**. Concretely:

- **Default-deny egress, session-scoped, fail-closed.** One `EgressPolicy`
  (`go/internal/runtime/egress.go:29-34`) covers the session environment; its
  `NftScript()` base ruleset fails closed — if any table/set/chain/policy-drop
  rule fails to install, the script aborts and the caller tears the
  environment down rather than running it unfirewalled
  (`go/internal/runtime/egress.go:71-107`). A burst environment arms the same
  session policy before the op runs. Egress is per-session, never
  split-across-tiers.
- **Model-written code runs inside an isolation boundary** — the session
  sandbox, which is a hardware-virtualized microVM in the end state (I1) and a
  rootless-podman container through Dogfood + trusted-tenant Beta. Both the
  resident session environment and its burst environments run on it. The agent
  process itself holds no capability to alter the ruleset
  (`go/internal/runner/agent_exec.go:46-53`).
- **Secrets keep the 0600 / stdin-not-argv discipline**
  (`WriteAgentFile`, `go/internal/runtime/agent.go:238-258`: body over stdin,
  never argv; 0600 under umask 077). Secrets are injected into the session
  environment, session-scoped; a burst environment receives at most the
  JIT-scoped credentials the specific op needs.
- **The Server holds the sole forge write credential** (`server_only`
  declared secret, DL-052) — unchanged.

### Derived-state persistence — always-persist

Losing a large Rust/C++ `target/` makes Compass unusable (a from-scratch
compile is too long); the same holds for `node_modules`. The product promise
is "as seamless as an agent on your local box."

- **Always-persist.** All non-trivial derived state (`target/`,
  `node_modules`, build caches) lives on the per-session persistent volume,
  surviving suspend / resume / eviction.
- **sccache is a complement, not a replacement**: sccache is incompatible
  with incremental compilation (requires `CARGO_INCREMENTAL=0`), never caches
  linking, and is path-sensitive — a warm `target/` is strictly more. Run
  sccache as a cross-session shared compile cache in addition to persisting
  `target/`.
- **Stable workspace path invariant**: each session's volume mounts at the
  same absolute path on every box (we own the mount), keeping `target/` and
  sccache valid across a box move.
- **Bounded liability**: always-persist × per-session × multi-GB needs an
  expiry policy (a default expiry after session close, P2) — persistence is a
  product guarantee within a session's life, not an unbounded archive.
- **Box loss ≠ eviction**: eviction reclaims compute and never touches the
  volume; loss of the box (and its local volume) is an accepted degradation —
  the session resumes from the transcript and re-materializes the tree from
  the forge, paying a cold clone + cold build once (P2 states this
  explicitly).

## Alternatives considered

### The brain/hands split — seriously considered, rejected

A persistent split of the session into a cheap "brain" tier (reasoning +
editing on light boxes) and a heavy "hands" tier (compile/test in microVMs on
big boxes), connected by a shared virtual FS, was a full prior draft of this
record. It is rejected on a **principle**, stated here so the record survives
the split being re-proposed:

**A persistent brain/hands split pays only when BOTH conditions hold: (1) the
cheap reasoning/editing tier is genuinely cheap, and (2) heavy compute is rare
and cleanly isolatable.** The axis is those two conditions — not "coding vs
non-coding." Software engineering fails both:

- The inner loop is not cheap. LSP (gopls, rust-analyzer, tsserver),
  go-to-def, diagnostics, formatters, and git all need the materialized tree +
  toolchain + real CPU/RAM *continuously* — the "just editing" tier is
  already a full dev environment, with nothing light to peel off.
- Heavy ops are not a different *kind* of environment, just a bigger one.
  Though a small fraction of operations by count, they interleave with the
  inner loop that produces them (compile → edit → compile), so they cannot be
  peeled into a standing tier the session rarely touches. Condition (2) fails
  on *isolatability in time*, not on aggregate frequency.

So for a coding agent the split's premise is simply false. The two apparent
rescues collapse into simpler mechanisms this record already contains:

- **Security isolation** (run model-written code away from reasoning/creds)
  is delivered by sandboxing the whole *session* against other tenants + the
  host. Within one tenant's session, editing and exec are the same trust
  domain — no intra-session brain/hands boundary is needed (see Security).
- **Fleet density** (don't hold a big environment per idle session) is
  delivered by *suspend-idle*: evict the whole environment, resume from the
  session transcript. The resource is reclaimed without splitting the running
  session.

The only asymmetry that survives is **elastic burst for the genuinely-heavy
op — which is TEMPORAL (resize/burst on demand, then reclaim), not
TOPOLOGICAL (two standing tiers)** — and that is exactly this record's
design.

The split fails on **both** sides of the market, for opposite reasons.
Coding fails it as above: the cheap tier isn't cheap (the LSP inner loop is a
full dev environment) and heavy compute is constant. The general
computer-use / knowledge-work agent fails it the other way: that agent's
"compute" is not local — it drives SaaS backends over HTTP (Google Sheets,
web IDEs, Salesforce), so the heavy tier lives on someone else's servers, and
there is no heavy *local* tier to split off — only a browser/driver plus
reasoning, one modest environment.

Even that thin middle collapses. Heavy self-hosted analytics — the obvious
candidate for it — goes two ways, each back into a case already covered: run it
on a cloud analytics platform (BigQuery / Snowflake / Databricks / a hosted
notebook) and the compute is remote again (the computer-use collapse); run it
attached to the agent and you are writing Python/SQL/R — a coding agent, back
to the LSP inner loop. Driving computation yourself *is* scripting; "heavy
self-hosted compute driven by the agent but not by writing code" has no
members. The only genuine remainder is a **local-only application with no cloud
backend** (legacy desktop / CAD / air-gapped software — real, shrinking, tiny),
and even there the answer is not a split: it is a computer-use agent in one
sandboxed environment with a display, reasoning and app colocated, with no
heavy isolatable tier to carve. So the persistent split serves only a narrow
set Compass is not targeting — the honest exception is a self-hosted GPU
train/inference workload, where heavy compute genuinely *is* rare,
long-running, and cleanly isolatable (split condition 2 holds); Compass, a
coding agent, is not that workload. For everything Compass does, every real
need the split reaches for (isolation, fleet density, burst headroom) is met
more cleanly without it — by session sandboxing, suspend-idle, and temporal
elasticity respectively. The colocated elastic-session model is the fit.

The one honest steelman, stated precisely so this record doesn't overclaim:
you *do* want model-written code inside a hardware/OS isolation boundary —
but that is **session sandboxing** (a per-session boundary), not a
brain/hands split (a topology). The sandbox delivers the isolation; the split
adds nothing on top of it. Conflating the two is the trap the industry
pattern fell into.

### Content-addressed VFS (Merkle-root snapshots, hash-sync) — rejected

The prior draft adopted a JuiceFS-shaped content-addressed VFS (metadata DB +
hash-keyed blob store + FUSE lazy fetch) with a "sync to root hash R"
transfer primitive. Rejected, not deferred, for two reasons:

- The VFS existed to solve cross-machine tree transfer between brain and
  hands boxes. With the agent colocated with its filesystem — and a burst
  environment *sharing* the session's volume rather than receiving a synced
  copy — that problem does not exist.
- At the scale where lazy monorepo materialization pays
  (Google/Meta-class), the customer already owns that layer (their own
  EdenFS-class VFS, or git sparse-checkout). Compass interops with a mounted
  customer VFS behind the `VirtualFS` source-of-tree seam; it never builds or
  adopts one.

### Turso AgentFS / SQLite as the foundation — rejected (history)

An earlier draft chose AgentFS (a POSIX FS inside one SQLite `.db`); rejected
— alpha-maturity, single-writer SQLite is the wrong substrate for a
multi-tenant platform, and it stores file content in the relational store.

### "Tier 1 has no compute environment" — rejected

A compute-less editing tier (workspace-FS-only, shell-to-JS in-process exec)
cannot host the LSP inner loop, which needs the materialized tree, the
toolchain, and real CPU/RAM. Tiers in this record describe operation weight
inside a real environment, never the absence of one.

## Global Constraints

1. **Vendor neutrality (hard rule).** No cloud-provider foundation dependency
   anywhere in the stack. Every primitive must be self-hostable: rootless
   podman, git, nftables, Postgres, an S3-compatible object store, our own
   agent loop. A managed-cloud volume or resize API may implement a seam
   backend, never define one.
2. **Go through the seams (hard rule).** Every working-tree materialization
   goes through `VirtualFS`; every heavy-op exec goes through
   `ComputeRuntime`; the engine stays behind `ContainerRuntime`. No direct
   `exec` for a heavy op, no raw-disk path outside the session volume. Every
   bypass deletes the incremental-hardening migration path; a bypass is a
   design violation, not a shortcut.
3. **Compute routing fails closed; the agent can never lower isolation or
   sizing policy.** The backend (run-in-place vs burst, and the burst
   boundary) for any heavy op is chosen by Runner policy, not by the
   model-driven agent: an unknown/unclassified op escalates to the heavy
   path, and the exec request carries no authoritative backend from the agent
   — at most an upgrade-only hint policy may raise, never lower.
4. **Security floors carry over to every environment the session touches.**
   Default-deny egress (fail-closed, `go/internal/runtime/egress.go:71-107`
   semantics) arms in the session environment and in every burst environment
   before an op runs; model-written code never executes outside the session's
   isolation boundary; secrets keep the 0600 / stdin-not-argv discipline
   (`go/internal/runtime/agent.go:238-258`); the Server-only forge-credential
   posture (DL-052) is unchanged. Egress and secrets are session-scoped.
5. **Derived state always persists.** `target/`, `node_modules`, and build
   caches live on the per-session persistent volume, mounted at a stable
   absolute path on every box the session (or its burst) lands on. Eviction
   may reclaim compute, never the volume; the volume expires only by policy
   (P2), never by crash or eviction.
6. **The substrate stays runtime-agnostic.** The Runner's provision flow —
   the `SpecBuilder` seam (`go/internal/runner/host.go:46-48`), the
   dispatch/session plumbing (`go/internal/runner/agent_exec.go`,
   `dispatch.go`, `host.go`) — keeps its interfaces; the hardening lands as
   new implementations behind existing seams plus the two new ones
   (`VirtualFS`, `ComputeRuntime`). The dogfood loop
   (`docs/designs/platform/compass-dogfood-loop/design.md`) must keep passing
   unchanged at every increment.
7. **Sequencing: Beta-phase, after Dogfood.** Nothing in the Dogfood
   milestone blocks on this record; no task here may become a Dogfood
   dependency. Elastic-compute and density work start only when real session
   corpora exist to measure against (M0).
8. **The existing session path stays green throughout.** `AgentRuntime.Launch`
   (`go/internal/runtime/agent.go:177-198`) remains the working launch path
   at every increment; changes are additive behind seams, never a big-bang
   cutover.
9. **Version floors.** podman ≥ 4.3 (the `--userns=keep-id:uid=,gid=` floor,
   `go/internal/runtime/podman.go:405-413`) unchanged. The microVM boundary
   (I1) adds a KVM/nested-virt floor and a microVM-runtime floor (krun/libkrun
   or kata); a box without KVM degrades to the container runtime with an
   explicit capability log (I1), never silently.

## Plan

Incremental, all *through the seams* — going through the seams is the
migration discipline that lets elastic/burst and suspend-idle land without a
big-bang rewrite. Seven tasks: **M0** (parallel pre-task), **S1** (the seams,
fused configs), **I1** (the microVM inter-tenant boundary), **P2** (persistent
volume), **C3** (elastic compute), **D4** (suspend-idle density), **E5**
(end-to-end validation). Lanes: **seams/volume/isolation = infra**,
**compute/density = runner/infra**, **measurement = compass-agent**. M0, S1,
and I1 have no code dependency on each other and run in parallel.

### M0 — baseline the tier mix + working-set sizes from real sessions (lane: compass-agent, parallel pre-task)

Ground the design's two calibration inputs from existing dogfood-loop session
logs (the substrate already emits tool-call telemetry) before backends get
built:

- **Tier mix**: classify tool-calls into inner-loop vs heavy-op and report
  the real distribution. This calibrates the elastic-compute routing policy
  and sets E5's measured acceptance target.
- **Working-set size distribution in GB**: measure the working tree + derived
  state per session in gigabytes — repo *count* is a poor proxy, a repo
  varies wildly in size. This feeds the persistent-volume sizing and the
  expiry-policy cost model (P2).

- **Interfaces:** consumes existing dogfood session tool-call logs and
  session workspace measurements; produces a one-off report (tier-mix
  distribution + working-set GB histogram) committed in this record's
  directory. No seam. The telemetry ingest spine exists
  (`go/internal/runner/gateway/publish.go` forwards session/trace frames
  upstream), but M0's first sub-step is to confirm the retained frames are
  queryable at tool-call granularity and that workspace size is recoverable —
  if not, M0 adds the minimal capture before classifying (this is the one
  measurement gate, cheap, no backend depends on it).
- **Depends:** nothing (runs in parallel with S1).
- **Test cycle:** the report itself; the classification + measurement pass
  rerunnable as sessions accrue.

### S1 — the seams, landed with their fused in-container configurations (lane: infra)

Freeze the two new Go seams and land their trivial fused-model
configurations end to end, with `ContainerRuntime`'s existing verbs frozen:

- **`VirtualFS`** — the thin source-of-tree seam plus its checkout backend.
  At S1 the destination is **today's clone-dir workspace** (the genuinely
  trivial fused config; the persistent volume does not exist until P2, which
  depends on S1 — so S1 never references it). The tree is materialized by
  plain git checkout (sparse-checkout for large repos is a parameter of the
  same backend, not a second one). `Workspace`
  (`go/internal/runtime/agent.go:39-40`) gains a `WorkspaceSource` variant
  (clone-dir today | volume-backed at P2) without breaking the clone-based
  path.
- **`ComputeRuntime`** — the elastic-compute seam plus its in-environment
  local-exec backend (the fused passthrough: the op runs in the session's
  own environment at its current size), and the fail-closed routing-policy
  shell (Global Constraint 3) even while only one backend exists.
- Provision wiring: a provision request materializes the tree through
  `VirtualFS`; every heavy-op exec path routes through `ComputeRuntime`.

- **Interfaces:** produces `go/internal/vfs.VirtualFS`:
  `Materialize(ctx, src TreeSource) (root string, err error)` +
  `Release(ctx, root string) error`, where
  `TreeSource{Repo string, Ref string, Sparse []string, Snapshot VolumeSnapshotID, CustomerMount string}`
  selects checkout | sparse-checkout | volume snapshot | mounted customer
  VFS. The materialization *destination* is binding state of the `VirtualFS`
  instance (constructed with the target root — the clone-dir at S1, the
  session volume at P2), not a `Materialize` parameter, so P2 swaps the
  destination behind the frozen signature. `VolumeSnapshotID` is frozen at S1
  as an **opaque string** (its production shape is P2's to define). Produces
  `go/internal/compute.ComputeRuntime`:
  `Exec(ctx, ComputeSpec) (runtime.ExecOutput, error)` with
  `ComputeSpec{Command []string, Dir string, Env map[string]string, Resources ResourceClass, Timeout time.Duration}`
  — `Resources` is assigned by Runner policy, never authored by the agent
  (an agent hint may only upgrade). `ComputeRuntime` is **session-scoped**:
  constructed with the session's container handle, its volume, and its egress
  policy, so a backend has what run-in-place (the container), burst (the
  volume mount), and egress arming need without threading them through every
  `Exec` call. A streaming variant (`ExecStreaming`-shaped: live stdio +
  kill/wait handle, mirroring `go/internal/runtime/podman.go:299-307`) is
  **reserved in the interface now** for SEA-1720, even if unimplemented.
  Reuses `runtime.ExecOutput` (`go/internal/runtime/podman.go:139-146`) —
  fully buffered stdout/stderr, an accepted limit for whole-suite output
  until the streaming variant lands. `SpecBuilder`
  (`go/internal/runner/host.go:46-48`) derives the `WorkspaceSource`.
  `ContainerRuntime` also gains the additively-reserved
  `Resize(ctx, id ContainerID, limits ResourceLimits) error` — frozen here,
  unimplemented until C3 — so I1's microVM backend and every fake carry the
  full surface from the start and C3 lands no interface change.
  `ResourceLimits{CPUShares int, MemoryBytes int64}` is the concrete
  cgroup-limit struct, distinct from the `ComputeSpec.Resources ResourceClass`
  policy enum.
- **Depends:** nothing (first code task; M0 parallel).
- **Test cycle:** contract tests a fake and the real backend both pass, per
  seam; provision→materialize→session→release round-trip in an integration
  test; routing-policy unit tests incl. the fail-closed default (unknown op
  escalates) and hint-cannot-downgrade; clone-path regression suite stays
  green.

### I1 — the microVM inter-tenant boundary (lane: infra)

The committed inter-tenant isolation boundary (OQ 5, resolved): model-written
customer code runs inside a hardware-virtualized microVM, not only a rootless
container. This is the boundary an external multi-tenant customer requires,
and — since winning that customer depends on having it — it is built early,
not deferred behind the customer. The descoping of the split and the
content-addressed VFS is what frees the capacity to build it now.

- **Backend behind `ContainerRuntime`:** slot a microVM OCI runtime
  (krun/libkrun or kata) via podman's `--runtime` selection, so the engine
  seam (`go/internal/runtime/podman.go:286-324`) is reused rather than
  replaced where possible. The real work is above the seam: a microVM-bootable
  rootfs image carrying the devenv toolchain, boot/teardown plumbing, and
  arming the default-deny egress (`NftScript()`) inside the guest netns.
- **Same trust story, stronger boundary:** the session sandbox stays the
  isolation unit (Security section); the microVM makes it a hardware boundary
  against other tenants + host instead of a shared-kernel one. Both the
  resident session environment and C3's burst environments run on it.
- **Storage:** the session volume reaches the guest by virtio-fs at the same
  stable absolute path, preserving the no-copy invariant P2/C3 rely on.

- **Interfaces:** produces the microVM runtime binding behind
  `runtime.ContainerRuntime` (runtime selection + the rootfs image build +
  guest egress arming); consumes `runtime.EgressPolicy.NftScript()`
  (`go/internal/runtime/egress.go:71-107`). No new caller-facing seam — the
  boundary is an engine/runtime configuration behind the existing interface.
- **Depends:** nothing structurally (parallel with M0/S1); C3's burst backend
  consumes it.
- **Test cycle:** a session and a burst both boot on the microVM runtime and
  pass the S1/C3 contract tests unchanged; an inter-tenant isolation probe
  (a guest cannot reach another tenant's volume, the metadata/host network, or
  the host filesystem); egress fail-closed asserted inside the guest netns;
  the KVM-absent path degrades to the container runtime with an explicit
  capability log, never silently.

### P2 — persistent session volume + derived-state persistence (lane: infra)

The per-session persistent volume carrying the working tree AND derived state
(`target/`, `node_modules`, build caches), surviving suspend / resume /
eviction, mounted at a stable absolute path:

- **Attach mechanism (named):** a **local volume with session→box
  stickiness** — the volume is a directory subtree on the box's fast local
  storage, and the scheduler pins a session (and its bursts) to that box
  while the volume lives. Chosen over network block storage because it is
  vendor-neutral, needs no storage fabric, and matches the single-box
  deployments Compass runs today; the tradeoff (a burst cannot land on a
  different box until a network-volume backend exists) is accepted and
  recorded in OQ 2. The volume lifecycle (create / attach / reattach /
  expire) is owned Runner-side beside the container lifecycle.
- **Default expiry policy:** a session's volume persists for **14 days after
  session close**, then is reclaimed — always-persist × per-session ×
  multi-GB is otherwise an unbounded liability. The value is tunable per
  deployment; M0's working-set GB distribution calibrates it (OQ 3).
- **Volume snapshots** for first-clone amortization: a repo's freshly
  materialized tree is snapshotted (at the FS layer where the box's
  filesystem supports reflink/snapshot; by rsync-clone otherwise) so later
  sessions on the same repo start warm — the Codespaces prebuild model,
  behind `VirtualFS`'s `Snapshot` source.
- **sccache as the cross-session complement** (incompatible with incremental
  compilation — requires `CARGO_INCREMENTAL=0` — never caches linking,
  path-sensitive: a warm `target/` is strictly more; run both).
- **Box loss is an accepted degradation** (vs eviction, which never touches
  the volume): losing the box loses the volume; the session resumes from the
  transcript and re-materializes through `VirtualFS`, paying one cold clone +
  cold build.

- **Interfaces:** produces the volume lifecycle API in `go/internal/vfs`:
  `CreateVolume(ctx, sessionID string) (Volume, error)`,
  `Attach(ctx, v Volume) (path string, err error)` (stable absolute path),
  `Snapshot(ctx, v Volume) (VolumeSnapshotID, error)`,
  `Archive(ctx, v Volume) (ArchiveRef, error)` + `Restore(ctx, ArchiveRef) (Volume, error)`
  (push the volume to / pull it from the vendor-neutral object store for
  cold-idle — D4), and `Expire(ctx, olderThan time.Duration) error`; consumes
  `VirtualFS` (S1) for materialization onto the volume; `SpecBuilder` mounts
  the volume into the session container via `AgentSpec.Mounts`
  (`go/internal/runtime/agent.go:43-44`). This deliberately amends that
  field's documented contract (today "read-only host mounts") to carry the
  writable session volume — mechanically already supported (`Mount.ReadOnly`
  is a per-mount bool), so P2 updates the doc comment rather than the shape.
- **Depends:** S1.
- **Test cycle:** teardown-then-reprovision keeps `target/` warm (asserted by
  an incremental-build probe); the stable-path invariant asserted across
  reattach; a snapshot-materialized session skips the clone (probe); expiry
  reaps only closed-session volumes past the deadline; simulated box loss
  resumes the session cold without error.

### C3 — elastic compute: resize-in-place + burst backends (lane: runner/infra)

The two elastic backends behind `ComputeRuntime`, and the routing policy that
picks one:

- **Resize-in-place:** raise the session environment's CPU/memory limits for
  the op's duration, then restore, via `ContainerRuntime.Resize`. Available
  where the runtime supports live limit changes (rootless podman on cgroups
  v2); under the microVM boundary (I1) live memory hotplug is limited, so
  resize covers CPU/headroom cases and otherwise falls back to burst. The
  cheapest path when the box has headroom and the runtime allows it.
- **Burst-to-transient-environment (the primary elastic path):** spawn a
  right-sized transient **microVM** (I1's boundary) **on the same box** (P2's
  stickiness), mounting the session's volume at the same stable path, arming
  the same session egress policy, running the op, and reclaiming it. Same-box
  burst makes storage sharing a bind/virtio-fs mount; a cross-box burst
  backend is deferred until a network-volume attach exists (OQ 2). Because a
  fresh microVM is right-sized at spawn, burst — not resize — is the primary
  way a heavy op gets its compute.
- **Routing policy:** Runner-side, fail-closed (Global Constraint 3): a
  classified inner-loop op runs in place unresized; a classified heavy op
  goes to resize or burst per resource class; an unknown op escalates to the
  heavy path. The agent can hint upward only. Policy thresholds are
  M0-calibrated.
- **Crash reconciliation:** transient burst environments have no engine of
  record beyond the container engine itself; a Runner-startup reconciliation
  pass reaps orphaned burst environments and restores resized limits
  (volumes expire on policy, never on crash — Global Constraint 5).

- **Interfaces:** implements `compute.ComputeRuntime` for
  `ResourceClass ∈ {ClassInner, ClassResized, ClassBurst}`; consumes the P2
  volume attach (burst mount), `runtime.EgressPolicy.NftScript()`
  (`go/internal/runtime/egress.go:87`) to arm the burst environment, and
  `runtime.ContainerRuntime` for the transient environment's lifecycle.
  Produces the routing-policy table + its config surface + the startup
  reconciliation pass.
- **Depends:** S1, P2, I1 (the burst environment is I1's microVM boundary);
  M0 for policy calibration (thresholds only — the backends do not block on
  M0).
- **Test cycle:** integration: a compile op under `ClassBurst` sees
  pre-existing edits with no copy step and its artifacts persist on the
  volume after reclaim; egress probe inside the burst environment (blocked
  host unreachable); resize restores original limits after the op;
  fail-closed routing asserted (unknown op → heavy path; downgrade hint
  refused); burst environment reclaimed on op completion, timeout, and
  Runner crash (reconciliation test: kill the Runner mid-op, restart, no
  leaked environment, volume intact).

### D4 — suspend-idle density (lane: runner/infra)

The main density play: evict an idle session's environment entirely and
relaunch on activity.

- **Suspend (warm idle):** on idle (policy-defined inactivity), stop and
  remove the session container but leave the persistent volume on its box.
  Durable state = the server-reconstructed session transcript (resumed via
  `COMPASS_RESUME_SESSION_FILE`, `go/internal/runner/agent_exec.go:40-44,92-94`)
  plus the volume (P2). Resume is fastest here — the warm `target/` is still
  local.
- **Archive (cold idle):** past a deeper idle threshold, archive the volume
  (working tree + `target/` + caches) to the vendor-neutral object store and
  free the box's local disk entirely. A cold-idle session then costs object
  storage only — pennies, not a reserved local disk — which is what makes
  suspend-idle density real rather than a disk reservation. This is the
  natural escalation of eviction: warm idle frees compute, cold idle frees
  storage too. (Cold idle is default-on broadly — rehydration is not a
  first-order UX cost in the async-tree model, see Resume below; only the
  thresholds and retention window are left to tune, OQ 6. The volume lifecycle
  API from P2 carries the archive/restore verbs.)
- **What resume reconstructs (and what is lost by design).** The durable state
  is exactly the transcript + the volume; everything else is rebuilt on
  relaunch. `AgentRuntime.Launch` re-execs the bun bundle, so the MCP servers
  respawn and the in-process extensions (push-guard/git-guard) re-initialize
  from the transcript — their session-long *in-heap* state is intentionally
  not preserved (it is a function of the transcript, so it reconstructs).
  What does *not* survive, by design: in-flight ops at suspend time, live
  background processes, and any tool state written outside the tree (the
  scoped `$HOME` is container-local, reinstalled by `Launch`, not on the
  volume — so `$HOME`-installed tooling re-installs; if a session's
  seamlessness ever depends on `$HOME` state surviving, that is a later
  decision to move `$HOME` onto the volume, not assumed here). Consequently
  **idle detection gates on there being no live streaming exec**: a running
  dev server (SEA-1720) or an in-flight heavy op blocks suspend — suspending
  under one would kill it, which the user would see. Idle means the inner loop
  is quiescent *and* nothing long-lived is running.
- **Resume:** on activity, relaunch through the standard `AgentRuntime.Launch`
  path (`go/internal/runtime/agent.go:177-198`) with the volume restored at
  the stable path (rehydrated from the object store on a cold-idle resume,
  reattached in place on a warm-idle resume) and the resume file
  materialized; the agent reloads the session and continues. Cold-idle resume
  pays a rehydration pull, and **Compass's usage model absorbs it entirely**:
  the product is a tree of async agents the user round-robins attention across
  (the same flow as the current wave — type a full reply into one agent's
  channel, hit send, and immediately turn to any of the twenty other agents
  that finished work overnight). There is no synchronous single-agent wait to
  protect: a human is never blocked on *this* agent with nothing else in
  flight. The one genuinely single-agent moment is the initial manager-tree
  bootstrap — which is active by definition, never idle, so it never
  rehydrates. Rehydration latency is therefore not a first-order UX cost at
  all; it is a background storage-tier transition, and cold idle can be
  aggressive. (A marginal further win: begin rehydration when the user *starts
  typing* in a cold agent's channel, hiding the pull behind compose time — a
  small optimization, not load-bearing, since the async model already absorbs
  the cost.)
- **Packing:** with warm-idle sessions costing local storage only and
  cold-idle sessions costing object storage only, box packing is bounded by
  *active* sessions. **The stickiness↔density tension resolves through
  voluntary cold-migration:** because rehydration is cheap in the async model
  (above), a session's box-pin is not load-bearing — when a resume would land
  on a sticky box already at its active bound, the session cold-migrates
  (archive → restore on a box with headroom) instead of forcing a collision,
  paying the box-loss cost deliberately rather than involuntarily. Likewise a
  resized active session spikes into its box-mates' headroom, so the packing
  target and the resize-headroom reserve trade off. E5 measures packing under
  resume collisions and concurrent bursts, not warm-start alone; M0's tier mix
  and working-set sizes calibrate the oversubscription target.

- **Interfaces:** consumes the session-lifecycle plumbing
  (`go/internal/runner/agent_exec.go`, `StartAgent` `:136`), the P2 volume
  reattach + `Archive`/`Restore`, and the existing resume-file
  materialization; produces the two-stage idle-detection policy (warm-idle →
  cold-idle thresholds) + the suspend/archive/resume driver in the Runner, and
  warm-start + rehydration latency metrics on the session status path.
- **Depends:** P2 (the volume survives eviction; its `Archive`/`Restore` back
  cold idle); independent of C3.
- **Test cycle:** warm-idle suspend→resume round-trip preserves session
  continuity (the agent's next turn sees prior context) and a warm `target/`
  (incremental-build probe); cold-idle archive→restore round-trip
  reconstructs the tree + `target/` byte-for-byte from the object store and
  the incremental-build probe still hits warm; a suspend leaks no container
  (engine reconcile via `ContainerRuntime.Exists`) and a cold idle leaves no
  local disk footprint; warm-start and rehydration latency asserted against a
  budget this task produces — D4's own measurement round sets the envelope
  (there is no pre-existing number), and E5's managed-user behavior suite
  reuses it, so the assertion is falsifiable rather than open-ended.

### E5 — end-to-end validation on the hardened path (lane: runner/infra)

Drive a full real session end to end on the hardened runtime: tree
materialized through `VirtualFS` onto the persistent volume, inner-loop ops
in the session environment, at least one heavy op through each elastic
backend (resize and burst), one suspend→resume cycle mid-session, teardown
reclaims compute and preserves the volume. Instrument the tier mix so the
heavy-op fraction is **measured against the M0-calibrated target, not
asserted** as a hard-coded percentage.

- **Interfaces:** consumes everything above; produces a per-session tier-mix
  counter surfaced through the Runner's session status (the
  `AgentSessionStatus` attribution path, `go/internal/runtime/agent.go:52-56`
  comment trail) and the e2e harness.
- **Depends:** S1, P2, C3, D4.
- **Test cycle:** the dogfood-loop e2e extended with the hardened-path
  variant; tier-mix assertion against the M0-calibrated envelope; the
  managed-user-visible behavior suite (session flows, latency envelopes,
  artifacts) green on the hardened path.

## Tasks

Checklist mirroring the plan (owning lane in brackets; order = dependency
order):

- [ ] **M0** [compass-agent] — baseline the tier mix AND the working-set
      size distribution in GB from real dogfood session logs (parallel
      pre-task, no deps).
- [ ] **S1** [infra] — `vfs.VirtualFS` source-of-tree seam
      (checkout | sparse-checkout | volume snapshot | customer-VFS mount) +
      `compute.ComputeRuntime` elastic-compute seam (in-environment
      passthrough backend, reserved streaming variant, fail-closed
      routing-policy shell) + provision wiring + `WorkspaceSource` variant;
      `ContainerRuntime` existing verbs frozen, `Resize` added additively.
- [ ] **I1** [infra] — microVM inter-tenant boundary: microVM OCI runtime
      (krun/libkrun or kata) behind `ContainerRuntime` via podman `--runtime`,
      microVM-bootable rootfs image, guest-netns egress arming, virtio-fs
      volume mount, KVM-absent degrade-to-container path (parallel with
      M0/S1; consumed by C3).
- [ ] **P2** [infra] — persistent session volume: local volume +
      session→box stickiness, stable absolute path, derived-state
      persistence (`target/`/`node_modules`/caches), volume snapshots for
      clone amortization, sccache complement, 14-day default expiry, box-loss
      degradation stated (depends: S1).
- [ ] **C3** [runner/infra] — elastic compute: resize-in-place + same-box
      microVM burst backends behind `ComputeRuntime`, fail-closed
      M0-calibrated routing policy, crash reconciliation for burst
      environments (depends: S1, P2, I1).
- [ ] **D4** [runner/infra] — suspend-idle density: two-stage idle detection
      (warm idle → cold idle), suspend/archive/resume driver over
      `COMPASS_RESUME_SESSION_FILE` + P2 volume reattach (warm) / object-store
      `Archive`/`Restore` (cold), warm-start + rehydration latency metrics
      (depends: P2).
- [ ] **E5** [runner/infra] — full session end to end on the hardened path +
      tier-mix telemetry measured against the M0-calibrated target
      (depends: S1, P2, C3, D4).

## Open Questions

Each tagged **load-bearing** (an executor hits real ambiguity — blocks merge;
the driver asks Matt), **non-load-bearing** (deferred with rationale), or
**resolved** (decided and promoted to a plan task, carrying its Decision in
place of a recommendation). Each pending question carries a recommendation;
the record is drafted against the recommendation as a stated assumption.
Nothing pinned in the Approach is re-opened here.

1. **[non-load-bearing] Elastic-compute backend default — resize-in-place or
   burst?** Both land in C3; which one policy prefers when either would serve
   is a tunable trading box-packing headroom against environment-creation
   latency — no C3 executor is blocked by the default (the backends exist
   either way). **Recommendation:** measure via M0 + C3's integration
   benchmarks before fixing a default; drafted assumption: prefer
   resize-in-place when the box has headroom, burst otherwise. Deferred to
   measurement (same class as the expiry value below).
2. **[load-bearing] Burst storage-sharing mechanism beyond same-box.** P2
   pins local volume + session→box stickiness, making burst same-box (a bind
   mount). If fleet economics later demand cross-box burst, the volume needs
   a network attach (network block storage or a network FS).
   **Recommendation:** stay same-box until E5's measurements show a real
   packing ceiling; then evaluate network block storage as a second volume
   backend behind the same lifecycle API.
3. **[non-load-bearing] Volume expiry value.** Drafted default: 14 days
   after session close, tunable per deployment. Pure cost policy; M0's
   working-set GB distribution calibrates it. Deferred to measurement.
4. **[load-bearing] SEA-1720 streaming-exec seam shape.** `ComputeRuntime`
   reserves a streaming variant (live stdio + kill/wait handle) for
   agent-launched dev servers; the port-exposure and lifecycle wiring are
   SEA-1720's scope. **Recommendation:** freeze the reserved signature in S1
   mirroring `ContainerRuntime.ExecStreaming`
   (`go/internal/runtime/podman.go:299-307`); implement nothing here.
5. **[resolved — decided, now task I1] Inter-tenant isolation boundary =
   microVM.** The session environment and its bursts run model-written,
   ultimately untrusted customer code; rootless containers as the *sole*
   inter-tenant boundary for a hosted multi-tenant platform sit below the
   VM-class posture an external customer will require — and since landing that
   customer *depends on* having the boundary, deferring it behind the customer
   is circular. **Decision:** commit to a microVM inter-tenant boundary as the
   end-state (session and burst), built early (task I1) while the descoping of
   split/VFS frees the capacity. The boundary slots behind `ContainerRuntime`
   as a microVM OCI runtime (krun/libkrun or kata via podman `--runtime`), so
   the seam is expected to hold; the image/boot/egress plumbing above it is
   the real work I1 owns. Through Dogfood + trusted-tenant Beta the rootless
   container remains the running boundary; I1 lands the microVM before the
   first external multi-tenant tenant.
6. **[non-load-bearing] Cold-idle object-store archive — thresholds.** D4
   archives an idle session's volume to the object store past a deeper idle
   threshold (freeing local disk; a cold-idle session costs object storage
   only), rehydrating on resume. Because rehydration is not a first-order UX
   cost in Compass's async-tree model (D4 Resume), cold idle is **default-on
   broadly** — the only open piece is tuning: the warm-idle→cold-idle
   threshold values and the object-store retention window.
   **Recommendation:** ship `Archive`/`Restore` in D4 with cold idle on by
   default (self-hosted single-box may disable it — local disk is already
   paid for); thresholds M0-calibrated from the working-set GB distribution
   and real idle-gap data. Deferred to measurement.
