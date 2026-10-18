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
	// Tenant scopes the invalidation and subject; receivers subscribe across tenants, so it must travel in the payload.
	// Publish validates that it matches the tenant token in the subject.
	Tenant string `json:"tenant"`
	// SessionID names the session whose binding changed. It is the cache key
	// the receiver invalidates.
	SessionID string `json:"session_id"`
	// Op says which way the binding went, so an unbind needs no read.
	Op BindingOp `json:"op"`
}

// encode uses JSON so operators can inspect NATS traffic; the short payload makes packed encoding unnecessary.
// JSON also keeps additive fields forward-compatible.
func (b BindingChange) encode() ([]byte, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("fabric: encoding binding change %s/%s: %w", b.Tenant, b.SessionID, err)
	}
	return data, nil
}

// valid rejects empty session IDs at publish time; core NATS cannot redeliver a bad invalidation.
// Tenant must be a valid subject token.
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

// decodeBindingChange rejects missing sessions but preserves unknown ops for forward compatibility.
// Receivers invalidate and re-read rather than discard an update they may not understand.
// Tenant remains validated because the handler rebuilds the subject from it.
// Core NATS logs and drops decode failures; unlike JetStream, it has no ack or DLQ.
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
