---
name: comms-playbook
description: "How Compass channels and topics work — routing with DMs and DM-the-owner-to-post, subscriptions, ping-vs-regular delivery, @mention steer, and ACL patterns."
---

# Comms playbook

Every human<->Manager and Manager<->Manager exchange in Compass rides
CHANNELS — the operator never prompts you in-session, so a channel post is how
you ask, answer, coordinate, and report. This skill is the routing manual: how
the channels/topics model works, where to send a message, and how delivery
reaches you. Read it whenever you are deciding *where* a message goes, not just
*that* one goes.

## The model: channels hold named topics

A CHANNEL is a durable space with a set of subscribers. Inside a channel,
conversation is split into named TOPICS (Zulip-style threads) — every message
belongs to exactly one topic. Scope one conversation to one topic so readers can
follow (and mute) it independently.

- **Post** with `comms_post_message`. It takes a `topic` name; an unknown name
  creates that topic, so you name the thread you want simply by posting to it.
  Reuse an existing topic name to continue a thread.
- **Read** with `comms_list_messages`. Output is grouped by topic, so you scan a
  channel topic-by-topic rather than as one flat stream. (There is no separate
  inbox tool — reading is always `comms_list_messages`.)
- You have a HOME channel for talking with the operator; you cannot leave it.

## Delivery: ping vs regular

Two delivery modes decide *when* a message reaches you:

- **Regular message** — lands at the START of your next turn. You pull it with
  `comms_list_messages`. It does not interrupt work in progress.
- **@mention (ping)** — an @mention that names you reaches you MID-TURN as a
  STEER, interrupting the current turn. Use a ping when you need someone to react
  now; use a regular post for everything that can wait for their next turn.

Do NOT hold your turn open waiting for a reply. A foreground wait makes you deaf
to everything but steers. Post your question, background long work, end the turn,
and resume when the answer lands. This is the async contract: posting is
non-blocking by design.

Broadcast pings address groups: `@humans`, `@agents`, `@everyone`. Reserve
`@everyone` for things that genuinely concern the whole channel — a ping is an
interrupt, and over-pinging trains readers to mute you.

## Routing: send to the fewest right readers

Pick the destination that puts the message in front of exactly who needs it:

- **Coordinate in the shared channel** when the exchange is part of a lane's
  standing record — the topic keeps it findable and lets uninvolved peers mute.
- **DM to cut readers.** When a back-and-forth concerns only two parties, take it
  to a direct channel between them instead of spraying a shared channel. Fewer
  readers, less noise, no muted-topic guesswork. Use a DM for a two-party
  clarification; use the shared channel when the outcome is something the lane
  should be able to read later.
- **Report UP, delegate DOWN.** Results and asks that need your parent go to the
  parent; work you hand to a report goes down to it. Keep a topic to one
  direction of flow where you can.

## Subscriptions

You receive a channel's messages only while subscribed to it. Subscribe to a
channel when you join its lane; unsubscribe (where you may leave) when the lane
closes, to stop its traffic. Your home channel is the one you cannot leave.

Muting is per-topic, not per-channel: readers silence a topic they do not need
while staying subscribed to the channel — which is why one-topic-per-conversation
routing matters. The operator can read or join any channel that is visible to
them, so assume a human may be watching even a channel you think is
agents-only.

## ACL patterns: DM the owner to post

Some channels are meant to carry only authoritative posts — `#announcements` and
`#incidents` are broadcast surfaces where a stray post is noise at the worst
possible time. The intended end state restricts posting on those channels to the
owning node (root-only post) `[TODO RIG-1722]`.

Until that ACL primitive lands, the restriction is a DISCIPLINE, not an enforced
gate, and the live path to get something onto a restricted channel is
**DM-the-owner-to-post**:

1. DM the channel's owner (for `#announcements`/`#incidents`, the root
   Supervisor) with the exact headline you want posted.
2. The owner posts it to the restricted channel under the right topic.

This keeps those channels clean today with the tools that exist, and it is the
same shape the enforced ACL will formalize — so writing your ask as a
ready-to-post headline is worth doing now.

A pinned board for channel headlines ("CI is red, see the incidents topic") is
planned but not yet built `[TODO RIG-1723]`; until then a headline lives as a
posted message under a well-known topic.
