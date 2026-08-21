//go:build unix

package board

import (
	"context"
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

	mu     sync.RWMutex
	issues map[string]*compassv1.Issue // id -> latest canonical issue (in-memory cache)
}

// NewIssueProjection constructs an empty board over the SubscribeEvents bus it
// fans issue upserts onto and the store it reads/writes durable issue state
// through. Rehydrate seeds the map from the store before serving.
func NewIssueProjection(bus *events.Bus[busPayload], st *store.Store) *IssueProjection {
	return &IssueProjection{
		bus:    bus,
		store:  st,
		issues: make(map[string]*compassv1.Issue),
	}
}

// PublishIssueUpdate is the ingestion sink (part 3's issueSink contract):
// (1) map proto -> store.IssueForgeFields, (2) UpsertIssueForgeFields (durable
// commit; returns the stable id), (3) GetIssue(id) to read back the FULL row
// (forge fields just written + the store-owned state/machinery, so the cached +
// fanned Issue reflects committed truth incl. a prior human-set state), (4) map
// store.Issue -> *compassv1.Issue, (5) record in the map + Publish the issue=16
// variant, atomic under mu. Returns error on any store failure (part 3 stops on it).
//
// Lock discipline (load-bearing, mirrors PublishSessionStatus): the DURABLE PG
// commit (UpsertIssueForgeFields + the GetIssue read-back) happens BEFORE taking
// p.mu — the DB round-trip must NOT be held under the projection mutex, which
// would serialize all ingestion on the database. Only the map-record + the
// bus.Publish run under the lock, where Publish is non-blocking (a per-subscriber
// select/default under the bus's own distinct mutex), so record and fan-out are
// atomic to any Snapshot reader and deadlock-free.
//
// This assumes single-threaded ingestion per coordinate (the part-3 poller):
// the lock is released between the DB commit and the map-record, so two
// concurrent publishes of the SAME coordinate could record in commit order or
// in lock order. Harmless while one poller owns each coordinate; a
// per-coordinate guard would be needed if concurrent same-coordinate ingestion
// is ever introduced.
func (p *IssueProjection) PublishIssueUpdate(ctx context.Context, issue *compassv1.Issue) error {
	// (1)-(2) durable commit at the forge coordinate; the returned id is stable
	// across re-polls (the coordinate is the idempotency key).
	id, err := p.store.UpsertIssueForgeFields(ctx, protoToForgeFields(issue))
	if err != nil {
		return fmt.Errorf("board: upsert issue forge fields: %w", err)
	}
	// (3) read back the FULL committed row: the forge fields just written PLUS
	// the store-owned state/machinery, so the fanned Issue reflects committed
	// truth — including a prior human-set lifecycle state the forge re-poll did
	// not clobber (the 3a no-clobber property, made visible on the wire).
	committed, err := p.store.GetIssue(ctx, id)
	if err != nil {
		return fmt.Errorf("board: read back committed issue: %w", err)
	}
	// (4) map committed store.Issue -> wire Issue OUTSIDE the lock.
	wire := issueToProto(committed)

	// (5) record + fan out atomically under the write lock.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issues[wire.GetId()] = wire
	p.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_Issue{Issue: wire},
	})
	return nil
}

// RecordAndPublish is the STATE-ONLY record+publish the write-path transition
// executor (server/board.go, agent primary lifecycle T3-a) drives after it has
// already committed the new canonical state to Postgres AND read the full row
// back. It is the step-(5) tail of PublishIssueUpdate WITHOUT the store
// upsert/read-back: the executor owns the durable commit (a forge-only upsert
// would demand forge fields and could not carry the state column), so this only
// maps the committed row to the wire Issue and records+fans it — never a store
// write. committed is the executor's read-back of committed truth.
//
// Lock discipline mirrors PublishIssueUpdate exactly: the map -> proto mapping
// runs OUTSIDE p.mu (no DB round-trip is held under the projection mutex; here
// there is no DB work at all), and only the map-record + the non-blocking
// bus.Publish run under the lock, so record and fan-out are atomic to any
// Snapshot reader and deadlock-free. Per-coordinate serialization is the
// executor's job (its per-issue transition lock), not this projection's.
func (p *IssueProjection) RecordAndPublish(committed store.Issue) {
	// Map committed store.Issue -> wire Issue OUTSIDE the lock.
	wire := issueToProto(committed)

	// Record + fan out atomically under the write lock.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issues[wire.GetId()] = wire
	p.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_Issue{Issue: wire},
	})
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
	rows, err := p.store.ListIssues(ctx)
	if err != nil {
		return fmt.Errorf("board: rehydrate issues: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, si := range rows {
		proto := issueToProto(si)
		p.issues[proto.GetId()] = proto
	}
	return nil
}

// IssueToProto maps a store-native issue to the canonical wire Issue. It is the
// exported form of issueToProto for the write-path transition executor
// (server/board.go), which reads a committed row back through the store and must
// return it on the SetIssueState response wire — the store<->wire mapping stays
// owned by this package (the ONLY place the two types meet), so the executor
// borrows it rather than re-implementing the edge.
func IssueToProto(si store.Issue) *compassv1.Issue {
	return issueToProto(si)
}

// issueToProto maps the store-native issue to the canonical wire Issue. store
// enums -> proto enums by value (they mirror: IssueState 0..8, ForgeProvider
// 0..3); Forge is rebuilt as &compassv1.ForgeRef{Provider, Host}; Labels copied;
// empty->nil per the module contract; tracker/prs left nil (their producing
// slices own them). AgentAttribution is set only for a Compass-authored issue
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
