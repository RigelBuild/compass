# Durable agents, disposable compute

A Compass agent is **durable**; the machine it runs on is **disposable**. The
agent — its identity, its place in the tree, and the full history of its work —
outlives any particular container or microVM it happens to be executing in. The
compute is cattle; the agent and its session are the thing of record. Getting
this split right is what lets Compass tear down, move, suspend, and rebuild the
expensive part (the sandbox) without ever losing the valuable part (the agent's
state).

## The two things that persist, and the one that does not

- **The agent is a stable, long-lived tree node.** Managers and their peers are
  minted once and live as durable accounts — each with a home channel it is
  always subscribed to — not spun up one-shot per task. An agent's identity,
  its owner, its persona, and its position in the [agent
  tree](./handle-vs-account.md) are model facts in the Server's store, not
  properties of a running process. See [the comms model](./comms-model.md) for
  why agents are stable nodes with home channels rather than ephemeral runs.
- **The session is durable and Server-owned.** Everything the agent has thought
  and done — its streamed reasoning, tool calls, and edits — is persisted by the
  Server as a durable transcript. The Server owns this store; the agent does not
  custody it. (ledger DL-084, DL-093)
- **The compute is disposable.** The container or microVM the agent executes in
  holds only an *ephemeral working copy* of the session file, and it **dies with
  the container**. It custodies nothing of record: no durable state, and — by
  the [isolation and egress](./isolation-and-egress.md) posture — no storage
  credentials at all. (ledger DL-085, DL-089)

The test for which bucket something falls in: **would it survive the sandbox
being destroyed right now?** The agent, its identity, and its transcript survive.
The process, the local session file, the checkout, and any in-memory state do
not — and nothing is allowed to depend on them surviving.

## How the session stays durable without the agent holding storage

The agent persists nothing durable itself. As a session runs, each committed
session entry is **tee'd upstream** to the Server as a frame: the agent writes
its container-local ephemeral file normally (so the agent runtime's own loader,
compaction, and rewrites work), and the same committed write is streamed up to
the Server, which owns durability. (ledger DL-089)

The Server's durable store is **two-tier**:

- a **Postgres hot tail** holding the recent, resume-relevant slice of the
  transcript, and
- an **S3-compatible cold archive** that older, superseded segments flush out
  to as verbatim log segments.

So "the session streams out to durable object storage" is exactly right — the
durable home of a session's history is Server-owned Postgres plus S3-compatible
storage, never the sandbox's disk. (ledger DL-063, DL-093)

The durable transcript and the live trace you watch in the session log are the
**same artifact seen two ways** — the agent emits committed entries once, and
the Server both persists them and projects the live block-level trace from that
one store. (ledger DL-088)

## Resume: rebuild the compute, reattach the agent

Because the durable state lives in the Server and the compute is disposable,
bringing an agent back is a **reconstruct-into-a-fresh-sandbox** operation, not a
"find the old machine" one:

1. The old container/microVM is gone (torn down, evicted, crashed, or migrated).
2. On resume the Server **reconstructs the session** from its durable transcript
   store and hands that reconstructed session body to the Runner.
3. The Runner materializes it into a **new** container/microVM, and the agent
   picks up exactly where it left off.

The agent never notices it is running on different hardware. The identity was
never in the box; it was always in the Server. (ledger DL-086, DL-087)

## The compute substrate is moving: container → microVM

Today an agent's sandbox is a **rootless-podman container**, one per agent, for
blast-radius isolation (ledger DL-024). The end-state substrate is a
**hardware-virtualized microVM** — the same disposable-compute role, hardened to
a VM-class boundary around model-written code. The migration path is: container
through Dogfood and trusted-tenant Beta, microVM in the end state; a host without
KVM degrades to the container runtime with an explicit capability log, never
silently.

This move **does not change the durability contract on this page** — that is the
whole point of the split. Whether the sandbox is a container or a microVM, it is
still disposable, still custodies no durable state, and the agent still resumes
by the Server reconstructing its transcript into a fresh one. The substrate
change is an isolation upgrade (see [isolation and
egress](./isolation-and-egress.md)), not a change to what persists.

The direction is already load-bearing in shipped decisions — the microVM's KVM
floor is what retired local agent execution and made the native app a thin client
against a headless, KVM-capable stack (ledger DL-235), and the self-host stack is
a host-level bring-up on a KVM-capable Linux machine (ledger DL-259). The microVM
Runner backend itself is designed in
[`microvm-runner.md`](../designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md)
(RIG-2394), under the hosted-platform end-state record
[`compass-elastic-session-runtime`](../designs/infra/runtime/compass-elastic-session-runtime/design.md)
(RIG-1717).

## Why this split is the design, not an accident

- **Disposable compute is what makes hosted scale possible.** If the agent's
  state lived in the box, you could never evict an idle session, pack many
  sessions onto a host, migrate one to another machine, or reclaim a crashed
  sandbox without data loss. Because durable state is Server-owned, an idle
  session's environment can be torn down entirely and rebuilt on activity — the
  density play the hosted platform is built on.
- **Durable agents are what make supervision coherent.** A supervisor reasons
  about a stable fleet of named agents with continuous histories, not a churn of
  one-shot runs. The tree, the ownership trail, and the conversation history all
  assume the agent outlives its current process.
- **The boundary is a security boundary too.** Keeping durable state and
  credentials out of the sandbox means model-written code runs in a box that
  holds nothing of record and cannot exfiltrate what it never had — see
  [isolation and egress](./isolation-and-egress.md).

## Quick reference

| Thing | Durable? | Lives where |
| --- | --- | --- |
| Agent identity, owner, persona, tree position | yes | Server store (model fact) |
| Session transcript (reasoning, tools, edits) | yes | Server-owned: Postgres hot tail + S3-compatible cold archive |
| Home channel + its conversation history | yes | Server store |
| The container / microVM (the sandbox) | no | Runner host — disposable, rebuilt on resume |
| The container-local session file | no | dies with the sandbox (ephemeral working copy) |
| Storage / durability credentials | n/a | never in the sandbox — the agent holds none |
