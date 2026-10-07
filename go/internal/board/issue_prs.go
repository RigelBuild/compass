//go:build unix

package board

import (
	"context"
	"fmt"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

// PublishPullRequestUpdate stores a hydrated PR and its closing references, then
// republishes every board issue linked to it before or after the write with a
// fresh prs list. Only Prs changes on the cached issue, so a concurrent state
// change is never rolled back.
func (p *IssueProjection) PublishPullRequestUpdate(ctx context.Context, in ingest.IngestedPullRequest) error {
	row, refs, err := pullRequestRow(in)
	if err != nil {
		return err
	}
	affected, err := p.store.UpsertPullRequest(ctx, row, refs)
	if err != nil {
		return fmt.Errorf("board: upsert pull request: %w", err)
	}
	return p.republishPrs(ctx, affected)
}

// pullRequestRow maps an ingested PR to the store row plus its closing refs on the PR's forge.
func pullRequestRow(in ingest.IngestedPullRequest) (store.PullRequestRow, []store.ForgeCoord, error) {
	pr := in.PR
	body, err := protojson.Marshal(pr)
	if err != nil {
		return store.PullRequestRow{}, nil, fmt.Errorf("board: marshal pull request: %w", err)
	}
	provider := store.ForgeProvider(pr.GetForge().GetProvider())
	host := pr.GetForge().GetHost()
	row := store.PullRequestRow{
		Coord:     store.ForgeCoord{Provider: provider, Host: host, Repo: pr.GetRepo(), Number: uint64(pr.GetNumber())},
		State:     pr.GetForgeState(),
		CreatedAt: in.CreatedAt,
		UpdatedAt: in.UpdatedAt,
		PR:        body,
	}
	refs := make([]store.ForgeCoord, 0, len(in.ClosingRefs))
	for _, r := range in.ClosingRefs {
		refs = append(refs, store.ForgeCoord{Provider: provider, Host: host, Repo: r.Repo, Number: r.Number})
	}
	return row, refs, nil
}

// storeIssueCoord is issueCoord for a store row.
func storeIssueCoord(si store.Issue) store.ForgeCoord {
	return store.ForgeCoord{Provider: si.ForgeProvider, Host: si.ForgeHost, Repo: si.Repo, Number: uint64(si.Number)}.Normalized()
}

// loadPrs reads the ordered wire prs for each coordinate in one query.
func (p *IssueProjection) loadPrs(ctx context.Context, coords []store.ForgeCoord) (map[store.ForgeCoord][]*compassv1.PullRequest, error) {
	rows, err := p.store.PullRequestsForIssues(ctx, coords)
	if err != nil {
		return nil, fmt.Errorf("board: load pull requests: %w", err)
	}
	out := make(map[store.ForgeCoord][]*compassv1.PullRequest, len(rows))
	for c, prs := range rows {
		wire := make([]*compassv1.PullRequest, 0, len(prs))
		for _, r := range prs {
			pr := &compassv1.PullRequest{}
			if err := protojson.Unmarshal(r.PR, pr); err != nil {
				return nil, fmt.Errorf("board: decode stored pull request %v: %w", r.Coord, err)
			}
			wire = append(wire, pr)
		}
		out[c] = wire
	}
	return out, nil
}

// republishPrs reloads prs for the given issues and publishes each one already
// on the board. Issues not on the board are skipped; their links wait.
func (p *IssueProjection) republishPrs(ctx context.Context, coords []store.ForgeCoord) error {
	if len(coords) == 0 {
		return nil
	}
	prs, err := p.loadPrs(ctx, coords)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range coords {
		id, ok := p.byCoord[c.Normalized()]
		if !ok {
			continue
		}
		next := cloneIssue(p.issues[id])
		next.Prs = prs[c.Normalized()]
		p.issues[id] = next
		p.bus.Publish(&compassv1.SubscribeEventsResponse{
			Payload: &compassv1.SubscribeEventsResponse_Issue{Issue: next},
		})
	}
	return nil
}

// CommittedIssue maps a committed row to the wire Issue, taking prs from the
// cache; a row not yet cached loads them from the store.
func (p *IssueProjection) CommittedIssue(ctx context.Context, si store.Issue) (*compassv1.Issue, error) {
	if p != nil {
		p.mu.RLock()
		prev, ok := p.issues[si.ID]
		var prs []*compassv1.PullRequest
		if ok {
			prs = cloneIssue(prev).GetPrs()
		}
		p.mu.RUnlock()
		if ok {
			wire := issueToProto(si)
			wire.Prs = prs
			return wire, nil
		}
	}
	wires, err := p.IssueToProtoWithPrs(ctx, si)
	if err != nil {
		return nil, err
	}
	return wires[0], nil
}

// IssueToProtoWithPrs maps committed rows to wire Issues with their prs loaded,
// for responses built outside the cache (SetIssueState, SearchIssues). A nil
// projection, as in tests that run without a board, maps rows without prs.
func (p *IssueProjection) IssueToProtoWithPrs(ctx context.Context, rows ...store.Issue) ([]*compassv1.Issue, error) {
	if p == nil {
		out := make([]*compassv1.Issue, 0, len(rows))
		for _, si := range rows {
			out = append(out, issueToProto(si))
		}
		return out, nil
	}
	coords := make([]store.ForgeCoord, 0, len(rows))
	for _, si := range rows {
		coords = append(coords, storeIssueCoord(si))
	}
	prs, err := p.loadPrs(ctx, coords)
	if err != nil {
		return nil, err
	}
	out := make([]*compassv1.Issue, 0, len(rows))
	for i, si := range rows {
		wire := issueToProto(si)
		wire.Prs = prs[coords[i]]
		out = append(out, wire)
	}
	return out, nil
}
