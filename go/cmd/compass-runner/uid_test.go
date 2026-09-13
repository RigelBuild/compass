//go:build unix

package main

import (
	"errors"
	"strconv"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// TestHostDerivedUIDAcceptedByProvisionPath is the end-to-end regression this
// task exists to prevent: the uid the Runner derives for the host tier is
// exactly the uid the backend's AsUser rule accepts, so every provision-path
// exec (which passes Workspace.UID as AsUser) is accepted rather than rejected.
//
// The derivation and the AsUser rule now close structurally: ResolveWorkspaceUID
// returns HostRuntime.WorkspaceUID(), which reports the euid the backend captured
// at construction — the very same h.euid checkUser enforces. Both read one
// captured value, not two independent geteuid syscalls that agreed by accident.
//
// This box's euid is not asserted to differ from AgentUID, so it does not
// discriminate the "euid != 1000" regression on a 1000-box — that acceptance
// needs a box whose euid is not 1000 (see the report). What it proves here is
// the contract closure: derived uid == the uid checkUser trusts.
func TestHostDerivedUIDAcceptedByProvisionPath(t *testing.T) {
	host := runtime.NewHostRuntime(t.TempDir())
	uid, err := runner.ResolveWorkspaceUID(host)
	if err != nil {
		t.Fatalf("ResolveWorkspaceUID(host) err = %v", err)
	}
	id, err := host.Create(t.Context(), runtime.WorkloadSpec{Name: "agent-provision"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := host.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	asUser := strconv.FormatUint(uint64(uid), 10)
	if _, err := host.Exec(t.Context(), id, runtime.NewExecSpec("true").AsUser(asUser)); err != nil {
		if _, ok := errors.AsType[*runtime.UnsupportedUserError](err); ok {
			t.Fatalf("provision AsUser(%s) rejected by host backend: %v", asUser, err)
		}
		t.Fatalf("Exec AsUser(%s) = %v, want nil", asUser, err)
	}
}
