# Compass host runtime tier

Status: Draft
Tracking: RIG-TBD (host runtime tier)
Owner: compass-runner (runtime) → compass-agent (onboarding review)

## Problem / Intent

A new user's adoption cost is dominated by getting their existing skills,
tools, and secrets reachable by a Compass agent. Today the lowest tier is
podman (DL-325), which still asks the user to project their environment into a
container before the first useful session. Meanwhile the agent they already run
— a CLI agent on their own machine — has all of it for free. This record adds a
**host tier**: a `host` backend that runs the agent as a plain host process
with the same access as the CLI agent the user already runs, making replication
of their existing setup near-zero-setup. The ruling: the tier is worth it even
if some users never graduate, because the counterfactual is the user staying on
their current CLI agent at the same host-exposure posture — a host-tier Compass
user is strictly better off.

The second half of the same onboarding story is the imported corpus itself: the
transport (`compass agent-config push --dir`) exists, but a user importing
their existing skills/rules cannot tell what overlaps or is superseded by
Compass's built-ins. A silently-shadowed skill looks like it worked — the worst
onboarding failure. This record designs an **agent-driven config-import
review** as part of the first-run flow, and the host tier is what makes it
land: the agent is already sitting where the user's config lives.

## Approach

### Half A — the `host` backend

#### Placement

The host tier is a third `SelectBackend` value, joining `""`/`podman`/
`microvm`. `SelectBackend` is the sole backend selection point
(`go/internal/runtime/microvm.go:117-125`):

```go
func SelectBackend(cfg BackendConfig) (ContainerRuntime, error) {
    switch strings.TrimSpace(cfg.Backend) {
    case "", "podman":
        return NewPodmanCLI(), nil
    case "microvm":
        return NewMicroVMRuntime(cfg.MicroVM), nil
    default:
        return nil, fmt.Errorf("runtime: unknown backend %q: accepted values are \"podman\" (default) and \"microvm\"", cfg.Backend)
    }
}
```

A `case "host"` arm returns the new backend and the error string extends to
name the third accepted value. The default stays podman: `host` is opted into
explicitly, never fallen back to.

On the DL-325 trust-model axis
(`docs/designs/DECISIONS.md:158`: "untrusted multi-tenant operation requires
the microVM hardware boundary (KVM, unchanged); self-host single-tenant
deployments keep podman as a permanent, supported entry tier"), the host tier
sits **below** podman: a self-host single-tenant onboarding tier for a user
running their own agents on their own machine. It is never valid for untrusted
or multi-tenant operation. Guidance stays "prefer container/microVM" — the
docs recommend graduating — but the tier is not gated or crippled to force it.

The tier's second motivation is host capability the container cannot provide
at all: workflows that need the user's real session bus, display, or
device access (e.g. window-management tooling driving the live desktop
session). See Open Questions — this record ships the tier for onboarding and
raises the permanent-host-capability framing as a question rather than ruling
it.

#### `ContainerRuntime` implementation

`ContainerRuntime` is frozen at S1 (`go/internal/runtime/podman.go:348-396`;
the closing comment at `:399`: "ContainerRuntime is frozen (the Resize
reservation above)"). The host backend implements it; it does not amend it.
The nine methods, per the interface doc comments, and their host-process
semantics — including where the mapping is degenerate:

| Method | Interface contract (quoted) | Host semantics |
| --- | --- | --- |
| `Create(ctx, spec) (ContainerID, error)` | "makes a container from spec without starting it, returning its id" (`podman.go:349-350`) | Allocates a per-agent **handle**: mints a synthetic `ContainerID`, creates the agent's private state dir (workspace root, home overlay dir, socket dir) from `ContainerSpec`. No process is spawned. Spec fields that configure container machinery (image, mounts as bind specs, network) are interpreted or ignored per a documented field map — see T1. |
| `Start(ctx, id)` | "starts a created container" (`podman.go:352-353`) | **Degenerate.** There is no init process to start; the agent process itself is launched later by `ExecStreaming`. `Start` transitions the handle `created → started` and validates the state dir. It must not be pretended to be more: a "started" host handle is bookkeeping, not a running boundary. |
| `Exec(ctx, id, spec) (ExecOutput, error)` | "runs a command in a running container, capturing its output. A non-zero exit is a successful runtime call returning a failed command" (`podman.go:355-359`) | Runs the command as a **direct host subprocess** of the Runner, under the Runner's own uid, with `ExecSpec`'s env/cwd/stdin and the per-command timeout. `ExecSpec.AsUser` is **degenerate**: there is no user switch — the process runs as whoever runs the Runner. The backend rejects (errors on) an `AsUser` naming a different uid rather than silently running it wrong. |
| `ExecStreaming(ctx, id, spec) (*StreamingExec, error)` | "starts a long-lived streaming command … returning its live stdio pipes plus a kill/wait handle" (`podman.go:361-369`) | The one clean mapping: spawns the agent as a host child process in its own process group, stdio piped, bound to ctx. This is where the host-tier agent actually comes to life. |
| `Stop(ctx, id, timeout)` | "stops a running container, allowing timeout for graceful exit" (`podman.go:371-373`) | Signals the handle's process group: SIGTERM, wait up to `timeout`, then SIGKILL. Scope is the process group the backend spawned — a host process the agent double-forked out of the group is **not reliably stopped**; that leak is named, not papered over (no cgroup freezer in v1; see Open Questions). |
| `Remove(ctx, id)` | "removes a container (force-kills if still running)" (`podman.go:375-376`) | Force-kills the process group if live, then deletes the handle's state dir. It does **not** touch anything outside the state dir — the agent's writes to the real host filesystem are permanent, which is the tier's declared posture, not a cleanup bug. |
| `Exists(ctx, name) (bool, error)` | "reports whether a container with name currently exists (any state)" (`podman.go:378-379`) | **Degenerate.** There is no container registry to consult; existence means "the backend has a handle (state dir) under this name". This is handle-existence plus process-liveness, not container-existence: a crashed agent whose state dir remains still `Exists`, mirroring a stopped-but-not-removed container. |
| `MountLabel(ctx, id) (string, error)` | "reports the container's SELinux mount label (its private MCS category), read from `podman inspect`" (`podman.go:381-385`) | **Degenerate: returns `""`, nil.** There is no container and no per-container MCS category. Empty is already a first-class value in the consumer: `ConfigMaterializer.Materialize` documents "mcsLabel is \"\" on the PROVISION path … there is no label to target … skip chcon" (`go/internal/runner/config_materialize.go:138-144`). The host tier extends that meaning: empty on **every** path, so the `chcon -R` relabel (`config_materialize.go:353-354`) never runs. Agent reads succeed because materialized files carry the Runner's own label and the agent **is** the Runner's uid. See "MCS/SELinux relabel gap" below. |
| `Resize(ctx, id, limits)` | "changes a live container's cgroup resource limits in place … the resize BEHAVIOR … is C3's to fill in behind this signature" (`podman.go:387-396`) | **Degenerate for v1.** The host backend owns no cgroup. It returns a typed "unsupported on host backend" error, never a silent success — a caller that believes it resized must not be lied to. A future systemd user-scope/cgroup v2 delegation could make this real; out of scope here. |

The honest summary: `ExecStreaming`, `Exec`, `Stop`, `Remove` are real;
`Create`/`Start`/`Exists` are bookkeeping over a state dir; `MountLabel` and
`Resize` are degenerate by construction. The backend documents each degenerate
case at the method, in these terms.

#### Egress: explicitly unenforced

The container tiers arm a default-deny nftables firewall **in the container's
own network namespace** (`go/internal/runtime/egress.go:1-4`):

> "Default-deny + allowlist egress firewall for an agent container … The
> container's own network namespace is firewalled with nftables, so a
> compromised agent can't exfiltrate to an arbitrary host"

A host process has no private netns; that mechanism is structurally
unenforceable here. The ruling: host-tier egress is **explicitly unenforced** —
a first-class declared posture, not a degraded arm.

Concretely, the host backend must **not** implement the `inGuestEgressArmer`
probe-and-skip seam. That seam exists so a backend that armed egress itself can
tell `AgentRuntime.provision` to skip the host-side arm exec
(`go/internal/runtime/agent.go:307-312`):

```go
func (r *AgentRuntime) provision(ctx context.Context, id ContainerID, spec AgentSpec) error {
    if armer, ok := r.runtime.(inGuestEgressArmer); !ok || !armer.EgressArmedInGuest() {
        if err := r.armEgress(ctx, id, spec.Egress); err != nil {
            return err
        }
    }
```

and its contract is "the backend armed it internally" — the test names it "a
fakeRuntime that self-arms egress in-guest … so AgentRuntime.provision must
skip the host-side armEgress exec — mirroring the microVM backend"
(`go/internal/runtime/agent_test.go:298-301`). Returning `true` from
`EgressArmedInGuest()` on the host backend would **falsely claim someone armed
the firewall** when nobody did and nobody can. Instead:

- `AgentRuntime` grows an explicit unenforced path: a backend marker interface
  (e.g. `EgressUnenforced() bool`, name settled at T2) that makes provision
  skip `armEgress` **and** record the posture as unenforced — a distinct state,
  never conflated with armed.
- The unenforced posture is **visible in session state and UI**: the session
  carries an egress-posture field surfaced wherever session status renders, so
  a green launch is never read as contained. A user must be able to see, per
  session, "egress: unenforced (host tier)".
- A host-tier launch that carries a non-empty `EgressPolicy` allowlist fails
  loud at provision ("host backend cannot enforce an egress policy"), never
  silently ignores it.

Most users of this tier will not have armed egress anyway — it is primarily an
enterprise-posture control. A future bubblewrap (Linux) / `sandbox-exec`
(macOS) wrapping mode could add real containment to the host tier later; it is
noted as future work and deliberately not designed here.

#### Secrets: pin the SecretSpec `keyring://` provider

Per DL-024 (`docs/designs/DECISIONS.md:137`): "Each agent runs in a per-agent
container on the Runner for blast-radius isolation, not credential avoidance."
The container was never the thing keeping secrets from the agent — the agent is
handed resolved values regardless, and they are the user's own secrets. So the
host tier changes nothing about *who sees* secrets. What this record does pin,
**on merit and explicitly not as a mitigation**, is at-rest handling on the
Server side for host-tier (self-host, single-box) deployments: the SecretSpec
resolver's provider is pinned to `keyring://`, so resolved values live in the
OS keyring rather than wherever the SDK's default chain lands.

The seam exists and is currently unused: `WithProvider` pins the provider URI
(`go/internal/secrets/resolver.go:83-85`):

```go
// WithProvider pins the SecretSpec provider URI (e.g. "keyring://",
// "onepassword://Production"). Empty uses the SDK's default provider chain.
func WithProvider(uri string) SpecOption { return func(r *SpecResolver) { r.provider = uri } }
```

and production pins nothing today (`go/server/serve.go:528`):

```go
resolver := secrets.NewSpecResolver(st, secretsStateDir(cfg))
```

T3 threads a config knob through `serve.go` and defaults the host-tier
single-box profile to `keyring://`. This does not depend on, replace, or
preempt the gateway-credentials at-rest encryption record
(`docs/designs/server/compass-gateway-credentials-at-rest-encryption.md`),
whose T0–T5 are all unimplemented — see Global Constraints.

**The `$HOME/.compass/{env,secrets}` collision.** The materializer writes
resolved secrets into the agent's `$HOME/.compass/env` before agent start
(`go/internal/runner/host.go:360-384`: "Materialize the agent's secrets into
the container BEFORE exec'ing the agent … `h.materializer.Install(ctx,
handle.ID(), handle.HomeDir(), …)`"; the agent "sources that file from its own
namespace at startup", `go/internal/runner/agent_exec.go:72-74`). In a
container, `$HOME` is container-private. On the host tier, a naive `$HOME` is
the user's **real** home — colliding with any `.compass` state the user's own
CLI tooling keeps, and strewing per-agent runtime files into a shared dir.
This is an **ergonomics/path question, not a security one** (the values are
the same user's secrets either way, on the same machine, under the same uid).
Resolution: the host backend sets the agent's `HOME` to the handle's private
home-overlay dir inside the state dir (the `handle.HomeDir()` seam already
threads it), so `$HOME/.compass/env` lands per-agent and `Remove` cleans it.
The user's real home is reachable by path — the whole point of the tier — but
is not the agent's `$HOME`.

#### MCS/SELinux relabel gap

The config-update path reads the container's MCS label via `podman inspect`
`MountLabel` and `chcon -R`s the freshly materialized version dir into it
(`go/internal/runner/config_materialize.go:141-144`: "read via `podman
inspect` MountLabel … chcon -R it into the container's MCS category AFTER
writing and BEFORE the flip, or a confined agent gets EACCES"; the relabel
shellout at `:353-354`). With no container there is no label and no confined
domain: the host backend's `MountLabel` returns `""`, `Materialize` takes its
already-documented skip-chcon path on every call, and reads succeed because the
agent process runs as the same uid that wrote the files. No new mechanism; the
tier reuses the empty-label contract that already exists for the provision
path.

#### Structurally absent protections (declared, not weakened)

These are absent in this tier, not weaker versions of present ones. The tier's
documentation and session UI state them:

- **Host filesystem**: the agent runs as the user's uid with the user's full
  filesystem access. No mount narrowing, no MCS confinement, no private root.
- **Inter-agent isolation**: two host-tier agents on one box are two processes
  under one uid; each can read the other's state dir, sockets, and secrets
  file. (Corollary: the host tier is single-agent-at-a-time by default —
  see Open Questions.)
- **Egress**: unenforced, per above.

The counterfactual framing is the justification: the user's existing CLI agent
already runs at exactly this posture. The host tier adds Compass's session
management, config, and review flow at that same posture; it removes nothing
the user had.

### Half B — agent-driven config-import review

#### What exists and what is missing

The transport is implemented. `compass agent-config push --dir <path>`
tars+gzips a local directory and `PutAgentConfig`s it
(`go/cmd/compass/agent_config.go:30-32`: "newPushCmd builds `agent-config push
--dir <path>`: tar+gzip the dir into a bundle the store door accepts and
PutAgentConfig it (admin-gated)"). The bundle grammar whitelists top dirs
`skills/`, `extensions/`, `mcp/`, `settings/`, `rules/`, `agents/`, `prompts/`,
`profiles/` plus top-level `AGENTS.md` and `models.yml`
(`go/cmd/compass/bundle.go:29-53,71-80`, mirroring "the store door
(internal/store/agent_config.go) so a bundle this builder produces passes
validateAndHashConfigBundle", `bundle.go:23-24`).

What is missing is judgment. A user pushing their existing corpus cannot tell
which of their hand-written skills/rules Compass's built-ins already cover,
which conflict, and which are safe to drop. Filename collision checks are the
shallow half; real overlap is **semantic** — a hand-written skill that does
what a built-in does, differently, sharing no filename. Per the ruling this is
**not a deterministic gate**: it is an agent task.

#### The review task

A first-run onboarding flow in which a Compass agent (host tier — see below)
reads the user's imported corpus against Compass's built-ins and produces a
**report**; the user decides. Shape:

1. **Ingest**: the agent reads the source corpus directly from where it lives
   (`~/.agents`, `~/.claude`, an existing bundle dir) and enumerates candidate
   members against the bundle grammar (what would even be importable).
2. **Compare**: for each candidate, the agent reads it and the built-in corpus
   and classifies: **redundant** (a built-in already does this — including
   semantic overlap with no shared filename), **conflicting** (contradicts a
   built-in rule/skill or Compass's composition semantics), **complementary**
   (safe to import as-is), with a one-line rationale and the specific built-in
   it overlaps.
3. **Explain composition**: the report states, per category, what will actually
   happen on import, grounded in the shipped semantics — settings are
   fleet-first whole-file ("overlay-over-project precedence", DL-123), rules
   and AGENTS.md compose additively ("fleet-first, both levels load, no
   cross-level dedup", `docs/designs/agent/compass-agent-config-passthrough/design.md:481-483`;
   "the fleet file composes additively with the checkout's own AGENTS.md
   chain", `:50-53`); the bundle is a fleet-wide **singleton**, admin-gated,
   current-only ("upserts it as the single current bundle …
   current-only retention via the singleton PK upsert",
   `go/internal/store/agent_config.go:131-138`); and credential-marked settings
   are rejected at the door ("credentials never ride the config bundle",
   `go/internal/store/agent_config.go:1026-1030`) — so the agent tells the
   user up front which members will bounce and why.
4. **Decide**: the user marks each finding keep/drop/rewrite. The agent then
   assembles the approved subset into a bundle dir and (with the user's
   go-ahead) runs the existing push. **The agent proposes; the user disposes.**
   The review never mutates the user's source corpus and never pushes without
   an explicit user decision.

The deliverable of the review is the report plus the user's recorded
decisions — not an automatic mutation of anything.

#### Why the host tier makes this land

On the container tiers, reviewing a not-yet-imported corpus needs a
push-then-inspect round trip: the corpus must enter the bundle pipeline before
any agent can see it, which is backwards — the review is supposed to happen
*before* the push. A host-tier agent is already sitting where the user's config
lives and reads the source corpus directly. That makes the review a natural
first-run task on the exact tier a new user starts on, and it is a
demonstration of value in the first session: the first thing Compass does is
tell the user something true about their own setup.

### Cross-references

- The living tier spec (what the tiers are, operator-facing):
  `docs/specs/runtime/runner-tiers.md`. This record is the point-in-time why;
  the spec is the living what.
- The trust-model split this extends: DL-325 via
  `docs/designs/infra/runtime/compass-runner-adoption-strategy/design.md`.
- The embedded-mode front door this sits beside (DL-319/DL-320,
  `docs/designs/DECISIONS.md:321-322`): embedded mode lowers *stack* friction
  (the app spawns a local podman-backed stack); the host tier lowers *agent
  environment* friction. They compose: an embedded-local stack can run a
  host-backend Runner.

## Alternatives considered

### A dedicated `HostRuntime` interface instead of implementing `ContainerRuntime`

Rejected. `ContainerRuntime` is frozen at S1 and `SelectBackend` is the sole
selection point; a parallel interface would fork the `AgentRuntime` lifecycle
façade (Launch → provision → credentials) that all tiers share, for no gain —
the degenerate methods are few and honestly documentable. The microVM backend
already set the precedent of a non-podman backend behind the same interface.

### Deterministic import linting instead of an agent review

Rejected by ruling. A filename/collision linter catches only the shallow half
and gives false confidence on the dangerous half (semantic overlap). The
deterministic checks that make sense (bundle grammar, credential denylist)
already exist at the door and the client builder; the review's job is exactly
the part that needs reading comprehension.

### Sandboxed-by-default host tier (bubblewrap/sandbox-exec from day one)

Deferred, not rejected. Wrapping the host process would blunt the tier's core
promise — same access as the user's existing CLI agent, zero setup — and each
wrapper is platform-specific. Noted as future work; a later record may add an
opt-in wrapped mode.

## Global Constraints

- **Public repo.** No managed/multi-tenant product detail; managed-plane
  concerns are named and deferred, never sequenced here
  (`docs/concepts/self-host-and-managed.md`,
  `docs/designs/meta/oss-core-managed-boundary/design.md`).
- **Do not weaken the container tiers.** The podman/microVM egress path
  (`armEgress`, `EgressArmedInGuest`) is untouched; the host tier adds a
  distinct unenforced posture beside it, never a change to arming.
- **`ContainerRuntime` is frozen at S1.** The host backend implements the
  9-method interface as-is (`go/internal/runtime/podman.go:348-396`); no
  interface amendment.
- **No dependency on gateway-credentials at-rest encryption.** That record's
  T0–T5 are all unimplemented
  (`docs/designs/server/compass-gateway-credentials-at-rest-encryption.md`,
  tasks unchecked); nothing here waits on or assumes it.
- **The host tier is self-host single-tenant only** — never a valid backend
  for untrusted or multi-tenant operation (DL-325's axis).
- **Agent proposes, user disposes** — the import review never mutates the
  user's source corpus and never pushes without an explicit user decision.
- **Bundle grammar and door checks are authoritative and unchanged** — the
  review explains them; it does not bypass or re-implement them.

## Plan

- **T1 — `HostRuntime` backend** (`go/internal/runtime/host_backend.go`).
  The 9-method implementation per the table above: state-dir handle model,
  process-group spawn/stop, degenerate `MountLabel`/`Resize`/`AsUser`
  documented at the method. Includes the `ContainerSpec` field map (which
  fields are honored, interpreted, or rejected on host).
  Interfaces: implements `ContainerRuntime`
  (`go/internal/runtime/podman.go:348-396`) exactly; registered in
  `SelectBackend` (`go/internal/runtime/microvm.go:117-125`) as `case "host"`,
  error string extended. Unit tests with a real short-lived process
  (spawn/exec/stop/remove/exists), plus the degenerate-method contracts.
- **T2 — unenforced-egress posture** (`go/internal/runtime/agent.go` + session
  state). New backend marker (distinct from `inGuestEgressArmer`) making
  `provision` skip `armEgress` while recording posture=unenforced; fail-loud on
  a non-empty `EgressPolicy`; posture threaded into session state and rendered
  in the session UI/status surface.
  Interfaces: consumes the `provision` seam (`agent.go:307-312`); produces an
  egress-posture field on the session (exact proto/field shape decided at
  implementation, additive only). Tests mirror
  `TestInGuestArmerSkipsHostArmEgress` (`agent_test.go:312`) for the new
  marker, plus the fail-loud policy case, plus a test asserting the host
  backend does NOT satisfy `inGuestEgressArmer`.
- **T3 — `keyring://` provider pin** (`go/server/serve.go`,
  `go/internal/secrets`). Config knob for the resolver provider; host-tier
  single-box profile defaults it to `keyring://`.
  Interfaces: `secrets.NewSpecResolver(st, dir, secrets.WithProvider(uri))`
  (`go/internal/secrets/resolver.go:83-85,97`); wiring at `serve.go:528`.
  Test: resolver receives the configured URI; empty config preserves today's
  default chain.
- **T4 — per-agent `$HOME` overlay** (host backend + materializer path
  threading). The handle's private home dir is the agent's `HOME`;
  `$HOME/.compass/{env,secrets}` land there; `Remove` cleans them.
  Interfaces: `handle.HomeDir()` as consumed by `h.materializer.Install`
  (`go/internal/runner/host.go:384`); `HOME` on the streaming exec
  (`go/internal/runner/host_test.go:1194-1197` names the existing contract).
- **T5 — config-import review agent task** (compass-agent lane). The first-run
  review flow per Half B: ingest/compare/explain/decide, report format, and
  the assemble-and-push handoff to the existing
  `compass agent-config push --dir` path.
  Interfaces: consumes the bundle grammar (`go/cmd/compass/bundle.go:29-53`)
  and door semantics (`go/internal/store/agent_config.go:131-174,1026-1033`)
  read-only; produces a report artifact + an approved bundle dir. No new RPC.
- **T6 — docs**. Tier documentation: the declared-absent protections list, the
  graduation guidance (host → podman → microVM), and the first-run review
  walkthrough. Cross-links `docs/specs/runtime/runner-tiers.md`.

## Tasks

- [ ] T1 — `HostRuntime` backend implementing the frozen `ContainerRuntime`,
      registered in `SelectBackend` as `host`
- [ ] T2 — unenforced-egress posture: new marker (not `inGuestEgressArmer`),
      fail-loud on policy, posture visible in session state/UI
- [ ] T3 — SecretSpec provider knob; host-tier profile pins `keyring://`
- [ ] T4 — per-agent `$HOME` overlay for `.compass/{env,secrets}`
- [ ] T5 — agent-driven config-import review (first-run onboarding flow)
- [ ] T6 — tier docs: absent protections, graduation guidance, review
      walkthrough

## Open Questions

- **Two motivations, one feature?** (load-bearing for scope, not for T1-T4
  correctness) Onboarding convenience and permanent host capability (workflows
  needing the real session bus/display, which no container tier can provide)
  are two motivations wearing one backend. This record ships the tier framed as
  the onboarding wedge and treats host-capability use as a supported
  consequence, not a designed-for product surface. If host-capability is a
  first-class permanent use case, it likely wants its own follow-up record
  (device/session-bus documentation, multi-agent-on-host story). Recommendation:
  accept the onboarding framing here; revisit host-capability as its own record
  when a concrete workflow demands it.
- **Concurrent host-tier agents** (non-load-bearing, deferred): v1 documents
  the tier as effectively single-agent (no inter-agent isolation exists;
  process-group stop cannot contain a double-forked escapee). Whether to add a
  soft cap or a cgroup-scoped v2 is deferred until demand exists.
- **`Resize` future** (non-load-bearing, deferred): a systemd user-scope /
  cgroup v2 delegation could make host `Resize` real; deferred until C3's
  resize behavior lands anywhere.

## Ledger delta

Proposed rows for the coordinator to mint at freeze (described, ids not
invented here):

- **Host tier row**: a `host` backend joins `SelectBackend`
  (`""`/`podman`/`microvm`/`host`) as the self-host single-tenant onboarding
  tier — agent as a host process at the user's existing CLI-agent exposure;
  egress explicitly unenforced (a declared posture, visible in session state,
  never `EgressArmedInGuest`); blast-radius protections (host filesystem,
  inter-agent isolation, egress) structurally absent and declared. **AMENDS
  DL-325's trust-model axis** with a third tier below podman: microVM required
  for untrusted multi-tenant, podman the permanent self-host container tier,
  host the self-host onboarding tier — never valid for untrusted or
  multi-tenant operation.
- **Secrets-provider row**: the Server's SecretSpec resolver provider becomes
  configurable; the host-tier single-box profile pins `keyring://` — an
  at-rest-handling improvement on merit under DL-024's framing (isolation was
  never credential avoidance), explicitly not a mitigation.
- **Import-review row**: config-import review is an agent-driven first-run
  onboarding task (semantic overlap in scope, agent-proposes/user-disposes),
  not a deterministic gate; the deterministic door checks (grammar, credential
  denylist) remain the sole automatic enforcement.
