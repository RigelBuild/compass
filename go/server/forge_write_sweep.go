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
	writeLostAfter  = 24 * time.Hour
	writeSweepLimit = 100

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
}

// beginWrite records the create's intent before any forge call. A non-nil ref
// is an earlier attempt's object; a non-nil error ends the call. Otherwise the
// caller owns a fresh attempt and calls the forge.
func (s *forgeService) beginWrite(ctx context.Context, key string, t writeTarget) (store.WriteIntent, *forge.WrittenRef, *compassv1internal.ForgeCallError) {
	fresh := uuid.NewString()
	in, err := s.store.RecordWriteIntent(ctx, store.WriteIntent{
		ID:             fresh,
		Provider:       store.ForgeProvider(t.rf.provider),
		Host:           t.rf.host,
		Repo:           t.repo,
		Kind:           t.kind,
		Parent:         t.parent,
		HeadRef:        t.headRef,
		AgentAccountID: t.caller,
		OwnerUserID:    t.id.ownerID,
		SessionID:      t.session,
		RequestKey:     key,
	})
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
	case store.WriteStateDone:
		ref, fe := intentRef(in)
		return in, ref, fe
	case store.WriteStateLost:
		return in, nil, forgeErr(connect.CodeFailedPrecondition, writeLostMessage)
	case store.WriteStatePending:
		// An earlier attempt may still be in flight, so look for its object; never resend.
		ref, found, err := s.findWrite(ctx, in)
		if err != nil || !found {
			return in, nil, forgeErr(connect.CodeUnavailable, writePendingMessage)
		}
		if fe := s.completeFound(ctx, in, ref); fe != nil {
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

// sameWriteTarget compares a replayed intent with this call's target; repo case folds as the store does.
func sameWriteTarget(in store.WriteIntent, t writeTarget) bool {
	provider := store.ForgeProvider(t.rf.provider)
	fold := func(repo string) string { return store.ForgeCoord{Provider: provider, Repo: repo}.Normalized().Repo }
	return in.Provider == provider && in.Host == t.rf.host && fold(in.Repo) == fold(t.repo) &&
		in.Kind == t.kind && in.Parent == t.parent && in.HeadRef == t.headRef && in.AgentAccountID == t.caller
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
	return ok && se.Status >= 400 && se.Status < 500 && se.Status != 429
}

// completeWrite marks a fresh attempt done. A conflict means the sweep already
// recorded the same object, so the create still succeeded.
func (s *forgeService) completeWrite(ctx context.Context, intentID string, artifact *store.AuthoredArtifact, forgeKey string) *compassv1internal.ForgeCallError {
	if err := s.store.CompleteWriteIntent(ctx, intentID, artifact, forgeKey); err != nil && !errors.Is(err, store.ErrConflict) {
		return storeForgeError(err)
	}
	return nil
}

// findWrite looks for the object a pending intent may have created: a PR by
// its head, anything else by the intent's marker or id.
func (s *forgeService) findWrite(ctx context.Context, in store.WriteIntent) (forge.WrittenRef, bool, error) {
	rf, ok := s.providers.resolve(&compassv1.ForgeRef{Provider: compassv1.ForgeProvider(in.Provider), Host: in.Host})
	if !ok {
		return forge.WrittenRef{}, false, fmt.Errorf("forge: no provider configured for %d/%s", in.Provider, in.Host)
	}
	switch in.Kind {
	case forge.WriteKindPullRequest:
		pr, found, err := rf.author.FindPullRequestByHead(ctx, in.Repo, in.HeadRef)
		if err != nil || !found {
			return forge.WrittenRef{}, false, err
		}
		return forge.WrittenRef{Number: pr.Number, URL: pr.URL}, true, nil
	case forge.WriteKindReview:
		return rf.reviewer.FindByWriteMarker(ctx, in.Repo, in.Kind, in.Parent, in.ID, in.CreatedAt)
	case forge.WriteKindIssue, forge.WriteKindComment:
		return rf.author.FindByWriteMarker(ctx, in.Repo, in.Kind, in.Parent, in.ID, in.CreatedAt)
	default:
		return forge.WrittenRef{}, false, fmt.Errorf("forge: write intent %s has unknown kind %d", in.ID, in.Kind)
	}
}

// completeFound records a recovered object against its pending intent.
func (s *forgeService) completeFound(ctx context.Context, in store.WriteIntent, ref forge.WrittenRef) *compassv1internal.ForgeCallError {
	switch in.Kind {
	case forge.WriteKindIssue, forge.WriteKindPullRequest:
		kind := store.ForgeArtifactKindIssue
		if in.Kind == forge.WriteKindPullRequest {
			kind = store.ForgeArtifactKindPullRequest
		}
		a := store.AuthoredArtifact{
			Provider:        in.Provider,
			Host:            in.Host,
			Repo:            in.Repo,
			Kind:            kind,
			Number:          ref.Number,
			AgentAccountID:  in.AgentAccountID,
			OwnerUserID:     in.OwnerUserID,
			SessionID:       in.SessionID,
			CreatedAtUnixMS: s.now().UnixMilli(),
		}
		return s.completeWrite(ctx, in.ID, &a, "")
	case forge.WriteKindComment, forge.WriteKindReview:
		return s.completeWrite(ctx, in.ID, nil, ref.Key)
	default:
		return forgeErr(connect.CodeInternal, fmt.Sprintf("forge: write intent %s has unknown kind %d", in.ID, in.Kind))
	}
}

// intentRef returns a done attempt's object; the arm renders it with no forge call.
func intentRef(in store.WriteIntent) (*forge.WrittenRef, *compassv1internal.ForgeCallError) {
	ref := forge.WrittenRef{Key: in.ForgeKey}
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

// sweepWriteIntents runs each minute and resolves pending intents (D2).
func (s *forgeService) sweepWriteIntents(ctx context.Context) error {
	now := s.now()
	pending, err := s.store.PendingWriteIntents(store.WithSystemRole(ctx), now.Add(-writeSweepMinAge), writeSweepLimit)
	if err != nil {
		return fmt.Errorf("forge: list pending write intents: %w", err)
	}
	var errs []error
	for _, in := range pending {
		tctx := store.WithTenant(store.WithoutSystemRole(ctx), in.TenantID)
		ref, found, err := s.findWrite(tctx, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("forge: find write intent %s: %w", in.ID, err))
			continue
		}
		if found {
			if fe := s.completeFound(tctx, in, ref); fe != nil {
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
	return errors.Join(errs...)
}
