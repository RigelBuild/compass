//go:build unix

package board

import (
	"context"
	"slices"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

var _ RefResolver = (*store.Store)(nil)

type fakeRefResolver struct {
	prs             []store.PullRequestRow
	issues          []store.Issue
	authored        []store.AuthoredRef
	owned           []store.AuthoredRef
	authoredByAgent map[store.AccountID][]store.AuthoredRef
	ownedByAgent    map[store.AccountID][]store.AuthoredRef
	calls           []string
	coords          [][]store.ForgeCoord
	agents          [][]store.AccountID
	openErr         error
	ownedErr        error
	prsErr          error
	issuesErr       error
}

func refsByAgent(agents []store.AccountID, all []store.AuthoredRef, specific map[store.AccountID][]store.AuthoredRef) map[store.AccountID][]store.AuthoredRef {
	out := make(map[store.AccountID][]store.AuthoredRef, len(agents))
	for _, agent := range agents {
		refs := all
		if specific != nil {
			refs = specific[agent]
		}
		out[agent] = refs
	}
	return out
}

func (f *fakeRefResolver) AuthoredCoordsForAgents(_ context.Context, agents []store.AccountID, coords []store.ForgeCoord) (map[store.AccountID][]store.AuthoredRef, error) {
	f.calls = append(f.calls, "owned")
	f.agents = append(f.agents, slices.Clone(agents))
	f.coords = append(f.coords, slices.Clone(coords))
	return refsByAgent(agents, f.owned, f.ownedByAgent), f.ownedErr
}

func (f *fakeRefResolver) OpenAuthoredRefsForAgents(_ context.Context, agents []store.AccountID) (map[store.AccountID][]store.AuthoredRef, error) {
	f.calls = append(f.calls, "authored")
	f.agents = append(f.agents, slices.Clone(agents))
	return refsByAgent(agents, f.authored, f.authoredByAgent), f.openErr
}

func (f *fakeRefResolver) PullRequestsByCoords(_ context.Context, coords []store.ForgeCoord) ([]store.PullRequestRow, error) {
	f.calls = append(f.calls, "prs")
	f.coords = append(f.coords, slices.Clone(coords))
	return f.prs, f.prsErr
}

func (f *fakeRefResolver) IssuesByCoords(_ context.Context, coords []store.ForgeCoord) ([]store.Issue, error) {
	f.calls = append(f.calls, "issues")
	f.coords = append(f.coords, slices.Clone(coords))
	return f.issues, f.issuesErr
}

func TestDeriveRefs(t *testing.T) {
	pr := &compassv1.PullRequest{
		Forge:      &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
		Repo:       "owner/repo",
		Number:     42,
		Title:      "stored PR",
		ForgeState: "open",
		Url:        "https://github.com/owner/repo/pull/42",
		HeadSha:    "new-head",
		Checks:     &compassv1.ChecksSummary{State: "success"},
		Reviews:    []*compassv1.Review{{Author: "reviewer", Verdict: "approved", CommitId: "old-head"}},
	}
	prJSON, err := protojson.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := &fakeRefResolver{
		prs: []store.PullRequestRow{{
			Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 42},
			State: "open", UpdatedAt: updated, PR: prJSON,
		}},
		issues: []store.Issue{{
			ForgeProvider: store.ForgeProviderGitHub, ForgeHost: "github.com", Repo: "owner/repo", Number: 9,
			Title: "stored issue", ForgeState: "open", State: store.IssueStateInProgress, ForgeUpdatedAt: updated,
		}},
		authored: []store.AuthoredRef{
			{Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 77}, Kind: store.ForgeArtifactKindPullRequest},
			{Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 42}, Kind: store.ForgeArtifactKindPullRequest},
		},
	}
	forges := Forges{GitHubHost: "github.com", LinearHost: "linear.app", LinearTeams: map[string]bool{"TEAM": true}}

	t.Run("stored rows, stale approval, and untracked ref", func(t *testing.T) {
		got, untracked, err := DeriveRefs(context.Background(), fake, forges, "agent-1", []string{"owner/repo#42", "owner/repo#9"})
		if err != nil {
			t.Fatalf("DeriveRefs: %v", err)
		}
		if len(got) != 2 || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST || got[0].GetGate() != compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW || !got[0].GetStaleApproval() {
			t.Errorf("PR derivation = %+v, want pull request with stale approval and awaiting-review gate", got)
		}
		if got[1].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE || got[1].GetTitle() != "stored issue" || got[1].GetObservedAtUnixMs() != updated.UnixMilli() {
			t.Errorf("GitHub issue ref = %+v, want stored issue and timestamp %d", got[1], updated.UnixMilli())
		}
		if !slices.Equal(untracked, []string{"owner/repo#77"}) {
			t.Errorf("untracked = %v, want [owner/repo#77]", untracked)
		}
	})

	t.Run("unknown and unconfigured references stay unspecified", func(t *testing.T) {
		unknownResolver := &fakeRefResolver{}
		got, _, err := DeriveRefs(context.Background(), unknownResolver, forges, "agent-1", []string{"other/repo#1", "TEAM-1", "BAD"})
		if err != nil {
			t.Fatalf("DeriveRefs: %v", err)
		}
		if len(got) != 3 || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED || got[1].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED || got[2].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED {
			t.Errorf("unknown refs = %+v, want unspecified kinds", got)
		}
	})

	t.Run("one authored query and one batched query per row type", func(t *testing.T) {
		resolver := &fakeRefResolver{
			authoredByAgent: map[store.AccountID][]store.AuthoredRef{
				"agent-1": {
					{Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 77}, Kind: store.ForgeArtifactKindPullRequest},
					{Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 42}, Kind: store.ForgeArtifactKindPullRequest},
				},
			},
		}
		_, _, err := DeriveRefs(context.Background(), resolver, forges, "agent-1", []string{"owner/repo#42", "owner/repo#9"})
		if err != nil {
			t.Fatalf("DeriveRefs: %v", err)
		}
		if !slices.Equal(resolver.calls, []string{"authored", "owned", "prs", "issues"}) {
			t.Errorf("resolver calls = %v", resolver.calls)
		}
		if len(resolver.agents) != 2 || !slices.Equal(resolver.agents[0], []store.AccountID{"agent-1"}) || !slices.Equal(resolver.agents[1], []store.AccountID{"agent-1"}) {
			t.Errorf("resolver agents = %v, want agent-1 for both authored reads", resolver.agents)
		}
		if len(resolver.coords) != 3 || !slices.Equal(resolver.coords[0], resolver.coords[1]) || !slices.Equal(resolver.coords[1], resolver.coords[2]) || len(resolver.coords[0]) != 3 {
			t.Errorf("coordinate batches = %+v, want identical batch of three unique coordinates", resolver.coords)
		}
	})
}

func TestDeriveRefsPreservesMalformedRefs(t *testing.T) {
	resolver := &fakeRefResolver{}
	got, _, err := DeriveRefs(context.Background(), resolver, Forges{}, "agent-1", []string{"broken", "owner/repo#2"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if got[0].GetRef() != "broken" || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED {
		t.Errorf("malformed ref = %+v", got[0])
	}
	if got[1].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED {
		t.Errorf("unconfigured ref = %+v", got[1])
	}
}

func TestDeriveRefsZeroNumberStaysUnknown(t *testing.T) {
	resolver := &fakeRefResolver{}
	got, _, err := DeriveRefs(context.Background(), resolver, Forges{GitHubHost: "github.com"}, "agent-1", []string{"owner/repo#0"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 1 || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED {
		t.Fatalf("zero-number ref = %+v, want unspecified", got)
	}
	if !slices.Equal(resolver.calls, []string{"authored", "owned", "prs", "issues"}) {
		t.Fatalf("resolver calls = %v, want one call per read method", resolver.calls)
	}
	if len(resolver.coords) != 3 || len(resolver.coords[0]) != 0 || len(resolver.coords[1]) != 0 || len(resolver.coords[2]) != 0 {
		t.Fatalf("coordinates = %v, want empty batches", resolver.coords)
	}
}

func TestDeriveRefsCoordinatesForGitHubAndLinear(t *testing.T) {
	resolver := &fakeRefResolver{
		prs: []store.PullRequestRow{{
			Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 5},
			PR:    []byte(`{"forge":{"provider":"FORGE_PROVIDER_GITHUB","host":"github.com"},"repo":"owner/repo","number":5,"title":"GitHub PR","forgeState":"open"}`),
		}},
		issues: []store.Issue{{
			ForgeProvider: store.ForgeProviderLinear,
			ForgeHost:     "linear.app",
			Repo:          "KEY",
			Number:        7,
			Title:         "Linear issue",
		}},
	}
	forges := Forges{GitHubHost: "github.com", LinearHost: "linear.app", LinearTeams: map[string]bool{"KEY": true}}
	got, _, err := DeriveRefs(context.Background(), resolver, forges, "agent-1", []string{"OwNeR/RePo#5", "KEY-7"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 2 || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST || got[0].GetTitle() != "GitHub PR" || got[1].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE || got[1].GetTitle() != "Linear issue" {
		t.Fatalf("derived refs = %+v, want stored GitHub PR and Linear issue", got)
	}
	want := []store.ForgeCoord{
		{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 5},
		{Provider: store.ForgeProviderLinear, Host: "linear.app", Repo: "KEY", Number: 7},
	}
	if len(resolver.coords) != 3 || !slices.Equal(resolver.coords[0], want) || !slices.Equal(resolver.coords[1], want) || !slices.Equal(resolver.coords[2], want) {
		t.Fatalf("coordinate batches = %v, want %v for all batched reads", resolver.coords, want)
	}
}

func TestDeriveRefsPrefersPullRequestOverIssueAtSameCoordinate(t *testing.T) {
	coord := store.ForgeCoord{
		Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 15,
	}
	pr := &compassv1.PullRequest{
		Forge: &compassv1.ForgeRef{
			Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
			Host:     coord.Host,
		},
		Repo: coord.Repo, Number: uint32(coord.Number), Title: "stored PR", ForgeState: "open",
	}
	prJSON, err := protojson.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal PR: %v", err)
	}
	resolver := &fakeRefResolver{
		prs: []store.PullRequestRow{{Coord: coord, State: "open", PR: prJSON}},
		issues: []store.Issue{{
			ForgeProvider: coord.Provider, ForgeHost: coord.Host, Repo: coord.Repo,
			Number: uint32(coord.Number), Title: "stored issue", ForgeState: "open",
		}},
	}
	got, _, err := DeriveRefs(
		context.Background(), resolver, Forges{GitHubHost: coord.Host}, "agent-1", []string{"owner/repo#15"},
	)
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 1 || got[0].GetKind() != compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST || got[0].GetTitle() != "stored PR" {
		t.Fatalf("derived ref = %+v, want stored PR to win over issue at the same coordinate", got)
	}
}

func TestDeriveRefsOwnershipIncludesMergedAuthoredPR(t *testing.T) {
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12}
	resolver := &fakeRefResolver{
		prs: []store.PullRequestRow{{
			Coord: coord, State: "merged",
			PR: []byte(`{"forge":{"provider":"FORGE_PROVIDER_GITHUB","host":"github.com"},"repo":"owner/repo","number":12,"title":"merged","forgeState":"merged"}`),
		}},
		owned: []store.AuthoredRef{{Coord: coord, Kind: store.ForgeArtifactKindPullRequest}},
	}
	got, untracked, err := DeriveRefs(context.Background(), resolver, Forges{GitHubHost: "github.com"}, "agent-1", []string{"owner/repo#12"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 1 || !got[0].GetOwned() || got[0].GetGate() != compassv1.BoardPrGate_BOARD_PR_GATE_MERGED {
		t.Fatalf("merged authored PR = %+v, want owned merged PR", got)
	}
	if len(untracked) != 0 {
		t.Errorf("untracked = %v, want empty for merged PR", untracked)
	}
}

func TestDeriveRefsUnconfiguredGitHubRefSuppressesAuthoredIssue(t *testing.T) {
	resolver := &fakeRefResolver{
		authored: []store.AuthoredRef{{
			Coord: store.ForgeCoord{
				Provider: store.ForgeProviderGitHub, Host: "github.com",
				Repo: "owner/repo", Number: 7,
			},
			Kind: store.ForgeArtifactKindIssue,
		}},
	}
	_, untracked, err := DeriveRefs(
		context.Background(), resolver, Forges{}, "agent-1", []string{"owner/repo#7"},
	)
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(untracked) != 0 {
		t.Fatalf("untracked = %v, want empty because the task names the authored issue", untracked)
	}
}

func TestDeriveRefsConfiguredGitHubHostSuppressesAuthoredIssue(t *testing.T) {
	resolver := &fakeRefResolver{
		authored: []store.AuthoredRef{{
			Coord: store.ForgeCoord{
				Provider: store.ForgeProviderGitHub, Host: "github.com",
				Repo: "owner/repo", Number: 7,
			},
			Kind: store.ForgeArtifactKindIssue,
		}},
	}
	_, untracked, err := DeriveRefs(
		context.Background(), resolver, Forges{GitHubHost: "github.com"},
		"agent-1", []string{"owner/repo#7"},
	)
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(untracked) != 0 {
		t.Fatalf("untracked = %v, want empty because the task names the authored issue", untracked)
	}
}

func TestDeriveRefsUnconfiguredTrackerRefSuppressesUntracked(t *testing.T) {
	resolver := &fakeRefResolver{
		authored: []store.AuthoredRef{{
			Coord: store.ForgeCoord{Provider: store.ForgeProviderLinear, Host: "linear.app", Repo: "KEY", Number: 7},
			Kind:  store.ForgeArtifactKindIssue,
		}},
	}
	_, untracked, err := DeriveRefs(context.Background(), resolver, Forges{}, "agent-1", []string{"KEY-7"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(untracked) != 0 {
		t.Fatalf("untracked = %v, want empty because the task names KEY-7", untracked)
	}
}

func TestDeriveRefsSeparatesStacksByForgeHost(t *testing.T) {
	firstHost := "github.com"
	secondHost := "github.enterprise"
	makePR := func(host string, number uint32, base, head string) *compassv1.PullRequest {
		return &compassv1.PullRequest{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: host},
			Repo:  "owner/repo", Number: number, ForgeState: "open", BaseRef: base, HeadRef: head,
		}
	}
	prs := []*compassv1.PullRequest{
		makePR(firstHost, 1, "main", "feature"),
		makePR(firstHost, 2, "feature", "tip"),
		makePR(secondHost, 1, "main", "feature"),
		makePR(secondHost, 2, "feature", "tip"),
		makePR(secondHost, 3, "tip", "long-tip"),
	}
	resolver := &fakeRefResolver{}
	for _, pr := range prs {
		data, err := protojson.Marshal(pr)
		if err != nil {
			t.Fatal(err)
		}
		coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: pr.GetForge().GetHost(), Repo: pr.GetRepo(), Number: uint64(pr.GetNumber())}
		resolver.prs = append(resolver.prs, store.PullRequestRow{Coord: coord, State: "open", PR: data})
		resolver.authored = append(resolver.authored, store.AuthoredRef{Coord: coord, Kind: store.ForgeArtifactKindPullRequest})
	}
	got, _, err := DeriveRefs(context.Background(), resolver, Forges{GitHubHost: firstHost}, "agent-1", []string{"owner/repo#1"})
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 1 || got[0].GetStackPosition() != 1 || got[0].GetStackTipRef() != "owner/repo#2" {
		t.Fatalf("first-host stack = %+v, want position 1 and owner/repo#2 tip", got)
	}
}

func TestDeriveRefsUntrackedSuppressionMatchesFullCoordinate(t *testing.T) {
	tests := []struct {
		name  string
		coord store.ForgeCoord
	}{
		{
			name: "same repo and number on another host",
			coord: store.ForgeCoord{
				Provider: store.ForgeProviderGitHub, Host: "github.enterprise",
				Repo: "owner/repo", Number: 5,
			},
		},
		{
			name: "same repo and number from another provider",
			coord: store.ForgeCoord{
				Provider: store.ForgeProviderGitLab, Host: "github.com",
				Repo: "owner/repo", Number: 5,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &fakeRefResolver{
				authored: []store.AuthoredRef{{
					Coord: tt.coord, Kind: store.ForgeArtifactKindPullRequest,
				}},
			}
			_, untracked, err := DeriveRefs(
				context.Background(), resolver, Forges{GitHubHost: "github.com"},
				"agent-1", []string{"owner/repo#5"},
			)
			if err != nil {
				t.Fatalf("DeriveRefs: %v", err)
			}
			if !slices.Equal(untracked, []string{"owner/repo#5"}) {
				t.Fatalf("untracked = %v, want authored artifact at a distinct coordinate", untracked)
			}
		})
	}
}

func TestDeriveRefsSeparatesStacksByForgeProvider(t *testing.T) {
	const host = "forge.example.com"
	tests := []struct {
		provider store.ForgeProvider
		wire     compassv1.ForgeProvider
		number   uint32
		base     string
		head     string
	}{
		{store.ForgeProviderGitHub, compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, 1, "main", "feature"},
		{store.ForgeProviderGitHub, compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, 2, "feature", "tip"},
		{store.ForgeProviderGitLab, compassv1.ForgeProvider_FORGE_PROVIDER_GITLAB, 3, "main", "feature"},
		{store.ForgeProviderGitLab, compassv1.ForgeProvider_FORGE_PROVIDER_GITLAB, 4, "feature", "middle"},
		{store.ForgeProviderGitLab, compassv1.ForgeProvider_FORGE_PROVIDER_GITLAB, 5, "middle", "long-tip"},
	}
	resolver := &fakeRefResolver{}
	for _, tt := range tests {
		pr := &compassv1.PullRequest{
			Forge: &compassv1.ForgeRef{Provider: tt.wire, Host: host},
			Repo:  "owner/repo", Number: tt.number, ForgeState: "open",
			BaseRef: tt.base, HeadRef: tt.head,
		}
		data, err := protojson.Marshal(pr)
		if err != nil {
			t.Fatal(err)
		}
		coord := store.ForgeCoord{
			Provider: tt.provider, Host: host, Repo: "owner/repo", Number: uint64(tt.number),
		}
		resolver.prs = append(resolver.prs, store.PullRequestRow{Coord: coord, State: "open", PR: data})
		resolver.authored = append(resolver.authored, store.AuthoredRef{
			Coord: coord, Kind: store.ForgeArtifactKindPullRequest,
		})
	}
	got, _, err := DeriveRefs(
		context.Background(), resolver, Forges{GitHubHost: host},
		"agent-1", []string{"owner/repo#1"},
	)
	if err != nil {
		t.Fatalf("DeriveRefs: %v", err)
	}
	if len(got) != 1 || got[0].GetStackPosition() != 1 || got[0].GetStackTipRef() != "owner/repo#2" {
		t.Fatalf("GitHub stack = %+v, want position 1 and owner/repo#2 tip", got)
	}
}
