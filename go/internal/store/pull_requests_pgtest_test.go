//go:build pgtest

package store

// PR-to-issue link store contracts: explicit links outrank closing references,
// the forge_updated_at guard, read-time fallback, ordering, RLS and repo case.
// context.Background is the test root (the pgtest-suite convention).

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

var prBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ghCoord(repo string, n uint64) ForgeCoord {
	return ForgeCoord{Provider: ForgeProviderGitHub, Host: "github.com", Repo: repo, Number: n}
}

func prRow(repo string, n uint64, state string, created, updated time.Time) PullRequestRow {
	return PullRequestRow{
		Coord:     ghCoord(repo, n),
		State:     state,
		CreatedAt: created,
		UpdatedAt: updated,
		PR:        []byte(`{"state":"` + state + `"}`),
	}
}

func seedBoardIssue(t *testing.T, ctx context.Context, s *Store, c ForgeCoord) {
	t.Helper()
	if _, err := s.UpsertIssueForgeFields(ctx, IssueForgeFields{
		ForgeProvider: c.Provider, ForgeHost: c.Host, Repo: c.Repo,
		Number: uint32(c.Number), Title: "issue",
	}); err != nil {
		t.Fatalf("seed board issue %v: %v", c, err)
	}
}

func authoredPR(agent, owner AccountID, pr PullRequestRow) AuthoredArtifact {
	return AuthoredArtifact{
		Provider: pr.Coord.Provider, Host: pr.Coord.Host, Repo: pr.Coord.Repo,
		Kind: ForgeArtifactKindPullRequest, Number: pr.Coord.Number,
		AgentAccountID: agent, OwnerUserID: owner,
		SessionID: "sess", CreatedAtUnixMS: prBase.UnixMilli(),
	}
}

// prsFor returns the PR numbers attached to issue, in read order.
func prsFor(t *testing.T, ctx context.Context, s *Store, issue ForgeCoord) []uint64 {
	t.Helper()
	got, err := s.PullRequestsForIssues(ctx, []ForgeCoord{issue})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	out := make([]uint64, 0, len(got[issue]))
	for _, pr := range got[issue] {
		out = append(out, pr.Coord.Number)
	}
	return out
}

func mustUpsertPR(t *testing.T, ctx context.Context, s *Store, pr PullRequestRow, refs ...ForgeCoord) []ForgeCoord {
	t.Helper()
	affected, err := s.UpsertPullRequest(ctx, pr, refs)
	if err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	return affected
}

func TestPullRequestExplicitLinkSurvivesUpsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl1")
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	pr := prRow("a/b", 10, "open", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, pr), pr, &issue); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)))

	if got := prsFor(t, ctx, s, issue); !slices.Equal(got, []uint64{10}) {
		t.Fatalf("explicit link lost after upsert: got %v", got)
	}
}

func TestPullRequestExplicitTargetSurvivesClosingRefRemoval(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl2")
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	pr := prRow("a/b", 10, "open", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, pr), pr, &issue); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)), issue)
	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(2*time.Hour)))

	if got := prsFor(t, ctx, s, issue); !slices.Equal(got, []uint64{10}) {
		t.Fatalf("explicit target unlinked by closing-ref removal: got %v", got)
	}
}

func TestPullRequestUnresolvableExplicitLinkFallsBack(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl3")
	offBoard := ghCoord("a/b", 99)
	closing := ghCoord("a/b", 2)
	seedBoardIssue(t, ctx, s, closing)

	pr := prRow("a/b", 10, "open", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, pr), pr, &offBoard); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)), closing)

	if got := prsFor(t, ctx, s, closing); !slices.Equal(got, []uint64{10}) {
		t.Fatalf("off-board explicit link did not fall back: got %v", got)
	}
	fb, err := s.FallbackIssuesForTarget(ctx, offBoard)
	if err != nil {
		t.Fatalf("FallbackIssuesForTarget: %v", err)
	}
	if !slices.Equal(fb, []ForgeCoord{closing}) {
		t.Fatalf("FallbackIssuesForTarget = %v, want [%v]", fb, closing)
	}

	// Once the explicit target is on the board, the closing reference stops attaching.
	seedBoardIssue(t, ctx, s, offBoard)
	if got := prsFor(t, ctx, s, closing); len(got) != 0 {
		t.Fatalf("closing ref attached despite a resolvable explicit link: got %v", got)
	}
	if got := prsFor(t, ctx, s, offBoard); !slices.Equal(got, []uint64{10}) {
		t.Fatalf("resolvable explicit link not attached: got %v", got)
	}
}

func TestPullRequestRemovedClosingRefsAreAffected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	i2, i3, i4 := ghCoord("a/b", 2), ghCoord("a/b", 3), ghCoord("a/b", 4)

	first := mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)), i2, i3)
	if !sameCoords(first, []ForgeCoord{i2, i3}) {
		t.Fatalf("first affected = %v, want [%v %v]", first, i2, i3)
	}
	second := mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(2*time.Hour)), i3, i4)
	if !sameCoords(second, []ForgeCoord{i2, i3, i4}) {
		t.Fatalf("second affected = %v, want old+new [%v %v %v]", second, i2, i3, i4)
	}
	seedBoardIssue(t, ctx, s, i2)
	if got := prsFor(t, ctx, s, i2); len(got) != 0 {
		t.Fatalf("removed closing ref still linked: %v", got)
	}
}

func TestPullRequestOlderUpdateSkipped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "merged", prBase, prBase.Add(2*time.Hour)), issue)
	affected := mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)))
	if len(affected) != 0 {
		t.Fatalf("older update reported affected %v", affected)
	}
	got, err := s.PullRequestsForIssues(ctx, []ForgeCoord{issue})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	if prs := got[issue]; len(prs) != 1 || prs[0].State != "merged" {
		t.Fatalf("older update overwrote the row or its links: %+v", prs)
	}
}

func TestPullRequestCreateAfterHydrateKeepsHydratedRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl6")
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "hydrated", prBase, prBase.Add(time.Hour)))
	created := prRow("a/b", 10, "created", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, created), created, &issue); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	got, err := s.PullRequestsForIssues(ctx, []ForgeCoord{issue})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	prs := got[issue]
	if len(prs) != 1 || prs[0].State != "hydrated" || !prs[0].UpdatedAt.Equal(prBase.Add(time.Hour)) {
		t.Fatalf("create clobbered the hydrated row: %+v", prs)
	}
	if string(prs[0].PR) != `{"state": "hydrated"}` {
		t.Fatalf("PR JSON = %s, want the hydrated body", prs[0].PR)
	}
}

func TestPullRequestOrderFollowsForgeCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	for _, c := range []struct {
		n   uint64
		age time.Duration
	}{{30, 3 * time.Hour}, {10, time.Hour}, {20, 2 * time.Hour}} {
		mustUpsertPR(t, ctx, s, prRow("a/b", c.n, "open", prBase.Add(c.age), prBase.Add(c.age)), issue)
	}
	if got := prsFor(t, ctx, s, issue); !slices.Equal(got, []uint64{10, 20, 30}) {
		t.Fatalf("order = %v, want by forge_created_at [10 20 30]", got)
	}
}

func TestPullRequestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl8")
	issue := ghCoord("a/b", 1)
	seedBoardIssue(t, ctx, s, issue)

	created := prRow("a/b", 10, "created", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, created), created, &issue); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustUpsertPR(t, ctx, s, prRow("a/b", 10, "open", prBase, prBase.Add(time.Hour)), issue)
	got, err := s.PullRequestsForIssues(ctx, []ForgeCoord{issue})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	if prs := got[issue]; len(prs) != 1 || prs[0].State != "open" {
		t.Fatalf("create and ingest did not share one tenant row: %+v", prs)
	}

	ctxB := WithTenant(context.Background(), seedTenant(t, s, "prl-tenant-b"))
	if got := prsFor(t, ctxB, s, issue); len(got) != 0 {
		t.Fatalf("foreign tenant read PRs %v", got)
	}
	if fb, err := s.FallbackIssuesForTarget(ctxB, issue); err != nil || len(fb) != 0 {
		t.Fatalf("foreign tenant FallbackIssuesForTarget = %v, %v", fb, err)
	}
}

func TestPullRequestMixedCaseReposJoin(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl9")
	seedBoardIssue(t, ctx, s, ghCoord("owner/repo", 1))

	issue := ghCoord("Owner/Repo", 1)
	pr := prRow("OWNER/repo", 10, "open", prBase, time.Unix(0, 0))
	if err := s.CreatePullRequestWithLink(ctx, authoredPR(agent, owner, pr), pr, &issue); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustUpsertPR(t, ctx, s, prRow("owner/REPO", 10, "open", prBase, prBase.Add(time.Hour)), ghCoord("Owner/repo", 1))

	got, err := s.PullRequestsForIssues(ctx, []ForgeCoord{ghCoord("OWNER/REPO", 1)})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	prs := got[ghCoord("owner/repo", 1)]
	if len(prs) != 1 || prs[0].Coord != ghCoord("owner/repo", 10) {
		t.Fatalf("mixed-case repos did not join to one lowercased PR: %+v", got)
	}
	// The ownership row keeps the webhook's casing for the self-origin lookup.
	if _, err := s.AuthoredArtifactByCoordinate(ctx, ForgeProviderGitHub, "github.com", "OWNER/repo", ForgeArtifactKindPullRequest, 10); err != nil {
		t.Fatalf("authored row lost its repo casing: %v", err)
	}
}

func TestCreatePullRequestRejectsMismatchedArtifact(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "prl10")
	pr := prRow("a/b", 10, "open", prBase, time.Unix(0, 0))
	a := authoredPR(agent, owner, pr)
	a.Number = 11
	if err := s.CreatePullRequestWithLink(ctx, a, pr, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("mismatched artifact err = %v, want ErrInvalidArgument", err)
	}
	if got := prsFor(t, ctx, s, ghCoord("a/b", 1)); len(got) != 0 {
		t.Fatalf("rejected create wrote rows: %v", got)
	}
}

func sameCoords(got, want []ForgeCoord) bool {
	return len(got) == len(want) && !slices.ContainsFunc(want, func(c ForgeCoord) bool {
		return !slices.Contains(got, c)
	})
}
