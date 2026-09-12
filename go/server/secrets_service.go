//go:build unix

// The SecretsService implementation — the account-facing side of the compass.v1
// secrets contract (RIG-1327 T7). It sits beside CompassService/CommsService on
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

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
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
	// serverResolver is the SECOND resolver instance, reading the separate
	// server_secrets registry. It is a DISTINCT instance from resolver (whose
	// manifest is the user registry the container-delivery path reads), so a
	// server-secret write can never land in the container manifest. nil on a
	// server with no server-secret wiring, in which case the admin RPCs
	// fail closed with errNoServerResolver rather than silently writing to the
	// user registry.
	serverResolver secrets.Resolver
	signaler       secretsSignaler
}

// newSecretsService constructs the SecretsService handler.
func newSecretsService(st *store.Store, resolver, serverResolver secrets.Resolver, signaler secretsSignaler) *secretsService {
	return &secretsService{store: st, resolver: resolver, serverResolver: serverResolver, signaler: signaler}
}

// Ensure secretsService satisfies the generated interface at compile time.
var _ compassv1connect.SecretsServiceHandler = (*secretsService)(nil)

// errNoResolver is the fail-closed cause when a secrets RPC reaches a service
// built with no resolver — a server-wiring bug, never a silent success.
var errNoResolver = errors.New("no secret resolver configured on this server")

// errNoServerResolver is the fail-closed cause when a server-secret RPC reaches
// a service built with no SERVER resolver. It must never fall back to the user
// resolver: that would write the value into the registry the container-delivery
// path reads, inverting the whole point of the separate store.
var errNoServerResolver = errors.New("no server secret resolver configured on this server")

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
// a successful write the secrets version is bumped so live sessions re-fetch. The
// declare/set/rollback trio is not atomic and assumes no concurrent same-name
// writer (the single-Runner MVP: SetSecret is user-driven CLI); a future
// multi-writer path must re-examine the race.
func (s *secretsService) SetSecret(
	ctx context.Context,
	req *connect.Request[compassv1.SetSecretRequest],
) (*connect.Response[compassv1.SetSecretResponse], error) {
	callerID, role, err := s.requireUser(ctx)
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

	scopeKind, scopeID, err := resolveSecretScope(msg.GetScope(), callerID, role)
	if err != nil {
		return nil, err
	}

	declErr := s.store.DeclareSecret(ctx, callerID, msg.GetName(), scopeKind, scopeID, delivery, kind, msg.GetProvider(), msg.GetHost())
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

	// The audit reason carries the authenticated caller, so the provider's log
	// distinguishes which operator wrote a secret rather than recording every
	// write anonymously. The RPC is the only path that reaches this write, so
	// the prefix also records that provenance. callerID is resolved from the
	// bearer token (auth.CallerFrom -> the token subject), never a request
	// field, and every account id is server-minted hex (store/ids.go), so it
	// cannot carry a quote or newline into the reason; the CLI additionally
	// JSON-escapes the reason into its audit record, so a forged log entry is
	// doubly unreachable.
	reason := fmt.Sprintf("compass: operator secret write via SetSecret RPC (caller %s)", callerID)
	if err := s.resolver.Set(ctx, msg.GetName(), msg.GetValue(), reason); err != nil {
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
			// Roll back at the RESOLVED coordinate (D9): a fresh declaration lands
			// wherever resolveSecretScope placed it, so the rollback must target the
			// same coordinate, not a hardcoded tenant one.
			if delErr := s.store.DeleteSecretDeclaration(ctx, callerID, msg.GetName(), scopeKind, scopeID); delErr != nil {
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
	callerID, role, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.resolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoResolver)
	}
	name := req.Msg.GetName()
	scopeKind, scopeID, err := resolveSecretScope(req.Msg.GetScope(), callerID, role)
	if err != nil {
		return nil, err
	}
	// Ordering note: resolver.Delete is a validate-only no-op today, so calling
	// it before DeleteSecretDeclaration is inert. The provider verb it would
	// shell EXISTS at this pin (`secretspec delete`, 0.18+); wiring it is a
	// deferral (RIG-3436), not an upstream gap. When it lands, this MUST flip to
	// declaration-first: the declaration is the source of truth Resolve reads, and
	// deleting the provider value before the row would leave a required=true
	// declaration pointing at a missing value — the same global resolve-poison as a
	// failed Set, in reverse.
	if err := s.resolver.Delete(ctx, name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("deleting secret value: %w", err))
	}
	if err := s.store.DeleteSecretDeclaration(ctx, callerID, name, scopeKind, scopeID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("secret %q", name))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("deleting secret declaration: %w", err))
	}
	s.bumpSecretsVersion(ctx)
	return connect.NewResponse(&compassv1.DeleteSecretResponse{}), nil
}

// SetServerSecret declares a SERVER secret in the separate server_secrets
// registry and writes its value through the SERVER resolver. Admin-only at the
// door (classifyProcedure), which IS the authorization — a server secret is
// deployment-owned, so there is no per-account check to fall back on.
//
// Mirrors SetSecret's declare-then-Set flow and its rollback discipline: an
// empty value is rejected before any row is declared, an already-declared name
// is a legitimate re-Set, and a failed FRESH write rolls the declaration back so
// no orphan survives. The reserved-prefix requirement lives at the store door
// (DeclareServerSecret), so an unprefixed name is rejected there and surfaces
// as CodeInvalidArgument.
func (s *secretsService) SetServerSecret(
	ctx context.Context,
	req *connect.Request[compassv1.SetServerSecretRequest],
) (*connect.Response[compassv1.SetServerSecretResponse], error) {
	callerID, err := s.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if s.serverResolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoServerResolver)
	}
	msg := req.Msg
	name := msg.GetName()
	if name == store.MasterKeyName {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("%s is provisioned and rotated by the server, never set through this RPC", store.MasterKeyName))
	}
	if strings.TrimSpace(msg.GetValue()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("secret value is empty"))
	}

	declErr := s.store.DeclareServerSecret(ctx, callerID, name)
	switch {
	case declErr == nil:
		// Fresh declaration.
	case errors.Is(declErr, store.ErrConflict):
		// Already declared: a re-Set rewrites the value.
	case errors.Is(declErr, store.ErrInvalidArgument):
		return nil, connect.NewError(connect.CodeInvalidArgument, declErr)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("declaring server secret: %w", declErr))
	}

	// The audit reason names the SERVER-secret RPC specifically, so the
	// provider's log distinguishes a deployment-secret write from an operator's
	// user-secret write, and carries the authenticated admin caller. Same
	// injection reasoning as the user path: callerID comes from the bearer
	// token, never a request field, and is server-minted hex.
	reason := fmt.Sprintf("compass: server secret write via SetServerSecret RPC (caller %s)", callerID)
	if err := s.serverResolver.Set(ctx, name, msg.GetValue(), reason); err != nil {
		// Name validated at the store door and value screened non-empty above, so
		// a Set failure is a provider/exec fault — retryable and operator-side,
		// never the caller's argument. Roll back only a FRESH declaration; an
		// ErrConflict row legitimately pre-existed this call.
		if declErr == nil {
			if delErr := s.store.DeleteServerSecretDeclaration(ctx, name); delErr != nil {
				slog.ErrorContext(ctx, "rolling back server secret declaration after failed write", "err", delErr)
			}
		}
		slog.ErrorContext(ctx, "writing server secret value", "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("writing server secret value failed"))
	}
	// Deliberately NO bumpSecretsVersion: a server secret never reaches a live
	// session's FetchSecrets, so there is nothing for a session to re-fetch.
	return connect.NewResponse(&compassv1.SetServerSecretResponse{}), nil
}

// DeleteServerSecret removes a SERVER secret's registry row and its provider
// value. Admin-only at the door. The reserved master-key name is refused for the
// same reason SetServerSecret refuses it.
func (s *secretsService) DeleteServerSecret(
	ctx context.Context,
	req *connect.Request[compassv1.DeleteServerSecretRequest],
) (*connect.Response[compassv1.DeleteServerSecretResponse], error) {
	if _, err := s.requireCaller(ctx); err != nil {
		return nil, err
	}
	if s.serverResolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoServerResolver)
	}
	name := req.Msg.GetName()
	if name == store.MasterKeyName {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("%s is provisioned and rotated by the server, never deleted through this RPC", store.MasterKeyName))
	}

	// Provider value first, then the declaration: the same order as DeleteSecret,
	// so a failure leaves the declaration intact rather than orphaning a value
	// with no row naming it.
	if err := s.serverResolver.Delete(ctx, name); err != nil {
		slog.ErrorContext(ctx, "deleting server secret value", "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("deleting server secret value failed"))
	}
	if err := s.store.DeleteServerSecretDeclaration(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("deleting server secret declaration: %w", err))
	}
	return connect.NewResponse(&compassv1.DeleteServerSecretResponse{}), nil
}

// ListServerSecrets returns every declared SERVER secret's name with its
// set/unset state — names only, NEVER a value. Admin-only at the door
// (classifyProcedure), the same gate as its Set/Delete siblings and for the
// same reason: the rows are deployment-owned, so there is no per-account
// authorization to fall back on.
//
// is_set is a PROVIDER PROBE here, unlike ListSecrets which hardcodes true.
// That asymmetry is structural, not an inconsistency: on the user path declare
// and set are ONE operation (declare-then-set in SetSecret), so a declared row
// implies a written value. Server-secret names are instead SELF-DECLARED at
// every boot (declareServerSecretNames) while the operator populates the values
// separately, so declared-but-unset is a routine state — and telling the two
// apart is the entire purpose of this verb. The probe reads the value-free
// SecretSpec report (serverResolver.Statuses), so no value is ever resolved to
// answer it.
//
// A provider fault is CodeInternal, deliberately NOT an all-unset list: the
// caller must be able to tell a broken provider from an unprovisioned one,
// since the remedy for each is the opposite of the other.
func (s *secretsService) ListServerSecrets(
	ctx context.Context,
	_ *connect.Request[compassv1.ListServerSecretsRequest],
) (*connect.Response[compassv1.ListServerSecretsResponse], error) {
	if _, err := s.requireCaller(ctx); err != nil {
		return nil, err
	}
	if s.serverResolver == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoServerResolver)
	}
	decls, err := s.store.DeclaredServerSecrets(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("listing declared server secrets: %w", err))
	}
	if len(decls) == 0 {
		return connect.NewResponse(&compassv1.ListServerSecretsResponse{}), nil
	}
	// The audit reason names this RPC specifically, matching the Set path's
	// form, so the provider's log distinguishes a status probe from a write.
	statuses, err := s.serverResolver.Statuses(ctx, "compass: server secret status probe via ListServerSecrets RPC")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("probing server secret values: %w", err))
	}
	isSet := make(map[string]bool, len(statuses))
	for _, st := range statuses {
		isSet[st.Name] = st.IsSet
	}
	// The REGISTRY drives the output, not the probe: the declared set is what
	// this verb enumerates, and a declared name the probe did not report is
	// unset rather than omitted.
	out := make([]*compassv1.ServerSecretStatus, 0, len(decls))
	for _, d := range decls {
		// Name goes out EXACTLY as stored, carrying its reserved prefix. The
		// prefix strip is the CLI's, so the wire form stays unambiguous.
		out = append(out, &compassv1.ServerSecretStatus{Name: d.Name, IsSet: isSet[d.Name]})
	}
	return connect.NewResponse(&compassv1.ListServerSecretsResponse{ServerSecrets: out}), nil
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

// requireUser returns the authenticated caller id AND role only when the caller
// is a USER account; an agent account is CodePermissionDenied (the user-only
// write gate, record §919-927 — the same fail-closed posture as admin-gated
// IssueToken). No caller is CodeUnauthenticated (fail closed). The account kind
// is read from the store (an agent account has the Agent subtype set; a user
// does not — IsAgent). The role is returned so a handler can gate a tenant-scope
// write (D9) without a second GetAccount; a caller with no user payload (the
// reserved system account) is the least-privilege member, so it cannot pass the
// admin gate.
func (s *secretsService) requireUser(ctx context.Context) (store.AccountID, store.UserRole, error) {
	callerID, err := s.requireCaller(ctx)
	if err != nil {
		return "", 0, err
	}
	acct, err := s.store.GetAccount(ctx, callerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A caller the bearer door authenticated but whose account row is gone:
			// fail closed rather than admit a write under an unresolvable identity.
			return "", 0, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("caller account %q not found", callerID))
		}
		return "", 0, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller account: %w", err))
	}
	if acct.IsAgent() {
		return "", 0, connect.NewError(connect.CodePermissionDenied, errors.New("secret writes are user-only"))
	}
	var role store.UserRole
	if acct.User != nil {
		role = acct.User.Role
	}
	return callerID, role, nil
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

// resolveSecretScope maps a wire SecretScope + caller identity to the store
// coordinate (scope_kind, scope_id) a user-secret write targets, implementing
// D9's matrix. UNSPECIFIED and USER both land at the caller's private user
// coordinate — the unspecified default is USER, so an omitted field never
// silently writes a tenant-wide value. TENANT lands at the shared coordinate
// (store 0, "") and requires UserRoleAdmin, else CodePermissionDenied. An
// unknown/out-of-range wire value is CodeInvalidArgument (fail closed). The wire
// numbers deliberately differ from the store's (tenant is 2 here, 0 there), so
// this MAPS explicitly rather than casting — a cast would turn an omitted field
// into a tenant write, the exact bug the default exists to prevent.
func resolveSecretScope(scope compassv1.SecretScope, callerID store.AccountID, role store.UserRole) (int16, string, error) {
	switch scope {
	case compassv1.SecretScope_SECRET_SCOPE_UNSPECIFIED, compassv1.SecretScope_SECRET_SCOPE_USER:
		return store.SecretScopeUser, string(callerID), nil
	case compassv1.SecretScope_SECRET_SCOPE_TENANT:
		if role != store.UserRoleAdmin {
			return 0, "", connect.NewError(connect.CodePermissionDenied, errors.New("tenant-scoped secret writes require an admin"))
		}
		return store.SecretScopeTenant, "", nil
	default:
		return 0, "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown secret scope %d", scope))
	}
}
