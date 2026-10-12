//go:build pgtest && unix

package board

import (
	"context"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestDeriveRefsFromStore(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "derive-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "derive-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	updated := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 22}
	if _, err := st.UpsertIssueForgeFields(ctx, store.IssueForgeFields{
		ForgeProvider:  coord.Provider,
		ForgeHost:      coord.Host,
		Repo:           coord.Repo,
		Number:         uint32(coord.Number),
		Title:          "stored issue",
		ForgeState:     "open",
		ForgeUpdatedAt: updated,
	}); err != nil {
		t.Fatalf("UpsertIssueForgeFields: %v", err)
	}
	if err := st.RecordAuthoredArtifact(ctx, store.AuthoredArtifact{
		Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo,
		Kind: store.ForgeArtifactKindIssue, Number: coord.Number,
		AgentAccountID: agent.ID, OwnerUserID: owner.ID,
		SessionID: "session", CreatedAtUnixMS: updated.UnixMilli(),
	}); err != nil {
		t.Fatalf("RecordAuthoredArtifact: %v", err)
	}

	prCoord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 23}
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{Provider: prCoord.Provider, Host: prCoord.Host, Repo: prCoord.Repo, Enabled: true}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}
	pr := &compassv1.PullRequest{
		Forge:      &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: prCoord.Host},
		Repo:       prCoord.Repo,
		Number:     uint32(prCoord.Number),
		Title:      "stored PR",
		ForgeState: "open",
		Url:        "https://github.com/owner/repo/pull/23",
		HeadRef:    "feature",
		BaseRef:    "main",
		Checks:     &compassv1.ChecksSummary{State: "success"},
	}
	prJSON, err := protojson.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal PR: %v", err)
	}
	if err := st.CreatePullRequestWithLink(ctx, store.AuthoredArtifact{
		Provider: prCoord.Provider, Host: prCoord.Host, Repo: prCoord.Repo,
		Kind: store.ForgeArtifactKindPullRequest, Number: prCoord.Number,
		AgentAccountID: agent.ID, OwnerUserID: owner.ID,
		SessionID: "session", CreatedAtUnixMS: updated.UnixMilli(),
	}, store.PullRequestRow{Coord: prCoord, State: "open", CreatedAt: updated, UpdatedAt: updated, PR: prJSON}, nil); err != nil {
		t.Fatalf("CreatePullRequestWithLink: %v", err)
	}

	got, untracked, err := DeriveRefs(ctx, st, Forges{GitHubHost: "github.com"}, agent.ID, []string{"owner/repo#22", "owner/repo#23"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("derived refs = %d, want 2", len(got))
	}
	if got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE || got[0].GetTitle() != "stored issue" || got[0].GetObservedAtUnixMs() != updated.UnixMilli() {
		t.Errorf("issue ref = %+v, want stored row and timestamp %d", got[0], updated.UnixMilli())
	}
	if got[1].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST || got[1].GetGate() != compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW {
		t.Errorf("PR ref = %+v, want open PR awaiting review", got[1])
	}
	if len(untracked) != 0 {
		t.Errorf("untracked = %v, want empty", untracked)
	}
}
