//go:build pgtest

package comms

// resolve.go's handle-resolution contracts, driven DIRECTLY against a real
// Postgres (RIG-3536 / T8): the resolver's own properties — submitted-order
// preservation, the bare-vs-qualified namespace split, and the vantage-probe
// closure — asserted on their own terms rather than incidentally via handlers.

// Driven as a specific account via WithActor's caller id. The resolvers are
// package methods taking the caller explicitly, so these call them directly:
// an RPC-level test cannot tell a resolver bug from a handler bug.

// NOT re-proven here: batch atomicity — store.AccountsByHandles owns it. What IS
// covered is the comms-tier PASS-THROUGH: the store's atomic-miss error survives
// resolveHandles with its ErrNotFound sentinel intact, so edgeError maps it to
// CodeNotFound. The DB-free contracts live in the untagged resolve_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// notFoundFor is the exact error text every oracle-safe resolve miss must carry:
// the store.ErrNotFound sentinel plus AgentByHandle's `handle %q` template naming
// the SUBMITTED handle. Two misses are indistinguishable precisely when each
// equals this for its own submitted spelling and differ in nothing else.
func notFoundFor(handle string) string {
	return fmt.Sprintf("%v: handle %q", store.ErrNotFound, handle)
}

// connectNotFoundFor asserts err is CodeNotFound whose message is exactly
// notFoundFor(handle): two misses passing this for their own spellings are byte-identical.
func connectNotFoundFor(t *testing.T, err error, handle, ctx string) {
	t.Helper()
	connectCodeIs(t, err, connect.CodeNotFound, ctx)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Message() != notFoundFor(handle) {
		t.Fatalf("%s: error = %v, want message %q naming the submitted handle", ctx, err, notFoundFor(handle))
	}
}

// TestResolveHandlesPreservesSubmittedOrder: the returned ids follow the
// SUBMITTED order, one id per submitted SLOT, so a caller that also needs
// per-input identity (memberUpdatesFromWire, mapping.go:291-299 slices each list
// back out by offset) can zip them back (resolve.go:31-33, code at :52-55).
//
// What this guarantees, and how each half is pinned:
//
//   - Positionally one id per SLOT. Seven submitted handles over SIX distinct
//     accounts, one spelling (`worker`) repeated. The hit map store.AccountsByHandles
//     returns is keyed by submitted spelling, so it holds six entries for seven
//     slots: any re-derivation of the output from that map's own iteration order
//     returns SIX ids and fails the length assertion DETERMINISTICALLY, on every
//     run. That is the load-bearing guard — not chance.
//   - The right id in each slot, checked STRUCTURALLY against a handle→id table
//     built from the fixture's own accounts (byHandle), never against a
//     hand-maintained parallel slice. Six distinct accounts make an accidental
//     identity permutation 1/720 per run, but the length check above already
//     kills the map-order bug outright, so this assertion is here to catch a
//     permutation that keeps the count right (a sort, a reversal, an off-by-one).
//
// The handles are deliberately non-alphabetical and span both resolution arms
// (four global users and two bare agents in the caller's own owner namespace).
// An earlier version of this comment claimed the fixture's ids "do not sort along
// with the handles"; that was false — the ids are store-minted and differ every
// run, which is exactly why the guarantees above are stated as slot count plus a
// fixture-derived table instead.
func TestResolveHandlesPreservesSubmittedOrder(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	// Created in one order, submitted in another.
	alpha := mustUser(t, st, "alpha")
	zeta := mustUser(t, st, "zeta")
	mike := mustUser(t, st, "mike")
	bravo := mustUser(t, st, "bravo")
	worker := mustAgent(t, st, owner.ID, "worker")
	tiller := mustAgent(t, st, owner.ID, "tiller")

	// The fixture's own handle→id table: every positional assertion below is
	// checked against THIS, so the test cannot drift out of step with the fixture.
	byHandle := map[string]store.AccountID{
		"alpha":  alpha.ID,
		"zeta":   zeta.ID,
		"mike":   mike.ID,
		"bravo":  bravo.ID,
		"worker": worker.ID,
		"tiller": tiller.ID,
	}
	seen := make(map[store.AccountID]string, len(byHandle))
	for h, id := range byHandle {
		if prev, dup := seen[id]; dup {
			t.Fatalf("fixture handles %q and %q share account id %q; the positional assertions below would be vacuous", prev, h, id)
		}
		seen[id] = h
	}

	// The caller is the owner user: it sees its own agents (ag.owner_user_id =
	// viewer) and every user, and ResolveOwner(user) is the user itself, so the
	// bare agent handles resolve in its own namespace. `worker` is submitted
	// TWICE: seven slots, six distinct accounts.
	submitted := []string{"zeta", "worker", "alpha", "tiller", "mike", "bravo", "worker"}

	got, err := c.resolveHandles(ctx, owner.ID, submitted)
	if err != nil {
		t.Fatalf("resolveHandles(%v) = %v, want success", submitted, err)
	}
	if len(got) != len(submitted) {
		t.Fatalf("resolveHandles(%v) returned %d ids, want %d — one per submitted SLOT, positionally (a repeated handle gets its own slot; %d ids means the output was re-derived from the hit map instead of the submitted slice)",
			submitted, len(got), len(submitted), len(got))
	}
	for i, h := range submitted {
		if got[i] != byHandle[h] {
			t.Fatalf("resolveHandles(%v)[%d] = %q, want %q (the fixture's id for %q) — the returned ids must follow the SUBMITTED order so a caller can zip them back; got %v",
				submitted, i, got[i], byHandle[h], h, got)
		}
	}
}

// TestResolveHandlesBareAgentHandleIsCallerOwnerNamespaced: the SAME bare handle
// resolves to DIFFERENT agents depending on who calls — bare agent handles are
// looked up in the CALLER's own owner namespace (resolve.go:46-48 passes
// callerOwner; agentOwnerNamespace's bare arm, :121-122). Two owners each own an
// agent spelled `worker`; each owner's resolution must land on its own.
//
// This is the assertion that catches a namespacing regression: a resolver that
// dropped callerOwner and searched globally would return one of the two agents to
// BOTH callers, so exactly one half of this test would go red — and, worse in
// production, one owner would silently address the other owner's agent.
func TestResolveHandlesBareAgentHandleIsCallerOwnerNamespaced(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "owner-a")
	ownerB := mustUser(t, st, "owner-b")
	workerA := mustAgent(t, st, ownerA.ID, "worker")
	workerB := mustAgent(t, st, ownerB.ID, "worker")

	if workerA.ID == workerB.ID {
		t.Fatalf("the two owners' `worker` agents share id %q; the per-owner handle index is not creating distinct accounts", workerA.ID)
	}

	for _, tc := range []struct {
		caller store.AccountID
		name   string
		want   store.AccountID
	}{
		{ownerA.ID, "owner-a", workerA.ID},
		{ownerB.ID, "owner-b", workerB.ID},
	} {
		got, err := c.resolveHandles(ctx, tc.caller, []string{"worker"})
		if err != nil {
			t.Fatalf("resolveHandles(%s, [worker]) = %v, want success", tc.name, err)
		}
		if got[0] != tc.want {
			t.Fatalf("resolveHandles(%s, [worker]) = %q, want %s's own worker %q — a bare agent handle resolves in the CALLER's owner namespace, not globally",
				tc.name, got[0], tc.name, tc.want)
		}
	}
}

// TestResolveAgentHandleOwnerQualifiedVsBareNamespacing: the same namespacing
// split on the singular-agent path (resolve.go:115-128). A BARE handle resolves
// in the caller's own owner namespace; an OWNER-QUALIFIED one resolves the named
// user in the global index and then the agent under THAT owner. Driven from one
// caller so the two spellings are the only variable: `worker` and `owner-b/worker`
// from owner-a's vantage must land on different agents.
func TestResolveAgentHandleOwnerQualifiedVsBareNamespacing(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "owner-a")
	ownerB := mustUser(t, st, "owner-b")
	workerA := mustAgent(t, st, ownerA.ID, "worker")
	workerB := mustAgent(t, st, ownerB.ID, "worker")

	bare, err := c.resolveAgentHandle(ctx, ownerA.ID, "worker")
	if err != nil {
		t.Fatalf("resolveAgentHandle(owner-a, %q) = %v, want owner-a's own worker", "worker", err)
	}
	if bare != workerA.ID {
		t.Fatalf("resolveAgentHandle(owner-a, \"worker\") = %q, want owner-a's worker %q (bare → the caller's own owner namespace)", bare, workerA.ID)
	}

	qualified, err := c.resolveAgentHandle(ctx, ownerA.ID, "owner-b/worker")
	if err != nil {
		t.Fatalf("resolveAgentHandle(owner-a, %q) = %v, want owner-b's worker", "owner-b/worker", err)
	}
	if qualified != workerB.ID {
		t.Fatalf("resolveAgentHandle(owner-a, \"owner-b/worker\") = %q, want owner-b's worker %q (owner-qualified → the NAMED user's namespace, resolved in the global index)", qualified, workerB.ID)
	}
	if bare == qualified {
		t.Fatalf("bare and owner-qualified `worker` both resolved to %q; the owner qualifier is being ignored", bare)
	}
}

// TestResolveAgentHandleUnknownOwnerQualifierNamesSubmittedHandle: an owner
// qualifier that resolves to NOTHING is store.ErrNotFound naming the SUBMITTED
// handle — never the owner segment, and never the store's own UserByHandle
// spelling (resolve.go:117-119 + notFoundHandle :135-139). That is what makes an
// unknown-owner miss byte-identical to an unknown-agent miss: an attacker cannot
// tell WHICH segment failed, so the error is not an owner-existence oracle.
func TestResolveAgentHandleUnknownOwnerQualifierNamesSubmittedHandle(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	mustAgent(t, st, owner.ID, "worker")

	// `nobody` is not a user, so the owner qualifier resolves to nothing.
	_, err := c.resolveAgentHandle(ctx, owner.ID, "nobody/worker")
	if err == nil {
		t.Fatal("resolveAgentHandle(owner, \"nobody/worker\") = nil error, want NOT_FOUND: the owner qualifier resolves to nothing")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolveAgentHandle(unknown owner qualifier) = %v, which is not store.ErrNotFound; edgeError would map it to something other than CodeNotFound", err)
	}
	if err.Error() != notFoundFor("nobody/worker") {
		t.Fatalf("resolveAgentHandle(unknown owner qualifier) message = %q, want %q — the SUBMITTED handle names the miss, not the owner segment alone",
			err.Error(), notFoundFor("nobody/worker"))
	}

	// And it is indistinguishable from an unknown AGENT under a REAL owner: same
	// text modulo the submitted spelling, so the message reveals which segment
	// failed to nobody.
	_, agentErr := c.resolveAgentHandle(ctx, owner.ID, "owner/ghost")
	if agentErr == nil || agentErr.Error() != notFoundFor("owner/ghost") {
		t.Fatalf("resolveAgentHandle(owner, \"owner/ghost\") = %v, want %q — an unknown agent under a real owner must carry the SAME template as an unknown owner",
			agentErr, notFoundFor("owner/ghost"))
	}
}

// TestResolveVisibleAgentHandleInvisibleIsIndistinguishableFromUnknown keeps
// roster vantages on the list predicate: peering alone does not expose foreign
// agents, though addressing admits a mutually peered owner.
func TestResolveVisibleAgentHandleInvisibleIsIndistinguishableFromUnknown(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	// Two owners and an agent caller set up an unrelated foreign target. The
	// peered resolver path is tested separately; this function remains list-scoped.
	ownerA := mustUser(t, st, "owner-a")
	ownerB := mustUser(t, st, "owner-b")
	workerA := mustAgent(t, st, ownerA.ID, "worker")
	victim := mustAgent(t, st, ownerB.ID, "victim")

	for _, tc := range []struct {
		name   string
		caller store.AccountID
	}{
		{"agent caller", workerA.ID},
		{"user caller", ownerA.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {

			// The singular resolver stays list-independent; the roster vantage
			// enforces caller visibility separately through AccountVisibleTo.
			resolved, err := c.resolveAgentHandle(ctx, tc.caller, "owner-b/victim")
			if err != nil {
				t.Fatalf("resolveAgentHandle(%s, \"owner-b/victim\") = %v, want the victim's id: this path is owner-namespaced but NOT viewer-scoped, so an invisible-but-real agent in the resolution owner's namespace STILL resolves", tc.name, err)
			}
			if resolved != victim.ID {
				t.Fatalf("resolveAgentHandle(%s, \"owner-b/victim\") = %q, want victim %q", tc.name, resolved, victim.ID)
			}

			// Asymmetry, half two: the roster vantage layers its OWN visibility
			// check on top, so the same handle misses.
			_, invisibleErr := c.resolveVisibleAgentHandle(ctx, tc.caller, "owner-b/victim")
			if invisibleErr == nil {
				t.Fatalf("resolveVisibleAgentHandle(%s, \"owner-b/victim\") = nil error, want NOT_FOUND: a real-but-invisible vantage must not resolve, or the visibility check is dead (or keyed on the wrong argument) and the vantage-probe oracle is open", tc.name)
			}

			// The unknown-handle baseline, in the same owner namespace and the same
			// spelling shape.
			_, unknownErr := c.resolveVisibleAgentHandle(ctx, tc.caller, "owner-b/ghost")
			if unknownErr == nil {
				t.Fatalf("resolveVisibleAgentHandle(%s, \"owner-b/ghost\") = nil error, want NOT_FOUND for an unknown handle", tc.name)
			}

			// The equivalence: each error is EXACTLY the oracle-safe template for its
			// own submitted spelling. Nothing else in either message differs, so the
			// two outcomes are indistinguishable to the caller.
			if invisibleErr.Error() != notFoundFor("owner-b/victim") {
				t.Fatalf("invisible vantage error (%s) = %q, want %q — a real-but-invisible vantage must carry the same message an unknown handle does (which is %q for its own spelling); any extra detail is a vantage-probe oracle",
					tc.name, invisibleErr.Error(), notFoundFor("owner-b/victim"), unknownErr.Error())
			}
			if unknownErr.Error() != notFoundFor("owner-b/ghost") {
				t.Fatalf("unknown vantage error (%s) = %q, want %q", tc.name, unknownErr.Error(), notFoundFor("owner-b/ghost"))
			}
			// Stated as the equivalence itself, not just two template matches:
			// substitute each submitted spelling out and the two messages must be
			// byte-identical.
			invisibleShape := strings.Replace(invisibleErr.Error(), "owner-b/victim", "<handle>", 1)
			unknownShape := strings.Replace(unknownErr.Error(), "owner-b/ghost", "<handle>", 1)
			if invisibleShape != unknownShape {
				t.Fatalf("invisible-vantage and unknown-handle errors are DISTINGUISHABLE for the %s: %q vs %q (modulo the submitted handle) — that split is the vantage-probe oracle resolveVisibleAgentHandle exists to close",
					tc.name, invisibleShape, unknownShape)
			}

			// The resolved id must never appear: it is the other leak DL-269 closed.
			if strings.Contains(invisibleErr.Error(), string(victim.ID)) {
				t.Fatalf("invisible vantage error %q (%s) leaks the resolved account id %q; the submitted handle names the miss, never the resolved id", invisibleErr, tc.name, victim.ID)
			}

			// Both map to CodeNotFound at the edge, the code the wire contract
			// promises.
			connectCodeIs(t, edgeError(invisibleErr), connect.CodeNotFound, "edgeError(invisible vantage, "+tc.name+")")
			connectCodeIs(t, edgeError(unknownErr), connect.CodeNotFound, "edgeError(unknown vantage, "+tc.name+")")
		})
	}
}

// TestResolveVisibleAgentHandleVisibleAgentResolves is the paired positive: the
// visibility check must clip only what is genuinely invisible. Without this, a
// resolveVisibleAgentHandle that returned NOT_FOUND for EVERYTHING would satisfy
// the indistinguishability test above — the security property is only worth
// having if the happy path still works.
func TestResolveVisibleAgentHandleVisibleAgentResolves(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	worker := mustAgent(t, st, owner.ID, "worker")

	// The user owner sees its own agent without a shared channel.
	// The foreign-target exclusion is covered by TestResolveVisibleAgentHandleInvisibleIsIndistinguishableFromUnknown.
	got, err := c.resolveVisibleAgentHandle(ctx, owner.ID, "worker")
	if err != nil {
		t.Fatalf("resolveVisibleAgentHandle(owner, \"worker\") = %v, want the owner's own visible agent", err)
	}
	if got != worker.ID {
		t.Fatalf("resolveVisibleAgentHandle(owner, \"worker\") = %q, want %q", got, worker.ID)
	}
}

func TestResolveAddressableAgentPeeringAndRosterClip(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "address-owner-a")
	ownerB := mustUser(t, st, "address-owner-b")
	caller := mustAgent(t, st, ownerA.ID, "address-caller")
	target := mustAgent(t, st, ownerB.ID, "address-target")

	for _, handle := range []string{"address-owner-b/address-target", "address-target"} {
		if _, err := c.resolveAddressableAgent(ctx, caller.ID, handle); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolveAddressableAgent(%q) error = %v; want not-found before peering", handle, err)
		}
	}
	if _, err := st.ApprovePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("ApprovePeer(a->b): %v", err)
	}
	if _, err := c.resolveAddressableAgent(ctx, caller.ID, "address-owner-b/address-target"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("one-sided resolveAddressableAgent error = %v; want not-found", err)
	}
	if _, err := st.ApprovePeer(ctx, ownerB.ID, ownerA.ID); err != nil {
		t.Fatalf("ApprovePeer(b->a): %v", err)
	}
	got, err := c.resolveAddressableAgent(ctx, caller.ID, "address-owner-b/address-target")
	if err != nil || got.ID != target.ID {
		t.Fatalf("resolveAddressableAgent(peered) = (%q, %v), want %q", got.ID, err, target.ID)
	}
	if _, err := c.resolveVisibleAgentHandle(ctx, caller.ID, "address-owner-b/address-target"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolveVisibleAgentHandle(peered target) error = %v; want not-found", err)
	}

	if _, err := st.CreateChannel(ctx, ownerA.ID, store.NewChannel{
		Name: "address-shared-room", Kind: store.ChannelKindGroupDM,
		MemberAccountIDs: []store.AccountID{caller.ID, target.ID},
	}); err != nil {
		t.Fatalf("CreateChannel(shared room): %v", err)
	}
	got, err = c.resolveAddressableAgent(ctx, caller.ID, "address-owner-b/address-target")
	if err != nil || got.ID != target.ID {
		t.Fatalf("resolveAddressableAgent(co-member) = (%q, %v), want %q", got.ID, err, target.ID)
	}
	if _, err := st.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer(a->b): %v", err)
	}
	got, err = c.resolveAddressableAgent(ctx, caller.ID, "address-owner-b/address-target")
	if err != nil || got.ID != target.ID {
		t.Fatalf("resolveAddressableAgent(revoked co-member) = (%q, %v), want %q", got.ID, err, target.ID)
	}
}

// TestResolveHandlesPassesThroughStoreMissUnmangled: resolveHandles does NOT
// implement batch atomicity — store.AccountsByHandles does (resolve.go:48 →
// accounts.go:811), and the store tier already proves it
// (TestAccountsByHandlesAtomicMissNamesAll). What this pins is the comms-tier
// PASS-THROUGH at resolve.go:49-51: the store's error arrives at the caller with
// its store.ErrNotFound sentinel and its message INTACT — not swallowed into a
// partial result, not re-wrapped into something edgeError maps elsewhere.
//
// The sentinel is the load-bearing part: lose it and this becomes a
// CodeInternal, so an ordinary unknown handle would read as a server fault.
func TestResolveHandlesPassesThroughStoreMissUnmangled(t *testing.T) {
	c, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	mustAgent(t, st, owner.ID, "worker")

	submitted := []string{"worker", "ghost-one", "ghost-two"}
	ids, err := c.resolveHandles(ctx, owner.ID, submitted)
	if err == nil {
		t.Fatalf("resolveHandles(%v) = (%v, nil), want the store's miss to propagate", submitted, ids)
	}
	if ids != nil {
		t.Fatalf("resolveHandles(%v) returned ids %v alongside an error; a miss must yield NO partial result", submitted, ids)
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolveHandles(miss) = %v, which is not store.ErrNotFound; the sentinel must survive the pass-through or edgeError maps an ordinary unknown handle to a server fault", err)
	}
	connectCodeIs(t, edgeError(err), connect.CodeNotFound, "edgeError(resolveHandles miss)")

	// The store's message reaches the caller unmangled: it still names the
	// unresolved handles the store named, in their submitted spelling.
	storeErr := func() error {
		_, e := st.AccountsByHandles(ctx, owner.ID, owner.ID, []store.QualifiedHandle{
			store.ParseQualifiedHandle("worker"),
			store.ParseQualifiedHandle("ghost-one"),
			store.ParseQualifiedHandle("ghost-two"),
		})
		return e
	}()
	if storeErr == nil {
		t.Fatal("store.AccountsByHandles did not error on the same input; this test's premise (a store-tier miss to pass through) no longer holds")
	}
	if err.Error() != storeErr.Error() {
		t.Fatalf("resolveHandles error = %q, want the store's own %q verbatim — resolveHandles must pass the store's miss through UNMANGLED, not re-author it",
			err.Error(), storeErr.Error())
	}
}
