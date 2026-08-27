package comms

import (
	"context"
	"errors"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Handle resolution at the service edge (RIG-2751 handle cutover): every
// request-input account field carries a `@handle`, and the server resolves it to
// an account id here — the store layer stays id-typed. A resolver miss flows
// through edgeError as an in-band NOT_FOUND (never a transport teardown).
//
// Two resolution shapes, per §"Where resolution lives":
//   - Member/owner fields (resolveHandles → store.AccountsByHandles) name users
//     as well as agents and ARE visibility-scoped (OQ-6 SCOPED): the viewer is
//     the caller, so an invisible handle misses like an unknown one.
//   - Singular agent fields (resolveAgentHandle → store.AgentByHandle) are
//     owner-namespaced but NOT viewer-scoped — an invisible-but-real agent in the
//     resolution owner's namespace still resolves. The one exception is the
//     roster vantage, which layers its OWN visibility check on top to close the
//     vantage-probe oracle (resolveVisibleAgentHandle).

// resolveHandles resolves a batch of member/owner handles (which legitimately
// name users as well as agents) to their account ids, in the caller's own
// namespace and visibility scope. It is ATOMIC (OQ-2): any unresolved handle
// fails the whole call with store.ErrNotFound naming EVERY unresolved handle in
// its submitted spelling — the caller maps that through edgeError to
// CodeNotFound. Order is preserved: the returned ids follow the submitted order,
// so a caller that also needs per-input identity (memberUpdatesFromWire) can zip
// them back. Empty input is a no-op (nil, nil).
func (c *Comms) resolveHandles(ctx context.Context, caller store.AccountID, handles []string) ([]store.AccountID, error) {
	if len(handles) == 0 {
		return nil, nil
	}
	callerOwner, err := c.store.ResolveOwner(ctx, caller)
	if err != nil {
		return nil, err
	}
	parsed := make([]store.QualifiedHandle, len(handles))
	for i, h := range handles {
		parsed[i] = store.ParseQualifiedHandle(h)
	}
	// viewer = caller (OQ-6 visibility scope); callerOwner = the bare-agent
	// default namespace.
	hits, err := c.store.AccountsByHandles(ctx, caller, callerOwner, parsed)
	if err != nil {
		return nil, err
	}
	out := make([]store.AccountID, len(handles))
	for i, h := range handles {
		out[i] = hits[h]
	}
	return out, nil
}

// resolveAgentHandle resolves a singular agent handle (owner-qualified or bare)
// to its agent account id, in the caller's own owner namespace for a bare
// handle. Owner-namespaced but NOT viewer-scoped (§GetRoster's dual vantage: an
// invisible-but-real agent in the resolution owner's namespace still resolves) —
// the roster vantage adds its own visibility check separately. An unknown,
// wrong-owner, or non-agent handle is store.ErrNotFound naming the submitted
// handle (edgeError → CodeNotFound), the oracle-safe merge every handle-addressed
// agent target holds.
func (c *Comms) resolveAgentHandle(ctx context.Context, caller store.AccountID, handle string) (store.AccountID, error) {
	acc, err := c.resolveAgentAccount(ctx, caller, handle)
	if err != nil {
		return "", err
	}
	return acc.ID, nil
}

// resolveAgentAccount is resolveAgentHandle's account-returning form. It parses
// the owner qualifier, resolves the owner namespace (bare → caller's own owner,
// qualified → the named user in the global index), then the agent under it via
// store.AgentByHandle. The submitted handle — never the resolved id — names the
// NOT_FOUND. NOT viewer-scoped (see resolveAgentHandle).
func (c *Comms) resolveAgentAccount(ctx context.Context, caller store.AccountID, handle string) (store.Account, error) {
	qh := store.ParseQualifiedHandle(handle)
	owner, err := c.agentOwnerNamespace(ctx, caller, qh)
	if err != nil {
		return store.Account{}, notFoundHandle(err, qh.Raw)
	}
	acc, err := c.store.AgentByHandle(ctx, owner, qh.Handle)
	if err != nil {
		// Re-key the message to name the SUBMITTED handle (qh.Raw), never the
		// resolved owner id or the store's bare-handle spelling — oracle-safe.
		return store.Account{}, notFoundHandle(err, qh.Raw)
	}
	return acc, nil
}

// resolveVisibleAgentHandle is resolveAgentAccount plus a caller-visibility
// check — the roster vantage's DEFINED error posture (§GetRoster's dual vantage):
// a real-but-caller-invisible vantage maps to the SAME NOT_FOUND an unknown
// handle gets, closing the NOT_FOUND-vs-empty-success vantage-probe oracle. The
// submitted handle names the miss, never the resolved id.
func (c *Comms) resolveVisibleAgentHandle(ctx context.Context, caller store.AccountID, handle string) (store.AccountID, error) {
	acc, err := c.resolveAgentAccount(ctx, caller, handle)
	if err != nil {
		return "", err
	}
	visible, err := c.store.AccountVisibleTo(ctx, caller, acc.ID)
	if err != nil {
		return "", err
	}
	if !visible {
		return "", notFoundHandle(store.ErrNotFound, store.ParseQualifiedHandle(handle).Raw)
	}
	return acc.ID, nil
}

// agentOwnerNamespace resolves the owner-user id an agent handle is looked up
// under: the caller's own owner for a bare handle, or the named user (global
// index, users are globally unique) for an owner-qualified handle. An owner
// qualifier that resolves to nothing is store.ErrNotFound (indistinguishable
// from an unknown agent once notFoundHandle re-keys it to the submitted handle).
func (c *Comms) agentOwnerNamespace(ctx context.Context, caller store.AccountID, qh store.QualifiedHandle) (store.AccountID, error) {
	if qh.Owner == "" {
		return c.store.ResolveOwner(ctx, caller)
	}
	owner, err := c.store.UserByHandle(ctx, qh.Owner)
	if err != nil {
		return "", err
	}
	return owner.ID, nil
}

// notFoundHandle re-keys a store error to name the submitted handle when it is a
// not-found, preserving the store.ErrNotFound sentinel so edgeError maps it to
// CodeNotFound and matching AgentByHandle's `handle %q` template. A non-not-found
// error (a real query fault) passes through unchanged.
func notFoundHandle(err error, handle string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: handle %q", store.ErrNotFound, handle)
	}
	return err
}
