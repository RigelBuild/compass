//go:build pgtest

package comms

import (
	"context"
	"fmt"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

type countingBoardRefs struct {
	resolver board.RefResolver
	authored int
	owned    int
	prs      int
	issues   int
}

func (c *countingBoardRefs) OpenAuthoredRefsForAgents(ctx context.Context, agents []store.AccountID) (map[store.AccountID][]store.AuthoredRef, error) {
	c.authored++
	return c.resolver.OpenAuthoredRefsForAgents(ctx, agents)
}

func (c *countingBoardRefs) AuthoredCoordsForAgents(ctx context.Context, agents []store.AccountID, coords []store.ForgeCoord) (map[store.AccountID][]store.AuthoredRef, error) {
	c.owned++
	return c.resolver.AuthoredCoordsForAgents(ctx, agents, coords)
}

func (c *countingBoardRefs) PullRequestsByCoords(ctx context.Context, coords []store.ForgeCoord) ([]store.PullRequestRow, error) {
	c.prs++
	return c.resolver.PullRequestsByCoords(ctx, coords)
}

func (c *countingBoardRefs) IssuesByCoords(ctx context.Context, coords []store.ForgeCoord) ([]store.Issue, error) {
	c.issues++
	return c.resolver.IssuesByCoords(ctx, coords)
}

func (c *countingBoardRefs) reset() {
	c.authored, c.owned, c.prs, c.issues = 0, 0, 0, 0
}

func (c *countingBoardRefs) assertOneReadEach(t *testing.T, operation string) {
	t.Helper()
	if c.authored != 1 || c.owned != 1 || c.prs != 1 || c.issues != 1 {
		t.Errorf("%s reads = authored:%d owned:%d prs:%d issues:%d, want one each", operation, c.authored, c.owned, c.prs, c.issues)
	}
}

func TestBoardRefReadsAreBatchedForListAndRoster(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-refs-batch-owner")
	state := compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE
	for i := range 3 {
		agent := mustAgent(t, st, owner.ID, fmt.Sprintf("board-refs-batch-agent-%d", i))
		number := uint32(700 + i)
		ref := fmt.Sprintf("owner/repo#%d", number)
		pr := &compassv1.PullRequest{
			Forge: &compassv1.ForgeRef{
				Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
				Host:     "github.com",
			},
			Repo: "owner/repo", Number: number, Title: "Batch query PR", ForgeState: "open",
		}
		prJSON, err := protojson.Marshal(pr)
		if err != nil {
			t.Fatalf("protojson.Marshal(%d): %v", number, err)
		}
		coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: uint64(number)}
		if _, err := st.UpsertPullRequest(ctx, store.PullRequestRow{
			Coord: coord, State: "open", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PR: prJSON,
		}, nil); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", number, err)
		}
		if err := st.RecordAuthoredArtifact(ctx, store.AuthoredArtifact{
			Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Kind: store.ForgeArtifactKindPullRequest,
			Number: coord.Number, AgentAccountID: agent.ID, OwnerUserID: owner.ID, CreatedAtUnixMS: int64(number),
		}); err != nil {
			t.Fatalf("RecordAuthoredArtifact(%d): %v", number, err)
		}
		if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
			UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("PR task"), PrimaryRef: &ref, State: &state}},
		}); err != nil {
			t.Fatalf("UpdateBoardAsAccount(%s): %v", agent.Handle, err)
		}
	}
	svc.SetBoardForges(board.Forges{GitHubHost: "github.com"})
	counter := &countingBoardRefs{resolver: st}
	svc.boardRefs = counter

	listed, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(listed.GetBoards()) != 3 {
		t.Fatalf("listed boards = %d, want 3", len(listed.GetBoards()))
	}
	counter.assertOneReadEach(t, "ListBoardsAsAccount")

	counter.reset()
	roster, err := svc.RosterAsAccount(ctx, owner.ID, &compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER,
	})
	if err != nil {
		t.Fatalf("RosterAsAccount: %v", err)
	}
	if len(roster.GetEntries()) != 3 {
		t.Fatalf("roster entries = %d, want three agents", len(roster.GetEntries()))
	}
	counter.assertOneReadEach(t, "RosterAsAccount")
}
