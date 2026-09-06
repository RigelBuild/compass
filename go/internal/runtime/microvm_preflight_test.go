//go:build unix

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/hostcheck"
)

// okProbes returns a preflightProbes whose every axis passes: KVM opens, the
// trio resolves at floor, images stat + hash cleanly, and the volume quota
// reads as an active bound. Individual rows override the axis they exercise.
func okProbes() preflightProbes {
	return preflightProbes{
		openKVM:  func() error { return nil },
		lookPath: func(bin string) (string, error) { return "/usr/bin/" + bin, nil },
		version: func(_ context.Context, path string) (string, error) {
			base := filepath.Base(path)
			for _, f := range hostcheck.MicroVMFloors {
				if f.Binary == base {
					return base + " " + f.Display, nil
				}
			}
			return "", fmt.Errorf("unexpected version probe for %q", path)
		},
		statImage: func(string) error { return nil },
		hashImage: func(string) (string, error) { return "deadbeef", nil },
		// An active project quota by default (path totals below the mount
		// root's = the kernel's projection), so only a row that overrides this
		// axis exercises the D7 quota leg.
		readQuota: func(path string) (QuotaReading, error) {
			return QuotaReading{
				Path: path, MountRoot: "/",
				LimitBytes: 10 << 30, UsedBytes: 1 << 30,
				LimitInodes: 1 << 20, UsedInodes: 512,
				FilesystemBytes: 1 << 40, FilesystemInodes: 1 << 26,
			}, nil
		},
	}
}

// okConfig returns a MicroVMConfig with all images set and a short, writable
// RunRoot so only the axis a row overrides fails.
func okConfig(t *testing.T) MicroVMConfig {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	rootfs := filepath.Join(dir, "rootfs")
	initrd := filepath.Join(dir, "initrd")
	for _, p := range []string{kernel, rootfs, initrd} {
		if err := os.WriteFile(p, []byte("img"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	// A short RunRoot under /tmp keeps the worst-case suffixed path within the
	// AF_UNIX budget; the temp base's name embeds the test name, which can be
	// long, so use a dedicated short base.
	//nolint:usetesting // t.TempDir()'s path embeds the long test name, risking overflow of the sunPathMax budget this config must stay under; a short /tmp base is required.
	runRoot, err := os.MkdirTemp("", "rr-")
	if err != nil {
		t.Fatalf("creating run-root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	return MicroVMConfig{
		KernelImage: kernel,
		RootfsImage: rootfs,
		InitrdImage: initrd,
		RunRoot:     runRoot,
	}
}

// preflightRow is one verifyMicroVMSupport failure-axis case: mutate the
// all-green config/probes to break (or re-pose) exactly one axis, then assert
// the verdict. Shared by the capability table and the D7 quota table, which are
// separate functions only because one table covering every axis outgrows the
// funlen budget.
type preflightRow struct {
	name      string
	mutate    func(cfg *MicroVMConfig, p *preflightProbes)
	wantOK    bool
	wantParts []string
}

// runPreflightRows drives each row from the all-green baseline: an OK row must
// return nil, and a failing row must name every wantParts fragment so the D3
// "name the missing capability and the fix" contract is asserted, not just the
// existence of an error.
func runPreflightRows(t *testing.T, rows []preflightRow) {
	t.Helper()
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			cfg := okConfig(t)
			probes := okProbes()
			tt.mutate(&cfg, &probes)
			m := NewMicroVMRuntime(cfg)
			err := m.verifyMicroVMSupport(t.Context(), probes)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("verifyMicroVMSupport = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyMicroVMSupport = nil, want error mentioning %v", tt.wantParts)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err.Error(), part)
				}
			}
		})
	}
}

func TestVerifyMicroVMSupport(t *testing.T) {
	runPreflightRows(t, []preflightRow{
		{
			name:   "all green",
			mutate: func(_ *MicroVMConfig, _ *preflightProbes) {},
			wantOK: true,
		},
		{
			name: "kvm open error",
			mutate: func(_ *MicroVMConfig, p *preflightProbes) {
				p.openKVM = func() error { return errors.New("permission denied") }
			},
			wantParts: []string{"KVM"},
		},
		{
			name: "trio binary not found",
			mutate: func(_ *MicroVMConfig, p *preflightProbes) {
				p.lookPath = func(bin string) (string, error) {
					if bin == "virtiofsd" {
						return "", errors.New("not found in $PATH")
					}
					return "/usr/bin/" + bin, nil
				}
			},
			wantParts: []string{"virtiofsd", "1.14.0"},
		},
		{
			name: "trio binary below floor",
			mutate: func(_ *MicroVMConfig, p *preflightProbes) {
				p.version = func(_ context.Context, path string) (string, error) {
					if filepath.Base(path) == "cloud-hypervisor" {
						return "cloud-hypervisor v52.0.0", nil
					}
					return filepath.Base(path) + " 9999.0.0", nil
				}
			},
			wantParts: []string{"52.0.0", "53.0.0"},
		},
		{
			name: "kernel image unset",
			mutate: func(cfg *MicroVMConfig, _ *preflightProbes) {
				cfg.KernelImage = ""
			},
			wantParts: []string{"kernel", "--microvm-kernel", "COMPASS_MICROVM_KERNEL"},
		},
		{
			name: "rootfs image absent/unreadable",
			mutate: func(cfg *MicroVMConfig, p *preflightProbes) {
				p.statImage = func(path string) error {
					if path == cfg.RootfsImage {
						return errors.New("no such file")
					}
					return nil
				}
			},
			wantParts: []string{"rootfs"},
		},
		{
			name: "run-root unset",
			mutate: func(cfg *MicroVMConfig, _ *preflightProbes) {
				cfg.RunRoot = ""
			},
			wantParts: []string{"--microvm-runroot"},
		},
		{
			name: "run-root over budget",
			mutate: func(cfg *MicroVMConfig, _ *preflightProbes) {
				// Pad the RunRoot past the sunPathMax budget for the worst-case
				// suffixed path. Create the dir so writability passes and the
				// budget check is what fails.
				//nolint:usetesting // deliberately long path to exceed the sunPathMax budget under test.
				base, err := os.MkdirTemp("", "rr-long-")
				if err != nil {
					t.Fatalf("creating long run-root base: %v", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(base) })
				longDir := filepath.Join(base, strings.Repeat("p", sunPathMax))
				if err := os.MkdirAll(longDir, 0o700); err != nil {
					t.Fatalf("creating long run-root: %v", err)
				}
				cfg.RunRoot = longDir
			},
			wantParts: []string{"AF_UNIX", "--microvm-runroot"},
		},
		{
			name: "run-root not creatable/writable",
			mutate: func(cfg *MicroVMConfig, _ *preflightProbes) {
				// Point RunRoot at a path under a regular file so MkdirAll of
				// the probe dir fails with ENOTDIR — the writability axis with
				// no probe seam (verifyRunRoot calls os.MkdirAll directly).
				file := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
					t.Fatalf("writing blocking file: %v", err)
				}
				cfg.RunRoot = filepath.Join(file, "under-a-file")
			},
			wantParts: []string{"not creatable/writable"},
		},
	})
}

// TestVerifyMicroVMSupportQuota is the D7 session-volume quota axis: with
// QuotaRequired set, an active project quota passes and an absent one is a
// startup error naming the volume and the operator fix; with it unset (Dogfood's
// single trusted tenant) neither an absent quota nor an unreadable one gates.
// The unquota'd readings below are the real shape a plain filesystem produces —
// statfs at the path and at its mount root report the same totals, so nothing is
// projected.
func TestVerifyMicroVMSupportQuota(t *testing.T) {
	unquotad := func(path string) (QuotaReading, error) {
		return QuotaReading{
			Path: path, MountRoot: "/",
			LimitBytes: 1 << 40, UsedBytes: 1 << 30,
			FilesystemBytes: 1 << 40,
		}, nil
	}
	unreadable := func(string) (QuotaReading, error) {
		return QuotaReading{}, errors.New("statfs: permission denied")
	}

	runPreflightRows(t, []preflightRow{
		{
			name: "quota present and under limit, required, passes",
			mutate: func(cfg *MicroVMConfig, _ *preflightProbes) {
				cfg.QuotaRequired = true
			},
			wantOK: true,
		},
		{
			name: "quota absent and required fails naming the volume and the fix",
			mutate: func(cfg *MicroVMConfig, p *preflightProbes) {
				cfg.QuotaRequired = true
				p.readQuota = unquotad
			},
			wantParts: []string{"no enforced project quota", "prjquota", "quota-required off"},
		},
		{
			name: "quota absent and NOT required passes (Dogfood single tenant)",
			mutate: func(_ *MicroVMConfig, p *preflightProbes) {
				p.readQuota = unquotad
			},
			wantOK: true,
		},
		{
			name: "quota probe failure with required set fails with the read cause",
			mutate: func(cfg *MicroVMConfig, p *preflightProbes) {
				cfg.QuotaRequired = true
				p.readQuota = unreadable
			},
			wantParts: []string{"permission denied"},
		},
		{
			name: "quota probe failure without required set passes",
			mutate: func(_ *MicroVMConfig, p *preflightProbes) {
				p.readQuota = unreadable
			},
			wantOK: true,
		},
	})
}

// TestVerifyMicroVMSupportManifest covers the (d) hash-verification axis: a
// manifest set + all-match passes; a mismatch names the image and both digests;
// a configured image absent from the manifest is an error; an unset manifest
// passes presence-only (no error from the hash axis).
func TestVerifyMicroVMSupportManifest(t *testing.T) {
	// hashOf returns the real sha256 of a file so the manifest and the probe
	// agree when they should.
	digests := map[string]string{}
	writeImage := func(t *testing.T, dir, name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
		sum := sha256.Sum256([]byte(content))
		digests[name] = hex.EncodeToString(sum[:])
		return p
	}

	setup := func(t *testing.T) (MicroVMConfig, preflightProbes, string) {
		t.Helper()
		dir := t.TempDir()
		cfg := okConfig(t)
		cfg.KernelImage = writeImage(t, dir, "kernel.img", "kernel-content")
		cfg.RootfsImage = writeImage(t, dir, "rootfs.img", "rootfs-content")
		cfg.InitrdImage = writeImage(t, dir, "initrd.img", "initrd-content")
		probes := okProbes()
		probes.hashImage = hashFileSHA256
		return cfg, probes, dir
	}

	writeManifest := func(t *testing.T, dir string, entries map[string]string) string {
		t.Helper()
		var b strings.Builder
		for name, digest := range entries {
			b.WriteString(digest + "  " + name + "\n")
		}
		p := filepath.Join(dir, "manifest.sha256")
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("writing manifest: %v", err)
		}
		return p
	}

	t.Run("all match passes", func(t *testing.T) {
		cfg, probes, dir := setup(t)
		cfg.ImageManifest = writeManifest(t, dir, map[string]string{
			"kernel.img": digests["kernel.img"],
			"rootfs.img": digests["rootfs.img"],
			"initrd.img": digests["initrd.img"],
		})
		m := NewMicroVMRuntime(cfg)
		if err := m.verifyMicroVMSupport(t.Context(), probes); err != nil {
			t.Fatalf("verifyMicroVMSupport = %v, want nil", err)
		}
	})

	t.Run("uppercase manifest digest passes", func(t *testing.T) {
		cfg, probes, dir := setup(t)
		cfg.ImageManifest = writeManifest(t, dir, map[string]string{
			"kernel.img": strings.ToUpper(digests["kernel.img"]),
			"rootfs.img": strings.ToUpper(digests["rootfs.img"]),
			"initrd.img": strings.ToUpper(digests["initrd.img"]),
		})
		m := NewMicroVMRuntime(cfg)
		if err := m.verifyMicroVMSupport(t.Context(), probes); err != nil {
			t.Fatalf("verifyMicroVMSupport = %v, want nil (hex digest compare must be case-insensitive)", err)
		}
	})

	t.Run("mismatch names image and both digests", func(t *testing.T) {
		cfg, probes, dir := setup(t)
		bogus := strings.Repeat("0", 64)
		cfg.ImageManifest = writeManifest(t, dir, map[string]string{
			"kernel.img": digests["kernel.img"],
			"rootfs.img": bogus,
			"initrd.img": digests["initrd.img"],
		})
		m := NewMicroVMRuntime(cfg)
		err := m.verifyMicroVMSupport(t.Context(), probes)
		if err == nil {
			t.Fatal("verifyMicroVMSupport = nil, want mismatch error")
		}
		for _, part := range []string{"rootfs.img", bogus, digests["rootfs.img"]} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("error %q does not name %q", err.Error(), part)
			}
		}
	})

	t.Run("image absent from manifest is an error", func(t *testing.T) {
		cfg, probes, dir := setup(t)
		cfg.ImageManifest = writeManifest(t, dir, map[string]string{
			"kernel.img": digests["kernel.img"],
			"rootfs.img": digests["rootfs.img"],
			// initrd.img omitted
		})
		m := NewMicroVMRuntime(cfg)
		err := m.verifyMicroVMSupport(t.Context(), probes)
		if err == nil {
			t.Fatal("verifyMicroVMSupport = nil, want absent-from-manifest error")
		}
		if !strings.Contains(err.Error(), "initrd.img") {
			t.Errorf("error %q does not name the absent image", err.Error())
		}
	})

	t.Run("unset manifest passes presence-only", func(t *testing.T) {
		cfg, probes, _ := setup(t)
		cfg.ImageManifest = ""
		// A hashImage that would fail proves the hash axis is not exercised.
		probes.hashImage = func(string) (string, error) { return "", errors.New("must not be called") }
		m := NewMicroVMRuntime(cfg)
		if err := m.verifyMicroVMSupport(t.Context(), probes); err != nil {
			t.Fatalf("verifyMicroVMSupport = %v, want nil (unset manifest is presence-only)", err)
		}
	})
}

// TestParseManifest covers the sha256sum-format line shapes directly: text mode
// (two spaces), one-space, binary mode (`*` marker), blank-line tolerance, and a
// no-separator malformed line. The binary-mode row is the regression guard for
// the marker-stripping fix.
func TestParseManifest(t *testing.T) {
	const digest = "abc123"
	t.Run("line shapes key on bare basename", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manifest.sha256")
		content := "" +
			digest + "  text-mode.img\n" + // two spaces (GNU text mode)
			digest + " one-space.img\n" + // single space
			digest + " *binary-mode.img\n" + // binary mode `*` marker
			"\n" + // blank line, tolerated
			"   \n" // whitespace-only line, tolerated
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing manifest: %v", err)
		}
		got, err := parseManifest(path)
		if err != nil {
			t.Fatalf("parseManifest = %v, want nil", err)
		}
		want := map[string]string{
			"text-mode.img":   digest,
			"one-space.img":   digest,
			"binary-mode.img": digest,
		}
		if len(got) != len(want) {
			t.Fatalf("parseManifest = %v, want %v", got, want)
		}
		for name, wantDigest := range want {
			if got[name] != wantDigest {
				t.Errorf("key %q = %q, want %q", name, got[name], wantDigest)
			}
		}
	})

	t.Run("no-separator line is malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manifest.sha256")
		if err := os.WriteFile(path, []byte("nospacehere\n"), 0o600); err != nil {
			t.Fatalf("writing manifest: %v", err)
		}
		_, err := parseManifest(path)
		if err == nil {
			t.Fatal("parseManifest = nil, want malformed-line error")
		}
		if !strings.Contains(err.Error(), "malformed manifest line") {
			t.Errorf("error %q does not name the malformed line", err.Error())
		}
	})
}
