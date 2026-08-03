//go:build unix

package runnerhub

// T5 (SEA-1667): Hub.ReconstructSessionBody is the resume-body reconstructor —
// a PURE read-and-concatenate over the two-tier transcript store. In NORMAL
// operation it reads the PG hot-tail ONLY (SessionTranscript): the latest
// checkpoint's EntryJSON is a full SDK file body (header-first), emitted
// verbatim, then every later delta's EntryJSON is appended one line each,
// newline-joined — the store never parses entry JSON and neither does the
// reconstructor. The S3 fallback fires ONLY when the safety valve fired
// (SafetyValveSegments non-empty): the checkpoint body first, then every later
// delta — fetched segment(s) AND the PG tail alike — merged by entry_seq behind
// it, so the file stays header-first and complete. An unknown/empty session is
// ErrNotFound.
//
// White-box (package runnerhub), mirroring commit_frame_test.go's fake-store
// style: a hand-written TranscriptReader records/serves canned rows so a test
// drives the concatenation and the object-store-untouched invariant without a
// real database. The object-store leg is a fake that FAILS THE TEST if
// GetSegment is called on a normal (non-valve) resume — the record's PG-only
// invariant made observable.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// fakeTranscriptReader is a hand-written TranscriptReader: it serves canned
// SessionTranscript rows + safety-valve manifest rows + segment bodies, and
// records whether ReadArchiveSegment was ever called so a test can assert the
// object store stays untouched on a normal resume.
type fakeTranscriptReader struct {
	transcript     []store.TranscriptEntryRow
	transcriptErr  error
	segments       []store.ArchiveSegmentRow
	segmentsErr    error
	bodies         map[string][]byte // object_key -> segment body
	segmentReadErr error

	failOnRead *testing.T // when set, ReadArchiveSegment is a test failure
	readCalled bool
}

func (f *fakeTranscriptReader) SessionResumeSnapshot(_ context.Context, _ string) ([]store.TranscriptEntryRow, []store.ArchiveSegmentRow, error) {
	if f.transcriptErr != nil {
		return nil, nil, f.transcriptErr
	}
	if f.segmentsErr != nil {
		return nil, nil, f.segmentsErr
	}
	return f.transcript, f.segments, nil
}

func (f *fakeTranscriptReader) ReadArchiveSegment(_ context.Context, objectKey string) ([]byte, error) {
	f.readCalled = true
	if f.failOnRead != nil {
		f.failOnRead.Errorf("ReadArchiveSegment(%q) called on a normal (PG-only) resume", objectKey)
	}
	if f.segmentReadErr != nil {
		return nil, f.segmentReadErr
	}
	body, ok := f.bodies[objectKey]
	if !ok {
		return nil, errors.New("fake reader: no segment body for key")
	}
	return body, nil
}

// newHubWithReader builds a hub whose TranscriptReader is the returned fake
// (wired post-construction via SetTranscriptReader, the real wiring path).
func newHubWithReader(r *fakeTranscriptReader) *Hub {
	hub := NewHub(&fakeConversationSink{}, &fakeLifecycleSink{}, &fakeTailSink{}, &fakeCommsCaller{}, discardLogger())
	hub.SetTranscriptReader(r)
	return hub
}

// entry is a terse TranscriptEntryRow constructor for the tables below.
func entry(seq uint64, checkpoint bool, json string) store.TranscriptEntryRow {
	return store.TranscriptEntryRow{EntrySeq: seq, Checkpoint: checkpoint, EntryJSON: json}
}

// 1. N stored entries with no checkpoint reconstruct to the whole retained
// hot-tail, newline-joined in entry order (SessionTranscript returns the whole
// tail when there is no checkpoint).
func TestReconstructSessionBodyNoCheckpointJoinsWholeTail(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(1, false, `{"header":true}`),
			entry(2, false, `{"delta":"a"}`),
			entry(3, false, `{"delta":"b"}`),
		},
		failOnRead: t,
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	want := "{\"header\":true}\n{\"delta\":\"a\"}\n{\"delta\":\"b\"}"
	if string(got) != want {
		t.Fatalf("reconstructed body = %q, want %q", got, want)
	}
}

// 2. A checkpointed store reconstructs the checkpoint body FIRST then its tail
// only, once each — no double-count. SessionTranscript already returns
// [checkpoint .. now], so the reconstructor emits row 0 (the checkpoint) then
// the later deltas; the checkpoint body is a full header-first SDK file body.
func TestReconstructSessionBodyCheckpointThenTailNoDoubleCount(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(10, true, `{"header":true,"checkpoint":10}`),
			entry(11, false, `{"delta":"x"}`),
			entry(12, false, `{"delta":"y"}`),
		},
		failOnRead: t,
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	want := "{\"header\":true,\"checkpoint\":10}\n{\"delta\":\"x\"}\n{\"delta\":\"y\"}"
	if string(got) != want {
		t.Fatalf("reconstructed body = %q, want %q", got, want)
	}
}

// 3. The reconstructed bytes are header-first and newline-joined in seq order:
// the FIRST line is the verbatim checkpoint body and every subsequent line is a
// later delta's entry_json in ascending entry_seq. No SDK-file fixture exists in
// this tree (a real tee run's capture) — this asserts the header-first +
// newline-join structural contract the loader relies on, and NOTES the fixture
// gap to the driver (see reconstruct.go doc + the report).
func TestReconstructSessionBodyHeaderFirstNewlineJoined(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(5, true, `{"type":"session","version":1}`),
			entry(6, false, `{"type":"message","seq":6}`),
		},
		failOnRead: t,
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	lines := splitLines(string(got))
	if len(lines) != 2 {
		t.Fatalf("reconstructed body has %d lines, want 2: %q", len(lines), got)
	}
	if lines[0] != `{"type":"session","version":1}` {
		t.Fatalf("first line = %q, want the verbatim checkpoint body (header-first)", lines[0])
	}
	if lines[1] != `{"type":"message","seq":6}` {
		t.Fatalf("second line = %q, want the later delta in seq order", lines[1])
	}
}

// 4. A NORMAL resume reads the PG hot-tail ONLY — the object-store seam is
// UN-CALLED. failOnRead makes any ReadArchiveSegment a test failure; readCalled
// pins the assertion positively too, so a future refactor that reaches for S3 on
// the normal path reddens here.
func TestReconstructSessionBodyNormalResumeDoesNotTouchObjectStore(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(1, true, `{"header":true}`),
			entry(2, false, `{"delta":"a"}`),
		},
		// No safety-valve segments -> the S3 leg must never fire.
		failOnRead: t,
	}
	hub := newHubWithReader(r)

	if _, err := hub.ReconstructSessionBody(context.Background(), "sess-1"); err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	if r.readCalled {
		t.Fatal("normal resume called ReadArchiveSegment, want the object store UN-touched")
	}
}

// 5. A session with a safety_valve manifest segment emits the checkpoint body
// FIRST, then merges the fetched segment body + the PG tail by entry_seq behind
// it — header-first, no double-count. The valve evicted [11,12] to S3; the PG
// tail retains [10 (checkpoint), 13]. Reconstruct = checkpoint(10), then merged
// [11,12,13] in seq order.
func TestReconstructSessionBodySafetyValveMergesSegmentAndTail(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(10, true, `{"header":true,"checkpoint":10}`),
			entry(13, false, `{"delta":"d13"}`),
		},
		segments: []store.ArchiveSegmentRow{
			{ObjectKey: "sessions/sess-1/11-12.jsonl", MinEntrySeq: 11, MaxEntrySeq: 12, Kind: store.SegmentKindSafetyValve},
		},
		bodies: map[string][]byte{
			"sessions/sess-1/11-12.jsonl": []byte("{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}"),
		},
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	want := "{\"header\":true,\"checkpoint\":10}\n" +
		"{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}\n{\"delta\":\"d13\"}"
	if string(got) != want {
		t.Fatalf("reconstructed body = %q, want %q", got, want)
	}
	if !r.readCalled {
		t.Fatal("safety-valve resume did not call ReadArchiveSegment, want the S3 segment fetched")
	}
}

// 6. An unknown/empty session is ErrNotFound: the store's SessionTranscript
// returns store.ErrNotFound, which the reconstructor propagates as a
// CodeNotFound Connect error (the resume-body contract's not-found).
func TestReconstructSessionBodyUnknownSessionIsNotFound(t *testing.T) {
	r := &fakeTranscriptReader{
		transcriptErr: store.ErrNotFound,
		failOnRead:    t,
	}
	hub := newHubWithReader(r)

	_, err := hub.ReconstructSessionBody(context.Background(), "nope")
	if err == nil {
		t.Fatal("ReconstructSessionBody on an unknown session = nil error, want NotFound")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unknown-session error code = %v, want NotFound", got)
	}
}

// 7. A hub with NO TranscriptReader wired fails ReconstructSessionBody closed
// with CodeUnavailable — the resume read leg is not mounted, the same fail-
// closed posture as the write seam (SetTranscriptStore). Never a silent empty
// body an agent would resume from as if the session were empty.
func TestReconstructSessionBodyNilReaderIsUnavailable(t *testing.T) {
	hub := newHubOnly()

	_, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("ReconstructSessionBody on a reader-less hub = nil error, want CodeUnavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("nil-reader error code = %v, want Unavailable", got)
	}
}

// 8. TWO safety_valve segments interleave with the PG tail: checkpoint at 10,
// segments [11-12] and [14-15] evicted to distinct object keys, PG tail retains
// 10 (checkpoint), 13, 16. Reconstruct = checkpoint(10) then merged
// 11,12,13,14,15,16 in exact seq order. Exercises seg.MinEntrySeq + uint64(i)
// stamping across multiple segments interleaving with the tail.
func TestReconstructSessionBodyMergesMultipleSegmentsAndTail(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(10, true, `{"header":true,"checkpoint":10}`),
			entry(13, false, `{"delta":"d13"}`),
			entry(16, false, `{"delta":"d16"}`),
		},
		segments: []store.ArchiveSegmentRow{
			{ObjectKey: "sessions/sess-1/11-12.jsonl", MinEntrySeq: 11, MaxEntrySeq: 12, Kind: store.SegmentKindSafetyValve},
			{ObjectKey: "sessions/sess-1/14-15.jsonl", MinEntrySeq: 14, MaxEntrySeq: 15, Kind: store.SegmentKindSafetyValve},
		},
		bodies: map[string][]byte{
			"sessions/sess-1/11-12.jsonl": []byte("{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}"),
			"sessions/sess-1/14-15.jsonl": []byte("{\"delta\":\"d14\"}\n{\"delta\":\"d15\"}"),
		},
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	want := "{\"header\":true,\"checkpoint\":10}\n" +
		"{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}\n{\"delta\":\"d13\"}\n" +
		"{\"delta\":\"d14\"}\n{\"delta\":\"d15\"}\n{\"delta\":\"d16\"}"
	if string(got) != want {
		t.Fatalf("reconstructed body = %q, want %q", got, want)
	}
	if !r.readCalled {
		t.Fatal("multi-segment resume did not call ReadArchiveSegment, want the S3 segments fetched")
	}
}

// 9. A safety_valve segment body written WITH a trailing newline must NOT yield
// a spurious empty final line (which would be stamped a seq and emitted as a
// blank JSONL row). splitSegmentLines strips a single trailing '\n'.
func TestReconstructSessionBodySegmentTrailingNewlineNoBlankLine(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(10, true, `{"header":true,"checkpoint":10}`),
			entry(13, false, `{"delta":"d13"}`),
		},
		segments: []store.ArchiveSegmentRow{
			{ObjectKey: "sessions/sess-1/11-12.jsonl", MinEntrySeq: 11, MaxEntrySeq: 12, Kind: store.SegmentKindSafetyValve},
		},
		bodies: map[string][]byte{
			// Trailing newline after the last entry.
			"sessions/sess-1/11-12.jsonl": []byte("{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}\n"),
		},
	}
	hub := newHubWithReader(r)

	got, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ReconstructSessionBody = %v, want success", err)
	}
	want := "{\"header\":true,\"checkpoint\":10}\n" +
		"{\"delta\":\"d11\"}\n{\"delta\":\"d12\"}\n{\"delta\":\"d13\"}"
	if string(got) != want {
		t.Fatalf("reconstructed body = %q, want %q", got, want)
	}
	for _, line := range splitLines(string(got)) {
		if line == "" {
			t.Fatalf("reconstructed body contains a blank line: %q", got)
		}
	}
}

// 10. A generic (non-ErrNotFound) snapshot read error maps to CodeInternal —
// pins reconstructReadError's default branch.
func TestReconstructSessionBodyGenericReadErrorIsInternal(t *testing.T) {
	r := &fakeTranscriptReader{
		transcriptErr: errors.New("boom: pg read failed"),
		failOnRead:    t,
	}
	hub := newHubWithReader(r)

	_, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("ReconstructSessionBody with a generic read error = nil, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("generic-read-error code = %v, want Internal", got)
	}
}

// 11. A generic ReadArchiveSegment error (segments present, S3 fetch fails) maps
// to CodeInternal — pins reconstructReadError's default branch on the S3 leg.
func TestReconstructSessionBodySegmentReadErrorIsInternal(t *testing.T) {
	r := &fakeTranscriptReader{
		transcript: []store.TranscriptEntryRow{
			entry(10, true, `{"header":true,"checkpoint":10}`),
		},
		segments: []store.ArchiveSegmentRow{
			{ObjectKey: "sessions/sess-1/11-12.jsonl", MinEntrySeq: 11, MaxEntrySeq: 12, Kind: store.SegmentKindSafetyValve},
		},
		segmentReadErr: errors.New("boom: object store read failed"),
	}
	hub := newHubWithReader(r)

	_, err := hub.ReconstructSessionBody(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("ReconstructSessionBody with a segment read error = nil, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("segment-read-error code = %v, want Internal", got)
	}
}

// splitLines splits on '\n' (strings.Split semantics keep a trailing empty), so
// a spurious trailing newline is caught by the line-count assertions above
// rather than silently swallowed.
func splitLines(s string) []string {
	return strings.Split(s, "\n")
}
