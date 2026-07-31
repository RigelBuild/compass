//go:build unix

// The RunnerService connect-go handler: the server surface the Runner dials out
// to. It terminates the three frozen RPCs and drives them through the hub —
// Enroll registers the Runner, Sessions binds the command router to the live
// bidi stream and pumps results back, PublishEvents feeds each relayed frame
// into Deliver. Mounted on serve.go beside the CompassService/CommsService
// handlers, behind the Runner-subject bearer interceptor (auth.go).
package runnerhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/secrets"
)

// Handler implements compassv1internalconnect.RunnerServiceHandler over the hub.
// The hub owns the registry, router, and Deliver seam; the resolver is the
// Server-side secret resolve surface FetchSecrets delegates to. The handler is
// the wire-termination shell that drives them.
type Handler struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	hub      *Hub
	resolver secrets.Resolver
}

// NewHandler constructs the RunnerService handler over the hub and the secret
// resolver. resolver may be nil on a server built with no secrets surface, in
// which case FetchSecrets fails closed (CodeUnavailable) rather than panicking —
// the same posture the service takes for a nil hub.
func NewHandler(hub *Hub, resolver secrets.Resolver) *Handler {
	return &Handler{hub: hub, resolver: resolver}
}

// Ensure Handler satisfies the generated interface at compile time.
var _ compassv1internalconnect.RunnerServiceHandler = (*Handler)(nil)

// Enroll registers (or re-attaches) the authenticated Runner. The bearer
// interceptor has already resolved and Kind-gated the token to a SubjectRunner
// subject; an account token never reaches here (it failed the interceptor with
// Unauthenticated — the RunnerService cross-door rejection). The declared
// runner_id is cross-checked against the token subject: a mismatch is a spoofing
// attempt, rejected Unauthenticated.
func (h *Handler) Enroll(ctx context.Context, req *connect.Request[compassv1internal.EnrollRequest]) (*connect.Response[compassv1internal.EnrollResponse], error) {
	subj, ok := runnerSubjectFrom(ctx)
	if !ok {
		// No authenticated subject on the context — the interceptor did not run
		// or rejected. Defense in depth; the interceptor is the primary gate.
		return nil, errUnauthenticated
	}
	if id := req.Msg.GetRunnerId(); id != "" && id != subj.ID {
		// The declared id must match the authenticated subject — a Runner cannot
		// enroll under an identity other than its token's.
		return nil, errUnauthenticated
	}
	reattached := h.hub.enroll(subj.ID, subj)
	return connect.NewResponse(&compassv1internal.EnrollResponse{Reattached: reattached}), nil
}

// Sessions binds the attached Runner's command router to this live bidi stream:
// the router's send pushes commands down the response half, and the loop reads
// results off the request half and completes the correlated calls. The Runner
// opened the stream (it dials out), so the server's response half carries
// commands and the request half carries results — the dial-out inversion. When
// the stream ends (client hang-up or ctx cancel), the router detaches and every
// in-flight command fails, feeding the OQ6 disconnect path.
func (h *Handler) Sessions(ctx context.Context, stream *connect.BidiStream[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse]) error {
	subj, ok := runnerSubjectFrom(ctx)
	if !ok {
		return errUnauthenticated
	}
	router, _, err := h.hub.routerFor(subj.ID)
	if err != nil {
		// A Sessions stream with no enrolled Runner — the Runner must Enroll
		// before opening Sessions.
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}

	router.attach(stream.Send)
	defer router.detach(errStreamClosed)

	for {
		result, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		router.complete(result)
	}
}

// errStreamClosed is the cause in-flight commands fail with when the Runner's
// Sessions stream ends — the router detaches and every pending call observes it.
var errStreamClosed = errors.New("runner sessions stream closed")

// PublishEvents feeds each relayed frame the Runner streams into Deliver — the
// sole entry point Runner events take into the Server. It reads until the Runner
// closes the stream, then acks. A Deliver error (a write-through failure) ends
// the stream with the error so the Runner can retry the relay; a well-formed but
// unknown frame is not an error (Deliver logs+counts it).
func (h *Handler) PublishEvents(ctx context.Context, stream *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	if _, ok := runnerSubjectFrom(ctx); !ok {
		return nil, errUnauthenticated
	}
	for stream.Receive() {
		msg := stream.Msg()
		if err := h.hub.Deliver(ctx, RunnerEvent{
			RunnerSeq:      msg.GetRunnerSeq(),
			SessionID:      msg.GetSessionId(),
			Frame:          msg.GetFrame(),
			IdempotencyKey: msg.GetIdempotencyKey(),
		}); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1internal.PublishEventsResponse{}), nil
}

// RelayCommsCall forwards one agent-initiated comms call into the hub, which
// resolves the relayed session_id to its bound agent account and executes the
// call under that account, fail-closed (relay_comms.go). The bearer interceptor
// has already Kind-gated the caller to a SubjectRunner subject; a defense-in-
// depth check rejects a context with none. The hub returns an unresolved session
// as a Connect CodeNotFound (surfaced to the Runner) and a comms tool failure as
// the in-band CommsCallError variant of the result (never a stream teardown).
func (h *Handler) RelayCommsCall(ctx context.Context, req *connect.Request[compassv1internal.RelayCommsCallRequest]) (*connect.Response[compassv1internal.RelayCommsCallResponse], error) {
	if _, ok := runnerSubjectFrom(ctx); !ok {
		return nil, errUnauthenticated
	}
	resp, err := h.hub.RelayCommsCall(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// CommitConversationFrame durably commits one agent-authored conversation frame
// and returns the commit outcome — the DURABLE counterpart to PublishEvents. The
// hub resolves the relayed session_id to its bound agent account and commits the
// frame under that account, fail-closed (relay_comms.go): an unresolved session
// is a Connect CodeNotFound and a Deliver-only hub with no comms wired is
// CodeUnavailable. The bearer interceptor has already Kind-gated the caller to a
// SubjectRunner subject; the defense-in-depth check rejects a context with none.
//
// The commit is at-most-once keyed on the agent-minted idempotency_key: a fresh
// commit AND an idempotent replay of an already-committed key both return
// committed=true with the ORIGINAL row's message_id (stable across the replay).
// A non-commit is never committed=false with a nil error — it is always a
// Connect status error, because the Runner drives at-least-once purely off the
// Connect code. seq is deferred and shipped as 0.
func (h *Handler) CommitConversationFrame(ctx context.Context, req *connect.Request[compassv1internal.CommitConversationFrameRequest]) (*connect.Response[compassv1internal.CommitConversationFrameResponse], error) {
	if _, ok := runnerSubjectFrom(ctx); !ok {
		return nil, errUnauthenticated
	}
	resp, err := h.hub.CommitConversationFrame(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// FetchSecrets resolves the whole declared secret set for a live session and
// returns it to the Runner. Auth is already at the door (the Runner-subject
// bearer interceptor Kind-gates every RunnerService RPC — an account token is
// Unauthenticated here, the OQ7 cross-door rule); the runnerSubjectFrom check is
// defense in depth, mirroring the other handlers.
//
// Session-binding authz (record §756-762): the requested session_id must be a
// live session bound to this Runner, else CodePermissionDenied. Under inject-all
// + single-Runner, "bound to this Runner" == "is a live session in the hub"
// (there is exactly one Runner); HasLiveSession is that check. A foreign or
// unknown session is rejected CodePermissionDenied ("rejects a foreign session
// with CodePermissionDenied").
//
// NO-LOG posture (record §770-772): the response carries live secret values
// (ResolvedSecret.value/.version are [debug_redact] on the wire). This handler
// logs neither the response nor the resolved set.
func (h *Handler) FetchSecrets(ctx context.Context, req *connect.Request[compassv1internal.FetchSecretsRequest]) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	if _, ok := runnerSubjectFrom(ctx); !ok {
		return nil, errUnauthenticated
	}
	if h.resolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoResolver)
	}
	sessionID := req.Msg.GetSessionId()
	if !h.hub.HasLiveSession(sessionID) {
		// Unbound/unknown session: not this Runner's to resolve secrets for.
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("session %q is not a live session bound to this runner", sessionID))
	}
	resolved, err := h.resolver.Resolve(ctx, "runner fetch")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving secrets: %w", err))
	}
	out := make([]*compassv1internal.ResolvedSecret, 0, len(resolved))
	for _, s := range resolved {
		out = append(out, resolvedSecretToProto(s))
	}
	return connect.NewResponse(&compassv1internal.FetchSecretsResponse{Secrets: out}), nil
}

// errNoResolver is the fail-closed cause when FetchSecrets reaches a handler
// built with no resolver (a server with no secrets surface). It maps to
// CodeUnavailable — the secrets leg is not mounted, never a silent empty set.
var errNoResolver = errors.New("runnerhub: no secret resolver wired to serve FetchSecrets")

// resolvedSecretToProto maps a secrets.ResolvedSecret to the wire ResolvedSecret,
// translating the resolve-surface delivery/kind enums to the public proto enums
// at this edge (the same store↔proto enum discipline the secrets package uses at
// its own edge). The value/version ride through verbatim; they are the no-log
// targets ([debug_redact] on the wire), so this mapping never stringifies them.
func resolvedSecretToProto(s secrets.ResolvedSecret) *compassv1internal.ResolvedSecret {
	return &compassv1internal.ResolvedSecret{
		Name:     s.Name,
		Value:    s.Value,
		Version:  s.Version,
		Delivery: deliveryToProto(s.Delivery),
		Kind:     kindToProto(s.Kind),
		Host:     s.Host,
		Provider: s.Provider,
	}
}

// deliveryToProto maps the resolve-surface delivery enum to the public proto
// enum. The proto reserves 0 for UNSPECIFIED, so File/Env map to 1/2.
func deliveryToProto(d secrets.DeliveryKind) compassv1.SecretDelivery {
	if d == secrets.DeliveryEnv {
		return compassv1.SecretDelivery_SECRET_DELIVERY_ENV
	}
	return compassv1.SecretDelivery_SECRET_DELIVERY_FILE
}

// kindToProto maps the resolve-surface kind enum to the public proto enum. The
// proto reserves 0 for UNSPECIFIED, so Generic/Provider/GH map to 1/2/3.
func kindToProto(k secrets.SecretKind) compassv1.SecretKind {
	switch k {
	case secrets.SecretProvider:
		return compassv1.SecretKind_SECRET_KIND_PROVIDER
	case secrets.SecretGH:
		return compassv1.SecretKind_SECRET_KIND_GH
	default:
		return compassv1.SecretKind_SECRET_KIND_GENERIC
	}
}

// NewMountedHandler builds the RunnerService HTTP handler with the Runner-subject
// bearer interceptor applied, returning the mount path and handler for serve.go
// to Handle on the network mux. resolve is the shared credential resolver
// (auth.ResolveToken with the store closed over); the door authenticates every
// RPC through it for a SubjectRunner token. resolver is the Server-side secret
// resolve surface FetchSecrets delegates to.
func NewMountedHandler(hub *Hub, resolve TokenResolver, resolver secrets.Resolver) (string, http.Handler) {
	auth := &bearerAuth{resolve: resolve}
	return compassv1internalconnect.NewRunnerServiceHandler(
		NewHandler(hub, resolver),
		connect.WithInterceptors(auth.unaryInterceptor(), auth.streamInterceptor()),
	)
}
