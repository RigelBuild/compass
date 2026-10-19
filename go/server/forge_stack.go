//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/agentidentity"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	noIssueMessage   = "forge: a new pull request needs issue_ref; find an issue that fits with forge_list_issues, or create one with forge_create_issue, then submit again"
	mirrorOffMessage = "forge: the Server git mirror is not configured"
)

// stackRun carries one submit_stack call's resolved context across its entries.
type stackRun struct {
	s       *forgeService
	rf      resolvedForge
	id      callerIdentity
	caller  store.AccountID
	session string
	repo    string
	key     string
	bundled bool
	// pushed avoids repeating a push after foreign-base replay; intentIDs ties it to its claim.
	pushed    map[string]bool
	intentIDs map[string]string
	leases    forge.Leases
	links     map[string]*store.ForgeCoord
}

// foreignBase is a same-owner agent's open PR that the caller's line sits on.
type foreignBase struct {
	pr    forge.PullRequest
	agent store.AccountID
}

// submitStack pushes the agent's bundle and writes one PR per entry, bottom to
// top. The first failed entry stops the call; later entries are skipped.
func (s *forgeService) submitStack(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.SubmitStackRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := requireWriteKey(call); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := validateStack(req); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	if fe := checkStackDefaultBranch(ctx, rf, req.GetRepo(), req.GetEntries()); fe != nil {
		return forgeErrorResult(fe)
	}
	id, fe := s.resolveIdentity(ctx, caller, sessionID)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	entries := req.GetEntries()
	if len(entries) == 0 {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge: submit_stack needs at least one entry"))
	}
	if len(req.GetBundle()) > 0 && s.mirror == nil {
		return forgeErrorResult(forgeErr(connect.CodeFailedPrecondition, mirrorOffMessage))
	}
	r := &stackRun{s: s, rf: rf, id: id, caller: caller, session: sessionID, repo: req.GetRepo(), key: call.GetClientRequestId(), bundled: len(req.GetBundle()) > 0, pushed: map[string]bool{}, intentIDs: map[string]string{}, leases: forge.Leases{}, links: map[string]*store.ForgeCoord{}}
	out := make([]*compassv1internal.StackEntryResult, len(entries))
	prs := make([]forge.PullRequest, len(entries))
	// Every head must be the caller's or new before the mirror moves any ref.
	found, failed, fe := r.preflight(ctx, entries, prs)
	var foreign *foreignBase
	if fe == nil {
		if fe = r.applyBundle(ctx, req.GetBundle(), entries); fe != nil {
			return forgeErrorResult(fe)
		}
		failed, fe = r.requireBundleCommitIdentity(ctx, entries)
		if fe == nil {
			foreign, failed, fe = r.prepareStack(ctx, entries, found)
		}
	}
	if fe != nil {
		out[failed] = failedEntry(entries[failed].GetHeadRef(), fe)
	}
	if fe == nil {
		failed = r.ensureStack(ctx, entries, found, prs, out)
	}
	if fe == nil && failed < 0 {
		failed = r.restampStack(ctx, req, entries, prs, out, foreign)
	}
	finishStackResults(entries, out, failed)
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Stack{Stack: &compassv1internal.StackResult{Entries: out}}}
}

func (r *stackRun) prepareStack(ctx context.Context, entries []*compassv1internal.StackEntry, found []bool) (*foreignBase, int, *compassv1internal.ForgeCallError) {
	for i, entry := range entries {
		if found[i] {
			continue
		}
		localOID := ""
		if r.bundled {
			var err error
			localOID, err = r.s.mirror.RefHead(ctx, r.repo, "refs/heads/"+entry.GetHeadRef())
			if err != nil {
				return nil, i, forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: read bundled head %q: %v", entry.GetHeadRef(), err))
			}
		}
		if fe := r.requireOwnBranch(ctx, entry.GetHeadRef(), r.leases["refs/heads/"+entry.GetHeadRef()], localOID); fe != nil {
			return nil, i, fe
		}
	}
	foreign, fe := r.checkForeignBase(ctx, entries[0].GetBaseRef())
	if fe != nil {
		return nil, 0, fe
	}
	if foreign != nil {
		if fe := r.rebaseForeign(ctx, foreign, entries, found); fe != nil {
			return nil, 0, fe
		}
	}
	return foreign, -1, nil
}

func (r *stackRun) ensureStack(ctx context.Context, entries []*compassv1internal.StackEntry, found []bool, prs []forge.PullRequest, out []*compassv1internal.StackEntryResult) int {
	for i, entry := range entries {
		pr, created, fe := r.ensure(ctx, entry, stackBase(entries, i), prs[i], found[i])
		if fe != nil {
			out[i] = failedEntry(entry.GetHeadRef(), fe)
			return i
		}
		prs[i] = pr
		out[i] = &compassv1internal.StackEntryResult{
			HeadRef: entry.GetHeadRef(), PullRequest: translatePR(pr, r.rf, r.repo),
			Created: created, Authored: true, Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_DONE,
		}
	}
	return -1
}

func (r *stackRun) restampStack(ctx context.Context, req *compassv1internal.SubmitStackRequest, entries []*compassv1internal.StackEntry, prs []forge.PullRequest, out []*compassv1internal.StackEntryResult, foreign *foreignBase) int {
	lines := stackLines(foreign, entries, prs)
	if foreign != nil {
		if fe := r.restampForeign(ctx, foreign.pr.Number, lines); fe != nil {
			out[0] = failedEntry(entries[0].GetHeadRef(), fe)
			return 0
		}
	}
	for i, entry := range entries {
		pr, fe := r.restamp(ctx, entry, prs[i], stackBase(entries, i), lines, entry.GetHeadRef() == req.GetReadyHeadRef())
		if fe != nil {
			out[i] = failedEntry(entry.GetHeadRef(), fe)
			return i
		}
		out[i].PullRequest = translatePR(pr, r.rf, r.repo)
	}
	return -1
}

func finishStackResults(entries []*compassv1internal.StackEntry, out []*compassv1internal.StackEntryResult, failed int) {
	if failed >= 0 {
		for j := failed + 1; j < len(entries); j++ {
			out[j] = &compassv1internal.StackEntryResult{HeadRef: entries[j].GetHeadRef(), Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_SKIPPED}
		}
	}
	for j := range out {
		if out[j] == nil {
			out[j] = &compassv1internal.StackEntryResult{HeadRef: entries[j].GetHeadRef(), Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_SKIPPED}
		}
	}
}

// stackLines lists the line bottom to top, a foreign base first; a lone PR gets no block.
func stackLines(foreign *foreignBase, entries []*compassv1internal.StackEntry, prs []forge.PullRequest) []forge.StackLine {
	lines := make([]forge.StackLine, 0, len(entries)+1)
	if foreign != nil {
		lines = append(lines, forge.StackLine{Number: foreign.pr.Number, Title: foreign.pr.Title})
	}
	for i, e := range entries {
		title := prs[i].Title
		if e.GetReplaceText() {
			title = e.GetTitle()
		}
		lines = append(lines, forge.StackLine{Number: prs[i].Number, Title: title})
	}
	if len(lines) < 2 {
		return nil
	}
	return lines
}

// validateStack refuses a malformed line before any store or forge touch.
func validateStack(req *compassv1internal.SubmitStackRequest) *compassv1internal.ForgeCallError {
	entries := req.GetEntries()
	if len(entries) == 0 {
		return forgeErr(connect.CodeInvalidArgument, "forge: submit_stack needs at least one entry")
	}
	if entries[0].GetBaseRef() == "" {
		return forgeErr(connect.CodeInvalidArgument, "forge: the bottom entry needs base_ref")
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		head := e.GetHeadRef()
		if head == "" {
			return forgeErr(connect.CodeInvalidArgument, "forge: every entry needs head_ref")
		}
		if head == entries[0].GetBaseRef() {
			return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: head %q equals the bottom entry's base_ref", head))
		}
		if seen[head] {
			return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: head %q appears twice", head))
		}
		seen[head] = true
	}
	if ready := req.GetReadyHeadRef(); ready != "" && !seen[ready] {
		return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: ready_head_ref %q is not an entry", ready))
	}
	return nil
}

func checkStackDefaultBranch(ctx context.Context, rf resolvedForge, repo string, entries []*compassv1internal.StackEntry) *compassv1internal.ForgeCallError {
	defaultBranch, err := rf.author.DefaultBranch(ctx, repo)
	if err != nil {
		return mapForgeError(err, forgeOp{provider: rf.author.Name(), op: "get_default_branch"})
	}
	for _, entry := range entries {
		if entry.GetHeadRef() == defaultBranch {
			return forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: head %q is the repository's default branch", entry.GetHeadRef()))
		}
	}
	return nil
}

// stackBase chains each entry on the one below; the bottom takes its own base.
func stackBase(entries []*compassv1internal.StackEntry, i int) string {
	if i == 0 {
		return entries[0].GetBaseRef()
	}
	return entries[i-1].GetHeadRef()
}

// preflight finds each entry's PR and refuses any head that is not the caller's,
// or this call's own unrecorded create, before the mirror moves a ref.
func (r *stackRun) preflight(ctx context.Context, entries []*compassv1internal.StackEntry, prs []forge.PullRequest) ([]bool, int, *compassv1internal.ForgeCallError) {
	found := make([]bool, len(entries))
	// Each entry's key is claimed before anything else, so a concurrent call on
	// another target sees this claim or this call sees theirs; own PRs included.
	for i, e := range entries {
		if fe := r.claim(ctx, e.GetHeadRef()); fe != nil {
			return found, i, fe
		}
	}
	for i, e := range entries {
		head := e.GetHeadRef()
		pr, ok, err := r.rf.author.FindPullRequestByHead(ctx, r.repo, head)
		if err != nil {
			return found, i, mapForgeError(err, r.op(0))
		}
		if !ok {
			continue
		}
		if pr.HeadRef != head {
			return found, i, forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: lookup for head %q returned pull request #%d on %q", head, pr.Number, pr.HeadRef))
		}
		if pr.State != store.TransitionStateOpen {
			return found, i, forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: head %q has %s pull request #%d; sync first", head, pr.State, pr.Number))
		}
		if fe := r.requireOwn(ctx, pr, head); fe != nil {
			return found, i, fe
		}
		prs[i], found[i] = pr, true
	}
	for i, e := range entries {
		if found[i] {
			continue
		}
		if strings.TrimSpace(e.GetIssueRef()) == "" {
			return found, i, forgeErr(connect.CodeInvalidArgument, noIssueMessage)
		}
		link, fe := r.checkIssue(ctx, e.GetIssueRef())
		if fe != nil {
			return found, i, fe
		}
		r.links[e.GetHeadRef()] = link
	}
	return found, -1, nil
}

// claim holds the entry's derived key for this target and refuses one already
// used for another. A held row sends nothing; create promotes it before the write.
func (r *stackRun) claim(ctx context.Context, head string) *compassv1internal.ForgeCallError {
	t := r.target(head, "", nil)
	in, err := r.s.store.HoldWriteIntent(ctx, t.intent(uuid.NewString(), r.intentKey(head)))
	if err != nil {
		return storeForgeError(err)
	}
	if !sameWriteTarget(in, t) {
		return forgeErr(connect.CodeFailedPrecondition, writeOtherTarget)
	}
	r.intentIDs[head] = in.ID
	return nil
}

// applyBundle installs the entries' refs from the agent's bundle and keeps the
// fetch's leases. Each ref is pushed later, once its PR is known to be the caller's.
func (r *stackRun) applyBundle(ctx context.Context, bundle []byte, entries []*compassv1internal.StackEntry) *compassv1internal.ForgeCallError {
	if len(bundle) == 0 {
		return nil
	}
	heads := make([]string, len(entries))
	for i, e := range entries {
		heads[i] = "refs/heads/" + e.GetHeadRef()
	}
	leases, err := r.s.mirror.ApplyBundle(ctx, r.repo, bundle, heads)
	if err != nil {
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: apply bundle: %v", err))
	}
	maps.Copy(r.leases, leases)
	return nil
}

// requireBundleCommitIdentity keeps the line clean: each bundled tip must be the
// agent's own commit by author or committer email. Not a security boundary.
func (r *stackRun) requireBundleCommitIdentity(ctx context.Context, entries []*compassv1internal.StackEntry) (int, *compassv1internal.ForgeCallError) {
	if !r.bundled {
		return -1, nil
	}
	want := agentidentity.AgentCommitEmail(string(r.caller))
	for i, entry := range entries {
		head := entry.GetHeadRef()
		authorEmail, committerEmail, err := r.s.mirror.CommitIdentities(ctx, r.repo, "refs/heads/"+head)
		if err != nil {
			return i, forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: read commit identity for head %q: %v", head, err))
		}
		isAuthor := strings.EqualFold(authorEmail, want)
		isCommitter := strings.EqualFold(committerEmail, want)
		if isAuthor || isCommitter {
			continue
		}
		return i, forgeErr(connect.CodePermissionDenied, fmt.Sprintf(
			"forge: head %q's tip's author and committer are not this agent; the commit must be made by the agent itself via vcs_commit",
			head,
		))
	}
	return -1, nil
}

// checkForeignBase returns the base PR when another agent of the caller's owner
// opened it, and refuses a base PR any other owner opened.
func (r *stackRun) checkForeignBase(ctx context.Context, base string) (*foreignBase, *compassv1internal.ForgeCallError) {
	pr, found, err := r.rf.author.FindPullRequestByHead(ctx, r.repo, base)
	if err != nil {
		return nil, mapForgeError(err, r.op(0))
	}
	if !found || pr.State != store.TransitionStateOpen {
		return nil, nil
	}
	row, ok, fe := r.s.prAuthor(ctx, r.rf, r.repo, pr.Number)
	switch {
	case fe != nil:
		return nil, fe
	case ok && row.AgentAccountID == r.caller:
		return nil, nil
	case ok && row.OwnerUserID == r.id.ownerID:
		return &foreignBase{pr: pr, agent: row.AgentAccountID}, nil
	default:
		return nil, forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: base %q is pull request #%d, which this agent's owner did not create", base, pr.Number))
	}
}

// rebaseForeign replays the foreign bookmark and the caller's bookmarks onto the
// foreign PR's current base in one replay, then pushes every moved ref with leases.
// A conflict pushes nothing and asks the owning agent to resolve.
func (r *stackRun) rebaseForeign(ctx context.Context, f *foreignBase, entries []*compassv1internal.StackEntry, found []bool) *compassv1internal.ForgeCallError {
	if r.s.mirror == nil {
		return forgeErr(connect.CodeFailedPrecondition, mirrorOffMessage)
	}
	foreignRef := "refs/heads/" + f.pr.HeadRef
	oldHead, err := r.s.mirror.SeedRef(ctx, r.repo, foreignRef)
	if err != nil {
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: read base %s: %v", f.pr.HeadRef, err))
	}
	r.leases[foreignRef] = oldHead
	// Preflight already proved every entry is the caller's, so only own refs ride above the foreign one.
	line := []string{foreignRef}
	for i, e := range entries {
		ref := "refs/heads/" + e.GetHeadRef()
		// Without a bundle the caller's bookmarks are already on the forge.
		if !r.bundled {
			seeded, err := r.s.mirror.SeedRef(ctx, r.repo, ref)
			if err != nil {
				return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: read %s: %v", e.GetHeadRef(), err))
			}
			r.leases[ref] = seeded
			if !found[i] {
				if fe := r.requireOwnBranch(ctx, e.GetHeadRef(), seeded, ""); fe != nil {
					return fe
				}
			}
		}
		line = append(line, ref)
	}
	onto := "refs/remotes/origin/" + f.pr.BaseRef
	heads, conflict, err := r.s.mirror.Rebase(ctx, r.repo, line, onto)
	if err != nil {
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: rebase base %s: %v", f.pr.HeadRef, err))
	}
	if conflict {
		r.notify(ctx, f, fmt.Sprintf("Your bookmark %s (PR #%d) conflicts with %s, so I could not rebase my line onto it. Please rebase it onto %s, resolve the conflict, and push. I will submit again after that.", f.pr.HeadRef, f.pr.Number, f.pr.BaseRef, f.pr.BaseRef), oldHead)
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: base %s (pull request #%d) conflicts with %s; its agent was asked to resolve it", f.pr.HeadRef, f.pr.Number, f.pr.BaseRef))
	}
	if len(heads) == 0 {
		return nil
	}
	if err := r.s.mirror.Push(ctx, r.repo, heads, r.leases); err != nil {
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: push rebased line on %s: %v; sync and submit again", f.pr.HeadRef, err))
	}
	for ref := range heads {
		r.pushed[strings.TrimPrefix(ref, "refs/heads/")] = true
	}
	for _, e := range entries {
		head := e.GetHeadRef()
		if r.pushed[head] {
			r.markHeadPushed(ctx, r.intentIDs[head])
		}
	}
	r.notify(ctx, f, fmt.Sprintf("I rebased your bookmark %s (PR #%d) onto %s so my line could build on it. Old head %s, new head %s. Sync before you push again.", f.pr.HeadRef, f.pr.Number, f.pr.BaseRef, oldHead, heads[foreignRef]), heads[foreignRef])
	return nil
}

// notify records before delivery; failure cannot fail a completed rebase.
func (r *stackRun) notify(ctx context.Context, f *foreignBase, text, foreignHead string) {
	// Client request ids are unique per agent, so the sender scopes the tenant-wide notice key.
	key := string(r.caller) + ":" + r.key + ":notice:" + f.pr.HeadRef + ":" + foreignHead
	notice, err := r.s.store.RecordNotice(ctx, store.Notice{
		FromAccountID: r.caller,
		ToAccountID:   f.agent,
		RequestKey:    key,
		Text:          text,
	})
	if err != nil {
		slog.WarnContext(ctx, "forge: record foreign rebase notice failed", "pr", f.pr.Number, "error", err)
		return
	}
	if err := r.s.notifyAgent(ctx, r.caller, f.agent, notice.RequestKey, notice.Text); err != nil {
		slog.WarnContext(
			ctx,
			"forge: foreign rebase notice failed",
			"pr", f.pr.Number,
			"error", err,
		)
		if bumpErr := r.s.store.BumpNoticeAttempt(ctx, notice.ID); bumpErr != nil {
			slog.WarnContext(
				ctx,
				"forge: bump foreign rebase notice attempt failed",
				"pr", f.pr.Number,
				"error", bumpErr,
			)
		}
		return
	}
	if err := r.s.store.MarkNoticeDelivered(ctx, notice.ID); err != nil {
		slog.WarnContext(
			ctx,
			"forge: mark foreign rebase notice delivered failed",
			"pr", f.pr.Number,
			"error", err,
		)
	}
}

// restampForeign rewrites only the foreign PR's stack block. It re-reads the PR
// and sends the body alone, so its owner's title, base, and text edits stand.
func (r *stackRun) restampForeign(ctx context.Context, number uint64, lines []forge.StackLine) *compassv1internal.ForgeCallError {
	pr, err := r.rf.author.GetPullRequest(ctx, r.repo, number)
	if err != nil {
		return mapForgeError(err, r.op(0))
	}
	body := forge.StampStack(pr.Body, forge.RenderStack(lines, pr.Number))
	op := r.op(len(body))
	if r.rf.bodyLimit > 0 && len(body) > r.rf.bodyLimit {
		return mapForgeError(forge.ErrBodyTooLarge, op)
	}
	if body == pr.Body {
		return nil
	}
	if _, err := r.rf.author.UpdatePullRequest(ctx, r.repo, pr.Number, forge.UpdatePR{Body: body, BodyOnly: true}); err != nil {
		return mapForgeError(err, op)
	}
	return nil
}

// ensure creates a new entry's PR, or pushes the head of one preflight found. It
// writes no existing PR; restamp does that once the whole line is known.
func (r *stackRun) ensure(ctx context.Context, e *compassv1internal.StackEntry, base string, pr forge.PullRequest, found bool) (forge.PullRequest, bool, *compassv1internal.ForgeCallError) {
	if !found {
		return r.create(ctx, e, base)
	}
	if fe := r.push(ctx, e.GetHeadRef()); fe != nil {
		return forge.PullRequest{}, false, fe
	}
	if intentID := r.intentIDs[e.GetHeadRef()]; intentID != "" {
		r.markHeadPushed(ctx, intentID)
	}
	return pr, false, nil
}

// requireOwn accepts the caller's PR, and adopts an unrecorded one only when
// this call's pending intent names its head.
func (r *stackRun) requireOwn(ctx context.Context, pr forge.PullRequest, head string) *compassv1internal.ForgeCallError {
	row, ok, fe := r.s.prAuthor(ctx, r.rf, r.repo, pr.Number)
	switch {
	case fe != nil:
		return fe
	case !ok:
		return r.adopt(ctx, pr, head)
	case row.AgentAccountID == r.caller:
		return nil
	case row.OwnerUserID == r.id.ownerID:
		return forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: head %q is pull request #%d, which another agent created; only a foreign base is rebased", head, pr.Number))
	default:
		return forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: head %q is pull request #%d, which this agent's owner did not create", head, pr.Number))
	}
}

// adopt records an unrecorded PR an earlier attempt of this call created.
func (r *stackRun) adopt(ctx context.Context, pr forge.PullRequest, head string) *compassv1internal.ForgeCallError {
	in, ref, fe := r.s.beginWrite(ctx, r.intentKey(head), r.target(head, "", nil))
	if fe != nil {
		return fe
	}
	if ref != nil && ref.Number == pr.Number {
		return nil
	}
	if ref == nil {
		// No earlier attempt names this head; the fresh intent sends nothing, so end it.
		r.s.endUnusedIntent(ctx, in.ID)
	}
	return forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: head %q is pull request #%d, which this agent did not create", head, pr.Number))
}

// requireOwnBranch refuses to overwrite a PR-less remote branch without ownership evidence.
// A local match is safe because the subsequent push does not change the remote ref.
func (r *stackRun) requireOwnBranch(ctx context.Context, head, remoteOID, localOID string) *compassv1internal.ForgeCallError {
	if remoteOID == "" || remoteOID == localOID {
		return nil
	}
	owned, err := r.s.store.HasPushedPRHead(ctx, store.ForgeProvider(r.rf.provider), r.rf.host, r.repo, head, r.caller)
	if err != nil {
		return storeForgeError(err)
	}
	if !owned {
		return forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: head %q already exists on the forge with no pull request, and this agent did not push it", head))
	}
	return nil
}

func (r *stackRun) markHeadPushed(ctx context.Context, id string) {
	if !r.bundled {
		return
	}
	if err := r.s.store.MarkWriteIntentHeadPushed(ctx, id); err != nil {
		slog.WarnContext(ctx, "forge: mark pushed PR head", slog.String("intent", id), slog.Any("error", err))
	}
}

// endUnusedIntent fails a fresh intent that sent no create; a conflict means recovery already ended it.
func (s *forgeService) endUnusedIntent(ctx context.Context, id string) {
	if err := s.store.EndWriteIntent(ctx, id, store.WriteStateFailed); err != nil && !errors.Is(err, store.ErrConflict) {
		slog.WarnContext(ctx, "forge: end unused write intent", slog.String("intent", id), slog.Any("error", err))
	}
}

// create opens a draft PR for a new entry under a write intent, using its preflight-checked issue link.
func (r *stackRun) create(ctx context.Context, e *compassv1internal.StackEntry, base string) (forge.PullRequest, bool, *compassv1internal.ForgeCallError) {
	head := e.GetHeadRef()
	link := r.links[head]
	intent, prior, fe := r.s.beginWrite(ctx, r.intentKey(head), r.target(head, base, link))
	if fe != nil {
		return forge.PullRequest{}, false, fe
	}
	if prior != nil {
		pr, err := r.rf.author.GetPullRequest(ctx, r.repo, prior.Number)
		if err != nil {
			return forge.PullRequest{}, false, mapForgeError(err, r.op(0))
		}
		if pr.HeadRef != head {
			return forge.PullRequest{}, false, forgeErr(connect.CodeFailedPrecondition, writeOtherTarget)
		}
		if fe := r.push(ctx, head); fe != nil {
			return forge.PullRequest{}, false, fe
		}
		r.markHeadPushed(ctx, intent.ID)
		return pr, false, nil
	}
	// The head is pushed only once a fresh intent owns this create; a failed push sent no create.
	if fe := r.push(ctx, head); fe != nil {
		r.s.endUnusedIntent(ctx, intent.ID)
		return forge.PullRequest{}, false, fe
	}
	r.markHeadPushed(ctx, intent.ID)
	op := r.op(len(e.GetBody()))
	body, err := r.body(e.GetBody(), "")
	if err != nil {
		return forge.PullRequest{}, false, r.s.failWrite(ctx, intent.ID, err, op).GetError()
	}
	pr, err := r.rf.author.CreatePullRequest(ctx, r.repo, forge.CreatePR{Title: e.GetTitle(), Body: body, HeadRef: head, BaseRef: base, Draft: true})
	if err != nil {
		return forge.PullRequest{}, false, r.s.failWrite(ctx, intent.ID, err, op).GetError()
	}
	a := r.s.authoredArtifact(r.rf, r.id, r.caller, r.session, r.repo, store.ForgeArtifactKindPullRequest, pr.Number)
	if fe := r.s.recordPullRequest(ctx, intent.ID, a, pr, link); fe != nil {
		return forge.PullRequest{}, false, fe
	}
	return pr, true, nil
}

// checkIssue parses the user's issue ref and reads it: only an open issue
// passes. A tracker outage is retryable, so no PR is made against an unchecked issue.
func (r *stackRun) checkIssue(ctx context.Context, raw string) (*store.ForgeCoord, *compassv1internal.ForgeCallError) {
	ref, err := refs.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: issue_ref %q is not owner/repo#N or TEAM-N", raw))
	}
	in := &compassv1internal.PullRequestIssueLink{Number: ref.Number}
	if ref.Kind == refs.KindTracker {
		in.Forge = &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}
		in.Repo = ref.Key
	} else {
		in.Repo = ref.Owner + "/" + ref.Repo
	}
	link, fe := r.s.resolveIssueLink(r.rf, r.repo, in)
	if fe != nil {
		return nil, fe
	}
	// A non-nil input always resolves to a link; a nil one here is a Server bug.
	if link == nil {
		return nil, forgeErr(connect.CodeInternal, "forge: issue link did not resolve")
	}
	tracker, ok := r.s.providers.resolve(&compassv1.ForgeRef{Provider: compassv1.ForgeProvider(link.Provider), Host: link.Host})
	if !ok {
		return nil, forgeErr(connect.CodeNotFound, "forge: no provider configured for the issue's coordinate")
	}
	iss, err := tracker.author.GetIssue(ctx, link.Repo, link.Number)
	if err != nil {
		if se, ok := errors.AsType[*forge.StatusError](err); ok && (se.Status == 403 || se.Status == 404 || se.Status == 410) {
			return nil, forgeErr(connect.CodeNotFound, fmt.Sprintf("forge: issue %s not found", raw))
		}
		return nil, forgeErr(connect.CodeUnavailable, fmt.Sprintf("forge: could not check issue %s; retry later: %v", raw, err))
	}
	switch {
	case iss.IsPullRequest:
		return nil, forgeErr(connect.CodeInvalidArgument, fmt.Sprintf("forge: %s is a pull request, not an issue", raw))
	case iss.State != store.TransitionStateOpen:
		return nil, forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: issue %s is %s", raw, iss.State))
	}
	return link, nil
}

// restamp PATCHes only what differs: base, block, header, draft, and the text
// only on replace_text.
func (r *stackRun) restamp(ctx context.Context, e *compassv1internal.StackEntry, pr forge.PullRequest, base string, lines []forge.StackLine, ready bool) (forge.PullRequest, *compassv1internal.ForgeCallError) {
	title, text := pr.Title, existingText(pr.Body)
	if e.GetReplaceText() {
		title, text = e.GetTitle(), e.GetBody()
	}
	block := ""
	if len(lines) > 0 {
		block = forge.RenderStack(lines, pr.Number)
	}
	op := r.op(len(text))
	body, err := r.body(text, block)
	if err != nil {
		return forge.PullRequest{}, mapForgeError(err, op)
	}
	leaveDraft := ready && pr.Draft
	if title == pr.Title && body == pr.Body && base == pr.BaseRef && !leaveDraft {
		return pr, nil
	}
	updated, err := r.rf.author.UpdatePullRequest(ctx, r.repo, pr.Number, forge.UpdatePR{Title: title, Body: body, BaseRef: base, Ready: leaveDraft})
	if err != nil {
		return forge.PullRequest{}, mapForgeError(err, op)
	}
	return updated, nil
}

// existingText is a PR body without the Server's owner header and stack block.
func existingText(body string) string {
	clean, _, _ := forge.StripOwner(body)
	return forge.StripStack(clean)
}

// body stamps the owner header and the stack block onto agent text.
func (r *stackRun) body(text, block string) (string, error) {
	stamped, err := writeBody(r.rf, r.id.author, forge.StripStack(text), "", false)
	if err != nil {
		return "", err
	}
	out := forge.StampStack(stamped, block)
	if r.rf.bodyLimit > 0 && len(out) > r.rf.bodyLimit {
		return "", forge.ErrBodyTooLarge
	}
	return out, nil
}

// push sends the bundle's commit for head, leased on the forge's current head.
func (r *stackRun) push(ctx context.Context, head string) *compassv1internal.ForgeCallError {
	if !r.bundled || r.pushed[head] {
		return nil
	}
	ref := "refs/heads/" + head
	if err := r.s.mirror.Push(ctx, r.repo, map[string]string{ref: ref}, r.leases); err != nil {
		return forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: push %s: %v; sync and submit again", head, err))
	}
	r.pushed[head] = true
	return nil
}

func (r *stackRun) intentKey(head string) string { return r.key + ":" + head }

// op names this arm for error mapping; bodyLen feeds the oversized-body message.
func (r *stackRun) op(bodyLen int) forgeOp {
	return forgeOp{provider: r.rf.author.Name(), op: "submit_stack", bodyLen: bodyLen, bodyLimit: r.rf.bodyLimit}
}

func (r *stackRun) target(head, base string, issue *store.ForgeCoord) writeTarget {
	return writeTarget{rf: r.rf, id: r.id, caller: r.caller, session: r.session, repo: r.repo, kind: forge.WriteKindPullRequest, headRef: head, baseRef: base, issueLink: issue}
}

func failedEntry(head string, fe *compassv1internal.ForgeCallError) *compassv1internal.StackEntryResult {
	return &compassv1internal.StackEntryResult{HeadRef: head, Error: fe, Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_FAILED}
}

// prAuthor reads a PR's ownership row; ok=false means no row.
func (s *forgeService) prAuthor(ctx context.Context, rf resolvedForge, repo string, number uint64) (store.AuthoredArtifact, bool, *compassv1internal.ForgeCallError) {
	row, err := s.store.AuthoredArtifactByCoordinateFold(ctx, store.ForgeProvider(rf.provider), rf.host, repo, store.ForgeArtifactKindPullRequest, number)
	if errors.Is(err, store.ErrNotFound) {
		return store.AuthoredArtifact{}, false, nil
	}
	if err != nil {
		return store.AuthoredArtifact{}, false, storeForgeError(err)
	}
	return row, true, nil
}

// requirePRAuthor refuses a PR state change unless the caller created the PR.
func (s *forgeService) requirePRAuthor(ctx context.Context, caller store.AccountID, rf resolvedForge, repo string, number uint64) *compassv1internal.ForgeCallError {
	row, ok, fe := s.prAuthor(ctx, rf, repo, number)
	if fe != nil {
		return fe
	}
	if !ok || row.AgentAccountID != caller {
		return forgeErr(connect.CodePermissionDenied, fmt.Sprintf("forge: pull request #%d was not created by this agent", number))
	}
	return nil
}

// stackStatus reads each head's PR, its checks, and whether the caller authored it.
func (s *forgeService) stackStatus(ctx context.Context, caller store.AccountID, call *compassv1internal.ForgeCallRequest, req *compassv1internal.StackStatusRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	op := forgeOp{provider: rf.author.Name(), op: "stack_status"}
	out := make([]*compassv1internal.StackEntryResult, 0, len(req.GetHeadRefs()))
	for _, head := range req.GetHeadRefs() {
		pr, found, err := rf.author.FindPullRequestByHead(ctx, req.GetRepo(), head)
		if err != nil {
			out = append(out, failedEntry(head, mapForgeError(err, op)))
			continue
		}
		res := &compassv1internal.StackEntryResult{HeadRef: head, Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_DONE}
		if found {
			if pr.State == store.TransitionStateOpen {
				checks, err := rf.author.Checks(ctx, req.GetRepo(), pr.Number)
				if err != nil {
					out = append(out, failedEntry(head, mapForgeError(err, op)))
					continue
				}
				pr.Checks = checks
			}
			row, ok, fe := s.prAuthor(ctx, rf, req.GetRepo(), pr.Number)
			if fe != nil {
				out = append(out, failedEntry(head, fe))
				continue
			}
			res.Authored = ok && row.AgentAccountID == caller
			res.PullRequest = translatePR(pr, rf, req.GetRepo())
		}
		out = append(out, res)
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Stack{Stack: &compassv1internal.StackResult{Entries: out}}}
}

// retireHead closes the caller's PR for head, then deletes only that branch.
// A retry skips a close that already landed.
func (s *forgeService) retireHead(ctx context.Context, caller store.AccountID, call *compassv1internal.ForgeCallRequest, req *compassv1internal.RetireHeadRequest) *compassv1internal.ForgeCallResult {
	rf, fe := s.resolveTarget(call, req.GetRepo())
	if fe != nil {
		return forgeErrorResult(fe)
	}
	head := req.GetHeadRef()
	if head == "" {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge: head_ref is required"))
	}
	if fe := s.requireForgeScope(ctx, caller, rf, req.GetRepo()); fe != nil {
		return forgeErrorResult(fe)
	}
	if s.mirror == nil {
		return forgeErrorResult(forgeErr(connect.CodeFailedPrecondition, mirrorOffMessage))
	}
	op := forgeOp{provider: rf.author.Name(), op: "retire_head"}
	pr, found, err := rf.author.FindPullRequestByHead(ctx, req.GetRepo(), head)
	if err != nil {
		return forgeErrorResult(mapForgeError(err, op))
	}
	if !found {
		return forgeErrorResult(forgeErr(connect.CodeNotFound, fmt.Sprintf("forge: no pull request for head %q", head)))
	}
	if fe := s.requirePRAuthor(ctx, caller, rf, req.GetRepo(), pr.Number); fe != nil {
		return forgeErrorResult(fe)
	}
	if pr.State == store.TransitionStateOpen {
		closed, err := rf.author.TransitionPullRequestState(ctx, req.GetRepo(), pr.Number, forge.TransitionState{State: store.TransitionStateClosed})
		if err != nil {
			return forgeErrorResult(mapForgeError(err, op))
		}
		if fe := s.rememberTransition(ctx, rf, caller, req.GetRepo(), store.ForgeArtifactKindPullRequest, pr.Number, store.TransitionStateClosed); fe != nil {
			return forgeErrorResult(fe)
		}
		pr = closed
	}
	// The fetch sets the delete's lease; a branch it no longer sees was deleted by an earlier attempt.
	ref := "refs/heads/" + head
	leases, err := s.mirror.ApplyBundle(ctx, req.GetRepo(), nil, []string{ref})
	if err != nil {
		return forgeErrorResult(forgeErr(connect.CodeUnavailable, fmt.Sprintf("forge: fetch %s: %v", req.GetRepo(), err)))
	}
	if err := s.mirror.Push(ctx, req.GetRepo(), map[string]string{ref: ""}, leases); err != nil {
		return forgeErrorResult(forgeErr(connect.CodeFailedPrecondition, fmt.Sprintf("forge: delete branch %s: %v", head, err)))
	}
	return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Stack{Stack: &compassv1internal.StackResult{
		Entries: []*compassv1internal.StackEntryResult{{
			HeadRef: head, PullRequest: translatePR(pr, rf, req.GetRepo()), Authored: true,
			Status: compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_DONE,
		}},
	}}}
}
