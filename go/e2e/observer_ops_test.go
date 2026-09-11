//go:build podman

package e2e

// Podman-tagged teeth for the T1 fixture plumbing (RIG-3528): the observer-scoped
// client mint (AsObserver) and the CreateUser/CreateChannel setup wrappers T3/T4
// consume. These are the primitives' own proof — the multi-actor legs that USE
// them (T2-T5) land separately, and the marker-script half of T1 is proven in the
// hermetic no-podman lane (cannedmodel_test.go).
//
// The load-bearing case here is the NEGATIVE: AsObserver mints a non-admin bearer
// via IssueToken, which is the seam that makes "what an account CANNOT see"
// assertable at all — every other fixture RPC rides the bootstrap-admin bearer
// (clients.go:63), and an admin sees everything.
//
// podmanUsable-guarded with the byte-identical harness skip literal
// (harness_test.go:29) so the e2e CI guard's skip-string grep matches; no second
// skip condition. Every RPC derives its own deterministic deadline internally
// from the passed-in ctx; the outer ctx is the test root.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TestObserverAndSetupPrimitives is the deterministic proof of the three T1
// podman-tagged primitives over the real stack: CreateUser mints an owner
// account, CreateChannel mints both a private (ungrouped, membership-only) and a
// shared-group (globally visible) channel, and AsObserver mints a working
// NON-ADMIN credential for that owner — proven a real, distinct credential by the
// admin-gate negative below, not merely by an RPC succeeding.
//
// It also carries the record's required member/non-member smoke (design.md:106):
// over the observer's own ListChannels RESPONSE, the shared channel and the
// observer's own private channel are PRESENT and a third account's private
// channel is ABSENT. That single assertion is what gives the `private` flag any
// teeth at all — id-non-empty plus id-distinct hold for ANY two created
// channels, so without reading the response the flag could be dead (or both
// channels shared) and nothing would notice. The present-channel canary keeps
// the negative from going vacuous: an empty list fails the positives rather
// than passing the absence. Only the CROSS-OWNER case is deferred to T4; this
// same-owner smoke is T1's.
func TestObserverAndSetupPrimitives(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	f := NewFixture(ctx, t)

	ownerID, err := f.CreateUser(ctx, "t1-observer-owner", "T1 Observer Owner")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if ownerID == "" {
		t.Fatal("CreateUser returned an empty account id")
	}

	privateID, err := f.CreateChannel(ctx, ownerID, "t1-private", true)
	if err != nil {
		t.Fatalf("CreateChannel (private): %v", err)
	}
	if privateID == "" {
		t.Fatal("CreateChannel (private) returned an empty channel id")
	}

	sharedID, err := f.CreateChannel(ctx, ownerID, "t1-shared", false)
	if err != nil {
		t.Fatalf("CreateChannel (shared): %v", err)
	}
	if sharedID == "" {
		t.Fatal("CreateChannel (shared) returned an empty channel id")
	}
	if sharedID == privateID {
		t.Fatalf("CreateChannel returned the same id %q for both channels", sharedID)
	}
	// A THIRD channel, private to a DIFFERENT account: the negative surface. Its
	// member is a second owner, so the observer is not a member and — being
	// ungrouped (private=true) — it is reachable only through channel_members.
	// It must therefore be absent from the observer's list.
	otherID, err := f.CreateUser(ctx, "t1-observer-other", "T1 Observer Other")
	if err != nil {
		t.Fatalf("CreateUser (other owner): %v", err)
	}
	otherPrivateID, err := f.CreateChannel(ctx, otherID, "t1-other-private", true)
	if err != nil {
		t.Fatalf("CreateChannel (other owner's private): %v", err)
	}
	if otherPrivateID == "" {
		t.Fatal("CreateChannel (other owner's private) returned an empty channel id")
	}

	// The observer credential works for an authenticatedOpen CommsService read,
	// and the RESPONSE is what carries the visibility teeth — not merely that
	// the RPC did not error.
	observerCompass, observerComms, err := f.AsObserver(ctx, ownerID)
	if err != nil {
		t.Fatalf("AsObserver(%s): %v", ownerID, err)
	}
	listCtx, cancelList := context.WithTimeout(ctx, rpcTimeout)
	defer cancelList()
	listResp, err := observerComms.ListChannels(listCtx, connect.NewRequest(&compassv1.ListChannelsRequest{}))
	if err != nil {
		t.Fatalf("observer ListChannels over the real door: %v", err)
	}
	visible := make(map[string]bool, len(listResp.Msg.GetChannels()))
	for _, ch := range listResp.Msg.GetChannels() {
		visible[ch.GetId()] = true
	}
	// The POSITIVE canary first: both of the observer's own channels are present.
	// This is what makes the absence below meaningful — an empty or broken list
	// reddens here instead of silently satisfying the negative.
	if !visible[sharedID] {
		t.Fatalf("observer ListChannels omitted the SHARED channel %q; visible ids = %v", sharedID, visible)
	}
	if !visible[privateID] {
		t.Fatalf("observer ListChannels omitted the observer's OWN private channel %q (it is a member); visible ids = %v", privateID, visible)
	}
	// THE negative: a private (ungrouped) channel the observer is NOT a member of
	// must not be reachable. If CreateChannel ignored `private` and grouped every
	// channel as SHARED, this id would be globally visible and this reddens.
	if visible[otherPrivateID] {
		t.Fatalf("observer ListChannels exposed %q — a PRIVATE channel owned by another account the observer is not a member of; the private/shared distinction is not holding", otherPrivateID)
	}

	// THE teeth on the credential itself: the observer bearer must be a genuine
	// NON-ADMIN identity, not the admin token handed back under a new name. An
	// adminOnly RPC (IssueToken, internal/auth/admin_gate.go:65) over the
	// observer's own CompassService client must be PermissionDenied. Without this
	// the positive read above would also pass if AsObserver silently reused the
	// admin bearer — exactly the bug that would make every future
	// negative-visibility assertion vacuous.
	denyCtx, cancelDeny := context.WithTimeout(ctx, rpcTimeout)
	defer cancelDeny()
	_, err = observerCompass.IssueToken(denyCtx, connect.NewRequest(&compassv1.IssueTokenRequest{
		AccountHandle: ownerID,
	}))
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
		t.Fatalf("observer bearer on the adminOnly IssueToken = %v, want CodePermissionDenied (the observer must NOT be the admin)", code)
	}
}

// TestAsObserverUnknownHandleIsNotFound is the record's named negative: minting
// an observer for a handle no account carries must surface NOT_FOUND from the
// SERVER (service.go:425-430), never a synthesized local error and never a
// silently-admin client. AsObserver passes an unresolvable ref through unchanged
// precisely so the server stays the authority on the code.
func TestAsObserverUnknownHandleIsNotFound(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + AsObserver

	f := NewFixture(ctx, t)

	compass, comms, err := f.AsObserver(ctx, "t1-no-such-account")
	if err == nil {
		t.Fatal("AsObserver for an unknown handle succeeded, want NOT_FOUND")
	}
	if compass != nil || comms != nil {
		t.Fatal("AsObserver returned clients alongside its error; a failed mint must yield no usable client")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("AsObserver(unknown) = %v, want CodeNotFound", code)
	}
}
