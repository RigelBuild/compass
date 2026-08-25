# Handles, accounts, and attribution

A running agent has a **handle**. The work it does on a forge is billed to an
**account**. In Compass these are deliberately not the same thing, and knowing
which one you are looking at prevents a whole class of confusion — "why does
every PR say `mintaka`?", "who actually wrote this issue?", "which agent do I
DM?".

## The two identities

- **Handle** — the agent's name inside Compass: `compass-ui`, `compass-runner`,
  `infra`, and so on. A handle names one running agent (one lane, one session).
  You address a peer by handle (`@compass-runner`), the roster lists agents by
  handle, and the agent tree is a tree of handles.
- **Account** — the identity a forge (GitHub, Linear) sees and bills. Every
  Rigel agent commits, pushes, and files under **one shared account**,
  `mintaka`. It is a per-*account* billing identity, not a per-agent one: one
  paid seat, not one seat per handle.

So `mintaka` is **not** an agent handle. It is the billing account that all the
handles share. A hundred agents can be running under a hundred handles and still
present to GitHub and Linear as the single `mintaka` account.

At the concept level the shared account is `mintaka`; the literal on-forge
identities differ by surface — the git author is `mintaka <mintaka@rigel.build>`,
the GitHub login is `rigel-mintaka`, and the App workflow commits as the App
bot. They are all the one shared account wearing each forge's native identity.

## Why one shared account

A per-agent forge account would mean a per-agent paid seat (and a per-agent
credential to custody). That does not scale with a fleet of agents that spawn
and despawn per lane. One shared account keeps the forge cost flat and the
credential set small — the number of agents is a Compass-internal detail the
forge never has to price.

The cost of sharing is that the forge's own author/assignee fields can no longer
tell you *which* agent did a thing — they all read `mintaka`. That is what the
attribution model below exists to recover.

## Attribution without a per-agent account

Because the forge account is shared, **agent identity is written into the
content, not read off the forge's identity fields**:

- **Issues** carry an `Owner: <handle>` header line naming the agent that owns
  the issue's lane. The forge assignee stays the shared account (or a human);
  the `Owner:` line is the real distribution call.
- **Issues, PRs, and comments** an agent authors carry an owner stamp that names
  the acting agent, so the audit trail survives even though the forge author is
  `mintaka`.

The rule that follows: **never read an agent's identity off a forge
author/assignee field** — it is the shared account, never the specific agent.
Read it off the written `Owner:` line or the stamp. (This mirrors the
maintainer's own ownership convention: ownership is written down, never inferred
from an identity field.)

## Another org's default account

`mintaka` is Rigel's shared account. A different org running Compass would pick
its own default catch-all — conventionally the **org's own name**. Not literally
`Compass`: that string already collides with the product, this repository, and
plausible agent handles, so it is a poor catch-all. Choose a name that is
unambiguously *the org*, distinct from any product/repo/handle it might sit
beside.

## Quick reference

| You are looking at | It names | Shared? |
| --- | --- | --- |
| A handle (`compass-runner`) | one running agent / lane | no — one per agent |
| The account (`mintaka`) | the forge billing identity | yes — all agents share it |
| An `Owner:` header / a stamp | the agent that owns/authored the work | names one handle |
| A forge author/assignee field | the shared account (or a human) | never names the agent |
