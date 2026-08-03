package store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a minimal in-memory S3-compatible HTTP handler: enough of the
// PutObject/GetObject verbs for the round-trip test, with NO live S3 and NO
// container. It keys stored bodies by the request path (bucket/key), which is
// exactly what the client writes under path-style addressing.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// minio-go signs streaming uploads with aws-chunked framing; the
		// STREAMING-*-PAYLOAD sha256 marker (or an aws-chunked encoding) flags
		// it. De-chunk so the stored object is the verbatim payload.
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING") ||
			strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
			body = dechunkAWS(body)
		}
		f.objects[r.URL.Path] = body
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// minio-go parses these response headers on GetObject; a stat with an
		// unparseable Last-Modified fails the read, so set valid values.
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) // test double: a write error to the recorder is not actionable
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// dechunkAWS strips AWS streaming-signature chunk framing
// ("<hexlen>;chunk-signature=<sig>\r\n<data>\r\n" repeated, terminated by a
// zero-length chunk) back to the raw payload.
func dechunkAWS(b []byte) []byte {
	var out []byte
	for {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			break
		}
		header := strings.TrimRight(string(b[:nl]), "\r")
		b = b[nl+1:]
		sizeHex := header
		if before, _, found := strings.Cut(header, ";"); found {
			sizeHex = before
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil || size == 0 {
			break
		}
		if int64(len(b)) < size {
			break
		}
		out = append(out, b[:size]...)
		b = b[size:]
		b = bytes.TrimPrefix(b, []byte("\r\n"))
	}
	return out
}

// TestS3ObjectStoreSatisfiesInterface is the compile-time + runtime assertion
// that S3ObjectStore is a store.ObjectStore.
func TestS3ObjectStoreSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var _ ObjectStore = (*S3ObjectStore)(nil)
}

// TestS3ObjectStoreRoundTrip exercises PutSegment then GetSegment against an
// in-memory fake S3 over httptest — the body written under a key comes back
// verbatim.
func TestS3ObjectStoreRoundTrip(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server url: %v", err)
	}

	os, err := NewS3ObjectStore(S3Config{
		Endpoint:  u.Host,
		Bucket:    "compass-archive",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "us-east-1",
		UseTLS:    false,
	})
	if err != nil {
		t.Fatalf("constructing s3 object store: %v", err)
	}

	ctx := context.Background()
	const key = "sessions/sess-1/1-3.jsonl"
	body := []byte(`{"a":1}` + "\n" + `{"b":2}` + "\n" + `{"c":3}`)

	if err := os.PutSegment(ctx, key, body); err != nil {
		t.Fatalf("PutSegment: %v", err)
	}

	got, err := os.GetSegment(ctx, key)
	if err != nil {
		t.Fatalf("GetSegment: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, body)
	}
}

// TestS3ObjectStoreGetMissing surfaces a not-found key as an error rather than
// an empty success.
func TestS3ObjectStoreGetMissing(t *testing.T) {
	t.Parallel()

	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server url: %v", err)
	}

	os, err := NewS3ObjectStore(S3Config{
		Endpoint: u.Host,
		Bucket:   "compass-archive",
		Region:   "us-east-1",
	})
	if err != nil {
		t.Fatalf("constructing s3 object store: %v", err)
	}

	_, err = os.GetSegment(context.Background(), "sessions/absent/9-9.jsonl")
	if err == nil {
		t.Fatal("GetSegment on a missing key: want error, got nil")
	}
}

// TestNewS3ObjectStoreRequiresEndpointAndBucket asserts the constructor rejects
// an absent-config case (the caller in serve.go skips construction entirely when
// config is absent; this guards a partial config reaching the constructor).
func TestNewS3ObjectStoreRequiresEndpointAndBucket(t *testing.T) {
	t.Parallel()

	cases := map[string]S3Config{
		"no endpoint": {Bucket: "b"},
		"no bucket":   {Endpoint: "e"},
		"neither":     {},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewS3ObjectStore(cfg); err == nil {
				t.Fatalf("NewS3ObjectStore(%s): want error, got nil", name)
			}
		})
	}
}

// TestS3ConfigPresent covers the present() gate serve.go uses to decide whether
// to wire the archive tier at all.
func TestS3ConfigPresent(t *testing.T) {
	t.Parallel()
	if (S3Config{}).present() {
		t.Fatal("empty config: want not present")
	}
	if !(S3Config{Endpoint: "e", Bucket: "b"}).present() {
		t.Fatal("endpoint+bucket: want present")
	}
}
