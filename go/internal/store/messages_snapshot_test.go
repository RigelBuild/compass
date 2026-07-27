//go:build pgtest

package store

import (
	"context"
	"sync"
	"testing"
)

// The snapshot-boundary tests need a concrete store-space seq to use as the
// point-in-time boundary, but Message does not expose Seq (scanMessages,
// messages.go:462, never selects it). We recover the boundary from the append
// order instead: messages.seq is BIGSERIAL starting at 1 (migrations/
// 0001_init.sql:114), and in a FRESH newTestStore store with a single channel
// and single-threaded sequential AppendMessage calls, the Nth appended message
// is assigned seq=N. So after appending K messages in order, SnapshotSeq=K is
// exactly the seq of the Kth message. The tests therefore assert on the stable,
// exposed message IDs + counts rather than raw seq.

// TestListMessagesSnapshotSeqBoundsPage is the SEA-1333 red-first regression:
// ListMessages must honor Page.SnapshotSeq as a point-in-time read boundary,
// returning only messages with seq <= SnapshotSeq (comms.proto:353-368,
// design.md:807-817). Today the field is accepted and ignored by the query, so
// a bounded read leaks every later row. RED until ListMessages honors
// Page.SnapshotSeq with WHERE seq <= $snap; GREEN after, with NO test change.
func TestListMessagesSnapshotSeqBoundsPage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	append1 := func(body string) MessageID {
		t.Helper()
		m, _, err := s.AppendMessage(ctx, Message{
			Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
			Blocks: []MessageBlock{textBlock(body)},
		}, "")
		if err != nil {
			t.Fatalf("AppendMessage(%s): %v", body, err)
		}
		return m.ID
	}

	// Append m1..m5 (seq 1..5) — these are the messages at or below the boundary.
	ids := make([]MessageID, 0, 7)
	for _, body := range []string{"m1", "m2", "m3", "m4", "m5"} {
		ids = append(ids, append1(body))
	}
	// snap = seq of m3 (the 3rd appended message in a fresh store → seq 3).
	const snap = uint64(3)
	// m6, m7 (seq 6,7) — strictly beyond the boundary; must never appear.
	for _, body := range []string{"m6", "m7"} {
		ids = append(ids, append1(body))
	}
	m1, m2, m3 := ids[0], ids[1], ids[2]

	idsOf := func(ms []Message) []MessageID {
		out := make([]MessageID, len(ms))
		for i, m := range ms {
			out[i] = m.ID
		}
		return out
	}
	wantOrder := func(t *testing.T, ms []Message, want ...MessageID) {
		t.Helper()
		got := idsOf(ms)
		if len(got) != len(want) {
			t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row order = %v, want %v (newest-first)", got, want)
			}
		}
	}

	// Single bounded page: only m1..m3 (seq <= 3), newest-first [m3,m2,m1].
	// RED today: the boundary is ignored → all 7 rows come back.
	page, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 100, SnapshotSeq: snap})
	if err != nil {
		t.Fatalf("ListMessages(bounded): %v", err)
	}
	wantOrder(t, page, m3, m2, m1)

	// Boundary holds ACROSS pages: first page [m3,m2], then before-m2 → [m1].
	// A leak would surface m4..m7 on either page.
	p1, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2, SnapshotSeq: snap})
	if err != nil {
		t.Fatalf("ListMessages(bounded page 1): %v", err)
	}
	wantOrder(t, p1, m3, m2)
	p2, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2, SnapshotSeq: snap, BeforeMessageID: m2})
	if err != nil {
		t.Fatalf("ListMessages(bounded page 2): %v", err)
	}
	wantOrder(t, p2, m1)

	// SnapshotSeq:0 means "latest / no boundary" — every current row (all 7).
	all, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 100, SnapshotSeq: 0})
	if err != nil {
		t.Fatalf("ListMessages(unbounded): %v", err)
	}
	if len(all) != 7 {
		t.Fatalf("SnapshotSeq:0 returned %d rows, want all 7 (0 = no boundary)", len(all))
	}
}

// TestListMessagesSnapshotSeqExcludesConcurrentWrites proves the SnapshotSeq
// filter excludes later-committed rows across a paginated catch-up: with the
// boundary at seq 4, rows seq 5..8 that are fully committed before the read
// never appear on any page, every boundary row appears exactly once, and no id
// spans two pages (design.md:807-817). The `written` handshake makes this
// deterministic (SEA-1226 start-barrier + WaitGroup style, NO sleeps): the
// writer appends seq 5..8 and closes `written`, and the reader waits on it
// before paging, so the store provably holds those later rows at read time —
// a build ignoring the boundary provably leaks them. The handshake serializes
// writer-before-reader by design, so this exercises the `WHERE seq <= $snap`
// clause — NOT a concurrent-with-read race, and NOT the subscribe-first
// live-tail ordering window (the assignment-order-vs-commit-order gap, closed
// in subscribe.go and not exercised here). RED until ListMessages honors
// Page.SnapshotSeq; GREEN after, with NO test change.
func TestListMessagesSnapshotSeqExcludesConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	append1 := func(body string) MessageID {
		t.Helper()
		m, _, err := s.AppendMessage(ctx, Message{
			Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
			Blocks: []MessageBlock{textBlock(body)},
		}, "")
		if err != nil {
			t.Fatalf("AppendMessage(%s): %v", body, err)
		}
		return m.ID
	}

	// Boundary set m1..m4 (seq 1..4), all appended BEFORE the barrier so their
	// seqs are deterministic. snap = seq of m4. rank records insertion order
	// (m1=0 .. m4=3) so we can assert newest-first (seq DESC) without a Seq field.
	boundary := map[MessageID]bool{}
	rank := map[MessageID]int{}
	for i, body := range []string{"m1", "m2", "m3", "m4"} {
		id := append1(body)
		boundary[id] = true
		rank[id] = i
	}
	const snap = uint64(4)

	// Goroutine A pages the catch-up under the boundary (page size 2, two pages),
	// unioning the ids it collects. Goroutine B appends new messages that get
	// seq > 4 and must never appear in A's view. Both release off one start
	// barrier (SEA-1226 style). For a deterministic RED with no sleeps, B closes
	// `written` once its appends have COMMITTED and A waits on it before paging:
	// this guarantees the seq>4 rows exist in the store at read time, so a build
	// that ignores the boundary provably leaks them, while the filtered build
	// provably excludes them. Without the handshake the leak is a scheduler race
	// (the reader might win), which would let a broken build pass — a flaky test.
	start := make(chan struct{})
	written := make(chan struct{})
	var wg sync.WaitGroup
	var collected []Message
	var pageA1, pageA2 []Message
	var readErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		<-written
		p1, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2, SnapshotSeq: snap})
		if err != nil {
			readErr = err
			return
		}
		// Cursor from the last row of page one; empty page → no second page.
		var p2 []Message
		if len(p1) > 0 {
			p2, err = s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2, SnapshotSeq: snap, BeforeMessageID: p1[len(p1)-1].ID})
			if err != nil {
				readErr = err
				return
			}
		}
		pageA1, pageA2 = p1, p2
		collected = append(append([]Message{}, p1...), p2...)
	}()
	go func() {
		defer wg.Done()
		<-start
		for _, body := range []string{"c1", "c2", "c3", "c4"} {
			if _, _, err := s.AppendMessage(ctx, Message{
				Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
				Blocks: []MessageBlock{textBlock(body)},
			}, ""); err != nil {
				t.Errorf("concurrent AppendMessage(%s): %v", body, err)
				close(written)
				return
			}
		}
		close(written)
	}()
	close(start)
	wg.Wait()

	if readErr != nil {
		t.Fatalf("concurrent paged read: %v", readErr)
	}

	// The union of A's pages is EXACTLY the boundary set {m1,m2,m3,m4}: every
	// boundary row present exactly once, none of B's concurrent writes leaked in,
	// no id on two pages. RED today: the ignored boundary lets seq>4 writes and
	// the full history spill across the pages.
	seen := map[MessageID]int{}
	for _, m := range collected {
		seen[m.ID]++
	}
	for id, n := range seen {
		if !boundary[id] {
			t.Fatalf("id %q appeared in the bounded catch-up but is a seq>%d concurrent write — boundary leaked", id, snap)
		}
		if n != 1 {
			t.Fatalf("id %q appeared %d times across pages, want exactly once (row crossed a page boundary)", id, n)
		}
	}
	if len(seen) != len(boundary) {
		t.Fatalf("collected %d distinct ids, want exactly the %d boundary rows {m1..m4}", len(seen), len(boundary))
	}

	// Each page is strictly newest-first (seq DESC): within every page the
	// insertion rank descends. Under a 4-row boundary, page one is [m4,m3] and
	// page two [m2,m1], so the ranks across the concatenation also descend.
	assertDescending := func(name string, ms []Message) {
		t.Helper()
		for i := 1; i < len(ms); i++ {
			if rank[ms[i-1].ID] <= rank[ms[i].ID] {
				t.Fatalf("%s = %v not in newest-first (seq DESC) order", name, idsOfMsgs(ms))
			}
		}
	}
	assertDescending("page one", pageA1)
	assertDescending("page two", pageA2)
	if len(pageA1) > 0 && len(pageA2) > 0 && rank[pageA1[len(pageA1)-1].ID] <= rank[pageA2[0].ID] {
		t.Fatalf("page two %v is not strictly older than page one %v", idsOfMsgs(pageA2), idsOfMsgs(pageA1))
	}
}

// idsOfMsgs is a local id extractor for failure messages.
func idsOfMsgs(ms []Message) []MessageID {
	out := make([]MessageID, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// TestSearchMessagesSnapshotSeqBoundsResults is the SEA-1333 red-first
// regression for SearchMessages: a bounded search returns only matches with
// seq <= SnapshotSeq. SearchMessages orders by ts_rank then seq DESC, so the
// assertion is on the id-SET membership + count, not strict order. RED until
// SearchMessages honors Page.SnapshotSeq with WHERE seq <= $snap; GREEN after,
// with NO test change.
func TestSearchMessagesSnapshotSeqBoundsResults(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	append1 := func(body string) MessageID {
		t.Helper()
		m, _, err := s.AppendMessage(ctx, Message{
			Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
			Blocks: []MessageBlock{textBlock(body)},
		}, "")
		if err != nil {
			t.Fatalf("AppendMessage(%s): %v", body, err)
		}
		return m.ID
	}

	// Five matching messages before the boundary (seq 1..5); all share "deploy".
	// Capture the first three ids (seq 1..3) — the exact bounded expectation.
	bodies := []string{
		"deploy the frontend",
		"deploy staging now",
		"rollback then deploy again",
		"deploy hotfix to prod",
		"deploy the worker pool",
	}
	want := map[MessageID]bool{}
	for i, body := range bodies {
		id := append1(body)
		if i < 3 {
			want[id] = true
		}
	}
	// snap = seq of the 3rd matching message (a fresh store → seq 3).
	const snap = uint64(3)
	// Two more matches beyond the boundary (seq 6,7) — must be excluded.
	append1("deploy the extra service")
	append1("deploy one more time")

	// Bounded search: exactly the three seq<=3 matches, by id-set (ts_rank then
	// seq DESC ordering means we assert membership + count, not strict order).
	// RED today: the boundary is ignored → all 7 matches come back.
	bounded, err := s.SearchMessages(ctx, author.ID, SearchScope{ChannelID: ch.ID}, "deploy", Page{Limit: 100, SnapshotSeq: snap})
	if err != nil {
		t.Fatalf("SearchMessages(bounded): %v", err)
	}
	seen := map[MessageID]bool{}
	for _, m := range bounded {
		if !want[m.ID] {
			t.Fatalf("bounded match %q is a seq>%d message — boundary leaked", m.ID, snap)
		}
		if seen[m.ID] {
			t.Fatalf("bounded match %q appeared twice", m.ID)
		}
		seen[m.ID] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("bounded search yielded %d distinct matches %v, want exactly the 3 seq<=%d matches", len(seen), idsOfMsgs(bounded), snap)
	}

	// SnapshotSeq:0 means no boundary — all seven matches.
	all, err := s.SearchMessages(ctx, author.ID, SearchScope{ChannelID: ch.ID}, "deploy", Page{Limit: 100, SnapshotSeq: 0})
	if err != nil {
		t.Fatalf("SearchMessages(unbounded): %v", err)
	}
	if len(all) != 7 {
		t.Fatalf("SnapshotSeq:0 search returned %d matches, want all 7 (0 = no boundary)", len(all))
	}
}
