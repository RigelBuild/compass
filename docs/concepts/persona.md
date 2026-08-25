# The persona convention

An agent's prompt is built in two layers. The **role** prompt is shared by every
agent of that role (every Manager gets the same Manager prompt). The **persona**
is an append-overlay on top of it that says *which* corner of the world this
particular agent works in. This doc is the convention for what belongs in a
persona.

## Role vs persona

- **Role** — the job. Selected by a role label that picks
  `config/prompts/<role>/SYSTEM.md`. One prompt file, many agents. Changing it
  changes every agent of that role.
- **Persona** — the standing context. Layered on top of the role prompt as an
  append-overlay, per agent. Two Managers share the Manager role prompt but each
  carries its own persona.

Both are set when the agent is first created and are stable for its life (see
[the lifecycle tools](./tools.md)).

## What a persona IS: stable working context

A persona names the **repos, projects, and lanes the agent works out of** — the
durable coordinates of its work:

- the repositories it owns or operates in,
- the projects / product areas it is responsible for,
- the lane it holds in the tree (what it coordinates, who it reports to in
  standing terms),
- standing conventions specific to its territory that the shared role prompt
  cannot know.

The test: **would this still be true after the current issue closes?** If yes,
it is persona.

## What a persona is NOT: the churn

A persona deliberately does **not** carry the specific issues the agent is
working right now. Those churn — an issue opens, ships, and closes on a scale of
hours to days, while the persona is stable for the agent's whole life. Putting a
`RIG-NNN` or a "currently doing X" line in a persona rots it immediately.

Keep out of a persona:

- specific issue IDs or "current task" descriptions,
- transient status ("blocked on review", "waiting on Matt"),
- anything that is true this week and false next week.

That churning state lives in the tracker (the issue, its `Owner:` line, its
status) and in the agent's live working memory — never baked into the prompt.

## Why the split matters

The persona is prompt-resident: it is injected every turn. Prompt-resident text
is expensive to keep correct — if it names a thing that changes, every change is
a prompt edit, and a stale prompt silently misdirects the agent. So the
convention is a hard line: **stable coordinates in the persona; everything that
moves in the tracker.** Get the line right and the persona is written once and
left alone; get it wrong and the persona becomes a second, lying copy of the
board.

## Example

A good persona (stable):

> Works out of `RigelBuild/compass` (server lane) and the Compass Linear
> project. Owns the server's forge-write and comms substrate. Reports up to the
> root supervisor.

A bad persona (churns — belongs in the tracker):

> Currently implementing RIG-2257 answer-as-message; blocked on Matt's freeze of
> #589; next up RIG-2680 docs.
