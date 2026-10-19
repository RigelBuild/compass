//go:build unix

// Command compass-runner is the Compass Runner entry point: the second binary.
// It dials OUT to the Server over gRPC with its per-Runner token, enrolls, and
// hosts per-agent containers — the Server never runs container-engine code. All
// seam logic lives in the internal/runner package; this binary is a thin wrapper
// over runner.Run that assembles config from flags/env and drives shutdown on a
// termination signal, mirroring cmd/compass-server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/otel/otelsetup"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// version is the Runner build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "compass-runner:", err)
		os.Exit(1)
	}
}

func run() error {
	runnerID := flag.String("runner-id", "",
		"This Runner's stable id, cross-checked against the token subject. Defaults to $COMPASS_RUNNER_ID.")
	tokenFile := flag.String("token-file", "",
		"Path to the projected Runner token. Defaults to $COMPASS_RUNNER_TOKEN_FILE.")
	serverAddr := flag.String("server", "",
		"The Server's base URL (e.g. https://server.example:443). Defaults to $COMPASS_SERVER_ADDR.")
	image := flag.String("image", "",
		"The container image every agent workstream runs. Defaults to $COMPASS_AGENT_IMAGE.")
	checkoutDir := flag.String("checkout-dir", "/workspace",
		"In-container checkout directory (the agent session's cwd).")
	homeDir := flag.String("home-dir", "/home/agent",
		"In-container scoped $HOME for the agent user.")
	agentModel := flag.String("agent-model", "",
		"Model selector handed to every agent this Runner starts (the agent's "+
			"COMPASS_MODEL). Empty leaves each agent on its own default. "+
			"Defaults to $COMPASS_AGENT_MODEL.")
	agentBatching := registerAgentBatchingFlag(flag.CommandLine)
	egressHosts := flag.String("egress-allow", "",
		"Comma-separated default-deny egress allowlist (DNS names or IP literals). "+
			"Agents that push with jj-vine need github.com and api.github.com.")
	runtimeDir := flag.String("runtime-dir", "/run/compass",
		"Runner-owned base dir for per-container agent sockets (RuntimeDir/containers/<container>/agent.sock).")
	serverName := registerServerNameFlag(flag.CommandLine)
	caPath := flag.String("ca", "",
		"PEM CA/certificate to trust for the Server's TLS network door, instead "+
			"of the system roots. Set this to the locally-generated self-signed "+
			"cert for the local dogfood path (where one cert is both the server's "+
			"--tls-cert and this trust anchor); leave unset when the Server is "+
			"behind a public CA. Defaults to $COMPASS_RUNNER_CA.")

	var mounts []runtime.Mount
	flag.Func("mount",
		"Repeatable host:container[:ro] bind mount into the agent container "+
			"(add ':ro' for read-only). Flag-only, no env fallback. A general "+
			"read-only bind-mount facility for host paths the agent needs (e.g. a "+
			"cache mounted read-only).",
		func(s string) error {
			m, err := parseMount(s)
			if err != nil {
				return err
			}
			mounts = append(mounts, m)
			return nil
		})
	backends := registerBackendFlags()
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	slog.SetDefault(slog.New(otel.NewTraceHandler(slog.NewTextHandler(os.Stderr, nil))))
	log := slog.Default()

	// The SELECTED engine's host-capability preflight (static check + boot canary)
	// runs ahead of every operator-input check: a refusal about a host fact must
	// precede telling an operator to fix a token, or they fix it, re-run, and only
	// then learn the engine can never launch. Select the engine, then gate on it.
	engine, err := backends.selectTracedEngine()
	if err != nil {
		return err
	}
	ctx, stop, err := preflightUnderSignals(context.Background(), engine)
	if err != nil {
		return err
	}
	defer stop()

	id := orEnv(*runnerID, "COMPASS_RUNNER_ID")
	addr := orEnv(*serverAddr, "COMPASS_SERVER_ADDR")
	if addr == "" {
		return errors.New("a server address is required: pass --server or set $COMPASS_SERVER_ADDR")
	}
	if err := validateAgentBatching(*agentBatching); err != nil {
		return err
	}
	tokenPath := orEnv(*tokenFile, "COMPASS_RUNNER_TOKEN_FILE")
	tokenSource, err := resolveTokenSource(os.Getenv("COMPASS_RUNNER_TOKEN"), tokenPath, log)
	if err != nil {
		return err
	}
	if id == "" && tokenPath == "" {
		return errors.New("a runner id is required: pass --runner-id or set $COMPASS_RUNNER_ID")
	}
	if err := checkMountsAgainstTokenFile(mounts, tokenPath); err != nil {
		return err
	}
	// The agent image is required by the container backends and unread by the
	// microVM backend, so the engine decides — resolved in newSpecBuilder.
	img := orEnv(*image, "COMPASS_AGENT_IMAGE")
	egress, err := parseEgress(*egressHosts)
	if err != nil {
		return err
	}

	httpClient, err := runnerHTTPClient(*caPath, *serverName)
	if err != nil {
		return err
	}
	specs, err := newSpecBuilder(ctx, engine, img, egress, *checkoutDir, *homeDir, mounts)
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "compass-runner starting",
		"version", version, "runner_id", id, "server", addr)

	// OTel emission (env-only, bounded flush on drain); see setupOtel.
	otelShutdown, err := setupOtel(ctx)
	if err != nil {
		return err
	}
	defer otelShutdown()

	return runner.Run(ctx, runner.RunnerConfig{
		RunnerID:      id,
		ServerAddr:    addr,
		Token:         tokenSource,
		Engine:        engine,
		RuntimeDir:    *runtimeDir,
		AgentModel:    orEnv(*agentModel, "COMPASS_AGENT_MODEL"),
		AgentBatching: resolveAgentBatchingConfig(*agentBatching).AgentBatching,
		HTTPClient:    httpClient,
	}, specs, log)
}

func registerServerNameFlag(flags *flag.FlagSet) *string {
	return flags.String("server-name", "",
		"Name to check on the Server certificate and send as SNI. Defaults to $COMPASS_SERVER_NAME.")
}

func runnerHTTPClient(caPath, serverName string) (connecthttp.HTTPClient, error) {
	ca := orEnv(caPath, "COMPASS_RUNNER_CA")
	name := orEnv(serverName, "COMPASS_SERVER_NAME")
	if ca == "" && name == "" {
		return http.DefaultClient, nil
	}
	return runner.NewServerClient(ca, name)
}

func registerAgentBatchingFlag(flags *flag.FlagSet) *string {
	return flags.String("agent-batching", "",
		"Idle inbound batching for every agent this Runner starts: \"on\" or \"off\" (default off). "+
			"Defaults to $COMPASS_AGENT_BATCHING.")
}

func resolveAgentBatchingConfig(flagValue string) runner.RunnerConfig {
	return runner.RunnerConfig{AgentBatching: orEnv(flagValue, "COMPASS_AGENT_BATCHING")}
}

// The agent acts only on "on", so any other non-empty value is a typo that would leave batching off silently.
func validateAgentBatching(flagValue string) error {
	v := resolveAgentBatchingConfig(flagValue).AgentBatching
	if v != "" && v != "on" && v != "off" {
		return fmt.Errorf("--agent-batching (or $COMPASS_AGENT_BATCHING) must be \"on\" or \"off\", got %q", v)
	}
	return nil
}

func resolveTokenSource(staticToken, tokenPath string, log *slog.Logger) (runner.TokenSource, error) {
	if staticToken != "" && tokenPath != "" {
		return nil, errors.New("runner token sources are mutually exclusive: use COMPASS_RUNNER_TOKEN or --token-file")
	}
	if staticToken != "" {
		return runner.StaticToken(staticToken), nil
	}
	if tokenPath != "" {
		return runner.NewFileToken(tokenPath, log, nil), nil
	}
	return nil, errors.New("a runner token is required: set $COMPASS_RUNNER_TOKEN or --token-file")
}

func checkMountsAgainstTokenFile(mounts []runtime.Mount, tokenPath string) error {
	if tokenPath == "" {
		return nil
	}
	absoluteTokenPath, err := absolutePathPreservingTraversal(tokenPath)
	if err != nil {
		return fmt.Errorf("resolving runner token directory: %w", err)
	}
	tokenDir, err := canonicalizePath(rawPathParent(absoluteTokenPath))
	if err != nil {
		return fmt.Errorf("resolving runner token directory: %w", err)
	}
	for _, mount := range mounts {
		hostPath, err := canonicalizePath(mount.HostPath)
		if err != nil {
			return fmt.Errorf("resolving --mount host path %q: %w", mount.HostPath, err)
		}
		if pathContains(tokenDir, hostPath) || pathContains(hostPath, tokenDir) {
			return fmt.Errorf("--mount host path %q overlaps runner token directory %q", mount.HostPath, tokenDir)
		}
	}
	return nil
}

func absolutePathPreservingTraversal(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd + string(filepath.Separator) + path, nil
}

func rawPathParent(path string) string {
	trimmed := strings.TrimRight(path, string(filepath.Separator))
	if trimmed == "" {
		return string(filepath.Separator)
	}
	separator := strings.LastIndexByte(trimmed, filepath.Separator)
	if separator <= 0 {
		return string(filepath.Separator)
	}
	return trimmed[:separator]
}

func canonicalizePath(path string) (string, error) {
	absolute, err := absolutePathPreservingTraversal(path)
	if err != nil {
		return "", err
	}
	root := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(absolute, root), root)
	resolved := root
	for index, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		canonical, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			resolved = filepath.Clean(canonical)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		tail := append([]string{part}, parts[index+1:]...)
		return filepath.Clean(filepath.Join(append([]string{resolved}, tail...)...)), nil
	}
	return filepath.Clean(resolved), nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// newSpecBuilder resolves per-backend workspace UIDs: host uses its euid, while
// containers keep the baked fleet constant. Backend policy and non-root checks
// live in internal/runner; this binary only wires operator inputs.
func newSpecBuilder(ctx context.Context, engine runtime.WorkloadRuntime, image string, egress runtime.EgressPolicy, checkoutDir, homeDir string, mounts []runtime.Mount) (runner.SpecBuilder, error) {
	uid, err := runner.ResolveWorkspaceUID(engine)
	if err != nil {
		return nil, err
	}
	egress, err = runner.ResolveEgress(engine, egress)
	if err != nil {
		return nil, err
	}
	image, imageIrrelevant, err := runner.ResolveAgentImage(engine, image)
	if err != nil {
		return nil, err
	}
	if err := verifyNoPastaForwards(ctx, engine, image); err != nil {
		return nil, err
	}
	return runner.NewConfigSpecBuilder(runner.SpecDefaults{
		Image:           image,
		ImageIrrelevant: imageIrrelevant,
		Egress:          egress,
		CheckoutDir:     checkoutDir,
		HomeDir:         homeDir,
		UID:             uid,
		NamePrefix:      runner.AgentContainerNamePrefix,
		Mounts:          mounts,
	})
}

// microVMPreflighter is the microVM backend's static host-capability probe:
// the host trio (VMM/virtiofsd binaries), KVM access, and guest-image presence.
type microVMPreflighter interface {
	VerifyMicroVMSupport(ctx context.Context) error
}

// podmanPreflighter is the podman backend's static host-capability probe:
// podman is new enough for the container userns remap.
type podmanPreflighter interface {
	VerifyUsernsRemapSupport(ctx context.Context) error
}

// appleContainerPreflighter is the apple-container backend's static
// host-capability probe: the `container` CLI is present and meets the version
// floor this backend's command contract relies on.
type appleContainerPreflighter interface {
	VerifyAppleContainerSupport(ctx context.Context) error
}

// pastaForwardVerifier is the podman backend's check that containers.conf opens
// no pasta port forward. It needs the agent image, so it runs after image resolve.
type pastaForwardVerifier interface {
	VerifyNoPastaForwards(ctx context.Context, image string) error
}

// verifyNoPastaForwards runs the pasta forward check on engines that have one.
func verifyNoPastaForwards(ctx context.Context, engine runtime.WorkloadRuntime, image string) error {
	v, ok := engine.(pastaForwardVerifier)
	if !ok {
		return nil
	}
	return v.VerifyNoPastaForwards(ctx, image)
}

// canaryBooter is the microVM backend's dynamic host-capability probe: it really
// boots a throwaway VM through the backend's own verbs, proving the whole boot
// chain. Kept a DISTINCT single-method interface from microVMPreflighter (not a
// widened two-method probe) so the single-method-probe discipline holds.
type canaryBooter interface {
	BootCanary(ctx context.Context) (runtime.CanaryReport, error)
}

// verifyBackendPreflight dispatches by concrete probe interface, checking
// microVM before Podman and Apple. A backend without a probe fails closed with
// its type named; startup validation must never be silently skipped.
func verifyBackendPreflight(ctx context.Context, engine runtime.WorkloadRuntime) error {
	switch e := engine.(type) {
	case microVMPreflighter:
		return runMicroVMPreflight(ctx, e, engine)
	case podmanPreflighter:
		return e.VerifyUsernsRemapSupport(ctx)
	case appleContainerPreflighter:
		return e.VerifyAppleContainerSupport(ctx)
	default:
		return fmt.Errorf("backend %T exposes no startup preflight probe", engine)
	}
}

// preflightUnderSignals installs the interrupt/SIGTERM ctx and runs the engine
// preflight under it, so an interrupt mid-canary cancels the boot and its deferred
// teardown removes the canary dirs. That interrupt exits non-zero with the
// cancel; a second signal during teardown is swallowed (SIGKILL aborts it).
func preflightUnderSignals(parent context.Context, engine runtime.WorkloadRuntime) (context.Context, context.CancelFunc, error) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	if err := verifyBackendPreflight(ctx, engine); err != nil {
		stop()
		return nil, nil, err
	}
	return ctx, stop, nil
}

// runMicroVMPreflight checks static support before booting a dynamic canary;
// absence of the canary probe fails closed. Split out to keep
// verifyBackendPreflight within funlen.
func runMicroVMPreflight(ctx context.Context, pre microVMPreflighter, engine runtime.WorkloadRuntime) error {
	if err := pre.VerifyMicroVMSupport(ctx); err != nil {
		return err
	}
	canary, ok := engine.(canaryBooter)
	if !ok {
		return fmt.Errorf("microVM backend %T exposes no boot canary probe", engine)
	}
	report, err := canary.BootCanary(ctx)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "microvm boot canary passed",
		"boot_latency", report.BootLatency,
		"guest_rss_bytes", report.GuestRSSBytes)
	return nil
}

// setupOtel installs env-gated tracer and meter providers with one shutdown.
// With no OTLP endpoint, the global providers are no-ops and the unconditionally
// mounted outbound interceptor remains inert.
func setupOtel(ctx context.Context) (shutdown func(), err error) {
	cfg := otelsetup.Config{
		ServiceName:    "compass-runner",
		ServiceVersion: version,
		Endpoint:       os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	tracerShutdown, err := otelsetup.SetupTracerProvider(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otel: tracer provider: %w", err)
	}
	meterShutdown, err := otelsetup.SetupMeterProvider(ctx, cfg)
	if err != nil {
		_ = tracerShutdown(ctx) // roll back the tracer provider we just installed; nothing actionable on its error here
		return nil, fmt.Errorf("otel: meter provider: %w", err)
	}
	// The drain ctx is canceled when shutdown runs; raw shutdown would drop its
	// final batch. Detach cancellation and start a 2s flush deadline at shutdown,
	// not process start.
	return func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = tracerShutdown(sctx) // best-effort flush on shutdown; export error not actionable at exit
		_ = meterShutdown(sctx)  // best-effort flush on shutdown; export error not actionable at exit
	}, nil
}

// backendFlags holds the runtime-backend selection flags, registered on the
// default flag set before flag.Parse and resolved into a runtime after it.
type backendFlags struct {
	backend, vmm, virtiofsd, kernel, rootfs, initrd, imageManifest, runRoot *string
	volumeRoot                                                              *string
	cpus, memoryMB                                                          *int
	quotaRequired                                                           *bool
}

// registerBackendFlags declares the backend-selection flags. Call before
// flag.Parse; resolve with selectEngine after.
func registerBackendFlags() backendFlags {
	return backendFlags{
		backend: flag.String("backend", "",
			"Container runtime backend: 'podman' (default; the permanent self-host entry tier), "+
				"'microvm' or 'apple-container'. Defaults to $COMPASS_RUNTIME_BACKEND."),
		vmm: flag.String("microvm-vmm", "",
			"Path to the microVM monitor binary (microvm backend). Defaults to $COMPASS_MICROVM_VMM."),
		virtiofsd: flag.String("microvm-virtiofsd", "",
			"Path to the virtiofs daemon binary (microvm backend). Defaults to $COMPASS_MICROVM_VIRTIOFSD."),
		kernel: flag.String("microvm-kernel", "",
			"Path to the guest kernel image (microvm backend). Defaults to $COMPASS_MICROVM_KERNEL."),
		rootfs: flag.String("microvm-rootfs", "",
			"Path to the guest rootfs image (microvm backend). Defaults to $COMPASS_MICROVM_ROOTFS."),
		initrd: flag.String("microvm-initrd", "",
			"Path to the guest initramfs image (microvm backend). Defaults to $COMPASS_MICROVM_INITRD."),
		imageManifest: flag.String("microvm-image-manifest", "",
			"Path to a sha256sum-format manifest of the guest images (microvm backend); "+
				"set enables hash-verification at preflight, unset checks presence only. "+
				"Defaults to $COMPASS_MICROVM_IMAGE_MANIFEST."),
		runRoot: flag.String("microvm-runroot", "",
			"Root dir for per-session microVM runtime dirs (microvm backend). Defaults to $COMPASS_MICROVM_RUNROOT."),
		cpus: flag.Int("microvm-cpus", 0,
			"Default vCPU count per session guest (microvm backend); 0 leaves the VMM default. "+
				"Defaults to $COMPASS_MICROVM_CPUS."),
		memoryMB: flag.Int("microvm-memory-mb", 0,
			"Default guest RAM in MiB per session (microvm backend); 0 leaves the VMM default. "+
				"Defaults to $COMPASS_MICROVM_MEMORY_MB."),
		volumeRoot: flag.String("microvm-volume-root", "",
			"Parent dir the per-session workspace volumes are minted under (microvm backend); "+
				"the filesystem the D7 session-volume quota is verified on. "+
				"Defaults to $COMPASS_MICROVM_VOLUME_ROOT."),
		quotaRequired: flag.Bool("microvm-quota-required", false,
			"Require an operator-provisioned project quota on the session-volume filesystem "+
				"(multi-tenant profile); startup fails when none is active (microvm backend). "+
				"Defaults to $COMPASS_MICROVM_QUOTA_REQUIRED."),
	}
}

// selectEngine resolves the configured runtime backend from the parsed flags
// and their environment fallbacks. guestInterceptors go to every guest control client except Health.
func (f backendFlags) selectEngine(guestInterceptors ...connect.ClientInterceptor) (runtime.WorkloadRuntime, error) {
	cfg, err := f.backendConfig()
	if err != nil {
		return nil, err
	}
	cfg.MicroVM.GuestClientInterceptors = guestInterceptors
	return runtime.SelectBackend(cfg)
}

// selectTracedEngine selects the engine with guest control client spans; Health stays untraced.
func (f backendFlags) selectTracedEngine() (runtime.WorkloadRuntime, error) {
	guestInterceptor, err := otelconnect.NewClientInterceptor()
	if err != nil {
		return nil, fmt.Errorf("otel: guest connect interceptor: %w", err)
	}
	return f.selectEngine(guestInterceptor)
}

// backendConfig resolves each backend knob from flag then env. Keep it separate
// from selectEngine so tests catch knobs that fail to reach BackendConfig,
// including the fail-open quota setting.
func (f backendFlags) backendConfig() (runtime.BackendConfig, error) {
	cpus, err := intOrEnv(*f.cpus, "COMPASS_MICROVM_CPUS")
	if err != nil {
		return runtime.BackendConfig{}, err
	}
	memoryMB, err := intOrEnv(*f.memoryMB, "COMPASS_MICROVM_MEMORY_MB")
	if err != nil {
		return runtime.BackendConfig{}, err
	}
	quotaRequired, err := boolOrEnv(*f.quotaRequired, "COMPASS_MICROVM_QUOTA_REQUIRED")
	if err != nil {
		return runtime.BackendConfig{}, err
	}
	return runtime.BackendConfig{
		Backend: orEnv(*f.backend, "COMPASS_RUNTIME_BACKEND"),
		MicroVM: runtime.MicroVMConfig{
			VMMPath:         orEnv(*f.vmm, "COMPASS_MICROVM_VMM"),
			VirtiofsdPath:   orEnv(*f.virtiofsd, "COMPASS_MICROVM_VIRTIOFSD"),
			KernelImage:     orEnv(*f.kernel, "COMPASS_MICROVM_KERNEL"),
			RootfsImage:     orEnv(*f.rootfs, "COMPASS_MICROVM_ROOTFS"),
			InitrdImage:     orEnv(*f.initrd, "COMPASS_MICROVM_INITRD"),
			ImageManifest:   orEnv(*f.imageManifest, "COMPASS_MICROVM_IMAGE_MANIFEST"),
			RunRoot:         orEnv(*f.runRoot, "COMPASS_MICROVM_RUNROOT"),
			VolumeRoot:      orEnv(*f.volumeRoot, "COMPASS_MICROVM_VOLUME_ROOT"),
			DefaultCPUs:     cpus,
			DefaultMemoryMB: memoryMB,
			QuotaRequired:   quotaRequired,
		},
		// Zero, explicitly: the runner exposes no program/timeout flags for
		// this backend, and NewAppleContainerCLI reads a zero field as "use the
		// default" (`container` on PATH, the shared command timeout).
		AppleContainer: runtime.AppleContainerConfig{},
	}, nil
}

// orEnv returns flagVal when non-empty, else the named environment variable.
func orEnv(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

// intOrEnv returns a nonzero flag value or parses env; empty env leaves the VMM
// default. Invalid values error with the variable and value, so a typo cannot
// silently become zero.
func intOrEnv(flagVal int, envKey string) (int, error) {
	if flagVal != 0 {
		return flagVal, nil
	}
	raw := os.Getenv(envKey)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("$%s=%q is not an integer: %w", envKey, raw, err)
	}
	return parsed, nil
}

// boolOrEnv uses a true flag, else parses env; invalid env values error. A false
// flag reads as unset, so env true beats an explicit false: a conflict leaves the
// quota gate ON. To turn it off, unset the env var.
// TestQuotaRequiredEnvTrueBeatsExplicitFlagFalse pins this direction.
func boolOrEnv(flagVal bool, envKey string) (bool, error) {
	if flagVal {
		return true, nil
	}
	raw := os.Getenv(envKey)
	if raw == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("$%s=%q is not a boolean (want true/false/1/0): %w", envKey, raw, err)
	}
	return parsed, nil
}

// parseEgress parses the comma-separated allowlist into a validated EgressPolicy.
// An empty list is a valid default-deny policy (no host reachable).
func parseEgress(csv string) (runtime.EgressPolicy, error) {
	if strings.TrimSpace(csv) == "" {
		return runtime.AllowEgress()
	}
	parts := strings.Split(csv, ",")
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			hosts = append(hosts, h)
		}
	}
	return runtime.AllowEgress(hosts...)
}

// parseMount parses one --mount value of the form host:container[:ro] into a
// bind mount. Exactly two fields (read-write) or three fields where the third is
// literally "ro" (read-only); host and container are required. Errors name the
// offending input and the accepted shape so an operator can act on them.
func parseMount(s string) (runtime.Mount, error) {
	fields := strings.Split(s, ":")
	if len(fields) != 2 && len(fields) != 3 {
		return runtime.Mount{}, fmt.Errorf(
			"invalid --mount %q: want host:container[:ro]", s)
	}
	host := strings.TrimSpace(fields[0])
	container := strings.TrimSpace(fields[1])
	if host == "" {
		return runtime.Mount{}, fmt.Errorf(
			"invalid --mount %q: empty host path, want host:container[:ro]", s)
	}
	if container == "" {
		return runtime.Mount{}, fmt.Errorf(
			"invalid --mount %q: empty container path, want host:container[:ro]", s)
	}
	if strings.ContainsRune(host, ',') || strings.ContainsRune(container, ',') {
		return runtime.Mount{}, fmt.Errorf(
			"invalid --mount %q: ',' is not allowed in a path (podman reads it as a "+
				"-v option separator), want host:container[:ro]", s)
	}
	// Agent containers run under podman --init, whose binary lives at
	// /run/podman-init; a mount over /run hides it and the container cannot start.
	if filepath.Clean(container) == "/run" {
		return runtime.Mount{}, fmt.Errorf(
			"invalid --mount %q: container path /run is reserved (it hides the container init), want host:container[:ro]", s)
	}
	readOnly := false
	if len(fields) == 3 {
		mode := strings.TrimSpace(fields[2])
		if mode != "ro" {
			return runtime.Mount{}, fmt.Errorf(
				"invalid --mount %q: unknown mode %q, want host:container[:ro]", s, mode)
		}
		readOnly = true
	}
	return runtime.Mount{HostPath: host, ContainerPath: container, ReadOnly: readOnly}, nil
}
