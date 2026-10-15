//go:build unix

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/mirror"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Owner-header session id for agent mirror comments; user mirrors use the
// render attribution and do not carry an owner header.
const mirrorAgentSession = "mirror"

var mirrorOwnerHeaderAllowance = forge.MaxOwnerHeaderLen(mirrorAgentSession)

var _ mirror.Poster = (*forgeService)(nil)

type userMirrorPoster struct {
	*forgeService
}

func (p userMirrorPoster) BodyLimit(ref refs.Ref) int {
	return p.ProviderBodyLimit(ref)
}

type agentMirrorPoster struct {
	*forgeService
}

func (p agentMirrorPoster) BodyLimit(ref refs.Ref) int {
	return p.forgeService.BodyLimit(ref)
}

type mirrorRefusalError struct {
	code    connect.Code
	message string
}

func (e *mirrorRefusalError) Error() string { return e.message }

func (e *mirrorRefusalError) Unwrap() error { return mirror.ErrRefused }

func (s *forgeService) mirrorAllowed(ctx context.Context, account store.Account, need store.ForgeMirrorMode, ref refs.Ref) (resolvedForge, string, uint64, *compassv1internal.ForgeCallError) {
	if account.ID == "" {
		return resolvedForge{}, "", 0, forgeErr(connect.CodeUnauthenticated, "forge mirror: caller is required")
	}
	rf, repo, number, fe := s.resolveMirrorCoordinate(ref)
	if fe != nil {
		return resolvedForge{}, "", 0, fe
	}
	mode, err := s.store.ForgeMirrorMode(ctx, store.ForgeProvider(rf.provider), rf.host, repo)
	if err != nil {
		return resolvedForge{}, "", 0, storeForgeError(err)
	}
	if mode < need {
		return resolvedForge{}, "", 0, forgeErr(connect.CodeFailedPrecondition, "forge mirror is disabled for "+repo)
	}
	if account.IsAgent() {
		if fe := s.requireForgeScope(ctx, account.ID, rf, repo); fe != nil {
			return resolvedForge{}, "", 0, fe
		}
	} else if account.User == nil {
		return resolvedForge{}, "", 0, forgeErr(connect.CodePermissionDenied, "forge mirror: caller must be a user or agent")
	}
	return rf, repo, number, nil
}

func (s *forgeService) BodyLimit(ref refs.Ref) int {
	limit := s.ProviderBodyLimit(ref)
	if limit == int(^uint(0)>>1) {
		return limit
	}
	limit -= mirrorOwnerHeaderAllowance
	if limit <= 0 {
		return 1
	}
	return limit
}

func (s *forgeService) ProviderBodyLimit(ref refs.Ref) int {
	rf, _, _, fe := s.resolveMirrorCoordinate(ref)
	if fe != nil {
		return 0
	}
	if rf.bodyLimit <= 0 {
		return int(^uint(0) >> 1)
	}
	return rf.bodyLimit
}

func (s *forgeService) resolveMirrorCoordinate(ref refs.Ref) (resolvedForge, string, uint64, *compassv1internal.ForgeCallError) {
	var provider compassv1.ForgeProvider
	var host, repo string
	switch ref.Kind {
	case refs.KindTracker:
		provider, host, repo = compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, forge.LinearHost, ref.Key
	case refs.KindForge:
		provider, repo = compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, ref.Owner+"/"+ref.Repo
	default:
		return resolvedForge{}, "", 0, forgeErr(connect.CodeInvalidArgument, "forge mirror: unsupported reference kind")
	}
	if ref.Number == 0 {
		return resolvedForge{}, "", 0, forgeErr(connect.CodeInvalidArgument, "forge mirror: reference number is required")
	}
	rf, ok := s.providers.resolve(&compassv1.ForgeRef{Provider: provider, Host: host})
	if !ok {
		return resolvedForge{}, "", 0, forgeErr(connect.CodeNotFound, "forge mirror: provider is not configured")
	}
	return rf, repo, ref.Number, nil
}
func (s *forgeService) PostMirror(ctx context.Context, actor store.AccountID, need store.ForgeMirrorMode, ref refs.Ref, commentKey, clientID, body string) (forge.Comment, error) {
	account, err := s.store.GetAccount(ctx, actor)
	if err != nil {
		return forge.Comment{}, fmt.Errorf("mirror: read caller: %w", err)
	}
	rf, repo, number, fe := s.mirrorAllowed(ctx, account, need, ref)
	if fe != nil {
		return forge.Comment{}, &mirrorRefusalError{code: parseConnectCode(fe.GetCode()), message: fe.GetMessage()}
	}
	switch {
	case account.IsAgent():
		identity, identityErr := s.resolveIdentity(ctx, actor, mirrorAgentSession)
		if identityErr != nil {
			return forge.Comment{}, fmt.Errorf("mirror: resolve agent attribution: %s", identityErr.GetMessage())
		}
		body, err = forge.StampOwner(body, identity.author, rf.bodyLimit)
		if err != nil {
			return forge.Comment{}, fmt.Errorf("mirror: stamp owner header: %w", err)
		}
	case account.User == nil:
		return forge.Comment{}, &mirrorRefusalError{code: connect.CodePermissionDenied, message: "caller must be a user or agent"}
	default:
		body, _, _ = forge.StripOwner(body)
	}
	if commentKey != "" {
		return rf.author.EditComment(ctx, repo, commentKey, body)
	}
	if ref.Kind == refs.KindForge {
		rows, err := s.store.PullRequestsByCoords(ctx, []store.ForgeCoord{{Provider: store.ForgeProvider(rf.provider), Host: rf.host, Repo: repo, Number: number}})
		if err != nil {
			return forge.Comment{}, fmt.Errorf("mirror: look up pull request: %w", err)
		}
		for _, row := range rows {
			if row.Coord.Number == number {
				return rf.author.CommentOnPullRequest(ctx, repo, number, body, clientID)
			}
		}
	}
	return rf.author.CommentOnIssue(ctx, repo, number, body, clientID)
}

func (s *forgeService) ListMirrorComments(ctx context.Context, actor store.AccountID, ref refs.Ref, since time.Time) ([]forge.Comment, error) {
	account, err := s.store.GetAccount(ctx, actor)
	if err != nil {
		return nil, err
	}
	need := store.ForgeMirrorAuto
	if account.User != nil {
		need = store.ForgeMirrorManual
	}
	rf, repo, number, fe := s.mirrorAllowed(ctx, account, need, ref)
	if fe != nil {
		return nil, &mirrorRefusalError{code: parseConnectCode(fe.GetCode()), message: fe.GetMessage()}
	}
	identity, ok := rf.author.(interface {
		AppLogin(ctx context.Context) (string, error)
	})
	if !ok {
		return nil, errors.New("mirror: provider has no app identity lookup")
	}
	login, err := identity.AppLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("mirror: resolve app author for marker scan: %w", err)
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, errors.New("mirror: app author identity is empty")
	}
	comments, err := rf.author.ListCommentsSince(ctx, repo, number, since)
	if err != nil {
		return nil, err
	}
	filtered := make([]forge.Comment, 0, len(comments))
	for _, comment := range comments {
		if strings.EqualFold(comment.ForgeAccount, login) {
			filtered = append(filtered, comment)
		}
	}
	return filtered, nil
}

func (s *forgeService) DeleteMirrorComment(ctx context.Context, actor store.AccountID, ref refs.Ref, commentKey string) error {
	account, err := s.store.GetAccount(ctx, actor)
	if err != nil {
		return err
	}
	need := store.ForgeMirrorAuto
	if account.User != nil {
		need = store.ForgeMirrorManual
	}
	rf, repo, _, fe := s.mirrorAllowed(ctx, account, need, ref)
	if fe != nil {
		return &mirrorRefusalError{code: parseConnectCode(fe.GetCode()), message: fe.GetMessage()}
	}
	return rf.author.DeleteComment(ctx, repo, commentKey)
}

func (s *forgeService) SetMirrorLifetime(ctx context.Context) {
	life, cancel := context.WithCancel(ctx)
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	if s.mirrorCancel == nil {
		s.mirrorDone = life.Done()
		s.mirrorCancel = cancel
	} else {
		cancel()
	}
	if s.mirrorTimers == nil {
		s.mirrorTimers = make(map[string]*mirrorTimer)
	}
}
func (s *forgeService) StopMirrorLifetime() {
	s.mirrorMu.Lock()
	s.mirrorClosing = true
	if s.mirrorCancel != nil {
		s.mirrorCancel()
	}
	for rowID, handle := range s.mirrorTimers {
		if handle.stop != nil && handle.stop() {
			handle.finish(s)
		}
		delete(s.mirrorTimers, rowID)
	}
	s.mirrorMu.Unlock()
	s.mirrorWG.Wait()
}

func (s *forgeService) beginMirrorWork(parent context.Context) (context.Context, func(), bool) {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	if s.mirrorClosing {
		return nil, nil, false
	}
	if s.mirrorDone == nil {
		life, cancel := context.WithCancel(context.WithoutCancel(parent))
		s.mirrorDone = life.Done()
		s.mirrorCancel = cancel
	}
	s.mirrorWG.Add(1)
	work, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := cancelOnDone(s.mirrorDone, cancel)
	done := func() {
		stop()
		cancel()
		s.mirrorWG.Done()
	}
	return work, done, true
}

func (s *forgeService) scheduleMirrorRecheck(ctx context.Context, row store.ForgeMirrorRow, bodyFn mirror.BodyFunc, failed bool) {
	if row.RecheckAt == nil {
		return
	}
	delay := max(time.Until(*row.RecheckAt), 0)
	parent := context.WithoutCancel(ctx)

	s.mirrorMu.Lock()
	if s.mirrorClosing {
		s.mirrorMu.Unlock()
		return
	}
	if s.mirrorTimers == nil {
		s.mirrorTimers = make(map[string]*mirrorTimer)
	}
	if s.mirrorRecheckRetry == nil {
		s.mirrorRecheckRetry = make(map[string]int)
	}
	retry := s.mirrorRecheckRetry[row.ID]
	if failed {
		delay = mirrorRecheckBackoff(retry)
	}
	if row.LeaseUntil != nil {
		delay = max(delay, time.Until(*row.LeaseUntil))
	}
	due := time.Now().Add(delay)
	if current := s.mirrorTimers[row.ID]; current != nil {
		if !due.Before(current.due) {
			s.mirrorMu.Unlock()
			return
		}
		if failed {
			s.mirrorRecheckRetry[row.ID] = min(retry+1, 2)
		}
		if current.stop != nil && current.stop() {
			current.finish(s)
		}
		delete(s.mirrorTimers, row.ID)
	} else if failed {
		s.mirrorRecheckRetry[row.ID] = min(retry+1, 2)
	}
	handle := &mirrorTimer{due: due}
	s.mirrorTimers[row.ID] = handle
	s.mirrorWG.Add(1)
	sched := s.scheduleMirrorAfter
	if sched == nil {
		sched = func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		}
	}
	done := s.mirrorDone
	handle.stop = sched(delay, func() {
		defer handle.finish(s)
		s.mirrorMu.Lock()
		current := s.mirrorTimers[row.ID] == handle
		if current {
			delete(s.mirrorTimers, row.ID)
		}
		closing := s.mirrorClosing
		s.mirrorMu.Unlock()
		if !current || closing {
			return
		}
		timerCtx, cancel := context.WithCancel(parent)
		stop := cancelOnDone(done, cancel)
		defer stop()
		defer cancel()
		recheckRow, driveErr := s.driveMirrorRow(timerCtx, row.ID, bodyFn)
		s.mirrorMu.Lock()
		if driveErr == nil {
			delete(s.mirrorRecheckRetry, row.ID)
		}
		s.mirrorMu.Unlock()
		if driveErr != nil && errors.Is(driveErr, store.ErrLeaseLost) {
			recheckRow, err := s.mirrorStoreDB.ForgeMirrorByID(timerCtx, row.ID)
			if err != nil {
				// A failed read keeps the timer's own row, so its recheck still retries with backoff.
				slog.Default().ErrorContext(timerCtx, "read forge mirror after lease loss", "mirror_id", row.ID, "error", err)
				recheckRow = row
			}
			if recheckRow.RecheckAt != nil {
				s.scheduleMirrorRecheck(timerCtx, recheckRow, bodyFn, true)
			}
			return
		}
		s.scheduleMirrorRecheck(timerCtx, recheckRow, bodyFn, driveErr != nil)
	})
	s.mirrorMu.Unlock()
}

func mirrorRecheckBackoff(attempt int) time.Duration {
	switch min(attempt, 2) {
	case 0:
		return time.Minute
	case 1:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

func (h *mirrorTimer) finish(s *forgeService) {
	h.once.Do(s.mirrorWG.Done)
}

func cancelOnDone(done <-chan struct{}, cancel context.CancelFunc) func() {
	if done == nil {
		return func() {}
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-done:
			cancel()
		case <-stop:
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

func (s *forgeService) driveMirrorRow(ctx context.Context, rowID string, bodyFn mirror.BodyFunc) (store.ForgeMirrorRow, error) {
	row, err := s.mirrorStoreDB.ForgeMirrorByID(ctx, rowID)
	if err != nil {
		slog.Default().ErrorContext(ctx, "read forge mirror for drive", "mirror_id", rowID, "error", err)
		return store.ForgeMirrorRow{}, err
	}
	if row.Source == 2 && bodyFn == nil && row.AskID != "" {
		bodyFn = s.userAskBodyForRow()
	}
	poster := mirror.Poster(userMirrorPoster{s})
	if row.Agent != "" {
		poster = agentMirrorPoster{s}
	}
	if s.mirrorPoster != nil {
		poster = s.mirrorPoster
	}
	result, err := mirror.DriveOne(ctx, s.mirrorStoreDB, poster, row, func(channelID, topicID string) mirror.Link {
		return mirror.Link{URL: topicLinkFor(s.publicURL, channelID, topicID)}
	}, bodyFn)
	if err != nil {
		slog.Default().ErrorContext(ctx, "drive forge mirror", "mirror_id", row.ID, "error", err)
	}
	return result, err
}

func (s *forgeService) userAskBodyForRow() mirror.BodyFunc {
	return func(ctx context.Context, row store.ForgeMirrorRow, limit int) (string, error) {
		account, err := s.mirrorStoreDB.GetAccount(ctx, row.Actor)
		if err != nil {
			return "", err
		}
		current, err := s.mirrorStoreDB.AskForForgeMirror(ctx, row.Actor, row.AskID)
		if err != nil {
			return "", err
		}
		var ask *store.Ask
		for _, block := range current.Message.Blocks {
			if block.Ask != nil {
				ask = block.Ask
				break
			}
		}
		if ask == nil {
			return "", store.ErrNotFound
		}
		link := mirror.Link{Channel: current.Channel, Topic: current.Topic, URL: topicLinkFor(s.publicURL, string(current.ChannelID), current.Message.TopicID)}
		body, _, _ := forge.StripOwner(mirror.RenderAsk(*ask, current.AnsweredBy, link))
		return truncateBody("Mirrored by @"+account.Handle+"\n\n"+body, limit), nil
	}
}

func (s *forgeService) DriveAsks(ctx context.Context, askIDs []string) {
	life, done, ok := s.beginMirrorWork(ctx)
	if !ok {
		return
	}
	defer done()
	rows, err := s.mirrorStoreDB.AskForgeMirrors(life, askIDs)
	if err != nil {
		slog.Default().ErrorContext(life, "list ask forge mirrors", "ask_ids", askIDs, "error", err)
		return
	}
	for _, row := range rows {
		result, err := s.driveMirrorRow(life, row.ID, nil)
		s.scheduleMirrorRecheck(life, result, nil, err != nil)
	}
}
func (s *forgeService) MirrorAsUser(ctx context.Context, account store.Account, req *compassv1.MirrorToForgeRequest) (string, error) {
	fail := func(code connect.Code, err error) (string, error) { return "", connect.NewError(code, err) }
	if req == nil {
		return fail(connect.CodeInvalidArgument, errors.New("mirror request is required"))
	}
	ref, err := refs.Parse(req.GetRef())
	if err != nil {
		return fail(connect.CodeInvalidArgument, err)
	}
	if account.ID == "" || account.User == nil {
		return fail(connect.CodePermissionDenied, errors.New("forge mirror: caller must be a user"))
	}
	askID, topicID, messageIDs := req.GetAskId(), req.GetTopicId(), req.GetMessageIds()
	sourceCount := 0
	if askID != "" {
		sourceCount++
	}
	if topicID != "" {
		sourceCount++
	}
	if len(messageIDs) > 0 {
		sourceCount++
	}
	if sourceCount != 1 || req.GetChannelId() == "" || (len(messageIDs) > 0 && len(messageIDs) > 50) {
		return fail(connect.CodeInvalidArgument, errors.New("exactly one source and channel_id are required; message_ids must contain 1 to 50 ids"))
	}
	member, err := s.mirrorStoreDB.IsChannelMember(ctx, account.ID, store.ChannelID(req.GetChannelId()))
	if err != nil {
		return "", mirrorStoreError(err)
	}
	if !member {
		return fail(connect.CodeNotFound, store.ErrNotFound)
	}
	if _, _, _, fe := s.mirrorAllowed(ctx, account, store.ForgeMirrorManual, ref); fe != nil {
		return fail(parseConnectCode(fe.GetCode()), errors.New(fe.GetMessage()))
	}
	channel, err := s.mirrorStoreDB.GetChannel(ctx, store.ChannelID(req.GetChannelId()))
	if err != nil {
		return "", mirrorStoreError(err)
	}
	if askID != "" {
		return s.mirrorAskAsUser(ctx, account, ref, req.GetChannelId(), channel.Name, askID, fail)
	}
	sourceKey, renderBody, err := s.resolveUserMirrorSource(ctx, account, req, channel.Name)
	if err != nil {
		return "", err
	}
	return s.claimAndDriveUserMirror(ctx, account, ref, sourceKey, renderBody, fail)
}

func (s *forgeService) resolveUserMirrorSource(ctx context.Context, account store.Account, req *compassv1.MirrorToForgeRequest, channel string) (string, mirror.BodyFunc, error) {
	if req.GetTopicId() != "" {
		return s.resolveUserTopicSource(ctx, account, req, channel)
	}
	ids := append([]string(nil), req.GetMessageIds()...)
	slices.Sort(ids)
	for i, id := range ids {
		if id == "" || i > 0 && id == ids[i-1] {
			return "", nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message ids must be non-empty and unique"))
		}
	}
	hash := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	messages := make([]store.Message, 0, len(ids))
	for _, id := range ids {
		messageChannel, err := s.mirrorStoreDB.MessageChannel(ctx, id)
		if errors.Is(err, store.ErrNotFound) || err == nil && messageChannel != store.ChannelID(req.GetChannelId()) {
			return "", nil, connect.NewError(connect.CodeNotFound, store.ErrNotFound)
		}
		if err != nil {
			return "", nil, mirrorStoreError(err)
		}
		message, err := s.mirrorStoreDB.MessageByID(ctx, id)
		if err != nil {
			return "", nil, mirrorStoreError(err)
		}
		messages = append(messages, message)
	}
	member, err := s.mirrorStoreDB.IsChannelMember(ctx, account.ID, store.ChannelID(req.GetChannelId()))
	if err != nil {
		return "", nil, mirrorStoreError(err)
	}
	if !member {
		return "", nil, connect.NewError(connect.CodeNotFound, store.ErrNotFound)
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].At.Equal(messages[j].At) {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].At.Before(messages[j].At)
	})
	key := hex.EncodeToString(hash[:])
	link := mirror.Link{Channel: channel, Topic: "selected messages", URL: deepLinkFor(s.publicURL, req.GetChannelId())}
	return key, userMessageBody(messages, messageAuthors(messages), link, account.Handle, ""), nil

}

func (s *forgeService) resolveUserTopicSource(ctx context.Context, account store.Account, req *compassv1.MirrorToForgeRequest, channel string) (string, mirror.BodyFunc, error) {
	topics, err := s.mirrorStoreDB.ListTopics(ctx, string(account.ID), req.GetChannelId(), true)
	if err != nil {
		return "", nil, mirrorStoreError(err)
	}
	for _, topic := range topics {
		if topic.ID != req.GetTopicId() {
			continue
		}
		messages, err := s.mirrorStoreDB.ListMessages(ctx, store.ListMessagesQuery{Actor: account.ID, ChannelID: store.ChannelID(req.GetChannelId()), TopicID: topic.ID, Page: store.Page{Limit: 51}})
		if err != nil {
			return "", nil, mirrorStoreError(err)
		}
		if messages == nil {
			messages = []store.Message{}
		}
		showLimitNote := len(messages) > 50
		if showLimitNote {
			messages = append([]store.Message(nil), messages[:50]...)
		}
		slices.Reverse(messages)
		note := ""
		if showLimitNote {
			note = "Showing the newest 50 messages."
		}
		link := mirror.Link{Channel: channel, Topic: topic.Name, URL: topicLinkFor(s.publicURL, req.GetChannelId(), topic.ID)}
		return topic.ID, userMessageBody(messages, messageAuthors(messages), link, account.Handle, note), nil
	}
	return "", nil, connect.NewError(connect.CodeNotFound, store.ErrNotFound)
}

func userMessageBody(messages []store.Message, authors map[store.AccountID]string, link mirror.Link, handle, note string) mirror.BodyFunc {
	return func(_ context.Context, _ store.ForgeMirrorRow, limit int) (string, error) {
		prefix := "Mirrored by @" + handle
		if note != "" {
			prefix += "\n\n" + note
		}
		body := mirror.Render(messages, authors, link, prefix, limit)
		clean, _, _ := forge.StripOwner(body)
		return truncateBody(clean, limit), nil
	}
}

func (s *forgeService) mirrorAskAsUser(ctx context.Context, account store.Account, ref refs.Ref, channelID, channelName, askID string, fail func(connect.Code, error) (string, error)) (string, error) {
	source, err := s.mirrorStoreDB.AskForForgeMirror(ctx, account.ID, askID)
	if err != nil {
		return "", mirrorStoreError(err)
	}
	if source.ChannelID != store.ChannelID(channelID) {
		return fail(connect.CodeNotFound, store.ErrNotFound)
	}
	rows, err := s.mirrorStoreDB.AskForgeMirrors(ctx, []string{askID})
	if err != nil {
		return "", mirrorStoreError(err)
	}
	for _, row := range rows {
		if row.TargetRef != ref.String() {
			continue
		}
		if row.Source == 1 {
			return s.clickDrive(ctx, row, nil, fail)
		}
		if row.Source != 2 || row.Actor != account.ID || row.AskID != askID {
			return fail(connect.CodeFailedPrecondition, errors.New("mirror source conflicts with another actor or source"))
		}
		return s.clickDrive(ctx, row, s.userAskBody(account, channelID, channelName), fail)
	}
	row, created, err := s.mirrorStoreDB.ClaimForgeMirror(ctx, store.ForgeMirrorRow{
		Source: 2, AskID: askID, SourceKey: "ask:" + askID, Actor: account.ID, TargetRef: ref.String(), BodyRev: 1, State: 1,
	})
	if err != nil {
		return "", mirrorStoreError(err)
	}
	if !created && (row.Source != 2 || row.AskID != askID || row.Actor != account.ID || row.TargetRef != ref.String()) {
		return fail(connect.CodeFailedPrecondition, errors.New("mirror source conflicts with another actor or target"))
	}
	return s.clickDrive(ctx, row, s.userAskBody(account, channelID, channelName), fail)
}

func (s *forgeService) userAskBody(account store.Account, channelID, channelName string) mirror.BodyFunc {
	return func(bodyCtx context.Context, row store.ForgeMirrorRow, limit int) (string, error) {
		current, err := s.mirrorStoreDB.AskForForgeMirror(bodyCtx, account.ID, row.AskID)
		if err != nil {
			return "", err
		}
		var ask *store.Ask
		for _, block := range current.Message.Blocks {
			if block.Ask != nil {
				ask = block.Ask
				break
			}
		}
		if ask == nil {
			return "", store.ErrNotFound
		}
		link := mirror.Link{Channel: channelName, Topic: current.Topic, URL: topicLinkFor(s.publicURL, channelID, current.Message.TopicID)}
		clean, _, _ := forge.StripOwner(mirror.RenderAsk(*ask, current.AnsweredBy, link))
		return truncateBody("Mirrored by @"+account.Handle+"\n\n"+clean, limit), nil
	}
}

func (s *forgeService) claimAndDriveUserMirror(ctx context.Context, account store.Account, ref refs.Ref, sourceKey string, bodyFn mirror.BodyFunc, fail func(connect.Code, error) (string, error)) (string, error) {
	row, created, err := s.mirrorStoreDB.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: sourceKey, Actor: account.ID, TargetRef: ref.String(), BodyRev: 1, State: 1})
	if err != nil {
		return "", mirrorStoreError(err)
	}
	if !created && (row.Source != 2 || row.Agent != "" || row.Actor != account.ID || row.SourceKey != sourceKey || row.TargetRef != ref.String()) {
		return fail(connect.CodeFailedPrecondition, errors.New("mirror source conflicts with another actor"))
	}
	return s.clickDrive(ctx, row, bodyFn, fail)
}
func (s *forgeService) clickDrive(ctx context.Context, row store.ForgeMirrorRow, bodyFn mirror.BodyFunc, fail func(connect.Code, error) (string, error)) (string, error) {
	s.mirrorMu.Lock()
	if s.mirrorClosing {
		s.mirrorMu.Unlock()
		return fail(connect.CodeUnavailable, errors.New("mirror still delivering"))
	}
	done := s.mirrorDone
	if done == nil {
		life, lifeCancel := context.WithCancel(context.WithoutCancel(ctx))
		done = life.Done()
		s.mirrorDone = done
		s.mirrorCancel = lifeCancel
	}
	s.mirrorWG.Add(1)
	s.mirrorMu.Unlock()
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopLife := cancelOnDone(done, cancel)

	type driveResult struct {
		row store.ForgeMirrorRow
		err error
	}
	resultCh := make(chan driveResult, 1)
	go func() {
		defer s.mirrorWG.Done()
		defer cancel()
		if stopLife != nil {
			defer stopLife()
		}
		poster := mirror.Poster(userMirrorPoster{s})
		if row.Agent != "" {
			poster = agentMirrorPoster{s}
		}
		if s.mirrorPoster != nil {
			poster = s.mirrorPoster
		}
		result, err := mirror.DriveOne(workCtx, s.mirrorStoreDB, poster, row, func(channelID, topicID string) mirror.Link {
			return mirror.Link{URL: topicLinkFor(s.publicURL, channelID, topicID)}
		}, bodyFn)
		if err != nil {
			slog.Default().ErrorContext(workCtx, "drive user forge mirror", "mirror_id", row.ID, "error", err)
		}
		s.scheduleMirrorRecheck(workCtx, result, bodyFn, err != nil)
		resultCh <- driveResult{row: result, err: err}
	}()
	select {
	case <-ctx.Done():
		return fail(connect.CodeUnavailable, errors.New("mirror still delivering"))
	case result := <-resultCh:
		if ctx.Err() != nil {
			return fail(connect.CodeUnavailable, errors.New("mirror still delivering"))
		}
		if result.row.State == 3 {
			if refusal, ok := errors.AsType[*mirrorRefusalError](result.err); ok {
				return fail(refusal.code, result.err)
			}
			if errors.Is(result.err, forge.ErrBodyTooLarge) {
				return fail(connect.CodeInvalidArgument, result.err)
			}
			if errors.Is(result.err, store.ErrLeaseLost) {
				return fail(connect.CodeUnavailable, result.err)
			}
			return fail(connect.CodeInternal, result.err)
		}
		if result.err != nil {
			return "", connect.NewError(connect.CodeInternal, result.err)
		}
		if result.row.State != 2 {
			return fail(connect.CodeUnavailable, errors.New("mirror row was not delivered"))
		}
		return result.row.CommentURL, nil
	}
}

func truncateBody(body string, limit int) string {
	if limit <= 0 || len(body) <= limit {
		return body
	}
	body = body[:limit]
	for !utf8.ValidString(body) {
		body = body[:len(body)-1]
	}
	return body
}

func messageAuthors(messages []store.Message) map[store.AccountID]string {
	authors := make(map[store.AccountID]string, len(messages))
	for _, message := range messages {
		authors[message.AuthorAccountID] = message.AuthorHandle
	}
	return authors
}

func parseConnectCode(code string) connect.Code {
	switch code {
	case connect.CodeInvalidArgument.String():
		return connect.CodeInvalidArgument
	case connect.CodeNotFound.String():
		return connect.CodeNotFound
	case connect.CodePermissionDenied.String():
		return connect.CodePermissionDenied
	case connect.CodeFailedPrecondition.String():
		return connect.CodeFailedPrecondition
	default:
		return connect.CodeInternal
	}
}

func mirrorStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, store.ErrFailedPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
