package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

const (
	// defaultFlushPassTimeout bounds one pass; it moves a cap-sized segment at
	// about 9 Mbit/s, so a hung object store cannot hold a slot forever.
	defaultFlushPassTimeout = 60 * time.Second
	// maxConcurrentFlushes caps passes in flight, bounding the segment bodies
	// held in memory while their PUTs run.
	maxConcurrentFlushes = 4
)

// errFlushRaced marks a pruning flush whose delete count differed from the rows
// it archived: another pass or writer moved the range, so its tx rolled back.
var errFlushRaced = errors.New("store: flush prune count mismatch")

// errNoObjectStore is the flush failure when no archive tier is wired.
var errNoObjectStore = errors.New("store: object store not configured for transcript flush")

// errFlusherStopped is FlushTranscript's result once Close has stopped the worker.
var errFlusherStopped = errors.New("store: transcript flusher stopped")

// flushSlot is one session with a pass owed or running. Guarded by
// transcriptFlusher.mu.
type flushSlot struct {
	tenant                 TenantID
	system, running, again bool
	err                    error         // last pass result
	idle                   chan struct{} // closed when the slot is deleted
}

// transcriptFlusher runs transcript flushes off the commit path, one pass at a
// time per session. The Store owns it: Open starts it and Close joins it.
type transcriptFlusher struct {
	mu             sync.Mutex
	slots          map[string]*flushSlot
	wake, sem      chan struct{} // cap 1; cap maxConcurrentFlushes
	wg             sync.WaitGroup
	stop           context.CancelFunc
	stopped        chan struct{}                     // closed after the loop and passes end
	failures       metric.Int64Counter               // nil when the meter failed
	passDone       func(sessionID string, err error) // test hook; nil in prod
	afterValveRead func()                            // test hook; nil in prod
}

func newTranscriptFlusher(failures metric.Int64Counter) *transcriptFlusher {
	return &transcriptFlusher{
		slots:    map[string]*flushSlot{},
		wake:     make(chan struct{}, 1),
		sem:      make(chan struct{}, maxConcurrentFlushes),
		stopped:  make(chan struct{}),
		failures: failures,
	}
}

// startTranscriptFlusher starts the run loop. Its ctx keeps ctx's values but
// not its cancellation, so only Close stops it.
func (s *Store) startTranscriptFlusher(ctx context.Context) {
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.flushes.stop = stop
	go s.runTranscriptFlusher(runCtx)
}

// requestFlush owes sessionID a pass and never blocks. A running slot is marked
// to run once more, so triggers that land mid-pass coalesce into one pass.
func (s *Store) requestFlush(ctx context.Context, sessionID string) *flushSlot {
	f := s.flushes
	f.mu.Lock()
	slot, ok := f.slots[sessionID]
	switch {
	case !ok:
		// The commit ctx carries the session's tenant; the pass reuses its scope
		// but not its deadline.
		slot = &flushSlot{tenant: s.resolveTenant(ctx), system: isSystemRole(ctx), idle: make(chan struct{})}
		f.slots[sessionID] = slot
	case slot.running:
		slot.again = true
	}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default: // A wake is already pending; the loop sees this slot on it.
	}
	return slot
}

// runTranscriptFlusher starts one goroutine per owed slot until ctx ends, then
// waits for every pass before closing stopped.
func (s *Store) runTranscriptFlusher(ctx context.Context) {
	f := s.flushes
	defer close(f.stopped)
	for {
		select {
		case <-ctx.Done():
			f.wg.Wait()
			return
		case <-f.wake:
		}
		f.mu.Lock()
		for sessionID, slot := range f.slots {
			if slot.running {
				continue
			}
			slot.running = true
			f.wg.Go(func() { s.runSlot(ctx, sessionID, slot) })
		}
		f.mu.Unlock()
	}
}

// runSlot runs passes for one session until no trigger is owed, then deletes
// the slot.
func (s *Store) runSlot(ctx context.Context, sessionID string, slot *flushSlot) {
	f := s.flushes
	for {
		err := s.runPass(ctx, sessionID, slot)
		if f.passDone != nil {
			f.passDone(sessionID, err)
		}
		f.mu.Lock()
		slot.err = err
		if slot.again && ctx.Err() == nil {
			slot.again = false
			f.mu.Unlock()
			continue
		}
		delete(f.slots, sessionID)
		close(slot.idle)
		f.mu.Unlock()
		return
	}
}

// runPass holds a semaphore slot and runs one pass under the slot's tenant
// scope and the pass deadline. A failure is logged, counted, and dropped: the
// next commit's pass covers the same rows.
func (s *Store) runPass(ctx context.Context, sessionID string, slot *flushSlot) error {
	f := s.flushes
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("store: transcript flush: %w", ctx.Err())
	}
	defer func() { <-f.sem }()

	passCtx := WithTenant(ctx, slot.tenant)
	if slot.system {
		passCtx = WithSystemRole(passCtx)
	}
	passCtx, cancel := context.WithTimeout(passCtx, s.flushPassTimeout)
	defer cancel()
	err := s.flushPass(passCtx, sessionID)
	if err != nil {
		slog.ErrorContext(ctx, "transcript flush failed", "session_id", sessionID, "err", err)
		if f.failures != nil {
			f.failures.Add(ctx, 1)
		}
	}
	return err
}

// flushPass works on the rows in PG now, not on its trigger: the primary flush
// below the latest checkpoint, then the safety valve.
func (s *Store) flushPass(ctx context.Context, sessionID string) error {
	cp, err := s.latestCheckpointSeq(ctx, sessionID)
	if err != nil {
		return err
	}
	if cp > 0 {
		// Archive every pre-checkpoint row as one superseded segment, and re-mark
		// any safety_valve segment the checkpoint now supersedes, in one tx.
		if err := s.flushUpto(ctx, sessionID, 0, cp-1, SegmentKindSuperseded, true, cp); err != nil {
			return err
		}
	}
	return s.maybeSafetyValve(ctx, sessionID)
}

// FlushTranscript runs a pass for sessionID and waits for its slot to go idle,
// returning the last pass's result. It is the synchronous barrier for tests.
func (s *Store) FlushTranscript(ctx context.Context, sessionID string) error {
	if s.objectStore == nil {
		return errNoObjectStore
	}
	slot := s.requestFlush(ctx, sessionID)
	select {
	case <-slot.idle:
		s.flushes.mu.Lock()
		defer s.flushes.mu.Unlock()
		return slot.err
	case <-ctx.Done():
		return fmt.Errorf("store: wait for transcript flush: %w", ctx.Err())
	case <-s.flushes.stopped:
		return errFlusherStopped
	}
}

// valveCut picks the safety valve's eviction range from the post-checkpoint
// rows: the oldest rows until the rest fit capBytes, never the newest row, and
// never a checkpoint row or anything after it. upto == 0 means no cut.
func valveCut(rows []db.HotTailSizesRow, capBytes int64) (from, upto uint64) {
	var total int64
	for _, r := range rows {
		total += r.Bytes
	}
	if total <= capBytes {
		return 0, 0
	}
	remaining := total
	for i := range len(rows) - 1 {
		if remaining <= capBytes || rows[i].Checkpoint {
			break
		}
		upto = toUint64(rows[i].EntrySeq)
		remaining -= rows[i].Bytes
	}
	if upto == 0 {
		return 0, 0
	}
	return toUint64(rows[0].EntrySeq), upto
}
