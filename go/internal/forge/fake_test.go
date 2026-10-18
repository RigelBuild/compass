package forge

// Contracts for the exported FakeProvider: an ordered, inspectable call log
// (so a test can assert exactly what a Service invoked, and ZERO on short-
// circuit), scripted results and errors (a *StatusError the 403/404 flattening
// reads via errors.AsType), and the compile-time proof it satisfies Provider.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Compile-time assertion: the fake IS a Provider.
var _ Provider = (*FakeProvider)(nil)

func TestFakeRecordsCallsInOrder(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")

	if f.Name() != "test-forge" {
		t.Errorf("Name() = %q, want %q", f.Name(), "test-forge")
	}

	if _, err := f.CreateIssue(ctx, "org/repo", CreateIssue{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := f.CommentOnIssue(ctx, "org/repo", 7, "hi", "client-1"); err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	if _, err := f.Checks(ctx, "org/repo", 42); err != nil {
		t.Fatalf("Checks: %v", err)
	}

	calls := f.Calls()
	if len(calls) != 3 {
		t.Fatalf("recorded %d calls, want 3", len(calls))
	}
	if calls[0].Method != "CreateIssue" || calls[0].Repo != "org/repo" {
		t.Errorf("call[0] = %+v, want CreateIssue on org/repo", calls[0])
	}
	if calls[1].Method != "CommentOnIssue" || calls[1].Number != 7 || calls[1].Body != "hi" || calls[1].ClientID != "client-1" {
		t.Errorf("call[1] = %+v, want CommentOnIssue number=7 body=hi clientID=client-1", calls[1])
	}
	if calls[2].Method != "Checks" || calls[2].Number != 42 {
		t.Errorf("call[2] = %+v, want Checks number=42", calls[2])
	}
}

func TestFakeCommentOperationsRecordAndScript(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	editResult := Comment{Key: "edited"}
	listResult := []Comment{{Key: "one"}, {Key: "two"}}
	f.EditCommentResult = editResult
	f.ListCommentsResult = listResult
	since := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

	if got, err := f.EditComment(ctx, "org/repo", "555", "edited body"); err != nil || got != editResult {
		t.Fatalf("EditComment = %+v, %v; want %+v, nil", got, err, editResult)
	}
	if err := f.DeleteComment(ctx, "org/repo", "duplicate"); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if got, err := f.ListCommentsSince(ctx, "org/repo", 7, since); err != nil || !reflect.DeepEqual(got, listResult) {
		t.Fatalf("ListCommentsSince = %+v, %v; want %+v, nil", got, err, listResult)
	}
	if _, err := f.CommentOnIssue(ctx, "org/repo", 7, "body", "client-1"); err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	if _, err := f.CommentOnPullRequest(ctx, "org/repo", 8, "body", "client-2"); err != nil {
		t.Fatalf("CommentOnPullRequest: %v", err)
	}

	calls := f.Calls()
	if len(calls) != 5 {
		t.Fatalf("recorded calls = %d, want 5", len(calls))
	}
	if calls[0].Method != "EditComment" || calls[0].CommentKey != "555" || calls[0].Body != "edited body" {
		t.Errorf("edit call = %+v", calls[0])
	}
	if calls[1].Method != "DeleteComment" || calls[1].CommentKey != "duplicate" {
		t.Errorf("delete call = %+v", calls[1])
	}
	if calls[2].Method != "ListCommentsSince" || calls[2].Number != 7 || !calls[2].Since.Equal(since) {
		t.Errorf("list call = %+v", calls[2])
	}
	if calls[3].Method != "CommentOnIssue" || calls[3].ClientID != "client-1" {
		t.Errorf("issue comment call = %+v", calls[3])
	}
	if calls[4].Method != "CommentOnPullRequest" || calls[4].ClientID != "client-2" {
		t.Errorf("PR comment call = %+v", calls[4])
	}
}

func TestFakeCommentOperationsScriptErrors(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	wantErr := errors.New("scripted comment failure")
	for _, method := range []string{"EditComment", "DeleteComment", "ListCommentsSince"} {
		f.SetError(method, wantErr)
	}

	if _, err := f.EditComment(ctx, "org/repo", "555", "edited"); !errors.Is(err, wantErr) {
		t.Errorf("EditComment err = %v, want scripted error", err)
	}
	if err := f.DeleteComment(ctx, "org/repo", "555"); !errors.Is(err, wantErr) {
		t.Errorf("DeleteComment err = %v, want scripted error", err)
	}
	if _, err := f.ListCommentsSince(ctx, "org/repo", 7, time.Time{}); !errors.Is(err, wantErr) {
		t.Errorf("ListCommentsSince err = %v, want scripted error", err)
	}
}

func TestFakeZeroCallsWhenUnused(t *testing.T) {
	f := NewFakeProvider("test-forge")
	if n := len(f.Calls()); n != 0 {
		t.Errorf("a fresh fake recorded %d calls, want 0", n)
	}
}

func TestFakeBodyLimit(t *testing.T) {
	f := NewFakeProvider("test-forge")
	if got := f.BodyLimit(); got != 0 {
		t.Errorf("default BodyLimit() = %d, want 0 (unlimited)", got)
	}
	f.BodyLimitResult = 65536
	if got := f.BodyLimit(); got != 65536 {
		t.Errorf("configured BodyLimit() = %d, want 65536", got)
	}
}

func TestFakeScriptedResult(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	f.GetIssueResult = Issue{Number: 99, Title: "scripted", State: "open"}

	got, err := f.GetIssue(ctx, "org/repo", 99)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Number != 99 || got.Title != "scripted" {
		t.Errorf("GetIssue = %+v, want the scripted issue", got)
	}
}

func TestFakeDefaultsToZeroValueSuccess(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	got, err := f.GetPullRequest(ctx, "org/repo", 1)
	if err != nil {
		t.Fatalf("unscripted GetPullRequest returned error: %v", err)
	}
	// PullRequest nests slices, so compare the discriminating scalar: a fresh
	// fake returns the zero-value PR (Number 0), not a scripted one.
	if got.Number != 0 || got.Title != "" || len(got.Reviews) != 0 {
		t.Errorf("unscripted GetPullRequest = %+v, want zero value", got)
	}
}

func TestFakeScriptedError(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	sentinel := errors.New("scripted boom")
	f.SetError("ListIssues", sentinel)

	_, err := f.ListIssues(ctx, "org/repo", IssueFilter{})
	if !errors.Is(err, sentinel) {
		t.Errorf("ListIssues err = %v, want the scripted sentinel", err)
	}

	// A different method is unaffected.
	if _, err := f.GetIssue(ctx, "org/repo", 1); err != nil {
		t.Errorf("GetIssue unexpectedly errored: %v", err)
	}
}

func TestFakeScriptedStatusError(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	f.SetError("GetIssue", &StatusError{Status: 404, Message: "not found"})

	_, err := f.GetIssue(ctx, "org/repo", 123)
	se, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("scripted error not a *StatusError: %v", err)
	}
	if se.Status != 404 {
		t.Errorf("recovered status = %d, want 404", se.Status)
	}
}

func TestFakeScriptedUnsupported(t *testing.T) {
	ctx := context.Background()
	f := NewFakeProvider("test-forge")
	f.SetError("CreatePullRequest", ErrUnsupported)

	_, err := f.CreatePullRequest(ctx, "org/repo", CreatePR{Title: "t"})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported recoverable via errors.Is", err)
	}
}

func TestFakeNewProviderMethods(t *testing.T) {
	f := NewFakeProvider("test-forge")
	f.UpdatePRResult = PullRequest{Number: 9, Title: "updated"}
	f.FindPRResult = PullRequest{Number: 10, HeadRef: "feature"}
	f.FindPRFound = true
	f.FindWriteResult = WrittenRef{Number: 11, Key: "12"}
	f.FindWriteFound = true

	updated, err := f.UpdatePullRequest(t.Context(), "org/repo", 9, UpdatePR{Title: "updated"})
	if err != nil || updated.Number != 9 {
		t.Fatalf("UpdatePullRequest = (%+v, %v)", updated, err)
	}
	foundPR, found, err := f.FindPullRequestByHead(t.Context(), "org/repo", "feature")
	if err != nil || !found || foundPR.Number != 10 {
		t.Fatalf("FindPullRequestByHead = (%+v, %v, %v)", foundPR, found, err)
	}
	foundRef, found, err := f.FindByWriteMarker(t.Context(), "org/repo", WriteKindReview, 10, "intent", time.Time{})
	if err != nil || !found || foundRef.Key != "12" {
		t.Fatalf("FindByWriteMarker = (%+v, %v, %v)", foundRef, found, err)
	}
}

// TestFakeConcurrentUse exercises the "safe for concurrent use" contract: many
// goroutines drive record-mutating methods while others read Calls() and mutate
// SetError(). It relies on `go test -race` to catch an unsynchronized access;
// the deterministic assertion is that the final call count equals the exact
// number of record-driving invocations (each worker drives 4).
func TestFakeConcurrentUse(t *testing.T) {
	ctx := context.Background() // test root — sanctioned exemption to F-ttsr.
	f := NewFakeProvider("test-forge")

	const workers = 50
	const drivesPerWorker = 4

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i uint64) {
			defer wg.Done()
			// Four record-driving calls, interleaved with reads and error
			// scripting to exercise every lock path concurrently.
			_, _ = f.CreateIssue(ctx, "org/repo", CreateIssue{Title: "t"})
			_ = f.Calls()
			_, _ = f.CommentOnIssue(ctx, "org/repo", i, "hi", "")
			f.SetError("Checks", nil) // drive concurrent errors-map mutation on a key that IS read
			_, _ = f.GetPullRequest(ctx, "org/repo", i)
			_ = f.Calls()
			_, _ = f.Checks(ctx, "org/repo", i)
		}(uint64(i))
	}
	wg.Wait()

	if got := len(f.Calls()); got != workers*drivesPerWorker {
		t.Errorf("recorded %d calls, want %d", got, workers*drivesPerWorker)
	}
}
