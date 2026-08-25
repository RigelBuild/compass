# No human clicks

The whole org an agent operates in — accounts, agents, channels, channel groups,
subscriptions, the tree itself — must be **standupable by agents through tools**.
The only surface reserved for a human is the **security boundary**. This is the
central design principle behind the org-management tool set, and the
mesh-internal twin of the infrastructure rule that a human's only step is
reviewing and merging a PR.

## The principle

If an agent needs an org structure to exist — a channel to coordinate on, a
channel group to namespace it, a sub-Manager to delegate to, a subscription to
receive a lane's traffic — the agent creates it, with a tool. No human opens a
console, clicks through a settings page, or runs a one-off command to make it
exist. An org operation that *requires* a human click is a **defect to design
out**, not a normal step.

The one thing an agent cannot do is cross the security boundary. Concretely:

- **An agent may DECLARE a secret by name.** "I need a credential called
  `LINEAR_FORGE_CLIENT_SECRET`" is something an agent states.
- **A human provides the VALUE for that named slot.** Supplying the actual
  secret value is the human's sole step — the one action reserved to a person,
  because it is the security boundary.

Everything up to that boundary — declaring what should exist, creating the
non-secret structure, wiring the shape — is agent work through tools.

## Why this shape

Two reasons it is a hard principle and not a nice-to-have:

- **It scales.** A fleet of agents that spawn and despawn per lane cannot wait
  on a human to click a console for each channel or sub-agent it needs. Making
  the org self-serviceable through tools is what lets the tree grow and
  reshape at agent speed.
- **It keeps the human on the one thing only a human should do.** By routing
  *everything* except secret-value provision through tools, the human's
  attention is spent entirely on the security boundary — the place where their
  judgment is actually load-bearing — instead of on mechanical setup a tool
  should have done.

## The two halves

| Surface | Who does it |
| --- | --- |
| Create a channel / channel group | agent (tool) |
| Spawn / despawn a standing agent | agent (tool, operator-approved for a child Manager) |
| Subscribe / unsubscribe to a lane | agent (tool) |
| Declare that a secret is needed, by name | agent |
| Provide the secret's value | human (the security boundary) |

## The infrastructure twin

This mirrors the repo's infrastructure posture exactly: state is declared in
code and applied by a merge to `main`, so a human's only step is reviewing and
merging the PR — never clicking a cloud console. The read half of external
systems follows the same logic ([read-only
inspection](./read-only-inspection.md)): agents get a wide read-only window and
route every mutation through the one human-reserved gate. Inside the mesh, the
org-management tools are that same principle applied to the agent org itself.
