# Compass agent config-delivery seam — skills / extensions / MCP-server configs / env-vars

Status: Draft

> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../compass-0.5/design.md:10-12`, restated
> `../compass-0.6/design.md:1116-1118`). Tracked as SEA-1568.
>
> **New named Decisions this record introduces (ledger rows DL-078..DL-081,
> landed in DECISIONS.md in this same PR):**
>
> - **CD-1** — Agent config (skills / extensions / MCP-server configs) is
>   declared into a Server-side **fleet-wide** bundle store — one bundle
>   every agent gets — via new operator-scoped `CompassService` RPCs, never
>   pushed as fields on `ProvisionAgentWorkspaceRequest`; persona/role-keyed
>   bundles are the named post-MVP seam.
> - **CD-2** — Carriage is Runner-fetch: a **server-streaming**
>   `FetchAgentConfig` RPC on the Runner-dialed `RunnerService`, plus a
>   `ConfigVersion` signal on the `Sessions` response stream —
>   signal-then-pull over the frozen dial-out inversion, the exact pattern
>   SEA-1327's `FetchSecrets`/`SecretsVersion` set; the inversion gains no
>   inbound route.
> - **CD-3** — Injection for skills/extensions/MCP-configs is a Runner-local,
>   Runner-materialized **read-only bind mount** of the **parent** config dir
>   at `/run/compass/agent-config` (the agent reads through `current/`),
>   realizing the v0.6 D11 mount spine; the MVP **forbids credentials in MCP
>   configs** (SEA-1576 tracks the post-MVP credentialed path); env-vars are
>   delivered **exclusively** via SEA-1327's
>   SecretMaterializer/`FetchSecrets` surface — this record builds no second
>   env channel.
> - **CD-4** — The MVP update path is re-materialize + **in-place agent
>   Reload** (the exec-driven model), reserving the throwaway-container
>   restart for image changes; live structured injection into the running
>   agent (the parked `ConfigControl`, SEA-1310) is the named future seam,
>   referenced and left unbuilt.

## Problem / Intent

A user must be able to give the agent fleet its skills, extensions,
MCP-server configs, and env-vars, and Compass must deliver them into each
agent's isolated podman container — today no injection seam exists for any
of the four, so an agent cannot run a real wave (no skills, no MCP tooling,
no env). This record designs that seam end to end for the MVP's
**fleet-wide** scope — one config bundle that every agent gets:
declaration, carriage over the frozen Server↔Runner inversion, per-type
injection into the container, and the update path. Per-agent
differentiation is deliberately out of the MVP; post-MVP, config keys on a
persona / agent-level role, never an individual agent id. Env-vars reuse
SEA-1327's secret surface; the new machinery here covers only the bulk
config content (skills / extensions / MCP-server configs) that surface does
not carry.

Today's gap, at source:

- `AgentSpec` carries no config or env of any kind — its whole field set is
  Name / Image / Workspace / Egress / Mounts
  (`go/internal/runtime/agent.go:31-43`: "`Name string` …
  `Image string` … `Workspace Workspace` … `Egress EgressPolicy` …
  `Mounts []Mount`").
- `createAndStart` populates only Image/Name/CapAdd/Mounts/Command —
  `Command: []string{"sleep", "infinity"}` — and never sets `Env`
  (`go/internal/runtime/agent.go:208-216`), even though the
  podman layer models it (`go/internal/runtime/podman.go:85-86`:
  "`// Env is the environment variables set on the container.`
  `Env map[string]string`").
- The provision request carries only account / repo oneof / ref /
  idempotency key (`proto/compass/v1/compass.proto:326-349`:
  `agent_account_id = 1`, `oneof repo { remote_url = 2; local_path = 3 }`,
  `ref = 4`, `client_request_id = 5`), and `BuildSpec` maps exactly those
  onto the spec, with everything else operator-static `SpecDefaults`
  (`go/internal/runner/spec.go:57-58`: "BuildSpec maps the
  request's agent account + repo + ref onto a full AgentSpec, filling
  image/egress/workspace-layout from the defaults").

## Global Constraints

Every task below inherits these; they are constraints, not choices.

1. **The Server↔Runner inversion is frozen.** The Runner dials OUT; "the
   Server has no inbound route to call the Runner: command delivery cannot be
   a unary Server->Runner RPC, so it rides the Server's *response* half of a
   Runner-opened bidi stream"
   (`proto/compass/v1/runner.proto:21-25`). Config delivery is
   therefore either (a) fields on the relayed `ProvisionAgentWorkspaceRequest`
   or (b) a Runner-initiated fetch signaled over the `Sessions` response
   stream — never a new inbound Server→Runner RPC. New Runner-initiated RPCs
   are additive and allowed (precedent: `RelayCommsCall`, "A fourth RPC,
   additive to the frozen dial-out shape (the Runner still initiates; the
   Server gains no inbound route)", `runner.proto:82-83`).
2. **`runner.proto` is internal-only.** "RunnerService is an internal control
   protocol spoken only between the Server and the Runner binaries … never
   the module-root exported `gen/` nor the public TS client"
   (`proto/compass/v1/runner.proto:5-11`). Every `.proto` delta in
   this record is a **held-for-review contract delta** landing in its
   implementation PR after `compass.v1`-owner review — the same "named here,
   not written" posture SEA-1327 T4 set
   (`compass-agent-container-runtime.md:742-747`).
3. **The container is immutable after create; the agent is exec-driven.**
   `createAndStart` sets `Command: []string{"sleep", "infinity"}` — "Keep the
   container alive so the Runner can exec into it; the agent is driven via
   exec, not as the container's main process"
   (`go/internal/runtime/agent.go:213-215`). Mounts and container
   Env are fixed at `Create` (`podman.go:71-90`); anything mutable
   post-create must land via the exec seam or a Runner-local mount whose
   *contents* the Runner swaps.
   *Reconciliation (decided, CD-3):* the config mount target is the
   **parent** config dir — the in-container path resolves through a
   `current/` symlink inside the mount, so the Runner swaps *contents* (a
   new version dir + a symlink flip) without touching the create-time mount
   set. A pinned `<version>/`-dir mount was rejected: a bind mount cannot
   see a later symlink flip.
4. **Env-vars ride SEA-1327's surface — no second env channel.** The frozen
   record `compass-agent-container-runtime.md` designs env/secret delivery
   end to end: `FetchSecrets` on RunnerService (its T4, `:750-754`), the
   `SecretMaterializer` with the file-vs-env `DeliveryKind` split and the
   0600 aggregate `$HOME/.compass/env` consumed via
   `podman exec --env-file` never `-e` (its T5, `:819-836`), and rotation
   (its T6, `:842-896`). The store half is already landed:
   `SecretDeliveryEnv` "delivers the secret as an environment value (via the
   aggregate 0600 env file each wrapped exec reads at spawn)"
   (`go/internal/store/secrets.go:20-22`). This record adds no
   env mechanism; a user-declared agent env-var IS a declared secret with
   `DeliveryEnv`.
5. **The in-container reader is out of build scope.** This record NAMES the
   on-disk contract the compass-agent (`packages/compass-agent`)
   reads — paths and formats — but builds no reader. The live-config control
   shell exists and is parked: "`ConfigControl` carries a tool set …
   payload fields parked (SEA-1310)"
   (`proto/compass/v1/agent.proto:148-155`,
   `message ConfigControl {}`). Referenced, not built.
6. **Go stack conventions.** All Runner/Server work is Go under
   `go` per the platform records
   `docs/designs/platform/go-idioms-and-libraries.md` and
   `docs/designs/platform/go-toolchain-default.md`: errors wrapped with
   `%w` and stage-tagged (the `StageError` pattern,
   `go/internal/runtime/agent.go:70-79`), no swallowed errors,
   `context.Context` threaded first-parameter throughout.
7. **Secret-adjacent posture for anything written into the container.**
   Content enters the container over the stdin-`sh -s` exec posture — "the
   script (and any secret it embeds) then never appears in the exec argv"
   (`go/internal/runtime/podman.go:101-103`) — or as a read-only
   bind mount (`go/internal/runtime/podman.go:62-66`,
   `type Mount struct { HostPath, ContainerPath string; ReadOnly bool }`).
   MCP-server configs are **credential-free by MVP rule** (CD-3), so nothing
   in the config bundle is secret-bearing; secret values ride SEA-1327's
   surface, which already takes this posture.
8. **Fleet-config writes are operator-scoped.** Setting the fleet's config
   is an operator/admin action, not a per-tenant one: `PutAgentConfig` /
   `DeleteAgentConfig` MUST be gated to the operator at the RPC boundary.
   The primitive exists: the network door's method-level gate marks an RPC
   "reachable on the network door only by the bootstrap admin"
   (`go/internal/auth/admin_gate.go:22-23`, `adminOnly`);
   `classifyProcedure` gates the privileged CompassService RPCs
   (`admin_gate.go:49-56`) and fail-closes — "An unrecognized path
   (ok=false) is treated as adminOnly — fail closed, never admit an unknown
   method as open" (`admin_gate.go:44-46`) — with an exhaustiveness test
   forcing every new procedure to be classified. T2 classifies both write
   RPCs `adminOnly`.

## Approach

The seam mirrors, deliberately, the shape SEA-1327 already froze for secrets
— its Decision 3: "Distribution + rotation ride the v0.6 config spine (Runner
fetches; `Sessions` stream signals; stdin-`exec` injection; the file-vs-env
delivery split)" (`compass-agent-container-runtime.md:31-36`) — because
skills/extensions/MCP-configs are the *config half* of the very same v0.6
spine that secrets ride ("the Runner pulls a hosted agent's config bundle
over its existing gRPC connection and materializes it as a **Runner-local
read-only bind mount** into the container",
`compass-0.6/design.md:871-873`). One spine, two payload classes: secrets
(SEA-1327, built/landing) and config bundles (this record).

### Decision CD-1 — declaration surface: a Server-side fleet-wide bundle store

An operator declares the fleet's config against the **Server** — one bundle
every agent gets, with no per-agent key — via new operator-scoped
`CompassService` RPCs (`PutAgentConfig` / `GetAgentConfigInfo` /
`DeleteAgentConfig`, T2; Global Constraint 8). The
bundle is a content-addressed tarball of three top-level directories:

```text
skills/<name>/SKILL.md [+ support files]   # skill trees, verbatim
extensions/<name>/…                        # extension trees, verbatim
mcp/<name>.json                            # one MCP-server config per file
```

The Server stores a **singleton** `(version, bundle_bytes)` — the fleet's
one current bundle — with `version` = a **canonical content hash of the
decompressed content** (sorted paths + file bytes, zeroed metadata; the
tarball is transport only — T1). Content-addressed in the sense the v0.6
store shape names ("the Server is the config store of record, holding each
agent's config versioned (content-addressed per agent)",
`compass-0.6/design.md:869-870`), narrowed for the MVP to one fleet-wide
bundle. Hashing canonical content rather than tarball bytes makes the
version deterministic: tar member ordering/mtimes/uid-gid and gzip framing
never perturb it, so a no-op re-push of identical config cannot mint a new
version (and cannot Reload a live fleet, T6).

Retention (folded from review): the Server keeps the **current bundle
only** — `PutAgentConfig` is a whole-bundle replace and no rollback RPC
exists, so the flip deletes the superseded row (T1); the Runner likewise
prunes superseded version dirs after a successful swap (T4, CD-3).

Post-MVP seam (named, unbuilt): **persona/role-keyed bundles** — when config
differentiates, it keys on a persona / agent-level role, never an individual
agent id.

Authoring workflow (recommended, MVP): the operator keeps the three config
directories in a **version-controlled repo** and PRs changes against it; a CI
step on merge runs `compass config put <dir>`, publishing the merged bundle
into the Server store. This gives config full PR review, history, and rollback
without Compass taking on a git-credential domain or a sync loop — the repo is
the *authoring* source, `put` is the *publish* step, and the delivery contract
(this record) is identical whether the bundle is authored by hand or from a
repo.

Post-MVP seam (named, unbuilt): **native GitOps pull** — an operator declares a
`config_repo_url + ref` and the Server clones/reconciles the config repo
directly, rather than a client running `put`. Deferred past MVP because it adds
a private-config-repo credential domain (distinct from the agent's target repo)
and a re-pull/sync mechanism — the "second mechanism" this record's fetch
inversion (CD-2) otherwise avoids — and it composes with persona-keying (a repo
path or branch per persona) when both land.

Not chosen: proto fields on `ProvisionAgentWorkspaceRequest`. That request is
relayed verbatim through the `Sessions` command oneof
(`proto/compass/v1/runner.proto:139`:
`ProvisionAgentWorkspaceRequest provision = 6;`). The bandwidth cost is
real but bounded — the provision command rides once per provision plus
idempotency retries, not on every frame — so the rejection rests on the
two structural counts: (b) bundle bytes are an internal transport concern
that would leak into the public `compass.proto` client surface, and (c)
provision-time push fires exactly once, so updates to a live agent would
need a second mechanism anyway. See *Alternatives considered*.

**Env-vars have no new declaration surface.** A user-supplied agent env-var
is declared as a secret with env delivery — the store row already models it:
"`SecretDeliveryEnv` delivers the secret as an environment value (via the
aggregate 0600 env file each wrapped exec reads at spawn)"
(`go/internal/store/secrets.go:20-22`). Non-secret env values
ride the same rows (a plain value in the secret store is safe; a secret in a
config bundle would not be), keeping exactly one env channel and one rotation
semantics (SEA-1327 T6).

### Decision CD-2 — carriage: streaming Runner-fetch over the frozen inversion

The Runner fetches the bundle; the Server only ever signals. Two
held-for-review deltas on the existing internal `RunnerService`
(`proto/compass/v1/runner.proto:43`), both Runner-initiated so
the frozen dial-out shape is untouched:

- `rpc FetchAgentConfig(FetchAgentConfigRequest) returns (stream FetchAgentConfigResponse)`
  — **server-streaming**, Runner→Server, unkeyed (it fetches the one fleet
  bundle); the response stream carries a version frame then bundle chunks
  (T3), so bundle size is never bounded by a unary message-size cap (gRPC's
  default recv cap is ~4 MiB; connect-go imposes none unless configured) —
  transfer is chunked, and the security caps (decompressed-size
  and file-count, enforced at T1's door and re-enforced at every Runner
  unpack) remain the guards, independent of transport. Additive to the
  service exactly as `RelayCommsCall` was ("A fourth RPC, additive to the
  frozen dial-out shape (the Runner still initiates; the Server gains no
  inbound route)", `runner.proto:82-83`) and as SEA-1327's `FetchSecrets`
  will be (`compass-agent-container-runtime.md:750-754`).
- A `ConfigVersion { version }` variant on the `SessionsResponse` command
  oneof (`runner.proto:132-141`) — signal only, never bundle bytes;
  fleet-wide, so it carries no agent key. The sibling of SEA-1327's
  `SecretsVersion` signal (`compass-agent-container-runtime.md:763-767`)
  and the concrete cut of the v0.6 line "the Server signals 'config version
  N for agent X' over the `Sessions` bidi stream"
  (`compass-0.6/design.md:876-877`), narrowed to the fleet bundle.

Signal-then-pull, stated precisely: the Server "reaches" the Runner for a
config (or env/secret) update only on the response half of the
Runner-opened `Sessions` stream — "the Server's RESPONSE stream pushes
session *commands* downward"
(`proto/compass/v1/runner.proto:53-56`) — and the Runner then
re-fetches. No new inbound Server→Runner RPC exists; the frozen dial-out
inversion is untouched. This is exactly what SEA-1327 T6 wires for
`SecretsVersion` (`compass-agent-container-runtime.md:844-856`); env-var
updates ride that same secrets path — this record adds no env mechanism.

Fetch-at-provision: when the Runner executes a relayed `provision` command
it calls `FetchAgentConfig` before `Launch`, so the bundle is materialized
and mounted at container create. A Runner that receives a `ConfigVersion`
newer than what it materialized re-fetches (T4/T6).

### Decision CD-3 — injection per config type

| Config type | Mechanism | Why |
| --- | --- | --- |
| Skills | Runner-local dir, **read-only bind mount** at `/run/compass/agent-config` | Bulk file trees; mount is zero-copy into the container and tamper-proof from inside (`ReadOnly` mounts exist today: `go/internal/runtime/podman.go:62-66`) |
| Extensions | Same mount, `extensions/` subtree | Same shape as skills |
| MCP-server configs | Same mount, `mcp/` subtree — **credential-free by MVP rule**: an MCP config MUST NOT embed a credential; MCP servers read their tokens from the aggregate env file (a declared env secret), inheriting SEA-1327 rotation/deletion for free. The better post-MVP shape (`secret://` resolution with correct rotation) is tracked as SEA-1576 | Config bodies are non-secret and mount fine; a Runner-resolved credential copy would escape SEA-1327 T6's rotation/deletion — its removal scan covers only `$HOME/.compass/secrets/` and its regeneration set is {aggregate env file, provider seed, gh `hosts.yml`} (`compass-agent-container-runtime.md:868-887`) — so the MVP forbids it rather than forking rotation |
| Env-vars | **SEA-1327's surface, unchanged**: declared-env secrets land in the 0600 aggregate `$HOME/.compass/env`, consumed via `podman exec --env-file` never `-e` (`compass-agent-container-runtime.md:819-836`) | One env channel, one rotation path; this record adds nothing here |

Concretely the Runner unpacks the fetched bundle into a versioned dir
`<runner-state>/config/<version>/`, relabels it into the container's SELinux
MCS category (below), flips a `current` symlink (atomic swap), and `Launch`
mounts the **parent** dir `<runner-state>/config/` read-only at
`/run/compass/agent-config`; the agent reads through
`/run/compass/agent-config/current/…`, so a later symlink flip is visible
live inside a running container — the shape the v0.6 spine describes ("the
Runner pulls a hosted agent's config bundle … and materializes it as a
**Runner-local read-only bind mount**", `compass-0.6/design.md:871-873`).
Cost, accepted: the agent can see superseded version dirs (mitigated by
pruning, below) and readers must tolerate mid-read flips (v0.6 already
assumes notify-then-re-read). This rides the existing `AgentSpec.Mounts`
seam ("Mounts is read-only host mounts",
`go/internal/runtime/agent.go:41-42`) — `AgentSpec` gains no new
field for the MVP; the SpecBuilder appends the config mount (T4/T5).

**SELinux relabel is load-bearing on the parent mount — resolved, not
open:** the Mount layer hard-codes relabel-at-create — "ReadOnly maps to :ro
and every mount gets SELinux relabelling (:Z) so the substrate works on
enforcing hosts" (`go/internal/runtime/podman.go:60-61`) — and
that relabel runs at container `Create`. A version dir the Runner writes
into the mounted parent tree AFTER create carries the Runner's label, not
the container's private MCS category, so on an enforcing host a confined
agent would get EACCES reading a newly flipped `current/<version>/` — the
live re-read would silently fail on exactly the hosts `:Z` exists to
support. Resolution splits by call site: at **provision** the create-time
`:Z` relabels the whole parent tree (the version dir and `current`) into
the new container's MCS category as part of `Create` — Materialize runs
before the container exists, so there is no label to target and no extra
step is needed. On the **post-create update path** (T6) the new version
dir is written into the already-mounted tree and would carry the Runner's
label, so there the Runner **`chcon -R`s the new version dir into the
container's MCS label** (read via `podman inspect` MountLabel) after
writing it and before flipping `current` — chosen over adding a per-mount
relabel control to the frozen `Mount` struct (a larger podman-layer delta
this record would have to own).

Pruning (folded from review): after a successful `current` flip (and, on
update, the subsequent Reload), the Runner **prunes** superseded
`<version>/` dirs — the Server is the store of record, so old versions are
re-fetchable and the host keeps only `current` (T4; the retention twin of
CD-1's current-only server rule).

**Env-vars are fleet-global for the MVP — settled, and consistent with the
fleet-wide reshape:** SEA-1327's reused surface is store-global inject-all
— "the MVP **injects the whole store into every agent**"
(`compass-agent-container-runtime.md:29-30`), and `FetchSecrets` takes "no
names filter, no grants" (`compass-agent-container-runtime.md:750-753`) —
which matches this record's scope exactly: one fleet config, one fleet env,
every agent. Per-agent/persona env scoping rides SEA-1327's named
grants/filter seam post-MVP, not a second channel here.

**The in-container contract (named, not built here):** the compass-agent
entrypoint reads `/run/compass/agent-config/current/skills/**`,
`…/current/extensions/**`, and `…/current/mcp/*.json` (credential-free) at
session construction — the caller-supplied `AgentSessionConfig` seam
("Constructed by the caller (container entrypoint) via `createAgentSession`
with its model/tools/system-prompt",
`packages/compass-agent/src/agent.ts:44-46`). A `version` file
in each version dir carries the bundle hash for observability. Building
that reader is compass-agent's lane.

### Decision CD-4 — update path: in-place agent Reload, live-pull as the named seam

MVP: on `ConfigVersion`, the Runner re-fetches, unpacks the new version dir,
chcons it into the container's MCS label, flips `current`, and applies via
an **in-place agent Reload** — no container teardown. The primitive exists
today: `Reload` "restarts a session's agent in place, reusing the session id
so the board entry is continuous"
(`go/internal/runner/host.go:230-231`) — it stops the agent's
exec stream and calls `StartAgent` against the SAME container
(`host.go:245-248`), which the exec-driven model makes cheap by
construction: the container's `Command` is `sleep infinity` — "Keep the
container alive so the Runner can exec into it; the agent is driven via
exec, not as the container's main process"
(`go/internal/runtime/agent.go:213-215`). Because the mount is
the parent dir (CD-3), the re-exec'd agent re-reads
`/run/compass/agent-config/current/…` and finds the new version — no mount
change, no create-time state touched.

The **throwaway-container restart** (stop → relaunch → transcript replay,
`compass-0.6/design.md:886-888`) is retained ONLY for changes that
genuinely need a fresh container — an image change (the versioned-OCI-pull
path) — never for config reload.

Named future seam, explicitly unbuilt: live structured injection into the
running agent over its control stream — the parked `ConfigControl` shell
("`ConfigControl` carries a tool set … parked (SEA-1310)",
`proto/compass/v1/agent.proto:148-155`) realizing v0.6's
"injected as structured state into the running first-party agent … the
SDK's `setTools`/`setSystemPrompt`/`setModel` surface"
(`compass-0.6/design.md:880-884`) — no restart at all. Scope of the drop-in
claim: a populated `ConfigControl` frame can carry
setTools/setSystemPrompt-shaped payloads with no re-plumbing of this
record's carriage or storage; and because the parent-dir mount makes a
`current` flip visible live, the file-shaped half needs only a re-read
notification in place of the Reload — the live path is a genuine drop-in on
both halves.

## Alternatives considered

### Push on `ProvisionAgentWorkspaceRequest`

Add `bytes config_bundle` (or per-type fields) to the provision request. Lost
on three counts: (a) the request is relayed verbatim inside the `Sessions`
stream (`runner.proto:127-130` — "The command variants reuse the frozen
public session-RPC request payloads (compass.proto) verbatim"), so bulk
skill trees inflate every relay frame, retry buffer, and the idempotency
replay; (b) it is a public `compass.proto` message — bundle bytes would leak
an internal transport concern into the public client surface; (c) push fires
once at provision, so updates would need a second mechanism anyway — fetch
serves both create and update with one code path.

### `podman cp` / fetch-into-container for the whole bundle

Exec-copy the tree into the container filesystem instead of mounting. Lost:
per-file stdin-exec writes are O(files) round-trips for what mounts deliver
in one flag; an in-container copy is agent-writable (a mount can be
`ReadOnly: true`); and the versioned-dir + symlink-flip host layout gives
atomic swap and rollback for free. Nothing retains it: MCP configs are
credential-free for the MVP (CD-3), so no exec-written 0600 config path
survives in this record.

### A second env surface (container `Env` at create)

`ContainerSpec.Env` exists (`podman.go:85-86`) and setting it at `Create`
would be easy — and wrong: env fixed at create cannot rotate, values ride
`-e KEY=VALUE` into host-visible podman argv (the exposure SEA-1327 T5
eliminates, `compass-agent-container-runtime.md:828-833`), and it forks
delivery from the already-landed `SecretDeliveryEnv` store split
(`go/internal/store/secrets.go:20-22`). Rejected outright.

### Per-item declaration RPCs (DeclareSkill / DeclareMCPConfig)

Mirror SEA-1327's per-item `DeclareSecret` shape instead of an opaque
tarball: one RPC per skill / extension / MCP config. Weighed and rejected
for MVP: skills and extensions are arbitrary file *trees* whose value is
the verbatim-tree property — per-item RPCs would need their own tree
encoding, reinventing the tarball per item — and a whole-bundle put gives
atomic all-or-nothing versioning for free. The line is thinner than it
looks (`GetAgentConfigInfo` already implies the Server parses the bundle
into item names), so a per-item surface can layer on later without moving
the store; for MVP one opaque, door-validated bundle is the simpler
contract.

### Content storage outside the relational store

Store bundle bytes on a filesystem or object store keyed by hash, with the
relational store holding only the `version` pointer. Rejected for MVP: a
BLOB column beside the existing secrets registry is the simplest correct
thing at dogfood scale (bundles are decompressed-size- and file-count-capped
at T1's door; transfer is streamed, CD-2), adds no second durability domain,
and rides the existing
store backup story. Named as the scale seam: the content-addressed
`version` key means moving the bytes out later changes no contract.

## Plan

Every `.proto` interface below is **held for review** (Global Constraint 2):
named here, landed in its implementation PR after `compass.v1`-owner review.

### T1 — Server config-bundle store: `internal/store` fleet bundle singleton

A fleet-wide singleton bundle row beside the existing secrets registry
(`go/internal/store/migrations/0002_secrets.sql`), storing the
bundle bytes and its canonical content hash; one current version for the
fleet, current-only retention.

- Interfaces:
  - Migration `migrations/0004_agent_config.sql`:
    `agent_config_bundle(singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton), version TEXT NOT NULL, bundle BLOB NOT NULL, created_at …)`
    — a one-row table holding the fleet's current bundle.
  - `func (s *Store) PutAgentConfig(ctx context.Context, actor AccountID, bundle []byte) (version string, err error)`
    — `version` = the **canonical content hash of the decompressed
    content**: sha256 over the sorted `(path, file bytes)` sequence with
    zeroed metadata, the tarball treated as transport only — so tar member
    ordering/mtimes/uid-gid and gzip mtime/level never perturb the version,
    and a no-op re-push of identical content is version-stable (it cannot
    emit `ConfigVersion` or Reload a live fleet, T6). Idempotent on
    identical content; replaces the singleton row in one transaction.
    Retention: **current-only** — `PutAgentConfig` is a whole-bundle
    replace and there is no rollback RPC, so the flip deletes the
    superseded row in the same transaction.
  - `func (s *Store) CurrentAgentConfig(ctx context.Context) (version string, bundle []byte, err error)`
    — `ErrNotFound` when no bundle is declared (fetch then materializes an
    **empty** config dir: no bundle is a valid state, never an error at
    provision).
  - Bundle validation at the door: a gzip tarball whose members all live
    under `skills/`, `extensions/`, or `mcp/`; no absolute paths, no `..`,
    no symlink members, **no hardlink members** (a distinct escape vector
    from the symlink/`..` checks); every top-level entry name —
    `skills/<name>`, `extensions/<name>`, `mcp/<name>.json` — matches
    `^[A-Za-z0-9_-]+$` (these become host path segments in T4); a
    **decompressed-size cap and a file-count cap** enforced during
    streamed decompression (the load-bearing guards — a gzip bomb defeats
    any compressed-only cap, and transfer is streamed so no transport cap
    substitutes; the same caps are re-enforced
    at every Runner unpack, T4); `mcp/*.json` must parse as JSON. Rejected
    with a wrapped `%w` field error before a row is written (the
    T3-of-SEA-1327 validate-at-the-door posture, `store/secrets.go:42-48`).
- Consumes: `internal/store` migration harness. Produces: the resolve surface
  T2/T3 serve from.

### T2 — Declaration RPCs on `CompassService` + CLI verbs

The operator-facing write path (public `compass.proto`, so
buf-breaking-checked); both write RPCs are **operator-scoped** (Global
Constraint 8):

- Interfaces (held for review):
  - `rpc PutAgentConfig(PutAgentConfigRequest) returns (PutAgentConfigResponse)`
    — request `{ bytes bundle = 1; }` (gzip tarball; decompressed-size and
    file-count capped at T1's door); response `{ string version = 1; }`.
  - `rpc GetAgentConfigInfo(GetAgentConfigInfoRequest) returns (GetAgentConfigInfoResponse)`
    — request `{}`; response
    `{ string version = 1; repeated string skills = 2; repeated string extensions = 3; repeated string mcp_servers = 4; }`
    — names only, never content (mirrors SEA-1327 T7's value-free status
    view, `compass-agent-container-runtime.md:898-902`).
  - `rpc DeleteAgentConfig(DeleteAgentConfigRequest) returns (DeleteAgentConfigResponse)`
    — request `{}` — the explicit return-to-unconfigured path (chosen over
    blessing an empty-tarball push): clears the singleton (T1's
    `ErrNotFound` → empty-config state, already valid at provision) and
    emits `ConfigVersion` with an empty version so live Runners
    re-materialize the empty dir.
  - Authorization: `PutAgentConfig` and `DeleteAgentConfig` are classified
    `adminOnly` in `classifyProcedure` — the network door's method-level
    operator gate (`go/internal/auth/admin_gate.go:47-56`);
    the exhaustiveness test forces the classification, and an unclassified
    procedure fail-closes to `adminOnly` anyway (`admin_gate.go:44-46`).
    `GetAgentConfigInfo` is value-free (names only) and may be
    `authenticatedOpen`.
  - On successful `PutAgentConfig` the Server emits `ConfigVersion` on
    every live `Sessions` stream (T3's signal — fleet-wide, every Runner).
  - Concurrency: concurrent `PutAgentConfig` calls are
    **last-writer-wins** — whole-bundle replace makes the outcome a valid
    (if surprising) complete bundle, acceptable for MVP; no compare-and-set
    is added.
  - CLI: `compass agent-config push <dir>` (tars + gzips the dir
    client-side), `compass agent-config show`, and
    `compass agent-config delete` over the three RPCs — the same CLI lane
    as SEA-1327's `compass secrets set/list/delete`
    (`compass-agent-container-runtime.md:931`).
- Consumes: T1. Produces: the declaration surface operators drive.

### T3 — `RunnerService` contract delta: streaming `FetchAgentConfig` + `ConfigVersion`

The internal carriage (Global Constraint 1 shape; precedent
`runner.proto:82-83` for an additive Runner-initiated RPC):

- Interfaces (held for review):
  - `rpc FetchAgentConfig(FetchAgentConfigRequest) returns (stream FetchAgentConfigResponse)`
    on `RunnerService` (`proto/compass/v1/runner.proto:43`) —
    **server-streaming**. Request
    `{ string if_version = 1; }` (optional; see T6's reconcile). Response
    frames are a oneof: the first frame carries `{ string version = 1; }`,
    subsequent frames carry `{ bytes chunk = 2; }` — so bundle size is
    never bounded by the connect/gRPC ~4 MiB unary recv cap; the security
    caps live at unpack (decompressed-size + file-count, T1's door,
    re-enforced at T4), not in the transport. On an `if_version` match the
    Server ends the stream after the version frame, no chunks (the
    version-only fetch). Authenticated by the per-Runner token exactly as
    the other RunnerService RPCs (account-subject tokens rejected,
    `runner.proto:45-48`); the fleet bundle is unkeyed, so no
    account→Runner binding question arises.
  - A new `SessionsResponse` command variant
    `ConfigVersion config_version = 9` on the command oneof
    (`runner.proto:132-141`), with
    `message ConfigVersion { string version = 1; }` — fleet-wide, no
    per-account key. The field number `9` is allocated through the
    proto-writer single-writer reservation convention (compass-repo authors
    the oneof text; this record designs the variant shape): the
    `SessionsResponse.command` oneof today uses only tags 2..6
    (`provision = 6`, `runner.proto:132-141`), so `9` is collision-free, and
    it is sequenced with SEA-1327's not-yet-numbered `SecretsVersion` variant
    on the same oneof so the two additive deltas cannot collide.
    Signal only, never bytes; sibling to SEA-1327's `SecretsVersion`
    signal (`compass-agent-container-runtime.md:763-767`). Note:
    `ConfigVersion` is not `request_id`-correlated and has NO
    `SessionsRequest` result variant — it is a notification, not a
    command; the Runner sends no result frame for it.
  - Server handler:
    `func (s *RunnerServiceHandler) FetchAgentConfig(ctx context.Context, req *connect.Request[compassv1.FetchAgentConfigRequest], stream *connect.ServerStream[compassv1.FetchAgentConfigResponse]) error`
    delegating to `Store.CurrentAgentConfig` (T1); empty-config → a version
    frame with an empty version and no chunks, never an error.
- Consumes: T1; the existing `RunnerService`. Produces: the wire surface
  T4/T6 pull through.

### T4 — Runner `ConfigMaterializer`: fetch, unpack, chcon, atomic version dirs

The Runner-side half: turn a fetched bundle into a mountable host dir whose
`current` flip a live container can see.

- Interfaces:
  - `type ConfigMaterializer struct { root string; client runnerv1connect.RunnerServiceClient }`
    where `root` is `<runner-state>/config`.
  - `func (m *ConfigMaterializer) Materialize(ctx context.Context) (ConfigMount, error)`
    — fetches (T3, draining the response stream to reassemble the bundle),
    validates the tarball with the same rules as T1's door (defense in
    depth, including the decompressed-size and file-count caps — every
    unpack re-enforces them), unpacks to `<root>/<version>/` (0755 dirs /
    0644 files, Runner-owned) — then, **on the update path (T6), relabels
    the new version dir into the container's SELinux MCS category (`chcon
    -R`, the label read via `podman inspect` MountLabel)**: the Mount layer
    relabels the whole tree only at container `Create` ("every mount gets
    SELinux relabelling (:Z)…",
    `go/internal/runtime/podman.go:60-61`), which covers the
    first-provision version dir, but a dir written into the already-mounted
    parent tree after create would carry the Runner's label and a confined
    agent would get EACCES without the chcon (CD-3) — then atomically flips
    `<root>/current` → `<version>` (symlink
    and rename). Idempotent: an already-materialized version only re-flips
    the link. Writes `<version>/version` containing the hash (the
    in-container observability file). After a successful flip (and, on
    update, the subsequent Reload, T6), prunes superseded `<version>/` dirs
    — the Server is the store of record, so old versions are re-fetchable
    and the host keeps only `current` (the retention rule; CD-1/CD-3 point
    here).
  - `type ConfigMount struct { HostPath string; Version string }` —
    `HostPath` is the **parent** config dir `<root>` (never the resolved
    `<version>/` dir); consumed by T5 as
    `runtime.Mount{HostPath: cm.HostPath, ContainerPath: "/run/compass/agent-config", ReadOnly: true}`
    (`go/internal/runtime/podman.go:62-66`). Mounting the
    parent keeps a later `current` flip visible inside a live container
    (CD-3); a version-dir mount would pin a live container to that version,
    because a bind mount cannot see a later symlink flip.
- Consumes: T3. Produces: the host dir T5 mounts.

### T5 — Provision-time wiring: fetch before `Launch`, mount on the spec

Thread the config mount through the existing provision path without
reshaping `AgentSpec`:

- Interfaces:
  - The Runner's provision command handler calls
    `ConfigMaterializer.Materialize` before `AgentRuntime.Launch`
    (`go/internal/runtime/agent.go:147-150` — "Launch brings an
    agent online: create + start the container …"); on success it appends
    the config mount (the parent dir, T4) to the built spec's `Mounts`
    (`agent.go:41-42`), leaving `SpecDefaults`/`BuildSpec`
    (`go/internal/runner/spec.go:57-58`) untouched — the mount
    is Runner-materialized state, not operator policy, so it composes AFTER
    `BuildSpec` exactly where the spec comment reserves the seam ("the
    per-agent-account credential and egress derivation that later tiers add
    plugs into the same SpecBuilder seam", `spec.go:6-8`).
  - A missing bundle (T1 `ErrNotFound` → T3 empty response) materializes an
    empty dir — provisioning an unconfigured fleet MUST still succeed.
- Consumes: T4; the built `Launch`/`provision` lifecycle. Produces: a
  container whose agent finds its config at
  `/run/compass/agent-config/current/`.

### T6 — Update loop: `ConfigVersion` → re-materialize → in-place Reload

- Interfaces:
  - The Runner's `Sessions` receive loop, on `ConfigVersion`: `Materialize`
    (T4 — fetch, unpack, chcon, flip `current`), then if
    `ConfigMount.Version` changed, drive an **in-place agent Reload** for
    each live session — `Reload` "restarts a session's agent in place,
    reusing the session id so the board entry is continuous"
    (`go/internal/runner/host.go:230-231`), stopping the
    agent's exec stream and `StartAgent`-ing against the same container
    (`host.go:245-248`); the re-exec'd agent re-reads
    `/run/compass/agent-config/current/…` through the parent mount (CD-3).
    No container teardown, no mount change; the throwaway-container restart
    stays reserved for image changes (CD-4).
  - No new proto beyond T3, no new agent code: `Reload` exists today and
    is already an admin-gated session RPC
    (`go/internal/auth/admin_gate.go:53`,
    `CompassServiceReloadAgentSessionProcedure`); this task only wires the
    `ConfigVersion` trigger to it.
  - Signal coalescing (folded from review): multiple `ConfigVersion`
    signals coalesce to a single re-materialize + Reload pass — only the
    latest matters, because the fetch is unkeyed (always the current fleet
    bundle); a signal arriving while a pass is in flight marks it dirty and
    re-runs once, never queues N Reloads. Interruption semantics, stated:
    the Reload lands on a busy agent by stopping its exec mid-turn; the
    session id and container survive, and the agent's replay barrier
    restores state
    (`packages/compass-agent/src/agent.ts:66-70` — the agent
    "guards locally: control frames that arrive before replay settles are
    applied as replay"); deferring application to a turn boundary is a
    refinement of the CD-4 Reload-vs-live-apply seam, not an MVP behavior.
  - Missed-signal reconciliation (decided): `ConfigVersion` rides live
    `Sessions` streams only — a Runner disconnected at put-time never
    receives it, and nothing else re-checks. On `Sessions`
    (re)establishment the Runner therefore compares its materialized
    `current` version against the Server via the version-only fetch
    (`FetchAgentConfigRequest.if_version`, T3 — on match the stream ends
    after the version frame, no chunks) and re-materializes + Reloads on
    mismatch. SEA-1327's `SecretsVersion` has the identical latent gap; the
    pattern settles jointly.
  - FUTURE seam (named, unbuilt): once SEA-1310 populates `ConfigControl`
    (`proto/compass/v1/agent.proto:148-155`), the same
    `ConfigVersion` handling swaps the Reload for a live control frame; the
    parent mount + symlink flip make that a drop-in (the agent need only
    re-read `current/`).
- Consumes: T3, T4, T5. Produces: config updates reaching a live agent.

### In-container contract (named for compass-agent's lane; NOT built here)

- `/run/compass/agent-config/` (read-only, the parent mount): `current/` →
  the active version's `skills/<name>/…`, `extensions/<name>/…`,
  `mcp/<name>.json` (credential-free — the MVP forbids embedded
  credentials, CD-3), `version`.
- `$HOME/.compass/env` (0600): SEA-1327's aggregate env file; consumed at
  exec spawn via `--env-file`, not read by the agent. An MCP server needing
  auth reads its token from env (a declared env secret), never from its
  mounted config.
- The entrypoint loads all of it when constructing the `AgentSessionConfig`
  (`packages/compass-agent/src/agent.ts:44-46`).

## Tasks

- [ ] T1 — `internal/store` fleet config-bundle singleton + canonical
      content-hash versioning + door validation (`0004_agent_config.sql`,
      `PutAgentConfig`/`CurrentAgentConfig`)
- [ ] T2 — `CompassService` `PutAgentConfig`/`GetAgentConfigInfo`/`DeleteAgentConfig`
      (held-for-review; write RPCs operator-scoped via the `adminOnly`
      gate) + `compass agent-config push/show/delete` CLI
- [ ] T3 — `RunnerService` server-streaming `FetchAgentConfig` RPC (with
      `if_version`) + `ConfigVersion` Sessions signal (held-for-review
      contract delta)
- [ ] T4 — Runner `ConfigMaterializer`: fetch/validate/unpack, chcon into
      the container's MCS label, versioned dirs + atomic `current` flip +
      superseded-dir pruning
- [ ] T5 — Provision wiring: materialize before `Launch`, append the
      read-only parent-dir mount
- [ ] T6 — Update loop: `ConfigVersion` → coalesced re-materialize →
      in-place agent Reload; reconnect reconciliation via the version-only
      fetch; live `ConfigControl` injection named as the SEA-1310 seam

## Open Questions

The seven load-bearing questions this draft batched are all ruled (Matt,
2026-07-30) and folded into the body above as decisions: fleet-wide bundle
scope (CD-1), streaming transfer (CD-2), credential-free MCP configs for
the MVP (CD-3; SEA-1576 tracks the post-MVP credentialed path),
fleet-global env (CD-3), canonical content-hash versioning (CD-1/T1), the
parent-dir mount target (CD-3), and reconnect reconciliation (T6). Two
non-load-bearing deferrals remain; the record may merge with their
defaults.

1. **(Non-load-bearing, deferred) Mount path constant.**
   `/run/compass/agent-config` chosen for tmpfs-adjacent, non-HOME
   stability (survives `$HOME` layout changes; visibly not agent-owned). If
   Matt prefers a `$HOME`-relative read-only path the change is a constant
   — nothing else in the design moves. Deferred: the record may merge with
   this default.
2. **(Non-load-bearing, deferred) Extension load semantics.** "Extensions"
   are carried as opaque file trees under `extensions/`; whether the agent
   entrypoint loads them as OMP extensions or something narrower is the
   reader-side lane's call (out of build scope here). Caveat, kept honest:
   extensions are executable code injected into the agent process, so the
   eventual reader-lane rule is a **security** decision, not merely a
   format one — and pushing a config bundle IS code execution in the agent
   container. That is within the trust model, not a privilege escalation:
   fleet config is an operator-managed write (the operator-scoped boundary,
   Global Constraint 8). The carriage is format-agnostic, so this may merge
   as-is.
