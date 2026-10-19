//go:build unix

package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// stackProvider is a FakeProvider that keeps PRs by head, so a line of several
// entries sees distinct PRs. events is shared with fakeMirror to check order.
type stackProvider struct {
	*forge.FakeProvider
	prs       map[string]*forge.PullRequest
	next      uint64
	createErr map[string]error
	findErr   map[string]error
	events    *[]string
}

func newStackProvider(events *[]string) *stackProvider {
	return &stackProvider{FakeProvider: forge.NewFakeProvider("gh-author"), prs: map[string]*forge.PullRequest{}, next: 100, createErr: map[string]error{}, findErr: map[string]error{}, events: events}
}

func (p *stackProvider) FindPullRequestByHead(_ context.Context, _ string, head string) (forge.PullRequest, bool, error) {
	p.log("find " + head)
	if err := p.findErr[head]; err != nil {
		return forge.PullRequest{}, false, err
	}
	pr, ok := p.prs[head]
	if !ok {
		return forge.PullRequest{}, false, nil
	}
	return *pr, true, nil
}

func (p *stackProvider) CreatePullRequest(_ context.Context, _ string, in forge.CreatePR) (forge.PullRequest, error) {
	p.log("create " + in.HeadRef)
	if err := p.createErr[in.HeadRef]; err != nil {
		return forge.PullRequest{}, err
	}
	p.next++
	pr := &forge.PullRequest{Number: p.next, Title: in.Title, Body: in.Body, State: "open", HeadRef: in.HeadRef, BaseRef: in.BaseRef, Draft: in.Draft}
	p.prs[in.HeadRef] = pr
	return *pr, nil
}

func (p *stackProvider) UpdatePullRequest(_ context.Context, _ string, n uint64, u forge.UpdatePR) (forge.PullRequest, error) {
	pr := p.byNumber(n)
	p.log("update " + pr.HeadRef)
	if u.BodyOnly {
		pr.Body = u.Body
		return *pr, nil
	}
	pr.Title, pr.Body, pr.BaseRef = u.Title, u.Body, u.BaseRef
	if u.Ready {
		pr.Draft = false
		p.log("ready " + pr.HeadRef)
	}
	return *pr, nil
}

func (p *stackProvider) GetPullRequest(_ context.Context, _ string, n uint64) (forge.PullRequest, error) {
	p.log("get")
	return *p.byNumber(n), nil
}

func (p *stackProvider) TransitionPullRequestState(_ context.Context, _ string, n uint64, in forge.TransitionState) (forge.PullRequest, error) {
	pr := p.byNumber(n)
	p.log("close " + pr.HeadRef)
	pr.State = in.State
	return *pr, nil
}
func (p *stackProvider) byNumber(n uint64) *forge.PullRequest {
	for _, pr := range p.prs {
		if pr.Number == n {
			return pr
		}
	}
	return nil
}

func (p *stackProvider) log(ev string) { *p.events = append(*p.events, ev) }

func (p *stackProvider) count(prefix string) int {
	n := 0
	for _, ev := range *p.events {
		if strings.HasPrefix(ev, prefix) {
			n++
		}
	}
	return n
}

// fakeMirror records applies and pushes into the shared event log. remote holds
// forge branch heads; a push is checked against its leases like a real lease.
type fakeMirror struct {
	events  *[]string
	applied [][]string
	pushes  []map[string]string
	leases  []forge.Leases
	pushErr error
	remote  map[string]string
	// rebases records each Rebase line and its onto; conflict scripts the result.
	rebases  [][]string
	ontos    []string
	conflict bool
	// unmoved makes Rebase report that onto is already in the line.
	unmoved bool
	// afterRebase runs once the replay has moved refs, to race the remote before the push.
	afterRebase func()
}

func (m *fakeMirror) ApplyBundle(_ context.Context, _ string, _ []byte, refs []string) (forge.Leases, error) {
	*m.events = append(*m.events, "apply")
	m.applied = append(m.applied, refs)
	leases := forge.Leases{}
	for _, ref := range refs {
		leases[ref] = m.remote[ref]
	}
	return leases, nil
}

func (m *fakeMirror) Push(_ context.Context, _ string, refs map[string]string, expected forge.Leases) error {
	for ref := range refs {
		*m.events = append(*m.events, "push "+ref)
	}
	if m.pushErr != nil {
		err := m.pushErr
		m.pushErr = nil
		return err
	}
	for ref, commit := range refs {
		lease, ok := expected[ref]
		if !ok {
			return errors.New("push without a lease for " + ref)
		}
		if lease != m.remote[ref] {
			return errors.New("stale lease for " + ref)
		}
		if commit == "" && lease == "" {
			delete(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	for ref, commit := range refs {
		if commit == "" {
			delete(m.remote, ref)
		} else {
			m.remote[ref] = commit
		}
	}
	m.pushes = append(m.pushes, refs)
	m.leases = append(m.leases, expected)
	return nil
}

func (m *fakeMirror) Rebase(_ context.Context, _ string, refs []string, onto string) (map[string]string, bool, error) {
	*m.events = append(*m.events, "rebase "+strings.Join(refs, ","))
	m.rebases = append(m.rebases, refs)
	m.ontos = append(m.ontos, onto)
	if m.conflict {
		return nil, true, nil
	}
	heads := map[string]string{}
	if m.unmoved {
		return heads, false, nil
	}
	for _, ref := range refs {
		heads[ref] = strings.TrimPrefix(ref, "refs/heads/") + "-new"
	}
	if m.afterRebase != nil {
		m.afterRebase()
	}
	return heads, false, nil
}

func (m *fakeMirror) SeedRef(_ context.Context, _ string, ref string) (string, error) {
	*m.events = append(*m.events, "seed "+ref)
	if _, ok := m.remote[ref]; !ok {
		m.remote[ref] = strings.TrimPrefix(ref, "refs/heads/") + "-old"
	}
	return m.remote[ref], nil
}

func newStackServiceForTest(t *testing.T) (*forgeService, *fakeForgeStore, *stackProvider, *fakeMirror) {
	t.Helper()
	events := &[]string{}
	p := newStackProvider(events)
	p.GetIssueResult = forge.Issue{Number: 7, State: "open"}
	svc, st := newForgeServiceForTest(t, p.FakeProvider, forge.NewFakeProvider("gh-reviewer"))
	reg := newForgeProviderRegistry()
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}, p, forge.NewFakeProvider("gh-reviewer"), true); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc.providers = reg
	m := &fakeMirror{events: events, remote: map[string]string{}}
	svc.mirror = m
	return svc, st, p, m
}

func stackEntry(head, issue string) *compassv1internal.StackEntry {
	return &compassv1internal.StackEntry{HeadRef: head, Title: "title " + head, Body: "body " + head, IssueRef: issue}
}

func stackCall(key, ready string, bundle []byte, entries ...*compassv1internal.StackEntry) *compassv1internal.ForgeCallRequest {
	if len(entries) > 0 && entries[0].GetBaseRef() == "" {
		entries[0].BaseRef = "main"
	}
	return &compassv1internal.ForgeCallRequest{ClientRequestId: key, Call: &compassv1internal.ForgeCallRequest_SubmitStack{SubmitStack: &compassv1internal.SubmitStackRequest{
		Repo: testRepo, Entries: entries, ReadyHeadRef: ready, Bundle: bundle,
	}}}
}

func stackEntries(t *testing.T, res *compassv1internal.ForgeCallResult) []*compassv1internal.StackEntryResult {
	t.Helper()
	if fe := res.GetError(); fe != nil {
		t.Fatalf("call error = %v", fe)
	}
	if res.GetStack() == nil {
		t.Fatalf("result arm = %T, want stack", res.GetResult())
	}
	return res.GetStack().GetEntries()
}

func statuses(entries []*compassv1internal.StackEntryResult) []compassv1internal.StackEntryStatus {
	out := make([]compassv1internal.StackEntryStatus, len(entries))
	for i, e := range entries {
		out[i] = e.GetStatus()
	}
	return out
}

// seedPR puts PR #5 on head "a", with an ownership row for agent unless agent is empty.
func seedPR(st *fakeForgeStore, p *stackProvider, state string, agent, owner store.AccountID) {
	const number = 5
	p.prs["a"] = &forge.PullRequest{Number: number, Title: "t", State: state, HeadRef: "a", BaseRef: "main"}
	if agent != "" {
		st.recordArtifact(store.AuthoredArtifact{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Kind: store.ForgeArtifactKindPullRequest, Number: number, AgentAccountID: agent, OwnerUserID: owner})
	}
}

const (
	doneStatus    = compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_DONE
	failedStatus  = compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_FAILED
	skippedStatus = compassv1internal.StackEntryStatus_STACK_ENTRY_STATUS_SKIPPED
)

func TestSubmitStackNewEntryWithoutIssueFails(t *testing.T) {
	svc, _, p, _ := newStackServiceForTest(t)
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", ""))))
	if got[0].GetStatus() != failedStatus || got[0].GetError().GetCode() != "invalid_argument" || !strings.Contains(got[0].GetError().GetMessage(), "forge_create_issue") {
		t.Fatalf("entry = %v, want invalid_argument naming forge_create_issue", got[0])
	}
	if p.count("create") != 0 {
		t.Fatalf("creates = %d, want 0", p.count("create"))
	}
}

func TestSubmitStackTwoEntriesMakeChainedDraftsAndRetryCreatesNothing(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	call := func() []*compassv1internal.StackEntryResult {
		return stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7"))))
	}
	got := call()
	if s := statuses(got); s[0] != doneStatus || s[1] != doneStatus || !got[0].GetCreated() || !got[1].GetCreated() {
		t.Fatalf("results = %v, want two created entries", got)
	}
	if p.prs["a"].BaseRef != "main" || p.prs["b"].BaseRef != "a" || !p.prs["a"].Draft || !p.prs["b"].Draft {
		t.Fatalf("bases/draft = a:%s/%v b:%s/%v, want main and a, both draft", p.prs["a"].BaseRef, p.prs["a"].Draft, p.prs["b"].BaseRef, p.prs["b"].Draft)
	}
	// Two creates under one call key: two rows, neither carries the key.
	if len(st.prCreates) != 2 || len(st.recorded) != 2 {
		t.Fatalf("pr creates=%d rows=%d, want 2 and 2", len(st.prCreates), len(st.recorded))
	}
	for _, r := range st.recorded {
		if r.ClientRequestID != "" {
			t.Fatalf("row key = %q, want empty", r.ClientRequestID)
		}
	}
	if len(st.intents) != 2 || st.intents[0].RequestKey != "k:a" || st.intents[1].RequestKey != "k:b" {
		t.Fatalf("intents = %+v, want keys k:a and k:b", st.intents)
	}
	for _, h := range []string{"a", "b"} {
		if n := strings.Count(p.prs[h].Body, "<!-- compass:stack -->"); n != 1 {
			t.Fatalf("%s blocks = %d, want 1", h, n)
		}
	}
	creates, updates := p.count("create"), p.count("update")
	got = call()
	if s := statuses(got); s[0] != doneStatus || s[1] != doneStatus || got[0].GetCreated() || got[1].GetCreated() {
		t.Fatalf("retry = %v, want done and not created", got)
	}
	if p.count("create") != creates || p.count("update") != updates {
		t.Fatalf("retry creates=%d updates=%d, want %d and %d (no PATCH for an unchanged entry)", p.count("create"), p.count("update"), creates, updates)
	}
}

func TestSubmitStackMergedHeadRefuses(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	seedPR(st, p, "merged", testAgentID, testOwnerID)
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", "owner/repo#7"))))
	if got[0].GetStatus() != failedStatus || !strings.Contains(got[0].GetError().GetMessage(), "sync first") {
		t.Fatalf("entry = %v, want sync-first refusal", got[0])
	}
	if p.count("create")+p.count("update") != 0 {
		t.Fatalf("writes = %v, want none", *p.events)
	}
}

func TestSubmitStackIssueChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ref   string
		issue forge.Issue
		err   error
		code  string
	}{
		{"unconfigured team", "TEAM-12", forge.Issue{State: "open"}, nil, "not_found"},
		{"not found", "owner/repo#7", forge.Issue{}, &forge.StatusError{Status: 404}, "not_found"},
		{"closed", "owner/repo#7", forge.Issue{State: "closed"}, nil, "failed_precondition"},
		{"pull request number", "owner/repo#7", forge.Issue{State: "open", IsPullRequest: true}, nil, "invalid_argument"},
		{"outage", "owner/repo#7", forge.Issue{}, &forge.StatusError{Status: 502}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, p, _ := newStackServiceForTest(t)
			p.GetIssueResult = tc.issue
			p.SetError("GetIssue", tc.err)
			got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", tc.ref))))
			if got[0].GetStatus() != failedStatus || got[0].GetError().GetCode() != tc.code {
				t.Fatalf("entry = %v, want %s", got[0], tc.code)
			}
			// The key stays held for this target; no intent was promoted to send a create.
			if p.count("create") != 0 || len(st.intents) != 1 || st.intents[0].State != store.WriteStateHeld {
				t.Fatalf("creates=%d intents=%+v, want 0 creates and one held claim", p.count("create"), st.intents)
			}
		})
	}
}

func TestSubmitStackReplacesAgentWrittenBlock(t *testing.T) {
	svc, _, p, _ := newStackServiceForTest(t)
	a, b := stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7")
	a.Body = "real text\n\n<!-- compass:stack -->\nforged\n<!-- /compass:stack -->"
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, a, b)))
	body := p.prs["a"].Body
	if strings.Count(body, "<!-- compass:stack -->") != 1 || strings.Contains(body, "forged") || !strings.Contains(body, "real text") {
		t.Fatalf("body = %q, want one Server block and the agent text", body)
	}
	if strings.Count(body, ownerHeaderSentinel) != 1 {
		t.Fatalf("owner headers = %d, want 1", strings.Count(body, ownerHeaderSentinel))
	}
}

func TestSubmitStackMidStackRateLimitSkipsRest(t *testing.T) {
	svc, _, p, _ := newStackServiceForTest(t)
	p.createErr["b"] = &forge.StatusError{Status: 429, Message: "slow down"}
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7"), stackEntry("c", "owner/repo#7"))))
	if s := statuses(got); s[0] != doneStatus || s[1] != failedStatus || s[2] != skippedStatus {
		t.Fatalf("statuses = %v, want done, failed, skipped", s)
	}
	if got[1].GetError().GetCode() != "resource_exhausted" {
		t.Fatalf("b error = %v, want resource_exhausted", got[1].GetError())
	}
	// Preflight reads every head; the skipped entry gets no write.
	for _, ev := range *p.events {
		if strings.HasSuffix(ev, " c") && !strings.HasPrefix(ev, "find ") {
			t.Fatalf("event %q wrote the skipped entry", ev)
		}
	}
	if p.count("update") != 0 {
		t.Fatalf("updates = %d, want no restamp after a failure", p.count("update"))
	}
}

func TestSubmitStackOnlyReadyHeadLeavesDraft(t *testing.T) {
	svc, _, p, _ := newStackServiceForTest(t)
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "b", nil, stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7"))))
	if !p.prs["a"].Draft || p.prs["b"].Draft || p.count("ready") != 1 {
		t.Fatalf("draft a=%v b=%v ready=%d, want a draft and only b ready", p.prs["a"].Draft, p.prs["b"].Draft, p.count("ready"))
	}
}

func TestSubmitStackBundlePushesBeforePRWrites(t *testing.T) {
	svc, _, p, m := newStackServiceForTest(t)
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7"))))
	if len(m.applied) != 1 || strings.Join(m.applied[0], ",") != "refs/heads/a,refs/heads/b" {
		t.Fatalf("applied = %v, want the entries' refs", m.applied)
	}
	if len(m.pushes) != 2 || m.pushes[0]["refs/heads/a"] != "refs/heads/a" || m.pushes[1]["refs/heads/b"] != "refs/heads/b" {
		t.Fatalf("pushes = %v, want each head at its bundled ref", m.pushes)
	}
	order := strings.Join(*p.events, "|")
	if strings.Index(order, "apply") > strings.Index(order, "create a") ||
		strings.Index(order, "push refs/heads/a") > strings.Index(order, "create a") ||
		strings.Index(order, "push refs/heads/b") > strings.Index(order, "create b") {
		t.Fatalf("events = %s, want apply and push before each create", order)
	}
}

func TestSubmitStackBundleWithoutMirrorFails(t *testing.T) {
	svc, _, p, _ := newStackServiceForTest(t)
	svc.mirror = nil
	res := svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/repo#7")))
	if fe := res.GetError(); fe == nil || fe.GetCode() != "failed_precondition" {
		t.Fatalf("result = %v, want failed_precondition", res)
	}
	if len(*p.events) != 0 {
		t.Fatalf("provider events = %v, want none", *p.events)
	}
}

// A foreign PR as an entry head is never written, whatever its owner.
func TestSubmitStackForeignHeadRefuses(t *testing.T) {
	for _, owner := range []store.AccountID{testOwnerID, "acct-other-owner"} {
		t.Run(string(owner), func(t *testing.T) {
			svc, st, p, m := newStackServiceForTest(t)
			seedPR(st, p, "open", "acct-other-agent", owner)
			got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", []byte("bundle"), stackEntry("a", ""), stackEntry("b", "owner/repo#7"))))
			if s := statuses(got); s[0] != failedStatus || s[1] != skippedStatus || got[0].GetError().GetCode() != "permission_denied" {
				t.Fatalf("results = %v, want permission_denied then skipped", got)
			}
			if len(m.pushes) != 0 || p.count("update")+p.count("create") != 0 {
				t.Fatalf("pushes=%d events=%v, want no writes", len(m.pushes), *p.events)
			}
		})
	}
}

// fakeNotifier records notices as the comms seam would post them.
type fakeNotifier struct {
	opened []string
	posts  []string
	keys   []string
}

func (n *fakeNotifier) OpenDMAsAccount(_ context.Context, account store.AccountID, req *compassv1.OpenDMRequest) (*compassv1.OpenDMResponse, error) {
	n.opened = append(n.opened, string(account)+"->"+req.GetPeerHandle())
	return &compassv1.OpenDMResponse{Channel: &compassv1.Channel{Id: "dm-1"}}, nil
}

func (n *fakeNotifier) PostAsAccount(_ context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error) {
	n.posts = append(n.posts, string(account)+"@"+req.GetChannelId()+": "+req.GetBlocks()[0].GetText())
	n.keys = append(n.keys, req.GetClientRequestId())
	return &compassv1.PostMessageResponse{}, nil
}

// seedForeignBase records "base" as an open PR by a sibling agent of tc's owner, with the caller's line above it.
func seedForeignBase(t *testing.T, owner store.AccountID) (*forgeService, *stackProvider, *fakeMirror, *fakeNotifier) {
	t.Helper()
	svc, st, p, m := newStackServiceForTest(t)
	const sibling = store.AccountID("acct-sibling")
	st.accounts[sibling] = store.Account{ID: sibling, Handle: "sibling", Agent: &store.AgentAccount{OwnerUserID: owner}}
	p.prs["base"] = &forge.PullRequest{Number: 9, Title: "sibling work", Body: "sibling text", State: "open", HeadRef: "base", BaseRef: "main"}
	st.recordArtifact(store.AuthoredArtifact{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Kind: store.ForgeArtifactKindPullRequest, Number: 9, AgentAccountID: sibling, OwnerUserID: owner})
	n := &fakeNotifier{}
	svc.notifier = n
	return svc, p, m, n
}

func foreignLineCall() *compassv1internal.ForgeCallRequest {
	a := stackEntry("a", "owner/repo#7")
	a.BaseRef = "base"
	return stackCall("k", "", []byte("bundle"), a, stackEntry("b", "owner/repo#7"))
}

func TestSubmitStackSameOwnerForeignBaseIsRebasedPushedAndRestamped(t *testing.T) {
	svc, p, m, n := seedForeignBase(t, testOwnerID)
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if s := statuses(got); s[0] != doneStatus || s[1] != doneStatus {
		t.Fatalf("results = %v, want both done", got)
	}
	line := []string{"refs/heads/base", "refs/heads/a", "refs/heads/b"}
	if len(m.rebases) != 1 || strings.Join(m.rebases[0], ",") != strings.Join(line, ",") || m.ontos[0] != "refs/remotes/origin/main" {
		t.Fatalf("rebases = %v onto %v, want one replay of %v onto origin/main", m.rebases, m.ontos, line)
	}
	if len(m.pushes) != 1 || len(m.pushes[0]) != 3 || m.pushes[0]["refs/heads/base"] != "base-new" {
		t.Fatalf("pushes = %v, want one leased push of the whole moved line", m.pushes)
	}
	base := p.prs["base"]
	if base.Title != "sibling work" || base.BaseRef != "main" || !strings.HasPrefix(base.Body, "<!-- compass:stack -->") || !strings.Contains(base.Body, "sibling text") {
		t.Fatalf("foreign PR = %+v, want only its stack block stamped", base)
	}
	for _, head := range []string{"base", "a", "b"} {
		if !strings.Contains(p.prs[head].Body, "stack containing 3 PRs") {
			t.Fatalf("%s body = %q, want the whole line in its block", head, p.prs[head].Body)
		}
	}
	if !strings.Contains(p.prs["a"].Body, "#9 ") {
		t.Fatalf("a body = %q, want the foreign PR in its block", p.prs["a"].Body)
	}
	if p.prs["a"].BaseRef != "base" || p.prs["b"].BaseRef != "a" {
		t.Fatalf("bases = %s, %s, want base, a", p.prs["a"].BaseRef, p.prs["b"].BaseRef)
	}
	if len(n.posts) != 1 || n.opened[0] != string(testAgentID)+"->sibling" {
		t.Fatalf("notices = %v via %v, want one in the caller's DM with sibling", n.posts, n.opened)
	}
	for _, want := range []string{"base", "#9", "base-old", "base-new"} {
		if !strings.Contains(n.posts[0], want) {
			t.Fatalf("notice %q, want it to name %s", n.posts[0], want)
		}
	}
}

func TestSubmitStackForeignBaseAlreadyCurrentIsNotPushedOrNoticed(t *testing.T) {
	svc, _, m, n := seedForeignBase(t, testOwnerID)
	m.unmoved = true
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if s := statuses(got); s[0] != doneStatus || s[1] != doneStatus {
		t.Fatalf("results = %v, want both done", got)
	}
	for _, push := range m.pushes {
		if _, ok := push["refs/heads/base"]; ok {
			t.Fatalf("pushes = %v, want the foreign bookmark untouched", m.pushes)
		}
	}
	if len(n.posts) != 0 {
		t.Fatalf("notices = %v, want none", n.posts)
	}
}

func TestSubmitStackForeignBaseConflictPushesNothingAndAsksToResolve(t *testing.T) {
	svc, p, m, n := seedForeignBase(t, testOwnerID)
	m.conflict = true
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if s := statuses(got); s[0] != failedStatus || s[1] != skippedStatus || got[0].GetError().GetCode() != "failed_precondition" {
		t.Fatalf("results = %v, want failed_precondition then skipped", got)
	}
	if len(m.pushes) != 0 || p.count("update")+p.count("create") != 0 {
		t.Fatalf("pushes=%v events=%v, want no writes", m.pushes, *p.events)
	}
	if len(n.posts) != 1 || !strings.Contains(n.posts[0], "resolve") || !strings.Contains(n.posts[0], "#9") {
		t.Fatalf("notices = %v, want one resolve request naming #9", n.posts)
	}
}

func TestSubmitStackDifferentOwnerForeignBaseRefuses(t *testing.T) {
	svc, p, m, n := seedForeignBase(t, "acct-other-owner")
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, foreignLineCall()))
	if s := statuses(got); s[0] != failedStatus || s[1] != skippedStatus || got[0].GetError().GetCode() != "permission_denied" {
		t.Fatalf("results = %v, want permission_denied then skipped", got)
	}
	if len(m.rebases)+len(m.pushes)+len(n.posts) != 0 || p.count("update")+p.count("create") != 0 {
		t.Fatalf("rebases=%v pushes=%v notices=%v events=%v, want nothing", m.rebases, m.pushes, n.posts, *p.events)
	}
}

func retireCall(head string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{Call: &compassv1internal.ForgeCallRequest_RetireHead{RetireHead: &compassv1internal.RetireHeadRequest{Repo: testRepo, HeadRef: head}}}
}

func TestRetireHeadClosesOwnPRAndDeletesOnlyItsBranch(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	seedPR(st, p, "open", testAgentID, testOwnerID)
	m.remote["refs/heads/a"], m.remote["refs/heads/main"] = "a-sha", "main-sha"
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, retireCall("a")))
	if got[0].GetStatus() != doneStatus || p.prs["a"].State != "closed" {
		t.Fatalf("result = %v state=%s, want done and closed", got, p.prs["a"].State)
	}
	if len(m.pushes) != 1 || len(m.pushes[0]) != 1 || m.pushes[0]["refs/heads/a"] != "" || m.leases[0]["refs/heads/a"] != "a-sha" {
		t.Fatalf("pushes = %v leases = %v, want one delete of refs/heads/a leased on a-sha", m.pushes, m.leases)
	}
	if _, ok := m.remote["refs/heads/main"]; !ok {
		t.Fatal("main was deleted, want only the retired branch gone")
	}
}

func TestRetireHeadRetryAfterEitherStepSendsNoSecondClose(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	seedPR(st, p, "open", testAgentID, testOwnerID)
	m.remote["refs/heads/a"] = "a-sha"
	m.pushErr = errors.New("network")
	if res := svc.ExecuteForgeCallAsAccountMust(t, retireCall("a")); res.GetError() == nil {
		t.Fatalf("first retire = %v, want the delete error", res)
	}
	for range 2 {
		stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, retireCall("a")))
	}
	if p.count("close") != 1 || len(m.pushes) != 1 {
		t.Fatalf("closes=%d deletes=%d, want 1 and 1", p.count("close"), len(m.pushes))
	}
}

// The delete landed but its answer was lost: the retry sees no branch and succeeds without pushing.
func TestRetireHeadRetryAfterLostDeleteResponseSucceeds(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	seedPR(st, p, "closed", testAgentID, testOwnerID)
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, retireCall("a")))
	if got[0].GetStatus() != doneStatus || p.count("close") != 0 || len(m.pushes) != 0 {
		t.Fatalf("result = %v closes=%d pushes=%v, want done with no close and no push", got, p.count("close"), m.pushes)
	}
}

func TestRetireHeadForeignHeadRefuses(t *testing.T) {
	for _, agent := range []store.AccountID{"acct-other-agent", ""} {
		svc, st, p, m := newStackServiceForTest(t)
		seedPR(st, p, "open", agent, testOwnerID)
		res := svc.ExecuteForgeCallAsAccountMust(t, retireCall("a"))
		if fe := res.GetError(); fe == nil || fe.GetCode() != "permission_denied" {
			t.Fatalf("agent %q: result = %v, want permission_denied", agent, res)
		}
		if p.count("close") != 0 || len(m.pushes) != 0 {
			t.Fatalf("agent %q: closes=%d pushes=%d, want none", agent, p.count("close"), len(m.pushes))
		}
	}
}

func TestTransitionPullRequestStateRequiresAuthor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent store.AccountID
		want  string
	}{
		{"own", testAgentID, ""},
		{"another agent", "acct-other-agent", "permission_denied"},
		{"no row", "", "permission_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			if tc.agent != "" {
				seedOwnPR(st, 9, tc.agent)
			}
			res := svc.ExecuteForgeCallAsAccountMust(t, transitionPRCall("closed"))
			if got := res.GetError().GetCode(); got != tc.want {
				t.Fatalf("error code = %q, want %q", got, tc.want)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want == ""]; len(author.Calls()) != want {
				t.Fatalf("provider calls = %d, want %d", len(author.Calls()), want)
			}
		})
	}
}

// seedOwnPR records an ownership row for a PR on the default test coordinate.
func seedOwnPR(st *fakeForgeStore, number uint64, agent store.AccountID) {
	st.recordArtifact(store.AuthoredArtifact{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Kind: store.ForgeArtifactKindPullRequest, Number: number, AgentAccountID: agent, OwnerUserID: testOwnerID})
}
