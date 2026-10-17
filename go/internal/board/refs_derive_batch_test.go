//go:build unix

package board

import (
	"context"
	"errors"
	"slices"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

func batchPRRow(t *testing.T, number uint32, base, head string) (store.ForgeCoord, store.PullRequestRow) {
	t.Helper()
	pr := &compassv1.PullRequest{
		Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
		Repo:  "owner/repo", Number: number, ForgeState: "open", BaseRef: base, HeadRef: head,
	}
	data, err := protojson.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: uint64(number)}
	return coord, store.PullRequestRow{Coord: coord, State: "open", PR: data}
}

func authoredPR(coord store.ForgeCoord) store.AuthoredRef {
	return store.AuthoredRef{Coord: coord, Kind: store.ForgeArtifactKindPullRequest}
}

// Two agents in one batch: ownership, untracked refs, and stacks each come
// from the requesting agent's authored rows only.
func TestDeriveRefsBatchSplitsPerAgent(t *testing.T) {
	c1, r1 := batchPRRow(t, 1, "main", "a1")
	c2, r2 := batchPRRow(t, 2, "a1", "a2")
	c3, r3 := batchPRRow(t, 3, "main", "b1")
	resolver := &fakeRefResolver{
		prs: []store.PullRequestRow{r1, r2, r3},
		authoredByAgent: map[store.AccountID][]store.AuthoredRef{
			"agent-a": {authoredPR(c1), authoredPR(c2)},
			"agent-b": {authoredPR(c3)},
		},
		ownedByAgent: map[store.AccountID][]store.AuthoredRef{
			"agent-a": {authoredPR(c1), authoredPR(c2)},
			"agent-b": {authoredPR(c3)},
		},
	}
	results := DeriveRefsBatch(context.Background(), resolver, Forges{GitHubHost: "github.com"}, []RefRequest{
		{Agent: "agent-a", Refs: []string{"owner/repo#1", "owner/repo#3"}},
		{Agent: "agent-b", Refs: []string{"owner/repo#1"}},
	})
	if !slices.Equal(resolver.calls, []string{"authored", "owned", "prs", "issues"}) {
		t.Fatalf("resolver calls = %v, want one read of each kind", resolver.calls)
	}
	a, b := results[0], results[1]
	if a.Err != nil || b.Err != nil {
		t.Fatalf("errors = %v, %v", a.Err, b.Err)
	}
	if !a.Refs[0].GetOwned() || a.Refs[1].GetOwned() {
		t.Errorf("agent-a owned = %v, %v; want #1 owned, #3 not", a.Refs[0].GetOwned(), a.Refs[1].GetOwned())
	}
	if b.Refs[0].GetOwned() {
		t.Errorf("agent-b owns #1 = true, want false")
	}
	if a.Refs[0].GetStackPosition() != 1 || a.Refs[0].GetStackTipRef() != "owner/repo#2" {
		t.Errorf("agent-a #1 stack = %d/%q, want position 1 tip owner/repo#2", a.Refs[0].GetStackPosition(), a.Refs[0].GetStackTipRef())
	}
	if b.Refs[0].GetStackPosition() != 0 {
		t.Errorf("agent-b #1 stack position = %d, want none: the stack is agent-a's", b.Refs[0].GetStackPosition())
	}
	if !slices.Equal(a.Untracked, []string{"owner/repo#2"}) || !slices.Equal(b.Untracked, []string{"owner/repo#3"}) {
		t.Errorf("untracked = %v, %v; want [owner/repo#2], [owner/repo#3]", a.Untracked, b.Untracked)
	}
}

func TestDeriveRefsBatchIsolatesUndecodablePR(t *testing.T) {
	c1, good := batchPRRow(t, 1, "main", "a1")
	bad := store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 9},
		State: "open", PR: []byte("{not json"),
	}
	resolver := &fakeRefResolver{prs: []store.PullRequestRow{good, bad}, authoredByAgent: map[store.AccountID][]store.AuthoredRef{}}
	results := DeriveRefsBatch(context.Background(), resolver, Forges{GitHubHost: "github.com"}, []RefRequest{
		{Agent: "agent-a", Refs: []string{"owner/repo#9"}},
		{Agent: "agent-b", Refs: []string{"owner/repo#1"}},
	})
	if results[0].Err == nil {
		t.Errorf("agent-a error = nil, want the decode failure for owner/repo#9")
	}
	if results[1].Err != nil || results[1].Refs[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST {
		t.Errorf("agent-b = %+v, want a derived PR ref for %v", results[1], c1)
	}
}

func TestDeriveRefsBatchStoreErrorFailsEveryRequest(t *testing.T) {
	boom := errors.New("boom")
	resolver := &fakeRefResolver{prsErr: boom}
	results := DeriveRefsBatch(context.Background(), resolver, Forges{GitHubHost: "github.com"}, []RefRequest{
		{Agent: "agent-a", Refs: []string{"owner/repo#1"}},
		{Agent: "agent-b", Refs: []string{"owner/repo#2"}},
	})
	for i, result := range results {
		if !errors.Is(result.Err, boom) {
			t.Errorf("result %d error = %v, want boom", i, result.Err)
		}
	}
	if slices.Contains(resolver.calls, "issues") {
		t.Errorf("resolver calls = %v, want no issue read after the PR read fails", resolver.calls)
	}
}
