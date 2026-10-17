//go:build unix

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

type fakeSessionBlobFetchServer struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	frames []*compassv1internal.FetchSessionBlobsResponse
	err    error
	wait   bool
	req    *compassv1internal.FetchSessionBlobsRequest
}

func (f *fakeSessionBlobFetchServer) FetchSessionBlobs(ctx context.Context, req *compassv1internal.FetchSessionBlobsRequest, stream compassv1internalconnect.RunnerServiceFetchSessionBlobsServerStream) error {
	f.req = req
	for _, frame := range f.frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	if f.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func blobHeaderFrame(sha string, size uint64, absent bool) *compassv1internal.FetchSessionBlobsResponse {
	return &compassv1internal.FetchSessionBlobsResponse{Frame: &compassv1internal.FetchSessionBlobsResponse_Header{
		Header: &compassv1internal.SessionBlobHeader{Sha256: sha, SizeBytes: size, Absent: absent},
	}}
}

func blobChunkFrame(data []byte) *compassv1internal.FetchSessionBlobsResponse {
	return &compassv1internal.FetchSessionBlobsResponse{Frame: &compassv1internal.FetchSessionBlobsResponse_Chunk{Chunk: data}}
}

func TestFetchSessionBlobsReassemblesChunksAndAbsentHeaders(t *testing.T) {
	data := []byte{0, 1, 2, 0xfe, 0xff}
	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	server := &fakeSessionBlobFetchServer{frames: []*compassv1internal.FetchSessionBlobsResponse{
		blobHeaderFrame("absent-hash", 0, true),
		blobHeaderFrame(hexDigest, uint64(len(data)), false),
		blobChunkFrame(data[:2]),
		blobChunkFrame(data[2:]),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchSessionBlobs(t.Context(), "container-1", "session-1", []string{"newest", "older"})
	if err != nil {
		t.Fatalf("FetchSessionBlobs = %v, want success", err)
	}
	if server.req.GetContainerName() != "container-1" || server.req.GetSessionId() != "session-1" || strings.Join(server.req.GetSha256(), ",") != "newest,older" {
		t.Fatalf("FetchSessionBlobs request = %+v, want container/session and newest-first hashes", server.req)
	}
	if _, ok := got.Absent["absent-hash"]; !ok {
		t.Fatalf("Absent = %v, want absent-hash", got.Absent)
	}
	if len(got.Blobs) != 1 || got.Blobs[0].SHA256 != hexDigest || string(got.Blobs[0].Data) != string(data) {
		t.Fatalf("Blobs = %+v, want one blob %s with exact bytes", got.Blobs, hexDigest)
	}
}

func TestFetchSessionBlobsRejectsChunkBeforeHeader(t *testing.T) {
	server := &fakeSessionBlobFetchServer{frames: []*compassv1internal.FetchSessionBlobsResponse{blobChunkFrame([]byte("orphan"))}}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchSessionBlobs(t.Context(), "container-1", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "chunk before") {
		t.Fatalf("FetchSessionBlobs error = %v, want chunk-before-header contract error", err)
	}
}

func TestFetchSessionBlobsDropsDigestMismatch(t *testing.T) {
	data := []byte("bytes do not match this digest")
	server := &fakeSessionBlobFetchServer{frames: []*compassv1internal.FetchSessionBlobsResponse{
		blobHeaderFrame(strings.Repeat("0", 64), uint64(len(data)), false),
		blobChunkFrame(data),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchSessionBlobs(t.Context(), "container-1", "session-1", nil)
	if err != nil {
		t.Fatalf("FetchSessionBlobs = %v, want mismatched blob dropped without failing stream", err)
	}
	if len(got.Blobs) != 0 {
		t.Fatalf("Blobs = %+v, want mismatched digest dropped", got.Blobs)
	}
}

func TestFetchSessionBlobsEnforcesResumeBudget(t *testing.T) {
	server := &fakeSessionBlobFetchServer{frames: []*compassv1internal.FetchSessionBlobsResponse{
		blobHeaderFrame(strings.Repeat("0", 64), uint64(maxResumeBlobBytes+1), false),
		blobChunkFrame([]byte("not buffered")),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchSessionBlobs(t.Context(), "container-1", "session-1", nil)
	if err != nil {
		t.Fatalf("FetchSessionBlobs = %v, want over-budget blob skipped", err)
	}
	if len(got.Blobs) != 0 {
		t.Fatalf("Blobs = %+v, want over-budget blob dropped", got.Blobs)
	}
}

func TestFetchSessionBlobsPropagatesMidStreamError(t *testing.T) {
	server := &fakeSessionBlobFetchServer{
		frames: []*compassv1internal.FetchSessionBlobsResponse{blobHeaderFrame("digest", 1, false), blobChunkFrame([]byte("x"))},
		err:    connect.NewError(connect.CodeUnavailable, "stream broke"),
	}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchSessionBlobs(t.Context(), "container-1", "session-1", nil)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("FetchSessionBlobs error = %v, want Unavailable", err)
	}
}

func TestResumeBlobsMarksOnlyAbsentReferencesFromSuccessfulFetch(t *testing.T) {
	const absentHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	presentData := []byte("present image bytes")
	presentDigest := sha256.Sum256(presentData)
	presentHash := hex.EncodeToString(presentDigest[:])
	body := `{"type":"message","message":{"content":[{"type":"image","data":"blob:sha256:` + absentHash + `"},{"type":"image","data":"blob:sha256:` + presentHash + `"}]}}` + "\n"
	server := &fakeSessionBlobFetchServer{frames: []*compassv1internal.FetchSessionBlobsResponse{
		blobHeaderFrame(absentHash, 0, true),
		blobHeaderFrame(presentHash, uint64(len(presentData)), false),
		blobChunkFrame(presentData),
	}}
	host := &agentHost{link: newLink(newRunnerServiceServer(t, server)), log: discardLoggerRunner()}

	marked, files, err := host.resumeBlobs(t.Context(), "container-1", "session-1", body, false)
	if err != nil {
		t.Fatalf("resumeBlobs = %v, want success", err)
	}
	wantBody := `{"message":{"content":[{"text":"[image unavailable after resume]","type":"text"},{"data":"blob:sha256:` + presentHash + `","type":"image"}]},"type":"message"}` + "\n"
	if marked != wantBody {
		t.Fatalf("resumeBlobs body = %q, want only absent reference replaced: %q", marked, wantBody)
	}
	if strings.Contains(marked, "blob:sha256:"+absentHash) || !strings.Contains(marked, "blob:sha256:"+presentHash) {
		t.Fatalf("resumeBlobs body = %q, want absent ref replaced and present ref preserved", marked)
	}
	if len(files) != 1 || files[0].Name != presentHash || string(files[0].Data) != string(presentData) {
		t.Fatalf("resumeBlobs files = %+v, want present blob %s with exact bytes", files, presentHash)
	}
}

func TestResumeBlobsTreatsOnlyUnimplementedAsAbsent(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body := `{"type":"message","message":{"content":[{"type":"image","data":"blob:sha256:` + hash + `"}]}}` + "\n"
	tests := []struct {
		name       string
		code       connect.Code
		wantMarked bool
	}{
		{name: "unsupported blob store marks images unavailable", code: connect.CodeUnimplemented, wantMarked: true},
		{name: "unbound session preserves image references", code: connect.CodeFailedPrecondition},
		{name: "unverifiable binding preserves image references", code: connect.CodeUnavailable},
		{name: "container authorization miss preserves image references", code: connect.CodePermissionDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeSessionBlobFetchServer{err: connect.NewError(tc.code, "blob fetch unavailable")}
			host := &agentHost{link: newLink(newRunnerServiceServer(t, server)), log: discardLoggerRunner()}

			marked, files, err := host.resumeBlobs(t.Context(), "container-1", "session-1", body, false)
			if err != nil {
				t.Fatalf("resumeBlobs = %v, want graceful fallback", err)
			}
			gotMarked := strings.Contains(marked, imageUnavailableText)
			if gotMarked != tc.wantMarked || len(files) != 0 {
				t.Fatalf("resumeBlobs = (%q, %v), want marked=%t and no files", marked, files, tc.wantMarked)
			}
			if !tc.wantMarked && marked != body {
				t.Fatalf("resumeBlobs body = %q, want original references %q", marked, body)
			}
		})
	}
}

func TestFetchSessionBlobsHonorsContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	server := &fakeSessionBlobFetchServer{wait: true}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchSessionBlobs(ctx, "container-1", "session-1", nil)
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("FetchSessionBlobs error = %v, want deadline exceeded", err)
	}
}

func TestBlobRefsNewestFirstDeduplicates(t *testing.T) {
	const older = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body := `{"content":[{"data":"blob:sha256:` + older + `"},{"data":"blob:sha256:` + newer + `"},{"data":"blob:sha256:` + older + `"}]}`
	want := []string{older, newer}
	got := blobRefsNewestFirst(body)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("blobRefsNewestFirst = %v, want %v", got, want)
	}
}

func TestBlobRefsNewestFirstBoundsToResumeBudget(t *testing.T) {
	refs := make([]string, maxResumeBlobCount+1)
	parts := make([]string, len(refs))
	for i := range refs {
		refs[i] = fmt.Sprintf("%064x", i+1)
		parts[i] = blobRefPrefix + refs[i]
	}
	got := blobRefsNewestFirst(strings.Join(parts, ","))
	if len(got) != maxResumeBlobCount {
		t.Fatalf("blobRefsNewestFirst returned %d references, want %d", len(got), maxResumeBlobCount)
	}
	for i, ref := range got {
		if want := refs[len(refs)-1-i]; ref != want {
			t.Fatalf("reference %d = %q, want newest-first %q", i, ref, want)
		}
	}
}
