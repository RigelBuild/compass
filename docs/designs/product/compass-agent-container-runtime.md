# Compass agent container runtime — design

Status: Active

Design for how a per-agent Compass container gets its **toolchain**, its
**credentials**, and stays **current** over a long-lived session, on the Go
stack (`go`). Companion to [`compass.md`](compass.md) §5.3/§5.5 and
the v0.6 record [`compass-0.6/design.md`](compass-0.6/design.md). All grounding
is against the working tree at `6b192e731` (branch
`compass-sea-1327-container-runtime-design`); `oss/` is byte-identical to
`origin/main` at `c1146579a` (verified this run:
`git diff --stat origin/main...HEAD -- oss/` → empty). The Rust
`compass-daemon` crate is cut from the tree (verified this run:
`git ls-tree -r origin/main --name-only | grep -c compass-daemon` → `0`) and is
never cited as live below.

> **Re-authored (SEA-1327 reopen).** A prior version of this record built a
> cred-avoidance sandbox around Nix evaluation (host-store overlay, a dedicated
> eval uid, capture-env-once, eval-phase egress narrowing). Matt's directives
> invert that premise — see *Threat model* below — so Decisions 1–3 are
> rewritten from scratch. Decisions 4–5 and the distribution spine carry over.

Five core decisions are made and are **not open** here:

1. Image = one **self-contained** base image shipping its own Nix + base store;
   the agent activates and rebuilds its devenv **itself, in-container, as
   itself** (no per-repo image builds, no host Nix dependency).
2. Secrets store = SecretSpec + keyring, Server-side — with **no
   repo-committed `secretspec.toml`**; the declared set is what's in the
   store, and the MVP **injects the whole store into every agent**.
3. Distribution + rotation ride the v0.6 config spine (Runner fetches;
   `Sessions` stream signals; stdin-`exec` injection; the file-vs-env
   delivery split) — with **provider (LLM) credentials riding the OMP SDK
   AuthStorage/`getApiKey` surface**, not a secret-file channel — a
   distinction of **consumption** (resolved per LLM call via `getApiKey`),
   not delivery: the credential still lands as a 0600 seed file.
4. No in-container supervisor for MVP; the Runner drives from outside. An
   in-container Go supervisor is a named FUTURE seam, explicitly unbuilt.
5. The Setup agent guides secrets onboarding (store population — it authors no
   in-repo secrets file).

This record grounds those decisions in the current code, fixes the concrete
mechanisms and interfaces, and decomposes the work. It **supersedes by
citation** (never by rewriting the frozen records): `compass.md` §5.5's
per-repo `devenv container build` image model and build-sandbox posture
(`compass.md:157-168`), §5.3/§6.7's default-deny-egress-as-MVP claims
(`compass.md:143`, `:252`), and the v0.6 OCI-image config-distribution bits
(`compass-0.6/design.md:862-888`), as detailed under *Spec impact* below.

## Problem / Intent

A launched agent container today gets exactly one toolchain source and one
secret: the image is built per-repo from its devenv
(`go/internal/runtime/image.go:10-13`: "`devenv container build
<name>` emits a nix2container image spec"), and provisioning installs only the
scoped git credential (`agent.go:232-242`: provision = firewall → credentials
→ clone). There is no provider (LLM) credential, no `gh` credential, no secret
store on the Server (`internal/store/` holds accounts/tokens/channels/messages
only — `store/migrations/0001_init.sql:1-5` names the whole schema: "accounts,
channel groups, channels + membership, agent workspaces, conversation
messages, and subject-typed token hashes"), and no way to rotate anything into
a long-lived container short of tearing it down. Per-repo image builds also
couple agent-runtime updates to repo rebuilds, and the Runner-side clone step
(`agent.go:278-289`) bakes in a one-repo-per-agent assumption the product
model contradicts (multi-repo workspaces, `compass.md:147`).

This record designs the container runtime that fixes all of it: a single
self-contained base image whose devenv the agent manages itself, a Server-side
SecretSpec+keyring secret store, Runner-driven secret distribution over the
v0.6 spine (provider creds through the OMP SDK auth surface), and a rotation
path split by delivery shape (file vs env).

## Approach

### Threat model — the agent is trusted

The container exists for **blast-radius containment, not credential
avoidance**. The agent is handed its credentials on purpose — the whole point
is a capable coding environment — so there is nothing to gain by gating the
agent's access to secrets it is entitled to. What the per-agent rootless
container still buys, and what this record preserves:

- **Host isolation** — a rootless podman container with its own namespaces;
  the agent user is unprivileged and capability-less inside it
  (`egress.go:6-10`: the agent "cannot flush or edit the ruleset even though
  the container nominally holds the capability").
- **Inter-agent isolation** — scoped `$HOME` per container, 0600 credentials,
  no shared writable state; "one agent's token never leaks into another's
  tree" (`workspace.go:1-9`).
- **Auditability** — every secret *fetch* through the Server is audited
  (SecretSpec audit log, D14 redaction). Under inject-all this is
  **fetch-level audit**: each `FetchSecrets` records one whole-store
  resolution with one reason string, not a per-secret access trail;
  per-secret granularity arrives with the future grants seam.

What this record deliberately does **not** build: any mechanism whose only
purpose is to keep the agent away from credentials it will be handed anyway —
no separate eval uid for Nix evaluation, no install-secrets-after-activation
ordering, no captured-env indirection, no eval-phase egress narrowing. Repo
Nix evaluates as the agent, with the agent's credentials present and the
agent's (default-open, Decision 1) egress — the same code the agent would run
by hand a moment later.

### The built substrate this composes with

All file+line references are into `go/internal/runtime/` unless
otherwise pathed. Everything below extends the merged Go runtime package, not
a parallel path:

- **Lifecycle façade** — `AgentRuntime.Launch` = `createAndStart` then
  `provision` (`agent.go:147-172`); `provision` runs, in fixed order,
  `armEgress` (root) → `installCredentials` (agent user) → `cloneRepo` (agent
  user) (`agent.go:232-242`). New provisioning is new steps in this function;
  this record also **deletes** one (the clone, Decision 1).
- **The canonical secret posture** — `installCredentials` feeds the credential
  script "over stdin to `sh -s`, never `sh -c <script>`: the token is in the
  script body, and argv is visible in the container's process list while stdin
  is not" (`agent.go:264-266`); the script installs git's `store` helper
  against a 0600 `$HOME/.git-credentials` (`workspace.go:143-150`). **Every
  secret channel in this record copies this posture.**
- **The container is exec-driven, not self-hosting** — `createAndStart` sets
  `Command: []string{"sleep", "infinity"}` with the comment "Keep the container
  alive so the Runner can exec into it; the agent is driven via exec, not as
  the container's main process" (`agent.go:213-215`). Decision 4 keeps this.
- **Egress** — nftables armed as root at provision; the container holds
  `NET_ADMIN` only so the root arm step can install the ruleset, and the
  agent user "cannot flush or edit the ruleset even though the container
  nominally holds the capability" (`egress.go:6-10`; `EgressPolicy` at
  `egress.go:29-34`). The mechanism stays; the MVP **policy** becomes
  default-open (Decision 1).
- **Scoped credentials type** — `Credentials{Host, Username, Token}` with
  `String`/`GoString` redaction so "a struct dump can't leak it"
  (`workspace.go:60-67`). New secret types copy the redaction.
- **The in-container agent + control stream** — the agent is the first-party
  OMP-SDK process: `CompassAgent` "wraps `Agent` from `@oh-my-pi/pi-agent-core`,
  subscribes its SDK event stream … consumes decoded `AgentControl` ops from
  the ControlSource and drives the SDK (`prompt`/`steer`/`setTools`/
  `setSystemPrompt`)" (`packages/compass-agent/src/agent.ts:3-7`).
  The frozen control oneof is
  `{prompt; steer; ask_answer; config; replay; replay_complete}`
  (`src/control.ts:6-12`); the typed domain union lives at `control.ts:30-53`;
  outbound frames ride `FrameSink` (`src/frame.ts:50-52`). This is the
  re-read-notify seam rotation rides. The SDK `Agent` is **caller-constructed**:
  "Constructed by the caller (container entrypoint) with its
  model/tools/system-prompt" (`agent.ts:27-31`; `opts.agent ?? new Agent()`,
  `agent.ts:54`) — the seam Decision 3's provider-credential wiring plugs into.
- **The SDK auth surface** — `AgentOptions.getApiKey?: (model: Model) =>
  Promise<ApiKey | undefined> | ApiKey | undefined` — "Resolves an API key or
  resolver dynamically for each LLM call. Useful for expiring tokens and
  model-scoped credential routing"
  (`@oh-my-pi/pi-agent-core` `dist/types/agent.d.ts:66-70`; vendored at
  `node_modules/.bun/@oh-my-pi+pi-agent-core@16.4.8`). Behind it,
  `@oh-my-pi/pi-ai` defines the persistence abstraction: `AuthCredential =
  ApiKeyCredential | OAuthCredential` (`dist/types/auth-storage.d.ts:16-24`),
  the `AuthCredentialStore` interface (`auth-storage.d.ts:234`), the
  `AuthStorage` manager over it (`constructor(store: AuthCredentialStore, …)`,
  `auth-storage.d.ts:596-604`), and the SQLite-backed
  `SqliteAuthCredentialStore` (`auth-storage.d.ts:1064-1067`).
- **Transport spine** — Runner↔Server is the frozen internal `RunnerService`:
  "container lifecycle commands **and** Server→Runner config-version signals
  ride the `Sessions` bidi stream (the only Server→Runner path); agent events
  ride `PublishEvents` (Runner→Server only)"
  (`compass-0.6/design.md:960-966`). Note: `RunnerService` is **designed, not
  yet in the Go tree** (grep of `go` for
  `RunnerService|FetchSecrets|Sessions|Enroll` this run: no Go handler
  matches; the wire shape is frozen in
  `docs/designs/platform/go-toolchain-default.md:905-981`, T8). The
  secret-fetch surface below is a delta on that held contract, not on live
  code.

### Decision 1 — self-contained base image; the agent manages its own devenv

**One self-contained base image** ships its **own Nix installation and base
Nix store**, plus devenv + direnv, bun + the bundled `compass-agent`, git, gh,
and the egress toolchain the arm script requires in-image ("Requires nft,
getent, and awk in the image", `egress.go:76-77`). There is **no host
`/nix/store` mount, no overlay store, and no assumption the host has Nix at
all** — Compass must run on hosts without Nix. The image is a normal OCI
artifact the Runner pulls, versioned like the v0.6 restart path expects ("The
agent binary and base image ride versioned OCI pulls",
`compass-0.6/design.md:886-888`).

**The agent activates and rebuilds its devenv itself, as itself.** The agent
works across multiple repos (`compass.md:147`), cloning them itself (see *No
clone step* below), so no single environment can be baked or pre-activated for
it. Instead the in-container Nix is set up for the agent uid — single-user
mode, `/nix` owned by the agent user — so `devenv`, `direnv allow`, and
`direnv exec` work normally, unprivileged, with write access to the store. The
agent modifies `devenv.nix`, re-evaluates, and rebuilds mid-session the same
way a developer does. Cache misses substitute from the public Nix binary cache
over the default-open egress (below) — no special-cased egress phase.

**Runner-driven execs pick the environment up via direnv at the workspace
root.** `ExecAsAgent` builds a bare argv exec — `NewExecSpec(command...)`
with `HOME` = scoped home (`agent.go:176-181`) — which fires no shell hook.
The Runner therefore wraps agent execs as `direnv exec <WorkDir> -- <argv>`
(T2). direnv resolves the `.envrc` governing the *given* directory, and
repos are subdirectories of `WorkDir` — so the wrapper resolves a **root
`.envrc` at `WorkDir`** if the agent has authored and `direnv allow`ed one,
and degrades to the base image's tools otherwise. Per-repo devenvs are the
agent's own business: the agent activates and uses them from *inside* each
repo (its own shell / `direnv exec <repo>` invocations), exactly as a
developer does — the Runner-side wrapper makes no per-repo claim.
Re-evaluation under the wrapper runs as the agent uid with the agent's
credentials and egress — exactly the trusted posture above, so no
captured-env indirection is needed.

**Egress: mechanism kept, MVP ruleset default-open.** `armEgress` and
`egress.go` stay in the tree and in the provision sequence — root-armed
nftables the capability-less agent cannot edit (`egress.go:6-10`) — but the
MVP policy is **allow-all**: the user configures zero domains, and Compass is
as easy to reach the network from as any coding agent on a laptop. T2 adds an
allow-all `EgressPolicy` mode whose ruleset still installs the table (so the
arming step, the NET_ADMIN posture, and the agent-can't-edit property all keep
working) with an accept default instead of the drop default + allowlist
(`egress.go:109-131` `baseRuleset`, "policy drop"). **Per-agent egress
restriction is a future opt-in**: the validated-allowlist path
(`AllowEgress`, `egress.go:40`; dual-stack resolve, `egress.go:16-19`) stays
available behind config, not default-on.

**No clone provision step.** `cloneRepo` (`agent.go:278-289`) is deleted from
the launch path: the agent has git credentials installed
(`installCredentials`, `agent.go:256-276`) and clones its own repos — one
agent, N repos, its own choice of layout. Launch becomes create+start →
`armEgress` (default-open) → `installCredentials`, nothing else. The
`Workspace` clone surface (`Source`, `Branch`, `CloneCommand`,
`workspace.go:69-97`) retires with it; `Workspace` keeps `HomeDir`/`UID`/
`Credentials` and gains `WorkDir` — a pre-created, agent-owned workspace root
replacing `CheckoutDir` ("the absolute path inside the container where the
repo is cloned", `workspace.go:74-76`). Checkout-dir assumptions this
disturbs: `ExecAsAgent` sets the exec workdir to the checkout
(`agent.go:176-181`) and `AgentHandle.CheckoutDir` documents it as the
session's cwd (`agent.go:63-65`) — both re-point at `WorkDir` (T2
Interfaces); the OMP-SDK session's cwd is `WorkDir`, and repo paths under it
are the agent's own business.

The Setup agent still authors `devenv.nix` when a repo lacks one
(`compass.md:164`); its output now feeds the agent's own in-container
activation instead of an image build. Container-readiness is no longer a
Runner-side pre-launch gate — there is no Runner-side checkout to probe —
so `IsContainerReady` (`image.go:82-89`) and `ImageBuilder` retire from the
hot path (flag for deletion in the impl PR if nothing else references them).
**Prebuilt per-repo images are an explicit FUTURE feature** (a warm cache in
front of in-container activation), not MVP.

**Accepted MVP launch cost:** with no prebuilt repo image and no host store,
every container cold-realizes its devenv from the public Nix binary cache —
N agents on a host each pull the same multi-GB closure, and first activation
takes minutes per launch. This is a deliberate MVP trade (boring, portable);
the named mitigations are the FUTURE prebuilt image above and the per-agent
`/nix` named volume recorded under *Alternatives considered*.

### Decision 2 — secrets store: SecretSpec + keyring, Server-side; no repo manifest; inject-all

The Server owns secrets through [SecretSpec](https://secretspec.dev)
([cachix/secretspec](https://github.com/cachix/secretspec), Apache-2.0),
verified against the upstream docs this run:

- Declaration/storage split + provider set: README — "separating secret
  **declaration** from secret **storage** … the actual values live in a secure
  provider like your system keyring, 1Password, or any other backend".
- Keyring layout: one entry per `secretspec/{project}/{profile}/{key}`,
  current OS user as account (docs `providers/keyring.md`, "Storage model").
- Provider fallback chains: per-secret `providers = [...]` lists tried left to
  right for reads; writes go only to the first provider (docs
  `concepts/providers.md`, "How SecretSpec selects a provider").
- Provider-connecting credentials (0.15+) pass **in memory**: "SecretSpec
  passes a retrieved credential to the destination provider in memory. It
  does not export the credential or include it in the environment of a
  process started by `secretspec run`" (docs `concepts/providers.md`,
  "Provider credentials").
- Audit logging (0.12+): "Every secret access recorded locally (who, when,
  why, outcome) — on by default, secret values never logged" (README) —
  feeding the D14 redaction/audit requirement.
- **Go SDK (0.13+):** `github.com/cachix/secretspec/secretspec-go` — "the
  `secretspec-ffi` C ABI loaded at runtime via purego (no cgo)", resolving
  "through the same providers, profiles, fallback chains, and generators as
  the CLI", with a typed `MissingRequiredError` (docs blog
  `secretspec-0-13-sdks.md`). Recommended Server integration surface (OQ2).

The keyring provider is the default (encryption at rest per the provider
matrix), pluggable to 1Password/pass/gopass/Vault later with zero Compass code
change. This satisfies the v0.5 D14 boundary
(`compass-0.5/design.md:528-546`) without a bespoke Compass encrypted store:
encryption at rest, rotation/revocation (provider-native writes), redaction
(the SecretSpec audit log never logs values, plus Compass-side redaction
below). SecretSpec runs **only on the Server** — never inside a container;
containers receive resolved values, not provider access.

**No repo-committed `secretspec.toml`.** One agent works across multiple
repos, so a single in-repo declaration file doesn't fit — and the store, not a
repo, is the source of truth. The user adds secrets through a **Compass
surface** (the Compass CLI and/or the Bridge UI, T7) that writes the
provider directly; the **declared set is what's in the store**. Mechanically
(the SecretSpec resolver is manifest-driven and exposes no enumeration —
the SDK "resolves the exact secrets your manifest declares", 0.13 blog), the
Server keeps a **names-only registry** — a `secrets` table in the existing
Postgres store (`internal/store/`, today accounts/tokens/channels/messages
only per `store/migrations/0001_init.sql:1-5`): name, delivery kind, provider
flag, timestamps, **never values** — and generates the manifest SecretSpec
resolves against from that registry (T3). This is bookkeeping, not
authorization.

**Inject-all for MVP; per-agent grants are a named FUTURE seam.** There is
**no per-agent authorization table**: every launched agent receives the whole
store (non-provider secrets as files/env over the stdin-`exec` channel;
provider creds via Decision 3's AuthStorage path). This follows directly from
the threat model — the agent is trusted with the user's working credentials,
and one user's agents share one store. A **single-user Server is an explicit
MVP constraint** (OQ7, ruled by Matt in this review; user scoping is the named
post-MVP path). **Per-agent scoping (which agent sees which secret) is
explicitly unbuilt**: the seam is the `FetchSecrets`
request/response (T4), which today carries no filter and would gain one — plus
a grants store and a Server-side check — without reshaping any other
interface. The redaction/audit posture is kept in full regardless.

### Decision 3 — distribution + rotation on the v0.6 config spine

Secrets move exactly like config, on the same hops
(`compass-0.6/design.md:960-966`):

1. **Runner fetches from Server** over the existing `RunnerService` gRPC
   connection — a new unary `FetchSecrets` RPC (held-for-review interface,
   T4), returning the **whole store's** resolved values (inject-all, no grants
   filter). The Server resolves via SecretSpec at fetch time; resolved values
   are never persisted outside the provider and never logged.
2. **Server signals rotation** over the `Sessions` bidi stream — the only
   Server→Runner path — as a secret-version signal shaped like the
   config-version signal ("the Server signals 'config version N for agent X'
   over the `Sessions` bidi stream", `compass-0.6/design.md:874-875`). The
   Runner re-fetches on signal; values never ride the signal itself.
3. **Runner injects into the container** via the built stdin-`exec` channel:
   a setup script fed to `sh -s` over stdin (`agent.go:264-270`), writing
   0600 files under the agent's scoped `$HOME` (the `workspace.go:143-150`
   pattern) or staging env values for the agent's exec env. Never argv.

**Provider (LLM) credentials ride the OMP SDK auth surface — not a
secret-file channel.** The in-container agent is constructed by the container
entrypoint (`agent.ts:27-31`: "Constructed by the caller (container
entrypoint) with its model/tools/system-prompt"; `agent.ts:54`), and the SDK
accepts a per-call credential resolver: `AgentOptions.getApiKey?: (model:
Model) => Promise<ApiKey | undefined> | ApiKey | undefined` — "Resolves an API
key or resolver dynamically for each LLM call. Useful for expiring tokens and
model-scoped credential routing" (`pi-agent-core` `agent.d.ts:66-70`). So:

- The Runner materializes fetched provider credentials as a 0600 **seed
  file** under the agent's scoped `$HOME` (same stdin-`exec` posture), keyed
  by provider, shaped as the SDK's `AuthCredential` union (`ApiKeyCredential |
  OAuthCredential`, `pi-ai` `auth-storage.d.ts:16-24`).
- The entrypoint constructs the SDK `Agent` with a **`getApiKey` resolver
  backed by that seed** (loaded into an in-memory `AuthStorage` over an
  `AuthCredentialStore`, `auth-storage.d.ts:234`, `:596-604` — or read
  directly for the api-key-only MVP), and hands it to `CompassAgent` via
  `CompassAgentOptions.agent`.
- Because `getApiKey` resolves **per LLM call**, provider-credential rotation
  is a seed-file rewrite + the `SecretsChanged` re-read notify (below) — no
  agent restart, no env involvement. Provider creds never appear in
  `secretspec`-style env/file delivery, in exec env maps, or in argv.

This re-expresses SEA-1115's Rust-era `ProviderCredentials` intent
(`docs/designs/agents/sea-1115-agent-provisioning-cotal.md:113`, `:151`) on
the Go stack, with the SDK auth surface replacing the harness-supplied
env-var/file fork — the first-party agent has a first-party credential API,
so Compass uses it.

**The file-vs-env delivery split for non-provider secrets (load-bearing).**
Where a secret lands determines how it rotates:

- **File-shaped secrets rotate in place.** The Runner rewrites the file over
  the same stdin-`exec` channel (write-temp + atomic `mv` within `$HOME`),
  then notifies the running agent via a **control-stream re-read op** — a new
  `AgentControl` variant, working name **`SecretsChanged`** (TS domain kind
  `secretsChanged`, carrying the rotated secret names, never values). The
  control seam is built for exactly this: the agent consumes decoded
  `AgentControl` ops from stdin and drives the SDK (`agent.ts:3-7`; union at
  `control.ts:30-53`). The frozen oneof has six variants (`control.ts:6-12`),
  so a seventh is an **additive contract delta held for review** —
  `AgentControl` is deliberately not yet in the generated proto
  (`proto/compass/v1/agent.proto:73-85`: "It is deliberately NOT
  defined in this PR"), so the variant lands with the decoder. Secrets a tool
  re-reads per use (git credentials — read by git's `store` helper on every
  network op; the provider seed — read per LLM call via `getApiKey`) rotate
  with no notify at all once rewritten. **Caveat (git token write side):**
  the forge token arrives via `AgentSpec.Workspace.Credentials` at provision
  (`agent.go:238`, `workspace.go:113-151`) — it is not a `ResolvedSecret` and
  no rotation path reaches it. **MVP scope: git-token rotation is OUT** — a
  stale forge token is refreshed by the throwaway-container restart path
  (re-provisioned with fresh `Credentials`); folding the forge credential
  into the declared store is the post-MVP path.
- **Env-shaped secrets rotate for subsequent execs; only captured env needs
  a restart.** Env delivery is itself file-backed: values live in the 0600
  aggregate env file each wrapped exec consumes at spawn time (T5's
  `--env-file`), so rewriting that file rotates env secrets for every
  *subsequent* exec with no restart. What cannot be mutated is the
  environment a process **already captured** at its own exec time — for a
  long-lived process holding a stale env value, rotation falls back to the
  **throwaway-container restart** path the v0.6 record already defines for
  image updates: "stop, restart on the new image, transcript replayed"
  (`compass-0.6/design.md:886-888`) — the replay barrier
  (`control.ts:10-12`) makes the restart loss-free. (The agent process
  itself consumes no declared env secret — provider creds ride the seed —
  so in practice the restart case is a user tool the agent left running.)
- Consequence: **prefer file delivery for anything expected to rotate**;
  env delivery still trails file delivery for long-lived consumers, which
  capture values at spawn.

**Placement set for MVP** (folded into Plan tasks):

- **git** — BUILT: `installCredentials` → `Workspace.CredentialSetupScript()`
  (`agent.go:256-276`, `workspace.go:113-151`). The canonical channel;
  unchanged.
- **provider (LLM)** — the AuthStorage seed above (T5), rotated live via
  seed rewrite + per-call `getApiKey` resolution.
- **gh** — a declared secret in the store with its own kind, `SecretGH`
  (T3): the registry row carries `kind = SecretGH` plus a `host` column
  (default `github.com`), so the T5 materializer routes it to
  `GHCredentials.SetupScript` — `~/.config/gh/hosts.yml` (0600, under
  scoped `$HOME`) for github.com **and** custom forges (with `GH_HOST` for
  the latter) — never to the generic `$HOME/.compass/secrets/<NAME>` path.
  File form is the default because it is the rotatable one *and* it avoids
  the env path's argv exposure (env values ride `-e KEY=VALUE` into podman
  argv today, `podman.go:594-596`/`:365-367` — see T5, the `--env-file`
  delta); `GH_TOKEN` env stays only for tools that can consume gh from env
  alone.
- **generic declared secrets** (DB URLs, API tokens) — file under
  `$HOME/.compass/secrets/<NAME>` or env, per the store-registered
  `DeliveryKind` (T3/T5).

### Decision 4 — no in-container supervisor for MVP

The container keeps `sleep infinity` as its main process and the Runner keeps
driving the agent via exec from outside ("the agent is driven via exec, not
as the container's main process", `agent.go:213-215`). The Runner (Go,
outside) owns lifecycle and secret placement; the TS agent
(`packages/compass-agent`) stays a pure OMP-SDK/frame process, speaking typed
`AgentFrame`/`AgentControl` over the built streaming-exec stdio pipes
(`podman.go:289-296`).

**Named FUTURE seam — the in-container Go supervisor (explicitly unbuilt):**
a small Go binary as PID 1 that owns secret-watch (inotify on the secret
files + originating the re-read notify) and agent lifecycle (spawns the TS
agent as a child, restarts it on crash), to cut the Runner out of the
per-agent hot path at scale, post-dogfood. Recorded rationale:

- **A supervisor cannot be the process it restarts** — restart authority must
  sit outside the restarted process: today the Runner restarts the agent;
  later the supervisor restarts the agent and the Runner restarts the
  supervisor.
- **Privileged secret placement must not share a process with model-driven
  agent code** — placement stays in the Runner (MVP) or the supervisor
  (future), never in the TS agent the model steers.

The MVP interfaces are shaped so the supervisor slots in without redesign:
secret files land at stable paths under `$HOME` (the `SecretMaterializer`
contract, T5), and the notify is a control-stream frame the supervisor could
equally originate.

### Decision 5 — Setup agent guides secrets onboarding

Devtools onboarding is already the Setup agent's job (`compass.md:164`; the
naming table entry: "authors a repo's `devenv` config so Compass can build
per-agent container images (§5.5)", `compass.md:353` — the image half of that
sentence is superseded by Decision 1; the authoring role stays). Secrets are
the remaining high-friction surface, so the Setup agent additionally walks the
user through **populating the store** — it authors **no in-repo secrets
file** (there is none, Decision 2):

1. Reports which expected secrets are set/unset (the Server's value-free
   status surface, T7 `ListSecrets`) and proposes names for what the repo's
   tooling appears to need.
2. Directs the user to the Compass CLI / Bridge UI entry flow (`SetSecret`,
   T7) for each missing value.
3. Detects an existing 1Password/pass/gopass store and suggests wiring it as
   the SecretSpec provider instead — provider `ref` fields can read
   credentials other applications already stored (docs `providers/keyring.md`,
   "Use existing secrets").

The Setup agent never sees secret values **in its transcript or
conversation**: it drives the Server's value-free status RPC, and value
entry happens in the Client UI / CLI directly against the Server — values
stay out of every transcript by construction (D14 redaction,
`compass-0.5/design.md:540-542`). (As a launched agent it does receive the
inject-all store as 0600 files like any other agent — the claim is
transcript-scoped, not container-scoped.)

### Spec impact — supersession, by citation

Per the sealed frozen-record convention, merged records are never edited;
this record supersedes specific claims by citing them, and the **impl PR's
living-spec update** carries the replacement into
`docs/specs/product/compass.md`:

- **`docs/designs/product/compass.md` §5.5 (`compass.md:157-168`)** — the
  per-repo image model is superseded: "the container image is built from a
  `devenv` environment" (`:159`) and "the image is the composed `devenv
  container build` output" (`:163`) give way to the single self-contained
  base image + agent-managed in-container activation. The build-sandbox
  posture (`:166`: "no ambient credentials, egress limited to the Nix
  substituter cache") is **also superseded**: there is no image build from
  repo devenv anymore, and under the trusted-agent threat model the agent
  evaluates repo Nix as itself, with its credentials and its (default-open)
  egress. **Kept intact:** the Setup-agent authoring flow (`:164`).
- **`compass.md` §5.3/§6.2/§6.7 default-deny egress (`:143`, `:201`,
  `:252`)** — "a default-deny egress allowlist at the container layer" is
  superseded **as the MVP default**: the mechanism remains built and armed,
  but the shipped ruleset is allow-all; default-deny/per-agent allowlists
  become the future opt-in restriction tier (consistent with the hardening
  tier `compass.md:378` already names for stricter egress). **Accepted cost
  (explicit):** the frozen spec leans on the closed allowlist as the
  structural floor beneath the hook gates — §6.2: "the per-agent
  container's default-deny egress allowlist (§5.3) bounds *every* outbound
  connection, hook-mediated or not … the container's allowlist is the
  structural floor beneath them" (`compass.md:201`), and §6.7:
  "exfiltration to an arbitrary host is blocked even when no hook fires"
  (`compass.md:252`). Under default-open MVP egress that floor is absent:
  egress synthesized inside an already-approved subprocess is **uncovered**
  until the opt-in restriction tier is enabled. The hook-level gates
  themselves keep working; what they lose is the container backstop
  beneath them.
- **`compass.md` §5.3 clone-per-container (`:145`)** — "created inside the
  container at startup" is adjusted, not replaced: clones are still
  per-container and in-container, but **the agent creates them**, not the
  Runner's provision step; multi-repo workspaces (`:147`) follow naturally.
- **`compass-0.6/design.md:862-888`** — the config-distribution mechanics are
  extended, not replaced: Runner-mediated pull, `Sessions` version signal,
  and read-only-mount swap stay for config; this record adds the secret
  fetch + placement channel beside them. Superseded within that range: "The
  agent binary and base image ride **versioned OCI pulls**" (`:886-887`)
  now applies to the **single base image only** — there is no per-repo image
  to pull.
- **v0.5 D14 (`compass-0.5/design.md:528-546`)** — "per-user / per-agent
  authorization on secret access" is **deferred in both halves, not
  dropped**: MVP inject-all applies no per-agent grants (the named-future
  seam, Decision 2) **and no per-user scoping** — the T3 registry and
  `FetchSecrets` carry no user dimension, so on a multi-user Server the whole
  store would reach every agent regardless of owner — hence the **ratified MVP
  constraint of a single-user Server** (OQ7, ruled by Matt; user scoping is the
  named post-MVP path).
  Encryption at rest, rotation/revocation, per-container isolation of
  delivered secrets, and redaction are delivered as specified.
- **Living spec** — `docs/specs/product/compass.md`'s "Not yet specified"
  tail (`compass.md:611` in the spec) does not yet name the container
  runtime; the impl PR adds agent-container-runtime requirements (base
  image, agent-managed activation, secret store, distribution/rotation,
  default-open egress) as new spec sections, leaving frozen design prose
  untouched.

## Alternatives considered

Alternatives to the five decisions themselves are settled and not re-argued;
recorded here are the rejected shapes inside each mechanism:

- **Per-repo prebuilt images (status quo, `image.go:10-13`)** — rejected for
  MVP: couples agent updates to repo rebuilds, costs a full `devenv container
  build` + skopeo load per repo per change, and the build product is
  node-local anyway. Named FUTURE: a prebuilt image as a warm cache in front
  of in-container activation, same activation contract.
- **Shared host Nix store (read-only lower + per-container overlay upper)** —
  rejected: it assumes Nix on the host (Compass must run without it), drags in
  SELinux relabel hazards on the store mount (`mountArg` unconditionally
  appends `:Z`, `podman.go:605-609`), uid-mapping reconciliation under
  `--userns=keep-id` (`podman.go:357`), and Nix-DB validity coupling — all
  complexity purchased for a warm-cache latency win the public binary cache
  already approximates. A self-contained image is boring and portable.
- **Per-agent podman named volume for `/nix` (warm cache)** — acknowledged,
  not chosen for MVP: a named volume persisted across containers would keep
  each agent's realized closure warm (cutting the cold-realize launch cost
  under Decision 1) with no host-Nix dependency — materially different from
  both the rejected host-store overlay and the FUTURE prebuilt image.
  Deferred because it adds per-agent volume lifecycle (GC, versioning
  against base-image Nix upgrades) the MVP doesn't need; named as the first
  knob if launch latency hurts in dogfood.
- **`podman secret` for secret delivery** — rejected: podman secrets mount
  at container **create** time only — no path adds or rotates one on a
  running container, forfeiting the live-rotation contract (T6).
- **Eval-uid sandbox + captured-env indirection (the prior version of this
  record)** — rejected on the threat model: the agent is trusted with its
  credentials, so isolating Nix evaluation *from the agent's own secrets*
  bought no security and cost an in-image second user, cross-uid ownership
  dances, a capture-env file contract, and a two-phase egress re-arm.
- **Bespoke Compass encrypted secret store** — rejected: v0.5 D14 needs
  encryption at rest, rotation, redaction (`compass-0.5/design.md:533-542`);
  SecretSpec + provider gives them off the shelf, with pluggable backends
  Compass would otherwise re-implement one by one.
- **Running SecretSpec inside the container** — rejected: it would hand every
  agent provider access (keyring/1Password reachability + provider
  credentials) — a *store*-level blast radius, beyond the agent's own
  resolved values. Trusted-agent does not mean store-admin. Containers get
  resolved values only.
- **Provider creds as a declared env/file secret** — rejected: the SDK has a
  first-party per-call credential surface (`getApiKey`,
  `agent.d.ts:66-70`) that gives live rotation and model-scoped routing for
  free; an env var can't live-rotate and a bespoke file contract would
  reinvent `AuthCredential` (`auth-storage.d.ts:16-24`).
- **Secret values riding the `Sessions` signal** — rejected: the signal is a
  version notification; carrying values would put secrets on a broadcast-ish
  control path and into any stream logging. Fetch stays a dedicated
  authenticated unary pull.
- **Read-only mount swap for rotatable secret files** — considered (it is the
  config mechanism, `compass-0.6/design.md:870-877`) and not chosen for
  secrets: mount swap requires Runner-host files (secrets at rest on the host
  disk) and mount churn; stdin-rewrite keeps secrets container-side and
  reuses the exact built posture on a running container.
- **Supervisor in the MVP** — rejected per Decision 4; the rationale pair
  (restart authority, privilege separation) is recorded there.

## Plan

Tasks are ordered by dependency; each carries its own test cycle. Exact
signatures are the contract; field names may be refined in review but arity
and placement may not.

### T1 — Base image: one self-contained agent base

Define and build the single base image: its own Nix (flakes enabled,
single-user, `/nix` owned by the agent uid), devenv, direnv, bun + the bundled
`compass-agent`, git, gh, and nft/getent/awk (required in-image by the egress
script, `egress.go:76-77`). Published as the one versioned OCI artifact the
Runner pulls (`compass-0.6/design.md:886-888` restart path). No host mounts,
no host-Nix dependency. `ImageBuilder` + `IsContainerReady`
(`image.go:82-89`) retire from the launch path (kept only if the FUTURE
prebuilt cache revives them — flag for deletion in the impl PR if nothing
else references them).

- Interfaces:
  - Nix: a `compass-agent-base` package/image attribute in the repo flake
    (exact attr path fixed in the impl PR; nix is declarative config, not a
    Go surface). The image bakes the agent uid's passwd entry, its `$HOME`
    skeleton, and agent-uid ownership of `/nix`.
  - **Uid mapping (load-bearing):** the baked agent uid must be the uid the
    container actually runs as. Today's create path uses bare
    `--userns=keep-id` (`podman.go:355-357`: "Rootless uid mapping: files
    the agent writes in a bind-mount map back to the invoking user" /
    `"--userns=keep-id"`), which maps the **host Runner uid** into the
    container — if that differs from the baked uid, the agent does not own
    `/nix` and rebuild-own-devenv fails. Fix: T2 switches create to
    `--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>` so the host Runner
    uid appears in-container **as** the baked uid; the invariant is
    baked-uid == `Workspace.UID` == the mapped in-container uid, pinned in
    one config constant and asserted by the T8 agent-owns-`/nix` test.
  - Go: `AgentSpec.Image` (`agent.go:35-36`) now always carries the base
    image ref supplied by the Runner's config, never an `ImageBuilder.Build`
    result.
- Consumes: nothing. Produces: the image every container starts from.

### T2 — Launch path: no clone, default-open egress, direnv exec wrapper

Simplify `provision` (`agent.go:232-242`) to exactly: `armEgress`
(default-open) → `installCredentials` → **new secret-materialization steps
(T5)**. Delete `cloneRepo` (`agent.go:278-289`) and the `Workspace` clone
surface; add a root pre-step that creates the agent-owned workspace root.
Wrap Runner-driven agent execs in `direnv exec <WorkDir>` so they pick up the
workspace-root `.envrc` when the agent has authored + allowed one (Decision 1).

- Interfaces:
  - `Workspace` (`workspace.go:69-85`) drops `Source`, `Branch`, and
    `CloneCommand()` (`workspace.go:87-97`); `CheckoutDir` becomes
    `WorkDir string` — the agent-owned workspace root the root pre-step
    `mkdir -p` + `chown`s at provision, the exec workdir
    (`agent.go:176-181`), and the OMP-SDK session's cwd (`agent.go:63-65`
    re-pointed). `RepoSource` and its tests retire.
  - `func AllowAllEgress() EgressPolicy` — the default-open policy; its
    `NftScript()` still installs the `compass_egress` table and chain
    (arming stays fail-closed and root-only, `egress.go:76-79`) with an
    accept default instead of the drop policy + allowlist sets
    (`egress.go:109-131`). `AllowEgress` (`egress.go:40`) stays as the
    future opt-in restriction path; no user network config exists in MVP.
  - `Create` (`podman.go:347-357`) switches `--userns=keep-id` to
    `--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>` — the T1 uid-mapping
    invariant (baked uid == `Workspace.UID` == mapped in-container uid).
  - `func (r *AgentRuntime) ExecAsAgent(ctx, handle, command ...string)` —
    unchanged signature; the built argv becomes
    `direnv exec <WorkDir> -- <command...>` (bare argv today,
    `agent.go:176-181`). `direnv` ships in the image (T1). The wrapper
    resolves the `.envrc` at `WorkDir` (the root convention, Decision 1)
    when the agent has authored + `direnv allow`ed one, else the base
    toolchain — it makes no per-repo claim; per-repo devenvs are the
    agent's own in-repo invocations. The exec additionally passes the T5
    aggregate env file via `--env-file` (a new exec-spec env-file field,
    `podman.go` delta) so declared env secrets reach the process without
    riding `-e` argv. The streaming exec that starts the agent itself
    (`podman.go:289-296`) takes the same wrapper so the agent process sees
    its activated env.
- Consumes: T1. Produces: a launched container the agent owns end to end —
  clones, envs, and rebuilds are the agent's.

### T3 — Server secret store: `internal/secrets` + names registry

New Server package wrapping SecretSpec resolution behind an interface, plus
the **names-only registry** that defines the declared set (Decision 2 — no
repo manifest, no grants table). All types redact like `Credentials`
(`workspace.go:60-67`).

- Interfaces:
  - `type ResolvedSecret struct { Name string; Value string; Version string; Delivery DeliveryKind; Kind SecretKind; Host string }`
    with `func (s ResolvedSecret) String() string` /
    `func (s ResolvedSecret) GoString() string` redacting `Value`. `Host`
    is set only for `SecretGH` rows (empty otherwise). **`Version` is
    produced by the Resolver as a content hash (SHA-256, hex) of the
    resolved value** — the registry stores no values and SecretSpec resolve
    returns values, not versions, so the hash is the only deterministic
    producer. Consequence (explicit): re-setting a secret to the **same
    value** yields the same `Version`, so T6's diff sees no change and
    triggers no rotation — correct, since nothing the container holds is
    stale.
  - `type DeliveryKind uint8` — `DeliveryFile` / `DeliveryEnv` (the
    load-bearing split), stamped from the registry row at declaration time.
  - `type SecretKind uint8` — `SecretGeneric` / `SecretProvider` /
    `SecretGH`; provider rows additionally carry `Provider string` (the SDK
    provider id) so T5 routes them to the AuthStorage seed, never the
    generic channels; `SecretGH` rows carry `Host string` (default
    `github.com`) so T5 routes them to `GHCredentials.SetupScript`
    (Decision 3's gh placement), never the generic file path.
  - `type Resolver interface { Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error); Set(ctx context.Context, name, value string) error; Delete(ctx context.Context, name string) error }`
    — `Resolve` resolves the **whole registry** (inject-all; a
    `names []string` parameter returns with the future grants seam);
    `Set`/`Delete` are the provider **write** path T7's
    `SetSecret`/`DeleteSecret` require — the Go SDK is read-shaped (builder
    → `Load()` → `Get()`/`SetAsEnv()`; upstream writes are CLI-only,
    `secretspec set`), so the write half shells to the pinned `secretspec`
    CLI with the **value over stdin, never argv** (OQ2). `reason` feeds the
    SecretSpec audit log. The resolver generates the SecretSpec manifest
    from the registry (one Compass project, one profile) and writes it
    under the Server's own state directory — the SDK builder takes
    provider/profile/reason, not a manifest path, so the resolver runs with
    that directory as its manifest root. Server state, never repo state.
  - Store: `func (s *Store) DeclareSecret(ctx context.Context, actor AccountID, name string, delivery DeliveryKind, kind SecretKind, provider, host string) error`,
    `func (s *Store) DeleteSecretDeclaration(ctx context.Context, actor AccountID, name string) error`,
    `func (s *Store) DeclaredSecrets(ctx context.Context) ([]SecretDeclaration, error)`;
    migration `migrations/0002_secrets.sql` (name UNIQUE, delivery, kind,
    provider, host, declared_by, timestamps — **no value column**).
  - **Name validation (load-bearing):** a declared name is validated against
    `^[A-Za-z_][A-Za-z0-9_]*$` (SecretSpec's env-var-name grammar) at
    `DeclareSecret` time — it later becomes a path under
    `$HOME/.compass/secrets/` and a line in a root-adjacent script (T5), so
    it is validated at the door, not escaped downstream.
- Consumes: `internal/store` (`store.go:36-38`), SecretSpec Go SDK.
  Produces: the Server-side resolve surface T4 serves from.

### T4 — Secret fetch + rotation signal on `RunnerService` (held-for-review contract delta)

The `.proto` delta is **named here, not written**. `RunnerService` now exists
in the Go tree — `proto/compass/v1/runner.proto:42` defines it with
`Enroll`, `Sessions`, and `PublishEvents` (`:50`, `:61`, `:70`). This record
adds `FetchSecrets` + a `SecretsVersion` signal to that existing service as a
contract delta: it rides its implementation PR, and the schema edit lands only
after `compass.v1`-owner + maintainer review:

- Interfaces (held for review):
  - `rpc FetchSecrets(FetchSecretsRequest) returns (FetchSecretsResponse)` on
    `RunnerService` — request: `session_id` only (**no names filter, no
    grants** — inject-all; the filter parameter is the named future
    per-agent-scoping seam). Response: resolved
    name/value/version/delivery/kind tuples for the whole store.
    Authenticated by the per-Runner token (`RunnerService` rejects account
    tokens, `go-toolchain-default.md:970-971`); the Server verifies the
    requested `session_id` is bound to *this* authenticated Runner via the
    platform record's **T9** session registry ("session id → owning Runner",
    `go-toolchain-default.md:1007-1011`) and rejects a foreign session with
    `CodePermissionDenied`. With inject-all there is no per-agent secret
    differentiation to protect beyond that Runner-binding check; the
    stricter agent-identity plumbing returns with the grants seam.
  - A `SecretsVersion { session_id, version }` server→runner message added to
    the existing `Sessions` stream's `SessionsResponse` command oneof
    (`runner.proto:115-117`), sibling to the config-version signal
    (`compass-0.6/design.md:874-875`) — signal only, never values.
    The Runner re-fetches on signal and re-materializes (T6).
  - Server handler: `func (s *RunnerServiceHandler) FetchSecrets(ctx context.Context, req *connect.Request[runnerv1.FetchSecretsRequest]) (*connect.Response[runnerv1.FetchSecretsResponse], error)`
    delegating to `secrets.Resolver`.
  - **No-log posture:** `FetchSecretsResponse` is marked no-log (a
    `debug_redact` field option or an interceptor skip) so a connect/gRPC
    logging interceptor can never dump resolved values.
- Consumes: T3; the T8 `RunnerService` seam from the platform record.
  Produces: the wire surface T5/T6 pull through.

### T5 — Runner secret materializer: AuthStorage seed + gh + declared secrets

The Runner-side half: turn fetched secrets into container state, copying the
git posture exactly (stdin `sh -s`, 0600, scoped `$HOME`, never argv —
`agent.go:264-270`, `workspace.go:143-150`). git stays as built. New
placements: the **provider-credential seed** (Decision 3), gh, and generic
declared secrets (file under `$HOME/.compass/secrets/<NAME>` or env, per
`DeliveryKind`).

- Interfaces:
  - `type ProviderSeed struct { Entries map[string]ProviderSeedEntry }` —
    provider id → credential, serialized to a 0600
    `$HOME/.compass/auth-seed.json`;
    `type ProviderSeedEntry struct { Type string; Key string }` (api-key MVP;
    the field set mirrors the SDK's `ApiKeyCredential`,
    `auth-storage.d.ts:16-20`, so an OAuth extension is additive). Both
    redact via `String`/`GoString`.
    `func ProviderSeedScript(homeDir string, seed ProviderSeed) (string, error)`
    — write-temp + atomic `mv`, umask 077; the serialized seed JSON is
    base64-embedded and decoded in-container (the same delimiter-independent
    transport used for file-secret values), never shell-interpolated.
  - TS (entrypoint, `packages/compass-agent` consumer): the container
    entrypoint constructs
    `new Agent({ getApiKey })` where `getApiKey` resolves the model's
    provider from the seed file (mtime-cached re-read, so a rewritten seed
    is picked up on the next LLM call — `agent.d.ts:66-70` semantics), and
    passes it as `CompassAgentOptions.agent` (`agent.ts:27-31`).
  - `type GHCredentials struct { Host string; Token string }` (redacting);
    `func (g GHCredentials) SetupScript(homeDir string) (string, error)` —
    `~/.config/gh/hosts.yml` + `GH_HOST` for custom forges.
  - `type SecretFile struct { Name string; Path string; Value string }` (redacting);
    `func SecretSetupScript(homeDir string, files []SecretFile) (string, error)`
    — one script, write-temp + `mv`, umask 077. **Script-safety invariants
    (load-bearing):** names were validated at declaration (T3) and are
    re-checked here (defense in depth) with a cleaned-prefix check that the
    resolved `Path` lies under `$HOME/.compass/secrets/`; **values are never
    interpolated into shell source** — a file value is base64-encoded on the
    host (Go) and embedded in the setup script as a single-quoted literal,
    decoded in-container to its 0600 file (`printf %s '<b64>' | base64 -d`).
    The base64 alphabet (`[A-Za-z0-9+/=]`) contains no single quote and no
    newline, so inside the single-quoted literal the value cannot break out,
    terminate the script framing, or become shell source — the transport is
    **delimiter-independent**, so a value whose own bytes form a delimiter
    line is inert. Env values are written as `KEY=VALUE` lines into a 0600
    aggregate env file at `$HOME/.compass/env` (a **sibling** of the
    `secrets/` dir, so no secret named `env` can collide with it), rejecting
    values with newline/NUL (the env-file line grammar).
  - `type SecretMaterializer struct { runtime ContainerRuntime }`;
    `func (m *SecretMaterializer) Install(ctx context.Context, handle *AgentHandle, secrets []secrets.ResolvedSecret) error`
    — routes by `Kind`: provider → `ProviderSeedScript`; gh (`SecretGH`) →
    `GHCredentials.SetupScript` (using `ResolvedSecret.Host`); generic
    file → per-secret 0600 files; generic env → the aggregate env file.
    Env values
    do **not** ride exec `Env` maps — that emits `-e KEY=VALUE` into podman
    argv (`podman.go:594-596`, `Create` `:365-367`), host-process-list
    visible. Env delivery has exactly **one** path: wrapped execs pass the
    aggregate file via `podman exec --env-file <0600-path>` (a new
    exec-spec env-file field, `podman.go` delta; T2), never `-e` — there is
    no source-in-wrapper variant ("source the file" is not expressible in
    T2's bare argv exec, and two delivery paths would fork the rotation
    semantics).
  - `provision` (`agent.go:232-242`) gains `installSecrets` (the
    materializer) after `installCredentials`.
- Consumes: T2 (order in `provision`), T4 (fetch). Produces: a container
  whose agent can reach its model and forge tooling.

### T6 — Rotation: file rewrite + `SecretsChanged` control op; env regen + captured-env restart

Wire the rotation loop end to end: `Sessions` `SecretsVersion` signal →
Runner re-fetch (T4) → diff by `Version` (the T3 content hash; a same-value
re-set hashes identically and is a no-op) → provider: seed rewrite (picked
up per LLM call via `getApiKey`, no notify strictly needed, one sent anyway
for observability); file-shaped: rewrite via `SecretSetupScript` + emit the
control-stream re-read op; env-shaped: regenerate the aggregate env file
(write-temp + `mv`), which rotates the value for every **subsequent** exec
(T5's `--env-file` is read at spawn) — the **throwaway-container restart**
(stop → relaunch → transcript replay, `compass-0.6/design.md:886-888`,
reusing the replay barrier, `control.ts:10-12`) remains only for a
long-lived in-container process that already captured the stale env value;
the agent process itself consumes no declared env secret (provider creds
ride the seed), so the restart is a per-case call, not an automatic step.

- Interfaces:
  - TS: `AgentControl` union (`control.ts:30-53`) gains
    `{ readonly kind: "secretsChanged"; readonly names: readonly string[] }`;
    `CompassAgent.#applyControl` re-reads/announces per name (names only,
    never values).
  - Proto (held for review, additive): a seventh `AgentControl` oneof
    variant `SecretsChanged secrets_changed = 7` — an amendment to the frozen
    six-variant oneof (`control.ts:6-12` — authoritative; the
    `agent.proto:73-85` comment is stale, listing five variants — OQ4).
  - Go (Runner): `func (m *SecretMaterializer) Rotate(ctx context.Context, handle *AgentHandle, fresh []secrets.ResolvedSecret) (changedFiles, removedFiles []string, needsRestart bool, err error)`.
    **Deletion cleanup (load-bearing):** a secret deleted from the store
    must not leave its stale 0600 file usable in a running container. The
    removal set is computed against **ground truth, not an in-memory
    manifest**: the installed set is what is physically present in the
    container's secret dir (`ls $HOME/.compass/secrets/` over the exec
    seam), and the should-exist set is the fresh `FetchSecrets` result.
    `Rotate` deletes (truncate then `rm`) every per-secret file whose name
    is not in the fresh file-delivery set; the aggregate env file, the
    provider seed, and the gh `hosts.yml` are **fully regenerated from
    `fresh`** on every rotation (write-temp + `mv`), so a deleted env
    secret, provider credential, or `SecretGH` entry is gone because the
    file is rebuilt from the current grant set, not edited. gh credentials
    are the one delivery target outside `$HOME/.compass/secrets/` (they land
    at `~/.config/gh/hosts.yml` via `GHCredentials.SetupScript`, T5), so the
    dir-scan removal never reaches them: `hosts.yml` is reconciled by this
    full-regeneration path — every rotation rewrites it from the fresh
    `SecretGH` rows and removes it when none remain, so a revoked or
    host-changed token cannot survive in the running container. Reading the container's
    own dir makes deletion correct across a Runner restart + reattach (an
    in-memory manifest would rebuild empty and never delete).
- Consumes: T4, T5, the built control seam — **and, cross-record, the
  parked `AgentControl` stdin decoder**: "`AgentControl` is NOT in ./gen
  and its concrete stdin decoder is a parked follow-up (stacked PR once the
  payload shape is ruled)" (`control.ts:17-21`; `agent.proto:73-85`). Until
  that stacked PR lands, **no `secretsChanged` frame reaches a running
  agent** — T6's file rewrite still lands (per-use re-readers pick it up),
  but the notify half is blocked on the decoder. Produces: live rotation
  for provider + file-shaped secrets (install, rewrite, **and delete**); a
  defined restart path for env-captured consumers.

### T7 — Secrets entry + status surface (CLI / Bridge UI / Setup agent)

The Server RPC surface behind the Compass CLI and Bridge UI (the store's only
write path) and the Setup agent's value-free status view (Decision 5):

- Interfaces (held for review where proto):
  - `rpc ListSecrets(ListSecretsRequest) returns (ListSecretsResponse)` —
    the registry's names + per-secret set/unset status, **never values**
    (via SecretSpec's value-free report path if the Go SDK exposes one —
    only the Rust-side `Secrets::report()` is grounded upstream; fallback:
    the pinned CLI's `secretspec check`, which reports set/unset without
    printing values). Callable by user and agent tokens (the
    Setup agent drives this).
  - `rpc SetSecret(SetSecretRequest) returns (SetSecretResponse)` — declares
    (registry row, T3) and writes the value through the T3 `Resolver.Set`
    provider write path (CLI-backed, value over stdin, never argv —
    T3/OQ2); value redacted from logs/audit by construction.
    `rpc DeleteSecret(DeleteSecretRequest) returns (DeleteSecretResponse)` —
    removes the registry row + provider entry (`Resolver.Delete`) and bumps
    the secrets version (triggering T6 deletion cleanup in live
    containers).
    **Both enforced user-only (load-bearing):** "Client-only" is not
    enforceable by a generic authenticated-account check — a user token and
    an agent token are both account subjects, so the Setup agent's own token
    would pass. `SetSecret`/`DeleteSecret` MUST explicitly require the
    caller's account kind to be User and reject an agent account with
    `CodePermissionDenied` (the same fail-closed posture as the admin-gated
    `IssueToken`, whose non-admin caller gets `permission_denied` —
    `gen/compass/v1/compassv1connect/compass.connect.go:93`). A regression
    test asserts an agent-token `SetSecret` is `CodePermissionDenied`.
  - Go: `func (s *SecretsService) ListSecrets(...)` /
    `func (s *SecretsService) SetSecret(...)` /
    `func (s *SecretsService) DeleteSecret(...)` (connect handlers over T3);
    a `compass secrets set/list/delete` CLI verb over the same RPCs.
- Consumes: T3. Produces: the onboarding loop Decision 5 describes and the
  store's sole write path.

### T8 — Red-first integration tests

Per rule red-green: BDD/integration tests written first, watched failing,
then implementation. Gated behind podman-available skips like the existing
lifecycle tests (`internal/runtime/lifecycle_test.go`).

- Interfaces: test names, not code —
  - launch shape: `Launch` against the base image performs no clone; the
    container has an empty agent-owned `WorkDir`, an armed `compass_egress`
    table, and installed git credentials; an `ExecAsAgent` reaches an
    arbitrary external host (default-open egress).
  - agent-managed devenv: as the agent uid, clone a fixture repo with a
    trivial `devenv.nix` into `WorkDir`, `direnv allow` it, and assert a
    devenv-provided tool resolves through the wrapped `ExecAsAgent` — and
    that the agent uid can rebuild after editing `devenv.nix` (write access
    to `/nix`).
  - provider seed: materialize a seed, boot the entrypoint's `getApiKey`
    resolver against it (bun test), assert per-call resolution; rewrite the
    seed and assert the next call picks up the new key with no restart.
  - secrets placement: fetch → install; probe 0600 + ownership + values via
    exec; assert no value ever appears in `podman` argv (process-list probe)
    nor in any log line (redaction test on `ResolvedSecret`, `ProviderSeed`,
    `GHCredentials`, `SecretFile`).
  - script safety: a declared name with `../`, a separator, or a newline is
    rejected at declaration and again at materialization; a secret *value*
    containing arbitrary bytes — a lone delimiter-style line, a newline,
    `$(...)`, `;`, or a single quote — is written verbatim to its 0600 file
    with no command execution (base64 transport) and no second `KEY=` line in
    the aggregate env file; an env value with newline/NUL is rejected.
  - fetch authz: a Runner requesting a `session_id` bound to a different
    Runner gets `CodePermissionDenied`.
  - rotation: rewrite a file secret, assert the `secretsChanged` frame
    reaches a stub ControlSource consumer; env rotation regenerates the
    aggregate env file and a subsequent wrapped exec sees the new value; a
    same-value re-set produces no rotation (identical content-hash
    `Version`).
  - deletion: delete a secret from the store → `Rotate` removes the stale
    0600 file (post-rotate exec cannot read it) and returns it in
    `removedFiles`; the same holds after discarding the in-memory
    `AgentHandle` (simulated Runner restart + reattach) — proving the
    removal set reads the container dir, not a cached manifest; a rotation
    that deletes a file secret while an env secret remains regenerates the
    aggregate env file (sibling of `secrets/`, never scanned) with the
    still-declared env value intact; deleting the last `SecretGH` row
    regenerates `~/.config/gh/hosts.yml` to empty (the stale host/token is
    gone from the running container) while a still-declared file secret
    stays readable — proving the gh file is reconciled by full regeneration,
    not the `secrets/` dir-scan.
  - write-authz: an agent-token `SetSecret` / `DeleteSecret` is
    `CodePermissionDenied`; a user-token call succeeds.

## Tasks

- [ ] T1 — Self-contained base image (own Nix + store, agent-uid-owned;
      devenv/direnv/bun/compass-agent/git/gh/nft in-image; Runner config
      carries the ref)
- [ ] T2 — Launch path: delete `cloneRepo` + `Workspace` clone surface,
      `WorkDir`, `AllowAllEgress` default-open policy, `direnv exec` wrapper
- [ ] T3 — `internal/secrets` Resolver over SecretSpec + `secrets` names
      registry migration (no values, no grants)
- [ ] T4 — `FetchSecrets` RPC (inject-all, no filter) + `SecretsVersion`
      Sessions signal (held-for-review `RunnerService` delta)
- [ ] T5 — `SecretMaterializer`: AuthStorage seed + `getApiKey` entrypoint
      wiring, gh + declared-secret placement, stdin-exec posture
- [ ] T6 — Rotation loop: seed/file rewrite + `secretsChanged` control op
      (TS + held proto variant); deletion cleanup; env regen → restart only
      for captured env
- [ ] T7 — Secrets entry/status RPCs (`ListSecrets` / `SetSecret` /
      `DeleteSecret`, user-only writes, held for review) + CLI verbs
- [ ] T8 — Red-first integration tests green; redaction + process-list
      probes pass
- [ ] Impl PR updates the living spec (`docs/specs/product/compass.md`) per
      *Spec impact*; frozen records untouched

## Global Constraints

- Go stack (`go`), idioms per
  `docs/designs/platform/go-idioms-and-libraries.md`; edit-over-create per
  compass `AGENTS.md`.
- **The agent is trusted with its credentials.** The container is per-agent
  blast-radius containment (host + inter-agent isolation) — never a mechanism
  to keep the agent from secrets it is handed. No task may reintroduce
  cred-avoidance machinery (separate eval identities, install-after-eval
  ordering, captured-env indirection, eval-phase egress narrowing).
- **Reuse the built stdin-`exec` credential posture for EVERY secret
  channel** (`agent.go:264-270`); a secret never appears in argv, an image
  layer, or a log line. Every secret-bearing Go type redacts under `%s`,
  `%v`, and `%#v` (the `workspace.go:60-67` pattern).
- **Secret redaction from transcripts + the audit log is mandatory** (v0.5
  D14, `compass-0.5/design.md:540-542`) — enforced by construction (values
  never enter agent-visible streams; SecretSpec's audit log never records
  values) and by test (T8).
- **Provider (LLM) credentials ride the OMP SDK auth surface only**
  (`getApiKey`, `pi-agent-core` `agent.d.ts:66-70`) — never the generic
  env/file secret channels, exec env maps, or argv.
- **Egress is default-open in MVP, mechanism retained**: `armEgress` stays in
  the provision sequence and the agent stays capability-less against the
  ruleset (`egress.go:6-10`); restriction is future opt-in config, never a
  user setup requirement.
- **Supersede by citation**; never rewrite the frozen `compass.md` §5.3/§5.5
  or the v0.6 record in place. The impl PR carries the living-spec update.
- **Design only for schema**: every `.proto` delta here (`FetchSecrets`,
  `SecretsVersion`, the `SecretsChanged` control variant, the secrets-entry
  RPCs) is a held-for-review interface — the schema edit lands in the impl
  PR after `compass.v1`-owner + maintainer review, additive,
  buf-breaking-gated.
- SecretSpec runs Server-side only; containers receive resolved values,
  never provider access or provider credentials. The store's write path is
  user-only (T7).
- Red → Green: BDD + unit tests first (T8 written before T2/T5/T6 land).

## Open Questions

Each carries a recommendation; **load-bearing** = a merge-blocker an executor
hits if unresolved. Forks the directives settle are folded into the record
above, not re-opened here.

1. **In-container Nix mode** (load-bearing for T1): single-user Nix with
   `/nix` owned by the agent uid vs a multi-user in-container `nix-daemon`.
   **Recommend: single-user, agent-owned** — one uid does all Nix work
   (matching agent-rebuilds-own-env), no daemon to supervise inside an
   exec-driven container (Decision 4 has no PID-1 to own it), and rootless
   podman maps one uid in — via `--userns=keep-id:uid=<agent-uid>` per T1's
   uid-mapping invariant (bare `keep-id`, `podman.go:355-357`, maps the
   host Runner uid in, so agent ownership of `/nix` only holds when the
   baked uid is the mapped one). Fallback if devenv proves daemon-dependent:
   a daemon socket-activated by the root arm step.
2. **SecretSpec integration surface on the Server** (load-bearing — T3's
   dependency choice): shell out to the `secretspec` CLI vs the Go SDK.
   **Recommend: the Go SDK** (`github.com/cachix/secretspec/secretspec-go`,
   0.13+ — the `secretspec-ffi` C ABI via purego, no cgo): same resolver,
   providers, and fallback chains as the CLI; in-process values (no secret
   transits a subprocess stdout); typed `MissingRequiredError`; no
   PATH/version-skew dependency. Two caveats shape the call: (a) the
   **write path is CLI-only regardless** — the SDK is read-shaped (builder
   → `Load()`); upstream writes are `secretspec set` — so the pinned CLI is
   in the Server's closure either way (T3 `Resolver.Set`/`Delete`), and
   "SDK" here decides only the read path; (b) the `secretspec-ffi` cdylib
   is **not on the Go module proxy** — the nix-built Server must stage
   `libsecretspec_ffi` in its closure or set `SECRETSPEC_FFI_LIB`, so FFI
   packaging fighting nix is the **likely** case to engineer for, not an
   edge. Fallback: the CLI for reads too — pinned in the Server's closure,
   values over a pipe, never argv.
3. **Provider-seed handoff shape** (load-bearing for T5's TS half): a JSON
   seed file the entrypoint's `getApiKey` resolver reads (recommended above)
   vs the Runner writing the SDK's SQLite store
   (`SqliteAuthCredentialStore.open`, `auth-storage.d.ts:1064-1067`)
   directly. **Recommend: the seed file + in-process resolver** — writing
   sqlite from a Go-driven shell script is fragile, the seed is
   atomic-rewritable over the built stdin-`exec` channel, and `getApiKey`'s
   per-call semantics (`agent.d.ts:66-70`) give live rotation either way.
   The sqlite store remains the natural upgrade when OAuth credentials
   (refresh state) arrive.
4. **`SecretsChanged` amends a frozen oneof** (load-bearing for T6): the
   `AgentControl` contract is frozen at six variants (`control.ts:6-12` —
   the authoritative statement; the `agent.proto:73-85` comment lists only
   five variants, missing `ask_answer`, and is stale — flag it for
   correction in the impl PR); this record needs a seventh. The proto
   message is deliberately not yet generated, so the addition is
   cheap **now** and expensive after the decoder lands. **Recommend: ratify
   the seventh variant with this record's review**, so the decoder PR
   implements the seven-variant oneof once. Regardless of timing, require
   the TS decoder's unknown-variant path to surface (count/log) an
   unrecognized control op rather than silently drop it — the codebase
   already has this discipline (the staged `askAnswer` arm is "never
   silently dropped", `control.ts:37-41`).
5. **Session-cwd contract under agent-managed clones** (not load-bearing —
   T2 picks a default): with no Runner-side clone, the OMP-SDK session's cwd
   is the workspace root `WorkDir`, not a repo checkout. **Recommend: fixed
   `WorkDir` (e.g. `/work`) as the session cwd**, with repo layout under it
   the agent's own convention; anything downstream that assumed
   cwd-is-a-git-repo (none found in `go` this run beyond the
   `agent.go:63-65` doc comment) must treat the cwd as a plain directory.
6. **Keyring reachability + the headless-deploy provider** — **RESOLVED by
   Matt in this design-PR review.** The concern (load-bearing for deployment
   defaults, not for code): the keyring provider needs an OS credential
   service reachable *and unlocked* from the Server process (Linux: Secret
   Service — gnome-keyring/KWallet, per the provider docs). A desktop can
   fail too (a pre-login systemd unit with no session bus, a locked keyring)
   and a headless box can succeed with a session keyring. **Ruling: keyring
   stays the default for the single-user desktop-local Server (the dogfood
   shape), accepting its interactive keychain unlock; container/headless
   deploys wire SecretSpec to a machine-authenticated provider — 1Password
   (service account), Vault/OpenBao (machine token), or a cloud secret
   manager (AWS Secrets Manager, GCP Secret Manager, Azure Key Vault) — a
   config-time provider choice, no code fork.** `pass`/`gopass` are **not**
   the headless answer: both are GPG-agent-based, so they carry the same
   interactive-unlock ceremony as keyring (its peers on the unlock problem,
   per the SecretSpec provider matrix), not a solution to the container
   case. State the keyring precondition and the first-resolve error surface,
   so an install fails loudly at first resolve rather than mysteriously.
7. **Inject-all has no user scope on a multi-user Server** — **RESOLVED by
   Matt in this design-PR review: option (a).** The concern: the T3 secrets
   table has no user dimension (name UNIQUE, delivery, kind, provider, host,
   declared_by — one flat store), and `FetchSecrets` returns the whole store
   on a `session_id` with only the Runner-binding check (T4), while the
   account schema *is* multi-user
   (`go/internal/store/migrations/0001_init.sql:27-38` —
   `user_accounts`, and `agent_accounts.owner_user_id NOT NULL`). So on a
   two-user Server, user A's secrets would reach user B's agents — beyond the
   trusted-agent premise, which trusts an agent with **its own user's**
   credentials, not every user's. **Ruling: (a) — a single-user Server is an
   explicit MVP constraint.** Dogfood and early user testing are all
   single-user / multi-agent workflows, so every agent receiving the whole
   store is accepted for the MVP. **(b) is the named post-MVP path**: an
   `owner_user_id` column on the secrets table, resolve session → agent →
   `owner_user_id` (`agent_accounts.owner_user_id`), and filter `FetchSecrets`
   to that owner's rows — addable without reshaping any other interface once
   multi-user support lands.
