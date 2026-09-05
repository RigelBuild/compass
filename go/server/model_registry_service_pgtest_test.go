//go:build pgtest && unix

package server

// Store-gated CompassService model-registry handler contracts (RIG-3122 P2):
// PutModelRegistry persists under a compare-and-set and returns the new version;
// GetModelRegistry returns an empty-but-valid response on an unconfigured fleet
// and the payload on a configured one; a malformed payload is CodeInvalidArgument;
// an orphaning removal is CodeInvalidArgument; a stale expected_version is
// CodeAborted; and the two write RPCs are admin-gated on the network door. They
// need a real Postgres because the writes persist the singleton row and the
// handler reads a genuine caller identity. The admin-gate denial runs through the
// full network-door chain (networkDoorHandler); the handler-contract cases run
// through the bearer-only fixture. Behind `pgtest && unix` (SKIP when no runtime).

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
)

// wireReg1 builds a single-entry wire registry keyed on the given stable name.
func wireReg1(name string) *compassv1.ModelRegistry {
	return &compassv1.ModelRegistry{Entries: map[string]*compassv1.ModelRegistryEntry{
		name: {
			DisplayName: name,
			Candidates:  []*compassv1.ModelCandidate{{Provider: "anthropic", ModelId: "claude-" + name}},
		},
	}}
}

// TestPutModelRegistryPersistsAndReadsBack: a valid seed returns version 1 and
// GetModelRegistry reads that version and payload back.
func TestPutModelRegistryPersistsAndReadsBack(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	putResp, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	}))
	if err != nil {
		t.Fatalf("PutModelRegistry: %v", err)
	}
	if putResp.Msg.GetVersion() != 1 {
		t.Fatalf("seed version = %d, want 1", putResp.Msg.GetVersion())
	}
	getResp, err := f.client.GetModelRegistry(ctx, authReq(f, &compassv1.GetModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	if getResp.Msg.GetVersion() != 1 {
		t.Errorf("read version = %d, want 1", getResp.Msg.GetVersion())
	}
	if _, ok := getResp.Msg.GetRegistry().GetEntries()["opus"]; !ok {
		t.Errorf("read registry missing 'opus': %+v", getResp.Msg.GetRegistry())
	}
}

// TestGetModelRegistryUnconfiguredIsEmpty: an unconfigured fleet reports version
// 0 and an empty registry, never an error.
func TestGetModelRegistryUnconfiguredIsEmpty(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	resp, err := f.client.GetModelRegistry(ctx, authReq(f, &compassv1.GetModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetModelRegistry on unconfigured fleet: %v", err)
	}
	if resp.Msg.GetVersion() != 0 {
		t.Errorf("unconfigured version = %d, want 0", resp.Msg.GetVersion())
	}
	if n := len(resp.Msg.GetRegistry().GetEntries()); n != 0 {
		t.Errorf("unconfigured registry has %d entries, want 0", n)
	}
}

// TestPutModelRegistryVersionBumpObservableViaGet: a replace at the current
// version bumps it, and the new version + payload are observable via Get.
func TestPutModelRegistryVersionBumpObservableViaGet(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	v1, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	}))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v2, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.Msg.GetVersion(),
	}))
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if v2.Msg.GetVersion() != v1.Msg.GetVersion()+1 {
		t.Fatalf("replace version = %d, want %d", v2.Msg.GetVersion(), v1.Msg.GetVersion()+1)
	}
	getResp, err := f.client.GetModelRegistry(ctx, authReq(f, &compassv1.GetModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	if getResp.Msg.GetVersion() != v2.Msg.GetVersion() {
		t.Errorf("Get version = %d, want %d", getResp.Msg.GetVersion(), v2.Msg.GetVersion())
	}
}

// TestPutModelRegistryStaleVersionIsAborted: a write carrying a stale
// expected_version is CodeAborted — the connect/gRPC failed-CAS convention.
func TestPutModelRegistryStaleVersionIsAborted(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	v1, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	}))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.Msg.GetVersion(),
	})); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// Reuse the now-stale v1 version.
	_, err = f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("haiku"), ExpectedVersion: v1.Msg.GetVersion(),
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale version = %v, want CodeAborted", connect.CodeOf(err))
	}
}

// TestPutModelRegistryMalformedIsInvalidArgument: a payload that fails the door
// (an entry with no candidates) is CodeInvalidArgument.
func TestPutModelRegistryMalformedIsInvalidArgument(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	bad := &compassv1.ModelRegistry{Entries: map[string]*compassv1.ModelRegistryEntry{
		"opus": {DisplayName: "Opus", Candidates: nil}, // no candidates
	}}
	_, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: bad, ExpectedVersion: 0,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed payload = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

// TestPutModelRegistryOrphaningRemovalIsInvalidArgument: a replace dropping a
// stable name still referenced by a published profile is CodeInvalidArgument.
func TestPutModelRegistryOrphaningRemovalIsInvalidArgument(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Seed a registry holding "opus" FIRST — the reverse bundle-door lint rejects
	// publishing a profile that pins a name absent from the registry.
	v1, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	}))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Publish a config bundle whose profile pins "opus".
	if _, err := f.client.PutAgentConfig(ctx, authReq(f, &compassv1.PutAgentConfigRequest{
		Bundle: mkConfigBundle(t, map[string]string{
			"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
		}),
	})); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	_, err = f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.Msg.GetVersion(), // drops "opus"
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("orphaning removal = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

// TestModelRegistryWriteRPCsDenyNonAdminToken pins the network-door admin gate:
// a valid NON-admin (member) bearer is denied CodePermissionDenied on both
// PutModelRegistry and DeleteModelRegistry, while GetModelRegistry (open) is
// admitted. The gate short-circuits BEFORE the handler, so it uses the full
// network-door chain via networkDoorHandler. Reddens if the gate ever stops
// classifying either write adminOnly, or classifies Get anything but open.
func TestModelRegistryWriteRPCsDenyNonAdminToken(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)
	memberTok, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("rig3122-denial-test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	t.Run("PutModelRegistry as non-admin is PermissionDenied", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.PutModelRegistryRequest{Registry: wireReg1("opus")})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		cctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.PutModelRegistry(cctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin PutModelRegistry = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})

	t.Run("DeleteModelRegistry as non-admin is PermissionDenied", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.DeleteModelRegistryRequest{})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		cctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.DeleteModelRegistry(cctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin DeleteModelRegistry = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})

	t.Run("GetModelRegistry as non-admin is admitted (open)", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.GetModelRegistryRequest{})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		cctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.GetModelRegistry(cctx, req); err != nil {
			t.Fatalf("non-admin GetModelRegistry = %v, want admitted", err)
		}
	})
}

// TestPutModelRegistryMetadataRoundTripViaRPC is the M3 assertion at the wire
// boundary: it drives a fully-populated Metadata through registryFromProto (Put)
// and registryToProto (Get) — the four hand-mapped, transposition-prone fields —
// with DISTINCT non-zero values so a swap of the two cost fields, or a dropped
// field, reddens. It also pins the 2-candidate chain's read-back order (L4).
func TestPutModelRegistryMetadataRoundTripViaRPC(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	want := &compassv1.ModelRegistryEntry{
		DisplayName: "Claude Opus",
		Candidates: []*compassv1.ModelCandidate{
			{Provider: "anthropic", ModelId: "claude-opus-4"},   // order index 0
			{Provider: "openrouter", ModelId: "anthropic/opus"}, // order index 1
		},
		Metadata: &compassv1.ModelMetadata{
			ContextWindow:      200000,
			InputCostMicroUsd:  15,
			OutputCostMicroUsd: 75,
			Api:                "anthropic-messages",
		},
	}
	reg := &compassv1.ModelRegistry{Entries: map[string]*compassv1.ModelRegistryEntry{"opus": want}}
	if _, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
		Registry: reg, ExpectedVersion: 0,
	})); err != nil {
		t.Fatalf("PutModelRegistry: %v", err)
	}
	getResp, err := f.client.GetModelRegistry(ctx, authReq(f, &compassv1.GetModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	got, ok := getResp.Msg.GetRegistry().GetEntries()["opus"]
	if !ok {
		t.Fatalf("read registry missing 'opus': %+v", getResp.Msg.GetRegistry())
	}
	md := got.GetMetadata()
	if md.GetContextWindow() != 200000 {
		t.Errorf("context_window = %d, want 200000", md.GetContextWindow())
	}
	if md.GetInputCostMicroUsd() != 15 {
		t.Errorf("input_cost_micro_usd = %d, want 15", md.GetInputCostMicroUsd())
	}
	if md.GetOutputCostMicroUsd() != 75 {
		t.Errorf("output_cost_micro_usd = %d, want 75", md.GetOutputCostMicroUsd())
	}
	if md.GetApi() != "anthropic-messages" {
		t.Errorf("api = %q, want %q", md.GetApi(), "anthropic-messages")
	}
	// L4: candidate read-back order equals written order.
	cands := got.GetCandidates()
	if len(cands) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(cands))
	}
	if cands[0].GetModelId() != "claude-opus-4" || cands[1].GetModelId() != "anthropic/opus" {
		t.Errorf("candidate order = [%q, %q], want [claude-opus-4, anthropic/opus]", cands[0].GetModelId(), cands[1].GetModelId())
	}
}

// TestDeleteModelRegistryClearsAndOrphaningIsInvalidArgument (L3a) drives the
// DeleteModelRegistry handler + its mapModelRegistryErr path at the wire
// boundary — the store-layer clear/orphaning pair is covered, but the handler
// and error mapping were exercised only for the admin-gate denial. A clear on a
// seeded registry succeeds and Get then reports version 0; a clear that would
// strand a published profile's pin maps the store's ErrInvalidArgument to
// CodeInvalidArgument.
func TestDeleteModelRegistryClearsAndOrphaningIsInvalidArgument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("clear on seeded registry succeeds and Get reports version 0", func(t *testing.T) {
		f := newConfigFixture(t)
		if _, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
			Registry: wireReg1("opus"), ExpectedVersion: 0,
		})); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.client.DeleteModelRegistry(ctx, authReq(f, &compassv1.DeleteModelRegistryRequest{})); err != nil {
			t.Fatalf("DeleteModelRegistry: %v", err)
		}
		getResp, err := f.client.GetModelRegistry(ctx, authReq(f, &compassv1.GetModelRegistryRequest{}))
		if err != nil {
			t.Fatalf("GetModelRegistry after clear: %v", err)
		}
		if getResp.Msg.GetVersion() != 0 {
			t.Fatalf("version after clear = %d, want 0", getResp.Msg.GetVersion())
		}
	})

	t.Run("clear stranding a published profile pin is CodeInvalidArgument", func(t *testing.T) {
		f := newConfigFixture(t)
		if _, err := f.client.PutModelRegistry(ctx, authReq(f, &compassv1.PutModelRegistryRequest{
			Registry: wireReg1("opus"), ExpectedVersion: 0,
		})); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.client.PutAgentConfig(ctx, authReq(f, &compassv1.PutAgentConfigRequest{
			Bundle: mkConfigBundle(t, map[string]string{
				"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
			}),
		})); err != nil {
			t.Fatalf("publish pinning profile: %v", err)
		}
		_, err := f.client.DeleteModelRegistry(ctx, authReq(f, &compassv1.DeleteModelRegistryRequest{}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("orphaning clear = %v, want CodeInvalidArgument", connect.CodeOf(err))
		}
	})
}
