package fabric

import (
	"encoding/json"
	"fmt"
)

// BindingOp discriminates what happened to a session binding. It exists so a
// receiver can act on the change WITHOUT a Postgres read in the one case where
// the correct action needs no row: an unbind can only ever mean "drop the cache
// entry", and reading Postgres to confirm an absence is a query whose answer is
// already in the message.
//
// A bind still re-reads. The value is deliberately not carried (see
// BindingChange), so "bound" says only that the binding for this session
// changed — the receiver invalidates and lets the next resolution re-read.
//
// THE SET IS OPEN, and a receiver MUST handle a value that is not one of the
// constants below. The wire is shared with publishers that may be newer than
// the process reading it, and the receive path deliberately carries an
// unrecognized op through rather than dropping the message: an unknown op still
// means this session's binding genuinely changed, and this plane has no ack, no
// retry and no dead-letter subject, so a drop would leave the entry stale with
// nothing to reveal it. Treat any unrecognized op as "invalidate and re-read",
// which is the conservative action and stays correct for every op this type can
// grow — a switch with no default is the bug this warning exists to prevent.
type BindingOp string

// The binding operations known to this version. Snake-case-safe single words:
// they are JSON values, not subject tokens, but keeping them token-legal means
// a later grammar that wants the op in the subject does not have to rename
// them. See BindingOp: a receiver may observe a value outside this set.
const (
	// BindingBound means the session now resolves to a (possibly different)
	// instance. The receiver invalidates its entry; the next resolution reads
	// the durable truth.
	BindingBound BindingOp = "bound"
	// BindingUnbound means the session has no binding any more. The receiver
	// can drop its entry outright — there is no row to re-read.
	BindingUnbound BindingOp = "unbound"
)

// BindingChange is a compact reference to a committed session-binding change —
// tenant, session id, and which way it went — and NEVER a copy of the binding
// itself (§Global Constraints: "Postgres is the sole durability source of
// truth"). A receiver treats it as "your cached binding for this session is
// stale", exactly as a subscriber treats an EventRef as "re-read this row".
//
// That reference discipline is what makes this plane safe to run at-most-once
// (§T4). If the message carried the resolved instance, a reordered or duplicated
// delivery could install an older binding over a newer one, and the cache would
// need a version to arbitrate — a second store of state Postgres already owns.
// Carrying only the identity means every delivery is idempotent, and a DROPPED
// delivery degrades to a cache miss that re-reads Postgres, which is the
// arbiter.
type BindingChange struct {
	// Tenant is the owning tenant id, and also the tenant token of the subject
	// the change rides.
	//
	// It is carried in the payload as well as the subject because
	// SubscribeBindingChanges is a TENANT-WILDCARD subscribe whose callback
	// receives the decoded change and no subject (RoutingFabric) — so without
	// this field a receiver watching every tenant could not scope the cache
	// entry it is being told to drop. Publish requires it to equal the tenant
	// it publishes under, so the two can never disagree on the wire.
	Tenant string `json:"tenant"`
	// SessionID names the session whose binding changed. It is the cache key
	// the receiver invalidates.
	SessionID string `json:"session_id"`
	// Op says which way the binding went, so an unbind needs no read.
	Op BindingOp `json:"op"`
}

// encode marshals the change for the wire as JSON, for the same reasons
// EventRef.encode does: the payload is three short strings, so the size
// difference against a packed encoding is noise, while `nats sub
// 'compass.routing.binding.*'` on a live system stays readable and a later
// additive field is forward-compatible with an older subscriber.
func (b BindingChange) encode() ([]byte, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("fabric: encoding binding change %s/%s: %w", b.Tenant, b.SessionID, err)
	}
	return data, nil
}

// valid reports whether the change is publishable. Checked publish-side, like
// EventRef.valid, because this plane has no ack, no retry and no DLQ: a change
// that decodes but names no session is invisible on the wire — the receiver
// would invalidate cache key "" and see nothing wrong — so the only place it can
// be caught with the caller's stack is at its origin.
//
// Tenant must be a valid subject token because it IS a subject token.
func (b BindingChange) valid() error {
	if err := ValidSubjectToken("tenant", b.Tenant); err != nil {
		return err
	}
	if b.SessionID == "" {
		return fmt.Errorf("fabric: binding change for tenant %q has an empty session id", b.Tenant)
	}
	switch b.Op {
	case BindingBound, BindingUnbound:
	default:
		return fmt.Errorf("fabric: binding change %s/%s has op %q, want %q or %q",
			b.Tenant, b.SessionID, b.Op, BindingBound, BindingUnbound)
	}
	return nil
}

// decodeBindingChange parses a wire payload back into a BindingChange,
// rejecting one that decodes but names nothing: a change naming no session
// would invalidate cache key "" and read as "nothing changed".
//
// Deliberately NOT as strict as the publish side on Op. The publish side
// rejects an unknown op because a caller minting one is a bug with a stack;
// a RECEIVER seeing one is talking to a newer publisher, and dropping that
// message would leave a genuinely-changed binding cached as stale — with no
// ack, no retry and no DLQ to reveal it. So an unrecognized op is carried
// through and the receiver treats it as "invalidate and re-read", which is the
// conservative action and stays correct for every op this type can grow.
// decodeEventRef takes the same position on an unknown EventKind.
//
// Tenant is still checked as a subject token, because handleBindingChange
// rebuilds the subject from it to cross-check the delivery.
//
// A rejection here is LOGGED AND DROPPED by the caller, never retried: core
// NATS has no ack, so there is nothing to Nak and nowhere to park. That is the
// visible difference from the JetStream path, where an undecodable payload goes
// to DLQSubject.
func decodeBindingChange(data []byte) (BindingChange, error) {
	var b BindingChange
	if err := json.Unmarshal(data, &b); err != nil {
		return BindingChange{}, fmt.Errorf("fabric: decoding binding change from %d bytes: %w", len(data), err)
	}
	if err := ValidSubjectToken("tenant", b.Tenant); err != nil {
		return BindingChange{}, err
	}
	if b.SessionID == "" {
		return BindingChange{}, fmt.Errorf("fabric: binding change for tenant %q has an empty session id", b.Tenant)
	}
	if b.Op == "" {
		return BindingChange{}, fmt.Errorf("fabric: binding change %s/%s has no op", b.Tenant, b.SessionID)
	}
	return b, nil
}
