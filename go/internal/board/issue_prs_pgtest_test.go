//go:build pgtest && unix

package board

// The projection keeps each board issue's prs current: on PR hydrate, on issue
// upsert, on state change and across Rehydrate.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

var prT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ingestedPR is a hydrated PR in RigelBuild/compass closing the given issue numbers.
func ingestedPR(number uint32, age, updated time.Duration, closes ...uint64) ingest.IngestedPullRequest {
	refs := make([]forge.IssueRef, 0, len(closes))
	for _, n := range closes {
		refs = append(refs, forge.IssueRef{Repo: "RigelBuild/Compass", Number: n})
	}
	return ingest.IngestedPullRequest{
		PR: &compassv1.PullRequest{
			Forge:      &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
			Repo:       "RigelBuild/compass",
			Number:     number,
			ForgeState: "open",
		},
		CreatedAt:   prT0.Add(age),
		UpdatedAt:   prT0.Add(updated),
		ClosingRefs: refs,
	}
}

func prNumbers(iss *compassv1.Issue) []uint32 {
	out := make([]uint32, 0, len(iss.GetPrs()))
	for _, pr := range iss.GetPrs() {
		out = append(out, pr.GetNumber())
	}
	return out
}

// cachedIssue1 returns the board's cached issue #1, the issue every test here seeds.
func cachedIssue1(t *testing.T, p *IssueProjection) *compassv1.Issue {
	t.Helper()
	const number = 1
	for _, iss := range p.Snapshot() {
		if iss.GetNumber() == number {
			return iss
		}
	}
	t.Fatalf("issue #%d not on the board", number)
	return nil
}

func subscribe(t *testing.T, bus *events.Bus[busPayload]) <-chan events.Stamped[busPayload] {
	t.Helper()
	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub.Live
}

func mustPublishIssue(t *testing.T, p *IssueProjection, number uint32) {
	t.Helper()
	if err := p.PublishIssueUpdate(context.Background(), canonicalIssue(number)); err != nil {
		t.Fatalf("PublishIssueUpdate #%d: %v", number, err)
	}
}

func mustPublishPR(t *testing.T, p *IssueProjection, in ingest.IngestedPullRequest) {
	t.Helper()
	if err := p.PublishPullRequestUpdate(context.Background(), in); err != nil {
		t.Fatalf("PublishPullRequestUpdate: %v", err)
	}
}

func TestPublishPullRequestAttachesAndStateChangeKeepsPrs(t *testing.T) {
	ctx := context.Background()
	p, bus, st := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	live := subscribe(t, bus)

	mustPublishPR(t, p, ingestedPR(10, 0, time.Hour, 1))
	if got := prNumbers(recvIssue(t, live)); !slices.Equal(got, []uint32{10}) {
		t.Fatalf("fanned prs = %v, want [10]", got)
	}

	id := cachedIssue1(t, p).GetId()
	if err := st.SetIssueState(ctx, id, store.IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	committed, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if err := p.RecordAndPublish(context.Background(), committed); err != nil {
		t.Fatalf("RecordAndPublish: %v", err)
	}
	got := recvIssue(t, live)
	if got.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS || !slices.Equal(prNumbers(got), []uint32{10}) {
		t.Fatalf("state change fanned state %v prs %v, want IN_PROGRESS [10]", got.GetState(), prNumbers(got))
	}
	wire, err := p.IssueToProtoWithPrs(ctx, committed)
	if err != nil || !slices.Equal(prNumbers(wire[0]), []uint32{10}) {
		t.Fatalf("IssueToProtoWithPrs = %v, %v; want prs [10]", wire, err)
	}
}

func TestPublishPullRequestRepublishesIssueThatLostRef(t *testing.T) {
	p, bus, _ := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	mustPublishIssue(t, p, 2)
	mustPublishPR(t, p, ingestedPR(10, 0, time.Hour, 1, 2))

	live := subscribe(t, bus)
	mustPublishPR(t, p, ingestedPR(10, 0, 2*time.Hour, 2))
	fanned := map[uint32][]uint32{}
	for range 2 {
		iss := recvIssue(t, live)
		fanned[iss.GetNumber()] = prNumbers(iss)
	}
	if len(fanned[1]) != 0 || !slices.Equal(fanned[2], []uint32{10}) {
		t.Fatalf("fanned = %v, want #1 without the PR and #2 with it", fanned)
	}
}

func TestPullRequestBeforeIssueAttachesLater(t *testing.T) {
	p, _, _ := newIssueBoard(t)
	mustPublishPR(t, p, ingestedPR(10, 0, time.Hour, 1))
	mustPublishIssue(t, p, 1)
	if got := prNumbers(cachedIssue1(t, p)); !slices.Equal(got, []uint32{10}) {
		t.Fatalf("late issue prs = %v, want [10]", got)
	}
}

func TestFallbackMovesWhenExplicitTargetArrives(t *testing.T) {
	ctx := context.Background()
	p, bus, st := newIssueBoard(t)
	agent := mustBoardAgent(t, st)
	mustPublishIssue(t, p, 1) // issue A, the closing ref

	// PR 10 explicitly targets B (#2, not yet on the board) and closes A.
	created := ingestedPR(10, 0, 0, 1)
	row, _, err := pullRequestRow(created)
	if err != nil {
		t.Fatalf("pullRequestRow: %v", err)
	}
	row.UpdatedAt = time.Unix(0, 0)
	target := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "rigelbuild/compass", Number: 2}
	authored := store.AuthoredArtifact{
		Provider: row.Coord.Provider, Host: row.Coord.Host, Repo: "RigelBuild/compass",
		Kind: store.ForgeArtifactKindPullRequest, Number: 10,
		AgentAccountID: agent.agent, OwnerUserID: agent.owner, SessionID: "s", CreatedAtUnixMS: 1,
	}
	if err := st.CreatePullRequestWithLink(ctx, authored, row, &target); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}
	mustPublishPR(t, p, ingestedPR(10, 0, time.Hour, 1))
	if got := prNumbers(cachedIssue1(t, p)); !slices.Equal(got, []uint32{10}) {
		t.Fatalf("A prs before B arrives = %v, want the fallback [10]", got)
	}

	live := subscribe(t, bus)
	mustPublishIssue(t, p, 2)
	fanned := map[uint32][]uint32{}
	for range 2 {
		iss := recvIssue(t, live)
		fanned[iss.GetNumber()] = prNumbers(iss)
	}
	if !slices.Equal(fanned[2], []uint32{10}) || len(fanned[1]) != 0 {
		t.Fatalf("fanned = %v, want B with [10] and A republished without it", fanned)
	}
}

func TestPrsOrderSurvivesRehydrate(t *testing.T) {
	p, _, st := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	mustPublishPR(t, p, ingestedPR(30, 3*time.Hour, 3*time.Hour, 1))
	mustPublishPR(t, p, ingestedPR(10, time.Hour, 4*time.Hour, 1))
	mustPublishPR(t, p, ingestedPR(20, 2*time.Hour, 5*time.Hour, 1))

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	fresh := NewIssueProjection(bus, st)
	if err := fresh.Rehydrate(context.Background()); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if got := prNumbers(cachedIssue1(t, fresh)); !slices.Equal(got, []uint32{10, 20, 30}) {
		t.Fatalf("rehydrated prs = %v, want forge-created order [10 20 30]", got)
	}
}

type boardAgent struct{ agent, owner store.AccountID }

func mustBoardAgent(t *testing.T, st *store.Store) boardAgent {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, store.NewUser{Handle: "prowner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a, err := st.CreateAgent(ctx, u.ID, store.NewAgent{Handle: "pragent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return boardAgent{agent: a.ID, owner: u.ID}
}

// TestTransitionOnUncachedIssueLoadsPrs pins the cache-miss path: a transition
// on an issue not yet in the cache fans and caches its stored prs.
func TestTransitionOnUncachedIssueLoadsPrs(t *testing.T) {
	ctx := context.Background()
	p, bus, st := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	mustPublishPR(t, p, ingestedPR(10, 0, time.Hour, 1))
	id := cachedIssue1(t, p).GetId()

	cold := NewIssueProjection(bus, st)
	if err := st.SetIssueState(ctx, id, store.IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	committed, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if err := cold.RecordAndPublish(ctx, committed); err != nil {
		t.Fatalf("RecordAndPublish: %v", err)
	}
	wire, err := cold.CommittedIssue(ctx, committed)
	if err != nil || !slices.Equal(prNumbers(wire), []uint32{10}) {
		t.Fatalf("CommittedIssue prs = %v, %v; want [10]", prNumbers(wire), err)
	}
}

// TestTransitionRecordsRowNewerThanCaller pins the re-read: a forge update that
// commits after the transition's read is not rolled back in the cache.
func TestTransitionRecordsRowNewerThanCaller(t *testing.T) {
	ctx := context.Background()
	p, _, st := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	id := cachedIssue1(t, p).GetId()
	if err := st.SetIssueState(ctx, id, store.IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	stale, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	newer := canonicalIssue(1)
	newer.Title = "renamed"
	if err := p.PublishIssueUpdate(ctx, newer); err != nil {
		t.Fatalf("PublishIssueUpdate: %v", err)
	}
	if err := p.RecordAndPublish(ctx, stale); err != nil {
		t.Fatalf("RecordAndPublish: %v", err)
	}
	if got := cachedIssue1(t, p); got.GetTitle() != "renamed" || got.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
		t.Fatalf("cached title %q state %v, want renamed IN_PROGRESS", got.GetTitle(), got.GetState())
	}
}

// TestTransitionPublishesWhenReadBackFails pins that a durable transition still
// reaches the board when the re-read fails, and the error is reported.
func TestTransitionPublishesWhenReadBackFails(t *testing.T) {
	p, _, st := newIssueBoard(t)
	mustPublishIssue(t, p, 1)
	id := cachedIssue1(t, p).GetId()
	committed, err := st.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	committed.State = store.IssueStateInProgress
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.RecordAndPublish(ctx, committed); err == nil {
		t.Fatal("RecordAndPublish error = nil, want the failed re-read")
	}
	if got := cachedIssue1(t, p).GetState(); got != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
		t.Fatalf("cached state = %v, want IN_PROGRESS", got)
	}
}
