# The architecture: Server, Runner, and the resident agent

Compass is a three-tier system. A **Server** is the control plane. A **Runner**
is the host-side substrate that provisions and holds one disposable sandbox per
session. Inside each sandbox a **resident agent process** does the work. This
page defines that topology and the two load-bearing paths across it; the
[runtime](./durable-agents-disposable-compute.md) and
[isolation](./isolation-and-egress.md) docs are projections of this model, not
separate systems.

## The components

- **Server — the control plane.** It owns identity, persona/role, session
  lifecycle from the outside, secrets brokering, and the forge relay; it holds
  the sole forge write credential as a `server_only` declared secret. It places
  nothing and knows no internals of how sessions are packed.
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:174-179`,
  ledger DL-052)
- **Runner — the host substrate.** It provisions, starts, and stops session
  environments: `SpecBuilder` maps each provision request to a complete
  `AgentSpec` (image, per-agent workspace, egress policy) that the sandbox is
  launched with. (`go/internal/runner/host.go:46-48`,
  `go/internal/runtime/agent.go:32-57`) The Runner is *also* the pure forwarder
  for agent-initiated privileged calls — it forwards the call and asserts no
  identity. (`go/internal/runnerhub/relay_comms.go`)
- **RunnerHub — the Server-side binding layer.** It is the Server leg that
  resolves `session_id → agent account` from this hub's own binding — recorded
  from the provision request's `agent_account_id`, promoted to the minted
  `session_id` at Start — and executes the call under that account. Spawn/despawn
  rides the same relay, orchestrated server-side, with the hub resolving caller
  identity only. (`go/internal/runnerhub/relay_comms.go`, ledger DL-076)
- **The sandbox — one disposable environment per session.** A rootless-podman
  container today, a hardware-virtualized microVM in the end state; egress-sealed
  and holding no server token. It custodies nothing of record, so it is safe to
  tear down. Its containment and egress posture is owned by
  [isolation and egress](./isolation-and-egress.md).
  (`go/internal/runtime/agent.go:32-57`)
- **The agent process — resident per session.** The bun bundle runs as a
  long-lived process inside the sandbox: it hosts stateful in-process extensions
  (push-guard/git-guard as `tool_call` gates) and MCP servers that run
  session-long in-heap. Density comes from suspending idle sessions, not from
  shrinking the per-process footprint.
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:180-184`)

## The privileged-op path

The agent is egress-sealed and holds no Compass server token, so it cannot call
privileged server RPCs directly. A privileged operation it appears to perform —
spawning a peer, a forge write — travels a fixed hop chain:

1. **Agent** issues the call. It holds no credential and asserts no account.
2. **Runner** forwards the call to the Server. It is a pure relay and asserts no
   identity of its own.
3. **RunnerHub** resolves `session_id → agent account` from the
   provision-recorded session binding.
4. **Server** executes the call under that resolved account's authority.

The load-bearing property: **a `session_id` on the wire selects an account, it
never carries one.** The binding is authoritative Server-side state, so an
unknown, stopped, or dropped session fails closed — never a stale account,
never a bootstrap-admin fallback. This is why the agent needs no server
credential: capability is resolved by the Server from state it owns, not handed
to the model. (`go/internal/runnerhub/relay_comms.go`, ledger DL-076)

## The durability path

The agent persists nothing durable: each committed session entry is tee'd
upstream to the Server as a frame (ledger DL-089). The Server owns a two-tier
durable store — a Postgres hot tail plus an S3-compatible cold archive (ledger
DL-093). On resume the Server reconstructs the transcript and the Runner
materializes it into a fresh sandbox, so the agent picks up on new hardware
without noticing (ledger DL-087). The full contract — what persists, how resume
replays, why the split is the design — is
[durable agents, disposable compute](./durable-agents-disposable-compute.md).

## Quick reference

| Component | Role | Grounding |
| --- | --- | --- |
| Server | control plane: identity, lifecycle, secrets, forge relay, sole forge credential | `design.md:174-179`, DL-052 |
| Runner | host substrate: provisions/starts/stops sandboxes; pure forwarder for agent calls | `host.go:46-48`, `agent.go:32-57`, `relay_comms.go` |
| RunnerHub | Server-side binding: resolves `session_id → account` and executes | `relay_comms.go`, DL-076 |
| Sandbox | one disposable env per session (container → microVM), egress-sealed, no server token | `agent.go:32-57` |
| Agent process | resident per session; hosts in-process gates + MCP servers | `design.md:180-184` |

## The detailed projections

Each sibling concept doc is a projection of this topology onto one concern:

- [Durable agents, disposable compute](./durable-agents-disposable-compute.md) —
  the durability path in full: what survives a sandbox teardown and how resume
  rebuilds it.
- [Isolation and egress](./isolation-and-egress.md) — the sandbox as the
  security boundary: per-agent containment, default-deny egress, no server
  credential.
- [The comms model](./comms-model.md) — the two agent surfaces (session log for
  work, threads for conversation) that ride this topology.
