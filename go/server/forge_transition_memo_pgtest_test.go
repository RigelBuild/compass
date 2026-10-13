//go:build pgtest && unix

package server

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestForgeTransitionMemoResolvesFromWebhookCoordinate drives a transition arm
// to write the actor memo, parses the forge's echo webhook with the production
// parser, and consumes the memo at the PARSED coordinate. The memo is keyed on
// the caller's repo string while the read keys on the webhook's, so this is the
// only place the two are checked against each other. The perturbed cases prove
// the check can fail: a casing difference on one side must miss.
func TestForgeTransitionMemoResolvesFromWebhookCoordinate(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	u, err := st.CreateUser(ctx, store.NewUser{Handle: "memocoordowner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, u.ID, store.NewAgent{Handle: "memocoordagent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	author := forge.NewFakeProvider("gh-author")
	linear := forge.NewFakeProvider("linear")
	reg := newForgeProviderRegistry()
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: "github.com"}, author, forge.NewFakeProvider("gh-reviewer"), true); err != nil {
		t.Fatalf("register github: %v", err)
	}
	if err := registerLinearForgeCoordinate(reg, linear); err != nil {
		t.Fatalf("register linear: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newForgeService(st, board.NewIssueProjection(bus, st), reg)
	secret := []byte("memo-coord-secret")

	githubIssue := func(t *testing.T, callerRepo, webhookRepo string, n uint64) (*compassv1internal.ForgeCallRequest, forge.ForgeEvent) {
		t.Helper()
		author.TransitionIssueResult = forge.Issue{Number: n, State: "closed"}
		call := &compassv1internal.ForgeCallRequest{Call: &compassv1internal.ForgeCallRequest_TransitionIssueState{
			TransitionIssueState: &compassv1internal.TransitionIssueStateRequest{Repo: callerRepo, IssueNumber: n, State: "closed", CloseReason: "completed"},
		}}
		sw := newFakeGitHubForge(secret, webhookRepo).closeIssue(t, n, "https://gh/issues")
		return call, parseGitHubEcho(t, sw)
	}
	githubPR := func(t *testing.T, callerRepo, webhookRepo string, n uint64) (*compassv1internal.ForgeCallRequest, forge.ForgeEvent) {
		t.Helper()
		author.TransitionPRResult = forge.PullRequest{Number: n, State: "closed"}
		call := &compassv1internal.ForgeCallRequest{Call: &compassv1internal.ForgeCallRequest_TransitionPullRequestState{
			TransitionPullRequestState: &compassv1internal.TransitionPullRequestStateRequest{Repo: callerRepo, PrNumber: n, State: "closed"},
		}}
		sw := newFakeGitHubForge(secret, webhookRepo).closePR(t, n, "https://gh/pull")
		return call, parseGitHubEcho(t, sw)
	}
	linearIssue := func(t *testing.T, callerRepo, webhookRepo string, n uint64) (*compassv1internal.ForgeCallRequest, forge.ForgeEvent) {
		t.Helper()
		linear.TransitionIssueResult = forge.Issue{Number: n, State: "closed"}
		call := &compassv1internal.ForgeCallRequest{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR},
			Call: &compassv1internal.ForgeCallRequest_TransitionIssueState{
				TransitionIssueState: &compassv1internal.TransitionIssueStateRequest{Repo: callerRepo, IssueNumber: n, State: "closed"},
			},
		}
		sw := newFakeLinearForge(secret, webhookRepo, "proj").changeIssueState(t, n, "https://linear.app/i", "completed")
		ev, ok, err := linearagent.ParseLinearDataEvent(sw.body)
		if err != nil || !ok {
			t.Fatalf("ParseLinearDataEvent ok=%v err=%v", ok, err)
		}
		return call, ev
	}

	cases := []struct {
		name                    string
		build                   func(*testing.T, string, string, uint64) (*compassv1internal.ForgeCallRequest, forge.ForgeEvent)
		callerRepo, webhookRepo string
		number                  uint64
		wantResolved            bool
	}{
		{"github issue, canonical repo", githubIssue, "octo/repo", "octo/repo", 11, true},
		{"github pull request, canonical repo", githubPR, "octo/repo", "octo/repo", 12, true},
		{"linear issue, canonical team key", linearIssue, "RIG", "RIG", 13, true},
		{"github issue, caller casing differs", githubIssue, "Octo/Repo", "octo/repo", 21, false},
		{"github pull request, caller casing differs", githubPR, "Octo/Repo", "octo/repo", 22, false},
		{"linear issue, caller casing differs", linearIssue, "rig", "RIG", 23, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, ev := tc.build(t, tc.callerRepo, tc.webhookRepo, tc.number)
			fresh := time.Now().Add(-time.Minute)

			res, err := svc.ExecuteForgeCallAsAccount(ctx, agent.ID, "sess", call)
			if err != nil || res.GetError() != nil {
				t.Fatalf("transition: err=%v in-band=%v", err, res.GetError())
			}
			if ev.Change != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE {
				t.Fatalf("parsed change = %v, want STATE", ev.Change)
			}

			got, ok, err := st.ConsumeStateTransition(ctx, store.ForgeProvider(ev.Provider), ev.Host, ev.Repo, store.ForgeArtifactKind(ev.Kind), ev.Number, ev.State, fresh)
			if err != nil {
				t.Fatalf("ConsumeStateTransition at %s/%s#%d: %v", ev.Host, ev.Repo, ev.Number, err)
			}
			if ok != tc.wantResolved {
				t.Fatalf("memo resolved = %v at webhook coordinate %s/%s#%d (caller repo %q), want %v", ok, ev.Host, ev.Repo, ev.Number, tc.callerRepo, tc.wantResolved)
			}
			if ok && got != agent.ID {
				t.Fatalf("memo actor = %q, want %q", got, agent.ID)
			}
		})
	}
}

func parseGitHubEcho(t *testing.T, sw signedWebhook) forge.ForgeEvent {
	t.Helper()
	ev, ok, err := forge.ParseGitHubEvent(sw.event, sw.body)
	if err != nil || !ok {
		t.Fatalf("ParseGitHubEvent(%s) ok=%v err=%v", sw.event, ok, err)
	}
	return ev
}
