//go:build unix

package runnerhub

// T5 (SEA-1667): the resume-body reconstructor. ReconstructSessionBody rebuilds
// the loadable SDK session file a resuming agent starts from, by pure
// read-and-concatenate over the two-tier transcript store — no control-lane ops,
// no entry-JSON parsing. See the method doc for the normal (PG-only) and the
// safety-valve (S3 fallback) shapes.

import (
	"context"
	"errors"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// errReaderUnavailable is the fail-closed cause when a hub with no
// TranscriptReader wired serves ReconstructSessionBody (a Deliver-only hub, or a
// server assembly that never wired the read seam). It maps to CodeUnavailable —
// the resume read leg is not mounted, never a silent empty body an agent would
// resume from as if the session were fresh.
var errReaderUnavailable = errors.New("runnerhub: no transcript reader wired to serve ReconstructSessionBody")

// ReconstructSessionBody rebuilds the resume body for sessionID — the loadable
// SDK session file the resuming agent's first lifetime starts from (T5, record
// §1142-1205). It is a PURE read-and-concatenate: the store never parses entry
// JSON and neither does this, so the result is byte concatenation joined by
// newlines.
//
// NORMAL operation reads the PG hot-tail ONLY (the object store is NOT touched):
// SessionTranscript yields the latest checkpoint (if any) followed by every
// later delta in entry order. The checkpoint's EntryJSON is a full SDK file body
// (header-first, the loader-validated header); it is emitted VERBATIM, then each
// later delta's EntryJSON is appended one line each. The result is a valid
// loadable session file BY CONSTRUCTION — this never validates or parses it.
//
// S3 FALLBACK — ONLY when the safety valve fired: if SafetyValveSegments is
// non-empty, the checkpoint full body is emitted FIRST, then every later delta —
// from the fetched S3 segment(s) AND the PG tail alike — is merged by entry_seq
// BEHIND that checkpoint body, so the file stays header-first and complete. This
// is the only resume path that touches the object store; it never fires in
// normal operation.
//
// An unknown or empty session is CodeNotFound (propagating the store's
// ErrNotFound). A hub with no reader wired is CodeUnavailable.
//
// Fixture gap (NOTE to the driver): no captured real-tee SDK session-file
// fixture exists in this tree, so the loadability guarantee is asserted
// structurally (header-first + newline-joined in seq order), not against a
// golden file. The store's checkpoint-is-a-full-body invariant is what makes the
// concatenation loadable; a fixture would pin it end-to-end.
func (h *Hub) ReconstructSessionBody(ctx context.Context, sessionID string) ([]byte, error) {
	h.mu.Lock()
	reader := h.reader
	h.mu.Unlock()
	if reader == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errReaderUnavailable)
	}

	// One atomic snapshot: the PG hot-tail and the safety_valve manifest are
	// read together, so a concurrent flush can't commit between them and corrupt
	// the body. The NotFound-on-empty-tail early return (inside the snapshot)
	// relies on the store invariant that the safety valve always retains the
	// newest post-checkpoint entry (maybeSafetyValve never evicts the last row):
	// a session with any data always has >=1 PG row, so "empty tail but segments
	// exist" cannot occur — a future eviction change MUST preserve this or resume
	// would wrongly NotFound a segment-only session.
	tail, segments, err := reader.SessionResumeSnapshot(ctx, sessionID)
	if err != nil {
		return nil, reconstructReadError(err)
	}

	// The checkpoint (if any) is the first row of the hot-tail and is a full
	// header-first body: it leads verbatim. Everything after it is a later delta
	// merged by entry_seq. With no checkpoint, the whole tail is deltas and there
	// is no distinguished header row — SessionTranscript still returns them in
	// seq order, so the plain join is header-first by construction.
	var (
		head     string    // the checkpoint full-body line, emitted first when present
		haveHead bool      // whether the first hot-tail row is a checkpoint (header body)
		deltas   []seqLine // every later entry, merged from PG tail + fetched segments
	)
	if len(tail) > 0 && tail[0].Checkpoint {
		head, haveHead = tail[0].EntryJSON, true
		for _, row := range tail[1:] {
			deltas = append(deltas, seqLine{seq: row.EntrySeq, line: row.EntryJSON})
		}
	} else {
		for _, row := range tail {
			deltas = append(deltas, seqLine{seq: row.EntrySeq, line: row.EntryJSON})
		}
	}

	// S3 fallback: only when the valve fired. Fetch each safety_valve segment and
	// fold its lines into the delta set at their entry_seq. By construction these
	// segments are all post-latest-checkpoint (the store re-marks any stale
	// segment superseded when a later checkpoint arrives), so they merge behind
	// the checkpoint body with the PG tail. NOTE: this object read runs OUTSIDE the
	// RepeatableRead snapshot that made the manifest+tail read atomic; it is safe
	// only because safety_valve segment objects are immutable and never deleted
	// while a manifest row references them (no object-delete path exists, and
	// PutSegment is ON CONFLICT DO NOTHING on a deterministic key). Any future
	// object-GC path MUST preserve that, or move this read into the snapshot's
	// consistency domain.
	for _, seg := range segments {
		body, err := reader.ReadArchiveSegment(ctx, seg.ObjectKey)
		if err != nil {
			return nil, reconstructReadError(err)
		}
		for i, line := range splitSegmentLines(body) {
			// The segment covers [MinEntrySeq..MaxEntrySeq] contiguously in JSONL
			// order; stamp each line with its seq so the merge orders it against
			// the PG tail. The store writes one entry per line in seq order.
			deltas = append(deltas, seqLine{seq: seg.MinEntrySeq + uint64(i), line: line})
		}
	}

	sort.SliceStable(deltas, func(i, j int) bool { return deltas[i].seq < deltas[j].seq })

	lines := make([]string, 0, len(deltas)+1)
	if haveHead {
		lines = append(lines, head)
	}
	for _, d := range deltas {
		lines = append(lines, d.line)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// seqLine is one transcript line keyed by its session-scoped entry_seq, so lines
// from the PG tail and fetched S3 segments merge into a single seq-ordered body.
type seqLine struct {
	seq  uint64
	line string
}

// splitSegmentLines splits a verbatim-JSONL segment body into its lines. A
// segment is entry_json values joined by '\n' (store.collectSegment), so this is
// the inverse. An empty body — or one that is only newlines — yields no lines
// (never a blank line that would inject a spurious row into the reconstructed
// file). Trailing newlines are stripped first, so a body written with a trailing
// '\n' does not yield a spurious empty final line that would be stamped a seq and
// emitted as a blank JSONL row. This relies on collectSegment never emitting an
// empty entry_json between real lines; internal empties are deliberately not
// filtered, since dropping them would shift the seq stamping in the caller.
func splitSegmentLines(body []byte) []string {
	trimmed := strings.TrimRight(string(body), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// reconstructReadError maps a transcript read error onto the Connect code the
// resume contract expects: the store's not-found (unknown/empty session) is
// CodeNotFound, and any other read fault is CodeInternal — never leaked as a
// bare CodeUnknown.
func reconstructReadError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
