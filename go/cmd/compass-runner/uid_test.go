//go:build unix

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// TestSpecDefaultsUIDHostDerivesFromEuid: on the host backend the workspace uid
// is the Runner's own effective uid, read through the geteuid seam, never the
// baked fleet constant. The seam returns a value distinct from AgentUID so the
// test discriminates: the pre-T1a code handed AgentUID here.
func TestSpecDefaultsUIDHostDerivesFromEuid(t *testing.T) {
	const euid = 4242
	uid, err := specDefaultsUID(runtime.NewHostRuntime(t.TempDir()), func() int { return euid })
	if err != nil {
		t.Fatalf("specDefaultsUID(host) err = %v, want nil", err)
	}
	if uid != euid {
		t.Fatalf("specDefaultsUID(host) = %d, want the derived euid %d", uid, euid)
	}
	if uid == agentuid.AgentUID {
		t.Fatalf("specDefaultsUID(host) = %d, must not be the baked fleet constant", uid)
	}
}

// TestSpecDefaultsUIDContainerTiersKeepConstant: the podman/microvm/
// apple-container tiers keep AgentUID byte-identically — their userns remap maps
// the invoking host uid onto the baked 1000, so the geteuid seam is never
// consulted (it panics if it is).
func TestSpecDefaultsUIDContainerTiersKeepConstant(t *testing.T) {
	neverCalled := func() int {
		t.Fatal("geteuid must not be read for a container backend")
		return 0
	}
	engines := map[string]runtime.WorkloadRuntime{
		"podman":          runtime.NewPodmanCLI(),
		"microvm":         runtime.NewMicroVMRuntime(runtime.MicroVMConfig{}),
		"apple-container": runtime.NewAppleContainerCLI(runtime.AppleContainerConfig{}),
	}
	for name, engine := range engines {
		t.Run(name, func(t *testing.T) {
			uid, err := specDefaultsUID(engine, neverCalled)
			if err != nil {
				t.Fatalf("specDefaultsUID(%s) err = %v, want nil", name, err)
			}
			if uid != agentuid.AgentUID {
				t.Fatalf("specDefaultsUID(%s) = %d, want AgentUID %d unchanged", name, uid, agentuid.AgentUID)
			}
		})
	}
}

// TestSpecDefaultsUIDNegativeEuidRefused: os.Geteuid returns -1 where the
// syscall is unavailable; the derivation must refuse it rather than wrap it into
// a huge uint32. Reachable only through the seam.
func TestSpecDefaultsUIDNegativeEuidRefused(t *testing.T) {
	_, err := specDefaultsUID(runtime.NewHostRuntime(t.TempDir()), func() int { return -1 })
	if err == nil {
		t.Fatal("specDefaultsUID(host, euid=-1) err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "-1") {
		t.Fatalf("error %q does not name the offending euid", err)
	}
}

// TestSpecDefaultsUIDRootRefusedAtStartup: a host Runner whose euid is 0 is
// refused at startup by the existing non-root check, not at the first provision.
// The derivation itself returns 0 (root reaches the same check the container
// tiers do); NewConfigSpecBuilder is what refuses it.
func TestSpecDefaultsUIDRootRefusedAtStartup(t *testing.T) {
	uid, err := specDefaultsUID(runtime.NewHostRuntime(t.TempDir()), func() int { return 0 })
	if err != nil {
		t.Fatalf("specDefaultsUID(host, euid=0) err = %v, want nil (the non-root check refuses it)", err)
	}
	if uid != 0 {
		t.Fatalf("specDefaultsUID(host, euid=0) = %d, want 0", uid)
	}
	_, err = runner.NewConfigSpecBuilder(runner.SpecDefaults{
		Image:       "img",
		CheckoutDir: "/workspace",
		HomeDir:     "/home/agent",
		UID:         uid,
		NamePrefix:  runner.AgentContainerNamePrefix,
	})
	if err == nil {
		t.Fatal("NewConfigSpecBuilder with a root uid err = nil, want a startup refusal")
	}
	if !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("error %q is not the non-root refusal", err)
	}
}

// TestHostDerivedUIDAcceptedByProvisionPath is the end-to-end regression this
// task exists to prevent: the uid the Runner derives for the host tier is
// exactly the uid the backend's AsUser rule accepts, so every provision-path
// exec (which passes Workspace.UID as AsUser) is accepted rather than rejected.
// A real HostRuntime captures os.Geteuid at construction; deriving through the
// same source ties the two together.
//
// This box's euid is not asserted to differ from AgentUID, so it does not
// discriminate the "euid != 1000" regression on a 1000-box — that acceptance
// needs a box whose euid is not 1000 (see the report). What it proves here is
// the contract closure: derived uid == the uid checkUser trusts.
func TestHostDerivedUIDAcceptedByProvisionPath(t *testing.T) {
	host := runtime.NewHostRuntime(t.TempDir())
	uid, err := specDefaultsUID(host, os.Geteuid)
	if err != nil {
		t.Fatalf("specDefaultsUID(host) err = %v", err)
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
