//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func appendAskForMirrorTest(t *testing.T, ctx context.Context, s *Store, author AccountID, ask Ask, requestID string) (Message, bool) {
	t.Helper()
	ch, err := s.CreateChannel(ctx, author, NewChannel{Name: "mirror-" + newID(), Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{author}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author,
		Blocks:          []MessageBlock{{Ask: &ask}},
	}, string(ch.ID), TopicRef{Name: testTopic, Create: true}, requestID)
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	return msg, inserted
}

func mirrorTestAsk(ref string, coord *ForgeCoord) Ask {
	return Ask{
		Ref:      ref,
		RefCoord: coord,
		Questions: []AskQuestion{{
			QuestionID: "q1",
			Question:   "Question?",
			Options:    []AskOption{{ID: "yes", Label: "Yes"}},
		}},
	}
}

func mirrorRowCount(t *testing.T, s *Store, ctx context.Context, agent AccountID) int {
	t.Helper()
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM forge_mirrors WHERE agent_account_id = $1`, string(agent)).Scan(&count); err != nil {
		t.Fatalf("count forge mirrors: %v", err)
	}
	return count
}

func TestAskMirrorAppendAutoManualOffUserAndRetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-append-owner")
	agent := mustAgent(t, s, owner.ID, "mirror-append-agent")
	coord := ForgeCoord{Provider: ForgeProviderLinear, Host: "linear.app", Repo: "RIG", Number: 1}
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Enabled: true}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}

	for _, tc := range []struct {
		name string
		mode ForgeMirrorMode
		want int
	}{
		{name: "auto", mode: ForgeMirrorAuto, want: 1},
		{name: "manual", mode: ForgeMirrorManual},
		{name: "off", mode: ForgeMirrorOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: tc.mode}); err != nil {
				t.Fatalf("SetForgeMirrorSetting: %v", err)
			}
			msg, inserted := appendAskForMirrorTest(t, ctx, s, agent.ID, mirrorTestAsk("RIG-12", &coord), "")
			if !inserted {
				t.Fatal("AppendMessage inserted=false, want true")
			}
			rows, err := s.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
			if err != nil {
				t.Fatalf("AskForgeMirrors: %v", err)
			}
			if len(rows) != tc.want {
				t.Fatalf("ask mirror count = %d, want %d", len(rows), tc.want)
			}
			if tc.want == 1 {
				row := rows[0]
				if row.Source != 1 || row.AskID != msg.Blocks[0].Ask.AskID || row.Actor != agent.ID || row.Agent != agent.ID || row.TargetRef != "RIG-12" || row.BodyRev != 1 || row.State != 1 {
					t.Fatalf("auto mirror row = %+v, unexpected fields", row)
				}
			}
		})
	}

	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: ForgeMirrorMode(-1)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative mode error = %v, want ErrInvalidArgument", err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(auto): %v", err)
	}
	userMsg, inserted := appendAskForMirrorTest(t, ctx, s, owner.ID, mirrorTestAsk("RIG-12", &coord), "user-ask")
	if !inserted {
		t.Fatal("AppendMessage(user) inserted=false, want true")
	}
	rows, err := s.AskForgeMirrors(ctx, []string{userMsg.Blocks[0].Ask.AskID})
	if err != nil {
		t.Fatalf("AskForgeMirrors(user): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("user-authored ask created %d mirror rows, want none", len(rows))
	}

	retryCoord := ForgeCoord{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: retryCoord.Provider, Host: retryCoord.Host, Repo: retryCoord.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(retry): %v", err)
	}
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: retryCoord.Provider, Host: retryCoord.Host, Repo: retryCoord.Repo, Enabled: true}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription(retry): %v", err)
	}
	ask := mirrorTestAsk("owner/repo#12", &retryCoord)
	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "mirror-retry-" + newID(), Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{owner.ID, agent.ID}})
	if err != nil {
		t.Fatalf("CreateChannel(retry): %v", err)
	}
	msg, inserted, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{{Ask: &ask}}}, string(ch.ID), TopicRef{Name: testTopic, Create: true}, "same-append-request")
	if err != nil || !inserted {
		t.Fatalf("AppendMessage(first) = (%t, %v), want inserted", inserted, err)
	}
	_, inserted, err = s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{{Ask: &ask}}}, string(ch.ID), TopicRef{Name: testTopic, Create: true}, "same-append-request")
	if err != nil {
		t.Fatalf("AppendMessage(retry): %v", err)
	}
	if inserted {
		t.Fatal("retry AppendMessage inserted=true, want false")
	}
	rows, err = s.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
	if err != nil {
		t.Fatalf("AskForgeMirrors(retry): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("retry created %d mirror rows, want one", len(rows))
	}
}

func TestAskMirrorAnswerBumpsPendingRevision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-answer-owner")
	agent := mustAgent(t, s, owner.ID, "mirror-answer-agent")
	coord := ForgeCoord{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "mirror-answer-" + newID(), Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{owner.ID, agent.ID}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{{Ask: &Ask{
		Ref: "owner/repo#12", RefCoord: &coord,
		Questions: []AskQuestion{{QuestionID: "q1", Question: "Question?", Options: []AskOption{{ID: "yes", Label: "Yes"}}}},
	}}}}, string(ch.ID), TopicRef{Name: testTopic, Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	askID := msg.Blocks[0].Ask.AskID
	if _, _, err := s.AnswerAsk(ctx, owner.ID, askID, []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"yes"}}}); err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}
	rows, err := s.AskForgeMirrors(ctx, []string{askID})
	if err != nil {
		t.Fatalf("AskForgeMirrors: %v", err)
	}
	if len(rows) != 1 || rows[0].BodyRev != 2 || rows[0].State != 1 {
		t.Fatalf("answer mirror rows = %+v, want one body_rev=2 pending row", rows)
	}
}

func TestClaimForgeMirrorConflictKeys(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-claim")
	base := ForgeMirrorRow{Source: 1, AskID: "claim-ask", Actor: agent, Agent: agent, TargetRef: "RIG-1", BodyRev: 1, State: 1}
	created, ok, err := s.ClaimForgeMirror(ctx, base)
	if err != nil || !ok {
		t.Fatalf("ClaimForgeMirror(first) = (%+v, %t, %v), want created", created, ok, err)
	}
	conflict := base
	conflict.TargetRef = "RIG-2"
	got, ok, err := s.ClaimForgeMirror(ctx, conflict)
	if err != nil || ok || got.ID != created.ID || got.TargetRef != "RIG-1" {
		t.Fatalf("ask conflict = (%+v, %t, %v), want stored row unchanged", got, ok, err)
	}

	request := ForgeMirrorRow{Source: 2, SourceKey: "request-key", Actor: owner, Agent: agent, ClientRequestID: "client-1", TargetRef: "owner/repo#1", BodyRev: 1, State: 1}
	requestRow, ok, err := s.ClaimForgeMirror(ctx, request)
	if err != nil || !ok {
		t.Fatalf("ClaimForgeMirror(request) = (%+v, %t, %v), want created", requestRow, ok, err)
	}
	request.ClientRequestID = "client-1"
	request.TargetRef = "owner/repo#2"
	got, ok, err = s.ClaimForgeMirror(ctx, request)
	if err != nil || ok || got.ID != requestRow.ID || got.TargetRef != "owner/repo#1" {
		t.Fatalf("client request conflict = (%+v, %t, %v), want stored row unchanged", got, ok, err)
	}

	source := ForgeMirrorRow{Source: 2, SourceKey: "topic-key", Actor: owner, Agent: agent, TargetRef: "owner/repo#3", BodyRev: 1, State: 2}
	sourceRow, ok, err := s.ClaimForgeMirror(ctx, source)
	if err != nil || !ok {
		t.Fatalf("ClaimForgeMirror(source) = (%+v, %t, %v), want created", sourceRow, ok, err)
	}
	source.BodyRev = 1
	source.State = 1
	got, ok, err = s.ClaimForgeMirror(ctx, source)
	if err != nil || ok || got.ID != sourceRow.ID || got.BodyRev != 2 || got.State != 1 {
		t.Fatalf("source-key conflict = (%+v, %t, %v), want same row requeued at rev 2", got, ok, err)
	}
	if n := mirrorRowCount(t, s, ctx, agent); n != 3 {
		t.Fatalf("mirror row count = %d, want 3", n)
	}
}

func TestForgeMirrorLeasesAttemptsAndMarks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-lease")
	row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{Source: 2, SourceKey: "lease-key", Actor: owner, Agent: agent, TargetRef: "owner/repo#1", BodyRev: 1, State: 1})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: (%+v, %t, %v)", row, created, err)
	}
	leased, ok, err := s.LeaseForgeMirror(ctx, row.ID, "token-1", time.Minute)
	if err != nil || !ok || leased.LeaseToken != "token-1" {
		t.Fatalf("LeaseForgeMirror(first) = (%+v, %t, %v)", leased, ok, err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "token-2", time.Minute); err != nil || ok {
		t.Fatalf("LeaseForgeMirror(held) = (%t, %v), want false", ok, err)
	}
	if ok, err := s.RenewForgeMirror(ctx, row.ID, "stale", time.Minute); err != nil || ok {
		t.Fatalf("RenewForgeMirror(stale) = (%t, %v), want false", ok, err)
	}
	firstAttempt, ok, err := s.BeginForgeMirrorAttempt(ctx, row.ID, "token-1")
	if err != nil || !ok || firstAttempt.Attempts != 1 || firstAttempt.FirstAttemptAt == nil {
		t.Fatalf("BeginForgeMirrorAttempt(first) = (%+v, %t, %v)", firstAttempt, ok, err)
	}
	secondAttempt, ok, err := s.BeginForgeMirrorAttempt(ctx, row.ID, "token-1")
	if err != nil || !ok || secondAttempt.Attempts != 2 || secondAttempt.FirstAttemptAt == nil || secondAttempt.LastAttemptAt == nil || !secondAttempt.FirstAttemptAt.Equal(*firstAttempt.FirstAttemptAt) {
		t.Fatalf("BeginForgeMirrorAttempt(second) = (%+v, %t, %v)", secondAttempt, ok, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	leased, ok, err = s.LeaseForgeMirror(ctx, row.ID, "token-2", time.Minute)
	if err != nil || !ok || leased.LeaseToken != "token-2" {
		t.Fatalf("LeaseForgeMirror(expired) = (%+v, %t, %v)", leased, ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "token-1", ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 11, CommentKey: "key", URL: "url"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkForgeMirror(stale) = %v, want ErrLeaseLost", err)
	}
	assertMirrorDelivery(t, s, row.ID, 1, "", "token-2")
	if err := s.MarkForgeMirror(ctx, row.ID, "token-2", ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 11, CommentKey: "key", URL: "url", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror(valid): %v", err)
	}
	assertMirrorDelivery(t, s, row.ID, 2, "key", "")
	if done, err := s.AskForgeMirrors(ctx, nil); err != nil || len(done) != 0 {
		t.Fatalf("AskForgeMirrors(empty) = (%d, %v), want none", len(done), err)
	}
}

func assertMirrorDelivery(t *testing.T, s *Store, id string, state int16, commentKey, token string) {
	t.Helper()
	var gotState int16
	var gotKey, gotToken string
	if err := s.pool.QueryRow(context.Background(), `SELECT state, comment_key, lease_token FROM forge_mirrors WHERE id = $1`, id).Scan(&gotState, &gotKey, &gotToken); err != nil {
		t.Fatalf("read forge mirror %s: %v", id, err)
	}
	if gotState != state || gotKey != commentKey || gotToken != token {
		t.Fatalf("forge mirror %s = state %d key %q token %q, want %d %q %q", id, gotState, gotKey, gotToken, state, commentKey, token)
	}
}

// OpenForgeMirrors lists pending and failed rows, plus done rows once recheck is due.
func TestForgeMirrorOpenRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-lease")
	row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{Source: 2, SourceKey: "lease-key", Actor: owner, Agent: agent, TargetRef: "owner/repo#1", BodyRev: 1, State: 1})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: (%+v, %t, %v)", row, created, err)
	}
	pendingRow, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{Source: 2, SourceKey: "pending-key", Actor: owner, Agent: agent, TargetRef: "owner/repo#2", BodyRev: 1, State: 1})
	if err != nil {
		t.Fatalf("ClaimForgeMirror(pending): %v", err)
	}
	failedRow, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{Source: 2, SourceKey: "failed-key", Actor: owner, Agent: agent, TargetRef: "owner/repo#3", BodyRev: 1, State: 1})
	if err != nil {
		t.Fatalf("ClaimForgeMirror(failed): %v", err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, failedRow.ID, "failed-token", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(failed): (%t, %v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, failedRow.ID, "failed-token", ForgeMirrorMark{State: 3, Err: "failed"}); err != nil {
		t.Fatalf("MarkForgeMirror(failed): %v", err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "token-2", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: (%t, %v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "token-2", ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 11, CommentKey: "key", URL: "url", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror: %v", err)
	}
	open, err := s.OpenForgeMirrors(ctx, agent)
	if err != nil || len(open) != 2 || open[0].ID != pendingRow.ID || open[0].State != 1 || open[1].ID != failedRow.ID || open[1].State != 3 {
		t.Fatalf("OpenForgeMirrors(pending and failed) = (%+v, %v), want the pending and failed rows", open, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire recheck: %v", err)
	}
	open, err = s.OpenForgeMirrors(ctx, agent)
	if err != nil || len(open) != 3 || open[0].ID != row.ID || open[1].ID != pendingRow.ID || open[2].ID != failedRow.ID || open[0].LeaseToken != "" || open[0].CommentKey != "key" {
		t.Fatalf("OpenForgeMirrors(past recheck) = (%+v, %v), want the done, pending, and failed rows", open, err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "token-4", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(past recheck) = (%t, %v), want true", ok, err)
	}
}

func TestForgeMirrorSettingsAndSubscriptionList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-settings-owner")
	enabled := ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/enabled", Enabled: true}
	disabled := ForgeRepoSubscription{Provider: ForgeProviderLinear, Host: "linear.app", Repo: "DISABLED", Enabled: false}
	if err := s.EnsureForgeRepoSubscription(ctx, enabled); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription(enabled): %v", err)
	}
	if err := s.EnsureForgeRepoSubscription(ctx, disabled); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription(disabled): %v", err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: disabled.Provider, Host: disabled.Host, Repo: disabled.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(disabled): %v", err)
	}
	settings, err := s.ListForgeMirrorSettings(ctx)
	if err != nil || len(settings) != 1 || settings[0].Mode != ForgeMirrorOff || settings[0].Repo != enabled.Repo {
		t.Fatalf("ListForgeMirrorSettings(default off) = (%+v, %v), want enabled repo only", settings, err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: enabled.Provider, Host: enabled.Host, Repo: enabled.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(auto): %v", err)
	}
	settings, err = s.ListForgeMirrorSettings(ctx)
	if err != nil || len(settings) != 1 || settings[0].Mode != ForgeMirrorAuto {
		t.Fatalf("ListForgeMirrorSettings(auto) = (%+v, %v), want Auto", settings, err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: enabled.Provider, Host: enabled.Host, Repo: enabled.Repo, Mode: ForgeMirrorOff}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(off): %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM forge_mirror_settings WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3`, int16(enabled.Provider), enabled.Host, enabled.Repo).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Off left %d setting rows (err=%v), want zero", count, err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: enabled.Provider, Host: enabled.Host, Repo: enabled.Repo, Mode: ForgeMirrorMode(3)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid mode error = %v, want ErrInvalidArgument", err)
	}
	if mode, err := s.ForgeMirrorMode(ctx, enabled.Provider, enabled.Host, enabled.Repo); err != nil || mode != ForgeMirrorOff {
		t.Fatalf("ForgeMirrorMode(after off) = (%d, %v), want Off", mode, err)
	}
}

func TestForgeMirrorTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA := seedTenant(t, s, "mirror-tenant-a")
	ctxA := WithTenant(context.Background(), tenantA)
	ctxB := WithTenant(context.Background(), seedTenant(t, s, "mirror-tenant-b"))
	owner, err := s.CreateUser(ctxA, NewUser{Handle: "mirror-tenant-owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(A): %v", err)
	}
	agent, err := s.CreateAgent(ctxA, owner.ID, NewAgent{Handle: "mirror-tenant-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent(A): %v", err)
	}
	coord := ForgeCoord{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}
	setting := ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: ForgeMirrorAuto}
	if err := s.SetForgeMirrorSetting(ctxA, owner.ID, setting); err != nil {
		t.Fatalf("SetForgeMirrorSetting(A): %v", err)
	}
	row, created, err := s.ClaimForgeMirror(ctxA, ForgeMirrorRow{Source: 2, SourceKey: "tenant-key", AskID: "tenant-ask", Actor: owner.ID, Agent: agent.ID, TargetRef: "owner/repo#1", BodyRev: 1, State: 1})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(A): (%+v, %t, %v)", row, created, err)
	}
	if mode, err := s.ForgeMirrorMode(ctxB, coord.Provider, coord.Host, coord.Repo); err != nil || mode != ForgeMirrorOff {
		t.Fatalf("ForgeMirrorMode(B) = (%d, %v), want Off", mode, err)
	}
	if rows, err := s.OpenForgeMirrors(ctxB, agent.ID); err != nil || len(rows) != 0 {
		t.Fatalf("OpenForgeMirrors(B) = (%+v, %v), want no rows", rows, err)
	}
	if rows, err := s.AskForgeMirrors(ctxB, nil); err != nil || len(rows) != 0 {
		t.Fatalf("AskForgeMirrors(B) = (%+v, %v), want no rows", rows, err)
	}
}

func TestAskForForgeMirrorRequiresParticipationAndReturnsAnswerer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "ask-mirror-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "ask-mirror-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	message, inserted := appendAskForMirrorTest(t, ctx, s, agent.ID, mirrorTestAsk("RIG-12", nil), "ask-mirror-request")
	if !inserted {
		t.Fatal("AppendMessage inserted=false")
	}
	askID := message.Blocks[0].Ask.AskID
	if _, _, err := s.AnswerAsk(ctx, owner.ID, askID, []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"yes"}}}); err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}
	got, err := s.AskForForgeMirror(ctx, agent.ID, askID)
	if err != nil {
		t.Fatalf("AskForForgeMirror: %v", err)
	}
	if got.Message.ID != message.ID || got.Channel == "" || got.Topic == "" || got.AnsweredBy != "ask-mirror-owner" {
		t.Fatalf("AskForForgeMirror = %+v, want original ask, source names, and owner answerer", got)
	}
	stranger, err := s.CreateUser(ctx, NewUser{Handle: "ask-mirror-stranger", DisplayName: "Stranger"})
	if err != nil {
		t.Fatalf("CreateUser(stranger): %v", err)
	}
	if _, err := s.AskForForgeMirror(ctx, stranger.ID, askID); err == nil {
		t.Fatal("AskForForgeMirror allowed an account outside the channel")
	}
	if _, err := s.AskForForgeMirror(ctx, agent.ID, "missing-ask"); err == nil {
		t.Fatal("AskForForgeMirror returned no error for a missing ask")
	}
}
func TestMarkForgeMirrorKeepsPendingAfterRevisionBump(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-revision-race")
	row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "revision-race", Actor: owner, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror = (created=%v, err=%v), want created row", created, err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "revision-token", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror = (ok=%v, err=%v), want leased row", ok, err)
	}
	if _, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "revision-race", Actor: owner, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	}); err != nil {
		t.Fatalf("ClaimForgeMirror(revision bump): %v", err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "revision-token", ForgeMirrorMark{
		PostedRev: 1, State: 2, CommentID: 7, CommentKey: "comment",
	}); err != nil {
		t.Fatalf("MarkForgeMirror: %v", err)
	}
	var bodyRev, postedRev, state int16
	if err := s.pool.QueryRow(ctx, `SELECT body_rev, posted_rev, state FROM forge_mirrors WHERE id = $1`, row.ID).Scan(&bodyRev, &postedRev, &state); err != nil {
		t.Fatalf("read forge mirror: %v", err)
	}
	if bodyRev != 2 || postedRev != 1 || state != 1 {
		t.Fatalf("mirror revision state = (%d, %d, %d), want (2, 1, 1)", bodyRev, postedRev, state)
	}
}

func TestMarkForgeMirrorPreservesRecheckUntilExplicitClear(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-recheck-preserve")
	row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "recheck-preserve", Actor: owner, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror = (created=%v, err=%v), want created row", created, err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "recheck-token-1", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(first) = (ok=%v, err=%v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "recheck-token-1", ForgeMirrorMark{
		PostedRev: 1, State: 2, CommentID: 7, CommentKey: "comment", Recheck: true,
	}); err != nil {
		t.Fatalf("MarkForgeMirror(schedule recheck): %v", err)
	}
	if _, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "recheck-preserve", Actor: owner, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	}); err != nil {
		t.Fatalf("ClaimForgeMirror(revision bump): %v", err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "recheck-token-2", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(second) = (ok=%v, err=%v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "recheck-token-2", ForgeMirrorMark{
		PostedRev: 2, State: 2, CommentID: 7, CommentKey: "comment",
	}); err != nil {
		t.Fatalf("MarkForgeMirror(delivery): %v", err)
	}
	var recheckSet bool
	if err := s.pool.QueryRow(ctx, `SELECT recheck_at IS NOT NULL FROM forge_mirrors WHERE id = $1`, row.ID).Scan(&recheckSet); err != nil {
		t.Fatalf("read recheck state: %v", err)
	}
	if !recheckSet {
		t.Fatal("delivery cleared a scheduled recheck")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("make scheduled recheck due: %v", err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "recheck-token-3", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(third) = (ok=%v, err=%v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "recheck-token-3", ForgeMirrorMark{
		PostedRev: 2, State: 2, CommentID: 7, CommentKey: "comment", ClearRecheck: true,
	}); err != nil {
		t.Fatalf("MarkForgeMirror(clear recheck): %v", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT recheck_at IS NOT NULL FROM forge_mirrors WHERE id = $1`, row.ID).Scan(&recheckSet); err != nil {
		t.Fatalf("read cleared recheck state: %v", err)
	}
	if recheckSet {
		t.Fatal("explicit recheck clear left a scheduled recheck")
	}
}

func TestClaimForgeMirrorSourceKeyDoesNotCrossAgentClass(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-class-conflict")
	manual, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "manual-class", Actor: owner,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(manual) = (created=%v, err=%v)", created, err)
	}
	otherOwner, err := s.CreateUser(ctx, NewUser{Handle: "mirror-class-other-owner", DisplayName: "Other"})
	if err != nil {
		t.Fatalf("CreateUser(other owner): %v", err)
	}
	agentClaim, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "manual-class", Actor: otherOwner.ID, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || created || agentClaim.ID != manual.ID || agentClaim.Agent != "" || agentClaim.Actor != owner || agentClaim.BodyRev != 1 {
		t.Fatalf("other actor conflict = (%+v, created=%v, err=%v), want unchanged row", agentClaim, created, err)
	}
	agentClassClaim, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "manual-class", Actor: agent, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || created || agentClassClaim.ID != manual.ID || agentClassClaim.Agent != "" || agentClassClaim.BodyRev != 1 {
		t.Fatalf("agent class conflict = (%+v, created=%v, err=%v), want unchanged Manual row", agentClassClaim, created, err)
	}
	automatic, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "automatic-class", Actor: agent, Agent: agent,
		TargetRef: "owner/repo#2", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(auto) = (created=%v, err=%v)", created, err)
	}
	userClaim, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "automatic-class", Actor: owner,
		TargetRef: "owner/repo#2", BodyRev: 1, State: 1,
	})
	if err != nil || created || userClaim.ID != automatic.ID || userClaim.Agent != agent || userClaim.Actor != agent || userClaim.BodyRev != 1 {
		t.Fatalf("user conflict = (%+v, created=%v, err=%v), want unchanged Auto row", userClaim, created, err)
	}
}

func TestClaimForgeMirrorSourceTwoRequiresSourceKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, owner := seedAgent(t, s, "mirror-source-key-required")
	_, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, Actor: owner, ClientRequestID: "request-only",
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ClaimForgeMirror(request key only) error = %v, want ErrInvalidArgument", err)
	}
}

// A claim stores the session and payload; a re-mirror of the same source
// replaces the payload and session and bumps the revision as an edit does.
func TestClaimForgeMirrorStoresSessionAndPayloadAndRemirrorUpdatesThem(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-payload")
	first, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "topic-1", Actor: agent, Agent: agent, ClientRequestID: "call-1",
		TargetRef: "owner/repo#3", BodyRev: 1, State: 1, SessionID: "sess-a", SourcePayload: []byte(`{"note":"one"}`),
	})
	if err != nil || !created || first.SessionID != "sess-a" || string(first.SourcePayload) != `{"note": "one"}` {
		t.Fatalf("ClaimForgeMirror(first) = (%+v, created=%v, err=%v), want stored session and payload", first, created, err)
	}
	second, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "topic-1", Actor: agent, Agent: agent, ClientRequestID: "call-2",
		TargetRef: "owner/repo#3", BodyRev: 1, State: 1, SessionID: "sess-b", SourcePayload: []byte(`{"note":"two"}`),
	})
	if err != nil || created || second.ID != first.ID || second.BodyRev != 2 || second.SessionID != "sess-b" || string(second.SourcePayload) != `{"note": "two"}` {
		t.Fatalf("ClaimForgeMirror(re-mirror) = (%+v, created=%v, err=%v), want bumped row with the new session and payload", second, created, err)
	}
}

// A retry of the re-mirror key finds the row and does not bump it again.
func TestRemirrorRequestKeyIsRecordedOnce(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-request-key")
	claim := func(key string) ForgeMirrorRow {
		t.Helper()
		row, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
			Source: 2, SourceKey: "topic-k", Actor: agent, Agent: agent, ClientRequestID: key,
			TargetRef: "owner/repo#5", BodyRev: 1, State: 1, SessionID: "sess-k", SourcePayload: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("ClaimForgeMirror(%s): %v", key, err)
		}
		return row
	}
	first := claim("call-1")
	if bumped := claim("call-2"); bumped.BodyRev != 2 {
		t.Fatalf("re-mirror body_rev = %d, want 2", bumped.BodyRev)
	}
	if again := claim("call-2"); again.BodyRev != 2 {
		t.Fatalf("re-mirror retry body_rev = %d, want unchanged 2", again.BodyRev)
	}
	for _, key := range []string{"call-1", "call-2"} {
		row, found, err := s.ForgeMirrorByRequest(ctx, agent, key)
		if err != nil || !found || row.ID != first.ID {
			t.Fatalf("ForgeMirrorByRequest(%s) = (%+v, found=%v, err=%v), want row %s", key, row, found, err, first.ID)
		}
	}
	if _, found, err := s.ForgeMirrorByRequest(ctx, agent, "call-unused"); err != nil || found {
		t.Fatalf("ForgeMirrorByRequest(unused) = (found=%v, err=%v), want not found", found, err)
	}
}

func TestMarkForgeMirrorStoresFailureCode(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-error-code")
	row, _, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "topic-e", Actor: agent, Agent: agent, TargetRef: "owner/repo#6", BodyRev: 1, State: 1,
	})
	if err != nil {
		t.Fatalf("ClaimForgeMirror: %v", err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "code-token", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror = (ok=%v, err=%v)", ok, err)
	}
	if err := s.MarkForgeMirror(ctx, row.ID, "code-token", ForgeMirrorMark{State: 3, Err: "gone", ErrCode: "not_found"}); err != nil {
		t.Fatalf("MarkForgeMirror: %v", err)
	}
	got, err := s.ForgeMirrorByID(ctx, row.ID)
	if err != nil || got.LastError != "gone" || got.LastErrorCode != "not_found" {
		t.Fatalf("ForgeMirrorByID = (%+v, %v), want the stored error and code", got, err)
	}
}

// The old cross-table race, driven in order: a re-mirror reserves a key for
// source A, then a new claim on source B arrives with the same key. The claim
// must resolve to A instead of inserting a second row for the key.
func TestRequestKeyReservedByRemirrorBlocksLaterClaim(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-key-race")
	claim := func(source, key string) (ForgeMirrorRow, bool) {
		t.Helper()
		row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
			Source: 2, SourceKey: source, Actor: agent, Agent: agent, ClientRequestID: key,
			TargetRef: "owner/repo#7", BodyRev: 1, State: 1, SessionID: "sess-r", SourcePayload: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("ClaimForgeMirror(%s, %s): %v", source, key, err)
		}
		return row, created
	}
	a, _ := claim("topic-a", "call-a")
	if remirrored, created := claim("topic-a", "call-shared"); created || remirrored.ID != a.ID {
		t.Fatalf("re-mirror = (%s, created=%v), want row %s", remirrored.ID, created, a.ID)
	}
	late, created := claim("topic-b", "call-shared")
	if created || late.ID != a.ID {
		t.Fatalf("late claim = (%s, created=%v), want the reserved row %s", late.ID, created, a.ID)
	}
	if n := mirrorRowCount(t, s, ctx, agent); n != 1 {
		t.Fatalf("mirror row count = %d, want 1", n)
	}
	for range 3 {
		got, found, err := s.ForgeMirrorByRequest(ctx, agent, "call-shared")
		if err != nil || !found || got.ID != a.ID {
			t.Fatalf("ForgeMirrorByRequest(call-shared) = (%s, found=%v, err=%v), want %s", got.ID, found, err, a.ID)
		}
	}
	// The reverse order: a claim reserves the key, then a re-mirror of another
	// source with that key resolves to the claim's row without bumping it.
	b, _ := claim("topic-c", "call-c")
	if again, created := claim("topic-a", "call-c"); created || again.ID != b.ID {
		t.Fatalf("re-mirror with a claimed key = (%s, created=%v), want %s", again.ID, created, b.ID)
	}
	if after, err := s.ForgeMirrorByID(ctx, a.ID); err != nil || after.BodyRev != 2 {
		t.Fatalf("row A body_rev = %d (err %v), want 2 (one re-mirror only)", after.BodyRev, err)
	}
}

// The window ClaimForgeMirror's early lookup cannot close: a racing claim
// reserves the key after this call's lookup missed. Calling claimConflict
// directly with the reservation already present drives the re-mirror's
// recorded == 0 path, which returns the reserved row as ClaimReserved.
func TestClaimConflictResolvesToRowReservedAfterLookup(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-key-window")
	row := func(source, key string) ForgeMirrorRow {
		return ForgeMirrorRow{
			Source: 2, SourceKey: source, Actor: agent, Agent: agent, ClientRequestID: key,
			TargetRef: "owner/repo#8", BodyRev: 1, State: 1, SessionID: "sess-w", SourcePayload: []byte(`{}`),
		}
	}
	loser, created, err := s.ClaimForgeMirror(ctx, row("topic-loser", "call-loser"))
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(loser) = (created=%v, err=%v)", created, err)
	}
	winner, created, err := s.ClaimForgeMirror(ctx, row("topic-winner", "call-winner"))
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(winner) = (created=%v, err=%v)", created, err)
	}
	// The racing call's reservation lands after this call's lookup missed. The
	// raw handle has no tenant GUC, so the row's tenant is copied explicitly.
	tag, err := s.pool.Exec(ctx, `INSERT INTO forge_mirror_requests (agent_account_id, client_request_id, mirror_id, tenant_id)
		SELECT $1, $2, id, tenant_id FROM forge_mirrors WHERE id = $3`,
		string(agent), "call-raced", winner.ID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reserve call-raced = (%d rows, err %v), want 1 row", tag.RowsAffected(), err)
	}
	got, resolution, err := s.claimConflict(ctx, row("topic-loser", "call-raced"))
	if err != nil || resolution != ClaimReserved || got.ID != winner.ID {
		t.Fatalf("claimConflict = (%s, %v, err=%v), want the reserving row %s reserved", got.ID, resolution, err, winner.ID)
	}
	after, err := s.ForgeMirrorByID(ctx, loser.ID)
	if err != nil || after.BodyRev != 1 {
		t.Fatalf("losing source row body_rev = %d (err %v), want unbumped 1", after.BodyRev, err)
	}
	if reserved, found, err := s.ForgeMirrorByRequest(ctx, agent, "call-raced"); err != nil || !found || reserved.ID != winner.ID {
		t.Fatalf("ForgeMirrorByRequest(call-raced) = (%s, found=%v, err=%v), want %s", reserved.ID, found, err, winner.ID)
	}
}

// A keyed claim's lookup misses, then a racing call inserts its own row under
// the same key. claimConflict, called directly after that insert, must find
// the racer's row by its request key and report it Reserved, not a conflict.
func TestClaimConflictResolvesRacingKeyedInsertAsReserved(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-key-insert")
	row := func(source, key string) ForgeMirrorRow {
		return ForgeMirrorRow{
			Source: 2, SourceKey: source, Actor: agent, Agent: agent, ClientRequestID: key,
			TargetRef: "owner/repo#9", BodyRev: 1, State: 1, SessionID: "sess-i", SourcePayload: []byte(`{}`),
		}
	}
	loser, created, err := s.ClaimForgeMirror(ctx, row("topic-loser", "call-loser"))
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(loser) = (created=%v, err=%v)", created, err)
	}
	racer, created, err := s.ClaimForgeMirror(ctx, row("topic-racer", "call-shared"))
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(racer) = (created=%v, err=%v)", created, err)
	}
	got, resolution, err := s.claimConflict(ctx, row("topic-loser", "call-shared"))
	if err != nil || resolution != ClaimReserved || got.ID != racer.ID {
		t.Fatalf("claimConflict = (%s, %v, err=%v), want the racer's row %s reserved", got.ID, resolution, err, racer.ID)
	}
	if after, err := s.ForgeMirrorByID(ctx, loser.ID); err != nil || after.BodyRev != 1 {
		t.Fatalf("losing source row body_rev = %d (err %v), want unbumped 1", after.BodyRev, err)
	}
}

// Two same-key re-mirrors of one source interleave: both missed the key
// lookup, and the winner reserved the key and bumped the row. The loser, via
// claimConflict, reaches remirrorSource with bumped=false and must not bump.
func TestClaimConflictSameSourceRemirrorLoserIsReserved(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "mirror-remirror-race")
	row := func(key string) ForgeMirrorRow {
		return ForgeMirrorRow{
			Source: 2, SourceKey: "topic-r", Actor: agent, Agent: agent, ClientRequestID: key,
			TargetRef: "owner/repo#10", BodyRev: 1, State: 1, SessionID: "sess-r", SourcePayload: []byte(`{}`),
		}
	}
	first, created, err := s.ClaimForgeMirror(ctx, row("call-first"))
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror(first) = (created=%v, err=%v)", created, err)
	}
	won, resolution, err := s.ClaimForgeMirrorResolved(ctx, row("call-again"))
	if err != nil || resolution != ClaimRemirrored || won.ID != first.ID || won.BodyRev != 2 {
		t.Fatalf("winning re-mirror = (%+v, %v, err=%v), want %s re-mirrored at body_rev 2", won, resolution, err, first.ID)
	}
	lost, resolution, err := s.claimConflict(ctx, row("call-again"))
	if err != nil || resolution != ClaimReserved || lost.ID != first.ID || lost.BodyRev != 2 {
		t.Fatalf("losing re-mirror = (%+v, %v, err=%v), want %s reserved at body_rev 2", lost, resolution, err, first.ID)
	}
	if after, err := s.ForgeMirrorByID(ctx, first.ID); err != nil || after.BodyRev != 2 {
		t.Fatalf("row body_rev = %d (err %v) after the losing re-mirror, want 2", after.BodyRev, err)
	}
}

// An auto ask row takes its session from the posting ctx; a post with no
// session on ctx stores an empty one for the poster to refuse.
func TestAskMirrorRowTakesSessionFromContext(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-session-owner")
	agent := mustAgent(t, s, owner.ID, "mirror-session-agent")
	coord := ForgeCoord{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/session", Number: 4}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	for _, tc := range []struct{ name, session string }{{"relayed", "sess-relay"}, {"no session", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			postCtx := ctx
			if tc.session != "" {
				postCtx = WithAgentSession(ctx, tc.session)
			}
			msg, _ := appendAskForMirrorTest(t, postCtx, s, agent.ID, mirrorTestAsk("owner/session#4", &coord), "")
			rows, err := s.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
			if err != nil || len(rows) != 1 || rows[0].SessionID != tc.session {
				t.Fatalf("AskForgeMirrors = (%+v, %v), want one row with session %q", rows, err, tc.session)
			}
		})
	}
}

func TestForgeMirrorExpiredLeaseFencesAttemptsAndMarks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "mirror-expired-lease")
	row, created, err := s.ClaimForgeMirror(ctx, ForgeMirrorRow{
		Source: 2, SourceKey: "expired-lease", Actor: owner, Agent: agent,
		TargetRef: "owner/repo#1", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror = (created=%v, err=%v)", created, err)
	}
	if _, ok, err := s.LeaseForgeMirror(ctx, row.ID, "expired-token", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror = (ok=%v, err=%v)", ok, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, ok, err := s.BeginForgeMirrorAttempt(ctx, row.ID, "expired-token"); err != nil || ok {
		t.Fatalf("BeginForgeMirrorAttempt(expired) = (ok=%v, err=%v), want false", ok, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() + interval '1 minute' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("renew test lease: %v", err)
	}
	if _, ok, err := s.BeginForgeMirrorAttempt(ctx, row.ID, "expired-token"); err != nil || !ok {
		t.Fatalf("BeginForgeMirrorAttempt(live) = (ok=%v, err=%v), want true", ok, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire lease before mark: %v", err)
	}
	err = s.MarkForgeMirror(ctx, row.ID, "expired-token", ForgeMirrorMark{PostedRev: 1, State: 2})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkForgeMirror(expired) error = %v, want ErrLeaseLost", err)
	}
}

func TestAskMirrorInvalidCoordinateDoesNotRejectMessage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-invalid-coordinate-owner")
	agent := mustAgent(t, s, owner.ID, "mirror-invalid-coordinate-agent")
	coord := ForgeCoord{Provider: ForgeProviderGitHub, Host: "", Repo: "owner/repo"}
	msg, inserted := appendAskForMirrorTest(t, ctx, s, agent.ID, mirrorTestAsk("owner/repo#1", &coord), "invalid-coordinate")
	if !inserted || msg.ID == "" {
		t.Fatalf("AppendMessage = (inserted=%v, id=%q), want persisted message", inserted, msg.ID)
	}
	rows, err := s.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("AskForgeMirrors(invalid coordinate) = (%+v, %v), want no mirror rows", rows, err)
	}
}

func TestForgeMirrorSettingsNormalizeMixedCaseGitHubRepo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "mirror-settings-case-owner")
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/MixedCase", Enabled: true,
	}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}
	if err := s.SetForgeMirrorSetting(ctx, owner.ID, ForgeMirrorSetting{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/MixedCase", Mode: ForgeMirrorAuto,
	}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	settings, err := s.ListForgeMirrorSettings(ctx)
	if err != nil {
		t.Fatalf("ListForgeMirrorSettings: %v", err)
	}
	for _, setting := range settings {
		if setting.Provider == ForgeProviderGitHub && setting.Repo == "owner/mixedcase" {
			if setting.Mode != ForgeMirrorAuto {
				t.Fatalf("mixed-case subscription mode = %d, want Auto", setting.Mode)
			}
			return
		}
	}
	t.Fatalf("ListForgeMirrorSettings() = %+v, want normalized owner/mixedcase with Auto mode", settings)
}
