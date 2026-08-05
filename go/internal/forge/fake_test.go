package forge

// Contracts for the exported FakeProvider: an ordered, inspectable call log
// (so a test can assert exactly what a Service invoked — and that ZERO calls
// happened when it short-circuits), scripted results and scripted errors
// (including a *StatusError the 403/404 flattening reads via errors.As), and the
// compile-time proof that *FakeProvider satisfies Provider. context.Background()
// here is the test root — the sanctioned test exemption to F-ttsr.

import (
	"context"
	"errors"
	"sync"
	"testing"
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
	if _, err := f.CommentOnIssue(ctx, "org/repo", 7, "hi"); err != nil {
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
	if calls[1].Method != "CommentOnIssue" || calls[1].Number != 7 || calls[1].Body != "hi" {
		t.Errorf("call[1] = %+v, want CommentOnIssue number=7 body=hi", calls[1])
	}
	if calls[2].Method != "Checks" || calls[2].Number != 42 {
		t.Errorf("call[2] = %+v, want Checks number=42", calls[2])
	}
}

func TestFakeZeroCallsWhenUnused(t *testing.T) {
	f := NewFakeProvider("test-forge")
	if n := len(f.Calls()); n != 0 {
		t.Errorf("a fresh fake recorded %d calls, want 0", n)
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
	var se *StatusError
	if !errors.As(err, &se) {
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
			_, _ = f.CommentOnIssue(ctx, "org/repo", i, "hi")
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
