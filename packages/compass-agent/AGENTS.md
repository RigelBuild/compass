# compass-agent — package contract

The first-party in-container agent: it drives an `AgentSession` from
`@oh-my-pi/pi-coding-agent`, maps each session event to a `compass.v1`
`AgentFrame` on the outbound sink, and applies inbound `AgentControl` frames
from the Runner. This file is the load-bearing contract a change to the package
must not silently break.

## No promptable session

**The Compass agent has no promptable session.** There is no composer and no
input box. The session log is operator-visible for **monitoring and stop
only** — watching what the agent does moment to moment, and interrupting it.
There is **no reply path through the session**.

All operator↔agent communication flows through comms **channels and topics**
(the agent's home channel + named topics), never the session. This is the
deliberate break from the traditional agentic session loop, and it is what the
rest of this package is shaped around:

- An **ask** is a typed comms message, not a session dialog. The agent raises
  one with the `comms_post_ask` tool, which posts `PostMessage(MessageBlock{ask})`
  onto a channel topic and returns immediately with a server-minted `ask_id`.
  The call is **async and non-blocking**: the agent posts its questions and
  continues; it never awaits the answer within the turn.
- The **answer** returns asynchronously as an `AskAnswerControl` on the control
  lane, correlated by `ask_id`, and is delivered to the model as a prompt on a
  later turn.
- The agent **never answers an ask**. Answering is the human side of the
  conversation; the prohibition is structural, not a convention — the comms
  request oneof cannot express `RespondToAsk`.

The agent role delta already tells the model this ("async comms / no `ask`",
DECISIONS.md DL-139); this file is the package-code contract behind it, not a
restatement of the role prompt.

## The comms toolset

Five native comms tools ship (`src/comms.ts`), none of them ask-answering:

- `comms_post_message` — post a markdown message to a channel topic.
- `comms_post_ask` — raise a structured ask (async; the answer arrives on a
  later turn).
- `comms_list_messages` — read a channel's recent messages.
- `compass_roster` — list the agent's neighborhood/subtree/owner roster.
- `compass_set_status` — set the agent's presence activity.

`comms_post_ask` mints each `AskOption.id` as the option's zero-based index
(a decimal string) — the native SDK ask option carries no id, and the id is the
referent an answer's `chosen_option_ids` echoes back. Server-owned fields
(`ask_id`, `answered`, and every answer field) are never set client-side.

## Answer liveness — a temporary limit

The answer wake is delivered to whatever session is live for the asking agent
at the moment the operator answers. Until the runner/hub owed-to-handle
delivery lands (tracked as RIG-2257 — key the wake on the agent handle, not the
session id), an answer submitted while the asking agent is **not live** (offline,
or after a relaunch minted a new session id) is missed: the answer is durably
recorded on the ask, but its push to the agent does not fire and is not
redelivered.

Recovery is a human decision at answer time — relaunch the asking agent (it
receives the owed answer on its next live session) or route the answer to a new
agent. There is deliberately **no agent-side boot poll** for stale answers: the
agent's pending-ask registry is in-memory within a live session, and an
`ask_id` with no live registry entry is surfaced (a counted unmapped op), never
a fabricated answer and never a silent drop.

## Control application is at-least-once

Inbound control ops are acked on **apply-then-ack**: an op is acked only when
the control loop returns to pull the next one, so an arm that returns normally
is acked and retired from Runner retention. An arm that cannot yet apply an op
(e.g. an `askAnswer` that arrives before `ReplayComplete`) **throws** rather than
returning — a thrown apply is not acked, so the Runner redelivers the op to the
next session. Returning a counted "refusal" for such an op would ack and
permanently drop it. See `src/transport/control-source.ts` for the seam.
