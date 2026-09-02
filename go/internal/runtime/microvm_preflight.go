//go:build unix

package runtime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // microVM preflight: LookPath + fixed-arg --version probes of the VMM userspace trio
	"path/filepath"
	"strings"

	"github.com/RigelBuild/compass/go/internal/hostcheck"
	"github.com/RigelBuild/compass/go/internal/runtime/microvm"
)

// preflightProbes is the effectful seam VerifyMicroVMSupport drives its host
// checks through, so every failure axis is hermetically unit-testable without a
// real /dev/kvm, PATH binary, or filesystem — the parsePodmanVersion testability
// split generalized (podman.go). defaultPreflightProbes wires the real
// implementations; tests inject fakes.
type preflightProbes struct {
	// openKVM opens /dev/kvm R/W and returns the open error (nil = openable).
	openKVM func() error
	// lookPath resolves a binary on PATH, returning its path or a not-found error.
	lookPath func(string) (string, error)
	// version runs `<path> --version` and returns its combined output.
	version func(ctx context.Context, path string) (string, error)
	// statImage opens the image path for reading (presence + readability).
	statImage func(string) error
	// hashImage returns the streaming lowercase-hex SHA-256 of the image file.
	hashImage func(string) (string, error)
}

// defaultPreflightProbes wires the real host-facing implementations behind the
// preflight seam.
func defaultPreflightProbes() preflightProbes {
	return preflightProbes{
		openKVM:  hostcheck.ProbeKVM,
		lookPath: exec.LookPath,
		version: func(ctx context.Context, path string) (string, error) {
			out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
			return string(out), err
		},
		statImage: func(path string) error {
			f, err := os.Open(path) //nolint:gosec // G304: operator-supplied guest-image path is the intended input
			if err != nil {
				return err
			}
			// Readability probe only; the boot path opens its own handle. A
			// close error on a file we only opened to test access is not
			// actionable.
			_ = f.Close()
			return nil
		},
		hashImage: hashFileSHA256,
	}
}

// VerifyMicroVMSupport is the static startup preflight for the microVM backend:
// it refuses to start when the host cannot run microVMs, naming the missing
// capability and the fix (D3 — no fallback, no degrade). Checks run in order and
// the FIRST failure returns; all-green returns nil.
func (m *MicroVMRuntime) VerifyMicroVMSupport(ctx context.Context) error {
	return m.verifyMicroVMSupport(ctx, defaultPreflightProbes())
}

// verifyMicroVMSupport is the seam-injected core of VerifyMicroVMSupport, so
// every failure axis is unit-testable with fake probes.
func (m *MicroVMRuntime) verifyMicroVMSupport(ctx context.Context, probes preflightProbes) error {
	// 1. KVM: the host must expose /dev/kvm openable by the Runner uid.
	if kvm := hostcheck.DecideKVM(probes.openKVM()); !kvm.OK {
		return fmt.Errorf("microvm preflight: %s — needs KVM: use the managed service or run on a KVM-capable host", kvm.Detail)
	}

	// 2. PATH trio at floors: the VMM userspace binaries the boot path resolves
	// from PATH, each at/above its devenv.lock pin.
	for _, floor := range hostcheck.MicroVMFloors {
		path, lookErr := probes.lookPath(floor.Binary)
		var output string
		var runErr error
		if lookErr == nil {
			output, runErr = probes.version(ctx, path)
		}
		if v := hostcheck.DecideVersion(floor, lookErr, runErr, output); !v.OK {
			return fmt.Errorf("microvm preflight: %s (install %s %s or newer, e.g. via the pinned dev shell)", v.Detail, floor.Binary, floor.Display)
		}
	}

	// 3. Guest images: each configured, present, readable, and hash-verified
	// against the manifest when one is supplied (record §(d)).
	if err := m.verifyImages(probes); err != nil {
		return err
	}

	// 4. RunRoot: set, writable, and short enough that a session's worst-case
	// suffixed gateway socket path fits the AF_UNIX budget (record §(b)/§(e)).
	return m.verifyRunRoot(probes)
}

// imageKnob names the flag and env knob that sets one guest image path, for the
// D3 error naming the missing capability and the fix.
type imageKnob struct {
	name string
	path string
	flag string
	env  string
}

// verifyImages checks each configured guest image is set, present, and readable,
// then — when a manifest is supplied — hash-verifies it against the manifest
// entry keyed by basename. An unset manifest logs one warning naming the
// un-verified state (record §(d)).
func (m *MicroVMRuntime) verifyImages(probes preflightProbes) error {
	images := []imageKnob{
		{name: "kernel", path: m.config.KernelImage, flag: "--microvm-kernel", env: "COMPASS_MICROVM_KERNEL"},
		{name: "rootfs", path: m.config.RootfsImage, flag: "--microvm-rootfs", env: "COMPASS_MICROVM_ROOTFS"},
		{name: "initrd", path: m.config.InitrdImage, flag: "--microvm-initrd", env: "COMPASS_MICROVM_INITRD"},
	}
	for _, img := range images {
		if img.path == "" {
			return fmt.Errorf("microvm preflight: guest %s image is not configured: set %s or %s", img.name, img.flag, img.env)
		}
		if err := probes.statImage(img.path); err != nil {
			return fmt.Errorf("microvm preflight: guest %s image %q is not present/readable: %w", img.name, img.path, err)
		}
	}

	if m.config.ImageManifest == "" {
		slog.Warn("microvm preflight: guest images are not hash-verified; set --microvm-image-manifest ($COMPASS_MICROVM_IMAGE_MANIFEST) to enable content verification")
		return nil
	}

	manifest, err := parseManifest(m.config.ImageManifest)
	if err != nil {
		return fmt.Errorf("microvm preflight: reading image manifest %q: %w", m.config.ImageManifest, err)
	}
	for _, img := range images {
		want, ok := manifest[filepath.Base(img.path)]
		if !ok {
			return fmt.Errorf("microvm preflight: guest %s image %q is absent from the manifest %q: verify --microvm-image-manifest matches the configured images", img.name, img.path, m.config.ImageManifest)
		}
		got, err := probes.hashImage(img.path)
		if err != nil {
			return fmt.Errorf("microvm preflight: hashing guest %s image %q: %w", img.name, img.path, err)
		}
		if got != want {
			return fmt.Errorf("microvm preflight: guest %s image %q digest mismatch: expected %s, got %s (verify the image against --microvm-image-manifest)", img.name, img.path, want, got)
		}
	}
	return nil
}

// verifyRunRoot checks RunRoot is set, creatable/writable, and short enough that
// the worst-case suffixed gateway socket path fits the AF_UNIX budget — the same
// sunPathMax comparison Create performs per-session (microvm_lifecycle.go), run
// once at startup against a worst-case (32-hex) session id.
func (m *MicroVMRuntime) verifyRunRoot(_ preflightProbes) error {
	if m.config.RunRoot == "" {
		return errors.New("microvm preflight: run-root is not configured: set --microvm-runroot or $COMPASS_MICROVM_RUNROOT")
	}
	probeDir := filepath.Join(m.config.RunRoot, "microvm", ".preflight")
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		return fmt.Errorf("microvm preflight: run-root %q is not creatable/writable: %w", m.config.RunRoot, err)
	}
	// Best-effort cleanup of the probe dir; a removal failure here does not
	// affect the writability verdict already established by MkdirAll.
	_ = os.RemoveAll(probeDir)

	// Worst case = the fully-suffixed gateway path for a max-length session id:
	// <RunRoot>/microvm/<32-hex>/vsock.sock, then GatewaySocketPath appends
	// "_<gateway port>" (mirrors microvm_lifecycle.go's Create-time guard).
	const idLen = 2 * 16 // mintSessionID: hex of 16 random bytes = 32 chars
	vsockSocket := filepath.Join(m.config.RunRoot, "microvm", strings.Repeat("a", idLen), "vsock.sock")
	gatewayPath := microvm.GatewaySocketPath(vsockSocket, agentGatewayVsockPort)
	if len(gatewayPath) > sunPathMax {
		return fmt.Errorf("microvm preflight: worst-case gateway socket path is %d bytes, over the %d-byte AF_UNIX limit: shorten the Runner's --run-root", len(gatewayPath), sunPathMax)
	}
	return nil
}

// hashFileSHA256 returns the streaming lowercase-hex SHA-256 of the file at path.
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied guest-image path is the intended input
	if err != nil {
		return "", err
	}
	defer func() {
		// Read-only handle; a close error after a successful hash is not
		// actionable and cannot corrupt the already-computed digest.
		_ = f.Close()
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseManifest reads a sha256sum-format manifest into a basename→digest map.
// Lines are `<hex>␠␠<basename>` (two spaces) or `<hex>␠<basename>`; blank lines
// are tolerated.
func parseManifest(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied manifest path is the intended input
	if err != nil {
		return nil, err
	}
	defer func() {
		// Read-only handle; a close error after a full read is not actionable.
		_ = f.Close()
	}()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		digest, name, found := strings.Cut(line, " ")
		if !found {
			return nil, fmt.Errorf("malformed manifest line %q: want `<hex digest>  <basename>`", line)
		}
		// A second leading space (sha256sum's binary-mode marker or its two-space
		// separator) leaves the name starting with a space; trim it.
		out[strings.TrimSpace(name)] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
