//go:build pgtest && unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestServeMergeQueueSweeperPersistsChangedEntryWithinOneTick(t *testing.T) {
	api, requests := startFakeTrunk(t)
	st, issueCoord, dsn := seedMergeQueueServeStore(t)
	config := mergeQueueServeConfig(t, dsn, api)
	serveCtx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(serveCtx, config) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("Serve shutdown: %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("Serve did not stop after cancellation")
		}
	})
	waitListening(t, config.SocketPath)
	select {
	case <-requests:
	case <-time.After(testTimeout):
		t.Fatal("Serve did not call the fake Trunk API within one configured tick")
	}
	waitForQueueState(t, st, issueCoord, "testing")
	select {
	case <-requests:
	case <-time.After(testTimeout):
		t.Fatal("Serve did not call fake Trunk on its recurring tick")
	}
	waitForQueueState(t, st, issueCoord, "failed")
}

func startFakeTrunk(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	const token = "fake-trunk-test-token"
	var calls atomic.Int32
	requests := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/getSubmittedPullRequests" {
			t.Errorf("Trunk request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("X-Api-Token"); got != token {
			t.Errorf("Trunk token header = %q, want fake test token", got)
		}
		var body struct {
			TargetBranch string `json:"targetBranch"`
			PRs          []struct {
				Number uint64 `json:"number"`
			} `json:"prs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Trunk request: %v", err)
		}
		if body.TargetBranch != "main" || len(body.PRs) != 1 || body.PRs[0].Number != 7 {
			t.Errorf("Trunk request branch/PRs = %q/%+v, want main/[7]", body.TargetBranch, body.PRs)
		}
		select {
		case requests <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		state := "testing"
		if calls.Add(1) > 1 {
			state = "failed"
		}
		_, _ = fmt.Fprintf(w, `{"pullRequests":[{"prNumber":7,"state":%q,"stateChangedAt":"2026-10-11T12:34:56Z","isCurrentlySubmittedToQueue":true}],"notFound":[]}`, state)
	}))
	t.Cleanup(api.Close)
	return api, requests
}

func seedMergeQueueServeStore(t *testing.T) (*store.Store, store.ForgeCoord, string) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	t.Cleanup(st.Close)
	issueCoord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: "owner/repo", Number: 1}
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: "owner/repo", Enabled: true}); err != nil {
		t.Fatalf("seed forge subscription: %v", err)
	}
	if _, err := st.UpsertIssueForgeFields(ctx, store.IssueForgeFields{
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     forgeTestHost,
		Repo:          "owner/repo",
		Number:        1,
		Title:         "linked issue",
		ForgeState:    "open",
		URL:           "https://github.com/owner/repo/issues/1",
		ForgeAccount:  "octocat",
	}); err != nil {
		t.Fatalf("seed board issue: %v", err)
	}

	pr := &compassv1.PullRequest{
		Forge:      &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost},
		Repo:       "owner/repo",
		Number:     7,
		ForgeState: "open",
	}
	prJSON, err := protojson.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPullRequest(ctx, store.PullRequestRow{
		Coord:     store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: "owner/repo", Number: 7},
		State:     "open",
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		PR:        prJSON,
	}, []store.ForgeCoord{issueCoord}); err != nil {
		t.Fatalf("seed linked pull request: %v", err)
	}
	if err := st.DeclareServerSecret(ctx, "", store.ServerSecretPrefix+"TRUNK_TOKEN"); err != nil {
		t.Fatalf("declare Trunk token secret: %v", err)
	}
	return st, issueCoord, dsn
}

func mergeQueueServeConfig(t *testing.T, dsn string, api *httptest.Server) ServeConfig {
	t.Helper()
	const token = "fake-trunk-test-token"
	secretFile := filepath.Join(t.TempDir(), "serve.env")
	secretContent := store.MasterKeyName + "=" + dotenvValue(testMasterKeyHex) + "\n" +
		serverSecretName("TRUNK_TOKEN") + "=" + dotenvValue(token) + "\n"
	if err := os.WriteFile(secretFile, []byte(secretContent), 0o600); err != nil {
		t.Fatalf("write fake test secrets: %v", err)
	}
	config := ServeConfig{
		SocketPath:         filepath.Join(t.TempDir(), "compass.sock"),
		Version:            "merge-queue-test",
		DatabaseDSN:        dsn,
		SecretProvider:     "dotenv://" + secretFile,
		MergeQueueInterval: 25 * time.Millisecond,
		Forge: ForgeConfig{
			Host:                 forgeTestHost,
			MergeQueues:          []MergeQueueConfig{{Repo: "owner/repo", Provider: "trunk", Branch: "main"}},
			TrunkTokenSecretName: "TRUNK_TOKEN",
			TrunkHTTP:            &http.Client{Transport: queueHTTPTransport{target: api}},
		},
	}
	provisionNats(t, &config)
	return config
}

func waitForQueueState(t *testing.T, st *store.Store, issueCoord store.ForgeCoord, state string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		rows, err := st.PullRequestsForIssues(ctx, []store.ForgeCoord{issueCoord})
		if err != nil {
			t.Fatalf("read linked pull request: %v", err)
		}
		prs := rows[issueCoord]
		if len(prs) == 1 {
			got := &compassv1.PullRequest{}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(prs[0].PR, got); err != nil {
				t.Fatalf("decode stored linked pull request: %v", err)
			}
			if got.GetMergeQueue().GetState() == state {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("Serve did not persist queue entry state %q: linkedPRs=%d", state, len(prs))
		case <-poll.C:
		}
	}
}

func TestReconcileMergeQueueConfigsDoesNotEnrollUnwatchedRepo(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	err := reconcileMergeQueueConfigs(ctx, st, forgeTestHost, []MergeQueueConfig{{Repo: "owner/repo", Provider: "trunk", Branch: "main"}})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reconcileMergeQueueConfigs error = %v, want missing subscription", err)
	}
	enabled, err := st.IsEnabledForgeRepo(ctx, "owner/repo")
	if err != nil || enabled {
		t.Fatalf("queue-only subscription enabled = %v, %v; want false", enabled, err)
	}
}

func TestMergeQueueProjectionPublishesOnlyChangedQueueEntries(t *testing.T) {
	ctx := context.Background()
	st, issueCoord, _ := seedMergeQueueServeStore(t)
	if err := st.SetForgeRepoMergeQueueConfig(ctx, forgeTestHost, "owner/repo", "trunk", "main"); err != nil {
		t.Fatalf("seed queue config: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	issueBrd := board.NewIssueProjection(bus, st)
	if err := issueBrd.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate board: %v", err)
	}
	subscription, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(subscription.Cancel)
	projection := mergeQueueProjection{brd: issueBrd}
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: "owner/repo", Number: 7}
	entry := &compassv1.MergeQueueEntry{Provider: "trunk", State: "testing"}
	if err := projection.PublishPullRequestMergeQueue(ctx, coord, entry); err != nil {
		t.Fatalf("publish new entry: %v", err)
	}
	select {
	case event := <-subscription.Live:
		if event.Payload.GetIssue().GetNumber() != uint32(issueCoord.Number) {
			t.Fatalf("first event issue = %d, want %d", event.Payload.GetIssue().GetNumber(), issueCoord.Number)
		}
	case <-time.After(testTimeout):
		t.Fatal("changed queue entry did not republish linked issue")
	}
	if err := projection.PublishPullRequestMergeQueue(ctx, coord, entry); err != nil {
		t.Fatalf("publish unchanged entry: %v", err)
	}
	select {
	case event := <-subscription.Live:
		t.Fatalf("unchanged queue entry republished issue: %v", event.Payload)
	default:
	}
}

type queueHTTPTransport struct{ target *httptest.Server }

func (r queueHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copyReq := req.Clone(req.Context())
	copyReq.URL.Scheme = "http"
	copyReq.URL.Host = r.target.Listener.Addr().String()
	return http.DefaultTransport.RoundTrip(copyReq)
}
