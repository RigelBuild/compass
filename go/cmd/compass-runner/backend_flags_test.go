//go:build unix

package main

// The operator-knob wiring for the microVM backend. Every knob here is dead
// code unless it reaches MicroVMConfig, and a knob that silently does not is the
// worst kind of gap: QuotaRequired was declared on the config, consumed by the
// D7 preflight, and unsettable by any operator — so the multi-tenant gate could
// never fire, on any profile. These tests assert BOTH resolution paths (the flag
// and the environment fallback) for the quota knobs, so that failure mode
// cannot recur silently.

import (
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// flagsFor builds a backendFlags whose pointers address local values, so a test
// sets a "flag" without touching the global flag set (which flag.Parse would
// own and which cannot be re-registered across tests).
func flagsFor() (backendFlags, *bool, *string) {
	var (
		empty                                                     string
		zero                                                      int
		backend, vmm, virtiofsd, kernel, rootfs, initrd, manifest = empty, empty, empty, empty, empty, empty, empty
		runRoot, volumeRoot                                       = empty, empty
		cpus, memoryMB                                            = zero, zero
		quotaRequired                                             bool
	)
	return backendFlags{
		backend:       &backend,
		vmm:           &vmm,
		virtiofsd:     &virtiofsd,
		kernel:        &kernel,
		rootfs:        &rootfs,
		initrd:        &initrd,
		imageManifest: &manifest,
		runRoot:       &runRoot,
		volumeRoot:    &volumeRoot,
		cpus:          &cpus,
		memoryMB:      &memoryMB,
		quotaRequired: &quotaRequired,
	}, &quotaRequired, &volumeRoot
}

// TestQuotaRequiredReachesConfigFromFlag: --microvm-quota-required must land on
// MicroVMConfig.QuotaRequired. Without this the D7 preflight gate is
// unreachable dead code and every Runner runs fail-open.
func TestQuotaRequiredReachesConfigFromFlag(t *testing.T) {
	f, quotaRequired, _ := flagsFor()
	*quotaRequired = true

	cfg, err := f.backendConfig()
	if err != nil {
		t.Fatalf("backendConfig() = %v, want nil", err)
	}
	if !cfg.MicroVM.QuotaRequired {
		t.Fatal("--microvm-quota-required did not reach MicroVMConfig.QuotaRequired; " +
			"the D7 multi-tenant quota gate would be unsettable and could never fire")
	}
}

// TestQuotaRequiredReachesConfigFromEnv: the documented
// $COMPASS_MICROVM_QUOTA_REQUIRED fallback must resolve too — it is how the
// managed profile sets the knob, since it ships env, not argv.
func TestQuotaRequiredReachesConfigFromEnv(t *testing.T) {
	for _, raw := range []string{"true", "1", "TRUE"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("COMPASS_MICROVM_QUOTA_REQUIRED", raw)
			f, _, _ := flagsFor()

			cfg, err := f.backendConfig()
			if err != nil {
				t.Fatalf("backendConfig() = %v, want nil", err)
			}
			if !cfg.MicroVM.QuotaRequired {
				t.Fatalf("$COMPASS_MICROVM_QUOTA_REQUIRED=%q did not reach MicroVMConfig.QuotaRequired", raw)
			}
		})
	}
}

// TestQuotaRequiredDefaultsOff pins the transitional default: neither knob set
// means the Dogfood single-tenant posture, where an absent quota is documented
// and logged rather than fatal.
func TestQuotaRequiredDefaultsOff(t *testing.T) {
	t.Setenv("COMPASS_MICROVM_QUOTA_REQUIRED", "")
	f, _, _ := flagsFor()

	cfg, err := f.backendConfig()
	if err != nil {
		t.Fatalf("backendConfig() = %v, want nil", err)
	}
	if cfg.MicroVM.QuotaRequired {
		t.Fatal("QuotaRequired defaulted to true with no knob set; the transitional default is the single-tenant posture")
	}
}

// TestQuotaRequiredEnvRefusesGarbage: an unparseable value must REFUSE at
// startup naming the variable, never read as false. Silently defaulting a
// misspelled `=yes` to off is exactly the fail-open this knob exists to close.
func TestQuotaRequiredEnvRefusesGarbage(t *testing.T) {
	t.Setenv("COMPASS_MICROVM_QUOTA_REQUIRED", "yes-please")
	f, _, _ := flagsFor()

	_, err := f.backendConfig()
	if err == nil {
		t.Fatal("an unparseable $COMPASS_MICROVM_QUOTA_REQUIRED = nil error; a typo must refuse at startup, not read as false")
	}
	for _, part := range []string{"COMPASS_MICROVM_QUOTA_REQUIRED", "yes-please"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not name %q", err.Error(), part)
		}
	}
}

// TestQuotaRequiredFlagWinsOverEnv pins the precedence every other knob has
// (orEnv/intOrEnv): an explicitly-passed flag beats the environment fallback.
func TestQuotaRequiredFlagWinsOverEnv(t *testing.T) {
	t.Setenv("COMPASS_MICROVM_QUOTA_REQUIRED", "false")
	f, quotaRequired, _ := flagsFor()
	*quotaRequired = true

	cfg, err := f.backendConfig()
	if err != nil {
		t.Fatalf("backendConfig() = %v, want nil", err)
	}
	if !cfg.MicroVM.QuotaRequired {
		t.Fatal("--microvm-quota-required=true lost to $COMPASS_MICROVM_QUOTA_REQUIRED=false; the flag must win")
	}
}

// TestVolumeRootReachesConfig: --microvm-volume-root / its env fallback must
// land on MicroVMConfig.VolumeRoot. It is what makes the quota probe target the
// session-volume filesystem instead of the RunRoot socket dir, so an unwired
// knob would leave the preflight either fail-closed forever or (worse) reporting
// a verdict about the wrong mount.
func TestVolumeRootReachesConfig(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		f, _, volumeRoot := flagsFor()
		*volumeRoot = "/srv/compass/volumes"

		cfg, err := f.backendConfig()
		if err != nil {
			t.Fatalf("backendConfig() = %v, want nil", err)
		}
		if cfg.MicroVM.VolumeRoot != "/srv/compass/volumes" {
			t.Fatalf("VolumeRoot = %q, want the flag value", cfg.MicroVM.VolumeRoot)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("COMPASS_MICROVM_VOLUME_ROOT", "/mnt/volumes")
		f, _, _ := flagsFor()

		cfg, err := f.backendConfig()
		if err != nil {
			t.Fatalf("backendConfig() = %v, want nil", err)
		}
		if cfg.MicroVM.VolumeRoot != "/mnt/volumes" {
			t.Fatalf("VolumeRoot = %q, want the env value", cfg.MicroVM.VolumeRoot)
		}
	})
}

// TestVolumeRootIsNotTheRunRoot: the two knobs must resolve INDEPENDENTLY. A
// wiring that aliased VolumeRoot to RunRoot would reintroduce the exact bug the
// field exists to fix — a quota verdict read on the short /tmp socket dir rather
// than the durable session-volume filesystem.
func TestVolumeRootIsNotTheRunRoot(t *testing.T) {
	t.Setenv("COMPASS_MICROVM_RUNROOT", "/tmp/cvm")
	t.Setenv("COMPASS_MICROVM_VOLUME_ROOT", "/srv/compass/volumes")
	f, _, _ := flagsFor()

	cfg, err := f.backendConfig()
	if err != nil {
		t.Fatalf("backendConfig() = %v, want nil", err)
	}
	if cfg.MicroVM.RunRoot != "/tmp/cvm" {
		t.Errorf("RunRoot = %q, want /tmp/cvm", cfg.MicroVM.RunRoot)
	}
	if cfg.MicroVM.VolumeRoot != "/srv/compass/volumes" {
		t.Errorf("VolumeRoot = %q, want /srv/compass/volumes", cfg.MicroVM.VolumeRoot)
	}
}

// TestBackendConfigSelectsMicroVM is the end-to-end shape: the resolved config
// still drives backend selection, so the backendConfig split did not detach the
// knobs from the engine they configure.
func TestBackendConfigSelectsMicroVM(t *testing.T) {
	f, quotaRequired, volumeRoot := flagsFor()
	*f.backend = "microvm"
	*quotaRequired = true
	*volumeRoot = "/srv/compass/volumes"

	engine, err := f.selectEngine()
	if err != nil {
		t.Fatalf("selectEngine() = %v, want the microVM backend", err)
	}
	if _, ok := engine.(*runtime.MicroVMRuntime); !ok {
		t.Fatalf("selectEngine() = %T, want *runtime.MicroVMRuntime", engine)
	}
}
