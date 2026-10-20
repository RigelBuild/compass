//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	writeSweepInterval = time.Minute
	// writeSweepMinAge leaves a create in flight alone until it has had time to land.
	writeSweepMinAge = time.Minute
	// writeLostAfter is how long the sweep looks for an object before giving up.
	writeLostAfter   = 24 * time.Hour
	writeSweepLimit  = 100
	writePRClockSkew = time.Minute

	writePendingMessage = "pending: the Server is still checking the forge; retry later"
	writeLostMessage    = "the create's outcome is unknown; check the forge before creating it again"
	writeOtherTarget    = "forge: client_request_id was used for another target"
)

// writeTarget names one create made for an agent.
type writeTarget struct {
	rf      resolvedForge
	id      callerIdentity
	caller  store.AccountID
	session string
	repo    string
	kind    forge.WriteKind
	parent  uint64
	headRef string
	baseRef string
	// defaultBase marks a base resolved from the repo default, which may move between same-key retries.
	defaultBase bool
	issueLink   *store.ForgeCoord
}

// beginWrite records the create's intent before any forge call. A non-nil ref
// is an earlier attempt's object; a non-nil error ends the call. Otherwise the
// caller owns a fresh attempt and calls the forge.
func (s *forgeService) beginWrite(ctx context.Context, key string, t writeTarget) (store.WriteIntent, *forge.WrittenRef, *compassv1internal.ForgeCallError) {
	fresh := uuid.NewString()
	in, err := s.store.RecordWriteIntent(ctx, t.intent(fresh, key))
	if err != nil {
		return store.WriteIntent{}, nil, storeForgeError(err)
	}
	if in.ID == fresh {
		return in, nil, nil
	}
	// The store keys an intent on agent and request key only, so a reused key must not replay another target's object.
	if !sameWriteTarget(in, t) {
		return in, nil, forgeErr(connect.CodeFailedPrecondition, writeOtherTarget)
	}
	switch in.State {
	case store.WriteStateHeld:
		// A stack preflight reserved this key for this target and sent nothing; the create is now ours to send.
		if err := s.store.PromoteWriteIntent(ctx, t.intent(in.ID, key)); err != nil {
			return in, nil, storeForgeError(err)
		}
		in.State = store.WriteStatePending
		in.BaseRef = t.baseRef
		in.IssueLink = t.issueLink
		return in, nil, nil
	case store.WriteStateDone:
		ref, fe := intentRef(in)
		return in, ref, fe
	case store.WriteStateLost:
		return in, nil, forgeErr(connect.CodeFailedPrecondition, writeLostMessage)
	case store.WriteStatePending:
		// An earlier attempt may still be in flight, so look for its object; never resend.
		ref, pr, found, err := s.findWrite(ctx, in)
		if err != nil || !found {
			return in, nil, forgeErr(connect.CodeUnavailable, writePendingMessage)
		}
		if fe := s.completeFound(ctx, in, ref, pr); fe != nil {
			return in, nil, fe
		}
		return in, &ref, nil
	case store.WriteStateFailed:
		// The store never replays a failed attempt; treat one as a fault.
		return in, nil, forgeErr(connect.CodeInternal, "forge: write intent replayed a failed attempt")
	default:
		return in, nil, forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent in unknown state %q", in.State))
	}
}

// intent is the row beginWrite and a stack hold record for this target.
func (t writeTarget) intent(id, key string) store.WriteIntent {
	return store.WriteIntent{
		ID:             id,
		Provider:       store.ForgeProvider(t.rf.provider),
		Host:           t.rf.host,
		Repo:           t.repo,
		Kind:           t.kind,
		Parent:         t.parent,
		HeadRef:        t.headRef,
		BaseRef:        t.baseRef,
		BaseIsDefault:  t.defaultBase,
		IssueLink:      t.issueLink,
		AgentAccountID: t.caller,
		OwnerUserID:    t.id.ownerID,
		SessionID:      t.session,
		RequestKey:     key,
	}
}

// sameWriteTarget compares a replayed intent with this call's target; repo case folds as the store does.
func sameWriteTarget(in store.WriteIntent, t writeTarget) bool {
	provider := store.ForgeProvider(t.rf.provider)
	fold := func(repo string) string { return store.ForgeCoord{Provider: provider, Repo: repo}.Normalized().Repo }
	if in.Provider != provider || in.Host != t.rf.host || fold(in.Repo) != fold(t.repo) ||
		in.Kind != t.kind || in.Parent != t.parent || in.HeadRef != t.headRef || in.AgentAccountID != t.caller {
		return false
	}
	if t.kind == forge.WriteKindPullRequest {
		bothBasesDefault := in.BaseIsDefault && t.defaultBase
		basesDiffer := in.BaseRef != "" && t.baseRef != "" && in.BaseRef != t.baseRef
		if (!bothBasesDefault && basesDiffer) ||
			(t.issueLink != nil && in.IssueLink != nil && !sameForgeCoord(in.IssueLink, t.issueLink)) {
			return false
		}
	}
	return true
}
func sameForgeCoord(a, b *store.ForgeCoord) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Normalized() == b.Normalized()
}

// requireWriteKey refuses a create with no idempotency key: the intent cannot exist without one.
func requireWriteKey(call *compassv1internal.ForgeCallRequest) *compassv1internal.ForgeCallError {
	if call.GetClientRequestId() == "" {
		return forgeErr(connect.CodeInvalidArgument, "forge: client_request_id is required for a create")
	}
	return nil
}

// writeBody strips agent-written markers, stamps the owner header, and on
// GitHub appends this intent's marker so recovery can find the object.
func writeBody(rf resolvedForge, author forge.Author, body, intentID string, marked bool) (string, error) {
	clean := forge.StripWriteMarkers(body)
	if !marked || rf.provider != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
		return forge.StampOwner(clean, author, rf.bodyLimit)
	}
	limit := rf.bodyLimit
	if limit > 0 {
		limit -= len(forge.WithWriteMarker("", intentID))
	}
	stamped, err := forge.StampOwner(clean, author, limit)
	if err != nil {
		return "", err
	}
	return forge.WithWriteMarker(stamped, intentID), nil
}

// failWrite maps a create error. Only a refusal ends the attempt as failed;
// any other error may hide a landed create, so the intent stays pending.
func (s *forgeService) failWrite(ctx context.Context, intentID string, err error, op forgeOp) *compassv1internal.ForgeCallResult {
	if forgeRefused(err) {
		// A conflict means recovery already resolved the attempt; that outcome stands.
		if endErr := s.store.EndWriteIntent(ctx, intentID, store.WriteStateFailed); endErr != nil && !errors.Is(endErr, store.ErrConflict) {
			slog.WarnContext(ctx, "forge: mark write intent failed", slog.String("intent", intentID), slog.Any("error", endErr))
		}
	}
	return forgeErrorResult(mapForgeError(err, op))
}

// forgeRefused reports whether no object can exist: the forge answered 4xx
// (not 429), or the request was never sent.
func forgeRefused(err error) bool {
	if errors.Is(err, forge.ErrBodyTooLarge) || errors.Is(err, forge.ErrUnsupported) {
		return true
	}
	if errors.Is(err, forge.ErrBudgetExhausted) {
		return false
	}
	se, ok := errors.AsType[*forge.StatusError](err)
	return ok && se.Status >= 400 && se.Status < 500 && se.Status != 408 && se.Status != 429
}

// completeWrite marks a fresh attempt done. A conflict means the sweep already
// recorded the same object, so the create still succeeded.
func (s *forgeService) completeWrite(ctx context.Context, intentID string, artifact *store.AuthoredArtifact, forgeKey, forgeURL string) *compassv1internal.ForgeCallError {
	if err := s.store.CompleteWriteIntent(ctx, intentID, artifact, forgeKey, forgeURL); err != nil && !errors.Is(err, store.ErrConflict) {
		return storeForgeError(err)
	}
	return nil
}

// findWrite looks for the object a pending intent may have created: a PR by
// its head, anything else by the intent's marker or id.
// A found PR is returned too.
func (s *forgeService) findWrite(ctx context.Context, in store.WriteIntent) (forge.WrittenRef, *forge.PullRequest, bool, error) {
	rf, ok := s.providers.resolve(&compassv1.ForgeRef{Provider: compassv1.ForgeProvider(in.Provider), Host: in.Host})
	if !ok {
		return forge.WrittenRef{}, nil, false, fmt.Errorf("forge: no provider configured for %d/%s", in.Provider, in.Host)
	}
	switch in.Kind {
	case forge.WriteKindPullRequest:
		pr, found, err := rf.author.FindPullRequestByHead(ctx, in.Repo, in.HeadRef)
		if err != nil || !found {
			return forge.WrittenRef{}, nil, false, err
		}
		// A reused head is ambiguous without the intent's base, so recover only a newer PR with that base.
		baseMatches := in.BaseRef != "" && pr.BaseRef == in.BaseRef
		createdAfterIntent := !pr.CreatedAt.IsZero() &&
			!pr.CreatedAt.Before(in.CreatedAt.Add(-writePRClockSkew))
		if !baseMatches || !createdAfterIntent {
			return forge.WrittenRef{}, nil, false, nil
		}
		return forge.WrittenRef{Number: pr.Number, URL: pr.URL}, &pr, true, nil
	case forge.WriteKindReview:
		ref, found, err := rf.reviewer.FindByWriteMarker(ctx, in.Repo, in.Kind, in.Parent, in.ID, in.CreatedAt)
		return ref, nil, found, err
	case forge.WriteKindIssue, forge.WriteKindComment:
		ref, found, err := rf.author.FindByWriteMarker(ctx, in.Repo, in.Kind, in.Parent, in.ID, in.CreatedAt)
		return ref, nil, found, err
	default:
		return forge.WrittenRef{}, nil, false, fmt.Errorf("forge: write intent %s has unknown kind %d", in.ID, in.Kind)
	}
}

// completeFound records a recovered object against its pending intent.
func (s *forgeService) completeFound(ctx context.Context, in store.WriteIntent, ref forge.WrittenRef, pr *forge.PullRequest) *compassv1internal.ForgeCallError {
	a := store.AuthoredArtifact{
		Provider: in.Provider, Host: in.Host, Repo: in.Repo,
		AgentAccountID: in.AgentAccountID, OwnerUserID: in.OwnerUserID,
		SessionID: in.SessionID, CreatedAtUnixMS: s.now().UnixMilli(),
	}
	switch in.Kind {
	case forge.WriteKindIssue:
		a.Kind, a.Number = store.ForgeArtifactKindIssue, ref.Number
		return s.completeWrite(ctx, in.ID, &a, "", ref.URL)
	case forge.WriteKindPullRequest:
		if pr == nil {
			return forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent %s found no pull request", in.ID))
		}
		a.Kind, a.Number = store.ForgeArtifactKindPullRequest, ref.Number
		return s.recordPullRequest(ctx, in.ID, a, *pr, in.IssueLink)
	case forge.WriteKindComment, forge.WriteKindReview:
		return s.completeWrite(ctx, in.ID, nil, ref.Key, ref.URL)
	default:
		return forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent %s has unknown kind %d", in.ID, in.Kind))
	}
}

// intentRef returns a done attempt's object; the arm renders it with no forge call.
func intentRef(in store.WriteIntent) (*forge.WrittenRef, *compassv1internal.ForgeCallError) {
	ref := forge.WrittenRef{Key: in.ForgeKey, URL: in.ForgeURL}
	if in.Kind == forge.WriteKindIssue || in.Kind == forge.WriteKindPullRequest {
		n, err := strconv.ParseUint(in.ForgeKey, 10, 64)
		if err != nil {
			return nil, forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent %s has no artifact number", in.ID))
		}
		ref.Number = n
	}
	return &ref, nil
}

// refID reads a GitHub numeric comment or review id; a Linear UUID key has none.
func refID(ref *forge.WrittenRef) uint64 {
	id, err := strconv.ParseUint(ref.Key, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// replayResult renders an earlier attempt's object by the intent's own kind,
// so a same-key retry on another arm still shows what was created.
func replayResult(in store.WriteIntent, ref *forge.WrittenRef) *compassv1internal.ForgeCallResult {
	switch in.Kind {
	case forge.WriteKindIssue, forge.WriteKindPullRequest:
		kind := store.ForgeArtifactKindIssue
		if in.Kind == forge.WriteKindPullRequest {
			kind = store.ForgeArtifactKindPullRequest
		}
		res := coordinateResult(store.AuthoredArtifact{Provider: in.Provider, Host: in.Host, Repo: in.Repo, Kind: kind, Number: ref.Number})
		if pr := res.GetPullRequest(); pr != nil {
			pr.Url = ref.URL
		}
		return res
	case forge.WriteKindComment:
		return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_IssueComment{
			IssueComment: &compassv1internal.CommentRef{Url: ref.URL, CommentId: refID(ref)},
		}}
	case forge.WriteKindReview:
		return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_Review{
			Review: &compassv1internal.ReviewRef{Url: ref.URL, ReviewId: refID(ref)},
		}}
	default:
		return forgeErrorResult(forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent %s has unknown kind %d", in.ID, in.Kind)))
	}
}

// runWriteSweep calls sweepWriteIntents each minute until ctx ends.
func (s *forgeService) runWriteSweep(ctx context.Context) error {
	tick := time.NewTicker(writeSweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := s.sweepWriteIntents(ctx); err != nil {
				slog.WarnContext(ctx, "forge: write intent sweep", slog.Any("error", err))
			}
		}
	}
}

// sweepWriteIntents resolves pending intents, drops expired holds, and retries durable notices.
func (s *forgeService) sweepWriteIntents(ctx context.Context) error {
	now := s.now()
	var errs []error
	if _, err := s.store.DeleteExpiredHeldWriteIntents(store.WithSystemRole(ctx), now.Add(-writeLostAfter), writeSweepLimit); err != nil {
		errs = append(errs, fmt.Errorf("forge: delete expired held write intents: %w", err))
	}
	pending, err := s.store.PendingWriteIntents(store.WithSystemRole(ctx), now.Add(-writeSweepMinAge), writeSweepLimit)
	if err != nil {
		errs = append(errs, fmt.Errorf("forge: list pending write intents: %w", err))
		pending = nil
	}
	for _, in := range pending {
		tctx := store.WithTenant(store.WithoutSystemRole(ctx), in.TenantID)
		ref, pr, found, err := s.findWrite(tctx, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("forge: find write intent %s: %w", in.ID, err))
			continue
		}
		if found {
			if fe := s.completeFound(tctx, in, ref, pr); fe != nil {
				errs = append(errs, fmt.Errorf("forge: complete write intent %s: %s", in.ID, fe.GetMessage()))
			}
			continue
		}
		if now.Sub(in.CreatedAt) < writeLostAfter {
			continue
		}
		if err := s.store.EndWriteIntent(tctx, in.ID, store.WriteStateLost); err != nil && !errors.Is(err, store.ErrConflict) {
			errs = append(errs, fmt.Errorf("forge: mark write intent %s lost: %w", in.ID, err))
		}
	}
	errs = append(errs, s.sweepForgeNotices(ctx, now)...)
	return errors.Join(errs...)
}

func (s *forgeService) sweepForgeNotices(ctx context.Context, now time.Time) []error {
	notices, err := s.store.UndeliveredNotices(store.WithSystemRole(ctx), writeSweepLimit)
	if err != nil {
		return []error{fmt.Errorf("forge: list undelivered notices: %w", err)}
	}
	errs := make([]error, 0)
	for _, notice := range notices {
		tctx := store.WithTenant(store.WithoutSystemRole(ctx), notice.TenantID)
		if err := s.retryForgeNotice(tctx, now, notice); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (s *forgeService) retryForgeNotice(ctx context.Context, now time.Time, notice store.Notice) error {
	if now.Sub(notice.CreatedAt) >= writeLostAfter {
		if err := s.store.MarkNoticeDelivered(ctx, notice.ID); err != nil {
			return fmt.Errorf("forge: stop retrying expired notice %s: %w", notice.ID, err)
		}
		slog.WarnContext(ctx, "forge: giving up on expired notice", "notice", notice.ID, "target_agent", notice.ToAccountID)
		return nil
	}
	if err := s.notifyAgent(ctx, notice.FromAccountID, notice.ToAccountID, notice.RequestKey, notice.Text); err != nil {
		if bumpErr := s.store.BumpNoticeAttempt(ctx, notice.ID); bumpErr != nil {
			return errors.Join(
				fmt.Errorf("forge: bump notice %s attempt: %w", notice.ID, bumpErr),
				fmt.Errorf("forge: deliver notice %s: %w", notice.ID, err),
			)
		}
		return fmt.Errorf("forge: deliver notice %s: %w", notice.ID, err)
	}
	if err := s.store.MarkNoticeDelivered(ctx, notice.ID); err != nil {
		return fmt.Errorf("forge: mark notice %s delivered: %w", notice.ID, err)
	}
	return nil
}
