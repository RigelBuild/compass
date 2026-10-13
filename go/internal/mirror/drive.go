package mirror

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	leaseTTL      = 2 * time.Minute
	renewInterval = 30 * time.Second
	commentWait   = 2 * time.Minute
	pendingState  = int16(1)
	doneState     = int16(2)
	failedState   = int16(3)
	maxAttempts   = 3
)

var (
	// ErrRefused is returned by Poster for a policy or scope refusal.
	ErrRefused = errors.New("mirror: post refused")
	sleep      = sleepContext
)

// Poster writes and reconciles comments for one actor and forge coordinate.
// ListMirrorComments returns only comments by the configured Compass App login;
// implementations fail closed when they cannot resolve that account.
// BodyLimit excludes the owner header; Drive reserves the row marker itself.
type Poster interface {
	PostMirror(ctx context.Context, actor store.AccountID, need store.ForgeMirrorMode, ref refs.Ref, commentKey, clientID, body string) (forge.Comment, error)
	ListMirrorComments(ctx context.Context, actor store.AccountID, ref refs.Ref, since time.Time) ([]forge.Comment, error)
	DeleteMirrorComment(ctx context.Context, actor store.AccountID, ref refs.Ref, commentKey string) error
	BodyLimit(ref refs.Ref) int
}

// LinkFunc builds a source link from the stored channel and topic IDs.
type LinkFunc func(channelID, topicID string) Link

// BodyFunc renders a source-2 row within its net body limit. Drive appends the row marker.
type BodyFunc func(context.Context, store.ForgeMirrorRow, int) (string, error)

// Drive leases and delivers each mirror row in order.
func Drive(ctx context.Context, st *store.Store, p Poster, rows []store.ForgeMirrorRow, link LinkFunc, bodyFn BodyFunc) {
	log := slog.Default()
	for _, candidate := range rows {
		if ctx.Err() != nil {
			return
		}
		if _, err := DriveOne(ctx, st, p, candidate, link, bodyFn); err != nil {
			log.ErrorContext(ctx, "drive forge mirror", "mirror_id", candidate.ID, "error", err)
		}
	}
}

// DriveOne leases and processes one row, then returns the tenant-scoped stored row.
func DriveOne(ctx context.Context, st *store.Store, p Poster, candidate store.ForgeMirrorRow, link LinkFunc, bodyFn BodyFunc) (store.ForgeMirrorRow, error) {
	log := slog.Default()
	for {
		if ctx.Err() != nil {
			return store.ForgeMirrorRow{}, ctx.Err()
		}
		token, err := newLeaseToken()
		if err != nil {
			return store.ForgeMirrorRow{}, fmt.Errorf("mirror: create lease token: %w", err)
		}
		row, ok, err := st.LeaseForgeMirror(ctx, candidate.ID, token, leaseTTL)
		if err != nil {
			return store.ForgeMirrorRow{}, fmt.Errorf("mirror: lease row %q: %w", candidate.ID, err)
		}
		if !ok {
			return st.ForgeMirrorByID(ctx, candidate.ID)
		}
		processErr := processLeasedRow(ctx, st, p, row, token, link, bodyFn, log)
		finalRow, readErr := st.ForgeMirrorByID(ctx, candidate.ID)
		if readErr != nil {
			return finalRow, errors.Join(processErr, readErr)
		}
		repointedComment := finalRow.State == pendingState && finalRow.CommentKey != row.CommentKey
		refusedCurrentRevision := errors.Is(processErr, ErrRefused) && finalRow.BodyRev <= row.BodyRev
		if refusedCurrentRevision || finalRow.BodyRev <= row.BodyRev && !repointedComment {
			return finalRow, processErr
		}
		candidate = finalRow
	}
}

func processLeasedRow(ctx context.Context, st *store.Store, p Poster, row store.ForgeMirrorRow, token string, link LinkFunc, bodyFn BodyFunc, log *slog.Logger) error {
	stopRenew, leaseLost, renewDone := renewLease(ctx, st, row.ID, token, log)
	defer func() {
		close(stopRenew)
		<-renewDone
	}()

	if row.State == doneState {
		if row.RecheckAt != nil {
			if err := recheckComments(ctx, st, p, row, token, leaseLost); err != nil {
				return fmt.Errorf("mirror: recheck row %q: %w", row.ID, err)
			}
		}
		return nil
	}

	ref, err := refs.Parse(row.TargetRef)
	if err != nil {
		cause := fmt.Errorf("mirror: parse target ref %q: %w", row.TargetRef, err)
		if markErr := markFailed(ctx, st, row, token, cause); markErr != nil {
			return errors.Join(cause, fmt.Errorf("mirror: mark invalid target failed: %w", markErr))
		}
		return cause
	}
	body, err := renderBody(ctx, st, p, row, link, bodyFn, ref)
	if err != nil {
		if markErr := markFailed(ctx, st, row, token, err); markErr != nil {
			return errors.Join(err, fmt.Errorf("mirror: mark render failure: %w", markErr))
		}
		return err
	}
	need := store.ForgeMirrorAuto
	if row.Agent == "" {
		need = store.ForgeMirrorManual
	}
	if err := deliverRow(ctx, st, p, row, token, ref, need, body, leaseLost, row.BodyRev); err != nil {
		return fmt.Errorf("mirror: deliver row %q: %w", row.ID, err)
	}
	return nil
}

func deliverRow(ctx context.Context, st *store.Store, p Poster, row store.ForgeMirrorRow, token string, ref refs.Ref, need store.ForgeMirrorMode, body string, leaseLost *atomic.Bool, renderedRev int16) error {
	if row.CommentKey != "" && row.PostedRev == row.BodyRev {
		return markDelivered(ctx, st, row, token, row.CommentID, row.CommentKey, row.CommentURL, false, renderedRev)
	}

	state := deliveryState{
		row:       row,
		isGitHub:  ref.Kind == refs.KindForge,
		uncertain: ref.Kind == refs.KindForge && row.CommentKey == "" && row.FirstAttemptAt != nil,
	}
	for attempt := range maxAttempts {
		if lostOrCanceled(ctx, leaseLost) {
			return nil
		}
		retry, err := deliverAttempt(ctx, st, p, token, ref, need, body, leaseLost, &state, renderedRev)
		if err != nil {
			state.lastErr = err
		}
		if !retry {
			return err
		}
		if attempt+1 < maxAttempts {
			if err := sleep(ctx, retryBackoff[attempt]); err != nil {
				return err
			}
		}
	}
	if state.lastErr != nil {
		return markFailed(ctx, st, state.row, token, state.lastErr)
	}
	return nil
}

type deliveryState struct {
	row       store.ForgeMirrorRow
	isGitHub  bool
	uncertain bool
	lastErr   error
}

func deliverAttempt(ctx context.Context, st *store.Store, p Poster, token string, ref refs.Ref, need store.ForgeMirrorMode, body string, leaseLost *atomic.Bool, state *deliveryState, renderedRev int16) (bool, error) {
	commentKey, comment, adopted, err := recoverPriorComment(ctx, p, state.row, ref, state.isGitHub, leaseLost)
	if err != nil {
		return true, err
	}
	if adopted {
		commentKey = comment.Key
		state.uncertain = true
		// An adopted comment's body revision is unknown, so always edit it.
	}

	if commentKey == "" {
		priorAttempt := state.row.FirstAttemptAt != nil
		updated, ok, err := st.BeginForgeMirrorAttempt(ctx, state.row.ID, token)
		if err != nil {
			return true, err
		}
		if !ok {
			return false, nil
		}
		state.row = updated
		state.uncertain = state.uncertain || state.isGitHub && priorAttempt
	}

	if lostOrCanceled(ctx, leaseLost) {
		return false, nil
	}
	comment, err = p.PostMirror(ctx, state.row.Actor, need, ref, commentKey, state.row.ClientCommentID, body)
	if err == nil {
		return false, markDelivered(ctx, st, state.row, token, comment.ID, comment.Key, comment.URL, state.uncertain, renderedRev)
	}
	if errors.Is(err, ErrRefused) {
		return false, errors.Join(ErrRefused, markFailed(ctx, st, state.row, token, err))
	}
	if state.isGitHub {
		state.uncertain = true
	}
	return true, err
}

func recoverPriorComment(ctx context.Context, p Poster, row store.ForgeMirrorRow, ref refs.Ref, isGitHub bool, leaseLost *atomic.Bool) (string, forge.Comment, bool, error) {
	if row.CommentKey != "" || !isGitHub || row.FirstAttemptAt == nil {
		return row.CommentKey, forge.Comment{}, false, nil
	}
	adopted, comment, err := recoverComment(ctx, p, row, ref, leaseLost)
	return row.CommentKey, comment, adopted, err
}

var retryBackoff = [...]time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

func renderBody(ctx context.Context, st *store.Store, p Poster, row store.ForgeMirrorRow, link LinkFunc, bodyFn BodyFunc, ref refs.Ref) (string, error) {
	providerLimit := p.BodyLimit(ref)
	limit := providerLimit

	markerLength := len(WithMarker("", row.ID))
	if providerLimit > 0 {
		if providerLimit < markerLength {
			return "", errors.New("mirror: provider body limit cannot fit row marker")
		}

		limit = providerLimit - markerLength
	}
	if row.Source == 2 {
		if bodyFn == nil {
			return "", errors.New("mirror: source-2 body supplier is required")
		}
		body, err := bodyFn(ctx, row, limit)
		if err != nil {
			return "", fmt.Errorf("mirror: render source-2 row: %w", err)
		}
		if providerLimit > 0 {
			body = truncateUTF8(body, limit)
		}
		return WithMarker(body, row.ID), nil
	}
	if row.Source != 1 {
		return "", fmt.Errorf("mirror: unsupported source %d", row.Source)
	}
	ask, err := st.AskForForgeMirror(ctx, row.Actor, row.AskID)
	if err != nil {
		return "", err
	}
	var askContent *store.Ask
	for _, block := range ask.Message.Blocks {
		if block.Ask != nil {
			askContent = block.Ask
			break
		}
	}
	if askContent == nil {
		return "", errors.New("mirror: ask row contains no ask block")
	}
	linkValue := Link{Channel: ask.Channel, Topic: ask.Topic}
	if link != nil {
		linkValue.URL = link(string(ask.ChannelID), ask.Message.TopicID).URL
	}
	return WithMarker(renderAskWithinLimit(*askContent, ask.AnsweredBy, linkValue, limit), row.ID), nil
}

func recoverComment(ctx context.Context, p Poster, row store.ForgeMirrorRow, ref refs.Ref, leaseLost *atomic.Bool) (bool, forge.Comment, error) {
	lastAttempt := row.LastAttemptAt
	if lastAttempt == nil {
		lastAttempt = row.FirstAttemptAt
	}
	if lastAttempt != nil {
		if wait := time.Until(lastAttempt.Add(commentWait)); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return false, forge.Comment{}, err
			}
		}
	}
	if lostOrCanceled(ctx, leaseLost) {
		return false, forge.Comment{}, context.Canceled
	}
	since := time.Time{}
	if row.FirstAttemptAt != nil {
		since = row.FirstAttemptAt.Add(-time.Minute)
	}
	comments, err := p.ListMirrorComments(ctx, row.Actor, ref, since)
	if err != nil {
		return false, forge.Comment{}, fmt.Errorf("mirror: list comments for recovery: %w", err)
	}
	marked := make([]forge.Comment, 0, len(comments))
	for _, comment := range comments {
		if HasMarker(comment.Body, row.ID) {
			marked = append(marked, comment)
		}
	}
	if len(marked) == 0 {
		return false, forge.Comment{}, nil
	}
	marked = oldestComments(marked)
	for _, duplicate := range marked[1:] {
		if lostOrCanceled(ctx, leaseLost) {
			return false, forge.Comment{}, context.Canceled
		}
		if err := p.DeleteMirrorComment(ctx, row.Actor, ref, duplicate.Key); err != nil {
			return false, forge.Comment{}, fmt.Errorf("mirror: delete duplicate comment %q: %w", duplicate.Key, err)
		}
	}
	return true, marked[0], nil
}

func recheckComments(ctx context.Context, st *store.Store, p Poster, row store.ForgeMirrorRow, token string, leaseLost *atomic.Bool) error {
	ref, err := refs.Parse(row.TargetRef)
	if err != nil {
		return markFailed(ctx, st, row, token, fmt.Errorf("mirror: parse target ref %q: %w", row.TargetRef, err))
	}
	if lostOrCanceled(ctx, leaseLost) {
		return nil
	}
	since := time.Time{}
	if row.FirstAttemptAt != nil {
		since = row.FirstAttemptAt.Add(-time.Minute)
	}
	comments, err := p.ListMirrorComments(ctx, row.Actor, ref, since)
	if err != nil {
		return fmt.Errorf("mirror: list comments for recheck: %w", err)
	}
	marked := make([]forge.Comment, 0, len(comments))
	for _, comment := range comments {
		if HasMarker(comment.Body, row.ID) {
			marked = append(marked, comment)
		}
	}
	marked = oldestComments(marked)
	for _, duplicate := range marked[1:] {
		if lostOrCanceled(ctx, leaseLost) {
			return nil
		}
		if err := p.DeleteMirrorComment(ctx, row.Actor, ref, duplicate.Key); err != nil {
			return fmt.Errorf("mirror: delete duplicate comment %q: %w", duplicate.Key, err)
		}
	}
	mark := store.ForgeMirrorMark{PostedRev: row.PostedRev, State: doneState, ClearRecheck: true}
	if len(marked) > 0 {
		mark.CommentID = marked[0].ID
		mark.CommentKey = marked[0].Key
		mark.URL = marked[0].URL
		if marked[0].Key != row.CommentKey {
			mark.PostedRev = 0
			mark.State = pendingState
		}
	} else {
		mark.CommentID = row.CommentID
		mark.CommentKey = row.CommentKey
		mark.URL = row.CommentURL
	}
	if err := st.MarkForgeMirror(ctx, row.ID, token, mark); err != nil {
		return fmt.Errorf("mirror: clear recheck state: %w", err)
	}
	return nil
}

func markDelivered(ctx context.Context, st *store.Store, row store.ForgeMirrorRow, token string, id uint64, key, url string, recheck bool, renderedRev int16) error {
	if err := st.MarkForgeMirror(ctx, row.ID, token, store.ForgeMirrorMark{
		PostedRev: renderedRev, State: doneState, CommentID: id,
		CommentKey: key, URL: url, Recheck: recheck,
	}); err != nil {
		return fmt.Errorf("mirror: mark delivered: %w", err)
	}
	return nil
}

func markFailed(ctx context.Context, st *store.Store, row store.ForgeMirrorRow, token string, cause error) error {
	if err := st.MarkForgeMirror(ctx, row.ID, token, store.ForgeMirrorMark{
		PostedRev: row.PostedRev, State: failedState, Err: cause.Error(),
	}); err != nil {
		return fmt.Errorf("mirror: mark failed: %w", err)
	}
	return nil
}

func renewLease(ctx context.Context, st *store.Store, id, token string, log *slog.Logger) (chan struct{}, *atomic.Bool, chan struct{}) {
	stop := make(chan struct{})
	done := make(chan struct{})
	lost := &atomic.Bool{}
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				ok, err := st.RenewForgeMirror(ctx, id, token, leaseTTL)
				if err != nil {
					lost.Store(true)
					log.ErrorContext(ctx, "renew forge mirror lease", "mirror_id", id, "error", err)
					return
				}
				if !ok {
					lost.Store(true)
					return
				}
			}
		}
	}()
	return stop, lost, done
}

func lostOrCanceled(ctx context.Context, leaseLost *atomic.Bool) bool {
	return leaseLost.Load() || ctx.Err() != nil
}

func newLeaseToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("mirror: read lease token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func oldestComments(comments []forge.Comment) []forge.Comment {
	sort.Slice(comments, func(i, j int) bool {
		if comments[i].ID != 0 && comments[j].ID != 0 {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].Key < comments[j].Key
	})
	return comments
}
