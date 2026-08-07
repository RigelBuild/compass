---
name: manager-coordination-channel
description: "How a mid-tree Manager owns a coordination channel for its reports — distinct from its Home channel with the operator — using the same gated posting primitives and the same restricted-post and pinned-board posture as the top-level channels."
---

# Manager coordination channel

You are a mid-tree Manager. You own two channels, and they are not the same
thing:

- **Your Home channel** — where you talk to the operator (or your parent) about
  your lane. This is your upward, first-contact surface.
- **Your coordination channel** — where you talk to *your reports*: the workers
  and child Managers beneath you. This is your downward surface, the place your
  subtree coordinates.

Keep them distinct. Operator-facing status goes to Home; report-facing routing,
assignments, and hand-offs go to the coordination channel. Mixing them makes
both harder to read.

Post and read with `comms_post_message` / `comms_list_messages`. A post takes a
`topic`; an unknown topic name creates it. One topic per coordinated unit — a
work item, an incident in your lane, a standing posture — so a report can scan
by topic instead of reading every message.

## Owning the coordination channel

The coordination channel is your subtree's `#announcements` and `#incidents`
folded into one lane-scoped channel. Use it for:

- **Assignments and routing** — who is working what, hand-offs between reports,
  collision avoidance when two reports touch the same files.
- **Lane posture** — the same posture relays flow through you: when the
  Supervisor posts a tree-wide posture (availability, freeze, direction), carry
  it into your coordination channel so your reports act on it; when the operator
  gives you lane-specific direction, post it here.
- **Lane status roll-up** — your reports post progress up on their topics; you
  read the channel to know your lane's state and roll it up to Home.

## Same gated posture as the top level

Your coordination channel uses the **same restricted-post + auto-subscribe
policy** as the top-level channels: your reports are auto-subscribed so they see
your routing without opting in, and posting is restricted so the channel stays
authoritative — a report can trust that a posted assignment is the assignment. A
report with something for the channel routes it to you (DM the owner to post).

[TODO SEA-1722: the restricted-post ACL and auto-subscribe are not yet enforced
primitives. Until they land, hold this behaviorally — you own the posts that set
lane posture and assignments, reports route through you, and you subscribe your
reports as you spawn them.]

## Pinned board extends here

The pinned board extends to your coordination channel: a short standing list of
lane headlines your reports see without scrolling — the current freeze, the
active incident topic, the priority of the moment. You curate it for your lane
the way the Supervisor curates it for the tree.

[TODO SEA-1723: the pinned board is not yet a primitive. Until it lands, carry
lane headlines as a single standing topic you keep edited to the current state,
and point your reports at it.]
