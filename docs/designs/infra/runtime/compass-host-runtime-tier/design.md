# Compass host runtime tier

Status: Draft
Tracking: RIG-3512
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
sits **below** podman. DL-325's rule is that **the boundary follows the trust
model, not the deployment shape** — so the host tier is scoped by *whose
machine and whose trust*, not by which product a user bought.

The agent runs on the operator's own machine, under their own uid, on work
they already trust themselves with. That is a **single-trust-domain** tier:
the operator is the only principal, so there is no boundary for the tier to
enforce. It is never a valid backend for **untrusted** work or for isolating
**mutually-distrusting** principals from each other — a shared kernel and a
shared `$HOME` cannot separate parties, whatever the deployment shape.

Nothing in that scoping is about the *number of tenants a deployment serves*.
A user still has their own machine whatever shape their Server runs in, and
running an agent on it — for onboarding, or for a task that genuinely needs
that box (below) — puts exactly one trust domain on the host: theirs. So the
tier is available to any user running an agent on their own machine, and the
deployment topology their Server sits in does not change the analysis.
Applying the tier to *someone else's* work, on a machine serving more than one
principal, is what DL-325 forbids — and that is a property of the trust
domain, not of the product.

Guidance stays "prefer container/microVM" — the docs recommend graduating —
but the tier is not gated or crippled to force it.

The tier's second motivation is host capability the container cannot provide
at all: workflows that need the user's real session bus, display, or
device access (e.g. window-management tooling driving the live desktop
session). This is not onboarding scaffolding — it is a permanent capability,
and it is the same need whichever deployment a user's Server belongs to: the
work has to run where the hardware and the session are. See Open Questions.

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
| `Exec(ctx, id, spec) (ExecOutput, error)` | "runs a command in a running container, capturing its output. A non-zero exit is a successful runtime call returning a failed command" (`podman.go:355-359`) | Runs the command as a **direct host subprocess** of the Runner, under the Runner's own uid, with `ExecSpec`'s env/cwd/stdin and the per-command timeout. `ExecSpec.AsUser` cannot switch user — the process runs as whoever runs the Runner — so the backend honors exactly one value, the Runner's own effective uid, and rejects (errors on) an `AsUser` naming **any other** uid rather than silently running it wrong. That strict rejection is only launchable because the host tier derives `Workspace.UID` from `os.Geteuid()` instead of the baked fleet constant, so every provision-path `AsUser` already carries the euid it will run as — see "The host-tier uid contract" below. Without that derivation this rule would error on every provision exec and no host agent could launch. |
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

#### The host-tier uid contract

The `Exec` row's strict `AsUser` rejection and the fleet's baked agent uid are
in direct conflict, and resolving it is a design obligation of this tier, not an
implementation detail. The fleet uid is a constant with no override:

```go
// The const is untyped on purpose: it flows into the uint32 runtime.AgentSpec.UID
// field and into int comparisons (e.g. os.Getuid()) alike, with no conversions at
// the call sites. Keeping it in one importable package is the single source of
// truth for the agent-uid invariant across the runner command and the runtime
// package's proofs.
const AgentUID = 1000
```

(`go/internal/agentuid/agentuid.go:8-13`.) The Runner hands exactly that into
its spec defaults (`go/cmd/compass-runner/main.go:153`: `UID:         agentuid.AgentUID,`),
and `BuildSpec` copies it verbatim into every agent's workspace
(`go/internal/runner/spec.go:89-93`, dedented):

```go
Workspace: runtime.Workspace{
    CheckoutDir: d.CheckoutDir,
    HomeDir:     d.HomeDir,
    UID:         d.UID,
},
```

Nothing configures it. The Runner's whole flag block declares no uid flag or env
override (`go/cmd/compass-runner/main.go:44-84`, `run()`'s flag declarations
through `flag.Parse()`; the command's only other `uid` mentions are the podman
userns-remap preflight comment at `main.go:96-98`), and
`NewConfigSpecBuilder`'s sole check on the value is that it is not root
(`go/internal/runner/spec.go:59-60`):

```go
if defaults.UID == 0 {
    return nil, errors.New("spec defaults require a non-root uid")
}
```

That uid is then what every provision-path exec passes as `AsUser`:
`AgentRuntime.ExecAsAgent` (`go/internal/runtime/agent.go:203-204`:
`AsUser(strconv.FormatUint(uint64(handle.spec.Workspace.UID), 10))`),
`WriteAgentFile` (`agent.go:249-250`), `installCredentials` (`agent.go:343-344`),
`ensureCheckoutDir` (`agent.go:359-360`), the secrets materializer
(`go/internal/runtime/secrets_materialize.go:435-436`), and the agent's own
streaming exec (`go/internal/runner/agent_exec.go:78-79`:
`AsUser(strconv.FormatUint(uint64(e.UID), 10))`).

**Ruling.** On the host tier `Workspace.UID` is derived from the Runner's real
effective uid — `os.Geteuid()` at Runner startup — and never from
`agentuid.AgentUID`. The fleet `SpecDefaults.UID` constant is **not usable on
this tier**: it names the uid baked into the agent *image*, which this tier does
not run, so a host Runner whose euid is not 1000 (the normal case, and the whole
premise of the tier) would fail every provision exec against a rule that is
otherwise correct. Deriving the uid makes the strict rejection both strict and
always-satisfied: the only value that ever reaches `AsUser` is the euid the
subprocess will run as anyway, and any other uid is a real caller bug that must
error. The rejection is **not** softened to accept-and-ignore — an `AsUser`
naming a different uid means the caller believes a user switch happened, and the
host backend cannot provide one.

Two consequences the plan carries (T1a):

- The host tier needs its own spec derivation. `SpecDefaults` is built once at
  Runner startup (`main.go:148-156`), so the host profile supplies the derived
  euid there rather than the constant; the existing non-root check
  (`spec.go:59-60`) still applies, so a Runner running as root is refused — the
  same posture the container tiers hold (`go/internal/runtime/workspace.go:51-53`:
  "UID is the unprivileged uid the agent runs as. Never container-root — that
  would let the agent tear down its own egress firewall").
- `runtime.Workspace.UID` is a `uint32` (`workspace.go:53`) while `os.Geteuid()`
  returns an `int`, and it returns `-1` on platforms without the syscall — so the
  derivation validates the value before narrowing rather than converting blindly.

#### Agent transport: the socket and config paths

`ContainerRuntime` does not deliver the agent its gateway socket or its config;
the Runner's `Provision` does, by bind-mount, to two paths that are frozen
constants on **both** sides of the rendezvous. A host process has no bind
mounts, so this is the one part of the tier that no `ContainerRuntime`
implementation can supply — it needs its own Provision leg.

Runner side (`go/internal/runner/host.go:33-38`, tabs expanded):

```go
const (
    agentSocketDir       = "containers"
    agentSocketFile      = "agent.sock"
    agentSocketMountPath = "/run/compass/agent.sock"
    agentConfigMountPath = "/run/compass/agent-config"
)
```

preceded by the comment that names the contract (`host.go:30-32`):
"`agentSocketMountPath` is the fixed in-container path the socket is
bind-mounted to, so the agent needs no per-session configuration — it always
dials the same path". Both are delivered as mounts on the podman provision leg —
`host.go:198`: `spec.Mounts = append(spec.Mounts, listener.Mount(agentSocketMountPath))`
and `host.go:214`:
`spec.Mounts = append(spec.Mounts, runtime.Mount{HostPath: mount.HostPath, ContainerPath: agentConfigMountPath, ReadOnly: true})`.

Agent side, both paths are compile-time constants with no configuration input
(`packages/compass-agent/src/cli.ts:86-91`):

```ts
/**
 * The in-container path the Runner bind-mounts this agent's socket to. Fixed by
 * contract with `internal/runner/host.go:33` — the agent takes no per-session
 * socket configuration, so this constant IS the rendezvous.
 */
export const AGENT_SOCKET_PATH = "/run/compass/agent.sock";
```

and (`packages/compass-agent/src/config-reader.ts:47-53`):

```ts
/**
 * The in-container path the Runner materializes the agent-config bundle to.
 * Fixed by contract with the Runner's mount (design §CD-3) — the agent takes no
 * per-session config location, so this constant IS the rendezvous. The agent
 * reads through `<mount>/current`, the symlink the Runner flips.
 */
export const AGENT_CONFIG_MOUNT_PATH = "/run/compass/agent-config";
```

Both are pinned by contract tests. `packages/compass-agent/src/cli.test.ts:113-116`
(tabs expanded):

```ts
describe("AGENT_SOCKET_PATH", () => {
    test("matches the Runner's fixed in-container mount path", () => {
        expect(AGENT_SOCKET_PATH).toBe("/run/compass/agent.sock");
    });
```

and `packages/compass-agent/src/config-reader.test.ts:67-70`, whose preceding
comment states why it is pinned (`config-reader.test.ts:63-66`) — "A drift is a
silent unconfigured boot, so it is pinned — beside `AGENT_SOCKET_PATH`'s
contract test":

```ts
describe("AGENT_CONFIG_MOUNT_PATH", () => {
    test("is the frozen /run/compass/agent-config contract path", () => {
        expect(AGENT_CONFIG_MOUNT_PATH).toBe("/run/compass/agent-config");
    });
```

So a host-tier agent launched with no transport design dials a literal
`/run/compass/agent.sock` that either does not exist (no gateway, dial timeout)
or — if the host backend created it for real — is machine-global and needs root
to bind, which makes it structurally un-per-agent and contradicts the tier's
one-state-dir-per-handle model. The config path fails worse: absent, it is a
**silent unconfigured boot**, exactly the drift the test above exists to catch.

**Ruling.** The host tier gets its own `Provision` leg, beside the existing
podman and microVM legs, and the two agent-side constants become
env-overridable:

- The leg serves the per-agent gateway socket inside the handle's own state dir
  (the same 0700 per-agent dir `Create` mints), not under a machine-global
  `/run/compass`, and materializes the config tree to a path in that dir. No
  mounts are appended — there is nothing to mount into.
- It threads both paths to the agent as environment variables on the streaming
  exec that starts it (`AgentEnv.execSpec`, `go/internal/runner/agent_exec.go:77-81`,
  already the seam that sets `HOME`/`COMPASS_WORKDIR`).
- `AGENT_SOCKET_PATH` and `AGENT_CONFIG_MOUNT_PATH` become **env-overridable
  with today's literals as defaults**, so an agent that receives no override
  behaves byte-identically to today and the container tiers are untouched. The
  two contract tests keep pinning the default; each gains a case asserting the
  override path. `cli.ts` already carries the precedent for the config half —
  `MainDeps.configMount` is documented as "Overridable ONLY so a test can point
  the reader at a tempdir fixture instead of the container path"
  (`cli.ts:586-590`) — this promotes that from a test-only dependency seam to a
  first-class environment input, which is a change to the frozen contract and is
  named as such.

This is a change to the `compass-agent` package's frozen path contract and to
its two pinned contract tests. It is deliberate and scoped: the frozen value
stays the default, and only the host tier ever supplies an override. The
alternative (a Provision-side probe seam alone, mirroring `vsockGatewayEngine`)
is rejected in Alternatives considered — a Provision-side probe can only change
what the *Runner* does, and cannot change a path the agent resolves from a
constant with no configuration input.

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
- A host-tier launch that carries **any** `EgressPolicy` reaching provision
  fails loud ("host backend cannot enforce an egress policy"), never silently
  ignores it. The trigger is the policy's **presence**, not a non-empty
  allowlist: an empty host set is the *strictest* posture, not the absence of a
  policy (`go/internal/runtime/egress.go:29-31`):

  ```go
  // EgressPolicy is the set of destinations an agent container may reach. An empty
  // host set is pure default-deny (only loopback, established flows, and DNS to
  // the container's own resolver).
  ```

  and empty is also the Runner's default: `--egress-allow` defaults to `""`
  (`go/cmd/compass-runner/main.go:58-59`) and the parse turns that into a real
  policy (`main.go:378-381`):

  ```go
  func parseEgress(csv string) (runtime.EgressPolicy, error) {
      if strings.TrimSpace(csv) == "" {
          return runtime.AllowEgress()
      }
  ```

  Keying the check on non-emptiness would therefore reject a *looser* policy
  while silently discarding the *tightest* one — the exact silent-ignore this
  bullet exists to prevent, inverted.
- Making presence expressible is a small upstream change the tier requires.
  `EgressPolicy` today draws no configured/unconfigured distinction: its only
  accessor is `Hosts()` (`go/internal/runtime/egress.go:67`), so a zero-value
  `EgressPolicy{}` and an explicit `AllowEgress()` are indistinguishable. T2
  adds a `configured bool` set by `AllowEgress`/`MustAllowEgress` plus a
  `Configured()` accessor, and the host backend refuses any spec whose policy
  reports configured. `Hosts()` and `NftScript()` are untouched, so container
  arming stays byte-identical.
- Consequently a host-tier launch must come through a path that carries **no**
  egress policy at all — the host Runner profile leaves `SpecDefaults.Egress` at
  its zero value rather than calling `parseEgress` — instead of relying on an
  allowlist happening to be empty.

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
The interface layer was never the hard part, and this record does not argue the
tier's feasibility there: the load-bearing work is outside `ContainerRuntime`
entirely — the uid derivation and the Provision transport leg above, neither of
which a parallel interface would have made easier.

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

### A Provision-side probe seam alone for the agent transport

Rejected as insufficient, not as ugly. The microVM backend's precedent is a
Provision probe (`go/internal/runner/host.go:50-60`, `vsockGatewayEngine`, whose
leg at `host.go:800-810` "launches the container with NO agent-socket mount and
NO config mount"), and the host tier does need the equivalent leg. But a probe
only changes what the **Runner** does. The path the agent dials is resolved from
a module-level constant with no configuration input
(`packages/compass-agent/src/cli.ts:91`, `config-reader.ts:53`), so no
Runner-side seam can redirect it. The agent-side override is unavoidable; the
probe leg is necessary but not sufficient, and the record takes both.

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
- **The host tier is a single-trust-domain backend** — valid only for an
  operator running their own agents on their own machine, never for untrusted
  work and never to isolate mutually-distrusting principals from each other
  (DL-325's axis: the boundary follows the trust model, not the deployment
  shape). It is **not** scoped by deployment topology: a user of any
  deployment shape may run an agent on their own box.
- **Agent proposes, user disposes** — the import review never mutates the
  user's source corpus and never pushes without an explicit user decision.
- **Bundle grammar and door checks are authoritative and unchanged** — the
  review explains them; it does not bypass or re-implement them.
- **The host tier derives its own uid.** `Workspace.UID` comes from the
  Runner's `os.Geteuid()`, never `agentuid.AgentUID`
  (`go/internal/agentuid/agentuid.go:13`) — that constant names the uid baked
  into the agent image and has no Runner-side override. Root is still refused
  (`go/internal/runner/spec.go:59-60`).
- **Container-tier agent behaviour stays byte-identical.** The two frozen
  agent-side path constants (`packages/compass-agent/src/cli.ts:91`,
  `config-reader.ts:53`) become env-overridable with today's literals as
  defaults; only the host tier ever supplies an override, and the existing
  contract tests keep pinning the defaults.
- **No egress policy reaches the host backend.** Presence, not emptiness, is
  the fail-loud trigger — empty is the strictest policy
  (`go/internal/runtime/egress.go:29-31`), so the host launch path must carry
  no policy at all.

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
- **T1a — host-tier spec + uid derivation** (`go/cmd/compass-runner/main.go`,
  `go/internal/runner/spec.go`). The host Runner profile derives
  `SpecDefaults.UID` from `os.Geteuid()` instead of `agentuid.AgentUID`,
  validating the `int` → `uint32` narrowing (and the `-1` no-syscall case)
  before use; the existing non-root check stays, so a root Runner is refused at
  startup. The host backend's `Exec`/`ExecStreaming` reject an `AsUser` naming
  any uid other than the Runner's own euid.
  Interfaces: consumes `runner.SpecDefaults` as built at `main.go:148-156`;
  produces `runtime.Workspace{UID: <derived euid>}` (`spec.go:89-93`,
  `go/internal/runtime/workspace.go:51-53`). Tests: the derived uid equals
  `os.Geteuid()` and is what every provision `AsUser` carries
  (`go/internal/runtime/agent.go:203-204`); a mismatched `AsUser` errors; a
  uid-0 derivation is refused (`spec.go:59-60`); a full host provision +
  launch succeeds on a box whose euid is NOT 1000 — the regression this task
  exists to prevent.
- **T1b — host `Provision` leg: agent socket + config delivery**
  (`go/internal/runner/host.go`, `packages/compass-agent/src/cli.ts`,
  `packages/compass-agent/src/config-reader.ts`). A third Provision leg beside
  the podman and `vsockGatewayEngine` legs: serve the per-agent gateway socket
  inside the handle's own 0700 state dir, materialize the config tree there,
  append no mounts, and thread both paths to the agent as env vars on the
  starting streaming exec. `AGENT_SOCKET_PATH` and `AGENT_CONFIG_MOUNT_PATH`
  become env-overridable, defaulting to today's literals. **This touches the
  `compass-agent` package's frozen path contract and its two pinned contract
  tests** (`cli.test.ts:113-117`, `config-reader.test.ts:67-70`), which keep
  pinning the defaults and each gain an override case.
  Interfaces: consumes the leg-selection seam (`host.go:191-193`) and the
  socket/config mount constants (`host.go:33-38`, delivered at `host.go:198`
  and `host.go:214`); produces two env vars on `AgentEnv.execSpec`
  (`go/internal/runner/agent_exec.go:77-81`). Tests: the host leg appends no
  mounts (mirroring `host_vsock_gateway_test.go:121-128`); the agent dials the
  overridden socket and reads the overridden config root; with no override both
  resolve to today's literals.
- **T2 — unenforced-egress posture** (`go/internal/runtime/agent.go` + session
  state). New backend marker (distinct from `inGuestEgressArmer`) making
  `provision` skip `armEgress` while recording posture=unenforced; fail-loud on
  ANY `EgressPolicy` that reaches provision (presence, not a non-empty
  allowlist — empty is the strictest policy, `go/internal/runtime/egress.go:29-31`),
  which requires adding a `configured bool` + `Configured()` to `EgressPolicy`
  (today `Hosts()` at `egress.go:67` is its only accessor) and leaving the host
  Runner profile's `SpecDefaults.Egress` at its zero value; posture threaded
  into session state and rendered
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
- [ ] T1a — host-tier uid derivation: `Workspace.UID` from `os.Geteuid()`, not
      `agentuid.AgentUID`; `AsUser` rejects any other uid; launch proven on a
      non-1000 euid
- [ ] T1b — host `Provision` leg: per-agent socket + config path in the handle's
      state dir, threaded as env vars; `AGENT_SOCKET_PATH` /
      `AGENT_CONFIG_MOUNT_PATH` env-overridable (touches the two pinned
      `compass-agent` contract tests)
- [ ] T2 — unenforced-egress posture: new marker (not `inGuestEgressArmer`),
      fail-loud on ANY `EgressPolicy` reaching provision (presence, not
      non-emptiness) via a new `Configured()` distinction, posture visible in
      session state/UI
- [ ] T3 — SecretSpec provider knob; host-tier profile pins `keyring://`
- [ ] T4 — per-agent `$HOME` overlay for `.compass/{env,secrets}`
- [ ] T5 — agent-driven config-import review (first-run onboarding flow)
- [ ] T6 — tier docs: absent protections, graduation guidance, review
      walkthrough

## Open Questions

- **Two motivations, one feature — RULED: both are permanent, and the tier is
  not deployment-scoped.** (Was: does host-capability want its own record?)
  Onboarding convenience and host capability (workflows needing the real
  session bus/display, which no container tier can provide) are two
  motivations wearing one backend, and the second is **not** onboarding
  scaffolding that a user graduates off. Some work simply has to run on the
  user's own box: the hardware, the display, and the live session are there
  and nowhere else. The tier therefore ships as a **permanent capability**,
  not a wedge, and the earlier framing ("a supported consequence, not a
  designed-for surface") is withdrawn as too weak.

  The same ruling settles the scope question: **availability follows the trust
  domain, not the deployment shape.** A user whose Server sits in any
  deployment topology still has their own machine, and running an agent there
  puts one trust domain on that host — theirs. So the tier is not restricted
  to a single-tenant deployment; what DL-325 forbids is applying it to
  untrusted work or to separate mutually-distrusting principals, which is a
  property of the trust domain (see Approach § Placement).

  Still open, narrowly: whether the device/session-bus surface (which devices,
  which sockets, how documented) wants its own follow-up record once a
  concrete workflow pins the requirements. That is a documentation and
  surface-area question, not a tier-existence question.
- **Concurrent host-tier agents** (non-load-bearing, deferred): v1 documents
  the tier as effectively single-agent (no inter-agent isolation exists;
  process-group stop cannot contain a double-forked escapee). Whether to add a
  soft cap or a cgroup-scoped v2 is deferred until demand exists.
- **`Resize` future** (non-load-bearing, deferred): a systemd user-scope /
  cgroup v2 delegation could make host `Resize` real; deferred until C3's
  resize behavior lands anywhere.
- **The `Container*` vocabulary is a known misnomer** (deferred, tracked
  separately): this tier makes `ContainerRuntime` span a third backend that is
  not a container — direct host processes — after `MicroVMRuntime` already made
  it span a second (`go/internal/runtime/microvm.go:71`). `SelectBackend`'s own
  comment states the endgame (`microvm.go:110-116`): once microVM is the sole
  runtime the container path goes away entirely, leaving an interface named
  `ContainerRuntime` with no container implementation. The misnomer is not the
  interface alone: `ContainerID` (214 refs) already keys microVM sessions
  (`microvm.go:84`) and would key host process groups here, and `ContainerSpec`
  (58 refs) is likewise backend-neutral in practice.

  Ruled name: **`Workload*`** (`WorkloadRuntime`/`WorkloadID`/`WorkloadSpec`) —
  verified unused in Go and proto, and true of a container, a microVM guest,
  and a host process group alike. `Session*` was rejected: a session is already
  the user-facing conversational stream (`SessionEvent` and siblings in
  `proto/compass/v1/compass.proto`), one environment outlives many sessions, so
  the name would assert a one-to-one relation that does not hold. `Sandbox` was
  rejected as asserting isolation the host tier explicitly does not provide.
  `AgentRuntime` (`go/internal/runtime/agent.go:155`) is **not** renamed — it is
  the per-agent lifecycle façade over a backend, and that name is accurate.

  Deliberately **not** in this record's scope: a ~365-reference mechanical
  rename would swamp the design content here, and the freeze at S1 covers the
  method set, not the identifier. Sequenced after the microVM default flip,
  when the vocabulary is forced by reality rather than argued.

## Ledger delta

Proposed rows for the coordinator to mint at freeze (described, ids not
invented here):

- **Host tier row**: a `host` backend joins `SelectBackend`
  (`""`/`podman`/`microvm`/`host`) as a permanent tier for an operator running
  agents on their own machine — agent as a host process at the user's existing
  CLI-agent exposure; egress explicitly unenforced (a declared posture,
  visible in session state, never `EgressArmedInGuest`); blast-radius
  protections (host filesystem, inter-agent isolation, egress) structurally
  absent and declared. It serves two permanent cases, onboarding and
  host-capability work no container tier can reach (real session bus, display,
  device access). **AMENDS DL-325's trust-model axis** with a third tier below
  podman: microVM required for untrusted multi-tenant, podman the permanent
  self-host container tier, host the single-trust-domain tier. Per DL-325's
  own rule the boundary follows the **trust model, not the deployment shape**,
  so the host tier is **not scoped by deployment topology** — it is available
  to any user running an agent on their own machine, and is never valid for
  untrusted work or for isolating mutually-distrusting principals.
  The tier also pins two mechanism decisions: `Workspace.UID` is derived from
  the Runner's `os.Geteuid()` rather than the baked `agentuid.AgentUID`, and the
  agent's socket/config rendezvous paths become env-overridable (defaults
  unchanged) so the host Provision leg can serve them per-agent inside the
  handle's state dir instead of by bind-mount.
- **Secrets-provider row**: the Server's SecretSpec resolver provider becomes
  configurable; the host-tier single-box profile pins `keyring://` — an
  at-rest-handling improvement on merit under DL-024's framing (isolation was
  never credential avoidance), explicitly not a mitigation.
- **Import-review row**: config-import review is an agent-driven first-run
  onboarding task (semantic overlap in scope, agent-proposes/user-disposes),
  not a deterministic gate; the deterministic door checks (grammar, credential
  denylist) remain the sole automatic enforcement.
