# The comms model: threads for conversation, the session log for work

The single most load-bearing idea about how a Compass agent communicates, and
the one most easily gotten backwards when bridging an external system in. An
agent has **two distinct surfaces**, and they are not interchangeable:

- **The session log** — the agent's *work*: its streamed reasoning, tool calls,
  code, narration. A live stream you watch in a side-panel log view. It is a
  projection of the agent's session, high-volume and moment-to-moment.
- **Channels and threads** — the agent's *communication*: asking, answering,
  coordinating, reporting. Every human↔agent and agent↔agent exchange rides a
  **channel**, and every message belongs to exactly one **topic** (a
  Zulip-style thread). This is the deliberate, human-facing surface.

## Why the split exists

Communication is kept out of the log, and work is kept out of the threads, on
purpose. If the two were one flat stream, every real exchange would drown
between long stretches of tool calls and code, and no one could follow a
conversation or track several in parallel. Splitting them keeps each
conversation **scoped and legible**: the log is where you watch an agent work,
the thread is where the exchanges that matter live, indexed per conversation.

This is **structural, not a prompting convention**. The only path that writes a
comms message is an explicit post; a streamed turn writes nothing to comms. An
agent cannot flood a channel just by thinking out loud — it must deliberately
post. See the [Zulip threading model](../designs/product/compass-zulip-threading-model/design.md)
record (ledger DL-098, DL-099) and [session persistence](../designs/product/compass-agent-session-persistence/design.md)
(the log and the live trace are two projections of one artifact, DL-088).

## The session log is read-only — you never prompt into a session

This is the departure from standard agent models most likely to trip up an
integration or a newcomer: **there is no way to prompt an agent directly into
its session.** The session log is a *read-only live view* — you watch the
agent's streamed output there, but nothing you do in it reaches the agent.
Every input to a running agent goes over comms, and there are exactly three
ways to reach a live session:

- **Post in a thread** — a normal comms message on the agent's home channel,
  which the agent picks up on its turn.
- **Ping the agent for a steering message** — a mid-turn nudge to a working
  agent, comms-originated (a channel post) and delivered over the control lane
  as a mid-turn injection, never typed into the read-only log surface.
- **Stop the agent** — halt the running session if needed.

Standard agent models assume a direct prompt-into-the-session REPL; Compass
does not have one, on purpose. Every assumption a bridge or a tool carries
about "send the user's text straight to the agent" must be re-expressed as one
of the three above: the session log streams *out*, and communication flows
*in* only through channels and threads.

## Stable agents with home channels

Compass agents are **long-lived tree nodes** — Managers and their peers — each
minted with a **home channel** it is always subscribed to, not ephemeral
one-shot runs. A conversation about a given issue is a **topic in the owning
agent's home channel**, and the agent's turns are driven by messages delivered
on that channel. The agent workspace surface is exactly these two panes: the
home channel and the session trace (ledger DL-158).

## Typed communication, not a flat chat line

Because communication is a first-class surface rather than a log tail, a comms
message can carry **typed blocks** richer than plain text — most notably a
structured **`ask`** (a question with discrete answer options): it is *posted*
over the comms lane like any message, and its answer is *correlated back* over
the control lane (see [the ask round-trip](../designs/product/compass-ask-comms-roundtrip/design.md),
ledger DL-211). More typed block kinds are planned. These are native to
Compass's comms surface and do not necessarily project onto a flatter external
message vocabulary.

## The contrast that matters for integrations

Compass deliberately does **not** use the shape common to external
agent-session protocols: *a single ephemeral session spun up per task, with
everything — work and conversation alike — dumped into one flat, unthreaded
log, and no stable agent behind it.* That flatter model is what the
threads/log split above exists to reject.

So when an external agent-session protocol (for example Linear's Agent Session)
is bridged into Compass, it is **not adopted wholesale**. The external session
is mapped *onto* the Compass model — routed to a stable owning agent, its
conversation carried as a topic in that agent's home channel, its work left in
the session log — never allowed to replace that model with its own flat one.
Any such bridge starts from this premise, and a fixed external activity
vocabulary (one that cannot express Compass's typed blocks) is a boundary to
design around, not a reason to flatten the Compass side to match.
