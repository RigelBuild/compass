package fabric

import (
	"strings"
	"testing"
)

// TestEventRefCodecRoundTrip defends the wire contract: what a publisher encodes
// is exactly what a subscriber decodes. Every field matters — a lost tenant
// re-reads the wrong tenant's row, a lost kind mis-routes, a lost row id reads
// nothing.
func TestEventRefCodecRoundTrip(t *testing.T) {
	t.Parallel()
	for _, want := range []EventRef{
		{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-1"},
		{Tenant: "0d8f1a2b-3c4d", Kind: KindTopicUpserted, RowID: "topic/with-slash"},
		{Tenant: "unicode-ténant", Kind: KindChannelChanged, RowID: "ch-✓"},
	} {
		t.Run(want.RowID, func(t *testing.T) {
			t.Parallel()
			b, err := want.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := decodeEventRef(b)
			if err != nil {
				t.Fatalf("decodeEventRef: %v", err)
			}
			if got != want {
				t.Fatalf("round trip = %+v, want %+v", got, want)
			}
		})
	}
}

// TestEventRefCarriesNoPayload defends the load-bearing global constraint:
// EventRef is a reference, never a copy. A field added later that carried row
// content would make JetStream a second store of committed state. Asserted
// structurally — the encoding has exactly the three reference fields.
func TestEventRefCarriesNoPayload(t *testing.T) {
	t.Parallel()
	b, err := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := string(b)
	if want := `{"tenant":"t1","kind":"message_posted","row_id":"m1"}`; got != want {
		t.Fatalf("encoded = %s, want %s (an extra field means a payload crept onto the wire)", got, want)
	}
}

// TestDecodeEventRefRejectsIncomplete defends the fail-closed decode. A ref that
// parses but names nothing is worse than a parse error: the subscriber would
// re-read row "" and read the miss as "nothing changed" — a silently dropped
// event.
func TestDecodeEventRefRejectsIncomplete(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty bytes", ``},
		{"not json", `not json at all`},
		{"empty object", `{}`},
		{"missing tenant", `{"kind":"message_posted","row_id":"m1"}`},
		{"missing kind", `{"tenant":"t1","row_id":"m1"}`},
		{"missing row id", `{"tenant":"t1","kind":"message_posted"}`},
		{"blank row id", `{"tenant":"t1","kind":"message_posted","row_id":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := decodeEventRef([]byte(tc.body)); err == nil {
				t.Fatalf("decodeEventRef(%q) = %+v, want an error", tc.body, got)
			}
		})
	}
}

// TestDecodeEventRefIgnoresUnknownFields defends forward compatibility, which is
// why the codec is JSON: an older subscriber must keep working against a
// publisher that added a field, rather than parking every event on the DLQ
// during a rolling deploy.
func TestDecodeEventRefIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	body := `{"tenant":"t1","kind":"message_posted","row_id":"m1","future_field":42}`
	got, err := decodeEventRef([]byte(body))
	if err != nil {
		t.Fatalf("decodeEventRef with an unknown field: %v", err)
	}
	want := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}
	if got != want {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

// TestMsgIDIsDeterministic defends the dedup key's whole purpose: two publishers
// of the same logical change must derive the SAME id (or JetStream stores two
// copies and the subscriber handles it twice), and two different changes must
// derive DIFFERENT ids (or the second is silently deduped away — a lost event).
func TestMsgIDIsDeterministic(t *testing.T) {
	t.Parallel()
	base := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}
	if a, b := base.msgID(), base.msgID(); a != b {
		t.Fatalf("msgID is not stable: %q vs %q", a, b)
	}
	same := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}
	if base.msgID() != same.msgID() {
		t.Fatal("two refs with identical fields must derive one msg id, or dedup never fires")
	}

	distinct := map[string]EventRef{
		"base":         base,
		"other tenant": {Tenant: "t2", Kind: KindMessagePosted, RowID: "m1"},
		"other kind":   {Tenant: "t1", Kind: KindMessageUpdated, RowID: "m1"},
		"other row":    {Tenant: "t1", Kind: KindMessagePosted, RowID: "m2"},
		// The field-boundary case a naive concatenation gets wrong: "t1|a" and
		// "t|1a" would join to the same string.
		"boundary a": {Tenant: "t1", Kind: KindMessagePosted, RowID: "am1"},
		"boundary b": {Tenant: "t", Kind: KindMessagePosted, RowID: "1am1"},
	}
	seen := make(map[string]string, len(distinct))
	for name, ref := range distinct {
		id := ref.msgID()
		if prev, dup := seen[id]; dup {
			t.Fatalf("msg id collision: %q and %q derive %q; the second event would be deduped away", prev, name, id)
		}
		seen[id] = name
	}
}

// TestEventRefValid defends publish-side validation: a malformed ref must fail
// at its origin with the caller's stack, not become an undecodable DLQ entry
// discovered by an operator later.
func TestEventRefValid(t *testing.T) {
	t.Parallel()
	if err := (EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}).valid(); err != nil {
		t.Fatalf("a complete ref must be valid: %v", err)
	}
	for _, tc := range []struct {
		name string
		ref  EventRef
	}{
		{"no tenant", EventRef{Kind: KindMessagePosted, RowID: "m1"}},
		{"no kind", EventRef{Tenant: "t1", RowID: "m1"}},
		{"no row id", EventRef{Tenant: "t1", Kind: KindMessagePosted}},
		{"tenant with a dot", EventRef{Tenant: "t.1", Kind: KindMessagePosted, RowID: "m1"}},
		{"kind with a wildcard", EventRef{Tenant: "t1", Kind: "message*", RowID: "m1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.ref.valid()
			if err == nil {
				t.Fatalf("%+v: want an error", tc.ref)
			}
			if !strings.Contains(err.Error(), "fabric:") {
				t.Errorf("error %q should be attributed to the fabric package", err)
			}
		})
	}
}
