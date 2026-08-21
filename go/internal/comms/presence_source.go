package comms

import (
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// PresenceSource is the in-memory presence read GetRoster joins the durable tree
// + activity sources against: the live presence ENUM per agent (DL-074 — the
// activity string is durable in the store, the presence enum is ephemeral and
// in-memory). runnerhub.Hub implements it; comms depends only on this narrow,
// public-typed surface — the runnerhub PresenceSnapshot / presence-map internals
// stay in the rail layer, off the comms handler, exactly as AskAnswerWaker keeps
// the control-dispatch envelope out of comms.
//
// An agent absent from the returned map reports OFFLINE at the roster (the hub's
// PresenceFor already defaults absent agents to OFFLINE, but the handler
// re-applies the default so a nil source — a hub-less unit test — also reads
// OFFLINE for every agent).
type PresenceSource interface {
	// PresenceFor returns the live presence enum for each requested agent that
	// has one; an agent absent from the map is OFFLINE at the caller.
	PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence
}
