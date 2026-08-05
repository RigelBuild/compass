package store

// The write-method input structs. Each mirrors the corresponding compass.v1
// request envelope's client-supplied fields (comms.proto:365-475) — the
// server-set fields (ids, timestamps, owner) are assigned by the store or
// passed as an explicit actor argument, never taken from client input, so a
// caller cannot forge ownership or identity (comms.proto:31-37).

// UserAccount input for CreateUser. The new account is always a regular member;
// role elevation is a separate admin path (comms.proto:39-42), not a field a
// signup can set, so no role appears here.
type NewUser struct {
	Handle      string
	DisplayName string
}

// AgentAccount input for CreateAgent. The owner is the authenticated caller
// (passed as the ownerUserID argument, not a field here), and home_channel_id
// is minted by the store at creation (RT-2), so neither appears in the input.
type NewAgent struct {
	Handle      string
	DisplayName string
	// Persona is the agent's system-prompt text, baked at creation (SEA-1571).
	// Empty means no persona override; the caller supplies it and the server
	// stores it verbatim, never synthesizing one.
	Persona string
	// ParentAgentID is the agent's parent in the agent tree; empty = root. The
	// server validates it (same resolved owner, must exist) before insert; an
	// empty value inserts SQL NULL, satisfying the FK.
	ParentAgentID AccountID
}

// ChannelGroup input for CreateChannelGroup. OwnerUserID is the authenticated
// caller (a separate argument); the visibility ceiling (child ≤ parent) is
// enforced by the store against the parent, not trusted from input.
type NewChannelGroup struct {
	Name          string
	ParentGroupID ChannelGroupID
	Visibility    ChannelGroupVisibility
}

// Channel input for CreateChannel. The store enforces transitive
// owner-membership (an agent's DMs and any channel it starts always include its
// owning user(s), design.md:231-234), so the caller-supplied member set is
// augmented, never trusted as complete.
type NewChannel struct {
	Name    string
	GroupID ChannelGroupID
	Kind    ChannelKind
	// MemberAccountIDs are the accounts to seed the channel with; the store adds
	// the required owner rows.
	MemberAccountIDs []AccountID
	// Policy is the channel's initial policy (T4). The zero value is the
	// pre-substrate default: OPEN posting, no owner, per-member opt-in
	// subscription. Thereafter mutated only through SetChannelPolicy.
	Policy ChannelPolicy
}

// MemberUpdate is one add/remove/subscribe mutation for UpdateChannelMembers
// (RT-1): the single membership-mutation carrier behind the RT-1 RPC. Add and
// Remove are mutually exclusive per call; Subscribed flips the per-member
// subscribed flag on an existing or added row.
type MemberUpdate struct {
	AccountID AccountID
	// Remove removes the member instead of adding/updating it.
	Remove bool
	// Subscribed sets the per-member subscribed flag (ignored when Remove).
	Subscribed bool
	// Unsubscribe marks this update as an explicit unsubscribe (the
	// unsubscribe_account_ids arm), distinct from a plain add that leaves
	// Subscribed false. The store rejects an explicit unsubscribe on a
	// mandatory_subscription channel (T4) — a plain add is unaffected.
	Unsubscribe bool
}
