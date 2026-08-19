//go:build unix

// The agent-initiated forge-write leg: forgeService implements the
// runnerhub.ForgeCaller seam (relay_forge.go, T5 — the interface DEFINITION
// lands there; this slice implements it) the RunnerHub delegates a
// resolved-caller forge call into. It is the forge sibling of boardService
// (board.go) and shares its trust model exactly, plus it is the ONE place
// agent-authored forge artifacts get owner-attribution stamped (DL-050), the
// create-idempotency memo consulted (F3), the credential role selected
// (author vs reviewer, F1), and a provider/transport fault flattened into one
// in-band ForgeCallError.
//
// Trust model (mirrors boardService). The caller AccountID is resolved
// Server-side by the hub from its own session binding and passed in; the Runner
// never asserts it. sessionID rides along because the DL-050 owner stamp
// interpolates it into the header (owner.go:64 grammar). Per Resolved decision 2
// (MVP, single-trust-domain) the caller is recorded for attribution but NO scope
// rejection ships (A8).
//
// The in-band vs Connect split (mirrors boardCallError). A tool-level failure — a
// forge 4xx/5xx, an over-limit body, an unconfigured coordinate — is returned
// IN-BAND on the ForgeCallResult_Error arm the agent renders. ONLY a malformed
// request (an unset oneof arm) or a missing caller resolution is a Connect error
// the transport carries (A1). The oneof dispatch lives HERE (not in the hub as
// executeBoardCall does) because forge has ten arms plus stamping, provider
// selection, and store access, so the ForgeCaller seam is single-method and the
// domain shape stays behind it (design.md:176-194).
package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/forge"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// forgeCoordinate is the registry key: the wire forge enum + host. A repo does
// NOT enter the key — one credential pair serves every repo on a coordinate
// (DL-091 multi-forge disambiguation is provider+host).
type forgeCoordinate struct {
	provider compassv1.ForgeProvider
	host     string
}

// forgeProviderRoles carries the TWO credential-role clients a coordinate holds
// (F1): the AUTHOR client every ordinary write dispatches on, and the REVIEWER
// client submit_review dispatches on — a distinct GitHub identity so an agent
// approving a PR it authored is a different account, dissolving the
// author-approving-own-PR rejection at the credential layer, not with a code
// special-case.
type forgeProviderRoles struct {
	author   forge.Provider
	reviewer forge.Provider
}

// forgeProviderRegistry maps a ForgeRef coordinate to its author+reviewer
// providers. A nil/unset ForgeRef (or an UNSPECIFIED provider) resolves the
// default coordinate; an empty host on an otherwise-set ref resolves the
// provider's default host (A3). T8 populates it at serve assembly with the real
// author/reviewer GitHub clients (and Linear); the tests drive it with fakes.
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
func (r *forgeProviderRegistry) register(coord forgeCoordinate, author, reviewer forge.Provider, isDefault bool) {
	r.entries[coord] = forgeProviderRoles{author: author, reviewer: reviewer}
	if _, ok := r.defaultHost[coord.provider]; !ok {
		r.defaultHost[coord.provider] = coord.host
	}
	if isDefault {
		r.def = coord
	}
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

// forgeStore is the narrow store surface the chokepoint needs: resolve the
// caller's attribution (agent handle + owning user), the F3 idempotency-memo
// lookup, and the DL-055 ownership-row + memo write. Satisfied by *store.Store;
// a narrow interface (the issueStore / CommsCaller pattern) so the
// stamp/dedup/record ordering is provable in the default lane against a fake,
// not only behind the pgtest tag.
type forgeStore interface {
	GetAccount(ctx context.Context, id store.AccountID) (store.Account, error)
	AuthoredArtifactByRequestID(ctx context.Context, agent store.AccountID, clientRequestID string) (store.AuthoredArtifact, bool, error)
	RecordAuthoredArtifact(ctx context.Context, a store.AuthoredArtifact) error
}

// forgeService is the ForgeCaller implementation and the DL-050 write
// chokepoint. It holds the narrow store (attribution + ownership index), the
// issue projection (the OQ-A tracked-read source), the provider registry
// (author/reviewer clients per coordinate), and an injectable clock (the DL-055
// row's created_at).
type forgeService struct {
	store     forgeStore
	issueBrd  *board.IssueProjection
	providers *forgeProviderRegistry
	now       func() time.Time
}

// newForgeService constructs the forge caller over the store, the issue
// projection (tracked reads), and the provider registry. Wired at serve
// assembly with hub.SetForgeCaller after all three exist (T8), breaking the
// hub<->forgeService construction cycle exactly as newBoardService does
// (sinks.go).
func newForgeService(st *store.Store, issueBrd *board.IssueProjection, providers *forgeProviderRegistry) *forgeService { //nolint:unused // wired at serve assembly by T8 via hub.SetForgeCaller (mirrors newBoardService, which sinks.go already calls); this slice defines the chokepoint, T8 mounts it.
	return &forgeService{store: st, issueBrd: issueBrd, providers: providers, now: time.Now}
}

// ExecuteForgeCallAsAccount is the ForgeCaller entry point and the per-arm
// dispatch. It fails a missing caller closed as a Connect error (never a stale
// account), and an unset/unknown oneof arm as CodeInvalidArgument (the
// executeBoardCall default-arm convention, relay_board.go:115-119) — a malformed
// request, not a tool failure. Every other outcome, success or tool fault, is
// returned in-band on the ForgeCallResult (the caller stamps call_id).
//
// The signature is byte-exact with the T5 runnerhub.ForgeCaller definition; T8
// adds `var _ runnerhub.ForgeCaller = (*forgeService)(nil)` once it can import
// the interface (it does not exist in this slice's workspace, so the assertion
// cannot compile here yet).
func (s *forgeService) ExecuteForgeCallAsAccount(
	ctx context.Context,
	caller store.AccountID,
	sessionID string,
	call *compassv1internal.ForgeCallRequest,
) (*compassv1internal.ForgeCallResult, error) {
	if caller == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("forge: caller account is required"))
	}

	switch c := call.GetCall().(type) {
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
	case *compassv1internal.ForgeCallRequest_GetIssue:
		return s.getIssue(ctx, call, c.GetIssue), nil
	case *compassv1internal.ForgeCallRequest_ListIssues:
		return s.listIssues(ctx, call, c.ListIssues), nil
	case *compassv1internal.ForgeCallRequest_GetPullRequest:
		return s.getPullRequest(ctx, call, c.GetPullRequest), nil
	case *compassv1internal.ForgeCallRequest_Subscribe:
		// Row writes on agent_forge_subscriptions are the poll-driver lane's
		// surface (DL-053/A8); no store writer for that table exists in this
		// slice, so the arm is unimplemented rather than inventing a store
		// method (see summary: subscription-writer dependency).
		return forgeErrorResult(forgeErr(connect.CodeUnimplemented, "forge: subscribe is not wired (no agent_forge_subscriptions store writer yet)")), nil
	case *compassv1internal.ForgeCallRequest_Unsubscribe:
		return forgeErrorResult(forgeErr(connect.CodeUnimplemented, "forge: unsubscribe is not wired (no agent_forge_subscriptions store writer yet)")), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("forge: call has no operation variant set"))
	}
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

// dedup is the F3 idempotency-memo lookup: the artifact the agent authored under
// clientRequestID, or ok=false on a miss. An empty key is always a miss (it is
// never stored), short-circuited so it never touches the store.
func (s *forgeService) dedup(ctx context.Context, caller store.AccountID, clientRequestID string) (store.AuthoredArtifact, bool, *compassv1internal.ForgeCallError) {
	if clientRequestID == "" {
		return store.AuthoredArtifact{}, false, nil
	}
	hit, ok, err := s.store.AuthoredArtifactByRequestID(ctx, caller, clientRequestID)
	if err != nil {
		return store.AuthoredArtifact{}, false, storeForgeError(err)
	}
	return hit, ok, nil
}

// record writes the DL-055 ownership-index row AND the F3 memo in the ONE
// ordered step RecordAuthoredArtifact performs, strictly AFTER a create's forge
// success — so a rejected write leaves no row and a stamp failure never orphans
// one (#995 design.md:2487-2488). Only create_issue / create_pull_request mint
// an artifact coordinate (kind issue|pull_request); the comment/review arms have
// no coordinate to record, so they never reach here (F3 is create-only per the
// frozen ruling, design.md:149-155/971-982).
func (s *forgeService) record(ctx context.Context, rf resolvedForge, id callerIdentity, caller store.AccountID, sessionID, clientRequestID, repo string, kind store.ForgeArtifactKind, number uint64) *compassv1internal.ForgeCallError {
	err := s.store.RecordAuthoredArtifact(ctx, store.AuthoredArtifact{
		Provider:        store.ForgeProvider(rf.provider),
		Host:            rf.host,
		Repo:            repo,
		Kind:            kind,
		Number:          number,
		AgentAccountID:  caller,
		OwnerUserID:     id.ownerID,
		SessionID:       sessionID,
		ClientRequestID: clientRequestID,
		CreatedAtUnixMS: s.now().UnixMilli(),
	})
	if err != nil {
		return storeForgeError(err)
	}
	return nil
}

// createIssue is a create arm: resolve target, F3 dedup (a hit returns the
// recorded coordinate with ZERO provider calls), then on a miss stamp → author
// client write → flatten → DL-055 row+memo → result.
func (s *forgeService) createIssue(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CreateIssueRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if hit, ok, fe := s.dedup(ctx, caller, call.GetClientRequestId()); fe != nil {
		return forgeErrorResult(fe)
	} else if ok {
		return coordinateResult(hit)
	}

	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	body, err := forge.StampOwner(req.GetBody(), id.author, rf.bodyLimit)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "create_issue", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit}))
	}
	iss, err := rf.author.CreateIssue(ctx, req.GetRepo(), forge.CreateIssue{Title: req.GetTitle(), Body: body, Labels: req.GetLabels()})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "create_issue"}))
	}
	if fe := s.record(ctx, rf, id, caller, sessionID, call.GetClientRequestId(), req.GetRepo(), store.ForgeArtifactKindIssue, iss.Number); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Issue{Issue: translateIssue(iss, rf, req.GetRepo())},
	}
}

// createPullRequest is the PR-create twin of createIssue (author client;
// F3-deduped; DL-055 row on success).
func (s *forgeService) createPullRequest(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CreatePullRequestRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if hit, ok, fe := s.dedup(ctx, caller, call.GetClientRequestId()); fe != nil {
		return forgeErrorResult(fe)
	} else if ok {
		return coordinateResult(hit)
	}

	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	body, err := forge.StampOwner(req.GetBody(), id.author, rf.bodyLimit)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "create_pull_request", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit}))
	}
	pr, err := rf.author.CreatePullRequest(ctx, req.GetRepo(), forge.CreatePR{
		Title:   req.GetTitle(),
		Body:    body,
		HeadRef: req.GetHeadRef(),
		BaseRef: req.GetBaseRef(),
		Draft:   req.GetDraft(),
	})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "create_pull_request"}))
	}
	if fe := s.record(ctx, rf, id, caller, sessionID, call.GetClientRequestId(), req.GetRepo(), store.ForgeArtifactKindPullRequest, pr.Number); fe != nil {
		return forgeErrorResult(fe)
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_PullRequest{PullRequest: translatePR(pr, rf, req.GetRepo())},
	}
}

// commentOnIssue stamps the comment body (author client) and returns the write
// ack. A comment mints no artifact coordinate, so it is neither F3-deduped nor
// DL-055-recorded (the store index has no comment kind).
func (s *forgeService) commentOnIssue(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CommentOnIssueRequest) *compassv1internal.ForgeCallResult { //nolint:dupl // deliberate parallel of commentOnPullRequest: two distinct proto arms (issue vs PR comment) with distinct result variants (IssueComment vs PrComment); the shared prefix resolves rf in-body, so a closure-extracted helper reads worse than the explicit parallel.
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	body, err := forge.StampOwner(req.GetBody(), id.author, rf.bodyLimit)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "comment_on_issue", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit}))
	}
	cm, err := rf.author.CommentOnIssue(ctx, req.GetRepo(), req.GetIssueNumber(), body)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "comment_on_issue"}))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: &compassv1internal.CommentRef{Url: cm.URL, CommentId: cm.ID}},
	}
}

// commentOnPullRequest is the PR-conversation-comment twin of commentOnIssue.
func (s *forgeService) commentOnPullRequest(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.CommentOnPullRequestRequest) *compassv1internal.ForgeCallResult { //nolint:dupl // deliberate parallel of commentOnIssue (see its note): distinct proto arm + result variant, explicit parallel over a closure helper.
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	body, err := forge.StampOwner(req.GetBody(), id.author, rf.bodyLimit)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "comment_on_pull_request", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit}))
	}
	cm, err := rf.author.CommentOnPullRequest(ctx, req.GetRepo(), req.GetPullNumber(), body)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "comment_on_pull_request"}))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_PrComment{PrComment: &compassv1internal.CommentRef{Url: cm.URL, CommentId: cm.ID}},
	}
}

// submitReview stamps the top-level review body normally (author identity) but
// dispatches on the REVIEWER client (F1). Every inline review-comment body is
// STRIPPED of any owner-header block and NEVER stamped (A6): ingestion parses
// owner headers out of comment bodies, so a hand-written header in an unstamped
// inline body would otherwise impersonate another agent. A review mints no
// artifact coordinate, so it is neither F3-deduped nor DL-055-recorded.
func (s *forgeService) submitReview(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.SubmitReviewRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	body, err := forge.StampOwner(req.GetBody(), id.author, rf.bodyLimit)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.reviewer.Name(), op: "submit_review", bodyLen: len(req.GetBody()), bodyLimit: rf.bodyLimit}))
	}

	comments := make([]forge.ReviewCommentInput, 0, len(req.GetComments()))
	for _, c := range req.GetComments() {
		// STRIP-not-stamp (A6): discard the parsed display claim, keep the clean
		// body. StripOwner removes every owner block regardless of forge/mangle.
		clean, _, _ := forge.StripOwner(c.GetBody())
		comments = append(comments, forge.ReviewCommentInput{
			Path: c.GetPath(),
			Line: c.GetLine(),
			Side: c.GetSide(),
			Body: clean,
		})
	}

	sr, err := rf.reviewer.SubmitReview(ctx, req.GetRepo(), req.GetPullNumber(), forge.SubmitReview{
		Verdict:  req.GetVerdict(),
		Body:     body,
		Comments: comments,
	})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.reviewer.Name(), op: "submit_review"}))
	}
	return &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_Review{Review: &compassv1internal.ReviewRef{Url: sr.URL, ReviewId: sr.ID, Verdict: sr.Verdict}},
	}
}

// getIssue answers from the issue projection for a TRACKED artifact (OQ-A), else
// composes a live author-client fetch + TranslateIssue. Read bodies are stripped
// of the owner header (never stamped) — provider truth, not attribution.
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

// getPullRequest composes a live author-client fetch + TranslatePullRequest. The
// projection holds only issues, so a PR has no tracked source; every PR read is
// live (see summary: tracked-PR-read follow-up seam).
func (s *forgeService) getPullRequest(ctx context.Context, call *compassv1internal.ForgeCallRequest, req *compassv1internal.GetPullRequestRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	pr, err := rf.author.GetPullRequest(ctx, req.GetRepo(), req.GetPullNumber())
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
	raws, err := rf.author.ListIssues(ctx, req.GetRepo(), forge.IssueFilter{State: req.GetState(), Labels: req.GetLabels()})
	if err != nil {
		return forgeErrorResult(mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "list_issues"}))
	}
	resp := &compassv1internal.ListIssuesResponse{Issues: make([]*compassv1.Issue, 0, len(raws))}
	for _, iss := range raws {
		resp.Issues = append(resp.Issues, translateIssue(iss, rf, req.GetRepo()))
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Issues{Issues: resp}}
}

// trackedIssue returns the projection's canonical Issue at the coordinate if the
// artifact is tracked (ingested), matching on provider+host+repo+number. The
// scan is O(n) over the snapshot (the projection carries no by-coordinate
// index); acceptable at the MVP's agent-read volume, a keyed lookup is the
// follow-up if it ever matters.
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

// translateIssue strips the owner header off a raw forge issue's body (read
// bodies never carry an owner-attribution header on the wire — provider truth, DL-050),
// parses the display attribution, translates to the canonical Issue, and stamps
// the forge coordinate the pure translator leaves zero (mirrors
// ingest.translateOne).
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

// coordinateResult builds the F3 memo-hit result: the recorded coordinate as the
// arm matching the STORED kind — a create retried with the same
// client_request_id but a different arm returns the originally-recorded artifact
// (and its kind) so the mismatch is observable to the caller (F3 ruling).
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

// forgeOp carries the context the single error-mapping function needs to name a
// fault: the provider + operation (unsupported), and the body size + limit (an
// over-limit body names the overage).
type forgeOp struct {
	provider  string
	op        string
	bodyLen   int
	bodyLimit int
}

// mapForgeError is the SINGLE source flattening a stamp/provider/transport fault
// onto an in-band ForgeCallError (design.md:700-713): ErrBodyTooLarge →
// invalid_argument naming the overage; *StatusError{403} ≡ *StatusError{404} →
// byte-identical not_found (the #995 flattening — the message is fixed, never the
// forge's, so 403 and 404 are indistinguishable); *StatusError{422} →
// invalid_argument carrying the forge's validation message; ErrUnsupported →
// unimplemented naming provider+op; ErrBudgetExhausted / *StatusError{429} →
// resource_exhausted; everything else → internal. NOTE: ForgeCallError.RetryAfterMs
// is currently always 0 — neither forge.ErrBudgetExhausted (a bare sentinel) nor
// forge.StatusError (only {Status, Message}) surfaces the forge's Retry-After /
// reset hint, so there is nothing to populate it from yet (cross-slice follow-up:
// the provider error surface must carry the reset value before retry_after_ms can
// be honored).
func mapForgeError(err error, op forgeOp) *compassv1internal.ForgeCallError {
	switch {
	case errors.Is(err, forge.ErrBodyTooLarge):
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf(
			"forge: body of %d bytes exceeds the %s limit of %d bytes once the owner header is reserved",
			op.bodyLen, op.provider, op.bodyLimit))
	case errors.Is(err, forge.ErrUnsupported):
		return forgeErr(connect.CodeUnimplemented, fmt.Sprintf("forge: operation %q is unsupported by provider %q", op.op, op.provider))
	case errors.Is(err, forge.ErrBudgetExhausted):
		return &compassv1internal.ForgeCallError{Code: connect.CodeResourceExhausted.String(), Message: err.Error()}
	}
	var se *forge.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case 403, 404:
			return forgeErr(connect.CodeNotFound, "forge: artifact not found")
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

// forgeErr builds an in-band ForgeCallError with the Connect status token as its
// code (mirrors boardCallError: code = connect.CodeOf(err).String()).
func forgeErr(code connect.Code, msg string) *compassv1internal.ForgeCallError {
	return &compassv1internal.ForgeCallError{Code: code.String(), Message: msg}
}

// forgeErrorResult wraps an in-band ForgeCallError as the Error arm of a
// ForgeCallResult — a tool-level failure the agent renders, with the transport
// intact (never a Connect error).
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
