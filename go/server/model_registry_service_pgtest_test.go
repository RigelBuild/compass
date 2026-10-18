//go:build pgtest && unix

package server

// Store-gated CompassService model-registry handler contracts (RIG-3122 P2):
// PutModelRegistry persists under a compare-and-set; Get returns empty-but-valid when
// unconfigured; malformed/orphaning is InvalidArgument; a stale version is Aborted;
// both writes are admin-gated. Behind `pgtest && unix`.

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

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

	ctx = authReq(f, ctx)
	putResp, err := f.client.PutModelRegistry(ctx, &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	})
	if err != nil {
		t.Fatalf("PutModelRegistry: %v", err)
	}
	if putResp.GetVersion() != 1 {
		t.Fatalf("seed version = %d, want 1", putResp.GetVersion())
	}
	ctx = authReq(f, ctx)
	getResp, err := f.client.GetModelRegistry(ctx, &compassv1.GetModelRegistryRequest{})
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	if getResp.GetVersion() != 1 {
		t.Errorf("read version = %d, want 1", getResp.GetVersion())
	}
	if _, ok := getResp.GetRegistry().GetEntries()["opus"]; !ok {
		t.Errorf("read registry missing 'opus': %+v", getResp.GetRegistry())
	}
}

// TestGetModelRegistryUnconfiguredIsEmpty: an unconfigured fleet reports version
// 0 and an empty registry, never an error.
func TestGetModelRegistryUnconfiguredIsEmpty(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	resp, err := f.client.GetModelRegistry(authReq(f, ctx), &compassv1.GetModelRegistryRequest{})
	if err != nil {
		t.Fatalf("GetModelRegistry on unconfigured fleet: %v", err)
	}
	if resp.GetVersion() != 0 {
		t.Errorf("unconfigured version = %d, want 0", resp.GetVersion())
	}
	if n := len(resp.GetRegistry().GetEntries()); n != 0 {
		t.Errorf("unconfigured registry has %d entries, want 0", n)
	}
}

// TestPutModelRegistryVersionBumpObservableViaGet: a replace at the current
// version bumps it, and the new version + payload are observable via Get.
func TestPutModelRegistryVersionBumpObservableViaGet(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	v1, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v2, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.GetVersion(),
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if v2.GetVersion() != v1.GetVersion()+1 {
		t.Fatalf("replace version = %d, want %d", v2.GetVersion(), v1.GetVersion()+1)
	}
	getResp, err := f.client.GetModelRegistry(authReq(f, ctx), &compassv1.GetModelRegistryRequest{})
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	if getResp.GetVersion() != v2.GetVersion() {
		t.Errorf("Get version = %d, want %d", getResp.GetVersion(), v2.GetVersion())
	}
}

// TestPutModelRegistryStaleVersionIsAborted: a write carrying a stale
// expected_version is CodeAborted — the connect/gRPC failed-CAS convention.
func TestPutModelRegistryStaleVersionIsAborted(t *testing.T) {
	f := newConfigFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	v1, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.GetVersion(),
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// Reuse the now-stale v1 version.
	_, err = f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("haiku"), ExpectedVersion: v1.GetVersion(),
	})
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
	_, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: bad, ExpectedVersion: 0,
	})
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
	v1, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("opus"), ExpectedVersion: 0,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Publish a config bundle whose profile pins "opus".
	if _, err := f.client.PutAgentConfig(authReq(f, ctx), &compassv1.PutAgentConfigRequest{
		Bundle: mkConfigBundle(t, map[string]string{
			"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
		}),
	}); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	_, err = f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: wireReg1("sonnet"), ExpectedVersion: v1.GetVersion(), // drops "opus"
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("orphaning removal = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

// A member bearer is denied on Put/Delete while Get remains open. The full door
// chain is required because the admin gate rejects before either handler runs.
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
		req := &compassv1.PutModelRegistryRequest{Registry: wireReg1("opus")}
		cctx, cancel := context.WithTimeout(ctx, testTimeout)
		cctx = clientContextWithBearer(cctx, "Bearer "+memberTok)
		defer cancel()
		if _, err := client.PutModelRegistry(cctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin PutModelRegistry = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})

	t.Run("DeleteModelRegistry as non-admin is PermissionDenied", func(t *testing.T) {
		req := &compassv1.DeleteModelRegistryRequest{}
		cctx, cancel := context.WithTimeout(ctx, testTimeout)
		cctx = clientContextWithBearer(cctx, "Bearer "+memberTok)
		defer cancel()
		if _, err := client.DeleteModelRegistry(cctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin DeleteModelRegistry = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})

	t.Run("GetModelRegistry as non-admin is admitted (open)", func(t *testing.T) {
		req := &compassv1.GetModelRegistryRequest{}
		cctx, cancel := context.WithTimeout(ctx, testTimeout)
		cctx = clientContextWithBearer(cctx, "Bearer "+memberTok)
		defer cancel()
		if _, err := client.GetModelRegistry(cctx, req); err != nil {
			t.Fatalf("non-admin GetModelRegistry = %v, want admitted", err)
		}
	})
}

// Distinct nonzero metadata values round-trip through Put/Get to catch dropped or
// transposed fields; the RPC also pins candidate read-back order.
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
	if _, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
		Registry: reg, ExpectedVersion: 0,
	}); err != nil {
		t.Fatalf("PutModelRegistry: %v", err)
	}
	getResp, err := f.client.GetModelRegistry(authReq(f, ctx), &compassv1.GetModelRegistryRequest{})
	if err != nil {
		t.Fatalf("GetModelRegistry: %v", err)
	}
	got, ok := getResp.GetRegistry().GetEntries()["opus"]
	if !ok {
		t.Fatalf("read registry missing 'opus': %+v", getResp.GetRegistry())
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

// Exercises delete and error mapping: clearing succeeds, but deleting a
// registry pinned by a published profile returns CodeInvalidArgument.
//
//nolint:contextcheck // test helpers (newTestStore, RequireDSN, must*) root their own context; threading ctx through them would touch every caller
func TestDeleteModelRegistryClearsAndOrphaningIsInvalidArgument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("clear on seeded registry succeeds and Get reports version 0", func(t *testing.T) {
		f := newConfigFixture(t)
		if _, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
			Registry: wireReg1("opus"), ExpectedVersion: 0,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.client.DeleteModelRegistry(authReq(f, ctx), &compassv1.DeleteModelRegistryRequest{}); err != nil {
			t.Fatalf("DeleteModelRegistry: %v", err)
		}
		getResp, err := f.client.GetModelRegistry(authReq(f, ctx), &compassv1.GetModelRegistryRequest{})
		if err != nil {
			t.Fatalf("GetModelRegistry after clear: %v", err)
		}
		if getResp.GetVersion() != 0 {
			t.Fatalf("version after clear = %d, want 0", getResp.GetVersion())
		}
	})

	t.Run("clear stranding a published profile pin is CodeInvalidArgument", func(t *testing.T) {
		f := newConfigFixture(t)
		if _, err := f.client.PutModelRegistry(authReq(f, ctx), &compassv1.PutModelRegistryRequest{
			Registry: wireReg1("opus"), ExpectedVersion: 0,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := f.client.PutAgentConfig(authReq(f, ctx), &compassv1.PutAgentConfigRequest{
			Bundle: mkConfigBundle(t, map[string]string{
				"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
			}),
		}); err != nil {
			t.Fatalf("publish pinning profile: %v", err)
		}
		_, err := f.client.DeleteModelRegistry(authReq(f, ctx), &compassv1.DeleteModelRegistryRequest{})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("orphaning clear = %v, want CodeInvalidArgument", connect.CodeOf(err))
		}
	})
}
