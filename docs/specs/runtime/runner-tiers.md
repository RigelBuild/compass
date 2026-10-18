# Runner tiers

Living source-of-truth for the **Compass runner tier strategy**: which runtime
backends exist, what boundary each provides, and how a user adopts them. The
rationale lives in two design records:

- [Runner adoption strategy](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/infra/runtime/compass-runner-adoption-strategy/design.md)
  — the trust-model split ruling (DL-325) and its execution plan.
- [Host runtime tier](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/infra/runtime/compass-host-runtime-tier/design.md)
  — the host tier's design record.

The host tier's first-run job is specified in
[config-import-review.md](config-import-review.md).

## The trust-model axis

**The security boundary follows the trust model, not the deployment shape.**
That is the ruled axis
([DL-325](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/infra/DL-325.md)):
untrusted multi-tenant operation requires the microVM hardware boundary (KVM);
self-host single-tenant deployments keep podman as a permanent, supported entry
tier that needs no `/dev/kvm`, with microVM the recommended (not required)
upgrade.

A tier is chosen by asking *who is being isolated from whom*. Untrusted
multi-tenant operation runs code from mutually-distrusting tenants and needs
the hardware boundary. A self-host single-tenant deployment runs the operator's
own agents on their own code and box, so the boundary strength is the
operator's choice across three tiers.

Per [DL-024](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/meta/DL-024.md),
the per-agent boundary is for blast-radius isolation, not credential
avoidance. No tier withholds credentials: secrets are materialized into the
agent's environment on every tier. Tiers differ only in what a misbehaving or
compromised agent can reach.

## The tiers

Tiers are selected at Runner start through `SelectBackend` in
`go/internal/runtime/microvm.go`. The operator sets the Runner's `--backend`
flag or `COMPASS_RUNTIME_BACKEND`; the flag wins when set. `ValidBackend`
accepts `""`/`"podman"` (default), `"microvm"`, `"apple-container"` (the macOS
runtime, DL-330) and `"host"`, and rejects anything else. This spec covers the
host, podman, and microVM tiers.

| Tier | Isolation boundary | Egress enforcement | Trust model served |
| --- | --- | --- | --- |
| **host** | None — the agent is a process on the operator's machine | **Explicitly unenforced** | One trust domain: the operator's own box, code and agents, in any deployment shape |
| **podman** | Rootless container (shared host kernel) | Enforced: default-deny nftables in the container's netns | Self-host single-tenant — the permanent entry tier |
| **microVM** | Hardware virtualization (cloud-hypervisor/KVM) | Enforced: armed in-guest by the backend | Required for untrusted multi-tenant; recommended self-host upgrade |

### host

- **Boundary:** none — no container, kernel, or namespace boundary. The agent
  has the same host exposure as any CLI agent the user already runs.
  `HostRuntime` (`go/internal/runtime/host_backend.go`) runs each agent as a
  child of the Runner, under the Runner's uid. Root is refused.
- **Select it:** `compass-runner --backend host` (or
  `COMPASS_RUNTIME_BACKEND=host`). Each agent gets a private 0700 state dir
  holding its socket, its config view, and its own `$HOME`, under
  `$TMPDIR/compass-host`. That root is shared, so on a box with other local
  users, set the Runner's `TMPDIR` to a private directory.
- **Egress:** explicitly unenforced, not partially armed. Compass's egress
  firewall is default-deny nftables in the container's own network namespace
  (`go/internal/runtime/egress.go`); a host process has no such namespace. The
  host tier does not claim the in-guest-armed marker (`inGuestEgressArmer` in
  `go/internal/runtime/agent.go`), because that marker means the backend armed
  egress itself. The runner refuses any configured egress policy rather than
  ignore it, and `compass agent` and the agent roster show the posture as
  `unenforced`, so a green launch is never read as contained.
- **When to use:** two permanent cases on the operator's own machine:
  - **Onboarding:** near-zero setup, with the user's existing CLI-agent
    posture plus Compass's server, comms, and config on top.
  - **Host-capability work** a container cannot reach: the real session bus,
    display, or devices (for example, window-management tooling driving the
    live desktop). This is not a transitional case.
- **Scope:** bounded by trust domain, not deployment shape. A user running an
  agent on their own machine puts one principal on that host, whatever topology
  their Server sits in.
- **When NOT to use:** untrusted work, or anything that must isolate
  mutually-distrusting principals — a shared kernel and `$HOME` cannot separate
  them. Also unsuitable wherever egress policy must bind.
- **Protections that are absent, not weaker:**
  - **Host filesystem:** the agent has the operator's full filesystem access.
    No mount narrowing, no SELinux/MCS confinement, no private root.
  - **Inter-agent isolation:** two host-tier agents are two processes under
    one uid. Each can read the other's state dir, socket, and secrets. Run one
    host-tier agent at a time unless that is acceptable.
  - **Egress:** unenforced, as above.
  - **Resource limits:** the backend owns no cgroup, so `Resize` returns an
    error.
- **Secrets at rest:** `compass-stack up --runtime-backend host` starts its
  server with `--secret-provider keyring://`, so host-tier secrets live in the
  OS keyring. `COMPASS_SECRET_PROVIDER` in the stack's environment still wins:
  the stack then passes no flag and the server reads the variable itself.
  Every other backend keeps the server's own default.

### podman

- **Boundary:** a rootless per-agent container over the podman CLI — a
  shared-kernel namespace boundary, no `/dev/kvm` required.
  `go/internal/runtime/podman.go` is the only place a subprocess is spawned.
- **Egress:** enforced. The host-side arm execs the nftables script inside
  the container, as root, before the agent runs (`AgentRuntime.armEgress`).
  It pins resolved allowlist addresses in `/etc/hosts`. An agent exec runs as
  the agent uid with no capabilities, so it cannot alter the ruleset or pins.
  The ruleset accepts loopback, so a pasta `-T`/`-U` forward (namespace to
  host) would reach host loopback ports. When agents' default network is
  pasta, the Runner refuses to start if `containers.conf` `pasta_options`
  enables one (`PodmanCLI.VerifyNoPastaForwards`,
  `go/internal/runtime/podman.go`).
- **When to use:** the permanent self-host entry tier — any Linux box or VPS
  without `/dev/kvm`, and macOS via podman-machine. It is the production
  default and the backend behind the embedded-local front door (DL-319).
- **When NOT to use:** untrusted multi-tenant operation — a shared kernel is
  not the required boundary there.

### microVM

- **Boundary:** a per-session microVM under cloud-hypervisor on KVM. Requires
  Linux with `/dev/kvm`; a host without KVM hard-fails, with no silent
  degrade.
- **Egress:** enforced, armed in-guest by the backend before the exec gate
  opens. The runtime advertises this with the in-guest-armed marker, so the
  host-side arm is skipped.
- **When to use:** required for untrusted multi-tenant operation (DL-325);
  recommended for self-host when the operator runs untrusted code, shares the
  box, or wants defense in depth.
- **When NOT to use:** never wrong on a capable host. The constraint is the
  KVM floor, which cheap VPS tiers mostly cannot expose.

## The adoption funnel

Each stage is a graduation, never a gate; a user may stay at any stage.

1. **Host tier** — the near-zero-setup first step: Compass agents at the same
   exposure as the CLI agent the user already runs. The alternative is that
   user staying on their existing agent without Compass, not that user on a
   container tier.
2. **Embedded-local (podman)** — the local-stack option in the native app's
   first-run chooser. When `app.toml` is absent and neither `--mode` nor
   `COMPASS_APP_MODE` is set, the app offers embedded-local or a server
   connection. It saves the choice to `app.toml` once and never rewrites an
   existing file.
3. **Self-host graduation** — always-on operation on a dedicated box: podman
   on any VPS, or microVM on a KVM-capable machine. Client mode is the
   recommended steady state (DL-319).

## Standing guidance

**The host tier is not the preferred steady state — as guidance, not a
gate.** It has no blast-radius boundary and no egress enforcement, so an agent
can affect the whole machine. Compass recommends graduating to a
container-backed tier, and the onboarding surfaces say so. The host tier is
not crippled, feature-gated, or nagged to force the move; a user who stays on
it is supported.

The same holds one tier up: podman is the permanent self-host entry tier, and
microVM is recommended in the docs but never required for single-tenant
self-host.

### Graduating off the host tier

Move up when an agent should not be able to touch the whole machine: when it
runs code you did not write, when you share the box, or when egress policy
must bind. Change only the Runner's backend: `--backend podman` (any Linux box
or podman-machine, no `/dev/kvm`) or `--backend microvm` (KVM). The fleet
config bundle, sessions, and Server are the same on every tier. Keep the host
tier for work that needs the live session bus, display, or devices.

## Not yet specified

This spec mixes current behavior with ruled strategy:

- **Current:** the host, podman, and microVM backends behind `SelectBackend`,
  podman as the default, egress enforcement on podman and microVM, the
  unenforced posture and `keyring://` secrets default on host, the trust-model
  split (DL-325, Active), and the native app's first-run chooser with one-time
  `app.toml` persistence (DL-406, Active; DL-319 keeps the embedded-local
  direction).
- **Future work, not designed:** an OS-sandbox egress mode for the host tier
  (bubblewrap / sandbox-exec). Nothing in this spec depends on it.
