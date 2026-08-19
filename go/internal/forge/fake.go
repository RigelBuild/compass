package forge

// FakeProvider is the in-package fake every downstream task's tests drive
// (#995 design.md:1736-1739). It implements Provider without a network: it
// records every call in an ordered, inspectable log (so a test can assert
// exactly what a Service invoked — and that ZERO calls happened when the Service
// short-circuits), returns scripted results per method (defaulting to zero-value
// success), and can be told to return a scripted error for a method — including
// a *StatusError with a set status, so the Service's 403/404 flattening is
// testable without a wire.

import (
	"context"
	"sync"
)

// Call is one recorded Provider invocation on a FakeProvider. Method is the
// interface method name; the remaining fields carry whichever arguments that
// method took (unset fields stay zero). Payload holds the CreateIssue / CreatePR
// input for the create methods; Filter holds the ListIssues filter.
type Call struct {
	// Method is the Provider method name (e.g. "CreateIssue").
	Method string
	// Repo is the repo argument every method takes.
	Repo string
	// Number is the issue/PR number for the methods that take one.
	Number uint64
	// Body is the comment body for the CommentOn* methods.
	Body string
	// Payload is the CreateIssue/CreatePR input for the create methods (nil
	// otherwise).
	Payload any
	// Filter is the ListIssues filter (zero for other methods).
	Filter IssueFilter
}

// FakeProvider is a network-free Provider for tests. Scripted results are set on
// the exported *Result fields; scripted errors are set per method via SetError.
// It is safe for concurrent use. Construct it with NewFakeProvider.
type FakeProvider struct {
	// CreateIssueResult is returned by CreateIssue when no error is scripted.
	CreateIssueResult Issue
	// CommentResult is returned by CommentOnIssue and CommentOnPullRequest when
	// no error is scripted.
	CommentResult Comment
	// GetIssueResult is returned by GetIssue when no error is scripted.
	GetIssueResult Issue
	// ListIssuesResult is returned by ListIssues when no error is scripted.
	ListIssuesResult []Issue
	// CreatePRResult is returned by CreatePullRequest when no error is scripted.
	CreatePRResult PullRequest
	// GetPRResult is returned by GetPullRequest when no error is scripted.
	GetPRResult PullRequest
	// ChecksResult is returned by Checks when no error is scripted.
	ChecksResult Checks
	// SubmitReviewResult is returned by SubmitReview when no error is scripted.
	SubmitReviewResult SubmittedReview
	// BodyLimitResult is returned by BodyLimit; 0 (the default) means unlimited.
	BodyLimitResult int

	mu     sync.Mutex
	name   string
	calls  []Call
	errors map[string]error
}

// Compile-time proof that the fake satisfies the interface it fakes.
var _ Provider = (*FakeProvider)(nil)

// NewFakeProvider returns a FakeProvider whose Name reports name. Every method
// defaults to zero-value success; script results on the *Result fields and
// errors via SetError.
func NewFakeProvider(name string) *FakeProvider {
	return &FakeProvider{name: name, errors: make(map[string]error)}
}

// SetError scripts err as the return of the named Provider method. Pass a
// *StatusError to exercise the Service's status flattening, or ErrUnsupported to
// exercise the unsupported-operation path. Passing a nil err clears the script.
func (f *FakeProvider) SetError(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errors, method)
		return
	}
	f.errors[method] = err
}

// Calls returns a copy of the recorded call log, in invocation order.
func (f *FakeProvider) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// Name reports the configured provider name.
func (f *FakeProvider) Name() string { return f.name }

// BodyLimit reports the configured body-size limit in bytes (0 = unlimited). It
// is not recorded in the call log — it is a pure accessor, not a network call.
func (f *FakeProvider) BodyLimit() int { return f.BodyLimitResult }

// CreateIssue records the call and returns the scripted result or error.
func (f *FakeProvider) CreateIssue(_ context.Context, repo string, in CreateIssue) (Issue, error) {
	if err := f.record(Call{Method: "CreateIssue", Repo: repo, Payload: in}); err != nil {
		return Issue{}, err
	}
	return f.CreateIssueResult, nil
}

// CommentOnIssue records the call and returns the scripted result or error.
func (f *FakeProvider) CommentOnIssue(_ context.Context, repo string, number uint64, body string) (Comment, error) {
	if err := f.record(Call{Method: "CommentOnIssue", Repo: repo, Number: number, Body: body}); err != nil {
		return Comment{}, err
	}
	return f.CommentResult, nil
}

// GetIssue records the call and returns the scripted result or error.
func (f *FakeProvider) GetIssue(_ context.Context, repo string, number uint64) (Issue, error) {
	if err := f.record(Call{Method: "GetIssue", Repo: repo, Number: number}); err != nil {
		return Issue{}, err
	}
	return f.GetIssueResult, nil
}

// ListIssues records the call and returns the scripted result or error.
func (f *FakeProvider) ListIssues(_ context.Context, repo string, filter IssueFilter) ([]Issue, error) {
	if err := f.record(Call{Method: "ListIssues", Repo: repo, Filter: filter}); err != nil {
		return nil, err
	}
	return f.ListIssuesResult, nil
}

// CreatePullRequest records the call and returns the scripted result or error.
func (f *FakeProvider) CreatePullRequest(_ context.Context, repo string, in CreatePR) (PullRequest, error) {
	if err := f.record(Call{Method: "CreatePullRequest", Repo: repo, Payload: in}); err != nil {
		return PullRequest{}, err
	}
	return f.CreatePRResult, nil
}

// CommentOnPullRequest records the call and returns the scripted result or error.
func (f *FakeProvider) CommentOnPullRequest(_ context.Context, repo string, number uint64, body string) (Comment, error) {
	if err := f.record(Call{Method: "CommentOnPullRequest", Repo: repo, Number: number, Body: body}); err != nil {
		return Comment{}, err
	}
	return f.CommentResult, nil
}

// SubmitReview records the call and returns the scripted result or error.
func (f *FakeProvider) SubmitReview(_ context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error) {
	if err := f.record(Call{Method: "SubmitReview", Repo: repo, Number: number, Payload: in}); err != nil {
		return SubmittedReview{}, err
	}
	return f.SubmitReviewResult, nil
}

// GetPullRequest records the call and returns the scripted result or error.
func (f *FakeProvider) GetPullRequest(_ context.Context, repo string, number uint64) (PullRequest, error) {
	if err := f.record(Call{Method: "GetPullRequest", Repo: repo, Number: number}); err != nil {
		return PullRequest{}, err
	}
	return f.GetPRResult, nil
}

// Checks records the call and returns the scripted result or error.
func (f *FakeProvider) Checks(_ context.Context, repo string, number uint64) (Checks, error) {
	if err := f.record(Call{Method: "Checks", Repo: repo, Number: number}); err != nil {
		return Checks{}, err
	}
	return f.ChecksResult, nil
}

// record appends a call and returns any error scripted for its method. The
// caller holds no lock; record takes it.
func (f *FakeProvider) record(c Call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.errors[c.Method]
}
