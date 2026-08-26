# Isolation and egress: the sandbox around model-written code

Compass runs code an AI model wrote. The load-bearing safety assumption is
therefore the opposite of trust: **treat every agent as potentially compromised
and contain it**, rather than trusting it to behave. Two boundaries do that
containment — an **execution boundary** (each agent in its own sandbox) and a
**network boundary** (default-deny egress from that sandbox). This page is the
model behind both.

## Each agent runs in its own sandbox

Every agent executes in a **per-agent sandbox** on the Runner, isolated from the
host and from every other agent. The reason is **blast radius, not credential
avoidance**: the point is that a misbehaving or compromised agent can damage only
its own sandbox, not the host and not its neighbors. (ledger DL-024)

The sandbox is [disposable compute](./durable-agents-disposable-compute.md) — it
custodies no durable state and no storage credentials, so destroying it loses
nothing of record. That is what makes aggressive containment cheap: the strong
move (tear the whole thing down) has no data cost.

The substrate is on a path from **rootless-podman container** (today, through
Dogfood and trusted-tenant Beta) to a **hardware-virtualized microVM** (the
end state). Both fill the same role; the microVM raises the execution boundary
from a shared-kernel container to a VM-class boundary with its own guest kernel —
the right isolation for running untrusted model-written code, and for putting
multiple tenants on one host. A box without KVM degrades to the container
runtime with an explicit capability log, never silently. The microVM Runner
backend is designed in
[`microvm-runner.md`](../designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md)
(RIG-2394); its KVM floor is already baked into shipped decisions — it is what
made the native app a thin client against a headless KVM-capable stack (ledger
DL-235) and the self-host stack a KVM-capable host bring-up (ledger DL-259).

## Default-deny egress: the agent reaches only what it is allowed to

Isolating the process is not enough — a compromised agent with open network
access could still exfiltrate. So the sandbox's network is **firewalled
default-deny**: an agent can reach nothing on the network unless a host is
explicitly allowlisted. (`go/internal/runtime/egress.go`)

- **Empty policy = pure default-deny.** With no allowlist, only loopback,
  already-established flows, and DNS to the container's own resolver work —
  nothing else. Reaching an LLM provider, a forge, or object storage requires
  that host to be added to the allowlist. (`egress.go:29-31`)
- **The agent cannot widen its own firewall.** The sandbox is granted the
  capability to *arm* nftables only so a root entrypoint can install the ruleset
  at launch; the agent itself then runs as a **non-root user with an empty
  capability set**, so it can neither flush nor edit the rules even though the
  sandbox nominally holds the capability. The ruleset is armed *before* the agent
  process starts. (`egress.go:6-10`)
- **The rules are built to resist evasion.** Both IPv4 and IPv6 addresses of an
  allowlisted host are resolved and allowed, so a dual-stack host can't be
  reached over the family you forgot to list; and DNS is allowlisted first, then
  names are resolved from inside the sandbox, so resolution can't be the thing
  that widens the allowlist. (`egress.go:12-19`)

## The agent holds no server credential

Containment extends to what the agent is *given*, not just what it can *reach*.
The agent is **egress-sealed and holds no Compass server token** — it cannot call
privileged server RPCs directly. Privileged operations an agent appears to
perform (spawning a peer, a forge write) are **relayed through the Server**, which
resolves the caller's identity and authority at the edge, rather than handed to
the agent as a credential it could misuse. (ledger DL-076)

This is the same shape as the [handle/account split](./handle-vs-account.md)
(the agent never holds a per-agent forge seat) and the [no-human-clicks
security boundary](./no-human-clicks.md) (the human provides secret *values*; the
agent only declares needs): capability is kept out of the model's hands and
mediated by the Server.

## Why this is a hard principle

- **The threat model is "the agent is compromised."** Model-written code is
  untrusted by construction. Every boundary here assumes the agent may try to
  reach a host it shouldn't or damage what it can touch, and contains the damage
  structurally instead of relying on the agent's good behavior.
- **Containment is cheap because compute is disposable.** Since the sandbox holds
  nothing durable, the strongest response — destroy it — costs nothing. Isolation
  and disposability reinforce each other: the box is safe to nuke *because* it is
  empty, and it can be empty *because* durability lives in the Server.
- **It scales to multi-tenant.** A VM-class boundary per agent plus default-deny
  egress is what lets many tenants share a host without one reaching another —
  the isolation guarantee the hosted platform is sold on.

## Quick reference

| Boundary | Mechanism | The agent cannot |
| --- | --- | --- |
| Execution | per-agent sandbox (container today → microVM end state) | touch the host or another agent's sandbox |
| Network | default-deny nftables egress, allowlist-only | reach any host not explicitly allowed |
| Firewall control | rules armed by root at launch; agent runs non-root, empty caps | flush or edit its own egress ruleset |
| Server authority | egress-sealed, no server token; privileged calls relayed via Server | call privileged server RPCs directly |
| Durable state / secrets | Server-owned; none in the sandbox | exfiltrate durable data or credentials it never held |
