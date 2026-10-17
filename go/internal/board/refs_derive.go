//go:build unix

package board

import (
	"context"
	"errors"
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
	AuthoredCoordsForAgents(ctx context.Context, agents []store.AccountID, coords []store.ForgeCoord) (map[store.AccountID][]store.AuthoredRef, error)
	OpenAuthoredRefsForAgents(ctx context.Context, agents []store.AccountID) (map[store.AccountID][]store.AuthoredRef, error)
}

// RefRequest identifies one agent's board references to derive.
type RefRequest struct {
	Agent store.AccountID
	Refs  []string
}

// RefResult contains references derived for one request.
type RefResult struct {
	Refs      []*compassv1.BoardRef
	Untracked []string
	Err       error
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
	results := DeriveRefsBatch(ctx, resolver, forges, []RefRequest{{Agent: agent, Refs: refsIn}})
	if len(results) == 0 {
		return nil, nil, nil
	}
	return results[0].Refs, results[0].Untracked, results[0].Err
}

type batchRequest struct {
	parsed   []*parsedBoardRef
	taskRefs map[artifactKey]struct{}
	coords   map[store.ForgeCoord]struct{}
}

// batchRows holds the batch's store reads, indexed for per-request splitting.
type batchRows struct {
	open   map[store.AccountID][]store.AuthoredRef
	owned  map[store.AccountID]map[artifactKey]struct{}
	prs    map[store.ForgeCoord]storedBoardPR
	badPRs map[store.ForgeCoord]error
	stacks map[store.AccountID]map[stackGroup]map[string]Stack
	issues map[store.ForgeCoord]store.Issue
}

// DeriveRefsBatch resolves references for multiple board requests with one store read per row type.
func DeriveRefsBatch(
	ctx context.Context,
	resolver RefResolver,
	forges Forges,
	reqs []RefRequest,
) []RefResult {
	results := make([]RefResult, len(reqs))
	requests := make([]batchRequest, len(reqs))
	agents := make([]store.AccountID, 0, len(reqs))
	agentSet := make(map[store.AccountID]struct{}, len(reqs))
	var allCoords []store.ForgeCoord
	coordSet := make(map[store.ForgeCoord]struct{})
	for i, req := range reqs {
		requests[i], results[i].Refs = parseRefRequest(req.Refs, forges, coordSet, &allCoords)
		if _, found := agentSet[req.Agent]; !found {
			agentSet[req.Agent] = struct{}{}
			agents = append(agents, req.Agent)
		}
	}
	rows, err := loadBatchRows(ctx, resolver, agents, reqs, requests, coordSet, allCoords)
	if err != nil {
		for i := range results {
			results[i] = RefResult{Err: err}
		}
		return results
	}
	for i, req := range reqs {
		results[i] = rows.result(req.Agent, requests[i], results[i].Refs)
	}
	return results
}

func parseRefRequest(refsIn []string, forges Forges, coordSet map[store.ForgeCoord]struct{}, allCoords *[]store.ForgeCoord) (batchRequest, []*compassv1.BoardRef) {
	request := batchRequest{
		parsed:   make([]*parsedBoardRef, len(refsIn)),
		taskRefs: make(map[artifactKey]struct{}, len(refsIn)),
		coords:   make(map[store.ForgeCoord]struct{}, len(refsIn)),
	}
	derived := make([]*compassv1.BoardRef, len(refsIn))
	for j, text := range refsIn {
		ref, err := refs.Parse(text)
		if err != nil {
			derived[j] = &compassv1.BoardRef{Ref: text}
			continue
		}
		item, base := parseBoardRef(ref, forges)
		derived[j] = base
		if item != nil {
			request.parsed[j] = item
			request.coords[item.coord] = struct{}{}
			addCoord(item.coord, coordSet, allCoords)
		}
		addTaskRefCoords(ref, item, request.taskRefs)
	}
	return request, derived
}

// loadBatchRows reads each row type once for the union of agents and coords;
// it also adds each agent's open authored coords to its requests.
func loadBatchRows(
	ctx context.Context,
	resolver RefResolver,
	agents []store.AccountID,
	reqs []RefRequest,
	requests []batchRequest,
	coordSet map[store.ForgeCoord]struct{},
	allCoords []store.ForgeCoord,
) (batchRows, error) {
	openByAgent, err := resolver.OpenAuthoredRefsForAgents(ctx, agents)
	if err != nil {
		return batchRows{}, fmt.Errorf("board: list open authored refs: %w", err)
	}
	for i, req := range reqs {
		for _, ref := range openByAgent[req.Agent] {
			coord := ref.Coord.Normalized()
			requests[i].coords[coord] = struct{}{}
			addCoord(coord, coordSet, &allCoords)
		}
	}
	ownedByAgent, err := resolver.AuthoredCoordsForAgents(ctx, agents, allCoords)
	if err != nil {
		return batchRows{}, fmt.Errorf("board: list authored coordinates: %w", err)
	}
	prRows, err := resolver.PullRequestsByCoords(ctx, allCoords)
	if err != nil {
		return batchRows{}, fmt.Errorf("board: load pull requests: %w", err)
	}
	issueRows, err := resolver.IssuesByCoords(ctx, allCoords)
	if err != nil {
		return batchRows{}, fmt.Errorf("board: load issues: %w", err)
	}
	rows := batchRows{
		open:   openByAgent,
		owned:  make(map[store.AccountID]map[artifactKey]struct{}, len(agents)),
		prs:    make(map[store.ForgeCoord]storedBoardPR, len(prRows)),
		badPRs: make(map[store.ForgeCoord]error),
		issues: make(map[store.ForgeCoord]store.Issue, len(issueRows)),
	}
	for _, agent := range agents {
		rows.owned[agent] = authoredKeys(ownedByAgent[agent])
	}
	for _, row := range prRows {
		if err := storePRRow(row, rows.prs); err != nil {
			rows.badPRs[row.Coord.Normalized()] = err
		}
	}
	rows.stacks = agentStacks(agents, openByAgent, prRows, rows.prs)
	for _, issue := range issueRows {
		rows.issues[issueCoord(issue)] = issue
	}
	return rows, nil
}

func authoredKeys(refsIn []store.AuthoredRef) map[artifactKey]struct{} {
	keys := make(map[artifactKey]struct{}, len(refsIn))
	for _, ref := range refsIn {
		keys[artifactKey{coord: ref.Coord.Normalized(), kind: boardRefKind(ref.Kind)}] = struct{}{}
	}
	return keys
}

// agentStacks groups each agent's own open authored PRs into stacks, so one
// agent's stack never shows on another agent's board.
func agentStacks(
	agents []store.AccountID,
	openByAgent map[store.AccountID][]store.AuthoredRef,
	prRows []store.PullRequestRow,
	prs map[store.ForgeCoord]storedBoardPR,
) map[store.AccountID]map[stackGroup]map[string]Stack {
	stacks := make(map[store.AccountID]map[stackGroup]map[string]Stack, len(agents))
	for _, agent := range agents {
		open := authoredKeys(openByAgent[agent])
		groups := make(map[stackGroup][]*compassv1.PullRequest)
		for _, row := range prRows {
			coord := row.Coord.Normalized()
			stored, ok := prs[coord]
			if !ok || row.State != "open" {
				continue
			}
			if _, owned := open[artifactKey{coord: coord, kind: compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST}]; !owned {
				continue
			}
			group := stackGroup{provider: coord.Provider, host: strings.ToLower(coord.Host)}
			groups[group] = append(groups[group], stored.pr)
		}
		stacks[agent] = make(map[stackGroup]map[string]Stack, len(groups))
		for group, groupPRs := range groups {
			stacks[agent][group] = StackOf(groupPRs)
		}
	}
	return stacks
}

// result fills one request from the batch. An undecodable PR fails only the
// requests whose coords include it.
func (rows batchRows) result(agent store.AccountID, request batchRequest, derived []*compassv1.BoardRef) RefResult {
	var decodeErr error
	for coord, err := range rows.badPRs {
		if _, relevant := request.coords[coord]; relevant {
			decodeErr = errors.Join(decodeErr, err)
		}
	}
	if decodeErr != nil {
		return RefResult{Err: decodeErr}
	}
	authoredSet := rows.owned[agent]
	for i, item := range request.parsed {
		if item != nil {
			rows.fillRef(derived[i], item, authoredSet, rows.stacks[agent])
		}
	}
	openAuthored := rows.open[agent]
	untracked := make([]string, 0, len(openAuthored))
	for _, ref := range openAuthored {
		if _, named := request.taskRefs[artifactKey{coord: ref.Coord.Normalized(), kind: boardRefKind(ref.Kind)}]; named {
			continue
		}
		untracked = append(untracked, authoredRefString(ref))
	}
	return RefResult{Refs: derived, Untracked: untracked}
}

func (rows batchRows) fillRef(base *compassv1.BoardRef, item *parsedBoardRef, authoredSet map[artifactKey]struct{}, stacks map[stackGroup]map[string]Stack) {
	switch item.kind {
	case compassv1.BoardRefKind_BOARD_REF_KIND_PULL_REQUEST:
		if stored, ok := rows.prs[item.coord]; ok {
			_, base.Owned = authoredSet[artifactKey{coord: item.coord, kind: item.kind}]
			group := stackGroup{provider: item.coord.Provider, host: strings.ToLower(item.coord.Host)}
			fillPullRequestRef(base, stored, item, stacks[group])
			return
		}
		if item.ref.Kind != refs.KindForge {
			return
		}
		if issue, ok := rows.issues[item.coord]; ok {
			fillIssueDerivedRef(base, item, issue, authoredSet)
		}
	case compassv1.BoardRefKind_BOARD_REF_KIND_ISSUE:
		if issue, ok := rows.issues[item.coord]; ok {
			fillIssueDerivedRef(base, item, issue, authoredSet)
		}
	case compassv1.BoardRefKind_BOARD_REF_KIND_UNSPECIFIED:
	}
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
