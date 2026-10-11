//go:build pgtest && unix

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
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
	waitForQueueState(t, st, issueCoord)
}

func startFakeTrunk(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	const token = "fake-trunk-test-token"
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
		_, _ = w.Write([]byte(`{"pullRequests":[{"prNumber":7,"state":"testing","stateChangedAt":"2026-10-11T12:34:56Z","isCurrentlySubmittedToQueue":true}],"notFound":[]}`))
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
	_, err = st.UpsertIssueForgeFields(ctx, store.IssueForgeFields{
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     forgeTestHost,
		Repo:          "owner/repo",
		Number:        1,
		Title:         "linked issue",
		ForgeState:    "open",
		URL:           "https://github.com/owner/repo/issues/1",
		ForgeAccount:  "octocat",
	})
	if err != nil {
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
	issueCoord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: "owner/repo", Number: 1}
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

func waitForQueueState(t *testing.T, st *store.Store, issueCoord store.ForgeCoord) {
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
			if got.GetMergeQueue().GetState() == "testing" {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("Serve did not persist queue entry: linkedPRs=%d", len(prs))
		case <-poll.C:
		}
	}
}

type queueHTTPTransport struct{ target *httptest.Server }

func (r queueHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copyReq := req.Clone(req.Context())
	copyReq.URL.Scheme = "http"
	copyReq.URL.Host = r.target.Listener.Addr().String()
	return http.DefaultTransport.RoundTrip(copyReq)
}
