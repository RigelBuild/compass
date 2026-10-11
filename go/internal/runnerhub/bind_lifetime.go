//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// LifetimeBinder is the store surface Hub.BindLifetime binds a session's
// transcript base through for a resume or a Reload. *store.Store implements it.
// Wired via SetLifetimeBinder; nil-safe: a hub with none fails the bind
// CodeUnavailable.
type LifetimeBinder interface {
	// AccountTenant resolves the tenant that owns account.
	AccountTenant(ctx context.Context, account store.AccountID) (store.TenantID, error)
	// BindLifetime snapshots sessionID's rebase base. A session that is unknown
	// or not owned by account is ErrNotFound.
	BindLifetime(ctx context.Context, sessionID string, account store.AccountID) (uint64, error)
}

// SetLifetimeBinder wires the store Hub.BindLifetime binds through. Called once
// at server assembly. Wired under mu; read under mu.
func (h *Hub) SetLifetimeBinder(b LifetimeBinder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.binder = b
}

// errLifetimeBinderUnavailable is the fail-closed cause when a hub with no
// LifetimeBinder wired receives a BindLifetime. It maps to CodeUnavailable.
var errLifetimeBinderUnavailable = errors.New("runnerhub: no lifetime binder wired to serve BindLifetime")

var errAccountTenantUnavailable = errors.New("runnerhub: no lifetime binder wired to resolve the account tenant")

// errBindDenied is the one PermissionDenied cause for every refused bind, so a
// foreign container, a foreign session, and an unknown session are identical.
var errBindDenied = errors.New("runnerhub: session is not bindable from this container")

// BindLifetime binds sessionID's transcript base for a resume or a Reload the
// Runner has accepted in containerName. The container must be provisioned on
// runnerID; its account must own the session. The bind runs under that
// account's tenant: the Runner door carries none, so the tenant is read under
// the system role and the write runs tenant-scoped.
func (h *Hub) BindLifetime(ctx context.Context, runnerID, containerName, sessionID string) error {
	h.mu.Lock()
	binder := h.binder
	h.mu.Unlock()
	if binder == nil {
		return connect.NewError(connect.CodeUnavailable, errLifetimeBinderUnavailable)
	}
	if sessionID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("BindLifetime requires a session_id"))
	}
	account, ok := h.AccountForContainer(runnerID, containerName)
	if !ok {
		return connect.NewError(connect.CodePermissionDenied, errBindDenied)
	}
	tctx, err := h.accountTenantContext(ctx, account)
	if err != nil {
		return err
	}
	if _, err := binder.BindLifetime(tctx, sessionID, account); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodePermissionDenied, errBindDenied)
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("binding lifetime: %w", err))
	}
	return nil
}

// accountTenantContext scopes ctx to account's tenant. The Runner door carries
// no tenant, so the lookup runs under the system role and the result does not.
func (h *Hub) accountTenantContext(ctx context.Context, account store.AccountID) (context.Context, error) {
	h.mu.Lock()
	binder := h.binder
	h.mu.Unlock()
	if binder == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errAccountTenantUnavailable)
	}
	tenant, err := binder.AccountTenant(store.WithSystemRole(ctx), account)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving account tenant: %w", err))
	}
	return store.WithTenant(store.WithoutSystemRole(ctx), tenant), nil
}
