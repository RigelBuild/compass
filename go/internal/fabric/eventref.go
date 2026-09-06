package fabric

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// EventKind names a class of comms event. It is the last token of a comms
// subject, so its value must be a valid single subject token (see
// ValidSubjectToken) — every constant below is snake_case for that reason.
type EventKind string

// The comms event kinds. These mirror the write-through publish sites in
// go/internal/comms: each one says "a row of this class changed", never what
// changed — the subscriber re-reads the row (see EventRef).
const (
	KindAccountChanged        EventKind = "account_changed"
	KindChannelGroupChanged   EventKind = "channel_group_changed"
	KindChannelChanged        EventKind = "channel_changed"
	KindAgentWorkspaceChanged EventKind = "agent_workspace_changed"
	KindMessagePosted         EventKind = "message_posted"
	KindMessageUpdated        EventKind = "message_updated"
	KindTopicUpserted         EventKind = "topic_upserted"
)

// EventRef is a compact reference to a committed Postgres row — event kind, row
// id, tenant — and NEVER a copy of the row's payload (§Global Constraints:
// "Postgres is the sole durability source of truth"). A subscriber receiving one
// re-reads the row it names.
//
// That is what keeps JetStream a transport rather than a second store: a
// replayed or double-delivered EventRef re-reads the same row and is idempotent,
// and a dropped one degrades to a delivery-cursor sweep. It also bounds the wire
// payload to a few dozen bytes regardless of how large the underlying row is.
type EventRef struct {
	// Tenant is the owning tenant id (store.TenantID's underlying type is
	// string). It is also the tenant token of the subject the ref rides, so the
	// subscriber can scope its re-read without trusting the subject.
	Tenant string `json:"tenant"`
	// Kind is the class of event.
	Kind EventKind `json:"kind"`
	// RowID is the primary-row id the subscriber re-reads from Postgres.
	RowID string `json:"row_id"`
}

// encode marshals the ref for the wire as JSON. JSON rather than proto or a
// packed encoding on purpose: the payload is three short strings, so the size
// difference is noise, while a `nats sub` on a live subject stays readable and a
// later additive field is forward-compatible with an older subscriber.
func (r EventRef) encode() ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("fabric: encoding event ref %s/%s: %w", r.Kind, r.RowID, err)
	}
	return b, nil
}

// msgID derives the JetStream deduplication id for the ref. Deterministic in
// (tenant, kind, row id) so two Servers publishing the same logical change — or
// one Server retrying a publish whose ack was lost — collapse to one stored
// message inside the stream's duplicate window.
//
// Hashed rather than concatenated because the three fields are opaque: a raw
// "tenant|kind|rowid" join could collide across differently-split ids, and a
// long row id would push the Nats-Msg-Id header wide for no benefit.
func (r EventRef) msgID() string {
	h := sha256.New()
	// Length-prefixed so no field boundary is ambiguous. hash.Hash.Write is
	// documented never to return an error, so there is none to handle.
	for _, f := range [...]string{r.Tenant, string(r.Kind), r.RowID} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// valid reports whether the ref is publishable: every field is required, and
// tenant and kind must be valid subject tokens because they are subject tokens.
// Checked publish-side so a malformed ref fails at its origin, with the caller's
// stack, instead of becoming an undecodable DLQ entry later.
func (r EventRef) valid() error {
	if err := ValidSubjectToken("tenant", r.Tenant); err != nil {
		return err
	}
	if err := ValidSubjectToken("event kind", string(r.Kind)); err != nil {
		return err
	}
	if r.RowID == "" {
		return fmt.Errorf("fabric: event ref %s/%s has an empty row id", r.Tenant, r.Kind)
	}
	return nil
}

// decodeEventRef parses a wire payload back into an EventRef, rejecting one
// missing any required field. A ref that decodes but names nothing is worse than
// a decode error: the subscriber would re-read row "" and see the miss as
// "nothing changed".
func decodeEventRef(b []byte) (EventRef, error) {
	var r EventRef
	if err := json.Unmarshal(b, &r); err != nil {
		return EventRef{}, fmt.Errorf("fabric: decoding event ref from %d bytes: %w", len(b), err)
	}
	if r.Tenant == "" || r.Kind == "" || r.RowID == "" {
		return EventRef{}, fmt.Errorf("fabric: decoded event ref is incomplete (tenant=%q kind=%q row_id=%q)", r.Tenant, r.Kind, r.RowID)
	}
	return r, nil
}
