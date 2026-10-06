//go:build pgtest && unix

package server

// The Linear session responder end to end: signed webhook -> real handler, dispatcher,
// store and comms, with a fake Linear recording the GraphQL return path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	linE2EPublicURL = "https://compass.e2e.test"
	linE2EClientID  = "cid"
	linE2ESecret    = "csecret"
	linE2EOpThought = "agentActivityCreate"
	linE2EOpSession = "agentSessionUpdate"
)

// linE2ECall is one GraphQL mutation the fake Linear received.
type linE2ECall struct {
	Op           string
	SessionID    string
	Token        string
	Status       int
	Content      linearagent.ActivityContent
	ExternalURLs []linearagent.ExternalURL
}

// linE2EGraphQLRequest is the slice of a mutation body the fake records.
type linE2EGraphQLRequest struct {
	Query     string `json:"query"`
	Variables struct {
		ID    string `json:"id"`
		Input struct {
			AgentSessionID string                      `json:"agentSessionId"`
			Content        linearagent.ActivityContent `json:"content"`
			ExternalURLs   []linearagent.ExternalURL   `json:"externalUrls"`
		} `json:"input"`
	} `json:"variables"`
}

// fakeLinearAPI plays Linear's OAuth token endpoint and GraphQL endpoint.
type fakeLinearAPI struct {
	srv *httptest.Server

	mu        sync.Mutex
	mints     int
	token     string
	fail401   int
	served401 int
	calls     []linE2ECall
	gate      chan struct{}
}

func newFakeLinearAPI(t *testing.T) *fakeLinearAPI {
	t.Helper()
	f := &fakeLinearAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.serveToken)
	mux.HandleFunc("POST /graphql", f.serveGraphQL)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinearAPI) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_id") != linE2EClientID ||
		r.PostForm.Get("client_secret") != linE2ESecret {
		http.Error(w, "bad client credentials", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.mints++
	f.token = fmt.Sprintf("tok-%d", f.mints)
	tok := f.token
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, tok); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (f *fakeLinearAPI) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req linE2EGraphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	call := linE2ECall{Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	switch {
	case strings.Contains(req.Query, linE2EOpThought+"("):
		call.Op = linE2EOpThought
		call.SessionID = req.Variables.Input.AgentSessionID
		call.Content = req.Variables.Input.Content
	case strings.Contains(req.Query, linE2EOpSession+"("):
		call.Op = linE2EOpSession
		call.SessionID = req.Variables.ID
		call.ExternalURLs = req.Variables.Input.ExternalURLs
	default:
		http.Error(w, "unknown operation", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	// A stale bearer is also a 401, so a client that skipped the re-mint fails here.
	if f.fail401 > 0 || call.Token != f.token {
		if f.fail401 > 0 {
			f.fail401--
		}
		f.served401++
		call.Status = http.StatusUnauthorized
	} else {
		call.Status = http.StatusOK
	}
	f.calls = append(f.calls, call)
	gate := f.gate
	f.mu.Unlock()

	if call.Status != http.StatusOK {
		http.Error(w, "unauthorized", call.Status)
		return
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"data":{%q:{"success":true}}}`, call.Op); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// hold parks every accepted GraphQL call until the returned release runs.
func (f *fakeLinearAPI) hold() func() {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

func (f *fakeLinearAPI) failNext401() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail401++
}

func (f *fakeLinearAPI) counters() (mints, served401 int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints, f.served401
}

func (f *fakeLinearAPI) callsFor(sessionID string) []linE2ECall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []linE2ECall
	for _, c := range f.calls {
		if c.SessionID == sessionID {
			out = append(out, c)
		}
	}
	return out
}

// linE2EWire is the assembled loop: store, comms, dispatcher, ingress, fake Linear.
type linE2EWire struct {
	st         *store.Store
	cm         *comms.Comms
	linear     *fakeLinearAPI
	handler    http.Handler
	resolver   *linearagent.Resolver
	secret     []byte
	adminID    store.AccountID
	bridgeID   store.AccountID
	supervisor store.Account
	manager    store.Account
	routingCh  store.ChannelID
}

func newLinE2EWire(t *testing.T) *linE2EWire {
	t.Helper()
	ctx := t.Context()
	st := forgeTestStore(t)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	bridge, err := st.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	supervisor, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: rootSupervisorHandle, DisplayName: rootSupervisorDisplayName, Role: rootSupervisorRole})
	if err != nil {
		t.Fatalf("CreateAgent(supervisor): %v", err)
	}
	routingCh, err := st.EnsureLinearRoutingChannel(ctx, admin.ID, supervisor.ID, bridge.ID)
	if err != nil {
		t.Fatalf("EnsureLinearRoutingChannel: %v", err)
	}
	manager, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "lane-manager", DisplayName: "Lane Manager", Role: "manager"})
	if err != nil {
		t.Fatalf("CreateAgent(manager): %v", err)
	}
	// A placement is what makes the manager live for the owning-manager walk.
	if err := st.RecordAgentPlacement(ctx, manager.ID, "runner-1", "compass-"+string(manager.ID)); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	// Authored rows: RIG-101 by the live manager; RIG-404 by an unplaced (despawned) peer
	// under it; RIG-405 by an agent with no live ancestor at all.
	peer, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "lane-peer", DisplayName: "Lane Peer", ParentAgentID: manager.ID})
	if err != nil {
		t.Fatalf("CreateAgent(peer): %v", err)
	}
	orphan, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "orphan", DisplayName: "Orphan"})
	if err != nil {
		t.Fatalf("CreateAgent(orphan): %v", err)
	}
	for number, author := range map[uint64]store.AccountID{101: manager.ID, 404: peer.ID, 405: orphan.ID} {
		if err := st.RecordAuthoredArtifact(ctx, store.AuthoredArtifact{
			Provider: store.ForgeProviderLinear, Host: forge.LinearHost, Repo: "RIG",
			Kind: store.ForgeArtifactKindIssue, Number: number,
			AgentAccountID: author, OwnerUserID: admin.ID,
			CreatedAtUnixMS: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("RecordAuthoredArtifact(RIG-%d): %v", number, err)
		}
	}

	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	cm := comms.NewComms(st, commsBus, nil, admin.ID)

	linear := newFakeLinearAPI(t)
	tokens := linearagent.NewTokenSource(linE2EClientID, linE2ESecret, linear.srv.Client(), linear.srv.URL+"/oauth/token")
	resolver := buildLinearResolver(st, admin.ID)
	d := buildLinearResponder(ServeConfig{PublicURL: linE2EPublicURL}, st, cm, bridge.ID, tokens, linear.srv.URL+"/graphql", resolver)
	if d == nil {
		t.Fatal("buildLinearResponder returned nil with a token source")
	}
	runCtx, cancel := context.WithCancel(ctx)
	g, gctx := errgroup.WithContext(runCtx)
	startLinearResponder(gctx, g, d, commsBus)
	t.Cleanup(func() {
		cancel()
		if err := g.Wait(); err != nil {
			t.Errorf("responder group: %v", err)
		}
	})

	secret := []byte("linear-e2e-webhook-secret")
	// The mount path is the network door's concern; this test drives the handler directly.
	_, handler := NewLinearWebhookHandler(func(context.Context) ([]byte, error) { return secret, nil }, nil, d, nil)
	return &linE2EWire{
		st: st, cm: cm, linear: linear, handler: handler, resolver: resolver, secret: secret,
		adminID: admin.ID, bridgeID: bridge.ID, supervisor: supervisor, manager: manager, routingCh: routingCh,
	}
}

// linE2ESessionBody marshals an AgentSessionEvent for signing; text is the
// promptContext on `created` and the activity body on `prompted`.
func linE2ESessionBody(t *testing.T, action, sessionID string, issue linearagent.Issue, text string, ts time.Time) []byte {
	t.Helper()
	ev := linearagent.SessionEvent{
		Type:             linearTypeSession,
		Action:           action,
		WebhookTimestamp: ts.UnixMilli(),
		AgentSession:     linearagent.AgentSession{ID: sessionID, Issue: issue},
	}
	if action == "prompted" {
		ev.AgentActivity.Content.Body = text
	} else {
		ev.PromptContext = text
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal session event: %v", err)
	}
	return body
}

func (w *linE2EWire) deliver(t *testing.T, body []byte, sig, delivery string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, linearWebhookPath, strings.NewReader(string(body)))
	req.Header.Set(linearSignatureHeader, sig)
	req.Header.Set(linearDeliveryHeader, delivery)
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	return rec.Code
}

func (w *linE2EWire) deliverSigned(t *testing.T, body []byte) int {
	t.Helper()
	return w.deliver(t, body, linSign(w.secret, body), uuid.NewString())
}

// messagesWithText lists the channel as the bridge (a member of every channel it posts to).
func (w *linE2EWire) messagesWithText(t *testing.T, channel store.ChannelID, text string) []store.Message {
	t.Helper()
	msgs, err := w.st.ListMessages(t.Context(), store.ListMessagesQuery{Actor: w.bridgeID, ChannelID: channel, Page: store.Page{Limit: 500}})
	if err != nil {
		t.Fatalf("ListMessages(%s): %v", channel, err)
	}
	var out []store.Message
	for _, m := range msgs {
		if textOfE2E(m) == text {
			out = append(out, m)
		}
	}
	return out
}

func (w *linE2EWire) waitForMessage(t *testing.T, channel store.ChannelID, text string) store.Message {
	t.Helper()
	var got []store.Message
	linE2EWaitUntil(t, fmt.Sprintf("message %q in channel %s", text, channel), func() bool {
		got = w.messagesWithText(t, channel, text)
		return len(got) > 0
	})
	return got[0]
}

// barrier pushes a fresh valid event and waits for its post; the dispatcher drains
// in order, so every event enqueued before it has been fully handled.
func (w *linE2EWire) barrier(t *testing.T) {
	t.Helper()
	sessionID := uuid.NewString()
	text := "barrier " + sessionID
	if code := w.deliverSigned(t, linE2ESessionBody(t, "created", sessionID, linearagent.Issue{}, text, time.Now())); code != http.StatusOK {
		t.Fatalf("barrier delivery status = %d, want 200", code)
	}
	w.waitForMessage(t, w.routingCh, text)
}

// assertNothingHappened checks a dropped session left no trace anywhere downstream.
func (w *linE2EWire) assertNothingHappened(t *testing.T, sessionID, text string) {
	t.Helper()
	if calls := w.linear.callsFor(sessionID); len(calls) != 0 {
		t.Errorf("GraphQL calls for dropped session = %+v, want none", calls)
	}
	if _, err := w.st.LinearAgentSession(t.Context(), sessionID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("association for dropped session: err = %v, want store.ErrNotFound", err)
	}
	for _, ch := range []store.ChannelID{w.routingCh, w.manager.Agent.HomeChannelID} {
		if msgs := w.messagesWithText(t, ch, text); len(msgs) != 0 {
			t.Errorf("channel %s holds %d messages from the dropped session, want 0", ch, len(msgs))
		}
	}
}

func linE2EWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := timeAfter()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s", testTimeout, what)
		case <-tick.C:
		}
	}
}

func TestLinearWebhookE2E(t *testing.T) {
	w := newLinE2EWire(t)

	t.Run("created_recorded_owner", w.scenarioCreatedRecordedOwner)
	t.Run("created_unstamped_routes_to_supervisor", w.scenarioCreatedUnstamped)
	t.Run("created_despawned_author_walks_up", w.scenarioDespawnedAuthor)
	t.Run("replayed_delivery_posts_once", w.scenarioReplay)
	t.Run("prompted_lands_in_same_topic", w.scenarioPrompted)
	t.Run("manager_reply_emits_one_response", w.scenarioManagerReply)
	t.Run("graphql_401_remints_once", w.scenario401)
	t.Run("tampered_signature_rejected", w.scenarioTampered)
	t.Run("stale_timestamp_dropped", w.scenarioStale)
	t.Run("session_link_follows_later_ownership", w.scenarioSessionLinkFollowsOwnership)
}

func (w *linE2EWire) scenarioCreatedRecordedOwner(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>RIG-101 prompt context " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-101", Identifier: "RIG-101"}, text, time.Now())

	// Linear is parked, so an ack that waited on the return path never returns.
	release := w.linear.hold()
	defer release()
	acked := make(chan int, 1)
	go func() { acked <- w.deliverSigned(t, body) }()
	select {
	case code := <-acked:
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-timeAfter():
		t.Fatal("webhook ack waited on the Linear return path")
	}
	release()

	home := w.manager.Agent.HomeChannelID
	msg := w.waitForMessage(t, home, text)
	if msg.AuthorAccountID != w.bridgeID {
		t.Errorf("post author = %s, want the bridge %s", msg.AuthorAccountID, w.bridgeID)
	}
	row, err := w.st.LinearAgentSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("LinearAgentSession: %v", err)
	}
	if row.ManagerAccountID != w.manager.ID || row.ChannelID != home || row.LinearIssueID != "issue-101" {
		t.Errorf("association = %+v, want manager %s channel %s issue issue-101", row, w.manager.ID, home)
	}
	if msg.TopicID != row.TopicID {
		t.Errorf("post topic = %s, want the associated topic %s", msg.TopicID, row.TopicID)
	}

	var thoughts, updates []linE2ECall
	for _, c := range w.linear.callsFor(sessionID) {
		switch c.Op {
		case linE2EOpThought:
			thoughts = append(thoughts, c)
		case linE2EOpSession:
			updates = append(updates, c)
		}
	}
	if len(thoughts) != 1 || thoughts[0].Status != http.StatusOK || thoughts[0].Content.Type != "thought" || thoughts[0].Content.Body == "" {
		t.Errorf("thought activities = %+v, want one accepted non-empty thought", thoughts)
	}
	wantURL := sessionLinkFor(linE2EPublicURL, sessionID)
	if len(updates) != 1 || updates[0].Status != http.StatusOK || len(updates[0].ExternalURLs) != 1 || updates[0].ExternalURLs[0].URL != wantURL {
		t.Errorf("session updates = %+v, want one accepted update with external URL %s", updates, wantURL)
	}
}

// scenarioSessionLinkFollowsOwnership: the one stored link points at the routing
// channel while unrouted, then at the owner's home once an authored row exists.
func (w *linE2EWire) scenarioSessionLinkFollowsOwnership(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>RIG-606 prompt context " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-606", Identifier: "RIG-606"}, text, time.Now())
	if code := w.deliverSigned(t, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	w.waitForMessage(t, w.routingCh, text)

	mux := http.NewServeMux()
	mux.Handle(linearSessionLinkPattern, newLinearSessionLinkHandler(w.st, w.resolver.ResolveResponder, linE2EPublicURL, nil))
	click := func() string {
		t.Helper()
		link, err := url.Parse(sessionLinkFor(linE2EPublicURL, sessionID))
		if err != nil {
			t.Fatalf("parse session link: %v", err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, link.RequestURI(), nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		return rec.Header().Get("Location")
	}
	if got, want := click(), deepLinkFor(linE2EPublicURL, string(w.routingCh)); got != want {
		t.Errorf("unrouted Location = %q, want %q", got, want)
	}

	if err := w.st.RecordAuthoredArtifact(t.Context(), store.AuthoredArtifact{
		Provider: store.ForgeProviderLinear, Host: forge.LinearHost, Repo: "RIG",
		Kind: store.ForgeArtifactKindIssue, Number: 606,
		AgentAccountID: w.manager.ID, OwnerUserID: w.adminID,
		CreatedAtUnixMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("RecordAuthoredArtifact(RIG-606): %v", err)
	}
	if got, want := click(), deepLinkFor(linE2EPublicURL, string(w.manager.Agent.HomeChannelID)); got != want {
		t.Errorf("routed Location = %q, want %q", got, want)
	}
}

func (w *linE2EWire) scenarioCreatedUnstamped(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>RIG-202 prompt context " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-202", Identifier: "RIG-202"}, text, time.Now())
	if code := w.deliverSigned(t, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	msg := w.waitForMessage(t, w.routingCh, text)
	row, err := w.st.LinearAgentSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("LinearAgentSession: %v", err)
	}
	if row.ManagerAccountID != w.supervisor.ID || row.ChannelID != w.routingCh || msg.TopicID != row.TopicID {
		t.Errorf("association = %+v (post topic %s), want supervisor %s in routing channel %s", row, msg.TopicID, w.supervisor.ID, w.routingCh)
	}
}

// scenarioDespawnedAuthor: a despawned author's issue lands with its nearest live
// ancestor; with no live ancestor it ends at the supervisor in the routing channel.
func (w *linE2EWire) scenarioDespawnedAuthor(t *testing.T) {
	for _, tc := range []struct {
		identifier  string
		wantManager store.AccountID
		wantChannel store.ChannelID
	}{
		{"RIG-404", w.manager.ID, w.manager.Agent.HomeChannelID},
		{"RIG-405", w.supervisor.ID, w.routingCh},
	} {
		sessionID := uuid.NewString()
		text := "<issue>" + tc.identifier + " prompt context " + sessionID + "</issue>"
		body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-" + tc.identifier, Identifier: tc.identifier}, text, time.Now())
		if code := w.deliverSigned(t, body); code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.identifier, code)
		}
		msg := w.waitForMessage(t, tc.wantChannel, text)
		row, err := w.st.LinearAgentSession(t.Context(), sessionID)
		if err != nil {
			t.Fatalf("%s: LinearAgentSession: %v", tc.identifier, err)
		}
		if row.ManagerAccountID != tc.wantManager || row.ChannelID != tc.wantChannel || msg.TopicID != row.TopicID {
			t.Errorf("%s: association = %+v (post topic %s), want manager %s in channel %s", tc.identifier, row, msg.TopicID, tc.wantManager, tc.wantChannel)
		}
	}
}

func (w *linE2EWire) scenarioReplay(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>RIG-303 prompt context " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-303", Identifier: "RIG-303"}, text, time.Now())
	delivery := uuid.NewString()
	for i := range 2 {
		if code := w.deliver(t, body, linSign(w.secret, body), delivery); code != http.StatusOK {
			t.Fatalf("delivery %d status = %d, want 200", i+1, code)
		}
	}
	w.barrier(t)
	if msgs := w.messagesWithText(t, w.routingCh, text); len(msgs) != 1 {
		t.Errorf("stored messages for the replayed delivery = %d, want 1", len(msgs))
	}
	ops := map[string]int{}
	for _, c := range w.linear.callsFor(sessionID) {
		ops[c.Op]++
	}
	if ops[linE2EOpThought] != 1 || ops[linE2EOpSession] != 1 || len(ops) != 2 {
		t.Errorf("GraphQL calls for the replayed delivery = %v, want one thought and one session update", ops)
	}
}

func (w *linE2EWire) scenarioPrompted(t *testing.T) {
	sessionID := uuid.NewString()
	created := "<issue>RIG-101 prompt context " + sessionID + "</issue>"
	owned := linearagent.Issue{ID: "issue-101", Identifier: "RIG-101"}
	if code := w.deliverSigned(t, linE2ESessionBody(t, "created", sessionID, owned, created, time.Now())); code != http.StatusOK {
		t.Fatalf("created status = %d, want 200", code)
	}
	w.waitForMessage(t, w.manager.Agent.HomeChannelID, created)
	row, err := w.st.LinearAgentSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("LinearAgentSession: %v", err)
	}

	// No issue on the follow-up: a re-resolve would land in the routing channel, so
	// only the recorded association can put it in the owner's topic.
	text := "follow-up prompt " + uuid.NewString()
	if code := w.deliverSigned(t, linE2ESessionBody(t, "prompted", sessionID, linearagent.Issue{}, text, time.Now())); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	msg := w.waitForMessage(t, row.ChannelID, text)
	if msg.TopicID != row.TopicID {
		t.Errorf("prompted post topic = %s, want the created topic %s", msg.TopicID, row.TopicID)
	}
	if got := w.messagesWithText(t, w.routingCh, text); len(got) != 0 {
		t.Errorf("routing channel holds %d copies of the follow-up, want 0", len(got))
	}
}

func (w *linE2EWire) scenario401(t *testing.T) {
	// The barrier mints and drains, so the 401 lands on a cached token with nothing in flight.
	w.barrier(t)
	mintsBefore, served401Before := w.linear.counters()
	w.linear.failNext401()

	sessionID := uuid.NewString()
	text := "<issue>RIG-505 prompt context " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{ID: "issue-505", Identifier: "RIG-505"}, text, time.Now())
	if code := w.deliverSigned(t, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	w.waitForMessage(t, w.routingCh, text)

	mints, served401 := w.linear.counters()
	if mints-mintsBefore != 1 || served401-served401Before != 1 {
		t.Errorf("re-mints = %d, 401s served = %d, want exactly one each", mints-mintsBefore, served401-served401Before)
	}
	calls := w.linear.callsFor(sessionID)
	if len(calls) != 3 {
		t.Fatalf("GraphQL calls = %+v, want thought 401, thought retry 200, session update 200", calls)
	}
	rejected, retried, update := calls[0], calls[1], calls[2]
	if rejected.Op != linE2EOpThought || rejected.Status != http.StatusUnauthorized {
		t.Errorf("first call = %+v, want the thought answered 401", rejected)
	}
	if retried.Op != linE2EOpThought || retried.Status != http.StatusOK || retried.Token == rejected.Token {
		t.Errorf("retry = %+v, want the thought accepted under a fresh token (rejected token %q)", retried, rejected.Token)
	}
	if update.Op != linE2EOpSession || update.Status != http.StatusOK || update.Token != retried.Token {
		t.Errorf("session update = %+v, want accepted under the re-minted token", update)
	}
}

func (w *linE2EWire) scenarioTampered(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>tampered " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{}, text, time.Now())
	sig := []byte(linSign(w.secret, body))
	// Swap in a different hex digit so the MAC compare, not hex decoding, rejects it.
	if sig[len(sig)-1] == '0' {
		sig[len(sig)-1] = '1'
	} else {
		sig[len(sig)-1] = '0'
	}
	if code := w.deliver(t, body, string(sig), uuid.NewString()); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	w.barrier(t)
	w.assertNothingHappened(t, sessionID, text)
}

func (w *linE2EWire) scenarioStale(t *testing.T) {
	sessionID := uuid.NewString()
	text := "<issue>stale " + sessionID + "</issue>"
	body := linE2ESessionBody(t, "created", sessionID, linearagent.Issue{}, text, time.Now().Add(-10*time.Minute))
	if code := w.deliverSigned(t, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	w.barrier(t)
	w.assertNothingHappened(t, sessionID, text)
}

func (w *linE2EWire) scenarioManagerReply(t *testing.T) {
	first := w.openManagerSession(t)
	w.managerReply(t, first, "reply 0")
	w.managerReply(t, first, "reply 1")
	// The tail handles the bus in order, so once a later session's response
	// lands, both replies above have been seen.
	later := w.openManagerSession(t)
	w.managerReply(t, later, "later reply")
	linE2EWaitUntil(t, "the later session's response", func() bool { return w.responses(later.LinearSessionID) == 1 })
	if got := w.responses(first.LinearSessionID); got != 1 {
		t.Errorf("response activities = %d, want 1 (first Manager reply only)", got)
	}
}

// openManagerSession delivers a `created` owned by the manager and returns its association.
func (w *linE2EWire) openManagerSession(t *testing.T) store.LinearAgentSessionRow {
	t.Helper()
	sessionID := uuid.NewString()
	created := "<issue>RIG-101 prompt context " + sessionID + "</issue>"
	owned := linearagent.Issue{ID: "issue-101", Identifier: "RIG-101"}
	if code := w.deliverSigned(t, linE2ESessionBody(t, "created", sessionID, owned, created, time.Now())); code != http.StatusOK {
		t.Fatalf("created status = %d, want 200", code)
	}
	w.waitForMessage(t, w.manager.Agent.HomeChannelID, created)
	row, err := w.st.LinearAgentSession(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("LinearAgentSession: %v", err)
	}
	return row
}

// managerReply posts text as the manager into the session's topic.
func (w *linE2EWire) managerReply(t *testing.T, row store.LinearAgentSessionRow, text string) {
	t.Helper()
	if _, err := w.cm.PostAsAccount(t.Context(), w.manager.ID, &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(row.ChannelID)},
		Topic:     &compassv1.PostMessageRequest_TopicId{TopicId: row.TopicID},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
	}); err != nil {
		t.Fatalf("manager reply %q: %v", text, err)
	}
}

// responses counts the `response` activities Linear received for sessionID.
func (w *linE2EWire) responses(sessionID string) int {
	n := 0
	for _, c := range w.linear.callsFor(sessionID) {
		if c.Content.Type == "response" {
			n++
		}
	}
	return n
}
