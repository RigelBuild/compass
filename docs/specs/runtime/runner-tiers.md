# Runner tiers

Living source-of-truth for the **Compass runner tier strategy**: which runtime
backends exist, what boundary each provides, and how a user adopts them. The
point-in-time rationale — why the axis is the trust model, why podman is
permanent, why a host tier exists at all — lives in the design records this
spec cites; this spec states only the standing strategy.

Two records carry the why:

- [Runner adoption strategy](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/infra/runtime/compass-runner-adoption-strategy/design.md)
  — the trust-model split ruling (DL-325) and its execution plan.
- [Host runtime tier](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/infra/runtime/compass-host-runtime-tier/design.md)
  — the host tier's design record (the third tier).

## The trust-model axis

**The security boundary follows the trust model, not the deployment
uniformly.** That is the ruled axis
([DL-325](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/infra/DL-325.md): "The runner end state splits by
trust model (RIG-3070): untrusted multi-tenant operation requires the microVM
hardware boundary (KVM, unchanged); self-host single-tenant deployments keep
podman as a permanent, supported entry tier requiring no `/dev/kvm`, with
microVM the recommended (not required) upgrade").

The consequence: a tier is chosen by asking *who is being isolated from whom*,
not by asking which deployment shape is in play. Untrusted multi-tenant
operation runs code from mutually-distrusting tenants and requires the
hardware isolation boundary. A self-host single-tenant deployment runs the
operator's own agents on their own code on their own box — there is no
untrusted tenant to isolate from — so the boundary strength is the operator's
choice, graded across three tiers.

One doctrine governs what every tier's boundary is *for*
([DL-024](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/meta/DL-024.md): "Each agent runs in a per-agent
container on the Runner for blast-radius isolation, not credential
avoidance"). No tier withholds credentials from the agent — secrets are
materialized into the agent's environment on every tier. What varies across
tiers is the *blast radius* a misbehaving or compromised agent can reach, not
what the agent is trusted with.

## The tiers

Tiers are selected at Runner start through the single backend seam,
`SelectBackend` in `go/internal/runtime/microvm.go`. The operator sets it with
the Runner's `--backend` flag or `COMPASS_RUNTIME_BACKEND`; the flag wins when
set. `ValidBackend` accepts `""`/`"podman"` (default), `"microvm"`,
`"apple-container"` (the macOS runtime, DL-330) and `"host"`, and rejects
anything else. This spec covers the host, podman, and microVM tiers.

| Tier | Isolation boundary | Egress enforcement | Trust model served |
| --- | --- | --- | --- |
| **host** | None — the agent runs as a process on the operator's own machine | **Explicitly unenforced** (see below) | Single **trust domain**: the operator's own box, own code, own agents. Not scoped by deployment shape — available to any user running an agent on their own machine |
| **podman** | Rootless container (shared host kernel) | Enforced: default-deny nftables in the container's own netns | Self-host single-tenant — the permanent supported entry tier |
| **microVM** | Hardware virtualization (cloud-hypervisor/KVM) | Enforced: armed in-guest by the backend | Required for untrusted multi-tenant; recommended self-host upgrade |

### host

- **Boundary:** none. The agent runs directly on the operator's machine, at
  the same host exposure as any CLI agent the user already runs. There is no
  container, no separate kernel, no namespace boundary. `HostRuntime`
  (`go/internal/runtime/host_backend.go`) runs each agent as a child of the
  Runner, under the Runner's own uid. Root is refused.
- **Select it:** `compass-runner --backend host` (or
  `COMPASS_RUNTIME_BACKEND=host`). Each agent gets a private 0700 state dir
  holding its socket, its config view, and its own `$HOME`, under
  `$TMPDIR/compass-host`. That root is shared, so on a box with other local
  users, set the Runner's `TMPDIR` to a private directory.
- **Egress:** **explicitly unenforced** — not a degraded or partial arm.
  Compass's egress firewall is default-deny nftables applied to the
  container's own network namespace (`go/internal/runtime/egress.go:2-4`:
  "The container's own network namespace is firewalled with nftables, so a
  compromised agent can't exfiltrate to an arbitrary host"); a host process
  has no such namespace, so the mechanism is structurally inapplicable. The
  host tier does not reuse the in-guest-armed marker
  (`go/internal/runtime/agent.go:298-299`: `type inGuestEgressArmer interface
  { EgressArmedInGuest() bool }`), because that marker means a backend armed
  egress itself — claiming it would be false. Host mode states plainly that
  egress policy is not enforced. The runner refuses any configured egress
  policy rather than ignore it, and `compass agent` and the agent roster show
  the posture as `unenforced`, so a green launch is never read as contained.
- **When to use:** two permanent cases, on the operator's own machine.
  **Onboarding** — near-zero setup, replicating the user's existing CLI-agent
  posture with Compass's server, comms, and config machinery on top. And
  **host-capability work a container cannot reach** — the real session bus,
  display, or device access (window-management tooling driving the live
  desktop, for instance). The second is not a transitional case a user
  graduates off: that work has to run where the hardware and the session are.
- **Scope:** the tier is bounded by **trust domain, not deployment shape**.
  Any user running an agent on their own machine puts exactly one principal
  on that host — themselves — whatever topology their Server sits in. The
  deployment a Server serves does not enter the analysis.
- **When NOT to use:** untrusted work, and any case that needs to isolate
  mutually-distrusting principals from each other — a shared kernel and a
  shared `$HOME` cannot separate parties. Also unsuitable wherever egress
  policy must actually bind. It is not the preferred steady state for
  general agent work (see [Standing guidance](#standing-guidance)).
- **Protections that are absent, not weaker:**
  - **Host filesystem:** the agent has the operator's full filesystem access.
    No mount narrowing, no SELinux/MCS confinement, no private root.
  - **Inter-agent isolation:** two host-tier agents are two processes under
    one uid. Each can read the other's state dir, socket, and secrets. Run one
    host-tier agent at a time unless that is acceptable.
  - **Egress:** unenforced, as above.
  - **Resource limits:** the backend owns no cgroup, so `Resize` returns an
    error rather than pretend.
- **Secrets at rest:** `compass-stack up --runtime-backend host` starts its
  server with `--secret-provider keyring://`, so host-tier secrets live in the
  OS keyring. `COMPASS_SECRET_PROVIDER` in the stack's environment still wins:
  the stack then passes no flag and the server reads the variable itself.
  Every other backend keeps the server's own default.
- **What it does and does not protect:** per DL-024 the container was never
  credential avoidance — agents receive the user's own secrets on every tier.
  The host tier gives up only the blast-radius boundary; it does not hand the
  agent anything the other tiers withhold.

### podman

- **Boundary:** a rootless per-agent container over the podman CLI — a
  shared-kernel namespace boundary, no `/dev/kvm` required. The backend is
  the thin seam implementation (`go/internal/runtime/podman.go:11-12`:
  "a thin WorkloadRuntime over the podman CLI: the only place a subprocess
  is spawned. Everything above depends on the interface").
- **Egress:** enforced. The host-side arm execs the nftables script inside
  the container, as root, before the agent runs (`AgentRuntime.armEgress`).
  It pins resolved allowlist addresses in `/etc/hosts`. An agent exec runs as
  the agent uid with no capabilities, so it cannot alter the ruleset or pins.
  The ruleset accepts loopback, so a pasta `-T`/`-U` forward (namespace to
  host) would reach host loopback ports. When agents' default network is
  pasta, the Runner refuses to start if `containers.conf` `pasta_options`
  enables one
  (`PodmanCLI.VerifyNoPastaForwards`, `go/internal/runtime/podman.go`).
- **When to use:** the permanent, supported self-host entry tier — any Linux
  box or VPS without `/dev/kvm`, and macOS via podman-machine. It is the
  production default today (`go/internal/runtime/microvm.go:119-120`:
  `case "", "podman": return NewPodmanCLI(), nil`) and the backend behind the
  embedded-local front door (DL-319).
- **When NOT to use:** untrusted multi-tenant operation — a shared kernel is
  not the required boundary there.

### microVM

- **Boundary:** hardware virtualization — a per-session microVM under
  cloud-hypervisor on KVM. Requires Linux with `/dev/kvm`; a KVM-absent host
  hard-fails on this path, with no silent degrade.
- **Egress:** enforced, armed in-guest by the backend itself before the exec
  gate opens; the runtime advertises this via the in-guest-armed marker so
  the host-side arm is skipped (`go/internal/runtime/agent.go:304-306`: "has
  already armed by Start, so the host-side armEgress exec … is skipped").
- **When to use:** required for untrusted multi-tenant operation (DL-325);
  recommended (not required) for self-host, for defense-in-depth or an
  operator who runs untrusted code or shares the box.
- **When NOT to use:** it is never wrong on a capable host — the constraint
  is the KVM floor, which cheap VPS tiers mostly cannot expose.

## The adoption funnel

Each stage is a graduation, never a gate — a user may stay at any stage.

1. **Host tier** — the near-zero-setup front step: run
   Compass agents at the same host exposure as the CLI agent you already use.
   The counterfactual is not "that user on a container tier"; it is that user
   staying on their existing agent with the same exposure and none of
   Compass.
2. **Embedded-local (podman)** — the local-stack option in the native app's
   first-run chooser. When `app.toml` is absent and neither `--mode` nor
   `COMPASS_APP_MODE` is set, the app offers embedded-local or a server
   connection. It saves the chosen config to `app.toml` once and never rewrites
   an existing file. Real isolation, still on your own box.
3. **Self-host graduation** — always-on operation on a dedicated box: the
   podman entry tier on any VPS (no `/dev/kvm` needed), or the microVM tier
   on a KVM-capable machine. Client mode is the recommended steady state
   (DL-319: client mode "stays first-class and is the RECOMMENDED
   steady-state for real self-host").

## Standing guidance

**The host tier is not the preferred steady state — as guidance, not a
gate.** The reason: it provides no blast-radius boundary and no egress
enforcement, so everything an agent can do, it can do to the whole machine.
The container and microVM tiers exist because that boundary is worth having
(DL-024's blast-radius doctrine). Compass therefore *recommends* graduating
to a container-backed tier, and the onboarding surfaces say so — but the host
tier is not crippled, feature-gated, or nagged into disuse to force the move.
A user who stays on the host tier indefinitely is a supported user, strictly
better off than on a bare CLI agent at the same exposure.

The same guidance-not-gate posture holds one tier up: podman is the permanent
self-host entry tier, microVM the recommended upgrade — recommended in the
docs, never required for single-tenant self-host.

### Graduating off the host tier

Move up when an agent should not be able to touch the whole machine: when it
runs code you did not write, when you share the box, or when egress policy
must bind. Change only the Runner's backend: `--backend podman` (any Linux box
or podman-machine, no `/dev/kvm`) or `--backend microvm` (KVM). The fleet
config bundle, sessions, and Server are the same on every tier, so nothing
else moves. Keep the host tier for work that needs the live session bus,
display, or devices.

## Host-tier first run: the config-import review

The host tier's first useful job is to review the operator's existing agent
config before it is imported. The built-in `config-import-review` skill
(`config/skills/config-import-review/SKILL.md`) runs it:

1. **Ingest.** The agent reads the operator's corpus where it lives (for
   example `~/.agents` or `~/.claude`) and maps each file to the bundle member
   it would become. Files with no member (hooks, other tools' state) are
   listed as not importable.
2. **Compare.** It reads each candidate against both baselines: Compass's
   shipped `config/` corpus and the live fleet bundle, which can be empty or
   differ from it. It classes each candidate **redundant**, **conflicting**, or
   **complementary**, naming the built-in it overlaps. Overlap is judged on
   content, not filenames. With no readable baseline it stops instead of
   guessing.
3. **Explain.** It states what import will do: the bundle is one fleet-wide
   singleton that a push replaces; rules and `AGENTS.md` add to the project's
   own; `settings/config.yml` replaces as a whole file; credential-bearing
   members bounce at the store door.
4. **Decide.** The operator marks each finding keep, drop, or rewrite. The
   agent writes the report, assembles the approved members in a new bundle
   dir, and runs `compass agent-config push --dir <dir>` only after an
   explicit yes.

The agent proposes; the operator disposes. The review never edits the source
corpus. On a container tier the same skill works only on paths the operator
mounts or copies in, which is why the host tier is where it fits.

## Not yet specified

This spec mixes current behavior with ruled strategy. The line:

- **Current:** the host, podman, and microVM backends behind `SelectBackend`
  (`go/internal/runtime/microvm.go`), podman as the default, egress
  enforcement on podman and microVM, the unenforced posture and `keyring://`
  secrets default on host, the trust-model split itself (DL-325, Active), and
  the native app's first-run chooser with one-time `app.toml` persistence
  (DL-406, Active; DL-319 retains the embedded-local product direction).
- **Future work, not designed:** an OS-sandbox egress mode for the host tier
  (bubblewrap / sandbox-exec) is a possible later addition; nothing in this
  spec depends on it.
