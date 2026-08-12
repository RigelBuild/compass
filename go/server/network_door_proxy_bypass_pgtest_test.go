//go:build pgtest && unix

package server

// Auth-door COMPOSITION tests (SEA-1946): proof that the authenticated
// network door derives caller identity ONLY from the Authorization: Bearer
// credential resolved against the store, and NEVER from any proxy-injected
// header. The Compass Bridge will be fronted by `tailscale serve` (a reverse
// proxy), so an attacker who can shape request headers must gain nothing from
// them: no identity, no elevation, no revocation. These tests drive the REAL
// served interceptor chain (BearerInterceptor outer, AdminGate inner) through
// the shared h2c connect harness in network_door_test.go (networkDoorHandler /
// newNetworkStore), so they exercise the same production seam a proxied client
// would hit.
//
// The forged headers below (X-Forwarded-User, X-Forwarded-For, X-Remote-User,
// Forwarded) are the identity-suggestive headers a reverse proxy conventionally
// injects and a downstream might be tempted to trust. The production door reads
// none of them: interceptor.go consults only authorizationHeader, and
// admin_gate.go decides admin only via CallerFrom(ctx). These tests lock that
// in — each names, in its doc comment, the concrete mutation that would redden
// it (the "if the door ever trusted header X" scenario), and since the invariant
// is a NEGATIVE (a header is NOT consulted), the teeth come from a differential:
// the forged headers must produce a response byte-identical to the same request
// without them.
//
// Store-gated and hermetic on the same terms as network_door_test.go: behind
// `//go:build pgtest && unix`, each test captures one isolated-schema DSN via
// newNetworkStore (which calls pgtest.RequireDSN → t.Skip when no runtime).

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/auth"
)

// forgedProxyHeaders is the set of identity-suggestive headers a reverse proxy
// (tailscale serve, nginx, an ALB) conventionally injects. handle is spliced
// into the user-naming headers so a test can forge a SPECIFIC identity (e.g. the
// admin handle) and prove even a targeted forgery grants nothing. The door reads
// none of these; they exist here only so the assertions can prove their inertness.
func forgedProxyHeaders(handle string) http.Header {
	h := http.Header{}
	h.Set("X-Forwarded-User", handle)
	h.Set("X-Forwarded-For", "10.0.0.1")
	h.Set("X-Remote-User", handle)
	h.Set("Forwarded", "for=10.0.0.1;by=proxy;host=bridge.example")
	return h
}

// applyHeaders copies every forged header onto a connect request's header set —
// the same mechanism a reverse proxy uses to inject them ahead of the origin.
func applyHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// TestNetworkDoorProxyHeadersGrantNoIdentity: a proxy-shaped request carrying
// identity-suggestive headers (X-Forwarded-User, X-Forwarded-For, X-Remote-User,
// Forwarded) but NO Authorization bearer is rejected exactly as a plain no-bearer
// request is — same connect.Code AND same message. This proves the forwarded
// headers grant NO identity: they neither authenticate the caller nor change the
// rejection the door emits.
//
// REDDENING MUTATION: if resolveBearer (or any interceptor upstream) ever fell
// back to a forwarded header when Authorization is absent — e.g. reading
// X-Forwarded-User as the caller handle and resolving it — then the forged
// request would authenticate (or fail differently) while the plain request stays
// Unauthenticated, so the two responses would diverge in Code or Message and this
// test reddens. Teeth are the differential: the ONLY difference between the two
// requests below is the presence of the forged headers, so any divergence is
// attributable solely to the door consulting them.
func TestNetworkDoorProxyHeadersGrantNoIdentity(t *testing.T) {
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("proxy-noidentity", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// call drives the open GetServerInfo RPC (authenticatedOpen: the only gate is
	// the bearer resolve, so no admin-gate step confounds the comparison) with no
	// Authorization header, optionally adding the forged proxy headers, and
	// returns the client-visible connect.Error.
	call := func(withForged bool) *connect.Error {
		req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
		if withForged {
			applyHeaders(req.Header(), forgedProxyHeaders("admin"))
		}
		_, err := client.GetServerInfo(t.Context(), req)
		var ce *connect.Error
		if !errors.As(err, &ce) {
			t.Fatalf("expected a connect.Error rejection, got %v", err)
		}
		return ce
	}

	plain := call(false)
	forged := call(true)

	if plain.Code() != connect.CodeUnauthenticated {
		t.Fatalf("plain no-bearer code = %v, want CodeUnauthenticated", plain.Code())
	}
	if forged.Code() != plain.Code() {
		t.Fatalf("forged-header request diverged in code: forged=%v plain=%v — the door consulted a forwarded header", forged.Code(), plain.Code())
	}
	if forged.Message() != plain.Message() {
		t.Fatalf("forged-header request diverged in message — the door consulted a forwarded header:\n  plain:  %q\n  forged: %q", plain.Message(), forged.Message())
	}
}

// TestNetworkDoorProxyHeadersDoNotElevate: forged admin-suggestive headers
// (X-Forwarded-User: <admin handle>, X-Remote-User: admin, …) added to a request
// bearing a valid NON-ADMIN account token still gets PermissionDenied on an
// adminOnly RPC (IssueToken) — the headers do not elevate the caller past the
// admin gate. The same non-admin bearer on GetServerInfo (authenticatedOpen)
// still succeeds, proving the non-admin token itself is valid and that only the
// gate — reading CallerFrom(ctx), the bearer-resolved identity — decides admin,
// never a header.
//
// REDDENING MUTATION: if AdminGate.check ever compared a forwarded header (e.g.
// X-Forwarded-User == admin handle, or X-Remote-User == "admin") against the
// admin instead of, or in addition to, CallerFrom(ctx), the forged non-admin
// request would clear the gate and IssueToken would no longer be
// PermissionDenied — this test reddens. Teeth: the open-RPC success proves the
// non-admin token is genuinely valid, so the PermissionDenied on the adminOnly
// RPC is the gate rejecting the bearer identity, and the forged admin headers
// riding along changed nothing.
func TestNetworkDoorProxyHeadersDoNotElevate(t *testing.T) {
	st, admin, member := newNetworkStore(t)
	memberTok, err := auth.IssueAccountToken(t.Context(), st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("proxy-noelevate", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// The forged headers name the admin handle seeded by newNetworkStore ("admin")
	// — a maximally targeted forgery: if any header path trusted the admin handle,
	// this would be the request that slips through.
	forged := forgedProxyHeaders("admin")

	t.Run("non-admin bearer + forged admin headers still PermissionDenied on adminOnly RPC", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountId: string(admin)})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		applyHeaders(req.Header(), forged)
		if _, err := client.IssueToken(t.Context(), req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin bearer + forged admin headers on IssueToken = %v, want CodePermissionDenied — the headers must not elevate", connect.CodeOf(err))
		}
	})

	t.Run("same non-admin bearer succeeds on open RPC", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		applyHeaders(req.Header(), forged)
		if _, err := client.GetServerInfo(t.Context(), req); err != nil {
			t.Fatalf("non-admin bearer on the open GetServerInfo RPC: %v — the token itself must be valid", err)
		}
	})
}

// TestNetworkDoorProxyHeadersAreInertForAdmin: a valid ADMIN token PLUS the same
// forged headers behaves identically to the admin token alone — the adminOnly
// RPC reaches the handler in both cases. This pins that forwarded headers neither
// GRANT admin (covered by the non-admin test) nor REVOKE it: an admin whose
// request happens to carry a proxy's X-Forwarded-User: someone-else (or a
// contradictory X-Remote-User) is still admin, because the bearer-resolved
// identity is the sole input to the gate.
//
// REDDENING MUTATION: if AdminGate.check ever consulted a forwarded header and
// REQUIRED it to agree with the admin (e.g. rejecting when X-Forwarded-User names
// a non-admin), the admin+forged request would be denied while the admin-only
// request succeeds — the two would diverge and this reddens. Teeth: the admin-only
// baseline succeeds, so the admin+forged case reaching the handler identically
// proves the headers are inert, not merely harmless-when-absent. NON-GOAL: the
// mint-TARGET is pinned by AccountId being an explicit request argument (not
// header-derived), so a forged X-Forwarded-User cannot redirect which account
// the token is minted for — a documented non-goal here, not a coverage gap.
func TestNetworkDoorProxyHeadersAreInertForAdmin(t *testing.T) {
	st, admin, _ := newNetworkStore(t)
	adminTok, err := auth.IssueAccountToken(t.Context(), st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("proxy-inert", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// issue drives the adminOnly IssueToken RPC with the admin bearer, optionally
	// adding forged headers that name a DIFFERENT (non-admin) user — the shape
	// that would trip a gate wrongly requiring header/bearer agreement. The target
	// account id is the admin (an existing account), so a cleared gate reaches the
	// handler and mints a token; the assertion is on success reaching the handler,
	// identical with and without the forged headers.
	issue := func(withForged bool) error {
		req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountId: string(admin)})
		req.Header().Set("Authorization", "Bearer "+adminTok)
		if withForged {
			applyHeaders(req.Header(), forgedProxyHeaders("member"))
		}
		resp, err := client.IssueToken(t.Context(), req)
		if err == nil && resp.Msg.GetToken() == "" {
			t.Fatal("IssueToken cleared the gate but returned an empty token")
		}
		return err
	}

	if err := issue(false); err != nil {
		t.Fatalf("admin token alone on IssueToken: %v — baseline must succeed", err)
	}
	if err := issue(true); err != nil {
		t.Fatalf("admin token + forged contradictory headers on IssueToken: %v — the headers must be inert (neither grant nor revoke)", err)
	}
}

// TestNetworkDoorProxyHeadersGrantNoIdentityStreaming is the SERVER-STREAM half
// of TestNetworkDoorProxyHeadersGrantNoIdentity: a proxy-shaped SubscribeEvents
// stream carrying the identity-suggestive headers but NO Authorization bearer is
// rejected exactly as a plain no-bearer stream is — same connect.Code AND same
// Message. SubscribeEvents is authenticatedOpen and its stream leg runs through
// BearerStreamInterceptor, a SEPARATE interceptor path from the unary door, so
// this pins the negative on the streaming seam too: forwarded headers grant no
// identity on the stream.
//
// REDDENING MUTATION: if the stream interceptor (BearerStreamInterceptor /
// resolveBearer on the stream leg) ever fell back to a forwarded header when
// Authorization is absent — reading X-Forwarded-User as the caller handle — the
// forged stream would authenticate (or fail differently) while the plain stream
// stays Unauthenticated, so the two would diverge in Code or Message and this
// reddens. The shared resolveBearer makes this transitive with the unary case,
// but the stream leg gets its own assertion because its interceptor wrapper is
// distinct. Teeth: the ONLY difference between the two streams is the forged
// headers, so any divergence is attributable solely to the door consulting them.
func TestNetworkDoorProxyHeadersGrantNoIdentityStreaming(t *testing.T) {
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("proxy-noidentity-stream", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// open drives the authenticatedOpen SubscribeEvents server-stream with no
	// Authorization header, optionally adding the forged proxy headers, and
	// returns the client-visible connect.Error. A rejected stream surfaces its
	// terminal error on the first Receive (the interceptor returns before the
	// handler runs), matching the pattern in TestNetworkDoorStreamingBearerAuth.
	open := func(withForged bool) *connect.Error {
		req := connect.NewRequest(&compassv1.SubscribeEventsRequest{SinceSeq: 0})
		if withForged {
			applyHeaders(req.Header(), forgedProxyHeaders("admin"))
		}
		ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
		defer cancel()
		stream, err := client.SubscribeEvents(ctx, req)
		if err != nil {
			var ce *connect.Error
			if !errors.As(err, &ce) {
				t.Fatalf("expected a connect.Error rejection on open, got %v", err)
			}
			return ce
		}
		defer func() { _ = stream.Close() }()
		if recvStreamOrTimeout(t, stream) {
			t.Fatalf("a rejected stream delivered an event: %+v", stream.Msg())
		}
		var ce *connect.Error
		if !errors.As(stream.Err(), &ce) {
			t.Fatalf("expected a connect.Error rejection, got %v", stream.Err())
		}
		return ce
	}

	plain := open(false)
	forged := open(true)

	if plain.Code() != connect.CodeUnauthenticated {
		t.Fatalf("plain no-bearer stream code = %v, want CodeUnauthenticated", plain.Code())
	}
	if forged.Code() != plain.Code() {
		t.Fatalf("forged-header stream diverged in code: forged=%v plain=%v — the stream door consulted a forwarded header", forged.Code(), plain.Code())
	}
	if forged.Message() != plain.Message() {
		t.Fatalf("forged-header stream diverged in message — the stream door consulted a forwarded header:\n  plain:  %q\n  forged: %q", plain.Message(), forged.Message())
	}
}

// TestNetworkDoorSmuggledSecondAuthorizationDoesNotElevate: a request bearing a
// valid NON-ADMIN bearer as its first Authorization header, then a SECOND
// smuggled admin credential appended as another Authorization header (the shape
// a proxy appending a header produces), sent to the adminOnly IssueToken RPC,
// MUST NOT succeed — the door never elevates on the smuggled admin value. The
// assertion is robust to whichever Authorization net/http surfaces (first, last,
// or comma-joined): the security invariant is only that a second admin header
// never elevates, so the RPC returns a non-nil error coded PermissionDenied
// (the admin value lost / gate rejects the non-admin) OR Unauthenticated (a
// joined value resolves to no token) — never nil/OK.
//
// REDDENING MUTATION: a proxy appending a second Authorization is proxy-shaped
// traffic; if the door ever preferred a LATER (or admin-looking) Authorization
// value over the first, the adminOnly IssueToken would clear the gate and
// succeed — this test reddens. Teeth: the first header is a genuinely valid
// non-admin bearer, so a success could only come from the door honoring the
// smuggled admin header.
func TestNetworkDoorSmuggledSecondAuthorizationDoesNotElevate(t *testing.T) {
	st, admin, member := newNetworkStore(t)
	memberTok, err := auth.IssueAccountToken(t.Context(), st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}
	adminTok, err := auth.IssueAccountToken(t.Context(), st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("proxy-smuggle", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountId: string(admin)})
	req.Header().Set("Authorization", "Bearer "+memberTok)
	req.Header().Add("Authorization", "Bearer "+adminTok)

	_, err = client.IssueToken(t.Context(), req)
	if err == nil {
		t.Fatal("smuggled second admin Authorization elevated the caller: IssueToken succeeded — the door honored the appended admin header")
	}
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied && code != connect.CodeUnauthenticated {
		t.Fatalf("smuggled second Authorization on IssueToken = %v, want CodePermissionDenied or CodeUnauthenticated", code)
	}
}
