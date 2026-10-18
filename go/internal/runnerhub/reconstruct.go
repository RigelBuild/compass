//go:build unix

package runnerhub

// The resume-body reconstructor. ReconstructSessionBody rebuilds the loadable
// SDK session file a resuming agent starts from, by pure read-and-concatenate
// over the two-tier transcript store — no control-lane ops, no entry parsing.

import (
	"context"
	"errors"
	"sort"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/store"
)

// errReaderUnavailable fails closed when the resume read leg is not wired.
// Returning an empty body would make the agent resume as if the session were fresh.
var errReaderUnavailable = errors.New("runnerhub: no transcript reader wired to serve ReconstructSessionBody")

// ReconstructSessionBody concatenates opaque transcript entries into a loadable SDK session file.
// It does not parse entries.
//
// Normal reads concatenate the PG tail; safety-valve reads merge archived segments by entry sequence.
// Checkpoints provide the full header-first body, and unknown sessions return NotFound.
func (h *Hub) ReconstructSessionBody(ctx context.Context, sessionID string) ([]byte, error) {
	h.mu.Lock()
	reader := h.reader
	h.mu.Unlock()
	if reader == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errReaderUnavailable.Error()).WithCause(errReaderUnavailable)
	}

	// Read the PG tail and valve manifest atomically so concurrent flushes cannot corrupt the body.
	// The valve must retain the last row or an empty-tail resume would wrongly NotFound.
	tail, segments, err := reader.SessionResumeSnapshot(ctx, sessionID)
	if err != nil {
		return nil, reconstructReadError(err)
	}

	// A checkpoint is a full header-first body; append later entries in sequence order.
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

	// Fetch immutable segments outside the snapshot and merge their rows by entry sequence.
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

// seqLine pairs a transcript line with its session-scoped sequence for merging.
type seqLine struct {
	seq  uint64
	line string
}

// splitSegmentLines reverses the store's JSONL join without adding rows for empty or trailing lines.
// Preserve internal empty entries because filtering them would shift sequence numbers.
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
		return connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	default:
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
}
