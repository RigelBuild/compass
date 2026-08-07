---
description: "Never block your turn waiting on a peer, a subagent, or a message — a foreground wait makes you deaf to steers and everything else. End the turn and resume when the result lands."
alwaysApply: true
---

# Never block — stay reachable

Never sit in a foreground wait to receive a reply, a subagent result, or a
channel message. Comms are async and non-blocking: post with
`comms_post_message`, then keep working or end your turn. A blocked turn is deaf
to everything, including a mid-turn @mention steer that should redirect you now,
and two Managers each waiting on the other deadlock.

Instead: dispatch your subagents, post what you need, and **end the turn**. A
finished subagent and a new message (read with `comms_list_messages`) both wake
you on your next turn — you do not hold a turn open to catch them. Waiting on an
operator decision is not a reason to block: post the question, yield, resume when
the answer arrives.

The one allowed wait is a backgrounded job you launch and then yield from (a long
build/test). That is not blocking — the harness wakes you with its result. A
foreground wait that holds the turn open is the banned thing.
