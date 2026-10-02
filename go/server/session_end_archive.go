package server

import (
	"context"
	"log/slog"

	"github.com/RigelBuild/compass/go/internal/store"
)

// archiveSessionEnd archives a session's remaining hot tail as one session_end
// segment, so history is complete for analytics. The PG tail is not pruned and
// stays authoritative for resume. Best-effort: failures are logged.
func archiveSessionEnd(ctx context.Context, st *store.Store, sessionID string) {
	maxSeq, err := st.SessionMaxEntrySeq(ctx, sessionID)
	if err != nil {
		slog.ErrorContext(ctx, "session-end transcript flush skipped: could not read max entry seq",
			"session_id", sessionID, "error", err)
		return
	}
	if maxSeq == 0 {
		return // no transcript rows: nothing to archive
	}
	if err := st.FlushSuperseded(ctx, sessionID, maxSeq, store.SegmentKindSessionEnd); err != nil {
		slog.ErrorContext(ctx, "session-end transcript flush failed",
			"session_id", sessionID, "upto_entry_seq", maxSeq, "error", err)
	}
}

// sessionEndArchiver archives sessions the hub saw end without Stop: a lost
// container or a re-enroll reap. A later resume and Stop archive again; the
// manifest dedups an identical range.
type sessionEndArchiver struct{ st *store.Store }

func (a sessionEndArchiver) OnSessionEnded(ctx context.Context, sessionID string) {
	archiveSessionEnd(ctx, a.st, sessionID)
}
