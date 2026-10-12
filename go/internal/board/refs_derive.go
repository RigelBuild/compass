//go:build unix

package board

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

// RefResolver is the stored-row read surface used to derive board references.
type RefResolver interface {
	PullRequestsByCoords(ctx context.Context, coords []store.ForgeCoord) ([]store.PullRequestRow, error)
	IssuesByCoords(ctx context.Context, coords []store.ForgeCoord) ([]store.Issue, error)
	AuthoredCoordsForAgent(ctx context.Context, agent store.AccountID, coords []store.ForgeCoord) ([]store.AuthoredRef, error)
	OpenAuthoredRefs(ctx context.Context, agent store.AccountID) ([]store.AuthoredRef, error)
}

// Forges maps parsed references to the configured forge hosts and Linear teams.
type Forges struct {
	GitHubHost  string
	LinearHost  string
	LinearTeams map[string]bool
}

type artifactKey struct {
	coord store.ForgeCoord
	kind  compassv1.BoardRefKind
}

type parsedBoardRef struct {
	ref   refs.Ref
	coord store.ForgeCoord
	kind  compassv1.BoardRefKind
}
type stackGroup struct {
	provider store.ForgeProvider
	host     string
}

type storedBoardPR struct {
	row store.PullRequestRow
	pr  *compassv1.PullRequest
}

// DeriveRefs joins parsed task refs to stored forge rows without contacting a provider.
func DeriveRefs(
	ctx context.Context,
	resolver RefResolver,
	forges Forges,
	agent store.AccountID,
	refsIn []string,
) ([]*compassv1.BoardRef, []string, error) {
	derived := make([]*compassv1.BoardRef, len(refsIn))
	parsed := make([]*parsedBoardRef, len(refsIn))
	coords := make([]store.ForgeCoord, 0, len(refsIn))
	coordSet := make(map[store.ForgeCoord]struct{}, len(refsIn))
	taskRefs := make(map[artifactKey]struct{}, len(refsIn))
	for i, text := range refsIn {
		ref, err := refs.Parse(text)
		if err != nil {
			derived[i] = &compassv1.BoardRef{Ref: text}
			continue
		}
		item, base := parseBoardRef(ref, forges)
		derived[i] = base
		if item == nil {
			addTaskRefCoords(ref, nil, taskRefs)
			continue
		}
		parsed[i] = item
		addCoord(item.coord, coordSet, &coords)
		addTaskRefCoords(ref, item, taskRefs)
	}
	openAuthored, err := resolver.OpenAuthoredRefs(ctx, agent)
	if err != nil {
		return nil, nil, fmt.Errorf("board: list open authored refs: %w", err)
	}
	openAuthoredSet := make(map[artifactKey]struct{}, len(openAuthored))
	for _, ref := range openAuthored {
		coord := ref.Coord.Normalized()
		openAuthoredSet[artifactKey{coord: coord, kind: boardRefKind(ref.Kind)}] = struct{}{}
		addCoord(coord, coordSet, &coords)
	}

	authored, err := resolver.AuthoredCoordsForAgent(ctx, agent, coords)
	if err != nil {
		return nil, nil, fmt.Errorf("board: list authored coordinates: %w", err)
	}
	authoredSet := make(map[artifactKey]struct{}, len(authored))
	for _, ref := range authored {
		authoredSet[artifactKey{coord: ref.Coord.Normalized(), kind: boardRefKind(ref.Kind)}] = struct{}{}
	}

	data, err := loadStoredRows(ctx, resolver, coords, openAuthoredSet)
	if err != nil {
		return nil, nil, err
	}
	prsByCoord, issuesByCoord, stacksByGroup := data.prs, data.issues, data.stacks

	for i, item := range parsed {
		if item == nil {
			continue
		}
		base := derived[i]
		switch item.kind {
		case compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST:
			if stored, ok := prsByCoord[item.coord]; ok {
				_, base.Owned = authoredSet[artifactKey{coord: item.coord, kind: item.kind}]
				group := stackGroup{provider: item.coord.Provider, host: strings.ToLower(item.coord.Host)}
				fillPullRequestRef(base, stored, item, stacksByGroup[group])
				continue
			}
			if item.ref.Kind != refs.KindForge {
				continue
			}
			issue, ok := issuesByCoord[item.coord]
			if !ok {
				continue
			}
			fillIssueDerivedRef(base, item, issue, authoredSet)
		case compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE:
			issue, ok := issuesByCoord[item.coord]
			if !ok {
				continue
			}
			fillIssueDerivedRef(base, item, issue, authoredSet)
		case compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED:
			continue
		}
	}

	untracked := make([]string, 0, len(openAuthored))
	for _, ref := range openAuthored {
		if _, named := taskRefs[artifactKey{coord: ref.Coord.Normalized(), kind: boardRefKind(ref.Kind)}]; named {
			continue
		}
		untracked = append(untracked, authoredRefString(ref))
	}
	return derived, untracked, nil
}

type storedRows struct {
	prs    map[store.ForgeCoord]storedBoardPR
	issues map[store.ForgeCoord]store.Issue
	stacks map[stackGroup]map[string]Stack
}

func loadStoredRows(
	ctx context.Context,
	resolver RefResolver,
	coords []store.ForgeCoord,
	openAuthored map[artifactKey]struct{},
) (storedRows, error) {
	prRows, err := resolver.PullRequestsByCoords(ctx, coords)
	if err != nil {
		return storedRows{}, fmt.Errorf("board: load pull requests: %w", err)
	}
	issueRows, err := resolver.IssuesByCoords(ctx, coords)
	if err != nil {
		return storedRows{}, fmt.Errorf("board: load issues: %w", err)
	}

	prsByCoord := make(map[store.ForgeCoord]storedBoardPR, len(prRows))
	openAuthoredPRs := make(map[stackGroup][]*compassv1.PullRequest)
	for _, row := range prRows {
		if err := storePRRow(row, prsByCoord); err != nil {
			return storedRows{}, err
		}
		coord := row.Coord.Normalized()
		key := artifactKey{coord: coord, kind: compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST}
		if row.State != "open" {
			continue
		}
		if _, owned := openAuthored[key]; !owned {
			continue
		}
		group := stackGroup{provider: coord.Provider, host: strings.ToLower(coord.Host)}
		openAuthoredPRs[group] = append(openAuthoredPRs[group], prsByCoord[coord].pr)
	}

	stacksByGroup := make(map[stackGroup]map[string]Stack, len(openAuthoredPRs))
	for group, prs := range openAuthoredPRs {
		stacksByGroup[group] = StackOf(prs)
	}

	issuesByCoord := make(map[store.ForgeCoord]store.Issue, len(issueRows))
	for _, issue := range issueRows {
		issuesByCoord[issueCoord(issue)] = issue
	}
	return storedRows{prs: prsByCoord, issues: issuesByCoord, stacks: stacksByGroup}, nil
}

func storePRRow(row store.PullRequestRow, byCoord map[store.ForgeCoord]storedBoardPR) error {
	pr := &compassv1.PullRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(row.PR, pr); err != nil {
		return fmt.Errorf("board: decode stored pull request %v: %w", row.Coord, err)
	}
	byCoord[row.Coord.Normalized()] = storedBoardPR{row: row, pr: pr}
	return nil
}

func parseBoardRef(parsed refs.Ref, forges Forges) (*parsedBoardRef, *compassv1.BoardRef) {
	coord, kind, ok := configuredCoord(parsed, forges)
	if !ok {
		return nil, &compassv1.BoardRef{Ref: parsed.String()}
	}
	return &parsedBoardRef{ref: parsed, coord: coord, kind: kind}, &compassv1.BoardRef{
		Ref: parsed.String(), Url: unknownRefURL(parsed, forges),
	}
}

func addCoord(coord store.ForgeCoord, seen map[store.ForgeCoord]struct{}, coords *[]store.ForgeCoord) {
	if _, exists := seen[coord]; exists {
		return
	}
	seen[coord] = struct{}{}
	*coords = append(*coords, coord)
}

func fillIssueDerivedRef(base *compassv1.BoardRef, item *parsedBoardRef, issue store.Issue, authored map[artifactKey]struct{}) {
	base.Kind = compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE
	_, base.Owned = authored[artifactKey{coord: item.coord, kind: compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE}]
	fillIssueRef(base, issue)
}

func boardRefKind(kind store.ForgeArtifactKind) compassv1.BoardRefKind {
	switch kind {
	case store.ForgeArtifactKindIssue:
		return compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE
	case store.ForgeArtifactKindPullRequest:
		return compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST
	default:
		return compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED
	}
}

func configuredCoord(ref refs.Ref, forges Forges) (store.ForgeCoord, compassv1.BoardRefKind, bool) {
	if ref.Number == 0 {
		return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
	}
	switch ref.Kind {
	case refs.KindForge:
		if forges.GitHubHost == "" {
			return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
		}
		return store.ForgeCoord{
			Provider: store.ForgeProviderGitHub,
			Host:     forges.GitHubHost,
			Repo:     ref.Owner + "/" + ref.Repo,
			Number:   ref.Number,
		}.Normalized(), compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST, true
	case refs.KindTracker:
		if forges.LinearHost == "" || !forges.LinearTeams[ref.Key] {
			return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
		}
		return store.ForgeCoord{
			Provider: store.ForgeProviderLinear,
			Host:     forges.LinearHost,
			Repo:     ref.Key,
			Number:   ref.Number,
		}.Normalized(), compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE, true
	default:
		return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
	}
}

func taskRefCoord(ref refs.Ref) (store.ForgeCoord, compassv1.BoardRefKind, bool) {
	if ref.Number == 0 {
		return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
	}
	switch ref.Kind {
	case refs.KindForge:
		return store.ForgeCoord{
			Provider: store.ForgeProviderGitHub, Host: "github.com",
			Repo: ref.Owner + "/" + ref.Repo, Number: ref.Number,
		}.Normalized(), compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST, true
	case refs.KindTracker:
		return store.ForgeCoord{
			Provider: store.ForgeProviderLinear, Host: "linear.app",
			Repo: ref.Key, Number: ref.Number,
		}.Normalized(), compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE, true
	default:
		return store.ForgeCoord{}, compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED, false
	}
}

func addTaskRefCoords(ref refs.Ref, item *parsedBoardRef, taskRefs map[artifactKey]struct{}) {
	coord, kind, ok := taskRefCoord(ref)
	if !ok {
		return
	}
	if item != nil {
		coord, kind = item.coord, item.kind
	}
	taskRefs[artifactKey{coord: coord, kind: kind}] = struct{}{}
	if ref.Kind == refs.KindForge {
		taskRefs[artifactKey{coord: coord, kind: compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE}] = struct{}{}
	}
}

func issueCoord(issue store.Issue) store.ForgeCoord {
	return store.ForgeCoord{
		Provider: issue.ForgeProvider,
		Host:     issue.ForgeHost,
		Repo:     issue.Repo,
		Number:   uint64(issue.Number),
	}.Normalized()
}

func unknownRefURL(ref refs.Ref, forges Forges) string {
	switch ref.Kind {
	case refs.KindForge:
		if forges.GitHubHost == "" {
			return ""
		}
		return "https://" + forges.GitHubHost + "/" + ref.Owner + "/" + ref.Repo + "/issues/" + strconv.FormatUint(ref.Number, 10)
	case refs.KindTracker:
		if forges.LinearHost == "" || !forges.LinearTeams[ref.Key] {
			return ""
		}
		return "https://" + forges.LinearHost + "/browse/" + ref.Key + "-" + strconv.FormatUint(ref.Number, 10)
	default:
		return ""
	}
}

func authoredRefString(ref store.AuthoredRef) string {
	coord := ref.Coord.Normalized()
	switch ref.Kind {
	case store.ForgeArtifactKindPullRequest, store.ForgeArtifactKindIssue:
		if coord.Provider == store.ForgeProviderLinear {
			return coord.Repo + "-" + strconv.FormatUint(coord.Number, 10)
		}
		return strings.ToLower(coord.Repo) + "#" + strconv.FormatUint(coord.Number, 10)
	default:
		return ""
	}
}

func boardRefKey(repo string, number uint64) string {
	return repo + "#" + strconv.FormatUint(number, 10)
}

func fillPullRequestRef(base *compassv1.BoardRef, stored storedBoardPR, item *parsedBoardRef, stacks map[string]Stack) {
	pr := stored.pr
	base.Kind = compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST
	base.Title = pr.GetTitle()
	base.Url = pr.GetUrl()
	base.Draft = pr.GetDraft()
	base.ChecksState = pr.GetChecks().GetState()
	base.Gate, base.StaleApproval = derivePRState(pr)
	base.MergeQueueState = pr.GetMergeQueue().GetState()
	base.ObservedAtUnixMs = unixMilli(stored.row.UpdatedAt)
	if stack, ok := stacks[boardRefKey(item.ref.Owner+"/"+item.ref.Repo, item.ref.Number)]; ok {
		base.StackPosition = stack.Position
		base.StackTipRef = stack.Tip
	}
}

func fillIssueRef(base *compassv1.BoardRef, issue store.Issue) {
	base.Title = issue.Title
	base.Url = issue.URL
	base.ForgeState = issue.ForgeState
	base.IssueState = compassv1.IssueState(issue.State)
	base.ObservedAtUnixMs = unixMilli(issue.ForgeUpdatedAt)
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
