//go:build pgtest

package store

import (
	"errors"
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

	lost, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent after loss: %v", err)
	}
	if lost.ID != retry.ID || lost.State != WriteStateLost {
		t.Fatalf("lost replay = %+v, want lost attempt %+v", lost, retry)
	}
}

func TestLiveWriteIntentByKeyReadsWithoutClaiming(t *testing.T) {
	s := newTestStore(t)
	in := writeIntentFixture(t, s, "intent-by-key", "key-read", "feature/read")
	if _, err := s.LiveWriteIntentByKey(t.Context(), in.AgentAccountID, in.RequestKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read before record = %v, want ErrNotFound", err)
	}
	if _, err := s.LiveWriteIntentByKey(t.Context(), in.AgentAccountID, in.RequestKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second read = %v, want ErrNotFound (a read claims nothing)", err)
	}
	rec, err := s.RecordWriteIntent(t.Context(), in)
	if err != nil {
		t.Fatalf("RecordWriteIntent: %v", err)
	}
	got, err := s.LiveWriteIntentByKey(t.Context(), in.AgentAccountID, in.RequestKey)
	if err != nil || got != rec {
		t.Fatalf("read = %+v, %v; want %+v", got, err, rec)
	}
	if err := s.EndWriteIntent(t.Context(), rec.ID, WriteStateFailed); err != nil {
		t.Fatalf("EndWriteIntent(failed): %v", err)
	}
	if _, err := s.LiveWriteIntentByKey(t.Context(), in.AgentAccountID, in.RequestKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after failure = %v, want ErrNotFound", err)
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
