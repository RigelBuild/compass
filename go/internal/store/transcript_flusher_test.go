//go:build pgtest

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingObjectStore holds every PUT until release closes or the pass ctx
// ends, and records how many PUTs ran at once.
type blockingObjectStore struct {
	mu          sync.Mutex
	objects     map[string][]byte
	inFlight    int
	maxInFlight int
	started     chan string
	release     chan struct{}
}

func newBlockingObjectStore() *blockingObjectStore {
	return &blockingObjectStore{
		objects: map[string][]byte{},
		started: make(chan string, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingObjectStore) PutSegment(ctx context.Context, key string, body []byte) error {
	b.mu.Lock()
	b.inFlight++
	b.maxInFlight = max(b.maxInFlight, b.inFlight)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	b.started <- key
	select {
	case <-b.release:
	case <-ctx.Done():
		return fmt.Errorf("blocking fake: %w", ctx.Err())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = append([]byte(nil), body...)
	return nil
}

func (b *blockingObjectStore) GetSegment(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("blocking fake: no object at %q", key)
	}
	return body, nil
}

func (b *blockingObjectStore) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxInFlight
}

// recv waits for one value on ch, failing the test when ctx ends first.
func recv[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", what, ctx.Err())
		var zero T
		return zero
	}
}

// awaitIdle waits for the session's current slot, if any, and returns its last
// pass result.
func awaitIdle(t *testing.T, s *Store, sessionID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s.flushes.mu.Lock()
	slot, ok := s.flushes.slots[sessionID]
	s.flushes.mu.Unlock()
	if !ok {
		return nil
	}
	recv(t, ctx, slot.idle, "the slot to go idle")
	s.flushes.mu.Lock()
	defer s.flushes.mu.Unlock()
	return slot.err
}

// boundSession seeds a session and binds its lifetime, so appends land at seq.
func boundSession(t *testing.T, s *Store, handle string) string {
	t.Helper()
	sess := seedSession(t, s, handle, "sess-"+handle)
	if _, err := s.BindLifetime(t.Context(), sess, sessionOwner(t, s, sess)); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	return sess
}

// pad40 is a 40-byte entry body, sized against the 40- and 80-byte test caps.
func pad40(c string) string {
	return `{"pad":"` + strings.Repeat(c, 30) + `"}`
}

// A commit returns once its row commits, while the flush's PUT is still blocked.
func TestAppendReturnsWhileFlushPutBlocked(t *testing.T) {
	cases := []struct {
		name     string
		cap      int
		seed     []bool // checkpoint flag per seeded seq, from 1
		trigger  bool   // checkpoint flag of the triggering append
		body     string
		wantKind SegmentKind
		wantSeg  [2]uint64
		wantTail []uint64
	}{
		{
			name: "checkpoint", seed: []bool{false, false}, trigger: true, body: `{"cp":3}`,
			wantKind: SegmentKindSuperseded, wantSeg: [2]uint64{1, 2}, wantTail: []uint64{3},
		},
		{
			name: "safety valve", cap: 40, seed: []bool{true, false}, trigger: false, body: pad40("v"),
			wantKind: SegmentKindSafetyValve, wantSeg: [2]uint64{2, 2}, wantTail: []uint64{1, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if tc.cap > 0 {
				s.SetSafetyValveCapBytes(tc.cap)
			}
			sess := boundSession(t, s, "nb-"+strings.ReplaceAll(tc.name, " ", ""))
			// Seed with no object store, so the seed requests no pass.
			for i, cp := range tc.seed {
				appendOK(t, s, sess, uint64(i+1), cp, pad40("s"))
			}
			fake := newBlockingObjectStore()
			s.SetObjectStore(fake)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			seq := uint64(len(tc.seed) + 1)
			if err := s.AppendTranscriptEntry(ctx, sess, seq, tc.trigger, tc.body, fmt.Sprintf("idem-%d", seq)); err != nil {
				t.Fatalf("append = %v, want nil while the PUT is blocked", err)
			}
			recv(t, ctx, fake.started, "the flush PUT")
			if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1, 2, 3) {
				t.Fatalf("hot tail while the PUT is blocked = %v, want [1 2 3]", got)
			}

			close(fake.release)
			if err := s.FlushTranscript(ctx, sess); err != nil {
				t.Fatalf("FlushTranscript: %v", err)
			}
			man := manifestRows(t, s, sess)
			if len(man) != 1 || man[0].Kind != tc.wantKind || man[0].MinEntrySeq != tc.wantSeg[0] || man[0].MaxEntrySeq != tc.wantSeg[1] {
				t.Fatalf("manifest = %+v, want one %s %v", man, tc.wantKind, tc.wantSeg)
			}
			if got := hotTailSeqs(t, s, sess); !seqsEqual(got, tc.wantTail...) {
				t.Fatalf("hot tail after flush = %v, want %v", got, tc.wantTail)
			}
		})
	}
}

// Triggers that land during a running pass coalesce into one more pass, and one
// session's PUTs never overlap.
func TestFlushTriggersCoalesce(t *testing.T) {
	s := newTestStore(t)
	sess := boundSession(t, s, "coalesce")
	appendOK(t, s, sess, 1, false, `{"e":1}`)
	appendOK(t, s, sess, 2, false, `{"e":2}`)
	fake := newBlockingObjectStore()
	s.SetObjectStore(fake)
	var (
		mu     sync.Mutex
		passes int
	)
	s.flushes.passDone = func(string, error) {
		mu.Lock()
		passes++
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	appendOK(t, s, sess, 3, true, `{"cp":3}`)
	recv(t, ctx, fake.started, "pass 1's PUT")
	s.flushes.mu.Lock()
	slot := s.flushes.slots[sess]
	s.flushes.mu.Unlock()
	appendOK(t, s, sess, 4, false, `{"e":4}`)
	appendOK(t, s, sess, 5, true, `{"cp":5}`)

	close(fake.release)
	recv(t, ctx, slot.idle, "the slot to go idle")
	mu.Lock()
	got := passes
	mu.Unlock()
	if got != 2 {
		t.Fatalf("passes = %d, want 2 (the two mid-pass triggers coalesce)", got)
	}
	if peak := fake.peak(); peak != 1 {
		t.Fatalf("concurrent PUTs = %d, want 1", peak)
	}
	if tail := hotTailSeqs(t, s, sess); !seqsEqual(tail, 5) {
		t.Fatalf("hot tail = %v, want [5]", tail)
	}
	man := manifestRows(t, s, sess)
	if len(man) != 2 || man[0].MinEntrySeq != 1 || man[0].MaxEntrySeq != 2 || man[1].MinEntrySeq != 3 || man[1].MaxEntrySeq != 4 {
		t.Fatalf("manifest = %+v, want superseded [1..2] and [3..4]", man)
	}
}

// A checkpoint that commits between the valve's reads is never evicted: resume
// heads with it, and no segment covers it.
func TestSafetyValveKeepsCheckpointCommittedMidPass(t *testing.T) {
	s := newTestStore(t)
	s.SetSafetyValveCapBytes(40)
	sess := boundSession(t, s, "cpcut")
	appendOK(t, s, sess, 1, true, `{"cp":1}`)
	appendOK(t, s, sess, 2, false, pad40("a"))
	appendOK(t, s, sess, 3, false, pad40("b"))
	s.SetObjectStore(newFakeObjectStore())
	var once sync.Once
	s.flushes.afterValveRead = func() {
		once.Do(func() {
			// A 40-byte checkpoint lands inside the range a stale cut would evict.
			for _, a := range []struct {
				seq  uint64
				cp   bool
				body string
			}{{4, true, pad40("c")}, {5, false, `{"e":5}`}} {
				if err := s.AppendTranscriptEntry(t.Context(), sess, a.seq, a.cp, a.body, fmt.Sprintf("idem-%d", a.seq)); err != nil {
					t.Errorf("mid-pass append %d: %v", a.seq, err)
				}
			}
		})
	}

	mustFlush(t, s, sess)
	tail := hotTailSeqs(t, s, sess)
	if !seqsEqual(tail, 4, 5) {
		t.Fatalf("hot tail = %v, want [4 5] (the mid-pass checkpoint kept)", tail)
	}
	for _, m := range manifestRows(t, s, sess) {
		if m.MinEntrySeq <= 4 && m.MaxEntrySeq >= 4 {
			t.Fatalf("segment %+v covers the mid-pass checkpoint 4", m)
		}
	}
	snap, _, err := s.SessionResumeSnapshot(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionResumeSnapshot: %v", err)
	}
	if len(snap) == 0 || snap[0].EntrySeq != 4 || !snap[0].Checkpoint {
		t.Fatalf("resume tail = %+v, want it headed by checkpoint 4", snap)
	}
}

// Two Stores whose valve ranges overlap under different keys: the database lets
// exactly one prune commit, and the other pass rolls back with errFlushRaced.
func TestOverlappingFlushesPruneOnce(t *testing.T) {
	a, dsn := newTestStoreDSN(t)
	b := reopenStore(t, dsn)
	a.SetSafetyValveCapBytes(40)
	b.SetSafetyValveCapBytes(80)
	sess := boundSession(t, a, "raced")
	appendOK(t, a, sess, 1, true, `{"cp":1}`)
	appendOK(t, a, sess, 2, false, pad40("a"))
	appendOK(t, a, sess, 3, false, pad40("b"))
	fake := newBlockingObjectStore()
	a.SetObjectStore(fake)
	b.SetObjectStore(fake)
	passA := make(chan error, 4)
	a.flushes.passDone = func(_ string, err error) { passA <- err }

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// A (cap 40) cuts [2..3]; B (cap 80) cuts [2..2].
	appendOK(t, a, sess, 4, false, pad40("c"))
	errB := make(chan error, 1)
	go func() { errB <- b.FlushTranscript(ctx, sess) }()
	recv(t, ctx, fake.started, "the first PUT")
	recv(t, ctx, fake.started, "the second PUT")
	close(fake.release)
	gotA := recv(t, ctx, passA, "A's pass")
	gotB := recv(t, ctx, errB, "B's flush")

	raced := 0
	for _, err := range []error{gotA, gotB} {
		switch {
		case errors.Is(err, errFlushRaced):
			raced++
		case err != nil:
			t.Fatalf("pass error = %v, want nil or errFlushRaced", err)
		}
	}
	if raced != 1 {
		t.Fatalf("passes reporting errFlushRaced = %d (A=%v, B=%v), want 1", raced, gotA, gotB)
	}
	man := manifestRows(t, a, sess)
	if len(man) != 1 {
		t.Fatalf("manifest = %+v, want exactly one row", man)
	}
	var want []uint64
	for seq := uint64(1); seq <= 4; seq++ {
		if seq < man[0].MinEntrySeq || seq > man[0].MaxEntrySeq {
			want = append(want, seq)
		}
	}
	if got := hotTailSeqs(t, a, sess); !seqsEqual(got, want...) {
		t.Fatalf("hot tail = %v, want %v (only the winning segment's rows pruned)", got, want)
	}
}

// A pass that outlives its deadline fails with DeadlineExceeded and leaves PG
// untouched.
func TestFlushPassDeadline(t *testing.T) {
	s := newTestStore(t)
	s.flushPassTimeout = 50 * time.Millisecond
	sess := boundSession(t, s, "deadline")
	appendOK(t, s, sess, 1, false, `{"e":1}`)
	appendOK(t, s, sess, 2, false, `{"e":2}`)
	appendOK(t, s, sess, 3, true, `{"cp":3}`)
	s.SetObjectStore(newBlockingObjectStore()) // never released

	err := s.FlushTranscript(t.Context(), sess)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FlushTranscript = %v, want context.DeadlineExceeded", err)
	}
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1, 2, 3) {
		t.Fatalf("hot tail = %v, want [1 2 3]", got)
	}
	if man := manifestRows(t, s, sess); len(man) != 0 {
		t.Fatalf("manifest = %+v, want empty", man)
	}
}

// shutdownObjectStore reports the pass ctx's cancellation, then holds the PUT
// until release so the test controls when the pass ends.
type shutdownObjectStore struct {
	started, cancelled, release chan struct{}
}

func (o *shutdownObjectStore) PutSegment(ctx context.Context, _ string, _ []byte) error {
	close(o.started)
	<-ctx.Done()
	close(o.cancelled)
	<-o.release
	return fmt.Errorf("shutdown fake: %w", ctx.Err())
}

func (o *shutdownObjectStore) GetSegment(context.Context, string) ([]byte, error) {
	return nil, errors.New("shutdown fake: no reads")
}

// Close cancels the in-flight pass and waits for it before closing the pool.
func TestCloseJoinsInFlightFlush(t *testing.T) {
	s, dsn := newTestStoreDSN(t)
	sess := boundSession(t, s, "shutdown")
	appendOK(t, s, sess, 1, false, `{"e":1}`)
	appendOK(t, s, sess, 2, false, `{"e":2}`)
	fake := &shutdownObjectStore{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	s.SetObjectStore(fake)
	closeDone := make(chan struct{})
	passDone := make(chan struct{})
	s.flushes.passDone = func(string, error) {
		select {
		case <-closeDone:
			t.Errorf("pass ended after Close returned")
		default:
		}
		close(passDone)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	appendOK(t, s, sess, 3, true, `{"cp":3}`)
	recv(t, ctx, fake.started, "the flush PUT")
	go func() {
		s.Close()
		close(closeDone)
	}()
	recv(t, ctx, fake.cancelled, "the pass ctx to cancel")
	select {
	case <-closeDone:
		t.Fatal("Close returned while a pass was in flight")
	default:
	}
	close(fake.release)
	recv(t, ctx, closeDone, "Close")
	select {
	case <-passDone:
	default:
		t.Fatal("Close returned before the pass ended")
	}

	other := reopenStore(t, dsn)
	if got := hotTailSeqs(t, other, sess); !seqsEqual(got, 1, 2, 3) {
		t.Fatalf("hot tail after Close = %v, want [1 2 3]", got)
	}
	if man := manifestRows(t, other, sess); len(man) != 0 {
		t.Fatalf("manifest after Close = %+v, want empty", man)
	}
}

// A pass runs under the committing request's tenant, not the bootstrap tenant.
func TestFlushRunsUnderAppendTenant(t *testing.T) {
	s, fake := storeWithFake(t)
	tenantB := seedTenant(t, s, "flush-tenant-b")
	ctxB := WithTenant(t.Context(), tenantB)
	agent := seedTenantAgent(t, ctxB, s, "flush-b")
	const sess = "sess-flush-b"
	if err := s.RecordAgentSession(ctxB, sess, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	for _, a := range []struct {
		seq uint64
		cp  bool
	}{{1, false}, {2, false}, {3, true}} {
		if err := s.AppendTranscriptEntry(ctxB, sess, a.seq, a.cp, fmt.Sprintf(`{"e":%d}`, a.seq), fmt.Sprintf("idem-b-%d", a.seq)); err != nil {
			t.Fatalf("append %d: %v", a.seq, err)
		}
	}
	// Wait on the append's own slot: a new FlushTranscript slot would take the
	// waiter's tenant instead.
	if err := awaitIdle(t, s, sess); err != nil {
		t.Fatalf("flush pass: %v", err)
	}
	man := manifestRows(t, s, sess)
	if len(man) != 1 || !fake.has(man[0].ObjectKey) {
		t.Fatalf("manifest = %+v, want one stored segment", man)
	}
	var tenant string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT tenant_id FROM agent_session_archive_segments WHERE session_id = $1`, sess).Scan(&tenant); err != nil {
		t.Fatalf("read segment tenant: %v", err)
	}
	if tenant != string(tenantB) {
		t.Fatalf("segment tenant = %q, want %q", tenant, tenantB)
	}
}

// A safety_valve row at or below the latest checkpoint is stale; both segment
// readers drop it and keep the one above.
func TestSafetyValveSegmentsHideRowsBelowCheckpoint(t *testing.T) {
	s := newTestStore(t)
	sess := boundSession(t, s, "svfilter")
	for seq, cp := range []bool{true, false, false, true, false} {
		appendOK(t, s, sess, uint64(seq+1), cp, fmt.Sprintf(`{"e":%d}`, seq+1))
	}
	for _, r := range [][2]int64{{2, 3}, {5, 5}} {
		if _, err := s.pool.Exec(t.Context(),
			`INSERT INTO agent_session_archive_segments (session_id, object_key, min_entry_seq, max_entry_seq, kind, tenant_id)
			 VALUES ($1, $2, $3, $4, 'safety_valve', $5)`,
			sess, segmentKey(sess, toUint64(r[0]), toUint64(r[1])), r[0], r[1], string(s.bootstrapTenantID)); err != nil {
			t.Fatalf("seed manifest row %v: %v", r, err)
		}
	}
	pool, err := s.SafetyValveSegments(t.Context(), sess)
	if err != nil {
		t.Fatalf("SafetyValveSegments: %v", err)
	}
	_, snap, err := s.SessionResumeSnapshot(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionResumeSnapshot: %v", err)
	}
	for name, got := range map[string][]ArchiveSegmentRow{"SafetyValveSegments": pool, "SessionResumeSnapshot": snap} {
		if len(got) != 1 || got[0].MinEntrySeq != 5 || got[0].MaxEntrySeq != 5 {
			t.Errorf("%s = %+v, want only the [5..5] segment above checkpoint 4", name, got)
		}
	}
}

// gatedObjectStore counts PUTs in flight, signals each entry, and holds each PUT
// until one release token arrives or the pass ctx ends.
type gatedObjectStore struct {
	mu      sync.Mutex
	active  int
	peak    int
	entered chan string
	release chan struct{}
}

func (g *gatedObjectStore) PutSegment(ctx context.Context, key string, _ []byte) error {
	g.mu.Lock()
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
	g.entered <- key
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gated fake: %w", ctx.Err())
	}
}

func (g *gatedObjectStore) GetSegment(context.Context, string) ([]byte, error) {
	return nil, errors.New("gated fake: no reads")
}

func (g *gatedObjectStore) counts() (active, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, g.peak
}

// Passes across sessions share one cap: a fifth session's pass waits until one
// of maxConcurrentFlushes running passes ends.
func TestFlushPassesShareGlobalCap(t *testing.T) {
	s := newTestStore(t)
	const sessions = maxConcurrentFlushes + 1
	ids := make([]string, sessions)
	for i := range ids {
		ids[i] = boundSession(t, s, fmt.Sprintf("cap%d", i))
		// Idempotency keys are global, so each session needs its own.
		for seq, cp := range []bool{false, false, true} {
			if err := s.AppendTranscriptEntry(t.Context(), ids[i], uint64(seq+1), cp, `{"e":1}`, fmt.Sprintf("cap%d-%d", i, seq)); err != nil {
				t.Fatalf("append %s/%d: %v", ids[i], seq+1, err)
			}
		}
	}
	fake := &gatedObjectStore{entered: make(chan string, sessions), release: make(chan struct{})}
	s.SetObjectStore(fake)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, id := range ids[:maxConcurrentFlushes] {
		s.requestFlush(ctx, id)
	}
	for range maxConcurrentFlushes {
		recv(t, ctx, fake.entered, "a capped PUT")
	}

	// The fifth pass is owed but must not reach its PUT while four hold the cap.
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelShort()
	if err := s.FlushTranscript(short, ids[maxConcurrentFlushes]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FlushTranscript on the fifth session = %v, want context.DeadlineExceeded", err)
	}
	if active, peak := fake.counts(); active != maxConcurrentFlushes || peak != maxConcurrentFlushes {
		t.Fatalf("PUTs active=%d peak=%d, want %d (the cap)", active, peak, maxConcurrentFlushes)
	}

	fake.release <- struct{}{}
	recv(t, ctx, fake.entered, "the queued fifth PUT")
	close(fake.release)
	for _, id := range ids {
		mustFlush(t, s, id)
	}
	if _, peak := fake.counts(); peak != maxConcurrentFlushes {
		t.Fatalf("peak PUTs = %d, want %d", peak, maxConcurrentFlushes)
	}
}
