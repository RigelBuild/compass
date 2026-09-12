//go:build unix

package server

// Default-lane (no database) tests for the two forge state-transition arms
// (compass-forge-state-transition design.md §The server arm / §Actor
// attribution). They ride the same harness as forge_test.go — the exported
// forge.FakeProvider plus the faithful in-memory forgeStore — so the arm
// pipeline (resolveTarget → state-domain screen → refinement/provider screen →
// author-client dispatch → mapForgeError → actor memo → updated artifact) is
// observable end to end without Postgres. The memo's real-Postgres contract
// (upsert-latest-wins, consume-once, freshness) is the store package's own
// pgtest suite (forge_state_transitions_pgtest_test.go).

import (
	"errors"
	"strings"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// transitionIssueCall builds a TransitionIssueState request against the default
// coordinate, carrying whichever refinements the screen under test needs.
func transitionIssueCall(state, closeReason, workflowState string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_TransitionIssueState{TransitionIssueState: &compassv1internal.TransitionIssueStateRequest{
			Repo: testRepo, IssueNumber: 7, State: state, CloseReason: closeReason, WorkflowState: workflowState,
		}},
	}
}

// transitionPRCall builds a TransitionPullRequestState request against the
// default coordinate. The PR arm carries no refinement fields at all.
func transitionPRCall(state string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_TransitionPullRequestState{TransitionPullRequestState: &compassv1internal.TransitionPullRequestStateRequest{
			Repo: testRepo, PrNumber: 9, State: state,
		}},
	}
}

// newLinearForgeServiceForTest builds a service whose DEFAULT coordinate is
// LINEAR, so the refinement screen's provider-specific half is drivable in both
// directions (close_reason rejected on Linear, workflow_state rejected on
// GitHub) rather than only the GitHub one.
func newLinearForgeServiceForTest(t *testing.T, author *forge.FakeProvider) (*forgeService, *fakeForgeStore) {
	t.Helper()
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("lin-reviewer"))
	reg := newForgeProviderRegistry()
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, host: "linear.app"}, author, author, true)
	svc.providers = reg
	return svc, st
}

// --- tests: arm dispatch ------------------------------------------------------

// TestForgeTransitionIssueStateDispatchesAndReturnsUpdatedIssue pins the issue
// arm end to end: the AUTHOR client's TransitionIssueState is invoked with the
// requested coordinate and portable target, and the result comes back on the
// EXISTING Issue arm carrying the UPDATED artifact (post-transition truth), not
// a new result arm and not the pre-transition state.
func TestForgeTransitionIssueStateDispatchesAndReturnsUpdatedIssue(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)
	author.TransitionIssueResult = forge.Issue{Number: 7, Title: "t", State: "closed"}

	res := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "completed", ""))
	if fe := res.GetError(); fe != nil {
		t.Fatalf("transition returned an in-band error: %v", fe)
	}
	iss := res.GetIssue()
	if iss == nil {
		t.Fatalf("result arm = %T, want the Issue arm", res.GetResult())
	}
	if iss.GetNumber() != 7 || iss.GetRepo() != testRepo {
		t.Fatalf("issue coordinate = %s#%d, want %s#7", iss.GetRepo(), iss.GetNumber(), testRepo)
	}
	if iss.GetForge().GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB || iss.GetForge().GetHost() != testHost {
		t.Fatalf("issue forge ref = %v, want the resolved coordinate", iss.GetForge())
	}

	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "TransitionIssueState" {
		t.Fatalf("author calls = %+v, want one TransitionIssueState", calls)
	}
	if calls[0].Repo != testRepo || calls[0].Number != 7 {
		t.Fatalf("dispatched coordinate = %s#%d, want %s#7", calls[0].Repo, calls[0].Number, testRepo)
	}
	in, ok := calls[0].Payload.(forge.TransitionState)
	if !ok {
		t.Fatalf("payload = %T, want forge.TransitionState", calls[0].Payload)
	}
	if in.State != "closed" || in.CloseReason != "completed" || in.WorkflowState != "" {
		t.Fatalf("dispatched input = %+v, want {closed completed }", in)
	}
	if len(reviewer.Calls()) != 0 {
		t.Fatalf("reviewer calls = %d, want 0 (a transition is an ordinary author write)", len(reviewer.Calls()))
	}
}

// TestForgeTransitionPullRequestStateDispatchesAndReturnsUpdatedPR is the PR
// twin: the AUTHOR client's TransitionPullRequestState runs with a state-ONLY
// input (the PR arm has no refinement fields) and the updated PR comes back on
// the existing PullRequest arm.
func TestForgeTransitionPullRequestStateDispatchesAndReturnsUpdatedPR(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)
	author.TransitionPRResult = forge.PullRequest{Number: 9, Title: "t", State: "closed"}

	res := svc.ExecuteForgeCallAsAccountMust(t, transitionPRCall("closed"))
	if fe := res.GetError(); fe != nil {
		t.Fatalf("transition returned an in-band error: %v", fe)
	}
	pr := res.GetPullRequest()
	if pr == nil {
		t.Fatalf("result arm = %T, want the PullRequest arm", res.GetResult())
	}
	if pr.GetNumber() != 9 || pr.GetRepo() != testRepo {
		t.Fatalf("PR coordinate = %s#%d, want %s#9", pr.GetRepo(), pr.GetNumber(), testRepo)
	}

	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "TransitionPullRequestState" {
		t.Fatalf("author calls = %+v, want one TransitionPullRequestState", calls)
	}
	in, ok := calls[0].Payload.(forge.TransitionState)
	if !ok {
		t.Fatalf("payload = %T, want forge.TransitionState", calls[0].Payload)
	}
	if in != (forge.TransitionState{State: "closed"}) {
		t.Fatalf("dispatched input = %+v, want a state-only {closed}", in)
	}
}

// --- tests: validation screens ------------------------------------------------

// TestForgeTransitionStateOutsideDomainIsInvalidArgument pins the state-domain
// screen on BOTH arms: only "open" and "closed" are portable targets, so a
// provider-native name ("merged", a Linear column name) or an empty state is an
// in-band invalid_argument with ZERO provider calls and ZERO memo writes.
func TestForgeTransitionStateOutsideDomainIsInvalidArgument(t *testing.T) {
	for _, state := range []string{"", "merged", "Done", "OPEN"} {
		for _, arm := range []struct {
			name string
			call func(string) *compassv1internal.ForgeCallRequest
		}{
			{"issue", func(s string) *compassv1internal.ForgeCallRequest { return transitionIssueCall(s, "", "") }},
			{"pull_request", transitionPRCall},
		} {
			t.Run(arm.name+"/"+state, func(t *testing.T) {
				author := forge.NewFakeProvider("gh-author")
				svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))

				fe := svc.ExecuteForgeCallAsAccountMust(t, arm.call(state)).GetError()
				if fe == nil || fe.GetCode() != "invalid_argument" {
					t.Fatalf("state %q error = %v, want invalid_argument", state, fe)
				}
				if len(author.Calls()) != 0 || len(st.transitions) != 0 {
					t.Fatalf("rejected state touched provider/store: provider=%d memo=%d", len(author.Calls()), len(st.transitions))
				}
			})
		}
	}
}

// TestForgeTransitionEmptyRepoIsInvalidArgument pins that the transition arms
// inherit resolveTarget's posture: an empty repo is invalid_argument before any
// store or provider touch, exactly as on every other call.
func TestForgeTransitionEmptyRepoIsInvalidArgument(t *testing.T) {
	calls := map[string]*compassv1internal.ForgeCallRequest{
		"issue": {Call: &compassv1internal.ForgeCallRequest_TransitionIssueState{TransitionIssueState: &compassv1internal.TransitionIssueStateRequest{
			Repo: "", IssueNumber: 7, State: "closed",
		}}},
		"pull_request": {Call: &compassv1internal.ForgeCallRequest_TransitionPullRequestState{TransitionPullRequestState: &compassv1internal.TransitionPullRequestStateRequest{
			Repo: "", PrNumber: 9, State: "closed",
		}}},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))

			fe := svc.ExecuteForgeCallAsAccountMust(t, call).GetError()
			if fe == nil || fe.GetCode() != "invalid_argument" {
				t.Fatalf("empty repo error = %v, want invalid_argument", fe)
			}
			if len(author.Calls()) != 0 || len(st.transitions) != 0 {
				t.Fatalf("empty repo touched provider/store: provider=%d memo=%d", len(author.Calls()), len(st.transitions))
			}
		})
	}
}

// TestForgeTransitionWorkflowStateOnGitHubIsInvalidArgument pins half the
// refinement/provider screen: workflow_state is a LINEAR refinement, so
// addressing GitHub with one is rejected at the ARM — never silently dropped,
// and never passed down for the provider to ignore.
func TestForgeTransitionWorkflowStateOnGitHubIsInvalidArgument(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))

	fe := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "", "Done")).GetError()
	if fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("workflow_state on GitHub error = %v, want invalid_argument", fe)
	}
	if !strings.Contains(fe.GetMessage(), "workflow_state") || !strings.Contains(fe.GetMessage(), "gh-author") {
		t.Fatalf("message %q does not name the field and the provider", fe.GetMessage())
	}
	if len(author.Calls()) != 0 || len(st.transitions) != 0 {
		t.Fatalf("rejected refinement touched provider/store: provider=%d memo=%d", len(author.Calls()), len(st.transitions))
	}
}

// TestForgeTransitionCloseReasonOnLinearIsInvalidArgument pins the other half:
// close_reason is a GitHub-ISSUE refinement, so addressing Linear with one is
// rejected at the arm. The mirrored case is what proves the screen is
// provider-directional rather than a single hard-coded rejection.
func TestForgeTransitionCloseReasonOnLinearIsInvalidArgument(t *testing.T) {
	author := forge.NewFakeProvider("linear")
	svc, st := newLinearForgeServiceForTest(t, author)

	fe := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "not_planned", "")).GetError()
	if fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("close_reason on Linear error = %v, want invalid_argument", fe)
	}
	if !strings.Contains(fe.GetMessage(), "close_reason") || !strings.Contains(fe.GetMessage(), "linear") {
		t.Fatalf("message %q does not name the field and the provider", fe.GetMessage())
	}
	if len(author.Calls()) != 0 || len(st.transitions) != 0 {
		t.Fatalf("rejected refinement touched provider/store: provider=%d memo=%d", len(author.Calls()), len(st.transitions))
	}
}

// TestForgeTransitionLinearWorkflowStateReachesProvider is the screen's positive
// control: a refinement the ADDRESSED provider CAN express passes the screen and
// arrives in the dispatched input. Without this, a screen that rejected every
// refinement would pass the two rejection tests above.
func TestForgeTransitionLinearWorkflowStateReachesProvider(t *testing.T) {
	author := forge.NewFakeProvider("linear")
	svc, _ := newLinearForgeServiceForTest(t, author)
	author.TransitionIssueResult = forge.Issue{Number: 7, State: "closed"}

	res := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "", "Done"))
	if fe := res.GetError(); fe != nil {
		t.Fatalf("Linear workflow_state was rejected: %v", fe)
	}
	calls := author.Calls()
	if len(calls) != 1 {
		t.Fatalf("author calls = %d, want 1", len(calls))
	}
	in, ok := calls[0].Payload.(forge.TransitionState)
	if !ok || in.WorkflowState != "Done" {
		t.Fatalf("dispatched input = %+v, want WorkflowState=Done", calls[0].Payload)
	}
}

// TestForgeTransitionPRArmCarriesNoRefinements pins the PR-refinement screen
// structurally: the PR wire message exposes no refinement setter at all, so the
// only thing that can reach the provider is the portable state. Asserting the
// dispatched input is state-only is what catches a future arm that starts
// forwarding an issue refinement onto the PR path.
func TestForgeTransitionPRArmCarriesNoRefinements(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	author.TransitionPRResult = forge.PullRequest{Number: 9, State: "open"}

	if fe := svc.ExecuteForgeCallAsAccountMust(t, transitionPRCall("open")).GetError(); fe != nil {
		t.Fatalf("PR transition returned an in-band error: %v", fe)
	}
	in, ok := author.Calls()[0].Payload.(forge.TransitionState)
	if !ok {
		t.Fatalf("payload = %T, want forge.TransitionState", author.Calls()[0].Payload)
	}
	if in.CloseReason != "" || in.WorkflowState != "" {
		t.Fatalf("PR arm forwarded a refinement: %+v", in)
	}
}

// TestForgeTransitionUnsupportedIsUnimplemented pins the flattening path both
// arms share: a provider with no PR model answers ErrUnsupported, which
// mapForgeError renders as an in-band unimplemented naming provider+op — and no
// memo lands, because the provider never succeeded.
func TestForgeTransitionUnsupportedIsUnimplemented(t *testing.T) {
	author := forge.NewFakeProvider("linear")
	svc, st := newLinearForgeServiceForTest(t, author)
	author.SetError("TransitionPullRequestState", forge.ErrUnsupported)

	fe := svc.ExecuteForgeCallAsAccountMust(t, transitionPRCall("closed")).GetError()
	if fe == nil || fe.GetCode() != "unimplemented" {
		t.Fatalf("ErrUnsupported error = %v, want unimplemented", fe)
	}
	if !strings.Contains(fe.GetMessage(), "linear") || !strings.Contains(fe.GetMessage(), "transition_pull_request_state") {
		t.Fatalf("unimplemented message %q does not name provider+op", fe.GetMessage())
	}
	if len(st.transitions) != 0 {
		t.Fatalf("memo writes = %d, want 0 (the provider failed)", len(st.transitions))
	}
}

// --- tests: the actor memo ----------------------------------------------------

// TestForgeTransitionWritesActorMemoAfterProviderSuccess pins the §Actor
// attribution write half: on success the memo lands at the transitioned
// coordinate carrying the CALLING agent, the APPLIED portable state, and the
// chokepoint clock — and it lands STRICTLY AFTER the provider call, which the
// zero-memo-on-failure test below is the other half of. It also pins that a
// transition writes NO DL-055 ownership row: that row is a write-once authorship
// fact a transition must not touch.
func TestForgeTransitionWritesActorMemoAfterProviderSuccess(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	author.TransitionIssueResult = forge.Issue{Number: 7, State: "closed"}

	if fe := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "completed", "")).GetError(); fe != nil {
		t.Fatalf("transition returned an in-band error: %v", fe)
	}
	if len(st.transitions) != 1 {
		t.Fatalf("memo writes = %d, want 1", len(st.transitions))
	}
	got := st.transitions[0]
	want := recordedTransition{
		provider: store.ForgeProvider(compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB),
		host:     testHost,
		repo:     testRepo,
		kind:     store.ForgeArtifactKindIssue,
		number:   7,
		state:    "closed",
		agent:    testAgentID,
		at:       nowStub(),
	}
	if got != want {
		t.Fatalf("memo = %+v, want %+v", got, want)
	}
	if len(st.recorded) != 0 {
		t.Fatalf("DL-055 rows = %d, want 0 (a transition mints no coordinate and must not touch the authorship row)", len(st.recorded))
	}
}

// TestForgeTransitionPRMemoCarriesPortableStateAndPRKind pins the PR arm's memo:
// kind is pull_request, and the recorded state is the REQUESTED portable target,
// never the returned artifact's raw state — a merged PR reads back "merged",
// which is outside the portable domain the notify lane matches on.
func TestForgeTransitionPRMemoCarriesPortableStateAndPRKind(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	author.TransitionPRResult = forge.PullRequest{Number: 9, State: "merged"}

	if fe := svc.ExecuteForgeCallAsAccountMust(t, transitionPRCall("closed")).GetError(); fe != nil {
		t.Fatalf("transition returned an in-band error: %v", fe)
	}
	if len(st.transitions) != 1 {
		t.Fatalf("memo writes = %d, want 1", len(st.transitions))
	}
	got := st.transitions[0]
	if got.kind != store.ForgeArtifactKindPullRequest || got.number != 9 {
		t.Fatalf("memo coordinate = kind %d number %d, want pull_request #9", got.kind, got.number)
	}
	if got.state != "closed" {
		t.Fatalf("memo state = %q, want the requested portable %q (never the raw %q)", got.state, "closed", "merged")
	}
}

// TestForgeTransitionProviderFailureLeavesNoMemo pins the ordering the record
// arm holds too: the memo is written strictly AFTER provider success, so a
// rejected transition leaves NOTHING behind for the notify lane to attribute.
// Inverting the order would attribute a STATE event to an agent whose write the
// forge refused.
func TestForgeTransitionProviderFailureLeavesNoMemo(t *testing.T) {
	for _, arm := range []struct {
		name   string
		method string
		call   *compassv1internal.ForgeCallRequest
	}{
		{"issue", "TransitionIssueState", transitionIssueCall("closed", "", "")},
		{"pull_request", "TransitionPullRequestState", transitionPRCall("closed")},
	} {
		t.Run(arm.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			author.SetError(arm.method, &forge.StatusError{Status: 422, Message: "cannot reopen a merged pull request"})

			fe := svc.ExecuteForgeCallAsAccountMust(t, arm.call).GetError()
			if fe == nil || fe.GetCode() != "invalid_argument" {
				t.Fatalf("422 error = %v, want invalid_argument", fe)
			}
			if len(author.Calls()) != 1 {
				t.Fatalf("provider calls = %d, want 1 (the transition was attempted)", len(author.Calls()))
			}
			if len(st.transitions) != 0 {
				t.Fatalf("memo writes = %d, want 0 (the provider rejected the transition)", len(st.transitions))
			}
		})
	}
}

// TestForgeTransitionMemoFailureAfterProviderSuccessIsInternal pins the
// memo-after-success fault path: the forge transition SUCCEEDED but the memo
// write failed, so the call returns an in-band internal error rather than a
// success the notify lane could never attribute. The forge-side state is already
// changed — the caller learning the attribution half failed is the honest answer.
//
// The MESSAGE is part of that honesty and is asserted here: it must name the
// coordinate and the applied state, so a caller cannot mistake a landed
// transition for a no-op and retry it, and a human reading the trace can see
// which half failed.
func TestForgeTransitionMemoFailureAfterProviderSuccessIsInternal(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	author.TransitionIssueResult = forge.Issue{Number: 7, State: "closed"}
	st.transErr = errors.New("memo: db unavailable")

	fe := svc.ExecuteForgeCallAsAccountMust(t, transitionIssueCall("closed", "", "")).GetError()
	if fe == nil || fe.GetCode() != "internal" {
		t.Fatalf("memo-after-success error = %v, want internal", fe)
	}
	for _, want := range []string{testRepo, "#7", "closed", "memo: db unavailable"} {
		if !strings.Contains(fe.GetMessage(), want) {
			t.Errorf("memo-after-success message %q does not name %q — the caller cannot tell the transition landed", fe.GetMessage(), want)
		}
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("provider calls = %d, want 1 (the transition ran before the memo failed)", len(author.Calls()))
	}
	if len(st.transitions) != 0 {
		t.Fatalf("memo writes = %d, want 0 (the write failed)", len(st.transitions))
	}
}
