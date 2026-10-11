package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestTrunkEntriesMapsStatesAndRequest(t *testing.T) {
	changedAt := "2026-10-11T12:34:56.123Z"
	var request struct {
		Repo struct {
			Host  string `json:"host"`
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repo"`
		TargetBranch string `json:"targetBranch"`
		PRs          []struct {
			Number uint64 `json:"number"`
		} `json:"prs"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/getSubmittedPullRequests" {
			t.Errorf("request = %s %s, want POST /v1/getSubmittedPullRequests", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Token"); got != "fixture-token" {
			t.Errorf("X-Api-Token = %q, want fixture-token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pullRequests":[` +
			`{"prNumber":1,"state":"not_ready","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":2,"state":"pending","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":3,"state":"testing","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":4,"state":"tests_passed","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":5,"state":"failed","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":6,"state":"pending_failure","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":true},` +
			`{"prNumber":7,"state":"cancelled","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":false},` +
			`{"prNumber":8,"state":"merged","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":false},` +
			`{"prNumber":9,"state":"pending","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":false}` +
			`],"notFound":[{"number":10}]}`))
	}))
	defer server.Close()

	client := &http.Client{Transport: roundTripTo(server)}
	queue := NewTrunk(TrunkConfig{
		Token: func(context.Context) ([]byte, error) { return []byte("fixture-token"), nil },
		Host:  "github.com",
		HTTP:  client,
	})
	got, err := queue.entriesAt(context.Background(), server.URL, "owner/repo", "main", []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	wantRepo := struct {
		Host  string `json:"host"`
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}{Host: "github.com", Owner: "owner", Name: "repo"}
	if request.Repo != wantRepo || request.TargetBranch != "main" || len(request.PRs) != 10 {
		t.Fatalf("request repo/branch/PRs = %+v/%q/%d, want %+v/main/10", request.Repo, request.TargetBranch, len(request.PRs), wantRepo)
	}
	wantAt, err := time.Parse(time.RFC3339Nano, changedAt)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]QueueEntry{
		1: {Number: 1, State: QueueQueued, ChangedAt: wantAt},
		2: {Number: 2, State: QueueQueued, ChangedAt: wantAt},
		3: {Number: 3, State: QueueTesting, ChangedAt: wantAt},
		4: {Number: 4, State: QueueTesting, ChangedAt: wantAt},
		5: {Number: 5, State: QueueFailed, ChangedAt: wantAt},
		6: {Number: 6, State: QueueFailed, ChangedAt: wantAt},
		7: {Number: 7, State: QueueRemoved, ChangedAt: wantAt},
		8: {Number: 8, State: QueueMerged, ChangedAt: wantAt},
		9: {Number: 9, State: QueueRemoved, ChangedAt: wantAt},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Entries = %#v, want %#v", got, want)
	}
	if _, ok := got[10]; ok {
		t.Fatalf("notFound PR #10 unexpectedly returned: %#v", got[10])
	}
}

func TestTrunkEntriesMapsCancelledSubmissionToRemoved(t *testing.T) {
	changedAt := "2026-10-11T12:34:56Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pullRequests":[{"prNumber":7,"state":"pending","stateChangedAt":"` + changedAt + `","isCurrentlySubmittedToQueue":false}],"notFound":[]}`))
	}))
	defer server.Close()
	queue := NewTrunk(TrunkConfig{
		Token: func(context.Context) ([]byte, error) { return []byte("fixture-token"), nil },
		HTTP:  &http.Client{Transport: roundTripTo(server)},
	})
	got, err := queue.entriesAt(context.Background(), server.URL, "owner/repo", "main", []uint64{7})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if entry, ok := got[7]; !ok || entry.State != QueueRemoved {
		t.Fatalf("Entries = %#v, want PR #7 removed", got)
	}
}

func TestTrunkEntriesRejectsMoreThanFiftyPRs(t *testing.T) {
	queue := NewTrunk(TrunkConfig{Token: func(context.Context) ([]byte, error) { return []byte("fixture-token"), nil }})
	numbers := make([]uint64, 51)
	if _, err := queue.Entries(context.Background(), "owner/repo", "main", numbers); err == nil {
		t.Fatal("Entries with 51 PRs returned no error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func roundTripTo(server *httptest.Server) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(r)
	})
}
