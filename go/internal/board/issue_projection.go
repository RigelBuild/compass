//go:build unix

package board

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/proto"
)

// IssueProjection is the Server-authoritative board issue projection: the
// durable canonical issue state (DL-019, in Postgres via the store) fronted by
// an in-memory map for snapshot + a live issue=16 fan-out onto SubscribeEvents
// (DL-020, the bus is cache/fan-out, never the store of record). Sibling to the
// agent-session Projection; keyed by Compass issue id, not session.
//
// It is the ONLY place in the tree that maps between store.Issue (store-native,
// no proto) and *compassv1.Issue (wire): it imports BOTH store and compassv1,
// where the store package imports no generated code. The two mapping funcs
// issueToProto / protoToForgeFields are the whole of that edge.
type IssueProjection struct {
	bus   *events.Bus[busPayload]
	store *store.Store

	// writeMu orders each store read with the cache write it feeds, so a slower
	// writer never caches an older row or prs list over a newer one. Never held
	// under mu; taken before it.
	writeMu sync.Mutex
	mu      sync.RWMutex
	issues  map[string]*compassv1.Issue // id -> latest canonical issue (in-memory cache)
	byCoord map[store.ForgeCoord]string // normalized forge coordinate -> issue id
}

// NewIssueProjection constructs an empty board over the SubscribeEvents bus it
// fans issue upserts onto and the store it reads/writes durable issue state
// through. Rehydrate seeds the map from the store before serving.
func NewIssueProjection(bus *events.Bus[busPayload], st *store.Store) *IssueProjection {
	return &IssueProjection{
		bus:     bus,
		store:   st,
		issues:  make(map[string]*compassv1.Issue),
		byCoord: make(map[store.ForgeCoord]string),
	}
}

// PublishIssueUpdate is the ingestion sink (part 3's issueSink contract):
// (1) map proto -> store.IssueForgeFields, (2) UpsertIssueForgeFields (durable
// commit; returns the stable id), (3) GetIssue(id) to read back the FULL row
// (forge fields just written + the store-owned state/machinery, so the cached +
// fanned Issue reflects committed truth incl. a prior human-set state), (4) map
// store.Issue -> *compassv1.Issue with its prs, (5) record in the map + Publish
// the issue=16 variant, atomic under mu, then (6) republish the closing-ref
// issues of PRs that now attach here. Returns error on any store failure.
//
// Lock discipline: the upsert runs unlocked. writeMu, taken before mu, spans the
// read-back through the record, so every projection writer is serialized and a
// write committed meanwhile is either read here or cached after this. mu covers
// only the map-record and the non-blocking bus.Publish, never a DB round-trip.
func (p *IssueProjection) PublishIssueUpdate(ctx context.Context, issue *compassv1.Issue) error {
	// (1)-(2) durable commit at the forge coordinate; the returned id is stable
	// across re-polls (the coordinate is the idempotency key).
	id, err := p.store.UpsertIssueForgeFields(ctx, protoToForgeFields(issue))
	if err != nil {
		return fmt.Errorf("board: upsert issue forge fields: %w", err)
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	// (3) read back the FULL committed row: the forge fields just written PLUS
	// the store-owned state/machinery, so the fanned Issue reflects committed
	// truth — including a prior human-set lifecycle state the forge re-poll did
	// not clobber (the 3a no-clobber property, made visible on the wire).
	committed, err := p.store.GetIssue(ctx, id)
	if err != nil {
		return fmt.Errorf("board: read back committed issue: %w", err)
	}
	// (4) map committed store.Issue -> wire Issue with its prs, OUTSIDE the lock.
	wires, err := p.IssueToProtoWithPrs(ctx, committed)
	if err != nil {
		return err
	}
	wire := wires[0]
	coord := storeIssueCoord(committed)

	// (5) record + fan out atomically under the write lock.
	p.mu.Lock()
	p.record(wire, coord)
	p.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_Issue{Issue: wire},
	})
	p.mu.Unlock()

	// PRs that fell back to closing refs while this issue was off the board now
	// attach here; republish those closing-ref issues without them.
	fallback, err := p.store.FallbackIssuesForTarget(ctx, coord)
	if err != nil {
		return fmt.Errorf("board: fallback issues for target: %w", err)
	}
	return p.republishPrsLocked(ctx, fallback)
}

// RecordAndPublish records and fans out an issue the transition executor
// (server/board.go) has already committed; it never writes the store. Under
// writeMu it re-reads the row, since a forge upsert may have landed after the
// caller's read, and loads prs for an uncached issue. Those reads are
// best-effort: on failure it still publishes committed, with cached or no prs,
// and returns the error for the caller to log, so a durable transition always
// reaches the board.
func (p *IssueProjection) RecordAndPublish(ctx context.Context, committed store.Issue) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	var readErr error
	if p.store != nil {
		fresh, err := p.store.GetIssue(ctx, committed.ID)
		if err == nil {
			committed = fresh
		} else {
			readErr = fmt.Errorf("board: read back transitioned issue: %w", err)
		}
	}
	wire := issueToProto(committed)

	// A state change never touches links, so cached prs carry over.
	p.mu.RLock()
	prev, cached := p.issues[wire.GetId()]
	p.mu.RUnlock()
	if cached {
		wire.Prs = prev.GetPrs()
	} else if p.store != nil {
		coord := storeIssueCoord(committed)
		prs, err := p.loadPrs(ctx, []store.ForgeCoord{coord})
		if err == nil {
			wire.Prs = prs[coord]
		} else {
			readErr = errors.Join(readErr, err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.record(wire, storeIssueCoord(committed))
	p.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_Issue{Issue: wire},
	})
	return readErr
}

// Snapshot returns every issue on the board (all states incl. ARCHIVED — the
// board's Done view shows archived; v1 carries upserts only, no removal), sorted
// by id for determinism. Each entry is a fresh clone the caller owns. This is
// the surface part 4b's ListIssues handler reads.
func (p *IssueProjection) Snapshot() []*compassv1.Issue {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*compassv1.Issue, 0, len(p.issues))
	for _, iss := range p.issues {
		out = append(out, cloneIssue(iss))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out
}

// Rehydrate loads the durable board from Postgres into the in-memory map at
// startup (DL-019: the projection is a read-through cache, the store is truth).
// Called once by serve.go before serving. Does NOT publish (nothing is
// subscribed yet at boot); it seeds the map so the first Snapshot/fan-out is
// complete.
func (p *IssueProjection) Rehydrate(ctx context.Context) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	rows, err := p.store.ListIssues(ctx)
	if err != nil {
		return fmt.Errorf("board: rehydrate issues: %w", err)
	}
	wires, err := p.IssueToProtoWithPrs(ctx, rows...)
	if err != nil {
		return fmt.Errorf("board: rehydrate issues: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, si := range rows {
		p.record(wires[i], storeIssueCoord(si))
	}
	return nil
}

// issueToProto maps the store-native issue to the canonical wire Issue. store
// enums -> proto enums by value (they mirror: IssueState 0..8, ForgeProvider
// 0..3); Forge is rebuilt as &compassv1.ForgeRef{Provider, Host}; Labels copied;
// empty->nil per the module contract; tracker/prs left nil (prs are loaded by
// record caches wire under its id and coordinate. Callers hold p.mu.
func (p *IssueProjection) record(wire *compassv1.Issue, coord store.ForgeCoord) {
	p.issues[wire.GetId()] = wire
	p.byCoord[coord] = wire.GetId()
}

// IssueToProtoWithPrs). AgentAttribution is set only for a Compass-authored issue
// (a non-empty agent_handle); a human author leaves it unset.
func issueToProto(si store.Issue) *compassv1.Issue {
	out := &compassv1.Issue{
		Id: si.ID,
		Forge: &compassv1.ForgeRef{
			Provider: compassv1.ForgeProvider(si.ForgeProvider),
			Host:     si.ForgeHost,
		},
		Repo:         si.Repo,
		Number:       si.Number,
		Title:        si.Title,
		Body:         si.Body,
		ForgeState:   si.ForgeState,
		Url:          si.URL,
		ForgeAccount: si.ForgeAccount,
		State:        compassv1.IssueState(si.State),
		Priority:     si.Priority,
		Assignee:     si.Assignee,
		Summary:      si.Summary,
		Branch:       si.Branch,
	}
	if len(si.Labels) > 0 {
		out.Labels = append([]string(nil), si.Labels...)
	}
	if si.AgentHandle != "" {
		out.Agent = &compassv1.AgentAttribution{AgentHandle: si.AgentHandle}
	}
	return out
}

// protoToForgeFields maps an ingested canonical Issue to the store's forge-only
// upsert input (the inverse used by PublishIssueUpdate). Reads proto via GetX().
// Pulls ONLY forge fields + coordinate + agent_handle — never state/machinery
// (those are store-owned; IssueForgeFields has no such field, the compile-time
// guarantee that the forge-only upsert cannot clobber a human-set state).
func protoToForgeFields(p *compassv1.Issue) store.IssueForgeFields {
	out := store.IssueForgeFields{
		ForgeProvider: store.ForgeProvider(p.GetForge().GetProvider()),
		ForgeHost:     p.GetForge().GetHost(),
		Repo:          p.GetRepo(),
		Number:        p.GetNumber(),
		Title:         p.GetTitle(),
		Body:          p.GetBody(),
		ForgeState:    p.GetForgeState(),
		URL:           p.GetUrl(),
		ForgeAccount:  p.GetForgeAccount(),
		AgentHandle:   p.GetAgent().GetAgentHandle(),
	}
	if len(p.GetLabels()) > 0 {
		out.Labels = append([]string(nil), p.GetLabels()...)
	}
	// The OQ-6(a) recency guard (RIG-2883 T4a): carry the forge's last-updated
	// timestamp so the store's conditional upsert can skip a stale re-sink. An
	// unset proto field leaves ForgeUpdatedAt zero, which stores NULL and keeps
	// the write additive.
	if ts := p.GetUpdatedAt(); ts != nil {
		out.ForgeUpdatedAt = ts.AsTime()
	}
	return out
}

// cloneIssue returns a deep copy for Snapshot: a caller mutating a returned
// Issue (or any sub-message / slice) must not touch the cached one. proto.Clone
// tracks the message definition automatically, so the copy stays complete as
// the Issue proto gains fields — a manual field-by-field copy would silently
// drop any field added later (e.g. Tracker/Prs, once their producing slices
// land).
func cloneIssue(in *compassv1.Issue) *compassv1.Issue {
	out, _ := proto.Clone(in).(*compassv1.Issue)
	return out
}
