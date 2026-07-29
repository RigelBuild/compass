package gateway

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// The agent is the untrusted side of this seam, and it chooses when to emit.
// These pin the ways that lets it reach past its own session's lifetime, and
// the bound on what one message can make the Runner allocate.

// An ack arriving after Retire must not resurrect the session.
//
// Stop ends the agent's child process; it does not close the socket, the HTTP
// server, or the agent's Publish stream, so an ack already in flight lands
// after the retirement. If that rebuilt the map entry, nothing would ever
// retire it again — the lifecycle already ran its one Stop for that id, and the
// next cycle mints a fresh one. The map would grow by one resurrected session
// per Stop/Start: the unbounded growth Retire exists to stop, reinstated by the
// untrusted side.
//
// RED against the creating lookup: the count comes back 1.
func TestControlAckAfterRetireDoesNotResurrectSession(t *testing.T) {
	p := newTestProducer()
	if err := p.Send(testSession, promptOp("op")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.Retire(testSession)
	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after Retire = %d, want 0 (the fixture must actually retire)", got)
	}

	// The trailing ack, after the lifecycle's one and only Stop for this id.
	p.AckControl(testSession, 1, nil)

	if got := p.sessionCount(); got != 0 {
		t.Errorf("sessions after a post-Retire ack = %d, want 0: the ack resurrected the retired session, "+
			"and nothing will retire it again", got)
	}
}

// The same for the barrier's ack entry point, which the agent also drives.
func TestControlReleaseAfterRetireDoesNotResurrectSession(t *testing.T) {
	p := newTestProducer()
	if err := p.Send(testSession, promptOp("op")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.Retire(testSession)

	p.ReleaseReplayBarrier(testSession)

	if got := p.sessionCount(); got != 0 {
		t.Errorf("sessions after a post-Retire ReplayCompleteAck = %d, want 0", got)
	}
}

// Modelling the real shape: the Runner reuses one container across Stop/Start,
// minting a fresh session id per cycle, and each cycle's agent emits one
// trailing ack. Every resurrection is permanent, so they accumulate.
func TestControlPostRetireAcksDoNotAccumulateAcrossCycles(t *testing.T) {
	const cycles = 50

	p := newControlProducer() // per-cycle ids, so each binds its own
	for i := range cycles {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		p.Bind(id) // the lifecycle's Start
		if err := p.Send(id, promptOp("op")); err != nil {
			t.Fatalf("Send on cycle %d: %v", i, err)
		}
		p.Retire(id)             // the lifecycle's Stop
		p.AckControl(id, 1, nil) // the agent's trailing ack, after Stop
	}

	if got := p.sessionCount(); got != 0 {
		t.Errorf("sessions after %d Stop/Start cycles each with a trailing ack = %d, want 0: "+
			"one resurrected session per cycle is the leak Retire was added to close", cycles, got)
	}
}

// seqSet is the retained-seq snapshot AckControl passes the builder.
func seqSet(seqs ...uint64) map[uint64]struct{} {
	set := make(map[uint64]struct{}, len(seqs))
	for _, seq := range seqs {
		set[seq] = struct{}{}
	}
	return set
}

// applied_above is agent-sized, and the only ceiling on the wire is the 16MiB
// read cap — which packed varints turn into millions of entries, each
// amplified into a map slot.
//
// The bound is an ALLOCATION property, so a correctness assertion cannot see
// it: an unbounded build prunes identically, just after allocating a slot per
// named seq. Measured against the builder rather than through AckControl,
// because the surrounding prune loop allocates on session state that two
// samples mutate as they run — comparing whole-AckControl samples drifts by an
// allocation and fails about one run in two hundred, which is a flake, not a
// finding.
//
// The seq window is deliberately far WIDER than retention here. Bounding by
// the window (cursor, nextSeq] instead of by retention passes a test where the
// two coincide, and is unbounded in general: an agent that prunes purely by
// applied_above holds the contiguous cursor at 0 forever, so retention never
// fills, Send never trips the cap, and nextSeq climbs for the life of the
// session. Retention is the real bound, so that is what this measures against.
//
// RED without the filter: the set grows with the field.
func TestBoundedAboveSetDoesNotScaleWithTheField(t *testing.T) {
	// Retention holds maxRetainedOps ops. The seqs run far above them, as they
	// do for any session whose cursor has not advanced.
	const beyondRetention = maxRetainedOps * 8

	retained := make([]uint64, maxRetainedOps)
	for i := range retained {
		retained[i] = uint64(beyondRetention + i + 1)
	}
	retainedSeqs := seqSet(retained...)

	oversized := append(append([]uint64{}, retained...), make([]uint64, maxRetainedOps*3)...)
	for i := maxRetainedOps; i < len(oversized); i++ {
		oversized[i] = uint64(i) // never retained: nothing to prune
	}

	bounded := testing.AllocsPerRun(10, func() {
		_ = boundedAboveSet(retained, retainedSeqs)
	})
	unbounded := testing.AllocsPerRun(10, func() {
		_ = boundedAboveSet(oversized, retainedSeqs)
	})

	if unbounded > bounded {
		t.Errorf("a set built from %d seqs allocated %.0f, vs %.0f from the %d retained: "+
			"the agent sizes this field, so it must not scale past what can be pruned",
			len(oversized), unbounded, bounded, len(retained))
	}
	if got := len(boundedAboveSet(oversized, retainedSeqs)); got != maxRetainedOps {
		t.Errorf("set size = %d, want the %d retained seqs", got, maxRetainedOps)
	}
}

// The proto specifies no ordering for applied_above, so position carries no
// meaning and the bound must not depend on it. An agent listing its set
// ascending — the natural encoding — puts the retained seqs LAST, so any
// positional bound (first N, or stop after N distinct) drops exactly the real
// entries and keeps the noise.
//
// RED against `appliedAbove[:maxRetainedOps]` and against a count-and-break
// bound alike: both discard the meaningful seq for sitting at the end.
func TestBoundedAboveSetKeepsEntriesRegardlessOfPosition(t *testing.T) {
	const live = 8

	// Filler first, the one retained seq last — the ascending encoding.
	ack := make([]uint64, 0, maxRetainedOps*4+1)
	for i := range maxRetainedOps * 4 {
		ack = append(ack, uint64(live+1+i))
	}
	ack = append(ack, live)

	above := boundedAboveSet(ack, seqSet(live))
	if _, ok := above[live]; !ok {
		t.Errorf("the retained seq %d was dropped: the bound must not depend on "+
			"an order the wire does not specify", live)
	}
	if len(above) != 1 {
		t.Errorf("set holds %d seqs, want only the 1 that can prune anything", len(above))
	}
}

// A seq the session no longer retains — already acked away, or never issued —
// prunes nothing, so naming it must not carry it.
func TestBoundedAboveSetDropsUnretainedSeqs(t *testing.T) {
	above := boundedAboveSet([]uint64{1, 2, 3, 7}, seqSet(7))

	if _, ok := above[7]; !ok {
		t.Errorf("above = %v, want the retained seq 7 kept", above)
	}
	if len(above) != 1 {
		t.Errorf("above = %v, want only 7: an unretained seq prunes nothing", above)
	}
}

// The ack still applies correctly through the bounded set: the cursor retires
// its prefix and a seq named above it is pruned individually.
func TestControlAckAppliesThroughTheBoundedSet(t *testing.T) {
	p := newTestProducer()

	for range 2 {
		if err := p.Send(testSession, promptOp("op")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	// Cursor takes op 1; op 2 is named above it, among far more entries than
	// retention can hold.
	ack := make([]uint64, 0, maxRetainedOps*4)
	for i := range maxRetainedOps * 4 {
		ack = append(ack, uint64(maxRetainedOps*8+i))
	}
	ack = append(ack, 2)
	p.AckControl(testSession, 1, ack)

	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()
	stream.none(t, "every op was acked, so a new subscription is owed nothing")
}

// The agent's own subscribe is the third path into control state, and it races
// Stop the same way an ack does: Gateway.Control resolves the session id, then
// Stop retires it, and only then does the handler reach serve. Creating on
// demand there rebuilt the entry — and permanently, since the lifecycle spends
// its one Stop for that id and the next cycle mints a fresh one.
//
// Bind and Retire are now the whole lifetime, so an id the Runner does not
// know is refused rather than minted.
//
// RED against the creating lookup: the entry survives the drainer's return.
func TestControlSubscribeAfterRetireIsRefused(t *testing.T) {
	p := newTestProducer()
	if err := p.Send(testSession, promptOp("op")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.Retire(testSession)

	// The subscribe that lost the race to Stop.
	err := p.serve(t.Context(), testSession, nullSink{})

	if !errors.Is(err, errNoBoundSession) {
		t.Errorf("serve on a retired session = %v, want errNoBoundSession: a stream to state "+
			"the lifecycle does not know is one nothing will ever write to", err)
	}
	if got := p.sessionCount(); got != 0 {
		t.Errorf("sessions after a post-Retire subscribe = %d, want 0: the subscribe resurrected "+
			"the session, and nothing will retire it again", got)
	}
}

// The same across the cycles that make it a leak rather than one stray entry.
func TestControlPostRetireSubscribesDoNotAccumulate(t *testing.T) {
	const cycles = 50

	p := newControlProducer()
	for i := range cycles {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		p.Bind(id) // Start
		if err := p.Send(id, promptOp("op")); err != nil {
			t.Fatalf("Send on cycle %d: %v", i, err)
		}
		p.Retire(id)                             // Stop
		_ = p.serve(t.Context(), id, nullSink{}) // the agent's trailing subscribe
	}

	if got := p.sessionCount(); got != 0 {
		t.Errorf("sessions after %d Stop/Start cycles each with a trailing subscribe = %d, want 0: "+
			"one resurrected session per cycle is the same leak, reached through Control", cycles, got)
	}
}

// Bind is idempotent because Start can be retried: a second Bind for a live id
// must not discard what the first one's session already retained.
func TestBindDoesNotDiscardLiveSessionState(t *testing.T) {
	p := newTestProducer()
	if err := p.Send(testSession, promptOp("retained")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	p.Bind(testSession) // a retried Start

	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()
	if in := stream.recv(t).GetPrompt().GetInput(); in != "retained" {
		t.Errorf("delivered %q, want the op retained before the re-Bind: re-binding a live "+
			"session must not replace its state", in)
	}
}

// Send is the lifecycle's own write path, not the agent's, but it names a
// session id and so it is the third way an entry could enter the map. It must
// refuse an id the Runner never bound and one it has retired: creating here
// would reinstate the leak from the other end, and a Send for a stopped
// session is a lifecycle bug the caller should see rather than a queue nobody
// will ever drain.
func TestControlSendRefusesUnboundSession(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(p *controlProducer)
	}{
		{"never bound", func(*controlProducer) {}},
		{"retired", func(p *controlProducer) { p.Bind(testSession); p.Retire(testSession) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newControlProducer()
			tt.setup(p)

			err := p.Send(testSession, promptOp("op"))
			if connect.CodeOf(err) != connect.CodeNotFound || !errors.Is(err, errNoBoundSession) {
				t.Fatalf("Send = %v (code %v), want CodeNotFound wrapping errNoBoundSession",
					err, connect.CodeOf(err))
			}
			if got := p.sessionCount(); got != 0 {
				t.Errorf("sessions after a refused Send = %d, want 0: a refusal must not "+
					"create the state it refuses to write to", got)
			}
		})
	}
}

// The agent sets the ack rate, and an ack naming nothing is a few bytes on the
// wire — so the work one costs must not scale with what the Runner is holding.
// The intersection of an empty field with anything is empty, so there is
// nothing to snapshot for it, and snapshotting anyway would charge a walk of
// retention under the session lock — exclusive with every Send and with the
// drainer — to the cheapest legal frame.
//
// An allocation property again, so it is measured rather than asserted.
// RED against an unconditional snapshot: the cost tracks retention.
func TestEmptyAckDoesNotAllocateWithRetention(t *testing.T) {
	measure := func(retained int) float64 {
		p := newTestProducer()
		for range retained {
			if err := p.Send(testSession, promptOp("op")); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		return testing.AllocsPerRun(10, func() {
			p.AckControl(testSession, 0, nil)
		})
	}

	empty, full := measure(0), measure(maxRetainedOps)
	if empty != 0 {
		t.Fatalf("an ack naming nothing allocated %.0f against no retention at all: the "+
			"comparison below is only meaningful against a zero baseline", empty)
	}
	if full > empty {
		t.Errorf("an ack naming nothing allocated %.0f against %d retained ops, vs %.0f "+
			"against none: the agent paces these, so one must not cost a walk of retention",
			full, maxRetainedOps, empty)
	}
}
