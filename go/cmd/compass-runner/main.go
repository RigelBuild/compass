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
	"os"
	"os/signal"
	"strings"
	"syscall"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/agentuid"
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
	egressHosts := flag.String("egress-allow", "",
		"Comma-separated default-deny egress allowlist (DNS names or IP literals).")
	runtimeDir := flag.String("runtime-dir", "/run/compass",
		"Runner-owned base dir for per-container agent sockets (RuntimeDir/containers/<container>/agent.sock).")
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

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	// Ahead of every operator-input check: this validates an engine fact the
	// whole launch path depends on — that podman is new enough for the
	// container userns remap (--userns=keep-id:uid=,gid=, podman ≥ 4.3;
	// docs/designs/infra/runtime/compass-runner-arbitrary-uid/design.md). It takes no
	// operator configuration and its failure is unconditional. Behind the flag
	// checks, an operator on too-old a podman is told to set a token, fixes
	// that, re-runs, and only then learns the engine can never launch a
	// container — so the legible startup refusal must come first.
	if err := runtime.NewPodmanCLI().VerifyUsernsRemapSupport(context.Background()); err != nil {
		return err
	}

	id := orEnv(*runnerID, "COMPASS_RUNNER_ID")
	if id == "" {
		return errors.New("a runner id is required: pass --runner-id or set $COMPASS_RUNNER_ID")
	}
	addr := orEnv(*serverAddr, "COMPASS_SERVER_ADDR")
	if addr == "" {
		return errors.New("a server address is required: pass --server or set $COMPASS_SERVER_ADDR")
	}
	// The per-Runner token is a bearer secret: env only, never a flag (a flag
	// leaks into the process table). Operator-provisioned, stored 0600 (OQ7).
	token := os.Getenv("COMPASS_RUNNER_TOKEN")
	if token == "" {
		return errors.New("a runner token is required: set $COMPASS_RUNNER_TOKEN")
	}
	img := orEnv(*image, "COMPASS_AGENT_IMAGE")
	if img == "" {
		return errors.New("an agent image is required: pass --image or set $COMPASS_AGENT_IMAGE")
	}
	egress, err := parseEgress(*egressHosts)
	if err != nil {
		return err
	}

	// --ca (or $COMPASS_RUNNER_CA) swaps the system root pool for a single
	// trusted CA — the local dogfood path, where the Server's self-signed
	// 127.0.0.1 cert is the trust anchor. Unset leaves HTTPClient nil, so Dial
	// uses http.DefaultClient (system roots) for a Server behind a public CA.
	var httpClient connect.HTTPClient
	if ca := orEnv(*caPath, "COMPASS_RUNNER_CA"); ca != "" {
		httpClient, err = runner.NewCATrustClient(ca)
		if err != nil {
			return err
		}
	}
	specs, err := runner.NewConfigSpecBuilder(runner.SpecDefaults{
		Image:       img,
		Egress:      egress,
		CheckoutDir: *checkoutDir,
		HomeDir:     *homeDir,
		UID:         agentuid.AgentUID,
		NamePrefix:  runner.AgentContainerNamePrefix,
		Mounts:      mounts,
	})
	if err != nil {
		return err
	}

	engine, err := backends.selectEngine()
	if err != nil {
		return err
	}

	slog.Info("compass-runner starting",
		"version", version, "runner_id", id, "server", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runner.Run(ctx, runner.RunnerConfig{
		RunnerID:   id,
		ServerAddr: addr,
		Token:      token,
		Engine:     engine,
		RuntimeDir: *runtimeDir,
		AgentModel: orEnv(*agentModel, "COMPASS_AGENT_MODEL"),
		HTTPClient: httpClient,
	}, specs, log)
}

// backendFlags holds the runtime-backend selection flags, registered on the
// default flag set before flag.Parse and resolved into a runtime after it.
type backendFlags struct {
	backend, vmm, virtiofsd, kernel, rootfs *string
}

// registerBackendFlags declares the backend-selection flags. Call before
// flag.Parse; resolve with selectEngine after.
func registerBackendFlags() backendFlags {
	return backendFlags{
		backend: flag.String("backend", "",
			"Container runtime backend: 'podman' (default, transitional) or "+
				"'microvm'. Defaults to $COMPASS_RUNTIME_BACKEND."),
		vmm: flag.String("microvm-vmm", "",
			"Path to the microVM monitor binary (microvm backend). Defaults to $COMPASS_MICROVM_VMM."),
		virtiofsd: flag.String("microvm-virtiofsd", "",
			"Path to the virtiofs daemon binary (microvm backend). Defaults to $COMPASS_MICROVM_VIRTIOFSD."),
		kernel: flag.String("microvm-kernel", "",
			"Path to the guest kernel image (microvm backend). Defaults to $COMPASS_MICROVM_KERNEL."),
		rootfs: flag.String("microvm-rootfs", "",
			"Path to the guest rootfs image (microvm backend). Defaults to $COMPASS_MICROVM_ROOTFS."),
	}
}

// selectEngine resolves the configured runtime backend from the parsed flags
// and their environment fallbacks.
func (f backendFlags) selectEngine() (runtime.ContainerRuntime, error) {
	return runtime.SelectBackend(runtime.BackendConfig{
		Backend: orEnv(*f.backend, "COMPASS_RUNTIME_BACKEND"),
		MicroVM: runtime.MicroVMConfig{
			VMMPath:       orEnv(*f.vmm, "COMPASS_MICROVM_VMM"),
			VirtiofsdPath: orEnv(*f.virtiofsd, "COMPASS_MICROVM_VIRTIOFSD"),
			KernelImage:   orEnv(*f.kernel, "COMPASS_MICROVM_KERNEL"),
			RootfsImage:   orEnv(*f.rootfs, "COMPASS_MICROVM_ROOTFS"),
		},
	})
}

// orEnv returns flagVal when non-empty, else the named environment variable.
func orEnv(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
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
