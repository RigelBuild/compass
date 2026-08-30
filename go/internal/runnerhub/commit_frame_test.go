//go:build unix

package runnerhub

// The durable transcript lane (#24 / OQ-3, RIG-1667 T4): Hub.CommitConversationFrame
// resolves the relayed session to its bound agent account as the fail-closed
// liveness gate (exactly as RelayCommsCall does), then writes the relayed
// transcript_entry to the transcript store keyed at most once on the agent-minted
// idempotency_key. The conversation_posted / conversation_updated write-through was
// removed with the Zulip threading model, so the durable lane now carries ONLY the
// transcript_entry variant — the exact frame the Runner's Gateway forwards
// (runner/gateway/post_conversation_frame.go). Every test here defends one contract
// clause the downstream Runner depends on:
//
//   - a hub with no transcript store wired fails CodeUnavailable, checked BEFORE
//     resolution (a Deliver-only hub);
//   - an unbound/unknown session fails closed CodeNotFound (no live account,
//     never a stale one), and NEVER reaches the store;
//   - a frame with no transcript_entry variant is CodeInvalidArgument (the
//     terminal malformed-frame the Runner does not retry), and never reaches the
//     store;
//   - a transcript_entry under a bound session forwards to AppendTranscriptEntry
//     with the entry's (session_id, seq, checkpoint, json, key) verbatim, and the
//     ack is committed=true (message_id/seq empty — the Runner reads neither);
//   - a store sentinel error maps to the right Connect code (ErrInvalidArgument →
//     InvalidArgument, ErrConflict → AlreadyExists, other → Internal), never
//     swallowed into a committed=false nil-error ack.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle and assert the store write through a fake TranscriptStore, matching
// relay_comms_test.go. Sleep-free: the hub calls the store inline.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// transcriptAppend records one AppendTranscriptEntry call: every argument the
// hub forwarded, so a test asserts the entry reached the store verbatim (the
// entry_json is opaque and must pass through unparsed).
type transcriptAppend struct {
	sessionID      string
	lifetimeSeq    uint64
	checkpoint     bool
	entryJSON      string
	idempotencyKey string
}

// fakeTranscriptStore is a hand-written TranscriptStore: it records every append
// so a test asserts the hub forwarded the relayed entry verbatim, and returns a
// configurable canned error so a test drives the store-error mapping without a
// real database. Concurrency-safe for parity with *store.Store, though the hub
// calls it inline.
type fakeTranscriptStore struct {
	mu        sync.Mutex
	calls     []transcriptAppend
	appendErr error
}

func (f *fakeTranscriptStore) AppendTranscriptEntry(_ context.Context, sessionID string, lifetimeSeq uint64, checkpoint bool, entryJSON, idempotencyKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, transcriptAppend{
		sessionID:      sessionID,
		lifetimeSeq:    lifetimeSeq,
		checkpoint:     checkpoint,
		entryJSON:      entryJSON,
		idempotencyKey: idempotencyKey,
	})
	return f.appendErr
}

func (f *fakeTranscriptStore) snapshot() []transcriptAppend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcriptAppend(nil), f.calls...)
}

// newHubWithTranscripts builds a hub whose TranscriptStore is the returned fake
// (wired post-construction via SetTranscriptStore, the real wiring path), so a
// CommitConversationFrame test drives the resolve->append path and asserts on the
// entry the fake was called with. comms is a stub fake (the durable lane no longer
// touches the CommsCaller).
func newHubWithTranscripts() (*Hub, *fakeTranscriptStore) {
	hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, &fakeCommsCaller{}, discardLogger())
	ts := &fakeTranscriptStore{}
	hub.SetTranscriptStore(ts)
	return hub, ts
}

// transcriptReq builds a CommitConversationFrameRequest carrying a transcript_entry
// variant under sessionID and the agent-minted idempotency key.
func transcriptReq(sessionID, idempotencyKey string, entrySeq uint64, checkpoint bool, entryJSON string) *compassv1internal.CommitConversationFrameRequest {
	return &compassv1internal.CommitConversationFrameRequest{
		SessionId: sessionID,
		Frame: &compassv1internal.AgentFrame{
			Frame: &compassv1internal.AgentFrame_TranscriptEntry{
				TranscriptEntry: &compassv1internal.TranscriptEntry{
					EntryJson:  entryJSON,
					Checkpoint: checkpoint,
					EntrySeq:   entrySeq,
				},
			},
		},
		IdempotencyKey: idempotencyKey,
	}
}

// unsetFrameReq builds a request whose AgentFrame has NO oneof variant set — the
// malformed frame the hub rejects CodeInvalidArgument.
func unsetFrameReq(sessionID, idempotencyKey string) *compassv1internal.CommitConversationFrameRequest {
	return &compassv1internal.CommitConversationFrameRequest{
		SessionId:      sessionID,
		Frame:          &compassv1internal.AgentFrame{},
		IdempotencyKey: idempotencyKey,
	}
}

// 1. A hub with no TranscriptStore wired fails CommitConversationFrame closed
// with CodeUnavailable — the durable transcript leg is not mounted, never a
// silent success. Checked BEFORE session resolution, so even a bound session
// gets Unavailable on a Deliver-only hub.
func TestCommitConversationFrameNilTranscriptStoreIsUnavailable(t *testing.T) {
	// A Deliver-only hub: no transcript store wired.
	hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, &fakeCommsCaller{}, discardLogger())
	bindLiveSession(hub)

	_, err := hub.CommitConversationFrame(context.Background(), transcriptReq("sess-1", "key-1", 1, false, `{"e":1}`))
	if err == nil {
		t.Fatal("CommitConversationFrame on a transcript-store-less hub = nil error, want CodeUnavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("nil-transcript-store error code = %v, want Unavailable", got)
	}
}

// 2. An unbound session fails closed CodeNotFound and NEVER reaches the store —
// the same fail-closed guard RelayCommsCall enforces, for the durable path: a
// session_id selects an account from the hub's own binding, it never carries
// one, so an id the hub never bound resolves to nothing and no append is
// attempted.
//
// Mutation: hardcode accountForSession to return a fixed account (ok=true) and
// this fails twice over — the error goes nil and the store records an append.
func TestCommitConversationFrameUnboundSessionFailsClosedNotFound(t *testing.T) {
	hub, ts := newHubWithTranscripts()

	_, err := hub.CommitConversationFrame(context.Background(), transcriptReq("never-bound", "key-1", 1, false, `{"e":1}`))
	if err == nil {
		t.Fatal("CommitConversationFrame for an unbound session = nil error, want CodeNotFound (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unbound-session error code = %v, want NotFound", got)
	}
	if calls := ts.snapshot(); len(calls) != 0 {
		t.Fatalf("store was written %d times for an unbound session, want 0 (no append attempt)", len(calls))
	}
}

// 3. A frame with no transcript_entry variant is a malformed frame —
// CodeInvalidArgument, the TERMINAL class the Runner does not retry (a permanent
// per-frame refusal). The store is never reached, and the error is a real Connect
// status error (never committed=false + nil, which the Runner would misread as a
// successful commit).
func TestCommitConversationFrameNoTranscriptVariantIsInvalidArgument(t *testing.T) {
	hub, ts := newHubWithTranscripts()
	bindLiveSession(hub)

	_, err := hub.CommitConversationFrame(context.Background(), unsetFrameReq("sess-1", "key-1"))
	if err == nil {
		t.Fatal("CommitConversationFrame with an unset frame variant = nil error, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("unset-variant error code = %v, want InvalidArgument", got)
	}
	if calls := ts.snapshot(); len(calls) != 0 {
		t.Fatalf("store was written %d times for a malformed frame, want 0", len(calls))
	}
}

// 4. The happy path forwards the relayed transcript_entry to the store VERBATIM
// under the bound session, and returns committed=true. This pins the things the
// durable contract depends on: the entry is keyed by the resolved session id, the
// agent-stamped (seq, checkpoint) and the opaque entry_json pass through unchanged,
// the agent-minted key reaches the at-most-once append, and the ack reports
// committed=true (message_id/seq empty — the Runner reads neither).
func TestCommitConversationFrameHappyTranscriptForwardsVerbatim(t *testing.T) {
	hub, ts := newHubWithTranscripts()
	bindLiveSession(hub)

	const entryJSON = `{"kind":"assistant","turn":7}`
	resp, err := hub.CommitConversationFrame(context.Background(), transcriptReq("sess-1", "idem-key-1", 42, true, entryJSON))
	if err != nil {
		t.Fatalf("CommitConversationFrame(transcript) = %v, want success", err)
	}

	calls := ts.snapshot()
	if len(calls) != 1 {
		t.Fatalf("store written %d times, want exactly 1", len(calls))
	}
	got := calls[0]
	if got.sessionID != "sess-1" {
		t.Fatalf("append session id = %q, want the resolved sess-1", got.sessionID)
	}
	if got.lifetimeSeq != 42 {
		t.Fatalf("append lifetime seq = %d, want the agent-stamped 42", got.lifetimeSeq)
	}
	if !got.checkpoint {
		t.Fatal("append checkpoint = false, want the agent-stamped true")
	}
	if got.entryJSON != entryJSON {
		t.Fatalf("append entry_json = %q, want the opaque relayed %q (verbatim, never parsed)", got.entryJSON, entryJSON)
	}
	if got.idempotencyKey != "idem-key-1" {
		t.Fatalf("append idempotency key = %q, want the relayed idem-key-1 (the key must thread to the at-most-once append)", got.idempotencyKey)
	}
	if !resp.GetCommitted() {
		t.Fatal("ack committed = false, want true on a fresh commit")
	}
	if id := resp.GetMessageId(); id != "" {
		t.Fatalf("ack message_id = %q, want empty (no message id for a transcript entry)", id)
	}
	if seq := resp.GetSeq(); seq != 0 {
		t.Fatalf("ack seq = %d, want 0 (deferred)", seq)
	}
}

// 5. A store sentinel error maps to the right Connect code and is NEVER swallowed
// into a committed=false nil-error ack. ErrInvalidArgument (a malformed or
// unknown-session entry) → InvalidArgument and ErrConflict (a genuine entry_seq
// collision) → AlreadyExists are TERMINAL; any other store fault → Internal is
// transient (the Runner retries under the same key). A bare store error must never
// surface as CodeUnknown, which the Runner would misread as a retryable teardown.
func TestCommitConversationFrameMapsStoreErrorCodes(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		want     connect.Code
	}{
		{"invalid argument is terminal invalid_argument", store.ErrInvalidArgument, connect.CodeInvalidArgument},
		{"conflict is terminal already_exists", store.ErrConflict, connect.CodeAlreadyExists},
		{"other store fault is transient internal", errors.New("transient store fault"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, ts := newHubWithTranscripts()
			ts.appendErr = tc.storeErr
			bindLiveSession(hub)

			resp, err := hub.CommitConversationFrame(context.Background(), transcriptReq("sess-1", "key-1", 1, false, `{"e":1}`))
			if err == nil {
				t.Fatal("CommitConversationFrame with a store error = nil error, want a Connect status error (never a committed=false nil-error ack)")
			}
			if resp != nil {
				t.Fatalf("response = %v on a store error, want nil (a non-commit is a Connect error, never an ack)", resp)
			}
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("mapped error code = %v, want %v", got, tc.want)
			}
		})
	}
}
