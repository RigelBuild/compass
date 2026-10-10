//go:build unix

package runnerhub

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/agentmsg"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type fakeSessionBlobStore struct {
	tenant    store.TenantID
	rows      []store.SessionBlobRow
	data      map[string][]byte
	err       error
	putErr    error
	sessionID string
	account   store.AccountID
}

func (f *fakeSessionBlobStore) AccountTenant(context.Context, store.AccountID) (store.TenantID, error) {
	return f.tenant, nil
}

func (f *fakeSessionBlobStore) PutSessionBlob(context.Context, string, string, []byte) error {
	return f.putErr
}

func (f *fakeSessionBlobStore) ResumeSessionBlobs(_ context.Context, sessionID string, account store.AccountID, _ []string) ([]store.SessionBlobRow, error) {
	if (f.sessionID != "" && f.sessionID != sessionID) || (f.account != "" && f.account != account) {
		return nil, nil
	}
	return f.rows, nil
}

func (f *fakeSessionBlobStore) ReadSessionBlob(_ context.Context, _, hash string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.data[hash]
	if !ok {
		return nil, errors.New("missing test blob")
	}
	return data, nil
}

func blobHash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func bindBlobTestSession(h *Hub, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionAccounts[sessionID] = sessionBinding{account: testAgentAccount, runnerID: testRunnerID}
}

func TestHubRelaySessionBlobMapsGuards(t *testing.T) {
	data := []byte("image bytes")
	hash := blobHash(data)
	tests := []struct {
		name   string
		store  *fakeSessionBlobStore
		sess   string
		runner string
		sha    string
		data   []byte
		blob   bool
		nilReq bool
		want   connect.Code
	}{
		{name: "store not wired", sess: "sess", sha: hash, data: data, blob: true, want: connect.CodeUnavailable},
		{name: "nil request", store: &fakeSessionBlobStore{}, want: connect.CodeInvalidArgument, nilReq: true},
		{name: "foreign session", store: &fakeSessionBlobStore{}, sess: "missing", sha: hash, data: data, blob: true, want: connect.CodeNotFound},
		{name: "foreign runner", store: &fakeSessionBlobStore{}, sess: "sess", runner: "runner-2", sha: hash, data: data, blob: true, want: connect.CodeNotFound},
		{name: "missing blob", store: &fakeSessionBlobStore{}, sess: "sess", want: connect.CodeInvalidArgument},
		{name: "empty data", store: &fakeSessionBlobStore{}, sess: "sess", sha: hash, blob: true, want: connect.CodeInvalidArgument},
		{name: "bad hash", store: &fakeSessionBlobStore{}, sess: "sess", sha: "bad", data: data, blob: true, want: connect.CodeInvalidArgument},
		{name: "oversize", store: &fakeSessionBlobStore{}, sess: "sess", sha: hash, data: make([]byte, agentmsg.MaxSessionBlobBytes+1), blob: true, want: connect.CodeInvalidArgument},
		{name: "store rejects digest", store: &fakeSessionBlobStore{putErr: store.ErrInvalidArgument}, sess: "sess", sha: hash, data: data, blob: true, want: connect.CodeInvalidArgument},
		{name: "object store absent", store: &fakeSessionBlobStore{putErr: store.ErrFailedPrecondition}, sess: "sess", sha: hash, data: data, blob: true, want: connect.CodeFailedPrecondition},
		{name: "store failure", store: &fakeSessionBlobStore{putErr: errors.New("write failed")}, sess: "sess", sha: hash, data: data, blob: true, want: connect.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHubOnly()
			if tc.store != nil {
				h.SetSessionBlobStore(tc.store)
			}
			runnerID := tc.runner
			if runnerID == "" {
				runnerID = testRunnerID
			}
			if tc.name != "store not wired" && tc.name != "nil request" && tc.name != "foreign session" {
				bindBlobTestSession(h, "sess")
			}
			var req *compassv1internal.RelaySessionBlobRequest
			if !tc.nilReq {
				req = &compassv1internal.RelaySessionBlobRequest{SessionId: tc.sess}
				if tc.blob {
					req.Blob = &compassv1internal.PutSessionBlobRequest{Sha256: tc.sha, Data: tc.data}
				}
			}
			_, err := h.RelaySessionBlob(t.Context(), runnerID, req)
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("RelaySessionBlob code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestHubFetchSessionBlobsReadsUnboundResumeSession(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	data := []byte("resumed image")
	hash := blobHash(data)
	h.SetSessionBlobStore(&fakeSessionBlobStore{
		tenant:    "tenant-a",
		rows:      []store.SessionBlobRow{{SHA256: hash, SizeBytes: int64(len(data))}},
		data:      map[string][]byte{hash: data},
		sessionID: "session-unbound",
		account:   testAgentAccount,
	})
	var frames []*compassv1internal.FetchSessionBlobsResponse
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-unbound", Sha256: []string{hash},
	}, func(frame *compassv1internal.FetchSessionBlobsResponse) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSessionBlobs for unbound resume session = %v, want success", err)
	}
	if len(frames) != 2 || frames[0].GetHeader() == nil || frames[0].GetHeader().GetAbsent() || string(frames[1].GetChunk()) != string(data) {
		t.Fatalf("FetchSessionBlobs frames = %v, want image header and exact bytes", frames)
	}
}

func TestHubFetchSessionBlobsSkipsOrdinaryReadFailure(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	data := []byte("indexed image")
	hash := blobHash(data)
	h.SetSessionBlobStore(&fakeSessionBlobStore{
		tenant: "tenant-a",
		rows:   []store.SessionBlobRow{{SHA256: hash, SizeBytes: int64(len(data))}},
		err:    errors.New("read failed"),
	})
	var frames []*compassv1internal.FetchSessionBlobsResponse
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-a", Sha256: []string{hash},
	}, func(frame *compassv1internal.FetchSessionBlobsResponse) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil || len(frames) != 0 {
		t.Fatalf("FetchSessionBlobs on read failure = (%v, %d frames), want nil and no frames", err, len(frames))
	}
}

func TestHubFetchSessionBlobsReturnsFailedPreconditionOnRead(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	data := []byte("indexed image")
	hash := blobHash(data)
	h.SetSessionBlobStore(&fakeSessionBlobStore{
		tenant: "tenant-a",
		rows:   []store.SessionBlobRow{{SHA256: hash, SizeBytes: int64(len(data))}},
		err:    store.ErrFailedPrecondition,
	})
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-unbound", Sha256: []string{hash},
	}, func(*compassv1internal.FetchSessionBlobsResponse) error { return nil })
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("FetchSessionBlobs read ErrFailedPrecondition = %v, want FailedPrecondition", err)
	}
}

func TestHubFetchSessionBlobsBoundsRequestedHashes(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	h.SetSessionBlobStore(&fakeSessionBlobStore{tenant: "tenant-a"})
	hashes := make([]string, maxResumeBlobs+1)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%064x", i+1)
	}
	var got []string
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-unbound", Sha256: hashes,
	}, func(frame *compassv1internal.FetchSessionBlobsResponse) error {
		if header := frame.GetHeader(); header != nil {
			got = append(got, header.GetSha256())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSessionBlobs: %v", err)
	}
	if len(got) != maxResumeBlobs {
		t.Fatalf("absent headers = %d, want at most %d", len(got), maxResumeBlobs)
	}
	for i, hash := range got {
		if hash != hashes[i] {
			t.Fatalf("absent header %d = %q, want request-order hash %q", i, hash, hashes[i])
		}
	}
}

func TestHubFetchSessionBlobsBudgetStopsBeforeBlob(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	first := []byte("first")
	firstHash := blobHash(first)
	second := []byte("second")
	secondHash := blobHash(second)
	h.SetSessionBlobStore(&fakeSessionBlobStore{
		tenant: "tenant-a",
		rows: []store.SessionBlobRow{
			{SHA256: firstHash, SizeBytes: int64(len(first))},
			{SHA256: secondHash, SizeBytes: maxResumeBlobBytes - int64(len(first)) + 1},
		},
		data:      map[string][]byte{firstHash: first, secondHash: second},
		sessionID: "session-a",
		account:   testAgentAccount,
	})
	var gotHashes []string
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-a", Sha256: []string{firstHash, secondHash},
	}, func(frame *compassv1internal.FetchSessionBlobsResponse) error {
		if header := frame.GetHeader(); header != nil {
			gotHashes = append(gotHashes, header.GetSha256())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSessionBlobs: %v", err)
	}
	if len(gotHashes) != 1 || gotHashes[0] != firstHash {
		t.Fatalf("blob headers = %v, want only first hash before crossing budget", gotHashes)
	}
}

func TestHubFetchSessionBlobsRejectsForeignContainer(t *testing.T) {
	h := newHubOnly()
	h.SetSessionBlobStore(&fakeSessionBlobStore{tenant: "tenant-a"})
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "foreign", SessionId: "session-a", Sha256: []string{blobHash([]byte("x"))},
	}, func(*compassv1internal.FetchSessionBlobsResponse) error { return nil })
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("FetchSessionBlobs for foreign container = %v, want PermissionDenied", err)
	}
}

func TestHubFetchSessionBlobsDoesNotLeakForeignSession(t *testing.T) {
	h := newHubOnly()
	h.bindContainer("container-a", testAgentAccount, testRunnerID)
	foreignData := []byte("foreign image")
	foreignHash := blobHash(foreignData)
	h.SetSessionBlobStore(&fakeSessionBlobStore{
		tenant:    "tenant-a",
		rows:      []store.SessionBlobRow{{SHA256: foreignHash, SizeBytes: int64(len(foreignData))}},
		data:      map[string][]byte{foreignHash: foreignData},
		sessionID: "session-b",
		account:   "acct-other",
	})
	var frames []*compassv1internal.FetchSessionBlobsResponse
	err := h.FetchSessionBlobs(t.Context(), testRunnerID, &compassv1internal.FetchSessionBlobsRequest{
		ContainerName: "container-a", SessionId: "session-b", Sha256: []string{foreignHash},
	}, func(frame *compassv1internal.FetchSessionBlobsResponse) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchSessionBlobs for foreign session = %v, want all-absent response", err)
	}
	if len(frames) != 1 || frames[0].GetHeader() == nil || !frames[0].GetHeader().GetAbsent() {
		t.Fatalf("foreign session frames = %v, want absent header only", frames)
	}
}
