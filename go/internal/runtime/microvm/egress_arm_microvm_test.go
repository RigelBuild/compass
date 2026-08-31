//go:build microvm && unix

package microvm

// The RIG-3020 (V3 W3) direct-guestd arm-failure proof, run KVM-backed beside
// the boot spike and the netfilter-autoload proof (boot_microvm_test.go). Where
// TestInGuestEgressArmAutoloadsNetfilter proves a VALID arm succeeds, this proves
// the fail-closed half on REAL hardware: an arm script that exits non-zero fails
// Provision with CodeInternal AND leaves the exec gate closed, so a follow-up
// Exec is refused. That fail-closed guestd path (supervisor.go: a failed armFunc
// returns before the ready->provisioned transition) is only fully exercised
// against the real guestd running as PID 1 in the guest — the hermetic W2 seam
// test (runtime.TestStartProvisionErrorFailsAndTearsDown) proves the host
// tears the VM down on a Provision error, but only a real boot proves guestd
// itself refuses the exec after a real arm failure.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// TestInGuestArmFailureKeepsExecGateClosed drives guestd directly (Launch +
// GuestClient) with a Provision whose nft_script exits non-zero. The arm runs
// `/bin/sh -c <script>` as guest root; a non-zero exit must fail Provision with
// CodeInternal (the arm is bounded and its exit status surfaced,
// guestd/supervisor.go runNftScript), and because the arm fails BEFORE the
// state transition, the gate stays at ready — so a follow-up Exec is refused
// with CodeFailedPrecondition. This is the §(b)/§(d) fail-closed contract on
// real hardware: a failed arm never opens the exec gate.
func TestInGuestArmFailureKeepsExecGateClosed(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 2, 1024)

	vm, err := Launch(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	t.Cleanup(func() {
		if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
			t.Errorf("Shutdown: %v", shutErr)
		}
	})

	ready := waitFor(t, fullBootDeadline, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		resp, healthErr := vm.Health(ctx)
		if healthErr != nil {
			return false
		}
		return resp.GetNetProvisioned() && resp.GetWorkspaceMounted()
	})
	if !ready {
		t.Fatalf("guest did not reach Health{net_provisioned && workspace_mounted} within %s.\n%s",
			fullBootDeadline, vm.Diagnostics())
	}

	client := GuestClient(vm.vsockSocket, vm.vsockPort)

	// A non-empty script that exits non-zero: the arm runs it as guest root and
	// must surface the failure. `set -eu` + a bad `nft` command mirrors a real
	// broken ruleset; a bare `exit 1` would also fail, but a failing nft add
	// proves the arm actually ran the script through /bin/sh, not merely that a
	// shell exited.
	provCtx, provCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer provCancel()
	_, provErr := client.Provision(provCtx, connect.NewRequest(&compassv1.ProvisionRequest{
		NftScript:      "set -eu\nnft add rule inet nonexistent_table output accept",
		DefaultExecUid: 1000,
	}))
	if provErr == nil {
		t.Fatalf("Provision with a failing arm script succeeded; want a fail-closed error.\n%s", vm.Diagnostics())
	}
	if got := connect.CodeOf(provErr); got != connect.CodeInternal {
		t.Fatalf("failed-arm Provision error code = %v, want CodeInternal (guestd/supervisor.go arm failure): %v", got, provErr)
	}
	t.Logf("failed arm correctly refused with CodeInternal: %v", provErr)

	// The gate must still be closed: the arm failed before the ready->provisioned
	// transition, so Exec is refused with CodeFailedPrecondition (the
	// requireProvisioned gate), NOT run.
	execCtx, execCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer execCancel()
	uid := uint32(1000)
	_, execErr := client.Exec(execCtx, connect.NewRequest(&compassv1.ExecRequest{
		Command: []string{"true"},
		Uid:     &uid,
	}))
	if execErr == nil {
		t.Fatalf("Exec after a failed arm succeeded; the exec gate must stay closed.\n%s", vm.Diagnostics())
	}
	if got := connect.CodeOf(execErr); got != connect.CodeFailedPrecondition {
		t.Fatalf("post-failed-arm Exec error code = %v, want CodeFailedPrecondition (gate closed): %v", got, execErr)
	}
	t.Logf("exec gate stayed closed after the failed arm: %v", execErr)
}
