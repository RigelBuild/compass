# P2 — Persistent Session Volume + VirtualFS Seam

> **Design record.** Detailing pass for task **P2** of
> [compass-elastic-session-runtime](./design.md) (RIG-1717, frozen in PR #446;
> its Plan § P2, design.md:603-653), incorporating the
> [VirtualFS descope amendment](./virtualfs-descope-amendment.md) (RIG-2395,
> ruled by Matt 2026-08-19, PR #459), which moved the `VirtualFS`
> source-of-tree seam, the `WorkspaceSource` variant, and the provision-
> materialize wiring from S1 into P2 and deferred the clone-model /
> clone-credential-location decision to this record
> (virtualfs-descope-amendment.md:57-72,116-129). Where this record and the
> frozen parent disagree, the amendment governs. Every `go/internal/*`
> citation is a path in the **`RigelBuild/compass`** monorepo at main
> `17111cc0` (line numbers drift; resolve against that commit).

Status: PROPOSED — details P2 under the frozen parent + the active amendment;
the central clone/credential fork (OQ-1) is ruled (Matt, 2026-09-05, DL-326).
Tracking: RIG-2395

Ledger impact: **DL-326** (the OQ-1 ruling: agent self-clone + clone-only
snapshot). It is a credential-**location** decision beside DL-052, which speaks
only to the Server's write credential (virtualfs-descope-amendment.md:62-67);
the row landed with Matt's ruling.

## Problem / Intent

A session's working tree and derived state (`target/`, `node_modules`, build
caches) must live on a **per-session persistent volume** that survives
suspend / resume / eviction and mounts at a **stable absolute path** on every
launch — today both die with the container, because the tree exists only
inside it (the agent self-clones post-launch into a container-local dir,
`go/internal/runtime/agent.go:354-358`). P2 builds that volume and its
lifecycle API, plus the `VirtualFS` source-of-tree seam that materializes the
tree onto it (descoped here from S1), and resolves the load-bearing decision
the amendment deferred: **who clones, and where the clone-read credential
lives**.

## Approach

### Code reality this record grounds on

The tree today is wholly container-internal, written by the agent itself:

- `AgentRuntime.Launch` "creates + starts the container, arms egress as root,
  installs scoped credentials, and creates the checkout dir as the agent
  user" (`go/internal/runtime/agent.go:173-177`); the checkout dir is an
  **empty** `mkdir -p` — "creates the in-container checkout directory as the
  agent user, so an agent that self-clones post-launch has an owned working
  dir" (`ensureCheckoutDir`, `go/internal/runtime/agent.go:354-358`).
- The self-clone model is a documented package invariant: "Each agent gets
  its own full git clone, created inside the container — not a shared
  checkout and not a host worktree … The clone's credentials live in the
  agent's $HOME/.gitconfig credential helper, never in the workspace
  .git/config" (`go/internal/runtime/workspace.go:1-8`); `Workspace.CheckoutDir`
  is "the absolute path inside the container where the agent's checkout dir
  is created (the agent self-clones into it post-launch)"
  (`go/internal/runtime/workspace.go:44-47`).
- The agent's clone credential is a per-agent machine-user token installed
  into its scoped `$HOME` via `CredentialSetupScript` — "the token lives in
  the agent's $HOME, never the workspace .git/config"
  (`go/internal/runtime/workspace.go:58-62`) — fed over stdin, never argv
  (`workspace.go:64-66`), the same 0600/stdin discipline as `WriteAgentFile`
  ("the body is fed over stdin, never argv … the file lands 0600 under umask
  077", `go/internal/runtime/agent.go:238-244`).
- The self-clone happens *under* an already-armed default-deny egress
  firewall: `Launch` arms egress before the agent runs, and `NftScript`'s
  base ruleset fails closed — "`set -eu` makes the base ruleset fail closed —
  if any table / set / chain / policy-drop rule fails to install, the script
  aborts non-zero and the caller tears the container down rather than
  running it unfirewalled" (`go/internal/runtime/egress.go:76-83`, the
  script builder at `:87-107`). Any Runner-side clone (Option B below) runs
  *outside* this per-session firewall — a posture difference the fork must
  weigh, not just a credential one.
- Host mounts already carry a per-mount writability bit: `Mount` is
  "a host→container bind mount. ReadOnly maps to :ro" with
  `ReadOnly bool` (`go/internal/runtime/podman.go:60-66`; the `:ro,Z` vs `:Z`
  suffix at `podman.go:846-850`). Only the *doc contract* on
  `AgentSpec.Mounts` says "Mounts is read-only host mounts (e.g. a host cache
  mounted read-only)" (`go/internal/runtime/agent.go:43-44`) — a comment, not
  a shape.
- `SpecBuilder` "maps a provision request to a complete runtime.AgentSpec —
  the image, per-agent workspace, and egress policy"
  (`go/internal/runner/host.go:40-48`) — the one seam where the volume mount
  and `WorkspaceSource` derivation land.
- The resume contract the volume composes with: `COMPASS_RESUME_SESSION_FILE`
  "is the absolute in-container path of a server-reconstructed session file
  the agent loads to resume" (`go/internal/runner/agent_exec.go:40-42`, field
  `:65-67`, exported at `:92-94`). Suspend/resume durability = transcript +
  volume (parent §Spine 4, design.md:123-128).
- `go/internal/vfs` **does not exist** on this checkout (verified by glob —
  no such directory). The descope removed the S1 draft (PR #456 closed,
  virtualfs-descope-amendment.md:144); P2 creates the package fresh.
- The seam style to mirror is the shipped `go/internal/compute` package: a
  doc-commented layering ("compute.go — the ComputeRuntime interface plus the
  value types that cross it … Every consumer depends on the interface, so a
  … backend can replace the S1 one without touching a caller",
  `go/internal/compute/compute.go:10-14`), session-scoped construction ("a
  backend is constructed with the session's container handle, its
  container-runtime engine, and its egress policy … without threading it
  through every Exec call", `compute.go:92-96`), and reserved-not-implemented
  surface returning an honest sentinel (`compute.go:26-29,106-111`; the
  `Resize` precedent, `go/internal/runtime/podman.go:387-396`).

### The volume: a local directory subtree with session→box stickiness

As the parent pins (design.md:609-617): the volume is a **directory subtree
on the box's fast local storage**, NOT network block storage. Vendor-neutral
(Global Constraint 1), no storage fabric, matches today's single-box
deployments; the accepted tradeoff — a burst cannot land on a different box
until a network-volume backend exists — is parent OQ 2 and is not re-opened
here. The lifecycle (create / attach / reattach / snapshot / archive /
restore / expire) is owned Runner-side beside the container lifecycle, in a
new `go/internal/vfs` package.

Concretely:

- **Layout.** A volume root under an operator-configured base dir (default
  under the Runner's state dir), one subtree per session keyed by session id.
  `Attach` bind-mounts it into the container at the **stable absolute
  in-container path** (the same path on every launch and on every burst
  environment — the invariant that keeps `target/` and sccache valid, parent
  design.md:297-299). The in-container path becomes `Workspace.CheckoutDir`'s
  parent; the host side rides `AgentSpec.Mounts` with `ReadOnly: false`. The
  base dir and every per-session subtree are created **by the Runner as its
  own invoking host user**, which the container's `keep-id` rootless remap
  (`--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>`, `podman.go:466-471`)
  maps to the agent uid in-container — so a Runner-created root appears
  agent-owned inside, satisfying `ensureCheckoutDir`'s precondition
  ("CheckoutDir's parent must be writable by the agent uid", `agent.go:356-357`).
  A base dir placed outside the Runner's own ownership (a root-owned `/var`
  path, a differently-privileged installer's dir) breaks every launch on the
  volume path — the invariant is load-bearing, asserted by a W1 test.
- **Stickiness.** While a volume lives, the session (and its C3 bursts)
  relaunch on the volume's box. In the OSS single-box Runner this is
  trivially true (there is one box); the invariant P2 encodes is *box-local*:
  a Runner never attaches a volume it does not host, and `Lookup` of an
  absent volume is a typed error, never a silent recreate. Multi-box placement is
  a control-plane concern outside this repo's scope (OQ-4).
- **Derived state always persists** (Global Constraint 5): `target/`,
  `node_modules`, caches live on the volume because the *whole working
  subtree* is the volume — there is no include-list to maintain. Eviction
  reclaims compute, never the volume; the volume dies only by `Expire`
  policy.
- **Expiry**: default **14 days after session close**, tunable per
  deployment; M0's working-set-GB distribution calibrates the value (parent
  design.md:618-621, OQ 3 — not re-opened). `Expire` reaps only volumes whose
  session is closed AND whose close-stamp is past the deadline; a live or
  suspended session's volume is never eligible.
- **Snapshots for first-clone amortization** (the Codespaces prebuild model,
  parent design.md:622-626): a freshly cloned, **provably-clean** tree
  is snapshotted at the FS layer where the box's filesystem supports
  reflink/snapshot (btrfs/XFS/bcachefs reflink copy), by rsync-clone
  otherwise; a later session on the same repo restores the snapshot and starts
  warm on `git fetch` + checkout-delta instead of a full clone. *What* gets
  snapshotted, *when* (the clean-tree stamp point), and the `(account, repo)`→snapshot
  index the provision path reads are settled by the ruled OQ-1b (clone-only,
  DL-326): a provably-clean post-clone tree, stamped at W5's clone-complete
  signal, keyed in the W2 index.
  Detection of the copy primitive is a runtime capability probe, not a config
  knob (OQ-3). `VolumeSnapshotID` stays an **opaque string** (frozen so by the
  parent, design.md:536-537); its P2 production shape is the snapshot store's
  key, never parsed by callers.
- **sccache is a cross-session complement, not a replacement** — incompatible
  with incremental compilation (`CARGO_INCREMENTAL=0`), never caches linking,
  path-sensitive; a warm `target/` is strictly more; run both (parent
  design.md:627-629). P2 carries no sccache code; the stable-path invariant
  is what keeps it viable.
- **Box loss is an accepted degradation** (vs eviction): losing the box loses
  the volume; the session resumes from the transcript
  (`COMPASS_RESUME_SESSION_FILE`) and re-materializes through `VirtualFS`,
  paying one cold clone + cold build (parent design.md:630-633). `Lookup` of
  a lost volume returns a typed not-found the provision path converts into a
  fresh `CreateVolume` + cold materialize — an error-shaped signal, never a
  silent recreate, so the cold path is observable.

### The `VirtualFS` seam

Created in `go/internal/vfs`, mirroring the `compute` package's shape (doc-
commented file layering, interface + crossing value types in one file,
backends in siblings, session-scoped construction):

- `Materialize(ctx context.Context, src TreeSource) (root string, err error)`
  and `Release(ctx context.Context, root string) error`.
- `TreeSource{Repo string, Ref string, Sparse []string, Snapshot VolumeSnapshotID, CustomerMount string}`
  selects checkout | sparse-checkout | volume snapshot | mounted customer VFS
  (sparse-checkout is a parameter of the checkout backend, not a second
  backend — parent design.md:516-518).
- **The destination is binding state of the `VirtualFS` instance** —
  constructed with the target root, which at P2 is the session's attached
  volume — not a `Materialize` parameter (parent design.md:533-536), so a
  later destination (a customer-VFS interop root) swaps behind the frozen
  signature.
- `Release` detaches/cleans the materialized root without destroying volume
  contents (volume destruction is `Expire`'s, never `Release`'s).

Under the ruled clone model (Option A, DL-326), the P2 checkout backend's
`Materialize` **prepares** the destination — snapshot-restore when
`TreeSource.Snapshot` is set, else an empty owned root — and the agent
completes the tree (clone or fetch-delta) in-container. The signature was
designed decision-proof: Option B would have kept it while performing the full
host-side clone in the backend body — which is why the amendment let the seam
wait for P2.

### The clone/credential fork — the record's central decision (OQ-1)

The amendment defers to P2 "who clones, and where the clone (read)
credential lives" (virtualfs-descope-amendment.md:66-72). DL-052 governs the
**write** credential only: "Only the Server holds forge write credentials, as
a `server_only` declared secret filtered out of container injection … The
agent keeps a push-scoped git credential" (`docs/designs/DECISIONS.md:83`;
the parent cites it as `docs/designs/product/DECISIONS.md:75` — the file
lives at `docs/designs/DECISIONS.md` on this checkout). The decision here
must be *consistent with* that posture, not governed by it.

**Option A — keep the agent self-clone (recommended).** The Runner
creates/attaches the volume and mounts it writable at the stable path;
`Materialize` prepares it (empty, or snapshot-restored host-side); the agent
clones/fetches into it with the machine-user token already in its scoped
`$HOME` (`workspace.go:58-62`), exactly today's model
(`ensureCheckoutDir`, `agent.go:354-358`) with the destination moved from a
container-local dir onto the volume.

- *Credential blast radius: zero delta.* No new credential class; no forge
  token ever exists host-side; the DL-052 split (Server = write,
  agent = push-scoped in-container) is untouched.
- *Global Constraint 8: trivially green.* The launch path changes only in
  where the checkout dir lives (a mount + a `CheckoutDir` value); the clone
  mechanics, credential install, and egress arming are byte-identical.
- *Snapshot amortization needs a clean stamp point (OQ-1b).* The volume is a
  host directory — host-visible regardless of who wrote it — so the Runner
  *can* snapshot it even though the clone happened in-container. But under A
  there is no automatic clean snapshot point: `Materialize` returns before any
  tree bytes exist (the agent clones asynchronously post-launch,
  `agent.go:354-358`), so a naive "snapshot after materialize" captures an
  empty root, and a "snapshot at session close" captures that session's
  uncommitted WIP and untracked non-ignored files — which a `git fetch` +
  checkout-delta restore does **not** remove, leaking one session's stray
  files into a stranger's tree (the very isolation `workspace.go:1-8` exists to
  hold, file contents in place of tokens). So A's amortization is sound only
  with a designed provenance mechanism: the recommended one snapshots a
  **provably-clean** post-clone tree (an agent→Runner clone-complete signal
  plus a `git status`-clean check), keyed in an `(account, repo)`→snapshot index (W2) the
  provision path reads. This is the load-bearing sub-fork **OQ-1b**; it rides
  A's ruling because "A amortizes fine" is load-bearing in A's own
  cost/benefit.
- *Cost:* `Materialize` on the fresh-clone path is preparation, not
  tree-writing — a deliberate, documented reading of Global Constraint 2
  ("every working-tree materialization goes through `VirtualFS`",
  design.md:423-428): the seam owns the destination and the source-selection;
  the tree bytes on the cold path are written by the agent under the seam's
  contract. Recorded in OQ-1 so Matt ratifies the reading, not just the
  option.

**Option B — move cloning Runner-side.** `Materialize` performs a full
host-side clone onto the volume before container start; the container gets a
ready tree.

- *Gain:* `VirtualFS` becomes the literal materializer (Global Constraint 2
  reads plainly); the prebuild model gets a host-side materialization point
  that can snapshot *before* any container exists (a true Codespaces
  prebuild service — build the snapshot with no session attached); cold
  start-up drops one in-container step.
- *Cost:* introduces a **host-side forge read credential** — a new credential
  class living on every Runner box, distinct from both DL-052's Server-only
  write secret and the in-container agent token. It needs provisioning,
  rotation, scoping (read-only, but to *which* repos?), and it widens the
  host's blast radius: a Runner-host compromise today yields no forge
  credential; under B it yields org-wide read. It also perturbs the launch
  path ordering (clone before create/start) against Global Constraint 8, and
  the private-mirror/file:// cases ("a file:// clone of a local mirror needs
  none", `workspace.go:68-69`) need re-plumbing host-side. And the clone
  itself moves outside the session's fail-closed egress firewall
  (`egress.go:76-83`) into the Runner host's own network posture — Global
  Constraint 4's "egress … session-scoped" gets a host-side carve-out to
  justify.

**Recommendation: Option A.** The one capability B uniquely adds —
sessionless prebuild snapshots — is not needed by any P2/C3/D4/E5 task (the
snapshot consumers are all sessions, and A amortizes their cross-session
snapshots via OQ-1b's clean-stamp mechanism), and B's price is a standing new
credential surface plus launch-path churn. A is reversible: if a prebuild
*service* ever materializes trees with no session, that is the moment a
host-side read credential earns its existence, and the `Materialize` signature
already accommodates it. The whole fork was **OQ-1, load-bearing** — its clone
posture (1a) and snapshot provenance (1b), ratified together by Matt as
**Option A + clone-only** (DL-326, §OQ-1); this record now builds to that
ruling.

### `WorkspaceSource` and the provision wiring

`AgentSpec`/`Workspace` gain the variant the parent reserved
(design.md:246-251): a `WorkspaceSource` discriminating today's
container-local clone-dir from the volume-backed workspace, derived by
`SpecBuilder` (`go/internal/runner/host.go:46-48`) from the provision
request. The clone-based path stays intact and default (Global Constraint 8);
volume-backed is opt-in per deployment until E5 validates it. The provision
flow under volume-backed: resolve-or-create volume → `Attach` → `Materialize`
(snapshot or prepare-empty) → append the writable mount to `AgentSpec.Mounts`
→ `Launch` unchanged. Teardown stops/removes the container
(`AgentRuntime.Teardown`, `agent.go:216-236`) and never touches the volume.

### Mounts doc-contract amendment

`AgentSpec.Mounts`'s comment — "Mounts is read-only host mounts"
(`agent.go:43-44`) — is amended to "host mounts (read-only caches, and the
writable session volume at P2)". This is a **doc-comment change, not a shape
change**: `Mount.ReadOnly` is already a per-mount bool
(`podman.go:62-66`) and the `:ro` suffix is already conditional
(`podman.go:846-850`). The downstream `ContainerSpec.Mounts` already documents
a **read-write** bind mount in the shipped tree — "Not all read-only … the
per-container agent gateway socket is mounted read-write (the agent must
connect() to it)" (`podman.go:100-103`) — so a writable mount at the layer the
volume rides is existing precedent, not a new contract. Recorded as a
P2-specific Global Constraint so no reviewer treats the writable mount as a
contract violation.

## Alternatives considered

- **Network block storage as the P2 volume backend.** Rejected by the parent
  (design.md:609-617, OQ 2): not vendor-neutral without a storage fabric,
  unneeded for same-box burst. Not re-opened; the lifecycle API is shaped so
  a network backend can slot behind it later.
- **Whole-`$HOME` on the volume.** The scoped `$HOME` (credentials,
  `.gitconfig`, agent config) stays container-local and is reinstalled by
  `Launch`, per the parent's D4 resume analysis ("the scoped `$HOME` is
  container-local, reinstalled by `Launch`, not on the volume",
  design.md:730-735). Persisting it would put a live forge token on the
  host's disk — worse credential posture for zero P2 benefit.
- **Content-addressed snapshot store / dedup across sessions.** Rejected with
  the parent's content-addressed-VFS alternative (design.md:387-401);
  snapshots here are dumb FS-level copies keyed by opaque id.
- **Ruling the clone fork inside this record.** Rejected — it is exactly the
  decision the amendment marked load-bearing for Matt
  (virtualfs-descope-amendment.md:123-129); this record recommended and
  sequenced around it (OQ-1) rather than deciding it — Matt ruled it on
  2026-09-05 (DL-326, §OQ-1), which this record now reflects.
- **Option C — a host-side read-only bare mirror + in-container `file://`
  clone.** The codebase names this pattern: `mountArg`'s doc calls the
  read-only mount "the shared bare-repo cache" (`podman.go:842-844`),
  `AgentSpec.Mounts`'s doc example is "a host cache mounted read-only"
  (`agent.go:43-44`), and `CredentialSetupScript` already returns `("", nil)`
  for "a file:// clone of a local mirror" (`workspace.go:68-69`) — so the agent
  clones with **no credential**. Rejected as non-dominant: someone must
  populate and refresh the mirror from the forge, which either puts the same
  host-side forge READ credential Option B needs on the box (merely narrowed to
  fetch-into-mirror — a real narrowing, not credential-free) or lets agents
  write a shared host cache other sessions read (a cross-session write channel
  where one compromised agent poisons objects a stranger clones — worse than A
  or B, and against `workspace.go:1-8`'s one-agent-one-clone isolation). C thus
  collapses to "B with a smaller blast radius." Recorded because a reviewer or
  executor will see the mirror pattern in the code and ask, and because the
  fetch-scoped-vs-clone-scoped credential narrowing was a genuine input to
  Matt's OQ-1 ruling (ruled Option A, DL-326).

## Global Constraints

**Inherited: all nine of the parent's** (design.md:416-467) apply unmodified
— vendor neutrality (1), go-through-the-seams (2, a P2-onward constraint per
the amendment, virtualfs-descope-amendment.md:111-114), fail-closed routing
(3), security floors in every environment (4), derived state always persists
(5), runtime-agnostic substrate (6), sequencing after Dogfood (7), existing
session path stays green (8), version floors (9). P2-specific additions:

- **P2-GC-a — Mounts doc amendment.** The writable session volume rides
  `AgentSpec.Mounts` with `ReadOnly: false`; P2 updates the field's doc
  comment (`agent.go:43-44`), never the `Mount` shape (`podman.go:62-66`). The
  read-write agent-gateway-socket mount (`podman.go:100-103`) is the existing
  read-write-bind-mount precedent at this layer.
- **P2-GC-b — no new credential surface.** The ruled Option A + clone-only
  (DL-326) introduces no host-side forge credential — the agent self-clones
  in-container with its existing `$HOME` token — and no P2 task may introduce
  one. This binds the ruled posture; the DL-326 revisit trigger — a sessionless
  prebuild *service* that materializes trees with no session — would re-open
  this constraint under its own decision (Option B/C earns its host-side read
  credential only there).
- **P2-GC-c — volume destruction only via `Expire`.** `Release`, `Teardown`,
  eviction, crash, and failed launches never delete volume contents; the only
  reclaim path is the policy reaper (the parent's GC 5 made mechanical).
- **P2-GC-d — stable-path invariant.** The in-container mount path of a
  session's volume is identical across every launch, resume, and burst
  environment of that session; a path change is a breaking bug (it
  invalidates `target/` and sccache).
- **P2-GC-e — clone-based path stays the default.** Volume-backed
  `WorkspaceSource` is opt-in until E5; the existing container-local
  self-clone path keeps its regression suite green at every increment
  (GC 8 made concrete for P2).
- **P2-GC-f — snapshots never cross an account boundary.** The repo→snapshot
  index is keyed by `(AgentAccountID, repo)` (`agent.go:52-56`) and a restore
  is never served across accounts: a snapshot produced by one account's
  session is invisible to another account's session of the same repo URL.
  Cleanliness is not authorization — a provably-clean tree of a private repo
  is still that repo's content — so the tenancy boundary the parent puts at
  "the session sandbox vs other tenants and the host" (design.md:258-259) is
  held in the index key, not left to the post-restore `git fetch`.

## Plan

Six implementation tasks (W1–W6). W1 is foundational. **OQ-1 is ruled Option A
with a clone-only snapshot (DL-326, §OQ-1)**, so W3's checkout-backend
semantics, W5's wiring plus its clone-complete-signal sub-unit, W2's
snapshot-store/index leg, and W6's snapshot-materialized acceptance probe
(probe 3) all **build to that contract**: the agent self-clones in-container
(no host-side forge credential), and the snapshot carries a provably-clean
post-clone tree only (no cross-session build state). The record was drafted
against this posture, so the ruling settled the contract without reshaping any
task. Hermetic unless noted.

### W1 — volume lifecycle API + local-dir backend (`go/internal/vfs`)

The package skeleton mirrors `go/internal/compute`'s layering
(`compute.go:10-24`): `vfs.go` holds the interfaces + crossing value types,
`localvolume.go` the local-dir backend, `snapshot_*.go` the W2 backends.

- **Interfaces:**

  ```go
  package vfs

  // Volume is a live per-session persistent volume: the session it belongs
  // to and its host-side root. Opaque to callers beyond these fields.
  type Volume struct {
      SessionID string
      HostRoot  string
  }

  // VolumeSnapshotID is the opaque key of a stored volume snapshot (frozen
  // opaque by the parent record; never parsed by callers).
  type VolumeSnapshotID string

  // ArchiveRef is the opaque reference to an archived volume in the
  // object store (consumed by D4's cold-idle; signatures frozen here,
  // implementation deferred — see OQ-2).
  type ArchiveRef string

  // VolumeManager owns the session-volume lifecycle Runner-side, beside the
  // container lifecycle. Constructed with the operator-configured base dir.
  type VolumeManager interface {
      CreateVolume(ctx context.Context, sessionID string) (Volume, error)
      // Lookup resolves a session's existing volume (with its HostRoot) or
      // returns ErrVolumeNotFound. It is the "resolve" half of the provision
      // path's resolve-or-create: Attach needs a resolved Volume, so a caller
      // cannot produce one from a bare session id without this verb.
      Lookup(ctx context.Context, sessionID string) (Volume, error)
      // Attach makes the resolved volume available for mounting and returns
      // its host path; it also atomically clears any close-stamp.
      Attach(ctx context.Context, v Volume) (path string, err error)
      Snapshot(ctx context.Context, v Volume) (VolumeSnapshotID, error)
      Archive(ctx context.Context, v Volume) (ArchiveRef, error)
      Restore(ctx context.Context, ref ArchiveRef) (Volume, error)
      // Expire reaps volumes whose session is closed and whose close-stamp
      // is older than olderThan. Never touches live/suspended sessions.
      Expire(ctx context.Context, olderThan time.Duration) error
  }
  ```

  `Archive`/`Restore` are **reserved-not-implemented** at P2 (honest
  sentinel, the `ErrExecStreamingNotImplemented` discipline,
  `compute.go:26-29`; see OQ-2). **Close-stamp mechanism** (the leak the
  parent's 14-day policy exists to bound, design.md:619-621, made robust): a
  marker file in the volume root's metadata dir, written by the teardown path
  and read by `Expire`, with three invariants the W1 backend holds —
  (a) **`Attach` atomically clears the stamp**, so a reopened
  closed-but-unexpired session never carries a past-deadline stamp into its
  new life; (b) **`Expire` takes a per-volume lock and re-verifies eligibility
  under it**, so a volume is never reaped in the window between the reaper
  reading its stamp and a concurrent `Attach`; (c) the stamp carries
  **close-vs-suspend intent** (a suspended session is *not* eligible however
  old — D4's suspend uses the same stop+remove teardown path,
  design.md:708-709, so the intent bit comes from the caller, not inferred
  from "container gone"). A crash between container-remove and stamp-write
  leaves an **unstamped** closed volume; the startup pass **stamps every
  unstamped volume closed at discovery time** — a discovered-orphan stamp
  whose deadline runs from discovery, not from the lost close — and invariant
  (a) undoes it for free if that session is re-provisioned before the
  deadline. This needs **no Server query**, and by design cannot want one:
  RunnerService exposes no session-query verb and `SessionsResponse` carries
  no session-set variant (its `command` oneof is exhaustive,
  `runner.proto:233-284`), while the Runner — not the Server — is
  authoritative for live session truth (OQ6,
  `go/internal/runner/host.go:3-7`, `go/internal/runner/dispatch.go:10-11`)
  and `Hub.enroll` clears the Server's session bindings at every enroll
  (`hub.go:933-935`), so the Server's live-session map is *empty* exactly when
  a restart would consult it. The in-memory `sessions` map (`host.go:77`) is
  rebuilt empty on restart and is not authoritative either. So a crash fails
  *safe* (reaped one full expiry window after discovery) not *open* (the
  volume is always stamped, so `Expire` can always reach it) and never *wrong*
  (a live or resuming session re-attaches and clears the stamp on its next
  launch, and a suspended session that never crashed was stamped *suspended*
  by its normal teardown, untouched by this pass).
- **Depends:** nothing (first P2 code task).
- **Test cycle:** hermetic over tempdirs — create/attach round-trip returns
  a stable path; `Lookup` round-trips the resolved volume and returns the
  typed not-found for an absent session; re-attach after simulated Runner
  restart returns the same path; the mounted root is **writable by the agent
  uid** (the keep-id ownership invariant, Layout); **attach clears a
  past-deadline stamp**; `Expire` reaps only closed-past-deadline volumes
  (live, suspended, recently-closed, and reopened all survive); a
  **crash-orphaned unstamped** volume is stamped at discovery, survives to
  its discovery-based deadline, is cleared by a subsequent `Attach`, and is
  reaped only when the deadline passes with no re-attach;
  `Archive`/`Restore` return the honest sentinel.

### W2 — snapshot backends: reflink with rsync fallback

- **Volume-copy primitive.** An unexported `cloner` seam with two
  implementations — reflink copy (`cp --reflink=always`-class, FS-supporting)
  and rsync-clone — chosen by a **runtime capability probe** (attempt a reflink
  of a probe file in the base dir at manager construction; cache the verdict;
  no config knob, OQ-3). Useful regardless (it is also D4's
  archive/restore copy path).
- **Snapshot store + index.** A sibling subtree under the base
  dir keyed by `VolumeSnapshotID`, plus an **`(AgentAccountID, repo)`→snapshot
  index** the provision path reads to set `TreeSource.Snapshot` for a new
  session of an already-seen `(account, repo)`. The account scope is
  load-bearing, not cosmetic: repo URL alone would restore one tenant's
  snapshot into another tenant's volume (P2-GC-f, `agent.go:52-56`). The warm
  path is unbuildable without this lookup — it is W2's, not left unowned.
  **Retention: one current snapshot per `(account, repo)`**, replaced
  atomically on a newer clean snapshot (the parent's own
  unbounded-multi-GB-liability argument, design.md:619-621, applies to the
  snapshot subtree exactly as to volumes); a superseded snapshot is unlinked
  only after the replacement commits. What counts as a snapshot-worthy (clean)
  source, and when it is taken, is the clone-only ruling (DL-326; the trigger
  is W5's clone-complete sub-unit).
- **Depends:** W1. The snapshot-store/index leg builds (clone-only, DL-326).
- **Test cycle:** hermetic on the rsync path (any FS); the reflink path
  needs a reflink-capable FS — CI job pinned to one, plus the probe's
  fallback asserted on a non-capable FS (tmpfs). Snapshot→restore
  round-trips byte-identical trees; restore into a fresh volume leaves the
  source snapshot immutable; the index returns the current snapshot for a seen
  `(account, repo)` and **nothing for the same repo under a different account**
  (P2-GC-f) or an unseen repo; taking a newer snapshot atomically supersedes
  the prior one (old key gone, one current key per `(account, repo)`).

### W3 — `VirtualFS` seam + backends *(Option A, DL-326)*

- **Interfaces:**

  ```go
  // TreeSource selects where a session's tree comes from.
  type TreeSource struct {
      Repo         string           // forge clone URL (empty with Snapshot/CustomerMount)
      Ref          string           // branch/commit to check out
      Sparse       []string         // non-empty => git sparse-checkout paths
      Snapshot     VolumeSnapshotID // non-empty => restore this snapshot
      CustomerMount string          // non-empty => interop: tree pre-mounted here
  }

  // VirtualFS materializes a working tree onto its destination. The
  // destination is BINDING STATE of the instance (constructed with the
  // target root — the session's attached volume at P2), never a
  // Materialize parameter, so a later destination swaps behind this
  // signature (parent design.md:533-536).
  type VirtualFS interface {
      Materialize(ctx context.Context, src TreeSource) (root string, err error)
      Release(ctx context.Context, root string) error
  }
  ```

  Under the ruled Option A (DL-326) the checkout backend's `Materialize`
  prepares the destination (snapshot-restore via W2, else an empty owned root)
  and records the expected `Repo`/`Ref`; the agent writes the tree bytes
  in-container. The seam, `TreeSource`, and the tests' shape were designed
  decision-proof — Option B would only have swapped the checkout backend's body
  (adding a host-side clone/fetch) and W5's launch-order, never the interface.
  **`Materialize`'s post-condition under A is "destination prepared, tree
  completed in-container by the agent"** — so the interface doc-comment states
  that post-condition explicitly (an empty-dir return must not surprise a
  future caller: C3 burst wiring, customer-VFS interop).
- **Depends:** W1, W2. The backend body encodes Option A (DL-326).
- **Test cycle:** contract tests a fake and the real backend both pass
  (the S1 seam discipline, design.md:561-565); snapshot-source materialize
  restores the tree; customer-mount source validates and passes through;
  `Release` never deletes volume contents (P2-GC-c asserted).

### W4 — `WorkspaceSource` variant + Mounts doc amendment

- **Interfaces:** `runtime.Workspace` gains
  `Source WorkspaceSource` with
  `type WorkspaceSource int` / `const (SourceCloneDir WorkspaceSource = iota; SourceVolume)`
  — zero value = today's clone-dir path, so an un-migrated caller is
  byte-identical (GC 8). Under `SourceVolume`, `ensureCheckoutDir` still
  runs (idempotent `mkdir -p` on the mounted path, same uid-ownership
  intent, `agent.go:354-358`). The `AgentSpec.Mounts` doc comment is amended
  per P2-GC-a. No `ContainerRuntime` change (the interface stays frozen,
  `podman.go:399-403`).
- **Depends:** W1 (the mount it documents); parallel with W3.
- **Test cycle:** existing launch-path regression suite green with zero-value
  `Source`; a `SourceVolume` spec produces the writable mount + stable
  in-container path; `Mount.ReadOnly=false` renders without `:ro`
  (`podman.go:846-850`).

### W5 — provision wiring + teardown/reprovision *(Option A, DL-326)*

- **Interfaces:** `SpecBuilder` (`go/internal/runner/host.go:46-48`) derives
  `WorkspaceSource` + the volume mount from the provision request, and is the
  **single author of the P2-GC-d path triple** — the `VirtualFS` instance's
  bound root (host side), `Workspace.CheckoutDir` (in-container,
  `workspace.go:44-47`), and `Mount.ContainerPath` (`podman.go:62-66`) are all
  derived from one value in `SpecBuilder`, never wired independently by a
  caller (independent wiring is how the P2-GC-d path-drift breaking bug is
  born). The provision path composes: resolve-or-create (`Lookup`, then
  `CreateVolume` iff `ErrVolumeNotFound`) → `Attach` (which clears any
  close-stamp) → `Materialize` → `Launch`. Teardown composes the reverse:
  `Teardown` the container (`agent.go:216-236`) → `Release` the materialized
  root (never deleting volume contents, P2-GC-c) → write the close-stamp
  **carrying the caller's close-vs-suspend intent** (W1); a suspend teardown stamps
  *suspended* and stays ineligible for `Expire`. Reprovision of a
  closed-but-unexpired session re-attaches and re-materializes warm. Box loss:
  `Lookup`'s typed not-found routes to the cold path (create + cold
  materialize), logged as a capability event, never an error to the user.
- **Clone-complete signal → snapshot (sub-unit).** Under
  provenance-(a) the snapshot is taken from a **provably-clean post-clone
  tree**, so the trigger is owned here, not left implicit: (i) the agent
  (`packages/compass-agent`, TypeScript) emits a **clone-complete** signal to
  the Runner over a **new unary `AgentGateway` RPC** — delivered-or-erred, the
  `PostConversationFrame` precedent (`agent_gateway.proto:68-73`), *not* the
  loss-tolerable `Publish` spine: the trigger fires exactly once per clone and
  cannot be reconstructed, so a drop on `Publish` would silently kill the warm
  path for that `(account, repo)` with no retry, whereas a unary drop is an
  agent-retried error; (ii) a Runner-side handler verifies a **host-side**
  `git status`-clean tree at the volume root (viable under the keep-id
  ownership invariant, Layout) at the expected `Ref`, then calls
  `VolumeManager.Snapshot` (W1) → writes the `(AgentAccountID, repo)`
  index entry (W2, P2-GC-f). This is the **only caller of `Snapshot`** in P2 —
  without it the verb is dead and the warm path never triggers. This sub-unit
  builds under the clone-only ruling (DL-326); it would have dropped only under
  a "descope snapshots" ruling, which Matt did not take.
- **Depends:** W1, W2, W3, W4. Encodes Option A + clone-only (DL-326):
  materialize-then-launch order, and the clone-complete sub-unit builds.
- **Test cycle:** integration — provision→session→teardown→reprovision
  round-trip on the volume path, extended to the parent's full
  provision→materialize→session→**release** round-trip (design.md:561-562);
  W5 owns at merge: the volume survives teardown, the mount path is stable
  across the reattach, and a suspend-stamped volume is `Expire`-ineligible.
  W5 and W6's snapshot-materialized probe (probe 3) land together (Option A,
  DL-326); the five-probe acceptance suite (the parent's end-to-end P2
  cycle) is W6's.

### W6 — expiry reaper wiring + the P2 acceptance suite

- **Interfaces:** a periodic `Expire` driver in the Runner (ticker + startup
  pass, the reconciliation idiom); config surface for the 14-day default.
  Plus the parent-mandated acceptance suite (design.md:649-653):
  1. teardown-then-reprovision keeps `target/` warm — asserted by a
     **rebuild-freshness probe**, not a timer: the second build recompiles
     nothing (e.g. `cargo build` emits no `Compiling <probe-crate>` line /
     fingerprints unchanged), valid because P2-GC-d's stable path plus the
     same toolchain image keeps fingerprints comparable. A wall-clock
     threshold is the flaky version an executor must *not* write;
  2. stable-path invariant across reattach (P2-GC-d);
  3. a snapshot-materialized session skips the clone — asserted by
     **git-invocation shape**, not traffic volume: the warm path runs
     `fetch` + `checkout`, never `clone` (assert the command shape, or
     object-count against the local `file://` mirror's refs);
  4. expiry reaps only closed-session volumes past the deadline, and never a
     reopened, suspended-past-deadline, or crash-orphaned-then-reconciled
     volume before its true eligibility (the W1 close-stamp invariants);
  5. simulated box loss (delete the volume out from under a suspended
     session) resumes cold without error.
- **Depends:** W1–W5.
- **Test cycle:** the five probes above; 1–4 hermetic (local FS + a local
  git mirror; `file://` clones need no credential, `workspace.go:68-69`);
  5 hermetic (volume deletion is simulable). The rebuild-freshness probe
  needs a real toolchain in the test image — CI-heavy but not
  hardware-gated. W2's reflink leg is the only FS-hardware-sensitive test
  in P2.

## Tasks

- [ ] **W1** — `go/internal/vfs` volume lifecycle API
      (`CreateVolume`/`Lookup`/`Attach`/`Snapshot`/`Archive`/`Restore`/`Expire`) +
      local-dir backend; `Archive`/`Restore` reserved-not-implemented
      (OQ-2). Hermetic.
- [ ] **W2** — snapshot backends: reflink probe + rsync fallback, snapshot
      store + `(AgentAccountID, repo)`→snapshot index (never cross-account,
      P2-GC-f), one-current-snapshot-per-`(account, repo)` retention, restore
      path (depends: W1; snapshot-store/index leg builds the clone-only snapshot
      per DL-326; reflink CI leg FS-pinned).
- [ ] **W3** — `VirtualFS` seam + checkout/snapshot/customer-mount backends
      (depends: W1, W2; backend body encodes Option A per DL-326).
- [ ] **W4** — `WorkspaceSource` variant + `AgentSpec.Mounts` doc amendment
      + mount rendering (depends: W1; parallel with W3).
- [ ] **W5** — provision wiring: resolve-or-create → attach (clears stamp) →
      materialize → launch; `SpecBuilder` owns the P2-GC-d path triple;
      teardown (container → release → close-stamp with close-vs-suspend
      intent); agent clone-complete signal → host-side git-status-clean verify
      → `Snapshot` → `(account, repo)` index write (clone-only snapshot, DL-326);
      box-loss cold path (depends: W1, W2, W3, W4; encodes Option A per DL-326).
- [ ] **W6** — expiry reaper driver + the five-probe P2 acceptance suite
      (rebuild-freshness + git-shape probes, not timers) (depends: W1–W5;
      probe 3 asserts the clone-only warm path, DL-326).

## Open Questions

Each tagged **load-bearing** (blocked the gated tasks' merge) or
**non-load-bearing** (deferred with rationale). OQ-1 (the one load-bearing
question) is ruled (DL-326) and is a contract; the non-load-bearing OQ-2..OQ-4
stay recommendations the record is drafted against as stated assumptions.

> Namespace: an unprefixed **OQ-N** refers to *this* record's open questions;
> the parent record's are always written **parent OQ N** (space, no hyphen).

### OQ-1 — Clone model + snapshot provenance *(RULED 2026-09-05, Matt)*

**Ruling: Option A (agent self-clone) + provenance-(a) (clone-only snapshot).**
Both coupled sub-decisions were ratified together as recommended; the record
was drafted against this posture, so the ruling changes no mechanism — it
un-gates the tasks and fixes the drafted-against assumption as the contract.
Ledgered as **DL-326**.

**1a — Clone model + clone-read-credential location: A.** The agent keeps its
self-clone: the Runner prepares/attaches the volume (empty or
snapshot-restored), the agent clones/fetches in-container with its existing
`$HOME` machine-user token (`workspace.go:58-62`, `agent.go:354-358`) — no new
credential class, GC 8 trivially green. Ratifying A also ratifies the record's
reading of Global Constraint 2 (design.md:423-428): the `VirtualFS` seam owns
the destination and source-selection, and the cold path's tree bytes are
written by the agent under the seam's contract — `Materialize` on the
fresh-clone path is preparation, not tree-writing. **Revisit trigger (Matt):**
two separate follow-ups, not one — box-global read-only clones of subscribed
repos on the Runner side are a deferred follow-up optimization (not needed
early), and *separately* a host-side read credential (Option B/C) earns its
existence only when a sessionless prebuild *service* materializes trees with no
session; the `Materialize` signature already accommodates that flip, so A is
reversible by design.

**1b — Snapshot-amortization provenance under A: clone-only (provenance-(a)).**
The snapshot carries a **provably-clean post-clone tree** and nothing else —
the agent signals clone-complete to the Runner (W5's clone-complete sub-unit),
the Runner verifies a `git status`-clean tree at the ref, then snapshots,
keyed in an `(account, repo)`→snapshot index (W2, P2-GC-f) the provision path
reads. A new session of a seen `(account, repo)` skips the cold **clone** (a
`git fetch` + checkout-delta restore) and then builds cold on its own volume.
This carries **zero cross-session leak**: the snapshot holds no WIP and no
other session's build artifacts, and the account scope in the index key — not
the tree's cleanliness — holds the tenancy boundary (P2-GC-f: cleanliness is
not authorization). Cross-session **build** prebuild (a warm `target/` shared
across sessions) is explicitly
**out of P2 scope** — it is the part that carries the leak vector and needs a
provenance story clone-only does not, and each session already keeps its own
warm `target/` across suspend/resume via its persistent volume. Build-prebuild
rides the same future prebuild-service moment as the 1a revisit trigger.

The parenthetical *(provenance-(a))* is the pre-ruling option label for this
clean-post-clone-tree snapshot; the alternative close-time approach — an
ignore-aware `git clean -fd` + `git reset --hard` — was rejected because it
still shares any *ignored* file (e.g. a gitignored `.env`) a session left
behind, so it does not close the leak.

### OQ-2 — `Archive`/`Restore` implementation timing *(non-load-bearing)*

The parent puts the verbs on P2's API but their consumer is D4's cold-idle
(design.md:638-641,714-723). **Recommendation:** freeze the signatures in
W1 with honest not-implemented sentinels (the `Resize`/`ExecStreaming`
discipline, `podman.go:387-396`, `compute.go:26-29`); the object-store
backend and endpoint config land with D4, which owns the archive
thresholds anyway. No P2 executor is blocked.

### OQ-3 — Snapshot FS-capability detection *(non-load-bearing)*

**Recommendation:** a runtime probe at `VolumeManager` construction (attempt a
reflink copy of a probe file in the base dir; cache the verdict; fall back to
rsync) — no operator knob until a deployment demonstrates the probe
mis-detecting. Pure mechanism; W2 owns it.

### OQ-4 — Session→box stickiness vs the scheduler *(non-load-bearing — out of scope)*

Multi-box placement (which box a resuming session lands on, collision
handling) is a control-plane concern outside this repo; the parent's D4
already designs the voluntary cold-migration relief valve
(design.md:759-770). P2 encodes only the box-local invariant: a Runner
attaches only volumes it hosts, and an absent volume is a typed error
routed to the cold path. Nothing in `go/internal/vfs` assumes or names
any particular placement layer.
