//go:build pgtest

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
)

func writeIntentFixture(t *testing.T, s *Store, handle, key, head string) WriteIntent {
	t.Helper()
	agent, owner := seedAgent(t, s, handle)
	return WriteIntent{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Repo",
		Kind: forge.WriteKindPullRequest, HeadRef: head,
		AgentAccountID: agent, OwnerUserID: owner,
		SessionID: "session-" + handle, RequestKey: key,
	}
}

func TestRecordWriteIntentIdempotencyAndHeadConflict(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-idempotency", "key-one", "feature/shared")

	first, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}
	retry, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent retry: %v", err)
	}
	if retry.ID != first.ID || retry != first {
		t.Fatalf("retry = %+v, want original intent %+v", retry, first)
	}

	other := writeIntentFixture(t, s, "intent-head-other", "key-two", in.HeadRef)
	if _, err := s.RecordWriteIntent(t.Context(), other); !errors.Is(err, ErrConflict) {
		t.Fatalf("another agent's PR intent for the same head = %v, want ErrConflict", err)
	}
}

func TestRecordWriteIntentFailedAttemptAndLostReplay(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-attempts", "key-attempt", "feature/retry")

	failed, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}
	if err := s.EndWriteIntent(t.Context(), failed.ID, WriteStateFailed); err != nil {
		t.Fatalf("EndWriteIntent(failed): %v", err)
	}

	retry, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent after failure: %v", err)
	}
	if retry.ID == failed.ID || retry.State != WriteStatePending {
		t.Fatalf("retry = %+v, want a new pending attempt after failed %q", retry, failed.ID)
	}
	if err := s.EndWriteIntent(t.Context(), retry.ID, WriteStateLost); err != nil {
		t.Fatalf("EndWriteIntent(lost): %v", err)
	}

	changed := in
	changed.Repo = "Owner/Changed"
	changed.HeadRef = "feature/changed"
	lost, err := s.RecordWriteIntent(t.Context(), changed)
	if err != nil {
		t.Fatalf("RecordWriteIntent after loss: %v", err)
	}
	if lost.ID != retry.ID || lost.State != WriteStateLost || lost.Repo != retry.Repo || lost.HeadRef != retry.HeadRef {
		t.Fatalf("lost replay = %+v, want lost attempt %+v with its original target", lost, retry)
	}
}

func TestWriteIntentTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA, tenantB := seedTenant(t, s, "intent-tenant-a"), seedTenant(t, s, "intent-tenant-b")
	ctxA, ctxB := WithTenant(t.Context(), tenantA), WithTenant(t.Context(), tenantB)
	in := WriteIntent{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Repo",
		Kind: forge.WriteKindPullRequest, HeadRef: "feature/tenant",
		SessionID: "session-tenant", RequestKey: "key-tenant",
	}
	inA, inB := in, in
	inA.AgentAccountID, inA.OwnerUserID = seedWriteIntentAgent(t, ctxA, s, "intent-tenant-a")
	inB.AgentAccountID, inB.OwnerUserID = seedWriteIntentAgent(t, ctxB, s, "intent-tenant-b")

	rowA, err := s.RecordWriteIntent(ctxA, inA)
	if err != nil || rowA.TenantID != tenantA {
		t.Fatalf("RecordWriteIntent(A) = %+v, %v; want tenant %q", rowA, err, tenantA)
	}
	if got, err := s.PRIntentForHead(ctxB, in.Provider, in.Host, in.Repo, in.HeadRef); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PRIntentForHead(B) = %+v, %v; want ErrNotFound", got, err)
	}
	if err := s.EndWriteIntent(ctxB, rowA.ID, WriteStateFailed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EndWriteIntent(B on A's row) = %v; want ErrNotFound", err)
	}
	rowB, err := s.RecordWriteIntent(ctxB, inB)
	if err != nil || rowB.ID == rowA.ID || rowB.TenantID != tenantB || rowB.State != WriteStatePending {
		t.Fatalf("RecordWriteIntent(B, same key) = %+v, %v; want a new pending row in tenant %q", rowB, err, tenantB)
	}
	if got, err := s.PRIntentForHead(ctxA, in.Provider, in.Host, in.Repo, in.HeadRef); err != nil || got.ID != rowA.ID || got.State != WriteStatePending {
		t.Fatalf("PRIntentForHead(A) = %+v, %v; want A's untouched pending row", got, err)
	}
}

func seedWriteIntentAgent(t *testing.T, ctx context.Context, s *Store, handle string) (agent, owner AccountID) {
	t.Helper()
	u, err := s.CreateUser(ctx, NewUser{Handle: handle + "-owner", DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", handle, err)
	}
	a, err := s.CreateAgent(ctx, u.ID, NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%s): %v", handle, err)
	}
	return a.ID, u.ID
}

// A held key is reserved for its target and replayed to a racing claim, but it
// claims no PR head and is not swept until it is promoted to pending.
func TestHoldWriteIntentReservesKeyWithoutClaimingHead(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-hold", "key-hold", "feature/hold")
	held, err := s.HoldWriteIntent(t.Context(), in)
	if err != nil || held.State != WriteStateHeld {
		t.Fatalf("HoldWriteIntent = %+v, %v; want a held row", held, err)
	}
	other := in
	other.Repo = "Owner/Other"
	if got, err := s.HoldWriteIntent(t.Context(), other); err != nil || got.ID != held.ID || got.Repo != held.Repo {
		t.Fatalf("hold on another target = %+v, %v; want the first hold %+v", got, err, held)
	}
	if got, err := s.RecordWriteIntent(t.Context(), other); err != nil || got.ID != held.ID {
		t.Fatalf("record on another target = %+v, %v; want the first hold", got, err)
	}
	// Another agent may still claim the same head: a hold is not a PR-head claim.
	rival := writeIntentFixture(t, s, "intent-hold-rival", "key-rival", in.HeadRef)
	if _, err := s.RecordWriteIntent(t.Context(), rival); err != nil {
		t.Fatalf("rival head claim = %v, want allowed while the key is only held", err)
	}
	if pending, err := s.PendingWriteIntents(t.Context(), time.Now().Add(time.Hour), 10); err != nil || slices.ContainsFunc(pending, func(w WriteIntent) bool { return w.ID == held.ID }) {
		t.Fatalf("pending = %+v, %v; want the held row excluded", pending, err)
	}
	if err := s.PromoteWriteIntent(t.Context(), held.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("promote onto a claimed head = %v, want ErrConflict", err)
	}
}

func TestPromoteWriteIntentOnlyMovesHeldRows(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-promote", "key-promote", "feature/promote")
	held, err := s.HoldWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("HoldWriteIntent: %v", err)
	}
	if err := s.PromoteWriteIntent(t.Context(), held.ID); err != nil {
		t.Fatalf("PromoteWriteIntent: %v", err)
	}
	got, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil || got.ID != held.ID || got.State != WriteStatePending {
		t.Fatalf("replay after promote = %+v, %v; want the same row pending", got, err)
	}
	if err := s.PromoteWriteIntent(t.Context(), held.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second promote = %v, want ErrConflict", err)
	}
}

func TestRecordWriteIntentRejectsEmptyRequestKey(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-empty-key", "", "feature/empty-key")
	if _, err := s.RecordWriteIntent(t.Context(), in); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("RecordWriteIntent(empty key) = %v, want ErrInvalidArgument", err)
	}
}

func TestRecordWriteIntentRequiresParentForCommentsAndReviews(t *testing.T) {
	s := newTestStore(t)
	for _, kind := range []forge.WriteKind{forge.WriteKindComment, forge.WriteKindReview} {
		t.Run(kind.String(), func(t *testing.T) {
			in := writeIntentFixture(t, s, "intent-parent-"+kind.String(), "key-parent-"+kind.String(), "")
			in.Kind = kind
			in.HeadRef = ""
			if _, err := s.RecordWriteIntent(t.Context(), in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("RecordWriteIntent(%s without parent) = %v, want ErrInvalidArgument", kind, err)
			}
		})
	}
}

func TestRecordWriteIntentSeparatesHosts(t *testing.T) {
	s := newTestStore(t)
	first := writeIntentFixture(t, s, "intent-hosts", "key-host-one", "feature/same")
	second := first
	second.Host = "ghe.example"
	second.RequestKey = "key-host-two"

	one, err := s.RecordWriteIntent(t.Context(), first)
	if err != nil {
		t.Fatalf("RecordWriteIntent(github.com): %v", err)
	}
	two, err := s.RecordWriteIntent(t.Context(), second)
	if err != nil {
		t.Fatalf("RecordWriteIntent(ghe.example): %v", err)
	}
	if one.ID == two.ID {
		t.Fatalf("intents for separate hosts share id %q", one.ID)
	}
}

func TestCompleteWriteIntentIsAtomicAndPendingOnly(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-complete", "key-complete", "feature/complete")
	in.Kind = forge.WriteKindIssue
	in.HeadRef = ""
	intent, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}

	artifact := AuthoredArtifact{
		Provider: in.Provider, Host: in.Host, Repo: "owner/repo",
		Kind: ForgeArtifactKindIssue, Number: 42,
		AgentAccountID: in.AgentAccountID, OwnerUserID: in.OwnerUserID,
		SessionID: in.SessionID, CreatedAtUnixMS: 100,
	}
	if err := s.CompleteWriteIntent(t.Context(), intent.ID, &artifact, ""); err != nil {
		t.Fatalf("CompleteWriteIntent: %v", err)
	}

	got, err := s.AuthoredArtifactByCoordinateFold(t.Context(), artifact.Provider, artifact.Host, artifact.Repo, artifact.Kind, artifact.Number)
	if err != nil {
		t.Fatalf("AuthoredArtifactByCoordinateFold after complete: %v", err)
	}
	if got.AgentAccountID != in.AgentAccountID || got.ClientRequestID != "" {
		t.Fatalf("completed artifact = %+v, want agent %q and NULL request key", got, in.AgentAccountID)
	}

	secondArtifact := artifact
	secondArtifact.Number = 43
	if err := s.CompleteWriteIntent(t.Context(), intent.ID, &secondArtifact, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("CompleteWriteIntent(done) = %v, want ErrConflict", err)
	}
	if _, err := s.AuthoredArtifactByCoordinateFold(t.Context(), artifact.Provider, artifact.Host, artifact.Repo, artifact.Kind, secondArtifact.Number); !errors.Is(err, ErrNotFound) {
		t.Fatalf("artifact from refused completion lookup = %v, want ErrNotFound", err)
	}
	if err := s.EndWriteIntent(t.Context(), intent.ID, WriteStateFailed); !errors.Is(err, ErrConflict) {
		t.Fatalf("EndWriteIntent(done, failed) = %v, want ErrConflict", err)
	}
}

func TestCompleteCommentIntentStoresForgeKey(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-comment", "key-comment", "")
	in.Kind = forge.WriteKindComment
	in.Parent = 42
	in.HeadRef = ""
	intent, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}
	if err := s.CompleteWriteIntent(t.Context(), intent.ID, nil, "comment-123"); err != nil {
		t.Fatalf("CompleteWriteIntent(comment): %v", err)
	}

	var state, forgeKey string
	if err := s.pool.QueryRow(t.Context(), "SELECT state, forge_key FROM forge_write_intents WHERE id = $1", intent.ID).Scan(&state, &forgeKey); err != nil {
		t.Fatalf("read completed comment intent: %v", err)
	}
	if state != string(WriteStateDone) || forgeKey != "comment-123" {
		t.Fatalf("completed comment intent = state %q, forge key %q", state, forgeKey)
	}
}

func TestCompleteWriteIntentRollsBackArtifactWhenStateWriteFails(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-rollback", "key-rollback", "")
	in.Kind = forge.WriteKindIssue
	intent, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(), `
CREATE FUNCTION fail_write_intent_completion() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'done' THEN
        RAISE EXCEPTION 'forced intent completion failure';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER fail_write_intent_completion
BEFORE UPDATE OF state ON forge_write_intents
FOR EACH ROW EXECUTE FUNCTION fail_write_intent_completion();`); err != nil {
		t.Fatalf("install completion failure trigger: %v", err)
	}

	artifact := AuthoredArtifact{
		Provider: in.Provider, Host: in.Host, Repo: in.Repo,
		Kind: ForgeArtifactKindIssue, Number: 44,
		AgentAccountID: in.AgentAccountID, OwnerUserID: in.OwnerUserID,
		SessionID: in.SessionID, CreatedAtUnixMS: 100,
	}
	if err := s.CompleteWriteIntent(t.Context(), intent.ID, &artifact, ""); err == nil {
		t.Fatal("CompleteWriteIntent succeeded with failing state trigger")
	}
	if _, err := s.AuthoredArtifactByCoordinateFold(t.Context(), artifact.Provider, artifact.Host, artifact.Repo, artifact.Kind, artifact.Number); !errors.Is(err, ErrNotFound) {
		t.Fatalf("artifact after rolled-back completion = %v, want ErrNotFound", err)
	}
	row, err := s.q.WriteIntentByID(t.Context(), intent.ID)
	if err != nil {
		t.Fatalf("read rolled-back intent: %v", err)
	}
	if WriteState(row.State) != WriteStatePending {
		t.Fatalf("intent state after failed completion = %q, want pending", row.State)
	}
}

func TestPendingWriteIntentsListsOnlyOldPendingRows(t *testing.T) {
	s := newTestStore(t)
	oldIntent := writeIntentFixture(t, s, "intent-old", "key-old", "feature/old")
	old, err := s.RecordWriteIntent(t.Context(), oldIntent)
	if err != nil {
		t.Fatalf("RecordWriteIntent(old): %v", err)
	}
	freshIntent := writeIntentFixture(t, s, "intent-fresh", "key-fresh", "feature/fresh")
	fresh, err := s.RecordWriteIntent(t.Context(), freshIntent)
	if err != nil {
		t.Fatalf("RecordWriteIntent(fresh): %v", err)
	}
	failedIntent := writeIntentFixture(t, s, "intent-not-pending", "key-not-pending", "feature/failed")
	failed, err := s.RecordWriteIntent(t.Context(), failedIntent)
	if err != nil {
		t.Fatalf("RecordWriteIntent(failed): %v", err)
	}
	if err := s.EndWriteIntent(t.Context(), failed.ID, WriteStateFailed); err != nil {
		t.Fatalf("EndWriteIntent(failed): %v", err)
	}

	cutoff := time.Now().Add(-time.Hour)
	if _, err := s.pool.Exec(t.Context(), "UPDATE forge_write_intents SET created_at = $2 WHERE id = $1", old.ID, cutoff.Add(-time.Second)); err != nil {
		t.Fatalf("age old intent: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(), "UPDATE forge_write_intents SET created_at = $2 WHERE id = $1", failed.ID, cutoff.Add(-time.Second)); err != nil {
		t.Fatalf("age failed intent: %v", err)
	}

	got, err := s.PendingWriteIntents(t.Context(), cutoff, 10)
	if err != nil {
		t.Fatalf("PendingWriteIntents: %v", err)
	}
	if len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("PendingWriteIntents = %+v, want only old pending intent %q (fresh %q, failed %q excluded)", got, old.ID, fresh.ID, failed.ID)
	}
}

func TestAuthoredArtifactByCoordinateFoldReturnsOldestCaseVariant(t *testing.T) {
	s := newTestStore(t)
	firstAgent, firstOwner := seedAgent(t, s, "intent-fold-first")
	secondAgent, secondOwner := seedAgent(t, s, "intent-fold-second")
	first := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Repo",
		Kind: ForgeArtifactKindIssue, Number: 42,
		AgentAccountID: firstAgent, OwnerUserID: firstOwner, CreatedAtUnixMS: 100,
	}
	second := first
	second.Repo = "owner/repo"
	second.AgentAccountID = secondAgent
	second.OwnerUserID = secondOwner
	second.CreatedAtUnixMS = 200
	for _, artifact := range []AuthoredArtifact{second, first} {
		if err := s.RecordAuthoredArtifact(t.Context(), artifact); err != nil {
			t.Fatalf("RecordAuthoredArtifact(%q): %v", artifact.Repo, err)
		}
	}

	got, err := s.AuthoredArtifactByCoordinateFold(t.Context(), ForgeProviderGitHub, "github.com", "OWNER/REPO", ForgeArtifactKindIssue, 42)
	if err != nil {
		t.Fatalf("AuthoredArtifactByCoordinateFold: %v", err)
	}
	if got.AgentAccountID != firstAgent || got.Repo != first.Repo {
		t.Fatalf("folded lookup = %+v, want oldest case variant by agent %q repo %q", got, firstAgent, first.Repo)
	}
}

// Only held rows past the cutoff go; fresh held rows and every other state stay.
func TestDeleteExpiredHeldWriteIntentsDropsOnlyOldHeldRows(t *testing.T) {
	s := newTestStore(t)
	record := func(handle, state string) WriteIntent {
		t.Helper()
		in := writeIntentFixture(t, s, "expire-"+handle, "key-"+handle, "feature/"+handle)
		var (
			got WriteIntent
			err error
		)
		if state == "held" {
			got, err = s.HoldWriteIntent(t.Context(), in)
		} else {
			got, err = s.RecordWriteIntent(t.Context(), in)
		}
		if err != nil {
			t.Fatalf("record %s: %v", handle, err)
		}
		if state == "done" || state == "lost" {
			if _, err := s.pool.Exec(t.Context(), "UPDATE forge_write_intents SET state = $2, forge_key = '1' WHERE id = $1", got.ID, state); err != nil {
				t.Fatalf("set %s state: %v", handle, err)
			}
		}
		return got
	}
	oldHeld, freshHeld := record("old-held", "held"), record("fresh-held", "held")
	others := []WriteIntent{record("old-pending", "pending"), record("old-done", "done"), record("old-lost", "lost")}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, in := range append([]WriteIntent{oldHeld}, others...) {
		if _, err := s.pool.Exec(t.Context(), "UPDATE forge_write_intents SET created_at = $2 WHERE id = $1", in.ID, cutoff.Add(-time.Hour)); err != nil {
			t.Fatalf("age %s: %v", in.ID, err)
		}
	}

	n, err := s.DeleteExpiredHeldWriteIntents(WithSystemRole(t.Context()), cutoff, 10)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredHeldWriteIntents = %d, %v; want 1 deleted", n, err)
	}
	var ids []string
	rows, err := s.pool.Query(t.Context(), "SELECT id FROM forge_write_intents ORDER BY id")
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate intents: %v", err)
	}
	want := []string{freshHeld.ID, others[0].ID, others[1].ID, others[2].ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("rows left = %v, want %v (old held %s deleted)", ids, want, oldHeld.ID)
	}
}

// A promote committed while the expiry DELETE waits on the row must keep the now-pending intent.
func TestDeleteExpiredHeldWriteIntentsSparesRowPromotedMidDelete(t *testing.T) {
	s := newTestStore(t)
	held, err := s.HoldWriteIntent(t.Context(), writeIntentFixture(t, s, "expire-race", "key-expire-race", "feature/expire-race"))
	if err != nil {
		t.Fatalf("HoldWriteIntent: %v", err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	if _, err := s.pool.Exec(t.Context(), "UPDATE forge_write_intents SET created_at = $2 WHERE id = $1", held.ID, cutoff.Add(-time.Hour)); err != nil {
		t.Fatalf("age held intent: %v", err)
	}
	// The promote holds the row lock until commit, so the DELETE must wait and then recheck.
	promote, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin promote: %v", err)
	}
	defer func() { _ = promote.Rollback(t.Context()) }()
	if _, err := promote.Exec(t.Context(), "UPDATE forge_write_intents SET state = 'pending' WHERE id = $1 AND state = 'held'", held.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	done := make(chan error, 1)
	var deleted int64
	go func() {
		var err error
		deleted, err = s.DeleteExpiredHeldWriteIntents(WithSystemRole(t.Context()), cutoff, 10)
		done <- err
	}()
	waitForExpiryDeleteLockWait(t, s, done)
	if err := promote.Commit(t.Context()); err != nil {
		t.Fatalf("commit promote: %v", err)
	}
	if err := <-done; err != nil || deleted != 0 {
		t.Fatalf("DeleteExpiredHeldWriteIntents = %d, %v; want 0 deleted", deleted, err)
	}
	var state string
	if err := s.pool.QueryRow(t.Context(), "SELECT state FROM forge_write_intents WHERE id = $1", held.ID).Scan(&state); err != nil || state != string(WriteStatePending) {
		t.Fatalf("promoted intent state = %q, %v; want pending", state, err)
	}
}

// waitForExpiryDeleteLockWait returns once the expiry DELETE waits on a row lock.
func waitForExpiryDeleteLockWait(t *testing.T, s *Store, done <-chan error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%DELETE FROM forge_write_intents%' AND pid <> pg_backend_pid()").Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("expiry DELETE finished before the promote committed: %v", err)
		case <-deadline:
			t.Fatal("expiry DELETE never waited on the promoted row")
		case <-tick.C:
		}
	}
}

// Two holds on one key with different targets race from a shared start: one
// target is kept and both calls get that row back.
func TestHoldWriteIntentConcurrentTargetsKeepOne(t *testing.T) {
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "intent-hold-race")
	for i := range 40 {
		key := fmt.Sprintf("race-%d", i)
		target := func(repo string) WriteIntent {
			return WriteIntent{Provider: ForgeProviderGitHub, Host: "github.com", Repo: repo, Kind: forge.WriteKindPullRequest, HeadRef: "feature/race",
				AgentAccountID: agent, OwnerUserID: owner, SessionID: "s", RequestKey: key}
		}
		start := make(chan struct{})
		got := make([]WriteIntent, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, repo := range []string{"owner/a", "owner/b"} {
			wg.Go(func() {
				<-start
				got[j], errs[j] = s.HoldWriteIntent(t.Context(), target(repo))
			})
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d holds: %v, %v", i, errs[0], errs[1])
		}
		if got[0].ID != got[1].ID || got[0].Repo != got[1].Repo {
			t.Fatalf("round %d holds = %+v and %+v, want one row returned to both", i, got[0], got[1])
		}
		var count int
		if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM forge_write_intents WHERE client_request_id = $1", key).Scan(&count); err != nil || count != 1 {
			t.Fatalf("round %d rows = %d, %v; want 1", i, count, err)
		}
	}
}
