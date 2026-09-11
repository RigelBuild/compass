package comms

// The two resolve.go contracts that need NO database, kept at the cheapest tier
// that can catch them (RIG-3536 / T8):
//
//   - notFoundHandle's discrimination (resolve.go:131-139): it re-keys ONLY a
//     store.ErrNotFound, and a real query fault passes through UNMANGLED. A pure
//     function over an error value — a pgtest case here would buy nothing.
//   - resolveHandles' empty-input no-op (resolve.go:33, :35-37). Driven against a
//     Comms with a NIL store, which is a strictly stronger assertion than a
//     pgtest one: the call can only return without panicking if the
//     len(handles)==0 short-circuit fires BEFORE ResolveOwner/AccountsByHandles
//     are reached.
//
// context.Background() is the test root (test-root ctx exemption).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestNotFoundHandleRekeysNotFoundToSubmittedHandle: a store not-found is
// re-keyed to name the SUBMITTED handle, keeping the store.ErrNotFound sentinel
// (so edgeError still maps it to CodeNotFound) and matching AgentByHandle's
// `handle %q` template. The point of the re-key is that the store's own spelling
// — here a resolved account id, the oracle leak DL-269 closed — is GONE from the
// message.
func TestNotFoundHandleRekeysNotFoundToSubmittedHandle(t *testing.T) {
	// What the store would have said: a not-found naming the RESOLVED id.
	storeErr := fmt.Errorf("%w: agent id %q", store.ErrNotFound, "acct_0123456789")

	got := notFoundHandle(storeErr, "owner/victim")

	if !errors.Is(got, store.ErrNotFound) {
		t.Fatalf("notFoundHandle(not-found) = %v, which is not store.ErrNotFound; edgeError would map it to Internal, not CodeNotFound", got)
	}
	want := fmt.Sprintf("%v: handle %q", store.ErrNotFound, "owner/victim")
	if got.Error() != want {
		t.Fatalf("notFoundHandle message = %q, want %q (the `handle %%q` template naming the submitted handle)", got.Error(), want)
	}
}

// TestNotFoundHandleEmptySubmittedHandleStillRekeys pins that the re-key is
// unconditional on a not-found: even an empty submitted handle produces the
// template rather than falling through and leaking the store's own message.
func TestNotFoundHandleEmptySubmittedHandleStillRekeys(t *testing.T) {
	storeErr := fmt.Errorf("%w: agent id %q", store.ErrNotFound, "acct_secret")

	got := notFoundHandle(storeErr, "")

	if got.Error() != fmt.Sprintf("%v: handle %q", store.ErrNotFound, "") {
		t.Fatalf("notFoundHandle(not-found, \"\") = %q, want the `handle \"\"` template; the store's own message must never survive a not-found", got.Error())
	}
}

// TestNotFoundHandleNonNotFoundPassesThroughUnmangled: a NON-not-found store
// error is a real fault, not a resolution miss — it passes through UNCHANGED
// (resolve.go:138-139). Asserted on identity, not just text: re-wrapping would
// break an errors.Is on the original sentinel and hide a query fault behind a
// CodeNotFound.
func TestNotFoundHandleNonNotFoundPassesThroughUnmangled(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"query fault", fmt.Errorf("store: resolve global handle: %w", errors.New("connection reset by peer"))},
		{"invalid argument sentinel", fmt.Errorf("%w: handle is required", store.ErrInvalidArgument)},
		{"conflict sentinel", fmt.Errorf("%w: handle already taken", store.ErrConflict)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := notFoundHandle(tc.err, "owner/victim")

			// Identity, not errors.Is: the contract is that the SAME error value
			// passes through, so a re-wrap that preserved the sentinel would
			// still be a violation.
			if got != tc.err {
				t.Fatalf("notFoundHandle(%s) = %#v, want the SAME error value back (%#v) — a non-not-found must pass through unmangled", tc.name, got, tc.err)
			}
			if got.Error() != tc.err.Error() {
				t.Fatalf("notFoundHandle(%s) message = %q, want %q unchanged", tc.name, got.Error(), tc.err.Error())
			}
			if errors.Is(got, store.ErrNotFound) {
				t.Fatalf("notFoundHandle(%s) = %v now satisfies errors.Is(store.ErrNotFound); a real query fault would surface as CodeNotFound instead of Internal", tc.name, got)
			}
			if got.Error() == fmt.Sprintf("%v: handle %q", store.ErrNotFound, "owner/victim") {
				t.Fatalf("notFoundHandle(%s) was re-keyed to the not-found template; only a store.ErrNotFound may be re-keyed", tc.name)
			}
		})
	}
}

// TestResolveHandlesEmptyInputIsNoOp: empty input is a no-op returning
// (nil, nil) — never a spurious miss, and never a store round-trip. Driven with
// a NIL store: reaching ResolveOwner or AccountsByHandles would nil-panic, so a
// clean (nil, nil) is proof the short-circuit fired first.
func TestResolveHandlesEmptyInputIsNoOp(t *testing.T) {
	c := &Comms{} // no store, no bus: the no-op path must touch neither.
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		handles []string
	}{
		{"nil slice", nil},
		{"empty non-nil slice", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The nil store is the instrument: any store call panics. Recover it
			// into a named failure rather than letting it abort the test binary
			// and mask the sibling subtests.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("resolveHandles(%s) reached the store (panic: %v); empty input must short-circuit to (nil, nil) BEFORE ResolveOwner/AccountsByHandles", tc.name, r)
				}
			}()
			ids, err := c.resolveHandles(ctx, store.AccountID("acct_caller"), tc.handles)
			if err != nil {
				t.Fatalf("resolveHandles(%s) error = %v, want nil (empty input is a no-op)", tc.name, err)
			}
			if ids != nil {
				t.Fatalf("resolveHandles(%s) = %#v, want nil ids", tc.name, ids)
			}
		})
	}
}
