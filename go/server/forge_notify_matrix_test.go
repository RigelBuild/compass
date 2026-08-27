//go:build unix

// The RIG-2848 forge-notification matrix over the LANDED pipeline: every
// ForgeNotificationKind × provider × scope, driven end to end from a fake
// signed webhook (forge_webhook_fakes_test.go) through the real ingress and
// normalize seam, then representative cells through the real NotifyRouter.
//
// Two seams are exercised, reflecting exactly what has landed on main:
//   - GitHub ingress is a real http.Handler (NewGitHubWebhookHandler): a fake
//     signed webhook POSTs through ServeHTTP → VerifyGitHubSignature →
//     ParseGitHubEvent, and the recordingSink captures the normalized event.
//   - The Linear data-change arm landed as pure functions (its /webhooks mount
//     is RIG-2717 T7, unlanded): a fake signed body runs through
//     linearagent.VerifySignature + ParseLinearDataEvent — the same normalize
//     seam, minus the not-yet-mounted HTTP handler.
//
// Then TestForgeNotifyMatrix_Route feeds normalized events through the real
// ingest.NotifyRouter with fakes on its OWN designed seams (store/dispatch/
// checks-roll-up), asserting the emitted ForgeNotification and the
// artifact-vs-container fan-out. The store-backed NotifyStore adapter + the
// full Runner/agent-turn delivery are gated on RIG-2732 T5/T7 + the RIG-2717
// mount; this suite is the seam-independent half that stacks onto them.
package server

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/linearagent"
)

const (
	mxIssue = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
	mxPR    = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST

	mxComment = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT
	mxState   = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE
	mxChecks  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS
	mxOpened  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED
	mxUpdate  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE
	mxReview  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW
)

// wantEvent is the normalized shape a matrix cell expects out of the ingress.
type wantEvent struct {
	provider     compassv1.ForgeProvider
	kind         compassv1internal.ForgeArtifactKind
	number       uint64
	change       compassv1internal.ForgeNotificationKind
	state        string // STATE verdict / REVIEW verdict
	url          string
	comment      bool   // a Comment ref is expected
	forgeAccount string // expected Comment.ForgeAccount (when comment)
	project      string // Linear container project id
}

func assertEvent(t *testing.T, ev forge.ForgeEvent, w wantEvent) {
	t.Helper()
	if ev.Provider != w.provider {
		t.Errorf("Provider = %v, want %v", ev.Provider, w.provider)
	}
	if ev.Kind != w.kind {
		t.Errorf("Kind = %v, want %v", ev.Kind, w.kind)
	}
	if ev.Number != w.number {
		t.Errorf("Number = %d, want %d", ev.Number, w.number)
	}
	if ev.Change != w.change {
		t.Errorf("Change = %v, want %v", ev.Change, w.change)
	}
	if w.state != "" && ev.State != w.state {
		t.Errorf("State = %q, want %q", ev.State, w.state)
	}
	if w.url != "" && ev.URL != w.url {
		t.Errorf("URL = %q, want %q", ev.URL, w.url)
	}
	if w.comment && ev.Comment == nil {
		t.Errorf("Comment = nil, want a comment ref")
	}
	if !w.comment && ev.Comment != nil {
		t.Errorf("Comment = %v, want nil", ev.Comment)
	}
	if w.forgeAccount != "" && ev.Comment.GetForgeAccount() != w.forgeAccount {
		t.Errorf("Comment.ForgeAccount = %q, want %q", ev.Comment.GetForgeAccount(), w.forgeAccount)
	}
	if w.project != "" && ev.Project != w.project {
		t.Errorf("Project = %q, want %q", ev.Project, w.project)
	}
}

// postGH drives one fake GitHub webhook through the REAL ingress handler and
// returns the single normalized event the sink captured. Fresh handler+sink per
// call keeps cells isolated.
func postGH(t *testing.T, secret []byte, sw signedWebhook) forge.ForgeEvent {
	t.Helper()
	h, sink := newTestHandler(t, secret)
	rec := doPost(h, sw.event, sw.delivery, sw.sig, sw.body)
	if rec.Code != 200 {
		t.Fatalf("ingress code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if sink.count() != 1 {
		t.Fatalf("enqueued = %d, want 1", sink.count())
	}
	return sink.events[0]
}

// TestForgeWebhookMatrix_GitHubIngress covers every GitHub notification kind on
// issues AND pull requests: a fake signed webhook → real ServeHTTP → real
// ParseGitHubEvent → the expected normalized ForgeEvent.
func TestForgeWebhookMatrix_GitHubIngress(t *testing.T) {
	secret := []byte("gh-webhook-secret")
	gh := newFakeGitHubForge(secret, "octo/repo")

	tests := []struct {
		name string
		send func() signedWebhook
		want wantEvent
	}{
		{
			name: "issue opened",
			send: func() signedWebhook { return gh.openIssue(t, 11, "https://gh/octo/repo/issues/11") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxIssue, number: 11, change: mxOpened, url: "https://gh/octo/repo/issues/11"},
		},
		{
			name: "issue closed (STATE)",
			send: func() signedWebhook { return gh.closeIssue(t, 11, "https://gh/octo/repo/issues/11") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxIssue, number: 11, change: mxState, state: "closed"},
		},
		{
			name: "issue edited (UPDATE)",
			send: func() signedWebhook { return gh.editIssue(t, 11, "https://gh/octo/repo/issues/11") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxIssue, number: 11, change: mxUpdate},
		},
		{
			name: "issue comment (COMMENT, kind=ISSUE)",
			send: func() signedWebhook {
				return gh.commentOnIssue(t, 11, "https://gh/octo/repo/issues/11#c1", "hi there", "octocat")
			},
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxIssue, number: 11, change: mxComment, url: "https://gh/octo/repo/issues/11#c1", comment: true, forgeAccount: "octocat"},
		},
		{
			name: "PR opened",
			send: func() signedWebhook { return gh.openPR(t, 12, "https://gh/octo/repo/pull/12") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxOpened, url: "https://gh/octo/repo/pull/12"},
		},
		{
			name: "PR merged (STATE=merged)",
			send: func() signedWebhook { return gh.mergePR(t, 12, "https://gh/octo/repo/pull/12") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxState, state: "merged"},
		},
		{
			name: "PR closed unmerged (STATE=closed)",
			send: func() signedWebhook { return gh.closePR(t, 12, "https://gh/octo/repo/pull/12") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxState, state: "closed"},
		},
		{
			name: "PR edited (UPDATE)",
			send: func() signedWebhook { return gh.editPR(t, 12, "https://gh/octo/repo/pull/12") },
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxUpdate},
		},
		{
			name: "PR conversation comment (COMMENT, kind=PR)",
			send: func() signedWebhook {
				return gh.commentOnPR(t, 12, "https://gh/octo/repo/pull/12#c2", "looks good", "octocat")
			},
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxComment, url: "https://gh/octo/repo/pull/12#c2", comment: true, forgeAccount: "octocat"},
		},
		{
			name: "PR review comment (COMMENT, kind=PR)",
			send: func() signedWebhook {
				return gh.reviewCommentOnPR(t, 12, "https://gh/octo/repo/pull/12#rc1", "nit here", "octocat")
			},
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxComment, url: "https://gh/octo/repo/pull/12#rc1", comment: true, forgeAccount: "octocat"},
		},
		{
			name: "PR review submitted (REVIEW)",
			send: func() signedWebhook {
				return gh.reviewPR(t, 12, "https://gh/octo/repo/pull/12#r1", "approving", "approved", "reviewer")
			},
			want: wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 12, change: mxReview, state: "approved", url: "https://gh/octo/repo/pull/12#r1", comment: true, forgeAccount: "reviewer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := postGH(t, secret, tc.send())
			assertEvent(t, ev, tc.want)
		})
	}
}

// TestForgeWebhookMatrix_LinearIngress covers the Linear data-change kinds
// through the landed normalize seam: a fake signed body → VerifySignature →
// ParseLinearDataEvent. Linear is issues+comments only; its PR/checks half is
// ErrUnsupported at the provider — no Linear webhook produces a PR/CHECKS event.
func TestForgeWebhookMatrix_LinearIngress(t *testing.T) {
	secret := []byte("ln-webhook-secret")
	ln := newFakeLinearForge(secret, "SEA", "proj-alpha")

	parse := func(t *testing.T, sw signedLinearWebhook) (forge.ForgeEvent, bool) {
		t.Helper()
		if !linearagent.VerifySignature(secret, sw.body, sw.sig) {
			t.Fatalf("VerifySignature = false, want true (valid Linear signature)")
		}
		ev, ok, err := linearagent.ParseLinearDataEvent(sw.body)
		if err != nil {
			t.Fatalf("ParseLinearDataEvent: %v", err)
		}
		return ev, ok
	}

	t.Run("issue create (OPENED, project carried)", func(t *testing.T) {
		ev, ok := parse(t, ln.openIssue(t, 5, "https://linear.app/rigel/SEA-5"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		assertEvent(t, ev, wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, kind: mxIssue, number: 5, change: mxOpened, url: "https://linear.app/rigel/SEA-5", project: "proj-alpha"})
	})

	t.Run("issue state change (STATE)", func(t *testing.T) {
		ev, ok := parse(t, ln.changeIssueState(t, 5, "https://linear.app/rigel/SEA-5", "completed"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		// "completed" is a closed Linear workflow-state type; pin the mapped
		// verdict literally rather than recomputing it via MapLinearState.
		assertEvent(t, ev, wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, kind: mxIssue, number: 5, change: mxState, state: "closed", project: "proj-alpha"})
	})

	t.Run("issue edit no-state (UPDATE)", func(t *testing.T) {
		ev, ok := parse(t, ln.editIssue(t, 5, "https://linear.app/rigel/SEA-5"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		assertEvent(t, ev, wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, kind: mxIssue, number: 5, change: mxUpdate, project: "proj-alpha"})
	})

	t.Run("comment create (COMMENT)", func(t *testing.T) {
		ev, ok := parse(t, ln.commentOnIssue(t, 5, "https://linear.app/rigel/SEA-5", "a comment", "matt"))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		assertEvent(t, ev, wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, kind: mxIssue, number: 5, change: mxComment, url: "https://linear.app/rigel/SEA-5", comment: true, forgeAccount: "matt", project: "proj-alpha"})
	})

	t.Run("issue remove (counted-and-dropped, ok=false)", func(t *testing.T) {
		_, ok := parse(t, ln.removeIssue(t, 5, "https://linear.app/rigel/SEA-5"))
		if ok {
			t.Error("ok = true, want false (remove has no notification kind)")
		}
	})
}

// TestForgeWebhookMatrix_TamperedRejected asserts the fail-closed contract on
// both providers: a body whose bytes changed after signing verifies false and
// (GitHub) is a 400 with nothing enqueued.
func TestForgeWebhookMatrix_TamperedRejected(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		secret := []byte("gh-webhook-secret")
		gh := newFakeGitHubForge(secret, "octo/repo")
		sw := gh.openIssue(t, 1, "u")
		h, sink := newTestHandler(t, secret)
		tampered := append([]byte{}, sw.body...)
		tampered[len(tampered)-1] = '!' // corrupt one body byte so the HMAC no longer matches (fail-closed happens before parse)
		rec := doPost(h, sw.event, sw.delivery, sw.sig, tampered)
		if rec.Code != 400 {
			t.Fatalf("code = %d, want 400 (fail-closed)", rec.Code)
		}
		if sink.count() != 0 {
			t.Errorf("enqueued = %d, want 0", sink.count())
		}
	})
	t.Run("linear", func(t *testing.T) {
		secret := []byte("ln-webhook-secret")
		ln := newFakeLinearForge(secret, "SEA", "proj-alpha")
		sw := ln.openIssue(t, 1, "u")
		tampered := append([]byte{}, sw.body...)
		tampered[len(tampered)-1] = '!'
		if linearagent.VerifySignature(secret, tampered, sw.sig) {
			t.Error("VerifySignature = true on tampered body, want false")
		}
	})
}

// ---- router-seam fakes (the NotifyRouter's OWN designed seams) ----

// matrixNotifyStore is the durable-seam fake: it holds the coordinate cursor and
// scripts the subscribers SubscribersForArtifact returns (artifact-scope vs
// opened container-scope, project-matched for Linear), recording upserts.
type matrixNotifyStore struct {
	cursor      *ingest.ArtifactCursor
	artifactSub []ingest.NotifySubscriber
	openedSub   []ingest.NotifySubscriber
	upserts     []ingest.ArtifactCursor
}

func (f *matrixNotifyStore) LoadArtifactCursor(context.Context, string, compassv1internal.ForgeArtifactKind, uint64) (*ingest.ArtifactCursor, error) {
	return f.cursor, nil
}

func (f *matrixNotifyStore) SubscribersForArtifact(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64, project string, opened bool) ([]ingest.NotifySubscriber, error) {
	if opened {
		var out []ingest.NotifySubscriber
		for _, s := range f.openedSub {
			if s.Project == project {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return f.artifactSub, nil
}

func (f *matrixNotifyStore) ListNotifyTargets(context.Context) ([]ingest.NotifyTarget, error) {
	return nil, nil
}

func (f *matrixNotifyStore) UpsertArtifactCursor(_ context.Context, cur ingest.ArtifactCursor) error {
	f.upserts = append(f.upserts, cur)
	f.cursor = &cur
	return nil
}

// matrixDispatcher records every notification dispatched, per account.
type matrixDispatcher struct {
	sent []*compassv1internal.ForgeNotification
}

func (d *matrixDispatcher) Notify(_ context.Context, _ string, n *compassv1internal.ForgeNotification) error {
	d.sent = append(d.sent, n)
	return nil
}

// matrixChecksRoller scripts the combined roll-up for a CHECKS event.
type matrixChecksRoller struct {
	res ingest.ChecksResult
}

func (c *matrixChecksRoller) RollUp(context.Context, string, uint64, string, string) (ingest.ChecksResult, error) {
	return c.res, nil
}

func mxRef() *compassv1.ForgeRef {
	return &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"}
}

// TestForgeNotifyMatrix_Route feeds normalized events through the REAL
// NotifyRouter and asserts the emitted ForgeNotification carries the right
// kind/coordinate/revision, that artifact-scope events reach the exact
// subscriber, and that the router never advances the delivery cursor (W3).
func TestForgeNotifyMatrix_Route(t *testing.T) {
	sub := ingest.NotifySubscriber{SubscriptionID: "sub-1", AgentAccountID: "acct-1"}

	tests := []struct {
		name       string
		ev         forge.ForgeEvent
		wantKind   compassv1internal.ForgeNotificationKind
		wantNumber uint64
		wantState  string
	}{
		{
			name:       "comment",
			ev:         forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxIssue, Number: 11, URL: "u#c", Change: mxComment, Comment: &compassv1internal.CommentRef{Url: "u#c", Body: "hi", ForgeAccount: "octocat"}},
			wantKind:   mxComment,
			wantNumber: 11,
		},
		{
			name:       "state merged",
			ev:         forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxPR, Number: 12, URL: "u", Change: mxState, State: "merged"},
			wantKind:   mxState,
			wantNumber: 12,
			wantState:  "merged",
		},
		{
			name:       "update",
			ev:         forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxIssue, Number: 11, URL: "u", Change: mxUpdate},
			wantKind:   mxUpdate,
			wantNumber: 11,
		},
		{
			name:       "review",
			ev:         forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxPR, Number: 12, URL: "u#r", Change: mxReview, State: "approved", Comment: &compassv1internal.CommentRef{Url: "u#r", Body: "lgtm", ForgeAccount: "reviewer"}},
			wantKind:   mxReview,
			wantNumber: 12,
			wantState:  "approved",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := &matrixNotifyStore{artifactSub: []ingest.NotifySubscriber{sub}}
			d := &matrixDispatcher{}
			r := ingest.NewNotifyRouter(st, d, &matrixChecksRoller{}, mxRef(), nil)
			if err := r.Route(t.Context(), tc.ev); err != nil {
				t.Fatalf("Route: %v", err)
			}
			if len(d.sent) != 1 {
				t.Fatalf("dispatched %d notifications, want 1", len(d.sent))
			}
			n := d.sent[0]
			if n.GetChange() != tc.wantKind {
				t.Errorf("Change = %v, want %v", n.GetChange(), tc.wantKind)
			}
			if n.GetNumber() != tc.wantNumber {
				t.Errorf("Number = %d, want %d", n.GetNumber(), tc.wantNumber)
			}
			if tc.wantState != "" && n.GetState() != tc.wantState {
				t.Errorf("State = %q, want %q", n.GetState(), tc.wantState)
			}
			if n.GetSubscriptionId() != sub.SubscriptionID {
				t.Errorf("SubscriptionId = %q, want %q", n.GetSubscriptionId(), sub.SubscriptionID)
			}
			// The revision is the snapshot digest the agent echoes in its ack.
			if n.GetRevision() == "" {
				t.Error("Revision = empty, want the snapshot digest")
			}
			// The router upserts the cursor BEFORE notify (fetch-side truth).
			if len(st.upserts) != 1 {
				t.Errorf("cursor upserts = %d, want 1 (before notify)", len(st.upserts))
			}
		})
	}
}

// TestForgeNotifyMatrix_ContainerScope covers the scope dimension: an OPENED
// event fans out to CONTAINER-scope subscribers (a GitHub repo container, and a
// Linear PROJECT container matched by project id), while an artifact-scope
// event reaches only the exact-coordinate subscriber.
func TestForgeNotifyMatrix_ContainerScope(t *testing.T) {
	t.Run("github repo container gets OPENED", func(t *testing.T) {
		container := ingest.NotifySubscriber{SubscriptionID: "repo-sub", AgentAccountID: "acct-repo"}
		st := &matrixNotifyStore{openedSub: []ingest.NotifySubscriber{container}}
		d := &matrixDispatcher{}
		r := ingest.NewNotifyRouter(st, d, &matrixChecksRoller{}, mxRef(), nil)
		ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxIssue, Number: 99, URL: "u", Change: mxOpened}
		if err := r.Route(t.Context(), ev); err != nil {
			t.Fatalf("Route: %v", err)
		}
		if len(d.sent) != 1 || d.sent[0].GetSubscriptionId() != "repo-sub" {
			t.Fatalf("container fan-out = %+v, want one notification to repo-sub", d.sent)
		}
		if d.sent[0].GetChange() != mxOpened {
			t.Errorf("Change = %v, want OPENED", d.sent[0].GetChange())
		}
	})

	t.Run("linear project container matches on project id", func(t *testing.T) {
		inProj := ingest.NotifySubscriber{SubscriptionID: "proj-sub", AgentAccountID: "acct-proj", Project: "proj-alpha"}
		otherProj := ingest.NotifySubscriber{SubscriptionID: "other-sub", AgentAccountID: "acct-other", Project: "proj-beta"}
		st := &matrixNotifyStore{openedSub: []ingest.NotifySubscriber{inProj, otherProj}}
		d := &matrixDispatcher{}
		r := ingest.NewNotifyRouter(st, d, &matrixChecksRoller{}, &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app"}, nil)
		ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app", Repo: "SEA", Kind: mxIssue, Number: 42, Project: "proj-alpha", URL: "u", Change: mxOpened}
		if err := r.Route(t.Context(), ev); err != nil {
			t.Fatalf("Route: %v", err)
		}
		if len(d.sent) != 1 || d.sent[0].GetSubscriptionId() != "proj-sub" {
			t.Fatalf("project fan-out = %+v, want exactly proj-sub (proj-alpha match)", d.sent)
		}
	})

	t.Run("artifact-scope reaches exact subscriber only", func(t *testing.T) {
		exact := ingest.NotifySubscriber{SubscriptionID: "art-sub", AgentAccountID: "acct-art"}
		container := ingest.NotifySubscriber{SubscriptionID: "repo-sub", AgentAccountID: "acct-repo"}
		st := &matrixNotifyStore{artifactSub: []ingest.NotifySubscriber{exact}, openedSub: []ingest.NotifySubscriber{container}}
		d := &matrixDispatcher{}
		r := ingest.NewNotifyRouter(st, d, &matrixChecksRoller{}, mxRef(), nil)
		// A COMMENT (not OPENED) is artifact-scope: no container fan-in.
		ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "octo/repo", Kind: mxIssue, Number: 7, URL: "u#c", Change: mxComment, Comment: &compassv1internal.CommentRef{Url: "u#c", Body: "x", ForgeAccount: "a"}}
		if err := r.Route(t.Context(), ev); err != nil {
			t.Fatalf("Route: %v", err)
		}
		if len(d.sent) != 1 || d.sent[0].GetSubscriptionId() != "art-sub" {
			t.Fatalf("artifact fan-out = %+v, want exactly art-sub (no container fan-in)", d.sent)
		}
	})
}

// TestForgeNotifyMatrix_CheckSuiteZeroNumberRejected pins a real gap surfaced by
// composing the landed pieces: parseGitHubCheckSuite (githubapp_webhook.go)
// carries head_sha but NO artifact number, and NotifyRouter.Route guards
// Number==0 → error. So a check_suite webhook cannot route until a
// head_sha→PR-number resolution step (T5/T7) sets Number. This asserts the
// router's zero-number guard fires, documenting the gap as a live contract.
func TestForgeNotifyMatrix_CheckSuiteZeroNumberRejected(t *testing.T) {
	secret := []byte("gh-webhook-secret")
	gh := newFakeGitHubForge(secret, "octo/repo")
	ev := postGH(t, secret, gh.completeCheckSuite(t, "abc123headsha"))

	// The landed parser produces a CHECKS event with HeadSHA set but Number 0.
	assertEvent(t, ev, wantEvent{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, kind: mxPR, number: 0, change: mxChecks})
	if ev.HeadSHA != "abc123headsha" {
		t.Errorf("HeadSHA = %q, want abc123headsha", ev.HeadSHA)
	}

	// Routing it must fail on the zero-number guard — the notification lane
	// needs the head_sha→number resolution (unlanded) before a check_suite can
	// deliver.
	st := &matrixNotifyStore{artifactSub: []ingest.NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}}}
	d := &matrixDispatcher{}
	r := ingest.NewNotifyRouter(st, d, &matrixChecksRoller{}, mxRef(), nil)
	if err := r.Route(t.Context(), ev); err == nil {
		t.Fatal("Route(check_suite with Number=0) = nil error, want the zero-number guard to reject it")
	}
	if len(d.sent) != 0 {
		t.Errorf("dispatched %d, want 0 (rejected before notify)", len(d.sent))
	}
}
