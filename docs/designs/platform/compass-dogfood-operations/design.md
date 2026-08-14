# Compass dogfood operations — environment topology + creds/config delivery

Status: Draft

> **Design record (platform).** THE SPINE of the dogfood-operations set: the
> `main` + `preview` environment model on `mattfw`, auto-deploy on merge, creds
> delivery composing frozen DL-026, config delivery composing frozen DL-078 via
> the orion CI publish job, and the `mattpc`→`mattfw` box migration. Record B
> (`compass-pr-validation/design.md`) and Record C (`compass-local-dev/design.md`)
> consume the contract this record produces; they are referenced, never designed
> here. Composes DL-025/026/027/078/112 — it re-opens none of them.
> Ledger-impact: none (platform record; the design-ledger gate governs only the
> product corpus — `tools/design-ledger-gate/index.ts:45`
> `export const PRODUCT_DIR = "docs/designs/product";`).

## Problem / Intent

The wave must move onto Compass aggressively, but the Compass instance the wave
runs on IS the wave's critical dev infrastructure — it can never run agent PR
code, while agent PRs still need a live place to be tested. This record defines
the two long-lived environments on `mattfw` (`main`: auto-deploys every merge to
`main`, never PR code; `preview`: isolated agent-PR testing), how creds (DL-026)
and Matt's personal config (DL-078, published from `personal/matt/agents/` by an
orion CI job) reach the agents in each, and the migration that moves the wave off
`mattpc`.

## Approach

One box, two self-contained Compass stacks, both instantiated from the SAME
shape the repo already ships: the dogfood loop `devenv up` composes today —
postgres + gen-cert + compass-server (three doors) + mint-runner-token +
compass-runner, all Linux-guarded (`devenv.nix:211` `services.postgres =
lib.optionalAttrs pkgs.stdenv.isLinux`, `devenv.nix:218` `processes =
lib.optionalAttrs pkgs.stdenv.isLinux`, `devenv.nix:346` `tasks =
lib.optionalAttrs pkgs.stdenv.isLinux`). No new deployment technology (no
nixos service module, no k8s, no container-of-containers): an env is a
dedicated checkout + `devenv up` under a systemd user unit. This maximizes
dogfood value — the envs run the exact bring-up every developer and CI path
exercises — and keeps zero packaging drift between "dev loop" and "deployed
env".

### The two environments (the contract Records B and C consume)

| | `main` | `preview` |
| --- | --- | --- |
| Purpose | CRITICAL wave infra; the fleet's live Compass | agent-PR testing (Record B's lanes run here/against it) |
| Code | `origin/main` only, auto-deployed on every merge | PR builds, deployed by Record B's mechanics |
| Checkout | `~/compass-envs/main` (tracks `origin/main`) | `~/compass-envs/preview` (PR ref per Record B) |
| Unix user | `mattw` | `compass-preview` (dedicated; see Creds) |
| Postgres | its own devenv-managed instance (per-checkout state) | its own; never shares a database with `main` |
| Server doors | Unix socket + dev-http `50051` + TLS network door `50061` on the tailnet address | same shape, ports `50151`/`50161`, loopback+tailnet |
| Runner | `--runner-id mattfw-main` | `--runner-id mattfw-preview` |
| Resource ceiling | none (it IS the box's job) | systemd slice cap: `CPUQuota=400%`, `MemoryMax=16G` |
| Lifecycle | systemd user unit `compass-main.service`, always-on, auto-restart | `compass-preview.service`, may be torn down/redeployed freely |

Isolation is structural, not policy: separate checkout ⇒ separate
`$DEVENV_STATE` ⇒ separate postgres data dir, separate TLS keypair, separate
admin token, separate runner token, separate per-container sockets
(`devenv.nix:334` `--runtime-dir "$XDG_RUNTIME_DIR/compass-runner"` becomes a
per-env path). Ports are pinned per env in an uncommitted `devenv.local.nix`
(devenv loads it beside `devenv.nix`: `forks/devenv/docs/src/files-and-variables.md:9-12`
"Same as `devenv.nix`, but not meant to be committed") rather than trusting
`ports.allocate`'s increment-until-free behavior
(`forks/devenv/src/modules/processes.nix:20` "Base port for auto-allocation
(increments until free)") — a stable port is part of the published contract, so
it must not depend on start order. Templates for both envs' `devenv.local.nix`
ship in the ops doc (T2).

The network door today binds loopback only (`devenv.nix:255`
`--listen "127.0.0.1:${toString config.processes.compass-server.ports.network.value}"`);
`main`'s override binds the tailnet address and issues its cert for it —
`compass-gen-cert` takes SANs (`go/cmd/compass-gen-cert/main.go:54-56`
`hosts := flag.String("hosts", defaultHosts, "Comma-separated SANs to issue the
cert for; IP literals bind as IP SANs, names as DNS SANs.")`) — so the orion CI
job, remote UI clients (Record B lane 1), and the wave's own tooling can dial
`https://mattfw.<tailnet>:50061`. Access control stays tailnet + bearer token;
nothing listens on a public interface.

**`main` never runs PR code** is enforced by construction: its checkout is
fast-forward-only from `origin/main` (the deployer refuses a non-FF), and no
other actor writes that checkout.

**Accepted residuals of user-level isolation (stated, on the record):**

- **Pre-auth body-drip DoS from `preview` to `main`.** `preview`'s Server and
  its agent containers can dial `main`'s door at :50061. Auth holds —
  cross-kind tokens are rejected at the door (`go/server/network_door.go:229-232`)
  — but the HTTP-layer body drip sits BELOW auth and IdleTimeout does not reap
  an actively-dripping connection (`network_door.go:173-181`): malicious PR
  code in `preview` can hold `main`'s door open with zero credentials.
  Mitigation (recommended, landed in T2): firewall `preview`'s egress to
  `main`'s ports with an nftables per-uid rule (or a tailscale ACL); absent
  that, the DoS residual is accepted explicitly, not silently.
- **Shared kernel, /tmp + XDG name-squatting, shared tailscaled.** Both envs
  share the host kernel, the world-writable /tmp namespace, and the one
  tailscaled — accepted for the MVP. The stronger-isolation option is weighed
  (and rejected) in Alternatives below: a full container/VM for `preview`.

### `main` auto-deploy on merge

Pull-based, not push-based: a systemd user timer (`compass-main-deploy.timer`,
2-minute interval) runs `git fetch origin main` in the `main` checkout; when
the remote head moved, it fast-forwards and restarts `compass-main.service`.
The binaries rebuild on process start — the server/runner processes `go build`
into the state dir before exec (`devenv.nix:249-250` `bin="${config.devenv.state}/compass/compass-server"`
/ `go build -o "$bin" ./cmd/compass-server`; same shape for the runner at
`devenv.nix:326-327`) — so "deploy" is exactly "advance the tree + restart",
with no separate build pipeline to drift. Restart is graceful: devenv-tasks
SIGTERMs the group and compass-server drains its doors (`devenv.nix:227-230`
"On stop, devenv-tasks traps SIGTERM and killpg()s that group … the SIGTERM
reaches compass-server, which traps it in cmd/compass-server and drains both
doors"). Store migrations apply automatically on the new binary's startup
(`devenv.nix:177-179` "compass-server opens it at startup and applies its
embedded migrations under an advisory lock (go/internal/store/store.go)").

**Deploy-restart blast radius (DECIDED: accept the restart; resume-verified
MVP floor).** Restarting `compass-main.service` stops the Runner, whose
shutdown contract is explicit: Close "drains every container this Runner is
hosting on process shutdown … no agent container outlives the Runner" — it
enumerates the provisioned-container set and tears each one down
(`go/internal/runner/host.go:227-242,261-278`). So every auto-deploy kills
every live agent container on the CRITICAL env, at wave merge-rate —
DECIDED#1 (aggressive dogfooding) and DECIDED#2 (auto-deploy every merge /
critical infra) in direct tension. Server-side resume machinery exists —
SessionResumeSnapshot/ReconstructSessionBody rebuild a session body
from the durable transcript (`go/internal/runnerhub/hub.go:159-182`), and the
Runner's re-enroll clears live bindings and drives orphaned sessions OFFLINE
(`hub.go:293-299,699-717`) — so state is recoverable, but a session does NOT
transparently survive a restart: something must re-drive resume. The
decision: accept the teardown, and REQUIRE T3's test cycle to prove a live
session RESUMES across a deploy (the MVP floor) — GetServerInfo-green is
necessary, not sufficient. Debounce/batch and deploy-on-idle are named
FUTURE hardening arms (Alternatives considered), deferred until the blast
radius bites.

**Build failure is loud, never silent-stale.** The devenv process exec
scripts carry no `set -e` (`devenv.nix:236-258`): a `go build` failure on a
broken origin/main HEAD would either crash-loop the unit or fall through to
`exec "$bin"` running the STALE binary against the NEW checkout — silent
version skew on the critical env. T3 pins the semantics: errexit in the exec
preamble (via the env's `devenv.local.nix`) plus a post-restart check that
the running server reports the deployed rev; any mismatch is a LOUD systemd
unit failure, never a stale binary presented as deployed.

**Rollback is bounded by the migration ratchet.** ff-only protects against
divergence, not against a bad-but-building commit. Migrations apply forward
automatically at the new binary's startup (`devenv.nix:177-179`) with NO down
path, so a tree rollback after a deploy that migrated leaves an old binary
against a new schema. Operator procedure (T6 runbook): stop the deploy timer
(`systemctl --user stop compass-main-deploy.timer`), `git reset --hard <sha>`,
restart — valid ONLY when no migration landed in the rolled-back range;
otherwise roll FORWARD (a revert commit on main). Some rollbacks are
impossible-by-design; that is the accepted cost of the forward-only ratchet.

Push-based alternatives were rejected: a self-hosted GitHub Actions runner on
`mattfw` puts a runner credential on the critical box and couples deploys to
GitHub availability; a webhook receiver is an inbound door on the wave box.
The 2-minute pull window is well inside the merge-to-live latency the wave
needs, and the poller is ~10 lines of sh under version control (T3).

### Creds delivery — composing DL-026 (frozen)

DL-026 is composed unchanged: "Secrets are a Server-side SecretSpec + keyring
store with inject-all, no repo manifest" (`docs/designs/product/DECISIONS.md:108`).
Each env's Server is its own resolver instance — the Server "is the single
place SecretSpec runs — the RunnerService FetchSecrets handler and the
SecretsService write path both delegate to this one instance"
(`go/server/serve.go:353-355`) — resolving under the fixed project
(`go/internal/secrets/resolver.go:19` `const manifestProject = "compass"`) and
the default profile (`resolver.go:23` `const defaultProfile = "default"`)
against the OS keyring, the ruled default for exactly this shape: "keyring
stays the default for the single-user desktop-local Server (the dogfood
shape)" (`docs/designs/product/compass-agent-container-runtime.md:1114-1116`).

**What the store holds** (names in the registry, values only in the keyring —
`go/internal/store/migrations/0001_init.sql:270-272` "their names and how each
is delivered/routed — and NOTHING about their values"):

| Name | kind/delivery | Purpose |
| --- | --- | --- |
| `LITELLM_API_KEY` | provider / env | the fleet's LLM access via Matt's LiteLLM proxy |
| `GH_TOKEN` | gh / env | forge access (PR submit, issue ops) as the seal bot |
| `LINEAR_API_KEY` | env | Linear tracker access |
| MCP creds (per service) | env | each MCP server the fleet config declares (DL-080 forbids creds in MCP configs — they ride this path) |

**How inject-all reaches agents in each env:** unchanged DL-026/DL-079
machinery, instantiated twice. Each env's Runner fetches from ITS server —
"Resolve reads the whole registry (inject-all: no per-agent filter in the MVP)"
(`resolver.go:34-36`) — authorized under the env's own runner enrollment
(`go/internal/runnerhub/handler.go:241-242` "Under inject-all + single-Runner,
'bound to this Runner' == 'present in the hub'"). Because the two envs are two
Servers with two databases, their registries are independent by construction.

**The keyring boundary is the isolation line.** The keyring is per-OS-user
(container-runtime record: "Keyring layout: one entry per
`secretspec/{project}/{profile}/{key}`, current OS user as account",
`compass-agent-container-runtime.md:262-263`). A `preview` Server built from an
agent PR is PR code running with resolver privileges — if it shared `mattw`'s
keyring it could read every production value regardless of profile. So
`preview` runs as a dedicated Unix user `compass-preview` with its OWN keyring
holding a scoped credential set: a spend-capped LiteLLM key, a
reduced-permission bot PAT, a Linear key for a test team (or none). This is the
one place the two envs deliberately diverge, and it is what makes "preview is
isolated from main" true against a malicious-or-buggy PR, not just against
accidents. (The one-user alternative is rejected in Alternatives considered.)

**The headless unlock (DECIDED: blank-password auto-unlock).** The
container-runtime record's keyring ruling was made for a desktop with a real
login; this box has none: `compass-preview` is a lingering system user that
NEVER logs in, and `main` must survive an unattended reboot with no
interactive session. A Secret Service keyring under a lingering headless
user stays LOCKED until something unlocks it, so DL-026's resolver would
fail every resolve. The decided fix: each env user's default keyring
collection is created with a blank password, so gnome-keyring auto-unlocks
it at user-unit start. At-rest protection degrades to file-equivalent (file
permissions + the Unix user boundary) — the honest posture on a
tailnet-trusted single-operator box — while the keyring API surface DL-026
ruled stays in place. (Rejected arms in Alternatives considered.) Until T1's
fresh-boot-no-login round-trip passes under BOTH users, the creds path is
UNTESTED.

**Seeding:** the entry RPCs exist (`proto/compass/v1/compass.proto:170-178`
`service SecretsService { rpc SetSecret… rpc ListSecrets… rpc DeleteSecret… }`)
but the operator CLI has no secrets noun yet — "secrets is a planned future
sibling noun" (`go/cmd/compass/main.go:6-7`). T4 adds `compass secret
set/list/delete` so seeding both envs is a scriptable runbook step, not
grpcurl archaeology.

### Config delivery — composing DL-078 (frozen) + the orion publish job

DL-078 is composed unchanged: fleet config is "a Server-side FLEET-WIDE bundle
store … published via a CI `compass config put` step from a version-controlled
config repo" (`docs/designs/product/DECISIONS.md:111`). The concrete verb that
ships is `compass agent-config push --dir <path>`: "tar+gzip the dir into a
bundle the store door accepts and PutAgentConfig it (admin-gated), printing the
returned canonical version" (`go/cmd/compass/agent_config.go:30-32`). The new
work is entirely on the publishing side, in the orion repo:

1. **Source:** `personal/matt/agents/` in the orion monorepo — already the
   authored home of Matt's AGENTS.md/rules/skills, symlinked to `~/.agents` by
   the nix `agent-config.nix` module (per `skill://nix-hosts`).
2. **Mapping step — a TRANSFORM, not a copy** (`tools/publish-agent-config`
   script in orion): the repo layout does NOT match the door whitelist. The
   door admits "a whitelisted top dir (skills/|extensions/|mcp/|settings/|
   rules/|agents/) or, for a single-component path, one of the two admitted
   top-level filenames (AGENTS.md, models.yml)", fail-closed with
   ErrInvalidArgument (`go/internal/store/agent_config.go:466-496`). Ground
   truth of `personal/matt/agents/` TODAY: a top-level `config.yml` (the door
   admits only `settings/config.yml`, `agent_config.go:582-586`), a top-level
   `mcp.json` (the door admits only `mcp/<name>.json`,
   `agent_config.go:560-580`), and `cotal/` — the wave's own multi-agent
   config, with no home in the whitelist and none needed (DECIDED: `cotal/`
   is transient wave-coordination scaffolding that Compass's own comms
   replace; it is REMOVED at the wave→Compass cutover — the very migration
   this record describes — and is never published via DL-078). The script
   encodes an explicit per-path disposition table: `config.yml` →
   `settings/config.yml`; `mcp.json` → renamed/split into `mcp/<name>.json`;
   `cotal/` → NOT bundled — transient wave-coordination config, removed at
   the wave→Compass cutover; Compass comms supersede it; `agents/`, `rules/`,
   `skills/`, `extensions/`, `AGENTS.md`, `models.yml` → verbatim; `.omp/`,
   `.cotal-config`, editor files → dropped by name. The script FAILS LOUDLY
   (a CI failure) on ANY path not in the table — never fail-open-by-drop,
   which is how a future new subdir would silently never publish while the
   job reports success. The client-side builder then pre-validates the staged
   dir ("The bundle build validates the dir client-side, so a malformed dir
   fails with a clear message before any RPC", `agent_config.go:91-92`).
3. **Trigger:** orion CI is Woodpecker (`ci/woodpecker/`, `ci/pipeline.ts`) —
   orion has NO GitHub Actions (`orion/.github/` holds only
   `secret_scanning.yml` and the PR template). The publish step is a
   Woodpecker pipeline with a path filter on `personal/matt/agents/**` and
   the admin bearer as a Woodpecker secret.
4. **Auth + wiring:** the job runs
   `compass agent-config push --dir <staging> --server-addr https://mattfw.<tailnet>:50061 --ca <pinned-cert>`
   with the admin bearer in `$COMPASS_ADMIN_TOKEN` (env, never a flag: "The
   admin bearer token is env/file only, NEVER a flag, so it cannot leak into
   the process table", `go/cmd/compass/main.go:11-13`; precedence
   `client.go:63-66` "the admin token resolves env -> --token-file ONLY").
   The CI secret holds a token minted once from `main`'s state-dir
   `admin-token` file; server restarts mint a fresh file token but leave
   prior token rows valid, so the CI credential survives restarts.
   Rotation is a first-class operation: `store.RevokeToken` exists and is
   idempotent, distinguishing revoked-from-unknown
   (`go/internal/store/tokens.go:55-81`; the door surfaces ErrTokenRevoked,
   `tokens.go:29-52`) — only a caller was missing, and T4 adds `compass
   token revoke`, converting rotation from DB surgery into a 30-second
   runbook step (T6). Named as a FUTURE fork, not built here: a
   publisher-scoped token kind that can PutAgentConfig but not IssueToken —
   IssueToken is the privilege-escalation half
   (`proto/compass/v1/compass.proto:113-120`; `go/server/service.go:405-413`),
   so a config-publisher that can also mint tokens is strictly worse than
   needed. The CI runner reaches `mattfw` over the tailnet (OQ-2 for the
   Woodpecker agent placement).
5. **Fan-out:** the push targets `main`. Each live agent then re-materializes
   via the frozen DL-079/DL-081 path (signal-then-pull + in-place Reload). The
   job additionally best-effort pushes the same bundle to `preview`'s door so
   both envs test against the same fleet config; a down `preview` does not
   fail the publish.

**Scope honesty on DL-080's "credential-free bundle":** the door enforces
credential-key rejection only for `settings/config.yml` and `models.yml`
(`go/internal/store/agent_config.go:661-738`); rules/agents/skills/AGENTS.md
are "prose, no content check" (`agent_config.go:505-507`). Credential-free
prose rests on authoring discipline plus orion's secret scanning
(`orion/.github/secret_scanning.yml`), NOT on door enforcement.

### Box migration `mattpc` → `mattfw`

DECIDED: the wave and both envs live on `mattfw`; `mattpc` is reclaimed.
`mattfw` is a NEW host in Matt's personal nix flake (today it declares only
`Matts-MacBook-Pro` and `mattpc` — `skill://nix-hosts` "Two host configurations
exist in flake.nix — do not assume others"). Sequence, gated so `mattpc` keeps
running the wave until the last step:

1. **Provision** (T1): add `nixosConfigurations.mattfw` to the personal flake —
   rootless podman with subuid/subgid for both `mattw` and `compass-preview`
   (the runner no longer gates on host uid — see Global Constraints; `preview`
   under a non-1000 uid DEPENDS on the compass-runner-arbitrary-uid record
   being merged and shipped), headless keyring provisioning via the decided
   blank-password auto-unlock, tailscale, the `compass-preview` user, and
   the shared dev
   modules.
2. **Stand up `main`** (T2+T3): checkout, `devenv.local.nix`, systemd units,
   deploy poller; smoke = one driven agent session end to end.
3. **Seed creds** (T4): `compass secret set` for the table above, both envs.
4. **Wire config publish** (T5): orion job live; verify a
   `personal/matt/agents` edit lands in a running agent's config dir.
5. **Parallel-run:** wave agents spawn on `mattfw` `main` for new work while
   `mattpc` sessions drain naturally; no hard cutover of live sessions.
6. **Reclaim gate** (T6): `mattpc` is reclaimed only when (a) `main` has run
   the wave ≥1 week without a `mattpc` fallback, (b) creds + config publish
   are verified on `mattfw` alone, (c) every item on T6's NAMED mattpc
   checklist (seeded from a `loginctl`/`systemctl --user`/process survey:
   zellij session, cotal mesh, workspaces, tailnet ACLs, the LiteLLM proxy's
   home, tokens) is migrated or retired. Until then `mattpc` stays a warm
   fallback.

### Alternatives considered (rejected, on the record)

- **Deploy on a tag/marker instead of every main commit** — decouples
  "merged" from "restart main now" and would shrink the deploy-restart blast
  radius, but
  reintroduces drift between origin/main and the live env plus a manual
  promotion step. Rejected: DECIDED#2 says `main` auto-deploys every merge.
- **Pull-based config publish** — `main`'s box polls orion and pushes the
  bundle to its OWN loopback door, so no admin bearer ever leaves the box:
  the same pull-not-push trust argument this record makes for deploys, and it
  deletes the headline admin-in-CI risk outright. Rejected for the MVP: it
  couples config freshness to a second poller, moves the mapping script's
  fail-loud gate off CI (where a failure is seen on the PR that caused it),
  and DL-078 ruled a CI publish step. Worth revisiting if the CI-held admin
  token ever becomes the governing risk.
- **Full container/NixOS-container/VM for `preview`** instead of a Unix user —
  closes the shared-kernel, /tmp + XDG name-squatting, and tailnet-adjacency
  residuals stated above. Rejected for MVP friction: a nested
  rootless-podman-inside-container/VM runner loop is exactly the packaging
  drift the devenv-up-as-deploy-unit approach exists to avoid. The residuals
  are accepted explicitly, with this as the named upgrade path.
- **Root-held keyring-unlock oneshot for the headless users** (instead of
  the decided blank-password auto-unlock) — a oneshot feeds the unlock
  secret from a root-held file. Rejected: absent a TPM it buys ~nothing over
  the blank-password collection — the unlock secret sits at rest on the SAME
  disk, root-readable — while adding an unlock-ordering moving part.
- **Non-keyring SecretSpec backend (file/`pass`) for the headless users** —
  deviates from DL-026's ruled keyring default. Not taken; the ruled keyring
  API surface stays in place.
- **Debounce/batch deploys** and **deploy-on-idle** (defer the restart while
  sessions are RUNNING — the hub knows session state) — named FUTURE
  hardening arms for the deploy-restart blast radius, not rejected outright:
  deferred unless/until the accepted-restart + resume-verified MVP floor
  proves insufficient (the blast radius bites at wave merge-rate).
- **One-user `preview` (both envs under `mattw`, separated by SecretSpec
  profile only)** — simpler (one user, one keyring unlock), but rejected: it
  fails DECIDED#2's "preview isolated from main" against PR code, not just
  accidents — PR code with resolver privileges reads the shared keyring
  account regardless of profile ("current OS user as account",
  `compass-agent-container-runtime.md:262-263`).
- **A long-term delivery mechanism for `cotal/`** (permanently
  nix-symlink-delivered outside the publish path, bundling under
  `extensions/cotal/`, or extending the door whitelist) — mooted by Matt's
  ruling: `cotal/` is only here until the wave moves to Compass; it is
  removed at that cutover, so it needs no long-term delivery mechanism at
  all.

### Non-goals (owned by the sibling records)

- **PR-preview mechanics** — how a PR build reaches `preview`, the ephemeral
  UI client on the live wave, the expanded e2e harness: Record B
  (`compass-pr-validation/design.md`). This record only guarantees B the
  `preview` env definition and both envs' creds/config surfaces.
- **Local dev, macOS, the pre-push gate fix**: Record C
  (`compass-local-dev/design.md`). Local dev is neither env — it is the
  developer's own checkout.

## Global Constraints

- **Frozen decisions composed, never re-opened:** DL-026 (secrets:
  SecretSpec+keyring, inject-all, no repo manifest — `DECISIONS.md:108`),
  DL-078 (fleet config bundle via CI `compass config put` — `DECISIONS.md:111`),
  DL-079/080/081 (carriage/injection/update), DL-025/027 (image/supervisor),
  DL-112 (agent image via GHCR pull).
- **Linux-only, rootless podman** for any env running the Runner: the whole
  process/task set is `lib.optionalAttrs pkgs.stdenv.isLinux`
  (`devenv.nix:211,218,346`). The runner does NOT gate on host uid — the
  uid-1000 startup guard was removed (compass-runner-arbitrary-uid): the only
  remaining startup engine check is the podman userns-remap preflight
  (`go/cmd/compass-runner/main.go:89-99`), and uid 1000 survives only as the
  in-container agent uid (`main.go:165-167`). `preview` under the non-1000
  `compass-preview` uid depends on that record being merged and shipped.
- **`main` runs only `origin/main` builds — never PR code.** Fast-forward-only
  deploys; any deviation is an incident, not a config choice.
- **Secrets never travel as flags or logs:** tokens are env/file only
  (`go/cmd/compass/main.go:11-13`, `go/cmd/compass-runner` convention); the
  registry stores names only (`0001_init.sql:270-272`).
- **Bundle content is credential-free (DL-080) — with an honest enforcement
  scope:** MCP creds ride the DL-026 path; the store door enforces
  credential-key rejection only for `settings/config.yml` and `models.yml`
  (`go/internal/store/agent_config.go:661-738`, DL-126); prose members
  (rules/agents/skills/AGENTS.md) carry "no content check"
  (`agent_config.go:505-507`) and rest on authoring discipline + orion
  secret scanning.
- **Nothing listens on a public interface.** All non-loopback doors bind the
  tailnet address; access = tailnet reachability + bearer token.
- **Ports are pinned per env** in `devenv.local.nix`, never left to
  `ports.allocate`'s increment-until-free (`forks/devenv/src/modules/processes.nix:20`).
  Reserved: `main` 50051/50061, `preview` 50151/50161.
- **Naming:** envs are exactly `main` and `preview`; runner ids
  `mattfw-main` / `mattfw-preview`; systemd units `compass-main.service`,
  `compass-main-deploy.{service,timer}`, `compass-preview.service`; checkouts
  under `~/compass-envs/<env>`. Records B and C use these names verbatim.
- **Cross-repo split:** T1/T5 land in the orion monorepo
  (`personal/matt/nix/`, its CI, `personal/matt/agents/`); T2/T3/T6 are
  mattfw host state driven from orion-versioned files; T4 lands in this repo.
- **Commits/PRs** per rule://commit-conventions; jj-vine submit only.

## Plan

### T1 — `mattfw` host provisioning (owner: platform; repo: orion)

Add `nixosConfigurations.mattfw` to `personal/matt/nix/flake.nix`, composing
the existing shared modules (`shared/home.nix`, `dev.nix`, `agent-config.nix`,
`linux*.nix`) plus a `nixos/mattfw/` overlay with: rootless podman with
subuid/subgid ranges for BOTH users (`mattw` and `compass-preview` — the
runner no longer gates on uid 1000; the only startup engine check is the
podman userns-remap preflight, `go/cmd/compass-runner/main.go:89-99`, and uid
1000 survives only as the in-container agent uid, `main.go:165-167`; `preview`
under a non-1000 uid therefore DEPENDS on the compass-runner-arbitrary-uid
record being merged and shipped), the decided headless keyring provisioning
(blank-password default collection for both env users, so gnome-keyring
auto-unlocks at user-unit start with no login),
`users.users.mattw.homeMode = "0700"` pinned explicitly (the 0600
admin-token file's protection silently depends on the home not being
world-traversable, `go/server/network_door.go:347-357`), tailscale, and a
`compass-preview` system user (its own 0700 home, subuid/subgid range,
lingering enabled so its user units run unattended).

- Interfaces:
  - Consumes: `personal/matt/nix/flake.nix` (host list), `nixos/common.nix`,
    shared modules per `skill://nix-hosts`.
  - Produces: `nixosConfigurations.mattfw` converging via
    `sudo nixos-rebuild switch --flake …#mattfw`; users `mattw` (homeMode
    0700) and `compass-preview` with `loginctl enable-linger`; both keyrings
    auto-unlocked headlessly via the blank-password collections.
- Test cycle: `nix-switch` on mattfw; `podman info` rootless under both
  users; then the load-bearing check — REBOOT the box, log NOBODY in, and
  round-trip `secret-tool store` + `secret-tool lookup` under BOTH `mattw`
  and `compass-preview` from their lingering user sessions. An
  interactive-session round-trip proves nothing here (login unlocks the
  keyring); until the fresh-boot-no-login round-trip passes under both
  users, the creds path is UNTESTED.

### T2 — env instantiation: checkouts + `devenv.local.nix` + systemd units (owner: platform; repo: orion, applied on mattfw)

Create `~/compass-envs/main` (as `mattw`) and `~compass-preview/compass-envs/preview`
clones. Author the two `devenv.local.nix` overlays (version-controlled as
templates in orion, copied per env): pin
`ports.devhttp.allocate`/`ports.network.allocate` per the reserved table,
override `--listen` to the tailnet address for `main`, extend the gen-cert
task's `--hosts` with the tailnet name/IP
(`go/cmd/compass-gen-cert/main.go:54-56`), and set per-env `--runner-id` +
`--runtime-dir`. Author `compass-main.service` / `compass-preview.service`
user units wrapping `devenv up` in the checkout (Restart=on-failure), with
`compass-preview.service` under a slice carrying `CPUQuota=400%` /
`MemoryMax=16G`.

T2 also installs the `preview`-egress firewall from the residuals paragraph:
an nftables rule matching `compass-preview`'s uid dropping egress to `main`'s
door ports (50051/50061), closing the pre-auth body-drip hold
(`go/server/network_door.go:173-181`) at the network layer.

- Interfaces:
  - Consumes: `devenv.nix` process/task set (`devenv.nix:218-427`); devenv's
    `devenv.local.nix` overlay mechanism
    (`forks/devenv/docs/src/files-and-variables.md:9-12`).
  - Produces: `systemctl --user start compass-main` ⇒ ready server (probe:
    GetServerInfo over dev-http, the existing readiness check
    `devenv.nix:291-293`); the ENV CONTRACT for Records B/C: `main` door
    `https://mattfw.<tailnet>:50061`, `preview` door `…:50161`, admin token at
    `<checkout>/.devenv/state/compass/admin-token`.
- Test cycle: both units up on one box simultaneously; `compass agent-config
  show` answers on both doors; kill -9 the server, unit restarts, the CI's
  stored token still authenticates (mint-on-restart writes a fresh
  admin-token file but leaves prior token rows valid until explicitly
  revoked — T4's `compass token revoke` is the rotation path); from a
  `compass-preview` shell, a connection to `main`'s ports is REFUSED
  (egress rule holds).

### T3 — `main` auto-deploy poller (owner: platform; repo: orion, applied on mattfw)

A `compass-main-deploy.service` (oneshot, `set -euo pipefail`) + `.timer`
(2 min): fetch `origin/main`; if head unchanged, exit 0; else `git merge
--ff-only origin/main` (refuse on non-FF) and `systemctl --user restart
compass-main`. Rebuild is implicit in process start (`devenv.nix:249-251` go
build into state dir before exec); migrations apply at server startup
(`devenv.nix:177-179`). Two failure semantics are pinned:

- **Never silent-stale:** the devenv exec scripts have no `set -e`
  (`devenv.nix:236-258`), so a broken build could fall through to `exec`ing
  the stale binary against the new checkout. The env's `devenv.local.nix`
  prepends errexit to the exec preamble, and the poller verifies
  post-restart that the running server reports the deployed rev — any
  mismatch (or build failure) is a LOUD systemd unit failure.
- **Failures are pushed, not polled:** on ANY deploy failure (non-FF, build
  failure, migration failure, readiness timeout) the unit pushes ONE line to
  the wave's home channel (~3 lines of curl, not an alerting system) —
  `systemctl --user --failed` is a pull surface nobody polls, and the deploy
  path can wedge silently. (This was OQ-4's minimal form, now in scope here;
  anything beyond it stays deferred, OQ-4.)

The poller also checks the TLS cert's NotAfter on each run and fails loudly
(unit failure + home-channel line) ≤30 days out: gen-cert is skip-if-present
against a 365-day validity (`go/internal/certgen/certgen.go:32`), and expiry
presents as an opaque TLS error taking down CI publish, runner re-enrollment
on restart, and every remote client at once (`devenv.nix:204-207` "Cert
expiry: gen-cert is skip-if-present forever against a finite --validity, so
once the cert expires the loop fails with an opaque TLS error") — a
day-365 time bomb a runbook-only entry would not catch. Rotation is
`compass-gen-cert --force` + restart (T6 runbook).

- Interfaces:
  - Consumes: the `main` checkout; `compass-main.service` from T2; the wave
    home channel's webhook/endpoint (one URL, held on-box, never in CI).
  - Produces: merge-to-live latency ≤ ~3 min; a journal line per deploy with
    old/new sha; ANY deploy failure ⇒ unit failure + one home-channel line;
    cert NotAfter warning ≤30 days out.
- Test cycle: land a trivial main commit and observe fetch→restart. The
  load-bearing check is session SURVIVAL, not liveness: start an agent
  session, deploy across it, and verify the session RESUMES on the restarted
  stack (the Runner's Close tears down its container,
  `go/internal/runner/host.go:227-242,261-278`; resume rebuilds from the
  durable transcript, `go/internal/runnerhub/hub.go:159-182`). GetServerInfo
  green is necessary, not sufficient — that is the decided MVP floor. Also
  force one build failure and verify unit failure + home-channel ping and no
  stale binary running.

### T4 — `compass secret` + `compass token revoke` CLI (owner: compass-server; repo: this)

Add the planned secrets sibling noun (`go/cmd/compass/main.go:36-37` "Secrets
is a future sibling noun; adding it is one more AddCommand here"): `compass
secret set <NAME> [--delivery env|file] [--kind …]` (value on stdin, never
argv), `secret list`, `secret delete <NAME>`, driving the existing
SecretsService RPCs (`proto/compass/v1/compass.proto:170-178`) through the
shared conn/bearer plumbing (`go/cmd/compass/client.go`).

Plus the rotation verb: `compass token revoke` — one Cobra command over the
existing `store.RevokeToken` (idempotent, distinguishes revoked-from-unknown,
`go/internal/store/tokens.go:55-81`; the door already surfaces
ErrTokenRevoked, `tokens.go:29-52`); only the caller is missing. This
converts the CI admin bearer from unrevokable-in-practice into a 30-second
rotation (runbook entry in T6).

- Interfaces:
  - Consumes: `SecretsService.SetSecret/ListSecrets/DeleteSecret`
    (`compass.proto:173,176,178`); `store.RevokeToken`
    (`go/internal/store/tokens.go:59`) surfaced through a server RPC or the
    socket door; `dialClient(cmd)` (`go/cmd/compass/client.go:204-210`); flag
    conventions of `addConnFlags` (`main.go:55-69`).
  - Produces: `compass secret set LITELLM_API_KEY --delivery env < key.txt`
    round-trips against a live door; `secret list` renders set/unset + routing,
    never values ("List declared secrets by name with set/unset + routing —
    never values", `compass.proto:174-175`); `compass token revoke` after
    which the revoked bearer fails ErrTokenRevoked at the door.
- Test cycle: CLI unit tests in the existing `cli_test.go` style, plus a
  pgtest round-trip (including revoke → door rejects); red-first per
  rule://red-green-testing.

### T5 — orion config-publish CI job (owner: platform; repo: orion)

The DL-078 publish step: the `tools/publish-agent-config` mapping script (the
per-path disposition TRANSFORM from `personal/matt/agents/` to the door
whitelist described in Approach — FAIL-LOUD on any unmapped path, never a
silent drop) + a Woodpecker pipeline (orion CI is Woodpecker —
`ci/woodpecker/`, `ci/pipeline.ts`; orion has no GitHub Actions)
path-filtered on `personal/matt/agents/**`, running `compass agent-config
push --dir <staging>` against `main`'s door (admin bearer from a Woodpecker
secret via `$COMPASS_ADMIN_TOKEN`, CA pinned from the checked-in `main`
cert), then best-effort the same push to `preview`. The Woodpecker shape
strengthens OQ-2's option (a): a tailnet-resident self-hosted Woodpecker
agent is the natural runner placement.

- Interfaces:
  - Consumes: `compass agent-config push --dir`
    (`go/cmd/compass/agent_config.go:30-32`); `--server-addr`/`--ca`/
    `$COMPASS_ADMIN_TOKEN` resolution (`client.go:63-66`); the T2 env contract
    (door addresses); a `compass` CLI binary available to the job (built from
    a pinned compass rev — the script owns the pin).
  - Produces: on merge, `agent-config show` on `main` reports the new
    canonical version; live agents re-materialize via DL-079/DL-081 with no
    operator action; ANY unmapped source path ⇒ a CI failure, never a silent
    drop.
- Test cycle: the disposition table exercised against the CURRENT
  `personal/matt/agents/` tree — it must map `config.yml` →
  `settings/config.yml`, `mcp.json` → `mcp/<name>.json`, and route `cotal/`
  to its explicit NOT-bundled entry (transient wave config, removed at the
  Compass cutover — never an unmapped drop); an injected
  unknown path fails the run. Then dry-run mode (build + client-side
  door-parity validation, `go/cmd/compass/bundle.go:23-25`, without pushing)
  on the job's own PR; then one real end-to-end publish verified in a running
  agent's `/run/compass/agent-config/current/` (DL-080 mount path).

### T6 — migration runbook + reclaim gate (owner: platform; repo: orion docs)

A short ops doc beside the flake: the T1→T5 order, the creds-seeding
checklist (both keyrings), the rotation runbooks (admin-token rotation via
`compass token revoke` + re-mint; TLS cert rotation via `compass-gen-cert
--force` + restart; the rollback procedure — deploy timer off, reset to sha,
FORWARD-only when a migration landed in the range), the parallel-run policy,
and the reclaim gate as a NAMED checklist seeded from a `loginctl` /
`systemctl --user` / process survey of `mattpc` — not a catch-all:

- the shared zellij wave session (moved or retired);
- cotal mesh / IRC connectivity (mesh services re-homed to mattfw);
- agent workspaces/clones under `~/agents/workspaces` (migrated or declared
  disposable);
- tailnet ACLs naming mattpc (rewritten to mattfw);
- **where Matt's LiteLLM proxy runs — answered explicitly:** if it runs on
  mattpc, reclaiming mattpc severs the FLEET'S LLM access and no other gate
  item catches it (the `LITELLM_API_KEY` row above presumes the proxy
  outlives the box);
- tokens/credentials minted on mattpc (revoked or re-homed);
- remove `cotal/` config at the Compass cutover (it does not migrate —
  Compass's own comms supersede it).

Parallel-run collision policy (one sentence in the doc, agreed before the
overlap week): both boxes' agents write the same forge/Linear/cotal surfaces
under the same bot identities, so lanes are partitioned by owner — new lanes
spawn on mattfw only, mattpc finishes only lanes already in flight, and no
lane is ever worked from both boxes.

mattpc's flake entry is retired only after the gate passes: (a) `main` has
run the wave ≥1 week with no mattpc fallback, (b) creds + config publish
verified mattfw-only, (c) every named checklist item above is ticked.

- Interfaces:
  - Consumes: T1–T5 outputs; the mattpc survey.
  - Produces: `personal/matt/nix/nixos/mattfw/MIGRATION.md`; the named gate
    checklist Matt ticks before reclaiming mattpc.
- Test cycle: doc review; the gate itself is the test.

## Tasks

- [ ] T1 — `mattfw` nixos host config (platform / orion)
- [ ] T2 — env checkouts + `devenv.local.nix` overlays + systemd units; publish the env contract (platform / orion+mattfw)
- [ ] T3 — `main` auto-deploy timer + failure ping + cert-expiry check (platform / orion+mattfw)
- [ ] T4 — `compass secret` CLI noun + `compass token revoke` (compass-server / this repo)
- [ ] T5 — orion config-publish CI job (platform / orion)
- [ ] T6 — migration runbook + mattpc reclaim gate (platform / orion)

Dependency order: T1 → T2 → {T3, T4} → T5 → T6. T4 can start immediately
(repo-local); T5 needs T2's doors and T4 only for its live verification leg.

## Open Questions

All load-bearing forks in this record are DECIDED and folded into the body
above (headless unlock, deploy-restart blast radius, `cotal/` disposition,
the dedicated `preview` user). What remains here are ONLY non-load-bearing
deferrals — the record is correct as designed; these are refinements or
external-dependency items:

- **OQ-2 (non-load-bearing deferral) — where the orion publish job's
  Woodpecker agent lives.** `compass agent-config push` must reach
  `mattfw`'s tailnet door, and orion CI is Woodpecker (`ci/woodpecker/`,
  `ci/pipeline.ts`). (a) a tailnet-resident self-hosted Woodpecker agent —
  the natural Woodpecker shape: zero new tailnet credential in CI, but
  publish couples to that agent's uptime; (b) a hosted/ephemeral agent
  joining via an ephemeral tailscale OAuth key — standard, but a tailnet
  credential lives in CI. **Recommendation: (a)**; orion is Matt's personal
  CI and the tailnet-resident agent is the smaller trust surface. Document
  (b) as the fallback in T5. Either placement works without changing the
  design.
- **OQ-3 (non-load-bearing deferral) — `preview` LLM spend scoping.** The
  scoped `preview` credential set wants a spend-capped LiteLLM key. If
  per-key budgets are not available on Matt's proxy, the fallback is sharing
  `main`'s key and accepting `preview` spend risk. **Recommendation:** mint
  a per-env key with a budget cap; treat the shared-key fallback as
  explicitly temporary, noted in T6's checklist. An external dependency on
  the proxy's feature set; the design holds either way.
- **OQ-4 (non-load-bearing deferral) — deploy-failure alerting beyond the
  one-line ping.** The minimal form — ONE line pushed to the wave's home
  channel on deploy failure — is IN SCOPE in T3 (`systemctl --user --failed`
  is a pull surface nobody polls, and the deploy path can wedge silently).
  What stays deferred is anything beyond it: real push-alerting infra,
  escalation, paging. The design is correct without those.
