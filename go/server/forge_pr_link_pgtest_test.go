//go:build pgtest && unix

package server

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestForgeCreatePullRequestShowsOnLinkedIssue pins the create path end to end
// on a real store: the board issue carries the new PR at once, with the forge's
// creation time stored, and a later hydrate replaces the create's row.
func TestForgeCreatePullRequestShowsOnLinkedIssue(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	u, err := st.CreateUser(ctx, store.NewUser{Handle: "prlinkowner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, u.ID, store.NewAgent{Handle: "prlinkagent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	author := forge.NewFakeProvider("gh-author")
	reg := newForgeProviderRegistry()
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: "github.com"}, author, forge.NewFakeProvider("gh-reviewer"), true); err != nil {
		t.Fatalf("register: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	svc := newForgeService(st, brd, reg)
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/prlink", Enabled: true}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}

	issue := &compassv1.Issue{
		Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
		Repo:  "Owner/PRLink", Number: 4, Title: "t", ForgeState: "open",
	}
	if err := brd.PublishIssueUpdate(ctx, issue); err != nil {
		t.Fatalf("PublishIssueUpdate: %v", err)
	}
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	author.CreatePRResult = forge.PullRequest{Number: 31, State: "open", CreatedAt: created}

	res, err := svc.ExecuteForgeCallAsAccount(ctx, agent.ID, "sess", &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_CreatePullRequest{CreatePullRequest: &compassv1internal.CreatePullRequestRequest{
			Repo: "Owner/PRLink", Title: "t", Body: "b", HeadRef: "f", BaseRef: "main",
			Issue: &compassv1internal.PullRequestIssueLink{Number: 4},
		}},
	})
	if err != nil || res.GetError() != nil {
		t.Fatalf("create: err=%v in-band=%v", err, res.GetError())
	}

	var got *compassv1.Issue
	for _, iss := range brd.Snapshot() {
		if iss.GetNumber() == 4 {
			got = iss
		}
	}
	if got == nil || len(got.GetPrs()) != 1 || got.GetPrs()[0].GetNumber() != 31 {
		t.Fatalf("board issue prs = %v, want PR #31", got.GetPrs())
	}
	at, ok, err := st.PullRequestUpdatedAt(ctx, store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/prlink", Number: 31})
	if err != nil || !ok || !at.Equal(time.Unix(0, 0)) {
		t.Fatalf("stored forge_updated_at = %v ok=%v err=%v, want the epoch", at, ok, err)
	}
	rows, err := st.PullRequestsForIssues(ctx, []store.ForgeCoord{{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/prlink", Number: 4}})
	if err != nil {
		t.Fatalf("PullRequestsForIssues: %v", err)
	}
	for _, prs := range rows {
		if len(prs) != 1 || !prs[0].CreatedAt.Equal(created) {
			t.Fatalf("stored rows = %+v, want one with forge_created_at %v", prs, created)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("link rows for issue = %d, want 1", len(rows))
	}

	hydrated := &compassv1.PullRequest{
		Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
		Repo:  "owner/prlink", Number: 31, Title: "hydrated", ForgeState: "open",
	}
	if err := brd.PublishPullRequestUpdate(ctx, ingest.IngestedPullRequest{PR: hydrated, CreatedAt: created, UpdatedAt: created.Add(time.Hour)}); err != nil {
		t.Fatalf("PublishPullRequestUpdate: %v", err)
	}
	for _, iss := range brd.Snapshot() {
		if iss.GetNumber() == 4 && (len(iss.GetPrs()) != 1 || iss.GetPrs()[0].GetTitle() != "hydrated") {
			t.Fatalf("after hydrate prs = %v, want the hydrated PR", iss.GetPrs())
		}
	}
}
