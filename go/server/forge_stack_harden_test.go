//go:build unix

package server

import (
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

func stackStatusCall(heads ...string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{Call: &compassv1internal.ForgeCallRequest_StackStatus{StackStatus: &compassv1internal.StackStatusRequest{Repo: testRepo, HeadRefs: heads}}}
}

func TestStackStatusReportsEachHead(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	p.ChecksResult = forge.Checks{HeadSHA: "sha", State: "success"}
	p.prs["own"] = &forge.PullRequest{Number: 1, State: "open", HeadRef: "own", BaseRef: "main"}
	p.prs["merged"] = &forge.PullRequest{Number: 2, State: "merged", HeadRef: "merged", BaseRef: "main"}
	p.prs["foreign"] = &forge.PullRequest{Number: 3, State: "open", HeadRef: "foreign", BaseRef: "main"}
	p.prs["unrecorded"] = &forge.PullRequest{Number: 4, State: "open", HeadRef: "unrecorded", BaseRef: "main"}
	seedOwnPR(st, 1, testAgentID)
	seedOwnPR(st, 2, testAgentID)
	seedOwnPR(st, 3, "acct-other-agent")

	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackStatusCall("missing", "own", "merged", "foreign", "unrecorded")))
	if len(got) != 5 {
		t.Fatalf("entries = %d, want 5", len(got))
	}
	for _, e := range got {
		if e.GetStatus() != doneStatus {
			t.Fatalf("%s status = %v, want done", e.GetHeadRef(), e.GetStatus())
		}
	}
	missing, own, merged, foreign, unrecorded := got[0], got[1], got[2], got[3], got[4]
	if missing.GetPullRequest() != nil || missing.GetAuthored() {
		t.Fatalf("missing = %v, want no PR and not authored", missing)
	}
	if !own.GetAuthored() || own.GetPullRequest().GetChecks().GetHeadSha() != "sha" {
		t.Fatalf("own = %v, want authored with checks", own)
	}
	if !merged.GetAuthored() || merged.GetPullRequest().GetChecks() != nil {
		t.Fatalf("merged = %v, want authored and no checks", merged)
	}
	if foreign.GetAuthored() || foreign.GetPullRequest() == nil {
		t.Fatalf("foreign = %v, want a PR not authored by the caller", foreign)
	}
	if unrecorded.GetAuthored() || unrecorded.GetPullRequest() == nil {
		t.Fatalf("unrecorded = %v, want a PR not authored by the caller", unrecorded)
	}
	// Checks are read only for open PRs: own, foreign, unrecorded.
	checks := 0
	for _, c := range p.Calls() {
		if c.Method == "Checks" {
			checks++
		}
	}
	if checks != 3 {
		t.Fatalf("checks reads = %d, want 3 (never for a non-open PR)", checks)
	}
}

func TestStackStatusProviderErrorsFailOnlyTheirHead(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	p.findErr["down"] = &forge.StatusError{Status: 502, Message: "bad gateway"}
	p.prs["nochecks"] = &forge.PullRequest{Number: 1, State: "open", HeadRef: "nochecks", BaseRef: "main"}
	p.prs["ok"] = &forge.PullRequest{Number: 2, State: "closed", HeadRef: "ok", BaseRef: "main"}
	seedOwnPR(st, 2, testAgentID)
	p.SetError("Checks", &forge.StatusError{Status: 503, Message: "checks down"})

	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackStatusCall("down", "nochecks", "ok")))
	if got[0].GetStatus() != failedStatus || got[0].GetError() == nil {
		t.Fatalf("down = %v, want failed with the provider error", got[0])
	}
	if got[1].GetStatus() != failedStatus || got[1].GetError() == nil {
		t.Fatalf("nochecks = %v, want failed with the checks error", got[1])
	}
	if got[2].GetStatus() != doneStatus || !got[2].GetAuthored() {
		t.Fatalf("ok = %v, want done and authored", got[2])
	}
}

// A sibling-owned PR as an entry head above a same-owner foreign base: refused
// in preflight, before the mirror rebases, pushes, or any PR is written.
func TestSubmitStackForeignHeadAboveForeignBaseTouchesNothing(t *testing.T) {
	svc, p, m, n := seedForeignBase(t, testOwnerID)
	p.prs["b"] = &forge.PullRequest{Number: 11, Title: "theirs", State: "open", HeadRef: "b", BaseRef: "a"}
	st := svc.store.(*fakeForgeStore)
	seedOwnPR(st, 11, "acct-sibling")

	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if got[1].GetStatus() != failedStatus || got[1].GetError().GetCode() != "permission_denied" {
		t.Fatalf("results = %v, want b refused permission_denied", got)
	}
	if len(m.applied)+len(m.rebases)+len(m.pushes)+len(n.posts) != 0 {
		t.Fatalf("applied=%v rebases=%v pushes=%v notices=%v, want no mirror work", m.applied, m.rebases, m.pushes, n.posts)
	}
	if p.count("update")+p.count("create") != 0 {
		t.Fatalf("events = %v, want no PR writes", *p.events)
	}
}

// The foreign branch moves on the forge between the seed and the push: the
// push leases on the seeded head, so it is rejected and nothing is written.
func TestSubmitStackForeignBaseMovedAfterSeedRejectsPush(t *testing.T) {
	svc, p, m, n := seedForeignBase(t, testOwnerID)
	m.afterRebase = func() { m.remote["refs/heads/base"] = "base-sibling-push" }

	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if got[0].GetStatus() != failedStatus || got[0].GetError().GetCode() != "failed_precondition" {
		t.Fatalf("results = %v, want the rejected push to fail the bottom entry", got)
	}
	if len(m.pushes) != 0 || m.remote["refs/heads/base"] != "base-sibling-push" {
		t.Fatalf("pushes=%v remote base=%s, want no push and the sibling's head kept", m.pushes, m.remote["refs/heads/base"])
	}
	if p.count("update")+p.count("create") != 0 || len(n.posts) != 0 {
		t.Fatalf("events=%v notices=%v, want no writes and no notice", *p.events, n.posts)
	}
}

// The foreign PR's owner edits it while the line is being submitted: only the
// block is written, from a fresh read, so the owner's title, base, and text stand.
func TestSubmitStackForeignRestampKeepsOwnersConcurrentEdit(t *testing.T) {
	svc, p, m, _ := seedForeignBase(t, testOwnerID)
	m.afterRebase = func() {
		base := p.prs["base"]
		base.Title, base.BaseRef, base.Body = "renamed by owner", "release", "owner rewrote this"
	}

	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if s := statuses(got); s[0] != doneStatus || s[1] != doneStatus {
		t.Fatalf("results = %v, want both done", got)
	}
	base := p.prs["base"]
	if base.Title != "renamed by owner" || base.BaseRef != "release" || !strings.Contains(base.Body, "owner rewrote this") || !strings.Contains(base.Body, "<!-- compass:stack -->") {
		t.Fatalf("foreign PR = %+v, want the owner's edit kept and only the block added", base)
	}
}
