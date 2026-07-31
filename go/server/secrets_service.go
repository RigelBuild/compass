//go:build unix

// The SecretsService implementation — the account-facing side of the compass.v1
// secrets contract (SEA-1327 T7). It sits beside CompassService/CommsService on
// the same account doors (socket + dev + network), behind the bearer + admin-gate
// interceptors that classify the three procedures authenticatedOpen (admin_gate.go):
// the door admits any authenticated account and THIS handler enforces the fine
// authz the frozen record pins.
//
//   - SetSecret / DeleteSecret are USER-ONLY (record §911-927): an agent-token
//     caller is CodePermissionDenied, the same fail-closed posture as the
//     admin-gated IssueToken. This is the load-bearing regression the record
//     calls out (§927).
//   - ListSecrets is open to user AND agent (record §904-910): the Setup agent
//     drives it. It returns value-free SecretStatus — never a value, and never
//     resolves values to compute is_set.
//
// A successful Set/Delete bumps the secrets version (a fire-and-forget hub push
// to live sessions, secretsSignaler) so live containers re-fetch (T6 cleanup).
// A secret value is never logged here (it is [debug_redact] on the wire; the
// server side keeps the same posture).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/secrets"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// secretsSignaler is the narrow hub surface the secrets service needs to emit a
// SecretsVersion signal after a registry write. *runnerhub.Hub satisfies it; the
// service depends on the interface (not the concrete hub) so a test can drive the
// bump path with a recorder, mirroring the hub's own narrow-sink pattern.
type secretsSignaler interface {
	// SignalSecretsVersion mints a fresh opaque monotonic set-change token and
	// pushes a SecretsVersion signal to every live session (best-effort).
	SignalSecretsVersion() error
}

// secretsService implements compassv1connect.SecretsServiceHandler over the
// store's names registry and the secret resolver. The store owns the value-free
// declaration rows; the resolver is the provider write/resolve path; the signaler
// notifies live sessions on a write. signaler may be nil on a server with no
// Runner door (socket-only), in which case a write completes without a signal —
// there is no live session to notify.
type secretsService struct {
	compassv1connect.UnimplementedSecretsServiceHandler
	store    *store.Store
	resolver secrets.Resolver
	signaler secretsSignaler
}

// newSecretsService constructs the SecretsService handler.
func newSecretsService(st *store.Store, resolver secrets.Resolver, signaler secretsSignaler) *secretsService {
	return &secretsService{store: st, resolver: resolver, signaler: signaler}
}

// Ensure secretsService satisfies the generated interface at compile time.
var _ compassv1connect.SecretsServiceHandler = (*secretsService)(nil)

// errNoResolver is the fail-closed cause when a secrets RPC reaches a service
// built with no resolver — a server-wiring bug, never a silent success.
var errNoResolver = errors.New("no secret resolver configured on this server")

// SetSecret declares a secret's registry row and writes its value via the
// resolver. USER-ONLY (record §911-927): an agent-token caller is
// CodePermissionDenied. `value` is never logged.
//
// Flow (declare-then-set): DeclareSecret records the value-free row, then
// resolver.Set writes the value to the provider. A re-Set of an already-declared
// name is a value REWRITE, not a failure: DeclareSecret returns ErrConflict for a
// duplicate name (store/secrets.go), so on ErrConflict this proceeds to
// resolver.Set anyway — the name already exists and we are rewriting its value.
// (Conflict policy per the driver brief; not invented here.) An empty value is
// rejected up front, before any row is declared. A failed FRESH write rolls back
// the declaration so no orphan survives (an orphaned declaration is required=true
// in the resolve manifest and would poison EVERY live session's FetchSecrets). On
// a successful write the secrets version is bumped so live sessions re-fetch.
func (s *secretsService) SetSecret(
	ctx context.Context,
	req *connect.Request[compassv1.SetSecretRequest],
) (*connect.Response[compassv1.SetSecretResponse], error) {
	callerID, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.resolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoResolver)
	}
	msg := req.Msg
	delivery, kind, err := secretRoutingFromProto(msg.GetDelivery(), msg.GetKind())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if strings.TrimSpace(msg.GetValue()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("secret value is empty"))
	}

	declErr := s.store.DeclareSecret(ctx, callerID, msg.GetName(), delivery, kind, msg.GetProvider(), msg.GetHost())
	switch {
	case declErr == nil:
		// Fresh declaration.
	case errors.Is(declErr, store.ErrConflict):
		// Already declared: a re-Set rewrites the value (proceed to resolver.Set).
	case errors.Is(declErr, store.ErrInvalidArgument):
		return nil, connect.NewError(connect.CodeInvalidArgument, declErr)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("declaring secret: %w", declErr))
	}

	if err := s.resolver.Set(ctx, msg.GetName(), msg.GetValue()); err != nil {
		// The name was validated by DeclareSecret and the value was screened
		// non-empty above, so a Set failure here is a provider/exec fault
		// (CLI unreachable, non-zero exit) — retryable and operator-side, never
		// the caller's argument, so CodeUnavailable, not CodeInvalidArgument.
		// Roll back a FRESH declaration: an orphaned declaration is required=true
		// in the resolve manifest and would fail EVERY live session's FetchSecrets
		// (a global denial from one failed write). Leave an ErrConflict (re-Set)
		// row alone — it legitimately pre-existed this call. The Set error wraps
		// name/cli/stderr, never the value, so logging it server-side is safe; the
		// client-facing error is value-free.
		if declErr == nil {
			if delErr := s.store.DeleteSecretDeclaration(ctx, callerID, msg.GetName()); delErr != nil {
				slog.ErrorContext(ctx, "rolling back secret declaration after failed write", "err", delErr)
			}
		}
		slog.ErrorContext(ctx, "writing secret value", "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("writing secret value failed"))
	}
	s.bumpSecretsVersion(ctx)
	return connect.NewResponse(&compassv1.SetSecretResponse{}), nil
}

// ListSecrets returns the value-free status of every declared secret. Open to
// USER AND AGENT (record §904-910): the Setup agent drives it, so no kind
// restriction. It reads the declaration registry and maps each row to a
// SecretStatus — NEVER a value (SecretStatus has no value field), and never
// resolves values to compute is_set.
//
// is_set: a declared row means SetSecret declared AND wrote it (the flow is
// declare-then-set), so is_set=true for every declared row. This does NOT resolve
// values (that would fetch every secret just to list them); the resolver's Resolve
// is never called here. (Fallback per the driver brief: the resolver exposes no
// value-free status path in the Go SDK; declaration implies a written value in the
// current SetSecret flow.)
func (s *secretsService) ListSecrets(
	ctx context.Context,
	req *connect.Request[compassv1.ListSecretsRequest],
) (*connect.Response[compassv1.ListSecretsResponse], error) {
	if _, err := s.requireCaller(ctx); err != nil {
		return nil, err
	}
	decls, err := s.store.DeclaredSecrets(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("listing declared secrets: %w", err))
	}
	out := make([]*compassv1.SecretStatus, 0, len(decls))
	for _, d := range decls {
		out = append(out, &compassv1.SecretStatus{
			Name: d.Name,
			// A declared row was written by SetSecret (declare-then-set), so it is
			// set. Computed WITHOUT resolving the value (no provider read to list).
			IsSet:    true,
			Delivery: deliveryToProto(d.Delivery),
			Kind:     kindToProto(d.Kind),
			Provider: d.Provider,
			Host:     d.Host,
		})
	}
	return connect.NewResponse(&compassv1.ListSecretsResponse{Secrets: out}), nil
}

// DeleteSecret removes a secret's provider value and registry row, then bumps the
// secrets version. USER-ONLY (record §915-918): an agent-token caller is
// CodePermissionDenied. A name that was never declared is CodeNotFound.
func (s *secretsService) DeleteSecret(
	ctx context.Context,
	req *connect.Request[compassv1.DeleteSecretRequest],
) (*connect.Response[compassv1.DeleteSecretResponse], error) {
	callerID, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.resolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoResolver)
	}
	name := req.Msg.GetName()
	// Ordering note: resolver.Delete is a validate-only no-op today (no upstream
	// provider hard-delete verb), so calling it before DeleteSecretDeclaration is
	// inert. When a real provider delete lands, this MUST flip to
	// declaration-first: the declaration is the source of truth Resolve reads, and
	// deleting the provider value before the row would leave a required=true
	// declaration pointing at a missing value — the same global resolve-poison as a
	// failed Set, in reverse.
	if err := s.resolver.Delete(ctx, name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("deleting secret value: %w", err))
	}
	if err := s.store.DeleteSecretDeclaration(ctx, callerID, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("secret %q", name))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("deleting secret declaration: %w", err))
	}
	s.bumpSecretsVersion(ctx)
	return connect.NewResponse(&compassv1.DeleteSecretResponse{}), nil
}

// requireCaller returns the authenticated caller id, or CodeUnauthenticated when
// none is in context (a door-wiring bug: an interceptor must attach one on every
// door — fail closed, mirroring SubscribeAgentSession).
func (s *secretsService) requireCaller(ctx context.Context) (store.AccountID, error) {
	callerID, ok := auth.CallerFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	return callerID, nil
}

// requireUser returns the authenticated caller id only when it is a USER account;
// an agent account is CodePermissionDenied (the user-only write gate, record
// §919-927 — the same fail-closed posture as admin-gated IssueToken). No caller is
// CodeUnauthenticated (fail closed). The account kind is read from the store (an
// agent account has the Agent subtype set; a user does not — IsAgent).
func (s *secretsService) requireUser(ctx context.Context) (store.AccountID, error) {
	callerID, err := s.requireCaller(ctx)
	if err != nil {
		return "", err
	}
	acct, err := s.store.GetAccount(ctx, callerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A caller the bearer door authenticated but whose account row is gone:
			// fail closed rather than admit a write under an unresolvable identity.
			return "", connect.NewError(connect.CodePermissionDenied, fmt.Errorf("caller account %q not found", callerID))
		}
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller account: %w", err))
	}
	if acct.IsAgent() {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("secret writes are user-only"))
	}
	return callerID, nil
}

// bumpSecretsVersion emits the SecretsVersion signal after a successful write.
// Best-effort: a nil signaler (socket-only server, no Runner door) is a no-op, and
// a push failure is logged, never surfaced to the caller — the write already
// committed, and the Runner re-fetches on reconnect regardless (the signal is only
// a "re-fetch now" prod, not the source of truth).
func (s *secretsService) bumpSecretsVersion(ctx context.Context) {
	if s.signaler == nil {
		return
	}
	if err := s.signaler.SignalSecretsVersion(); err != nil {
		slog.WarnContext(ctx, "emitting secrets version signal", "err", err)
	}
}

// secretRoutingFromProto maps the public proto delivery/kind enums to the store
// enums, rejecting an UNSPECIFIED value (the proto 0) as an invalid argument — a
// SetSecret must name a concrete delivery and kind. The store's DeclareSecret
// re-validates the kind↔provider/host routing invariant, so this only translates.
func secretRoutingFromProto(d compassv1.SecretDelivery, k compassv1.SecretKind) (store.SecretDelivery, store.SecretKind, error) {
	var delivery store.SecretDelivery
	switch d {
	case compassv1.SecretDelivery_SECRET_DELIVERY_FILE:
		delivery = store.SecretDeliveryFile
	case compassv1.SecretDelivery_SECRET_DELIVERY_ENV:
		delivery = store.SecretDeliveryEnv
	default:
		return 0, 0, errors.New("secret delivery is unspecified")
	}
	var kind store.SecretKind
	switch k {
	case compassv1.SecretKind_SECRET_KIND_GENERIC:
		kind = store.SecretKindGeneric
	case compassv1.SecretKind_SECRET_KIND_PROVIDER:
		kind = store.SecretKindProvider
	case compassv1.SecretKind_SECRET_KIND_GH:
		kind = store.SecretKindGH
	default:
		return 0, 0, errors.New("secret kind is unspecified")
	}
	return delivery, kind, nil
}

// deliveryToProto maps the store delivery enum to the public proto enum (the
// proto reserves 0 for UNSPECIFIED, so File/Env are 1/2).
func deliveryToProto(d store.SecretDelivery) compassv1.SecretDelivery {
	if d == store.SecretDeliveryEnv {
		return compassv1.SecretDelivery_SECRET_DELIVERY_ENV
	}
	return compassv1.SecretDelivery_SECRET_DELIVERY_FILE
}

// kindToProto maps the store kind enum to the public proto enum (the proto
// reserves 0 for UNSPECIFIED, so Generic/Provider/GH are 1/2/3).
func kindToProto(k store.SecretKind) compassv1.SecretKind {
	switch k {
	case store.SecretKindProvider:
		return compassv1.SecretKind_SECRET_KIND_PROVIDER
	case store.SecretKindGH:
		return compassv1.SecretKind_SECRET_KIND_GH
	default:
		return compassv1.SecretKind_SECRET_KIND_GENERIC
	}
}
