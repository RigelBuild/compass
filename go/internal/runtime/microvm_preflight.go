//go:build unix

package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
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
	"time"

	"github.com/RigelBuild/compass/go/internal/agentuid"
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
	// readQuota reads the active project quota scoping a volume path (D7:
	// read-only, never assign). Behind the seam so the quota axis is
	// hermetically testable — a prjquota-active filesystem cannot be
	// provisioned rootless, so a fake reading is the only way this decision is
	// covered on a dev box.
	readQuota quotaReadFn
	// verifySubordinateIDs resolves the invoking user's /etc/subuid range, the
	// host allocation newuidmap validates virtiofsd's uid/gid mapping against.
	// Behind the seam so the axis is testable on a box whose own subuid file
	// cannot be arranged to fail.
	verifySubordinateIDs func() error
}

// defaultPreflightProbes wires the real host-facing implementations behind the
// preflight seam.
func defaultPreflightProbes() preflightProbes {
	return preflightProbes{
		openKVM:  hostcheck.ProbeKVM,
		lookPath: exec.LookPath,
		version: func(ctx context.Context, path string) (string, error) {
			// CombinedOutput (not Output): a preflight must not miss a --version
			// a tool prints to stderr. hostcheck.FirstLine takes the first line,
			// and the trio all print --version as their first stdout line, so
			// the shared DecideVersion agrees with compass-stack's install-time
			// gate (which uses Output) for a well-behaved trio; capturing stderr
			// here only widens what this startup gate can catch.
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
		hashImage:            hashFileSHA256,
		readQuota:            readVolumeQuota,
		verifySubordinateIDs: microvm.VerifySubordinateIDRange,
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
	if err := m.verifyRunRoot(probes); err != nil {
		return err
	}

	// 5. Subordinate id range: virtiofsd's uid/gid mapping is validated by
	// newuidmap against the invoking user's /etc/subuid entry, so a host with
	// no subordinate range must fail HERE with the fix named — not at the first
	// session boot, where virtiofsd dies before binding its socket and the
	// cause surfaces only as "waiting for daemon sockets".
	if err := probes.verifySubordinateIDs(); err != nil {
		return fmt.Errorf("microvm preflight: %w", err)
	}

	// 6. Session-volume quota (D7): under the multi-tenant profile an
	// operator-provisioned project quota MUST be active on the session-volume
	// filesystem, or startup fails naming the fix. Otherwise the observed
	// utilization is logged and nothing gates.
	return m.verifyQuota(probes)
}

// verifyQuota runs the D7 volume-quota check against the SESSION-VOLUME
// filesystem, which is VolumeRoot — the parent dir P2's volume lifecycle mints
// per-session volumes under. It is deliberately NOT the RunRoot: that is the
// socket dir, held to a short /tmp path by the AF_UNIX sun_path budget, so a
// verdict read there is routinely about a different filesystem than the one
// sessions consume. Verification is read-only and rootless (microvm_quota.go);
// the Runner never assigns a quota.
//
// FAIL CLOSED when QuotaRequired is set and VolumeRoot is unknown: reporting a
// verdict about a filesystem that was never probed is worse than refusing, since
// it passes the multi-tenant gate on evidence from an unrelated mount. With
// QuotaRequired unset (Dogfood, single trusted tenant) an unknown volume root is
// logged and startup proceeds — an absent quota is the documented posture there.
//
// No meter is registered here; the coherent metric set is V7's.
func (m *MicroVMRuntime) verifyQuota(probes preflightProbes) error {
	if m.config.VolumeRoot == "" {
		if m.config.QuotaRequired {
			return errors.New(
				"microvm preflight: session-volume quota is required, but the session-volume filesystem " +
					"cannot be identified at startup; set --microvm-volume-root or $COMPASS_MICROVM_VOLUME_ROOT " +
					"to the parent dir session volumes are minted under (the run-root is the socket dir and is " +
					"routinely a different filesystem, so it is not a valid proxy)")
		}
		slog.Warn("microvm preflight: session-volume quota is not verified; no volume root is configured",
			"fix", "set --microvm-volume-root or $COMPASS_MICROVM_VOLUME_ROOT")
		return nil
	}
	reading, err := verifyVolumeQuota(m.config.VolumeRoot, VolumeQuota{}, probes.readQuota)
	if err != nil {
		if m.config.QuotaRequired {
			return err
		}
		slog.Warn("microvm preflight: session-volume quota is not verified; the single-tenant profile ships no host-enforced quota",
			"volume_root", m.config.VolumeRoot, "reason", err)
		return nil
	}
	// used_ratio is logged ONLY on an active bound, where its denominator is the
	// project's own limit and its numerator the project's own usage. With no
	// quota projected, statfs reports the whole filesystem, so the same
	// expression would silently mean "how full is the host disk" — a different
	// number under one key. The raw pair is logged instead, so V7 inherits a
	// single-meaning ratio (microvm_quota_linux.go on the dual meaning).
	slog.Info("microvm preflight: session-volume project quota is active",
		"volume_root", m.config.VolumeRoot,
		"limit_bytes", reading.LimitBytes,
		"used_bytes", reading.UsedBytes,
		"used_ratio", reading.UsedRatio(),
		"required", m.config.QuotaRequired)
	return nil
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
		if !strings.EqualFold(got, want) {
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
		return fmt.Errorf("microvm preflight: worst-case gateway socket path is %d bytes, over the %d-byte AF_UNIX limit: shorten --microvm-runroot or $COMPASS_MICROVM_RUNROOT", len(gatewayPath), sunPathMax)
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
// Lines are `<hex>␠␠<basename>` (text mode, two spaces) or `<hex>␠*<basename>`
// (binary mode, one space then a `*` marker); blank lines are tolerated.
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
		// Cut on the first space, then strip any leftover separator noise: text
		// mode leaves a second leading space, binary mode leaves a leading `*`
		// marker (`sha256sum -b`). Normalize both away to key on the bare
		// basename.
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		out[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// canaryDeadline bounds a whole BootCanary call end to end when the caller's ctx
// carries no deadline of its own: boot (≤60s worst case, bootDeadline) plus
// headroom for provision, one echo exec, and the severed teardown (record §(f)).
const canaryDeadline = 90 * time.Second

// canaryTeardownGrace is the fresh short grace the severed teardown ctx carries:
// the canary's own bounded ctx may already have expired (a mid-boot timeout), so
// teardown runs under a WithoutCancel copy of it so the VM is torn down cleanly
// instead of against an already-dead ctx (record §(f)). NB: this bounds Remove
// only if/when Remove honors its ctx deadline — today Remove is deadline-agnostic
// (vm.Shutdown re-strips cancellation and bounds itself with its own reapGrace
// timer, os.RemoveAll ignores ctx), so the grace is not yet an enforced ceiling.
const canaryTeardownGrace = 30 * time.Second

// canaryNamePrefix is the reserved name prefix every canary session carries. It
// sits outside runner.AgentContainerNamePrefix ("compass-agent-") so a canary
// can never collide with a real agent session in the table (record §(e)).
const canaryNamePrefix = "compass-canary-"

// CanaryReport is the measurement a successful BootCanary produces: the boot
// latency the boot chain took to reach ready, and the guest's memory footprint.
type CanaryReport struct {
	// BootLatency is the wall time of the canary's Start call — the full
	// Launch→Health-OK→Provision "time to ready" window a real session waits
	// (record §(e); mirrors TestMicroVMQBudget's Start timing).
	BootLatency time.Duration
	// GuestRSSBytes is the summed proportional-set-size (PSS) of the VM's
	// host-side processes (VMM/virtiofsd/passt), converted from the kB
	// smaps_rollup reports to bytes. PSS, not RSS: guest RAM is one shared
	// mapping and PSS divides shared pages among mappers, so it is the honest
	// per-VM share (record §(e)/OQ-10, launch.go PSS). The value undercounts by
	// the passt share on a healthy host (passt sets PR_SET_DUMPABLE=0, so its
	// smaps_rollup Pss is unreadable and drops out of the sum), so treat it as a
	// vmm+virtiofsd-dominated lower bound, not an exact footprint. Zero when PSS
	// is wholly unreadable — the canary's gate is the boot chain, RSS is
	// telemetry, so a PSS read error is reported, never fatal.
	GuestRSSBytes int64
}

// BootCanary is the dynamic startup preflight for the microVM backend: it really
// boots a throwaway canary VM through the backend's OWN lifecycle verbs
// (Create→Start→Exec→Remove), proving the whole chain — KVM, vsock, image,
// guest supervisor, exec gate — not just binary presence, and returns the boot
// latency + guest PSS the observability surface consumes (record §(e)). It owns
// the VM's entire lifetime inside the call, so it derives a bounded ctx spanning
// boot→teardown from the caller's ctx (the caller's deadline when present, else
// canaryDeadline) — the ctx-lifetime footgun guards VMs that OUTLIVE the bound,
// which this VM structurally cannot (record §(f)). Teardown is severed from that
// deadline so a mid-boot timeout still tears the VM down; the teardown and the
// throwaway-workspace cleanup errors are joined into the return, never discarded.
func (m *MicroVMRuntime) BootCanary(ctx context.Context) (report CanaryReport, err error) {
	// Derive the bound only when the caller carries none: under a caller
	// deadline the whole canary honors it as-is (mirrors bootPollContext).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, canaryDeadline)
		defer cancel()
	}

	name, err := canaryName()
	if err != nil {
		return CanaryReport{}, err
	}

	// A NON-empty mount set is required: an empty set leaves FSSharedDir empty,
	// yet Launch always runs virtiofsd, and the guest health gate blocks forever
	// on workspace_mounted (record §(e)). A freshly-minted throwaway dir is the
	// real virtio-fs share a session boots.
	workspace, err := os.MkdirTemp("", canaryNamePrefix)
	if err != nil {
		return CanaryReport{}, fmt.Errorf("microvm: canary: creating throwaway workspace: %w", err)
	}
	// Registered before the Remove defer so it runs AFTER teardown (LIFO): the
	// VM is gone before its backing share is deleted. Joins into the return.
	defer func() {
		if rmErr := os.RemoveAll(workspace); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("microvm: canary: removing throwaway workspace %s: %w", workspace, rmErr))
		}
	}()

	id, err := m.Create(ctx, ContainerSpec{
		Name:   name,
		UID:    agentuid.AgentUID,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: workspaceMountPath}},
	})
	if err != nil {
		return CanaryReport{}, fmt.Errorf("microvm: canary: creating session: %w", err)
	}
	// Remove ALWAYS runs — even when a later step errors — under a ctx severed
	// from the canary deadline (record §(f)), so a timed-out boot still tears
	// down. Its error is joined, never discarded (mirrors Remove's own
	// errors.Join, microvm_lifecycle.go).
	defer func() {
		teardownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), canaryTeardownGrace)
		defer cancel()
		if rmErr := m.Remove(teardownCtx, id); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("microvm: canary: removing session %s: %w", id, rmErr))
		}
	}()

	start := time.Now()
	if startErr := m.Start(ctx, id); startErr != nil {
		return CanaryReport{}, fmt.Errorf("microvm: canary: starting session: %w", startErr)
	}
	report.BootLatency = time.Since(start)

	nonce, err := canaryNonce()
	if err != nil {
		return CanaryReport{}, err
	}
	out, err := m.Exec(ctx, id, NewExecSpec("echo", nonce))
	if err != nil {
		return CanaryReport{}, fmt.Errorf("microvm: canary: echo exec: %w", err)
	}
	if out.ExitCode != 0 {
		return CanaryReport{}, fmt.Errorf("microvm: canary: echo exec exited %d, want 0 (stderr: %q)", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(strings.TrimSpace(out.Stdout), nonce) {
		return CanaryReport{}, fmt.Errorf("microvm: canary: echo output %q does not contain the nonce", out.Stdout)
	}

	session, err := m.session(id)
	if err != nil {
		return CanaryReport{}, fmt.Errorf("microvm: canary: resolving session for PSS: %w", err)
	}
	// PSS is best-effort telemetry, not the boot gate (record §(e)): a read
	// error (or a sandboxed helper with no readable smaps_rollup) leaves
	// GuestRSSBytes at 0, logged, never failing the canary.
	m.mu.Lock()
	vm := session.vm
	m.mu.Unlock()
	if vm != nil {
		pss, pssErr := vm.PSS()
		if pssErr != nil {
			slog.Warn("microvm: canary: reading guest PSS (best-effort)", "error", pssErr)
		}
		for _, kb := range pss {
			report.GuestRSSBytes += kb * 1024
		}
	}
	return report, nil
}

// canaryName mints a reserved canary session name: canaryNamePrefix plus 8 hex
// (4 random bytes), outside the agent-session prefix so it cannot collide with a
// real session (record §(e)).
func canaryName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("microvm: canary: minting name suffix: %w", err)
	}
	return canaryNamePrefix + hex.EncodeToString(b[:]), nil
}

// canaryNonce mints a fresh random hex token the echo exec must round-trip on
// stdout, proving the guest exec path really ran (record §(e)).
func canaryNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("microvm: canary: minting echo nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
