//go:build unix

// The agent-initiated forge-write leg: forgeService implements the runnerhub.ForgeCaller
// seam, the forge sibling of boardService, plus the ONE place agent forge artifacts get
// owner-attribution stamped (DL-050), the F3 memo consulted, the credential role selected
// (F1), and a fault flattened into one in-band ForgeCallError. The oneof dispatch lives here.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/mirror"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

// forgeCoordinate is the registry key: the wire forge enum + host. A repo does
// NOT enter the key — one credential pair serves every repo on a coordinate
// (DL-091 multi-forge disambiguation is provider+host).
type forgeCoordinate struct {
	provider compassv1.ForgeProvider
	host     string
}

// forgeProviderRoles separates author and reviewer identities so GitHub can
// distinguish an agent's approval from its author account.
type forgeProviderRoles struct {
	author   forge.Provider
	reviewer forge.Provider
}

// forgeProviderRegistry maps provider+host coordinates to clients. Unset refs
// use the registered default; empty hosts use provider defaults.
type forgeProviderRegistry struct {
	entries     map[forgeCoordinate]forgeProviderRoles
	defaultHost map[compassv1.ForgeProvider]string
	def         forgeCoordinate
}

// newForgeProviderRegistry returns an empty registry. Register coordinates with
// register; the first registered as default resolves a nil/unset ForgeRef.
func newForgeProviderRegistry() *forgeProviderRegistry {
	return &forgeProviderRegistry{
		entries:     make(map[forgeCoordinate]forgeProviderRoles),
		defaultHost: make(map[compassv1.ForgeProvider]string),
	}
}

// register adds coord's author+reviewer clients. The first host seen for a
// provider becomes that provider's default host (empty-host resolution, A3);
// isDefault marks coord as the coordinate a nil/unset ForgeRef resolves to.
// An empty host is refused: it would become the default the DL-055 row rejects.
func (r *forgeProviderRegistry) register(coord forgeCoordinate, author, reviewer forge.Provider, isDefault bool) error {
	if coord.host == "" {
		return fmt.Errorf("forge: register %s coordinate: empty host", coord.provider)
	}
	r.entries[coord] = forgeProviderRoles{author: author, reviewer: reviewer}
	if _, ok := r.defaultHost[coord.provider]; !ok {
		r.defaultHost[coord.provider] = coord.host
	}
	if isDefault {
		r.def = coord
	}
	return nil
}

// resolvedForge is one lookup result: the two role clients, the shared body
// limit (the author client's — both roles hit the same forge with the same
// limit), and the RESOLVED coordinate (provider+host) the DL-055 row records.
type resolvedForge struct {
	author    forge.Provider
	reviewer  forge.Provider
	bodyLimit int
	provider  compassv1.ForgeProvider
	host      string
}

// resolve maps a request's ForgeRef onto its coordinate entry. A nil/unset ref
// (or UNSPECIFIED provider) resolves the default coordinate; an empty host
// resolves the provider's default host (A3). ok=false for an unconfigured
// coordinate — the caller renders it as an in-band not_found.
func (r *forgeProviderRegistry) resolve(ref *compassv1.ForgeRef) (resolvedForge, bool) {
	coord := r.def
	if ref != nil && ref.GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_UNSPECIFIED {
		coord = forgeCoordinate{provider: ref.GetProvider(), host: ref.GetHost()}
		if coord.host == "" {
			coord.host = r.defaultHost[ref.GetProvider()]
		}
	}
	roles, ok := r.entries[coord]
	if !ok {
		return resolvedForge{}, false
	}
	return resolvedForge{
		author:    roles.author,
		reviewer:  roles.reviewer,
		bodyLimit: roles.author.BodyLimit(),
		provider:  coord.provider,
		host:      coord.host,
	}, true
}

// forgeStore is the narrow persistence surface for writes, attribution, scope,
// mirror mode, and tracked PR reads.
type forgeStore interface { //nolint:interfacebloat // the forge write path's whole store surface: attribution, write intents, scope, and mirror mode.
	GetAccount(ctx context.Context, id store.AccountID) (store.Account, error)
	CreatePullRequestWithLink(ctx context.Context, a store.AuthoredArtifact, pr store.PullRequestRow, issue *store.ForgeCoord) error
	EnsureAgentForgeSubscription(ctx context.Context, sub store.AgentForgeSubscription) (string, error)
	DeleteAgentForgeSubscription(ctx context.Context, agent store.AccountID, subscriptionID string) error
	RecordStateTransition(ctx context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64, state string, agent store.AccountID, at time.Time) error
	HasForgeScope(ctx context.Context, account store.AccountID, provider store.ForgeProvider, host, repo string) (bool, error)
	ForgeMirrorMode(ctx context.Context, provider store.ForgeProvider, host, repo string) (store.ForgeMirrorMode, error)
	PullRequestsByCoords(ctx context.Context, coords []store.ForgeCoord) ([]store.PullRequestRow, error)
	RecordWriteIntent(ctx context.Context, in store.WriteIntent) (store.WriteIntent, error)
	HoldWriteIntent(ctx context.Context, in store.WriteIntent) (store.WriteIntent, error)
	PromoteWriteIntent(ctx context.Context, id string) error
	CompleteWriteIntent(ctx context.Context, id string, artifact *store.AuthoredArtifact, forgeKey string) error
	EndWriteIntent(ctx context.Context, id string, state store.WriteState) error
	DeleteExpiredHeldWriteIntents(ctx context.Context, olderThan time.Time, limit int) (int64, error)
	PendingWriteIntents(ctx context.Context, olderThan time.Time, limit int) ([]store.WriteIntent, error)
	RecordNotice(ctx context.Context, n store.Notice) (store.Notice, error)
	MarkNoticeDelivered(ctx context.Context, id string) error
	UndeliveredNotices(ctx context.Context, limit int) ([]store.Notice, error)
	BumpNoticeAttempt(ctx context.Context, id string) error
	AuthoredArtifactByCoordinateFold(ctx context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64) (store.AuthoredArtifact, error)
}

// forgeService implements ForgeCaller and the forge write chokepoint. It owns
// attribution, issue projection, provider clients, and mirror timers.
type forgeService struct {
	store         forgeStore
	issueBrd      *board.IssueProjection
	providers     *forgeProviderRegistry
	now           func() time.Time
	mirrorStoreDB *store.Store
	publicURL     string
	// enforceScopes gates every coordinate write on an account_forge_scopes grant.
	enforceScopes       bool
	mirrorMu            sync.Mutex
	mirrorDone          <-chan struct{}
	mirrorCancel        context.CancelFunc
	mirrorClosing       bool
	mirrorWG            sync.WaitGroup
	mirrorTimers        map[string]*mirrorTimer
	scheduleMirrorAfter func(time.Duration, func()) func() bool
	mirrorPoster        mirror.Poster
	mirrorRecheckRetry  map[string]int
	// mirror pushes agent bundles for submit_stack and deletes retired branches; nil means off.
	mirror forge.Mirror
	// notifier tells an agent when another agent rebased its bookmark; nil logs instead.
	notifier agentNotifier
}

type mirrorTimer struct {
	stop func() bool
	due  time.Time
	once sync.Once
}

// newForgeService constructs the caller; Serve wires it after dependencies exist
// to break the hub cycle.
func newForgeService(st *store.Store, issueBrd *board.IssueProjection, providers *forgeProviderRegistry) *forgeService {
	return &forgeService{store: st, issueBrd: issueBrd, providers: providers, now: time.Now, mirrorStoreDB: st}
}

// ExecuteForgeCallAsAccount dispatches one forge call. Missing callers and
// malformed arms return Connect errors; tool outcomes are in-band results.
// The caller stamps call_id on each result.
func (s *forgeService) ExecuteForgeCallAsAccount(
	ctx context.Context,
	caller store.AccountID,
	sessionID string,
	call *compassv1internal.ForgeCallRequest,
) (*compassv1internal.ForgeCallResult, error) {
	if caller == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "forge: caller account is required")
	}

	variant := call.GetCall()
	if variant == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "forge: call has no operation variant set")
	}
	switch c := variant.(type) {
	case *compassv1internal.ForgeCallRequest_CreateIssue:
		return s.createIssue(ctx, caller, sessionID, call, c.CreateIssue), nil
	case *compassv1internal.ForgeCallRequest_CommentOnIssue:
		return s.commentOnIssue(ctx, caller, sessionID, call, c.CommentOnIssue), nil
	case *compassv1internal.ForgeCallRequest_CreatePullRequest:
		return s.createPullRequest(ctx, caller, sessionID, call, c.CreatePullRequest), nil
	case *compassv1internal.ForgeCallRequest_CommentOnPullRequest:
		return s.commentOnPullRequest(ctx, caller, sessionID, call, c.CommentOnPullRequest), nil
	case *compassv1internal.ForgeCallRequest_SubmitReview:
		return s.submitReview(ctx, caller, sessionID, call, c.SubmitReview), nil
	case *compassv1internal.ForgeCallRequest_TransitionIssueState:
		return s.transitionIssueState(ctx, caller, call, c.TransitionIssueState), nil
	case *compassv1internal.ForgeCallRequest_EditIssue:
		return s.editIssue(ctx, caller, sessionID, call, c.EditIssue), nil
	case *compassv1internal.ForgeCallRequest_TransitionPullRequestState:
		return s.transitionPullRequestState(ctx, caller, call, c.TransitionPullRequestState), nil
	case *compassv1internal.ForgeCallRequest_GetIssue:
		return s.getIssue(ctx, call, c.GetIssue), nil
	case *compassv1internal.ForgeCallRequest_ListIssues:
		return s.listIssues(ctx, call, c.ListIssues), nil
	case *compassv1internal.ForgeCallRequest_GetPullRequest:
		return s.getPullRequest(ctx, call, c.GetPullRequest), nil
	case *compassv1internal.ForgeCallRequest_Subscribe:
		return s.subscribeForge(ctx, caller, call, c.Subscribe), nil
	case *compassv1internal.ForgeCallRequest_Unsubscribe:
		return s.unsubscribeForge(ctx, caller, c.Unsubscribe), nil
	case *compassv1internal.ForgeCallRequest_MirrorMessages:
		return s.mirrorMessages(ctx, caller, sessionID, call, c.MirrorMessages), nil
	case *compassv1internal.ForgeCallRequest_SubmitStack:
		return s.submitStack(ctx, caller, sessionID, call, c.SubmitStack), nil
	case *compassv1internal.ForgeCallRequest_StackStatus:
		return s.stackStatus(ctx, caller, call, c.StackStatus), nil
	case *compassv1internal.ForgeCallRequest_RetireHead:
		return s.retireHead(ctx, caller, call, c.RetireHead), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, "forge: call has no operation variant set")
	}
}

// requireForgeScope rejects a write outside the caller's grants with the same
// result as a missing artifact, so a rejection does not reveal that the repo exists.
func (s *forgeService) requireForgeScope(ctx context.Context, caller store.AccountID, rf resolvedForge, repo string) *compassv1internal.ForgeCallError {
	if !s.enforceScopes {
		return nil
	}
	allowed, err := s.store.HasForgeScope(ctx, caller, store.ForgeProvider(rf.provider), rf.host, repo)
	if err != nil {
		return storeForgeError(err)
	}
	if !allowed {
		return artifactNotFound()
	}
	return nil
}

// callerIdentity is the resolved attribution for a write: the stamp Author plus
// the owning user's AccountID the DL-055 row records.
type callerIdentity struct {
	author  forge.Author
	ownerID store.AccountID
}

// resolveIdentity turns the resolved caller AccountID into the DL-050 stamp
// Author: the agent's own handle (AgentHandle), the owning user's handle
// (OwnerHandle, via the agent's owner_user_id), and the passed-through session
// id. A store miss/fault is mapped to an in-band ForgeCallError.
func (s *forgeService) resolveIdentity(ctx context.Context, caller store.AccountID, sessionID string) (callerIdentity, *compassv1internal.ForgeCallError) {
	agentAcct, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		return callerIdentity{}, storeForgeError(err)
	}
	if agentAcct.Agent == nil {
		return callerIdentity{}, forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: caller %q is not an agent account", caller))
	}
	ownerID := agentAcct.Agent.OwnerUserID
	ownerAcct, err := s.store.GetAccount(ctx, ownerID)
	if err != nil {
		return callerIdentity{}, storeForgeError(err)
	}
	return callerIdentity{
		author:  forge.Author{AgentHandle: agentAcct.Handle, OwnerHandle: ownerAcct.Handle, SessionID: sessionID},
		ownerID: ownerID,
	}, nil
}

// resolveTarget validates the repo (empty is invalid_argument BEFORE any store
// or provider touch) and resolves the provider coordinate (an unconfigured one
// is an in-band not_found). It is the first step of every arm.
func (s *forgeService) resolveTarget(call *compassv1internal.ForgeCallRequest, repo string) (resolvedForge, *compassv1internal.ForgeCallError) {
	if repo == "" {
		return resolvedForge{}, forgeErr(connect.CodeInvalidArgument, "forge: repo is required")
	}
	rf, ok := s.providers.resolve(call.GetForge())
	if !ok {
		return resolvedForge{}, forgeErr(connect.CodeNotFound, "forge: no provider configured for the requested coordinate")
	}
	return rf, nil
}

// subscribeToStoreKind maps the wire ForgeArtifactKind onto the store enum,
// rejecting UNSPECIFIED(0) as an in-band invalid_argument (a subscription must
// name an issue or a pull request). The two enums share their numeric domain
// (issue=1, pull_request=2), so a known kind is a direct cast.
func subscribeToStoreKind(kind compassv1internal.ForgeArtifactKind) (store.ForgeArtifactKind, *compassv1internal.ForgeCallError) {
	switch kind {
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE:
		return store.ForgeArtifactKindIssue, nil
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST:
		return store.ForgeArtifactKindPullRequest, nil
	default:
		return store.ForgeArtifactKindUnspecified, forgeErr(connect.CodeInvalidArgument, "forge: subscription kind must be issue or pull_request")
	}
}

// subscribeForge records interest in an artifact or container coordinate.
// It carries scope and project to the store; subscriptions do not stamp writes.
func (s *forgeService) subscribeForge(ctx context.Context, caller store.AccountID, call *compassv1internal.ForgeCallRequest, req *compassv1internal.SubscribeForgeRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	kind, fe := subscribeToStoreKind(req.GetKind())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	// Linear emits only issue events, so a pull-request subscription would never notify.
	if rf.provider == compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR && kind == store.ForgeArtifactKindPullRequest {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge: Linear subscriptions must use kind issue"))
	}
	id, err := s.store.EnsureAgentForgeSubscription(ctx, store.AgentForgeSubscription{
		AgentAccountID: caller,
		Provider:       store.ForgeProvider(rf.provider),
		Host:           rf.host,
		Repo:           req.GetRepo(),
		Kind:           kind,
		Number:         req.GetNumber(),
		Scope:          store.ForgeSubscriptionScope(req.GetScope()),
		Project:        req.GetProject(),
	})
	if err != nil {
		return forgeErrorResult(storeForgeError(err))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Subscribed{
			Subscribed: &compassv1internal.SubscribeForgeResponse{SubscriptionId: id},
		},
	}
}

// unsubscribeForge deletes the caller's subscription by id (scoped to the
// calling agent — an unknown id, or one owned by another agent, is an in-band
// not_found), running the DL-053 last-subscription cursor GC in the store's
// transaction. Unsubscribe is by id, so it needs no coordinate resolution.
func (s *forgeService) unsubscribeForge(ctx context.Context, caller store.AccountID, req *compassv1internal.UnsubscribeForgeRequest) *compassv1internal.ForgeCallResult {
	if err := s.store.DeleteAgentForgeSubscription(ctx, caller, req.GetSubscriptionId()); err != nil {
		return forgeErrorResult(storeForgeError(err))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Unsubscribed{
			Unsubscribed: &compassv1internal.UnsubscribeForgeResponse{},
		},
	}
}

// authoredArtifact builds the DL-055 ownership row for one write. The intent
// carries the request key, so the row carries none.
func (s *forgeService) authoredArtifact(rf resolvedForge, id callerIdentity, caller store.AccountID, sessionID, repo string, kind store.ForgeArtifactKind, number uint64) store.AuthoredArtifact {
	return store.AuthoredArtifact{
		Provider:        store.ForgeProvider(rf.provider),
		Host:            rf.host,
		Repo:            repo,
		Kind:            kind,
		Number:          number,
		AgentAccountID:  caller,
		OwnerUserID:     id.ownerID,
		SessionID:       sessionID,
		CreatedAtUnixMS: s.now().UnixMilli(),
	}
}

// createIssue is a create arm: resolve target, record the write intent (a
// replay returns the earlier object with no create), then stamp, create on the
// author client, and complete the intent with the ownership row.
func (s *forgeService) createIssue(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CreateIssueRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := requireWriteKey(call); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	intent, prior, fe := s.beginWrite(ctx, call.GetClientRequestId(), writeTarget{
		rf: rf, id: id, caller: caller, session: sessionID, repo: req.GetRepo(), kind: forge.WriteKindIssue,
	})
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if prior != nil {
		return replayResult(intent, prior)
	}
	body, err := writeBody(rf, id.author, req.GetBody(), intent.ID, true)
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: "create_issue", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit})
	}
	iss, err := rf.author.CreateIssue(ctx, req.GetRepo(), forge.CreateIssue{Title: req.GetTitle(), Body: body, ID: intent.ID, Labels: req.GetLabels()})
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: "create_issue"})
	}
	a := s.authoredArtifact(rf, id, caller, sessionID, req.GetRepo(), store.ForgeArtifactKindIssue, iss.Number)
	if fe := s.completeWrite(ctx, intent.ID, &a, ""); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Issue{Issue: translateIssue(iss, rf, req.GetRepo())},
	}
}

// createPullRequest is the PR-create twin of createIssue (author client; one
// write intent; ownership row, PR row, and link on success).
func (s *forgeService) createPullRequest(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CreatePullRequestRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := requireWriteKey(call); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	link, fe := s.resolveIssueLink(rf, req.GetRepo(), req.GetIssue())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	intent, prior, fe := s.beginWrite(ctx, call.GetClientRequestId(), writeTarget{
		rf: rf, id: id, caller: caller, session: sessionID, repo: req.GetRepo(), kind: forge.WriteKindPullRequest, headRef: req.GetHeadRef(),
	})
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if prior != nil {
		return replayResult(intent, prior)
	}
	// A PR is recovered by its head, so its body carries no write marker.
	body, err := writeBody(rf, id.author, req.GetBody(), intent.ID, false)
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: "create_pull_request", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit})
	}
	pr, err := rf.author.CreatePullRequest(ctx, req.GetRepo(), forge.CreatePR{
		Title:   req.GetTitle(),
		Body:    body,
		HeadRef: req.GetHeadRef(),
		BaseRef: req.GetBaseRef(),
		Draft:   req.GetDraft(),
	})
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: "create_pull_request"})
	}
	wire := translatePR(pr, rf, req.GetRepo())
	a := s.authoredArtifact(rf, id, caller, sessionID, req.GetRepo(), store.ForgeArtifactKindPullRequest, pr.Number)
	if fe := s.recordPullRequest(ctx, a, pr, wire, link); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.completeWrite(ctx, intent.ID, &a, ""); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_PullRequest{PullRequest: wire},
	}
}

// resolveIssueLink checks the shape of a create's explicit issue link without
// reading the issue, so a tracker outage never fails a PR create. An empty repo
// on the PR's own forge means the PR's repo. A nil link returns nil.
func (s *forgeService) resolveIssueLink(rf resolvedForge, prRepo string, in *compassv1internal.PullRequestIssueLink) (*store.ForgeCoord, *compassv1internal.ForgeCallError) {
	if in == nil {
		return nil, nil
	}
	if in.GetNumber() == 0 {
		return nil, forgeErr(connect.CodeInvalidArgument, "forge: issue number is required")
	}
	target := rf
	if in.GetForge().GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_UNSPECIFIED {
		var ok bool
		if target, ok = s.providers.resolve(in.GetForge()); !ok {
			return nil, forgeErr(connect.CodeNotFound, "forge: no provider configured for the issue's coordinate")
		}
	}
	// The store trims the repo, so a blank one must fail here, before the forge write.
	repo := strings.TrimSpace(in.GetRepo())
	if repo == "" {
		if target.provider != rf.provider || target.host != rf.host {
			return nil, forgeErr(connect.CodeInvalidArgument, "forge: issue repo is required on another forge")
		}
		repo = prRepo
	}
	return &store.ForgeCoord{Provider: store.ForgeProvider(target.provider), Host: target.host, Repo: repo, Number: in.GetNumber()}, nil
}

// recordPullRequest writes the DL-055 row, the PR row and the explicit link in
// one transaction, then republishes the linked issue so the PR shows at once.
// The PR row's forge_updated_at is the epoch, so the next hydrate always wins.
func (s *forgeService) recordPullRequest(ctx context.Context, a store.AuthoredArtifact, raw forge.PullRequest, wire *compassv1.PullRequest, link *store.ForgeCoord) *compassv1internal.ForgeCallError {
	body, err := protojson.Marshal(wire)
	if err != nil {
		return forgeErr(connect.CodeInternal, "forge: encode pull request")
	}
	coord := store.ForgeCoord{Provider: a.Provider, Host: a.Host, Repo: a.Repo, Number: raw.Number}
	row := store.PullRequestRow{Coord: coord, State: raw.State, CreatedAt: raw.CreatedAt, UpdatedAt: time.Unix(0, 0).UTC(), PR: body}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = s.now()
	}
	if err := s.store.CreatePullRequestWithLink(ctx, a, row, link); err != nil {
		return storeForgeError(err)
	}
	// The rows are committed; a failed publish heals on the next issue hydrate.
	if link != nil {
		if err := s.issueBrd.PublishExplicitLink(ctx, *link); err != nil {
			slog.WarnContext(ctx, "forge: publish linked issue failed", "error", err)
		}
	}
	return nil
}

// commentOnIssue writes an issue comment under a write intent (author client).
func (s *forgeService) commentOnIssue(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CommentOnIssueRequest) *compassv1internal.ForgeCallResult {
	return s.writeComment(ctx, caller, sessionID, call, req.GetRepo(), req.GetIssueNumber(), "comment_on_issue", req.GetBody(),
		func(p forge.Provider, body, clientID string) (forge.Comment, error) {
			return p.CommentOnIssue(ctx, req.GetRepo(), req.GetIssueNumber(), body, clientID)
		},
		func(ref *compassv1internal.CommentRef) *compassv1internal.ForgeCallResult {
			return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: ref}}
		})
}

// commentOnPullRequest is the PR-conversation-comment twin of commentOnIssue.
func (s *forgeService) commentOnPullRequest(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CommentOnPullRequestRequest) *compassv1internal.ForgeCallResult {
	return s.writeComment(ctx, caller, sessionID, call, req.GetRepo(), req.GetPrNumber(), "comment_on_pull_request", req.GetBody(),
		func(p forge.Provider, body, clientID string) (forge.Comment, error) {
			return p.CommentOnPullRequest(ctx, req.GetRepo(), req.GetPrNumber(), body, clientID)
		},
		func(ref *compassv1internal.CommentRef) *compassv1internal.ForgeCallResult {
			return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_PrComment{PrComment: ref}}
		})
}

// writeComment runs one comment create under a write intent. The intent id is
// the Linear comment id and the GitHub body marker, so recovery finds either.
func (s *forgeService) writeComment(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, repo string, parent uint64, op, rawBody string,
	create func(p forge.Provider, body, clientID string) (forge.Comment, error),
	wrap func(*compassv1internal.CommentRef) *compassv1internal.ForgeCallResult,
) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, repo)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := requireWriteKey(call); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, repo); fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	intent, prior, fe := s.beginWrite(ctx, call.GetClientRequestId(), writeTarget{
		rf: rf, id: id, caller: caller, session: sessionID, repo: repo, kind: forge.WriteKindComment, parent: parent,
	})
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if prior != nil {
		if intent.Kind != forge.WriteKindComment {
			return replayResult(intent, prior)
		}
		return wrap(&compassv1internal.CommentRef{Url: prior.URL, CommentId: refID(prior)})
	}
	body, err := writeBody(rf, id.author, rawBody, intent.ID, true)
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: op, bodyLen: len(rawBody), bodyLimit: rf.bodyLimit})
	}
	cm, err := create(rf.author, body, intent.ID)
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.author.Name(), op: op})
	}
	key := cm.Key
	if key == "" {
		key = strconv.FormatUint(cm.ID, 10)
	}
	if fe := s.completeWrite(ctx, intent.ID, nil, key); fe != nil {
		return forgeErrorResult(fe)
	}
	return wrap(&compassv1internal.CommentRef{Url: cm.URL, CommentId: cm.ID})
}

// submitReview stamps the top-level body and dispatches as the reviewer under
// a write intent. Inline bodies are stripped, not stamped, because ingestion
// parses owner headers.
func (s *forgeService) submitReview(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.SubmitReviewRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := requireWriteKey(call); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	intent, prior, fe := s.beginWrite(ctx, call.GetClientRequestId(), writeTarget{
		rf: rf, id: id, caller: caller, session: sessionID, repo: req.GetRepo(), kind: forge.WriteKindReview, parent: req.GetPrNumber(),
	})
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if prior != nil {
		return replayResult(intent, prior)
	}
	body, err := writeBody(rf, id.author, req.GetBody(), intent.ID, true)
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.reviewer.Name(), op: "submit_review", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit})
	}

	comments := make([]forge.ReviewCommentInput, 0, len(req.GetComments()))
	for _, c := range req.GetComments() {
		// STRIP-not-stamp (A6): discard the parsed display claim, keep the clean
		// body. StripOwner removes every owner block regardless of forge/mangle.
		clean, _, _ := forge.StripOwner(forge.StripWriteMarkers(c.GetBody()))
		comments = append(comments, forge.ReviewCommentInput{
			Path: c.GetPath(),
			Line: c.GetLine(),
			Side: c.GetSide(),
			Body: clean,
		})
	}

	sr, err := rf.reviewer.SubmitReview(ctx, req.GetRepo(), req.GetPrNumber(), forge.SubmitReview{
		Verdict:  req.GetVerdict(),
		Body:     body,
		Comments: comments,
	})
	if err != nil {
		return s.failWrite(ctx, intent.ID, err, forgeOp{provider: rf.reviewer.Name(), op: "submit_review"})
	}
	if fe := s.completeWrite(ctx, intent.ID, nil, strconv.FormatUint(sr.ID, 10)); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Review{Review: &compassv1internal.ReviewRef{Url: sr.URL, ReviewId: sr.ID, Verdict: sr.Verdict}},
	}
}

// transitionStateDomain is the portable target-state domain both transition arms
// screen against — exactly forge.Issue.State's, and exactly the store memo's.
// Anything else is an in-band invalid_argument BEFORE any provider touch.
func transitionStateDomain(state string) *compassv1internal.ForgeCallError {
	if state != store.TransitionStateOpen && state != store.TransitionStateClosed {
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf(
			"forge: state must be %q or %q, got %q", store.TransitionStateOpen, store.TransitionStateClosed, state))
	}
	return nil
}

// screenIssueRefinements rejects provider-specific fields the selected forge
// cannot express. It must reject rather than silently drop caller intent;
// this permits providers to ignore only foreign fields safely.
func screenIssueRefinements(rf resolvedForge, closeReason, workflowState string) *compassv1internal.ForgeCallError {
	if closeReason != "" && rf.provider != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf(
			"forge: close_reason is a GitHub-issue refinement and provider %q cannot express it", rf.author.Name()))
	}
	if workflowState != "" && rf.provider != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf(
			"forge: workflow_state is a Linear refinement and provider %q cannot express it", rf.author.Name()))
	}
	return nil
}

// rememberTransition records attribution after provider success. Webhooks match
// canonical repo names; non-canonical input leaves the event unattributed,
// causing only a redundant wake rather than a lost signal.
func (s *forgeService) rememberTransition(ctx context.Context, rf resolvedForge, caller store.AccountID, repo string, kind store.ForgeArtifactKind, number uint64, state string) *compassv1internal.ForgeCallError {
	if err := s.store.RecordStateTransition(ctx, store.ForgeProvider(rf.provider), rf.host, repo, kind, number, state, caller, s.now()); err != nil {
		// The provider write ALREADY LANDED, so the error must name what succeeded:
		// a caller told only "memo: db unavailable" reasonably retries an operation
		// that is already done. The mapped code is unchanged; the message gains it.
		fe := storeForgeError(err)
		fe.Message = fmt.Sprintf(
			"forge: %s#%d was transitioned to %s, but recording the acting agent failed: %v",
			repo, number, state, err)
		return fe
	}
	return nil
}

// transitionIssueState validates and dispatches state, then records attribution
// after provider success. Retries converge on the target state; no artifact is
// stamped or deduplicated.
func (s *forgeService) transitionIssueState(ctx context.Context, caller store.AccountID, call *compassv1internal.ForgeCallRequest, req *compassv1internal.TransitionIssueStateRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := transitionStateDomain(req.GetState()); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := screenIssueRefinements(rf, req.GetCloseReason(), req.GetWorkflowState()); fe != nil {
		return forgeErrorResult(fe)
	}
	iss, err := rf.author.TransitionIssueState(ctx, req.GetRepo(), req.GetIssueNumber(), forge.TransitionState{
		State:         req.GetState(),
		CloseReason:   req.GetCloseReason(),
		WorkflowState: req.GetWorkflowState(),
	})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "transition_issue_state"}))
	}
	if fe := s.rememberTransition(ctx, rf, caller, req.GetRepo(), store.ForgeArtifactKindIssue, iss.Number, req.GetState()); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Issue{Issue: translateIssue(iss, rf, req.GetRepo())},
	}
}

// editIssue checks the caller's scope, stamps any body edit, and returns the
// updated issue without creating an ownership row.
func (s *forgeService) editIssue(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.EditIssueRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if req.Title == nil && req.Body == nil {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge: edit_issue requires title or body"))
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	in := forge.EditIssue{Title: req.Title}
	if req.Body != nil {
		bodyText := req.GetBody()
		id, fe := s.resolveIdentity(ctx, caller, sessionID)
		if fe != nil {
			return forgeErrorResult(fe)
		}
		body, err := forge.StampOwner(bodyText, id.author, rf.bodyLimit)
		if err != nil {
			return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "edit_issue", bodyLen: len(bodyText), bodyLimit: rf.bodyLimit}))
		}
		in.Body = &body
	}
	issue, err := rf.author.EditIssue(ctx, req.GetRepo(), req.GetIssueNumber(), in)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "edit_issue"}))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Issue{Issue: translateIssue(issue, rf, req.GetRepo())},
	}
}

// transitionPullRequestState accepts portable state only; merging is separate.
// Providers without a PR model return ErrUnsupported.
func (s *forgeService) transitionPullRequestState(ctx context.Context, caller store.AccountID, call *compassv1internal.ForgeCallRequest, req *compassv1internal.TransitionPullRequestStateRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := transitionStateDomain(req.GetState()); fe != nil {
		return forgeErrorResult(fe)
	}
	// Only the author changes a PR's state; a PR with no row is a human's or another App's.
	if fe := s.requirePRAuthor(ctx, caller, rf, req.GetRepo(), req.GetPrNumber()); fe != nil {
		return forgeErrorResult(fe)
	}
	pr, err := rf.author.TransitionPullRequestState(ctx, req.GetRepo(), req.GetPrNumber(), forge.TransitionState{State: req.GetState()})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "transition_pull_request_state"}))
	}
	if fe := s.rememberTransition(ctx, rf, caller, req.GetRepo(), store.ForgeArtifactKindPullRequest, pr.Number, req.GetState()); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_PullRequest{PullRequest: translatePR(pr, rf, req.GetRepo())},
	}
}

// getIssue reads tracked artifacts from the projection; others are fetched live.
// Provider bodies are stripped of owner attribution, never stamped.
func (s *forgeService) getIssue(ctx context.Context, call *compassv1internal.ForgeCallRequest, req *compassv1internal.GetIssueRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if tracked, ok := s.trackedIssue(rf, req.GetRepo(), narrowForgeNumber(req.GetIssueNumber())); ok {
		return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Issue{Issue: tracked}}
	}
	iss, err := rf.author.GetIssue(ctx, req.GetRepo(), req.GetIssueNumber())
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "get_issue"}))
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Issue{Issue: translateIssue(iss, rf, req.GetRepo())}}
}

// getPullRequest fetches live because the projection stores issues only.
func (s *forgeService) getPullRequest(ctx context.Context, call *compassv1internal.ForgeCallRequest, req *compassv1internal.GetPullRequestRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	pr, err := rf.author.GetPullRequest(ctx, req.GetRepo(), req.GetPrNumber())
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "get_pull_request"}))
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_PullRequest{PullRequest: translatePR(pr, rf, req.GetRepo())}}
}

// listIssues composes a live author-client filtered fetch + TranslateIssue per
// row. A filtered live list has no projection index, so it is always live (see
// summary: tracked-list follow-up seam).
func (s *forgeService) listIssues(ctx context.Context, call *compassv1internal.ForgeCallRequest, req *compassv1internal.ListIssuesRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	state := req.GetState()
	if state == "" {
		state = "open" // the gateway contract; GitHub's own empty default is "all"
	}
	limit := listIssuesLimit(req.GetLimit())
	raws, err := rf.author.ListIssues(ctx, req.GetRepo(), forge.IssueFilter{State: state, Labels: req.GetLabels(), Limit: limit})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "list_issues"}))
	}
	if len(raws) > limit {
		raws = raws[:limit]
	}
	resp := &compassv1internal.ListIssuesResponse{Issues: make([]*compassv1.Issue, 0, len(raws))}
	for _, iss := range raws {
		resp.Issues = append(resp.Issues, translateIssue(iss, rf, req.GetRepo()))
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Issues{Issues: resp}}
}

// listIssuesLimit applies ListIssuesRequest.limit's contract: 0 is the default
// page of 30, and anything past 100 is capped.
func listIssuesLimit(limit uint32) int {
	switch {
	case limit == 0:
		return 30
	case limit > 100:
		return 100
	default:
		return int(limit)
	}
}

// trackedIssue finds an ingested issue by forge coordinate. It scans the
// projection snapshot (O(n)); there is no coordinate index.
func (s *forgeService) trackedIssue(rf resolvedForge, repo string, number uint32) (*compassv1.Issue, bool) {
	if s.issueBrd == nil {
		return nil, false
	}
	for _, iss := range s.issueBrd.Snapshot() {
		if iss.GetForge().GetProvider() == rf.provider &&
			iss.GetForge().GetHost() == rf.host &&
			iss.GetRepo() == repo &&
			iss.GetNumber() == number {
			return iss, true
		}
	}
	return nil, false
}

// translateIssue strips owner attribution from the provider body before
// translating, then stamps provider+host+repo left zero by the translator.
func translateIssue(raw forge.Issue, rf resolvedForge, repo string) *compassv1.Issue {
	clean, author, ok := forge.StripOwner(raw.Body)
	raw.Body = clean
	var attr *compassv1.AgentAttribution
	if ok && author.AgentHandle != "" {
		attr = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
	}
	out := forge.TranslateIssue(raw, attr)
	out.Forge = &compassv1.ForgeRef{Provider: rf.provider, Host: rf.host}
	out.Repo = repo
	return out
}

// translatePR strips the owner header off a raw forge PR body to parse
// attribution (the canonical PR carries no body field — attribution rides in
// attr), translates, and stamps the coordinate.
func translatePR(raw forge.PullRequest, rf resolvedForge, repo string) *compassv1.PullRequest {
	_, author, ok := forge.StripOwner(raw.Body)
	var attr *compassv1.AgentAttribution
	if ok && author.AgentHandle != "" {
		attr = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
	}
	out := forge.TranslatePullRequest(raw, attr)
	out.Forge = &compassv1.ForgeRef{Provider: rf.provider, Host: rf.host}
	out.Repo = repo
	return out
}

// coordinateResult returns the recorded artifact arm on an F3 memo hit, even
// when a retry uses a different arm, so the original artifact kind is visible.
func coordinateResult(a store.AuthoredArtifact) *compassv1internal.ForgeCallResult {
	ref := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider(a.Provider), Host: a.Host}
	if a.Kind == store.ForgeArtifactKindPullRequest {
		return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_PullRequest{
			PullRequest: &compassv1.PullRequest{Forge: ref, Repo: a.Repo, Number: narrowForgeNumber(a.Number)},
		}}
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Issue{
		Issue: &compassv1.Issue{Forge: ref, Repo: a.Repo, Number: narrowForgeNumber(a.Number)},
	}}
}

// forgeOp carries provider and operation context, plus body size and limit for
// oversized-body errors.
type forgeOp struct {
	provider  string
	op        string
	bodyLen   int
	bodyLimit int
}

// mapForgeError maps provider faults to Connect codes; 422 preserves its
// message, and rate-limit errors carry a clamped RetryAfterMs hint.
func mapForgeError(err error, op forgeOp) *compassv1internal.ForgeCallError {
	if errors.Is(err, forge.ErrNoInstallation) {
		return artifactNotFound()
	}
	if errors.Is(err, forge.ErrBudgetExhausted) {
		fe := &compassv1internal.ForgeCallError{Code: connect.CodeResourceExhausted.String(), Message: err.Error()}
		if rle, ok := errors.AsType[*forge.RateLimitError](err); ok {
			// Clamp before the uint32 cast so clock skew reads as "no hint" and a huge hint saturates.
			ms := rle.RetryAfter.Milliseconds()
			switch {
			case ms < 0:
				ms = 0
			case ms > math.MaxUint32:
				ms = math.MaxUint32
			}
			fe.RetryAfterMs = uint32(ms)
		}
		return fe
	}
	if errors.Is(err, forge.ErrInstallationLookup) {
		return forgeErr(installationErrorCode(err), "forge: GitHub App installation lookup failed")
	}
	switch {
	case errors.Is(err, forge.ErrMalformedRepo):
		return forgeErr(connect.CodeInvalidArgument, "forge: repo must be owner/name")
	case errors.Is(err, forge.ErrBodyTooLarge):
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: body of %d bytes exceeds the %s limit of %d bytes once the owner header is reserved", op.bodyLen, op.provider, op.bodyLimit))
	case errors.Is(err, forge.ErrUnsupported):
		return forgeErr(connect.CodeUnimplemented, fmt.Sprintf("forge: operation %q is unsupported by provider %q", op.op, op.provider))
	}
	if se, ok := errors.AsType[*forge.StatusError](err); ok {
		switch se.Status {
		case 403, 404:
			return artifactNotFound()
		case 422:
			return forgeErr(connect.CodeInvalidArgument, se.Message)
		case 429:
			return &compassv1internal.ForgeCallError{Code: connect.CodeResourceExhausted.String(), Message: err.Error()}
		}
	}
	return forgeErr(connect.CodeInternal, err.Error())
}

// storeForgeError maps a store sentinel onto an in-band ForgeCallError so a
// store fault on the attribution/memo/record path renders like any tool failure.
func storeForgeError(err error) *compassv1internal.ForgeCallError {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return forgeErr(connect.CodeNotFound, err.Error())
	case errors.Is(err, store.ErrInvalidArgument):
		return forgeErr(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, store.ErrConflict):
		return forgeErr(connect.CodeAlreadyExists, err.Error())
	default:
		return forgeErr(connect.CodeInternal, err.Error())
	}
}

// forgeErr builds an in-band ForgeCallError using the Connect status token.
func forgeErr(code connect.Code, msg string) *compassv1internal.ForgeCallError {
	return &compassv1internal.ForgeCallError{Code: code.String(), Message: msg}
}

// artifactNotFound is the single not-found shape for missing, forbidden, and
// out-of-scope artifacts.
func artifactNotFound() *compassv1internal.ForgeCallError {
	return forgeErr(connect.CodeNotFound, "forge: artifact not found")
}

// forgeErrorResult wraps an in-band failure as the result's Error arm; transport
// remains successful so the agent can render the tool-level failure.
func forgeErrorResult(fe *compassv1internal.ForgeCallError) *compassv1internal.ForgeCallResult {
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Error{Error: fe}}
}

// narrowForgeNumber narrows a forge's uint64 artifact number to the canonical
// uint32, clamping at the ceiling rather than silently truncating the high bits
// (mirrors forge.narrowNumber). A real forge number fits uint32 in practice.
func narrowForgeNumber(n uint64) uint32 {
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
