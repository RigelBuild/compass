//go:build unix

package gateway

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

type fakeSessionBlobRelay struct {
	resp  *compassv1internal.RelaySessionBlobResponse
	err   error
	req   *compassv1internal.RelaySessionBlobRequest
	calls int
}

func (f *fakeSessionBlobRelay) RelaySessionBlob(_ context.Context, req *compassv1internal.RelaySessionBlobRequest) (*compassv1internal.RelaySessionBlobResponse, error) {
	f.calls++
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestPutSessionBlobForwardsUnderBoundSession(t *testing.T) {
	sessions := boundSessions()
	relay := &fakeSessionBlobRelay{resp: &compassv1internal.RelaySessionBlobResponse{}}
	g := NewGateway(t.Context(), "cont-A", Deps{Sessions: sessions, SessionBlobs: relay})
	want := &compassv1internal.PutSessionBlobRequest{Sha256: "abc", Data: []byte{0, 1, 255}}

	resp, err := g.PutSessionBlob(t.Context(), want)
	if err != nil {
		t.Fatalf("PutSessionBlob = %v, want success", err)
	}
	if resp == nil {
		t.Fatal("PutSessionBlob response is nil")
	}
	if relay.calls != 1 {
		t.Fatalf("RelaySessionBlob calls = %d, want 1", relay.calls)
	}
	if relay.req.GetSessionId() != "sess-1" {
		t.Fatalf("forwarded session_id = %q, want sess-1", relay.req.GetSessionId())
	}
	if relay.req.GetBlob() != want {
		t.Fatalf("forwarded blob = %v, want the original request", relay.req.GetBlob())
	}
}

func TestPutSessionBlobUnboundSessionFailsClosed(t *testing.T) {
	relay := &fakeSessionBlobRelay{}
	g := NewGateway(t.Context(), "cont-A", Deps{Sessions: &staticSessions{}, SessionBlobs: relay})

	_, err := g.PutSessionBlob(t.Context(), &compassv1internal.PutSessionBlobRequest{Data: []byte("blob")})
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("PutSessionBlob error code = %v, want FailedPrecondition", got)
	}
	if relay.calls != 0 {
		t.Fatalf("RelaySessionBlob calls = %d, want 0", relay.calls)
	}
}

func TestPutSessionBlobRejectsOversizeBeforeRelay(t *testing.T) {
	relay := &fakeSessionBlobRelay{}
	g := NewGateway(t.Context(), "cont-A", Deps{Sessions: boundSessions(), SessionBlobs: relay})

	_, err := g.PutSessionBlob(t.Context(), &compassv1internal.PutSessionBlobRequest{
		Data: make([]byte, 15<<20+1),
	})
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("PutSessionBlob error code = %v, want InvalidArgument", got)
	}
	if relay.calls != 0 {
		t.Fatalf("RelaySessionBlob calls = %d, want 0", relay.calls)
	}
}

func TestPutSessionBlobPassesServerErrorUnchanged(t *testing.T) {
	wantErr := connect.NewError(connect.CodeFailedPrecondition, "no object store")
	relay := &fakeSessionBlobRelay{err: wantErr}
	g := NewGateway(t.Context(), "cont-A", Deps{Sessions: boundSessions(), SessionBlobs: relay})

	_, err := g.PutSessionBlob(t.Context(), &compassv1internal.PutSessionBlobRequest{Data: []byte("blob")})
	if err != wantErr {
		t.Fatalf("PutSessionBlob error = %v, want original Server error %v", err, wantErr)
	}
}

func TestPutSessionBlobAcceptsBinaryProtoOverAgentGateway(t *testing.T) {
	relay := &fakeSessionBlobRelay{resp: &compassv1internal.RelaySessionBlobResponse{}}
	g := NewGateway(t.Context(), "cont-A", Deps{Sessions: boundSessions(), SessionBlobs: relay})
	client := newAgentGatewayServer(t, g)
	want := &compassv1internal.PutSessionBlobRequest{Sha256: "digest", Data: []byte{0, 1, 2, 255}}

	if _, err := client.PutSessionBlob(t.Context(), want); err != nil {
		t.Fatalf("binary PutSessionBlob = %v, want success", err)
	}
	if relay.calls != 1 || relay.req.GetBlob().GetSha256() != want.GetSha256() || string(relay.req.GetBlob().GetData()) != string(want.GetData()) {
		t.Fatalf("binary request forwarded as calls=%d blob=%v, want original binary bytes", relay.calls, relay.req.GetBlob())
	}
}
