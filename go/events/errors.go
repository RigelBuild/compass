package events

import "errors"

// ErrBufferUnderflow signals that the caller's sinceSeq cannot be served by a
// gap-free replay: its epoch belongs to a prior server instance, it predates the
// oldest retained event (the span below it was evicted), or it sits at or beyond
// the next seq the bus would assign. Either way the handler answers with
// ResyncRequired and the client re-snapshots at sinceSeq = 0.
var ErrBufferUnderflow = errors.New("events: buffer underflow; resync required")
