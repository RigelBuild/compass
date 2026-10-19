# Ask mirroring and channel subscriptions

## Requirement: Users control ask mirroring to forge coordinates

`ListForgeMirrorSettings` SHALL list every enabled forge repository and its
current mode. Any authenticated account may list settings. The response SHALL
set `can_edit` only for a tenant admin. `SetForgeMirrorSetting` SHALL require a
user with the tenant admin role; other callers receive permission-denied.
An UNSPECIFIED mode SHALL be rejected with invalid-argument. Setting OFF SHALL
remove the stored setting, and an absent setting SHALL behave as OFF.

### Scenario: An admin configures automatic ask mirroring

- **Given** an enabled forge repository and a tenant admin
- **When** the admin sets its mode to AUTO
- **Then** the setting is returned by the list RPC with `can_edit` true

### Scenario: A queued ask is refused after the mode is turned off

- **Given** an ask for an AUTO coordinate waiting for delivery
- **When** an admin turns the coordinate OFF before the post
- **Then** the mirror is marked failed and no forge comment is created

### Scenario: An answer edits the existing mirrored ask

- **Given** an ask with a delivered forge mirror
- **When** a user answers the ask
- **Then** the same forge comment is edited to include the answer and answerer

The UI SHALL list every enabled forge coordinate in Settings with an Off,
Manual, or Auto select, editable only by a tenant admin. An ask and a topic
header SHALL offer an inline ref field that calls `MirrorToForge` and shows the
comment URL or the refusal beside the source.

`MirrorToForge` SHALL accept exactly one ask, topic, or one-to-fifty-message
source. The caller SHALL be a user who can see the source channel. The destination
mode SHALL be MANUAL or AUTO. The agent scope check applies only to agent posts.
An invisible channel SHALL return not-found. An agent caller SHALL receive
permission-denied. Invalid source combinations SHALL receive invalid-argument.
The user header SHALL name the clicking user and source text SHALL not be
allowed to forge an owner header. A second user mirroring the same source to the
same ref SHALL be refused while the first user's mirror exists.

Topic mirrors SHALL carry at most the newest 50 messages. If a topic has more
than 50, the mirror SHALL state, "Showing the newest 50 messages."

## Requirement: Agent mirror tools may mirror visible messages to AUTO forge targets

The agent `forge_mirror_messages` tool SHALL accept exactly one source: a topic
or one to fifty message IDs from one channel. The caller SHALL be able to see
the source channel. The tool SHALL check the target with the agent mirror policy
and SHALL post only when the target is in AUTO mode; MANUAL and OFF SHALL be
refused. The server SHALL render a topic's newest 50 messages, include the
optional note, drop whole oldest messages when the forge body limit requires it,
and state how many were dropped. Re-mirroring the same source to the same target
SHALL edit the existing comment. A completed retry with the same
`client_request_id` SHALL return the stored comment reference without posting
again; a pending retry SHALL return unavailable, and a failed retry SHALL
return its stored error. A target that is a tracked pull request SHALL return a
PR-comment result; another target SHALL return an issue-comment result.

The optional `channel` selector SHALL be a channel name, never a channel ID.
When it is omitted, the server SHALL take the channel from the topic or the
messages. When it names a channel that does not hold the source, or one the
agent cannot see, the server SHALL return not-found. Every agent mirror post
SHALL name the agent's real posting session in its owner header; a row with no
session SHALL fail without posting. The server SHALL store the source on the
row, so a mirror still pending at a restart is redriven with the same body.
A redrive SHALL check visibility again, and SHALL fail the row without posting
when the agent can no longer see the source.

### Scenario: A retry returns the first mirror result

- **Given** an agent mirror request that has already delivered a comment
- **When** the agent retries it with the same `client_request_id`
- **Then** the server returns the stored comment reference and the forge sees
  only one post

### Scenario: A topic is mirrored again to the same target

- **Given** a topic already mirrored to a forge target
- **When** the agent mirrors that topic to the same target again
- **Then** the server edits the existing comment instead of creating another

### Scenario: An agent cannot mirror to a MANUAL or OFF target

- **Given** a forge target configured as MANUAL or OFF
- **When** an agent calls `forge_mirror_messages` for that target
- **Then** the server refuses the call without posting a comment

### Scenario: An agent cannot mirror messages from a channel it cannot see

- **Given** a source topic or message set in a channel the agent cannot see
- **When** the agent calls `forge_mirror_messages`
- **Then** the server returns not-found without revealing the channel or messages

### Scenario: A pending mirror is redriven after a restart

- **Given** an agent mirror row that was claimed but not posted before a restart
- **When** the restarted server drives the agent's open rows
- **Then** it posts the same body, stamped with the session that made the call

## Requirement: Automatic mirroring follows the ask's explicit ref

An ask with an explicit ref to an enabled AUTO coordinate SHALL be mirrored
after its message commits. An ask with no ref, or whose coordinate is OFF or
MANUAL, SHALL NOT be automatically posted. The server SHALL check mode again
immediately before every post; a coordinate turned OFF while queued SHALL not
be posted. Answering an ask SHALL update the same forge comment with its answer
and answerer rather than create a second comment. A pending or failed mirror
SHALL be re-driven on the agent's next board write.

## Requirement: Channel membership carries a separate subscribe opt-in

`UpdateChannelMembers` SHALL manage both the channel's member set and each
member's subscribe flag. Joining a channel grants read access (the member
appears in the channel's members); subscribing is a separate per-member opt-in
(the member additionally appears in the channel's subscribers). One RPC covers
join, subscribe-toggle, and removal.

One comms behavior is designed but **not yet built**, so it is out of scope for
this spec until it lands (behind its own design amendment): **reserved-ping
resolution** (`@agents`/`@users`/`@everyone` expanding to a member set) — no ping
contract exists yet.

The `snapshot_seq` **consistent point-in-time snapshot** across a paginated
re-snapshot is **partially built**: the server half has landed (RIG-1333) — the
`SubscribeComms` `since_seq=0` response carries a leading `snapshot_seq` boundary
frame and the message read RPCs enforce it as `seq <= snapshot_seq`. The
normative Requirement (the boundary-frame contract + narrowing this reserved
slot) is deliberately held until the client-consumption seam is confirmed, so
the spec is not flipped to "built" mid-seam.
