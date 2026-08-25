# Read-only inspection of external state

Agents need to *see* the state of the systems they operate — the cloud
resources, the SaaS dashboards, the infrastructure. They do not need to
*mutate* it by hand: mutation goes through code and a merge (see
[no-human-clicks](./no-human-clicks.md)). This doc covers the read half — how an
agent inspects external state without a per-agent credential and without a human
clicking a console for it.

## One shared read-only bot per system

Inspection access follows the same shape as the forge account: **one shared,
read-only identity per external system**, not one per agent. A single read-only
bot account (or read-scoped token) for a SaaS provider — Pulumi Cloud, a
metrics/observability backend, a cloud console — is custodied once and reachable
by every agent that needs to look.

- **Read-only, always.** The shared inspection identity carries read scopes
  only. It can list stacks, read resource state, query dashboards, pull logs —
  never create, update, or delete. The mutating surface is fenced off from it
  entirely, so a shared read token cannot become a shared write footgun.
- **Shared, like the forge account.** The number of agents is a Compass-internal
  detail; the provider prices one read-only seat, not one per handle.

The live example is the Pulumi Cloud access agents already have: a read-only
inspection surface (list stacks, read resource state) with the mutating surface
deliberately unavailable. Any new SaaS an agent needs to observe follows that
template — a read-only shared bot, wired in as an inspection tool.

## Why read is agent-standupable but write is not

Reading external state is safe to hand an agent broadly: the worst case of a
read is a wasted call. Writing is where the security boundary sits, so writes go
through infrastructure-as-code and a human-reviewed merge, never a live
credential in an agent's hand (see [no-human-clicks](./no-human-clicks.md)).

That asymmetry is the whole design: give agents a wide, cheap, read-only window
onto everything they operate, and route every mutation through the one
human-reserved gate. An agent that needs to *know* the state reads it directly;
an agent that needs to *change* the state writes the code that changes it and
opens the PR.

## Declaring a new inspection surface

When an agent needs to observe a system it cannot yet see, the fix is to wire a
read-only bot for that system — declared by name, its read-scoped credential
provided once by a human into the named slot (the secret-by-name contract in
[no-human-clicks](./no-human-clicks.md)). The agent never holds the console; it
holds a read-only tool that speaks to the shared bot.
