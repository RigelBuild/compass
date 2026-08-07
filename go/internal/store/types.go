// Package store is the Server's Postgres store of record: accounts, channel
// groups, channels, agent workspaces, conversation messages, and issued-token
// hashes, all durable in Postgres from day one (compass v0.6 design.md T1).
// Postgres is THE substrate — no component treats the in-memory event bus as
// the store of record (Global Constraints); the bus is a live-fan-out cache in
// front of this store, and a reconnecting client that outruns the ring
// re-snapshots through these read methods.
//
// The types here are store-native Go that mirror the compass.v1 wire messages
// (design.md:1184-1185) rather than the generated stubs: the comms service (T2)
// and the auth layer (T3/T4) map proto ↔ store at their edge, so this package
// depends on no generated code and the two evolve independently. Search is
// Postgres full-text over message text (design.md:1137-1139) — the audit/search
// property served from the store of record, not a separate engine.
//
// Every read that a caller can see less than all of takes a visibleTo/actor
// AccountID and scopes its result to that account's visible set server-side
// (comms.proto:31-37, D9 owner-gated access): visibility is a store property,
// never a client-supplied filter.
package store

import "time"

// AccountID, ChannelID, ChannelGroupID, WorkspaceID, and MessageID are the
// server-assigned stable ids for their rows. Distinct named types (not bare
// strings) so a channel id can never be passed where an account id is meant —
// the compiler catches the transposition the wire's uniform `string` cannot.
type (
	// AccountID identifies a user or agent account.
	AccountID string
	// ChannelGroupID identifies a channel group (a namespace node).
	ChannelGroupID string
	// ChannelID identifies a channel.
	ChannelID string
	// WorkspaceID identifies an agent workspace (the observation pane).
	WorkspaceID string
	// MessageID identifies a message row.
	MessageID string
)

// UserRole is a human account's permission role (comms.proto:127-130). The
// zero value is the least-privilege member; admin is an explicit elevation.
type UserRole int32

const (
	// UserRoleMember is a regular member — the default, least privilege.
	UserRoleMember UserRole = 0
	// UserRoleAdmin is an explicit elevation with management rights.
	UserRoleAdmin UserRole = 1
)

// ChannelGroupVisibility is a group's own visibility (comms.proto:175-180). A
// child group's value may be no more open than its parent's, and a node's
// effective visibility is the most restrictive value on its path to the root
// (D9). The zero value is owner-scoped — private unless explicitly opened.
type ChannelGroupVisibility int32

const (
	// VisibilityOwner is visible only to the owning user and that user's agents.
	VisibilityOwner ChannelGroupVisibility = 0
	// VisibilityShared is visible to all accounts (announcements, collaboration).
	VisibilityShared ChannelGroupVisibility = 1
)

// ChannelKind distinguishes a plain channel from a direct or group DM
// (comms.proto:199-203). For DM/GROUP_DM, membership grants visibility
// regardless of the group lattice (design.md:235-243); a plain channel is
// lattice-governed.
type ChannelKind int32

const (
	// ChannelKindChannel is a plain channel — the default, lattice-governed.
	ChannelKindChannel ChannelKind = 0
	// ChannelKindDM is a direct conversation between two accounts.
	ChannelKindDM ChannelKind = 1
	// ChannelKindGroupDM is a multi-party direct conversation.
	ChannelKindGroupDM ChannelKind = 2
)

// SubjectKind tags a token's subject class so a Runner subject and an account
// subject share the token store but never collide — the prefix-separation the
// auth layer (T4) depends on. A resolved token carries its kind, so a door can
// reject a cross-kind token (a Runner token on CompassService/CommsService, an
// account token on RunnerService). Sealed to exactly these two (design.md:
// 1175-1183).
type SubjectKind int32

const (
	// SubjectAccount is a user or agent account token subject.
	SubjectAccount SubjectKind = 0
	// SubjectRunner is a provisioned-Runner token subject.
	SubjectRunner SubjectKind = 1
)

// Subject is a token's authenticated principal: its kind plus the id of the
// account or Runner it authenticates. ResolveTokenHash returns it with the kind
// set so a cross-kind token is rejected at the door, not silently accepted.
type Subject struct {
	Kind SubjectKind
	// ID is the AccountID (SubjectAccount) or the Runner id (SubjectRunner), as
	// a bare string because it spans two id spaces.
	ID string
}

// Account is a communication-layer account: a human user or an owned agent
// (comms.proto:107-118). Exactly one of User / Agent is non-nil, mirroring the
// wire `kind` oneof; the accessor helpers below make the discriminant explicit
// at call sites.
type Account struct {
	ID          AccountID
	Handle      string
	DisplayName string
	// User is set for a human account, nil for an agent.
	User *UserAccount
	// Agent is set for an agent account, nil for a human.
	Agent *AgentAccount
}

// IsAgent reports whether this account is an owned agent subtype.
func (a Account) IsAgent() bool { return a.Agent != nil }

// UserAccount is the human-account payload: a permission role (comms.proto:
// 121-123).
type UserAccount struct {
	Role UserRole
}

// AgentAccount is the owned-agent payload (comms.proto:136-142) plus the
// additive home_channel_id (RT-2): the agent's named channel, minted at
// CreateAgent, that fixes "the agent's own channel" for the always-subscribed
// row, turn-end delivery, and the observation-pane ACL.
type AgentAccount struct {
	// OwnerUserID is the owning user; server-set to the creating caller.
	OwnerUserID AccountID
	// HomeChannelID is the agent's home channel, minted at creation (RT-2).
	HomeChannelID ChannelID
	// Persona is the agent's system-prompt text, baked at creation (SEA-1571);
	// empty means no persona override.
	Persona string
	// Role is the agent's operator-set block-0 selector (SEA-1732 T10); empty
	// means no role (default OMP block-0). Unlike Persona (an append overlay),
	// the label selects config/prompts/<role>/SYSTEM.md, delivered as the
	// container's customSystemPrompt.
	Role string
	// ParentAgentID is the agent's parent in the agent tree; empty = root. Set
	// at creation and editable via ReparentAgent (comms.proto).
	ParentAgentID AccountID
}

// ChannelGroup is a namespace node holding channels and nested groups
// (comms.proto:155-169). Its own visibility may be no more open than its
// parent's; effective visibility is the most restrictive on the path to root.
type ChannelGroup struct {
	ID ChannelGroupID
	// Name is the leaf segment of the namespace, e.g. "matt".
	Name string
	// ParentGroupID is the parent group; empty for a top-level group.
	ParentGroupID ChannelGroupID
	// OwnerUserID is the user whose space this group is; empty for a shared
	// group. Server-set to the creating caller.
	OwnerUserID AccountID
	Visibility  ChannelGroupVisibility
}

// Channel is a named conversation within a group (comms.proto:183-195). Per the
// OQ-C narrowing the container surface is channel-only, so this is the sole
// message container.
type Channel struct {
	ID ChannelID
	// Name is the leaf name within the group, e.g. "coordination".
	Name string
	// GroupID is the owning group; empty for an ungrouped channel, which is
	// owner-scoped to its creating caller (the OWNER default), not global.
	GroupID ChannelGroupID
	Kind    ChannelKind
	// MemberAccountIDs are the accounts party to the channel. For DM/GROUP_DM
	// this set governs visibility directly (design.md:235-243).
	MemberAccountIDs []AccountID
	// SubscriberAccountIDs is the subset of members opted in to push delivery
	// (the per-member subscribed flag, RT-1). For an agent, a plain message in a
	// subscribed channel is delivered at its turn end; a joined-but-unsubscribed
	// member has read access only. A subset of MemberAccountIDs.
	SubscriberAccountIDs []AccountID
	// Policy carries the manager-comms channel-policy fields (T4): the post
	// policy, the owner/operator account, and the mandatory-subscription flag.
	// The zero value (PostPolicy OPEN, empty OwnerAccountID, MandatorySubscription
	// false) is the pre-substrate default every channel is born with.
	Policy ChannelPolicy
}

// ChannelPolicy is the manager-comms substrate's per-channel policy (T4,
// design.md:496-502): who may post (PostPolicy), the owner/operator account for
// policy operations (OwnerAccountID, empty when OPEN), and whether membership
// implies a non-togglable subscription (MandatorySubscription). Set at creation
// via NewChannel and thereafter mutated only through SetChannelPolicy.
type ChannelPolicy struct {
	PostPolicy            ChannelPostPolicy
	OwnerAccountID        AccountID // empty when OPEN
	MandatorySubscription bool
}

// ChannelPostPolicy is the store-native mirror of comms.proto's
// ChannelPostPolicy enum: who may post to a channel. Kept store-native (like
// ChannelKind) rather than the generated type so the store depends on no
// generated code (types.go package doc) — store and wire evolve independently,
// and the comms edge maps between them (channelPostPolicyToWire/FromWire). The
// values match the proto: OPEN=0 (any member), OWNER_ONLY=1 (owner only).
type ChannelPostPolicy int32

const (
	// ChannelPostPolicyOpen lets any channel member post — the default.
	ChannelPostPolicyOpen ChannelPostPolicy = 0
	// ChannelPostPolicyOwnerOnly restricts posting to the owner/operator.
	ChannelPostPolicyOwnerOnly ChannelPostPolicy = 1
)

// AgentWorkspace is an agent's observation pane (comms.proto:213-221, narrowed
// by superseded decision 4): it renders the live execution trace and the
// terminal/file panes, never persisted messages. participant_user_ids and the
// share/unshare RPCs are removed (fork f) — access is a projection of
// home-channel membership (RT-2), so the workspace carries no participant list.
type AgentWorkspace struct {
	ID WorkspaceID
	// AgentAccountID is the agent whose session this pane observes.
	AgentAccountID AccountID
}

// ContainerRef identifies the container a message lives in. Channel-only:
// OQ-C (resolved, Matt) dropped workspace_id from the message container, so an
// agent's durable messages live in its channel and the workspace is not a
// message container (design.md:1171-1174, 1809-1819).
type ContainerRef struct {
	ChannelID ChannelID
}

// SearchScope optionally narrows a search to one channel; the zero value
// (empty ChannelID) searches the actor's whole visible set. It mirrors
// SearchMessagesRequest's scope field, itself channel-only after OQ-C
// (design.md:1167-1170).
type SearchScope struct {
	// ChannelID narrows the search to one channel; empty searches all visible.
	ChannelID ChannelID
}

// Topic is a named thread within a channel — the unit of the Zulip-style
// threading model (compass-zulip-threading-model design.md D2). A channel's
// messages are partitioned into topics; every message lives in exactly one.
// Topics are born via a post naming a topic (get-or-create), so there is no
// separate create path. LastSeq is the denormalized highest messages.seq under
// the topic — the activity marker a topic index orders by. Archived is a
// tidiness flag, not a lock: a post naming an archived topic revives it.
type Topic struct {
	ID                 string
	ChannelID          string
	Name               string
	CreatedByAccountID string
	// CreatedAtUnixMS is the server-assigned birth time in unix milliseconds.
	CreatedAtUnixMS int64
	// Archived is the tidiness flag; a get-or-create on an archived name clears it.
	Archived bool
	// LastSeq is the highest messages.seq under this topic, maintained in the
	// append tx so a topic index can order by recency without scanning messages.
	LastSeq int64
}

// TopicRef addresses the topic a message targets: exactly one of ID or Name is
// set. A Name is get-or-created inside the append tx (agents address topics by
// name, the unit they can produce without a lookup); an ID names an existing
// topic, validated to live under the post's channel.
type TopicRef struct {
	ID   string
	Name string
}

// Message is the persisted unit of the comms layer: the durable human↔agent
// conversation (comms.proto:229-242, narrowed by superseded decision 4). The
// trace variants of the block oneof leave the comms surface entirely, so blocks
// carry only text and ask (OQ-A). A message records only its topic; the channel
// is topics.channel_id, one join away (design.md D2/F10).
type Message struct {
	ID MessageID
	// TopicID is the topic this message lives in (topics.id). It replaces the
	// former channel_id + parent_message_id: the channel is resolved through the
	// topic, and threading is by topic membership, not a parent pointer.
	TopicID string
	// AuthorAccountID is the posting account, a user or an agent.
	AuthorAccountID AccountID
	// At is the server-assigned post time.
	At time.Time
	// Blocks is the ordered content: text and ask blocks only.
	Blocks []MessageBlock
}

// MessageBlock is one content block, narrowed to the two durable-conversation
// variants (OQ-A: the trace variants thought/tool_call/plan/diff are removed).
// Exactly one of Text / Ask is non-nil, mirroring the wire `block` oneof.
type MessageBlock struct {
	// Text is settled user-facing / assistant markdown; nil if this is an ask.
	Text *string
	// Ask is a structured question; nil if this is a text block.
	Ask *Ask
}

// Ask is a structured question set the agent posts and, at the turn level,
// chooses whether to block on (see
// docs/designs/product/compass-ask-typed-derivation.md; the frozen
// single-question shape is superseded — one Ask now carries N AskQuestions,
// answered atomically in one RespondToAsk). AskID is minted once on append.
type Ask struct {
	// AskID is the correlation id echoed by RespondToAsk; server-assigned —
	// minted by the store on append when unset. One id per Ask (per native ask
	// tool call), NOT per question — questions are keyed by QuestionID. The
	// comms edge strips any client-supplied ask_id before append (askFromWire),
	// so every externally-posted ask enters id-less and is minted here, which
	// is what makes the id globally unique (comms.proto: "server-assigned and
	// globally unique"). A non-empty AskID is preserved (this is a store
	// primitive; internal callers may set one), so uniqueness rests on the edge
	// strip, not on the store rejecting a supplied id.
	AskID string
	// Questions are the questions the agent asked, in order. At least one; a
	// zero-question ask is rejected on both marshal and unmarshal.
	Questions []AskQuestion
	// Answered is true once a participant has answered this ask; it flips
	// exactly once, on the first AnswerAsk, and makes a second answer an
	// ErrConflict. It is the only reliable answered-signal: a fully-skipped ask
	// leaves every question empty, indistinguishable from pending by inspecting
	// the per-question answer fields alone.
	Answered bool
}

// AskQuestion is one question within an Ask, carrying its own options and
// answer state (empty/unset while pending, kept for audit once answered).
type AskQuestion struct {
	// QuestionID is the agent-supplied key an AskQuestionAnswer addresses;
	// unique AND non-empty within the Ask (enforced on marshal).
	QuestionID string
	// Question is the prompt text.
	Question string
	// Header is an optional short display chip; empty when absent.
	Header  string
	Options []AskOption
	// AllowMultiple reports whether more than one option may be chosen.
	AllowMultiple bool
	// Recommended is the zero-based index into Options of the agent-recommended
	// default; nil when the agent recommended nothing.
	Recommended *int32
	// ChosenOptionIDs are the answered option ids; empty while pending.
	ChosenOptionIDs []string
	// CustomText is the participant's free-text answer ("Other"); empty while
	// pending or when the question was answered by option choice alone.
	CustomText string
	// TimedOut is true when the answer was recorded by timeout auto-selection
	// rather than a participant (SEA-1310 owns whether the Compass answer path
	// times out; this is the audit carrier either way).
	TimedOut bool
}

// AskOption is one selectable answer to an AskQuestion.
type AskOption struct {
	ID    string
	Label string
	// Description is optional explanatory text shown under the label.
	Description string
	// Preview is optional rich preview content for interactive ask dialogs.
	Preview string
}

// Page is a clamped, cursor-paginated read window over a container's messages,
// newest-first. The store clamps Limit to a maximum (defaultPageLimit /
// maxPageLimit) so a caller cannot demand an unbounded page.
type Page struct {
	// Limit is the requested page size; the store clamps it to maxPageLimit and
	// substitutes defaultPageLimit when zero.
	Limit uint32
	// BeforeMessageID pages strictly before this message (exclusive); empty
	// reads the newest page.
	BeforeMessageID MessageID
	// SnapshotSeq is the point-in-time read boundary (comms.proto:353-368,
	// design.md:807-817): the read returns only messages with seq <= SnapshotSeq,
	// so a client paging a since_seq=0 catch-up sees one consistent view across
	// pages under concurrent writes. It is a store-space messages.seq value the
	// server hands the client on the subscribe response and the client passes
	// back on each read RPC. Zero means "latest" (no boundary) — the pre-catch-up
	// default and the value every non-resync caller sends.
	SnapshotSeq uint64
}

// ListMessagesQuery is the input to ListMessages: a channel-scoped, clamped,
// cursor-paginated read window over messages, newest-first, optionally narrowed
// to one topic. The channel is required and gates visibility (membership,
// resolved through the topic join now that a message carries no channel); an
// empty TopicID reads the whole channel across every topic.
type ListMessagesQuery struct {
	// Actor is the reading account; the read is scoped to its visible set.
	Actor AccountID
	// ChannelID is the channel to page; required.
	ChannelID ChannelID
	// TopicID optionally narrows the read to one topic; empty reads the whole
	// channel via the topic join.
	TopicID string
	// Page is the clamped, cursor-paginated window (newest-first).
	Page Page
}
