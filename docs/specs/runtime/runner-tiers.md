# Runner tiers

Living source-of-truth for the **Compass runner tier strategy**: which runtime
backends exist, what boundary each provides, and how a user adopts them. The
point-in-time rationale — why the axis is the trust model, why podman is
permanent, why a host tier exists at all — lives in the design records this
spec cites; this spec states only the standing strategy.

Two records carry the why:

- [Runner adoption strategy](../../designs/infra/runtime/compass-runner-adoption-strategy/design.md)
  — the trust-model split ruling (DL-325) and its execution plan.
- [Host runtime tier](../../designs/infra/runtime/compass-host-runtime-tier/design.md)
  — the host tier's design record (the third tier, not yet built).

## The trust-model axis

**The security boundary follows the trust model, not the deployment
uniformly.** That is the ruled axis (DL-325,
[`DECISIONS.md`](../../designs/DECISIONS.md): "The runner end state splits by
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

One doctrine governs what every tier's boundary is *for* (DL-024,
[`DECISIONS.md`](../../designs/DECISIONS.md): "Each agent runs in a per-agent
container on the Runner for blast-radius isolation, not credential
avoidance"). No tier withholds credentials from the agent — secrets are
materialized into the agent's environment on every tier. What varies across
tiers is the *blast radius* a misbehaving or compromised agent can reach, not
what the agent is trusted with.

## The tiers

Tiers are selected at constructor time through the single backend seam,
`SelectBackend` (`go/internal/runtime/microvm.go:117`:
`func SelectBackend(cfg BackendConfig) (ContainerRuntime, error)`), which
today accepts `""`/`"podman"` and `"microvm"` and rejects anything else
(`go/internal/runtime/microvm.go:124`: `accepted values are "podman"
(default) and "microvm"`). The **host** tier joins that seam as a third
`SelectBackend` value, `"host"` — designed, not yet built (see
[Not yet specified](#not-yet-specified)).

| Tier | Isolation boundary | Egress enforcement | Trust model served |
| --- | --- | --- | --- |
| **host** *(not yet built)* | None — the agent runs as a process on the operator's own machine | **Explicitly unenforced** (see below) | Single-tenant only: the operator's own box, own code, own agents |
| **podman** | Rootless container (shared host kernel) | Enforced: default-deny nftables in the container's own netns | Self-host single-tenant — the permanent supported entry tier |
| **microVM** | Hardware virtualization (cloud-hypervisor/KVM) | Enforced: armed in-guest by the backend | Required for untrusted multi-tenant; recommended self-host upgrade |

### host *(not yet built)*

- **Boundary:** none. The agent runs directly on the operator's machine, at
  the same host exposure as any CLI agent the user already runs. There is no
  container, no separate kernel, no namespace boundary.
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
  egress policy is not enforced.
- **When to use:** onboarding — near-zero setup, replicating the user's
  existing CLI-agent posture with Compass's server, comms, and config
  machinery on top; and host-capability work that a container cannot reach.
- **When NOT to use:** any deployment with an untrusted tenant, and any
  deployment where egress policy must actually bind. It is also not the
  preferred steady state for anyone (see
  [Standing guidance](#standing-guidance)).
- **What it does and does not protect:** per DL-024 the container was never
  credential avoidance — agents receive the user's own secrets on every tier.
  The host tier gives up only the blast-radius boundary; it does not hand the
  agent anything the other tiers withhold.

### podman

- **Boundary:** a rootless per-agent container over the podman CLI — a
  shared-kernel namespace boundary, no `/dev/kvm` required. The backend is
  the thin seam implementation (`go/internal/runtime/podman.go:11-12`:
  "a thin ContainerRuntime over the podman CLI: the only place a subprocess
  is spawned. Everything above depends on the interface").
- **Egress:** enforced. The host-side arm execs the nftables script inside
  the container before the agent runs (`go/internal/runtime/agent.go:319-321`:
  "armEgress arms the egress firewall as the image's default user (uid 1000)
  with CAP_NET_ADMIN. After this, an agent exec — run as the agent uid with
  no capabilities — cannot alter the ruleset").
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

1. **Host tier** *(not yet built)* — the near-zero-setup front step: run
   Compass agents at the same host exposure as the CLI agent you already use.
   The counterfactual is not "that user on a container tier"; it is that user
   staying on their existing agent with the same exposure and none of
   Compass.
2. **Embedded-local (podman)** — the low-friction onboarding front door
   (DL-319, [`DECISIONS.md`](../../designs/DECISIONS.md): "`mode="embedded"`
   returns as the low-friction onboarding / local-dev front door — the app
   spawns/supervises a LOCAL stack via rootless podman on the user's own
   machine"), with zero-config mode selection (DL-320: "absent → embedded
   (the zero-config onboarding default returns)"). Real isolation, still on
   your own box.
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

## Not yet specified

This spec mixes current behavior with ruled strategy. The line:

- **Current:** the podman and microVM backends behind `SelectBackend`
  (`go/internal/runtime/microvm.go:117-125`), podman as the default, egress
  enforcement on both, and the trust-model split itself (DL-325, Active).
- **Not yet built:** the **host tier** in its entirety — there is today no
  host/process backend and no `"host"` value in `SelectBackend`. Its design
  lives in the
  [host runtime tier record](../../designs/infra/runtime/compass-host-runtime-tier/design.md).
- **Not yet built:** the embedded-local front door's app architecture
  (DL-319/DL-320's dual-mode revival) is designed in the compass-native
  lane's embedded-revival record and lands there.
- **Future work, not designed:** an OS-sandbox egress mode for the host tier
  (bubblewrap / sandbox-exec) is a possible later addition; nothing in this
  spec depends on it.
