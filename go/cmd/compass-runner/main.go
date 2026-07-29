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

	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/runtime"
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
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	// Ahead of every operator-input check: this validates the process's own
	// identity, takes no configuration, and its failure is unconditional. Behind
	// the flag checks, an operator on the wrong uid is told to set a token, fixes
	// that, re-runs, and only then learns the process can never work as this user.
	if err := verifyRunnerUID(os.Getuid()); err != nil {
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
		UID:         defaultAgentUID,
		NamePrefix:  "compass-agent-",
	})
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
		Engine:     runtime.NewPodmanCLI(),
		RuntimeDir: *runtimeDir,
		AgentModel: orEnv(*agentModel, "COMPASS_AGENT_MODEL"),
		HTTPClient: httpClient,
	}, specs, log)
}

// defaultAgentUID is the unprivileged uid the agent user runs as inside the
// container, matching the runtime package's agent-user convention.
const defaultAgentUID uint32 = 1000

// verifyRunnerUID enforces the baked-uid invariant the agent image and the
// container runtime jointly depend on. The image bakes the agent user, /nix and
// $HOME as uid defaultAgentUID, and the containers are launched with podman's
// plain --userns=keep-id, which maps the host uid through unchanged rather than
// remapping it. A Runner running as any other uid therefore produces a container
// whose agent is that uid and so does not own /nix or its own home — the
// agent-managed devenv then fails deep inside the first nix build. Fail here
// instead, where the cause is visible.
//
// The caller passes the REAL uid (os.Getuid), deliberately, not the effective
// one: keep-id maps the invoking process's real uid into the container, so that
// is the uid the agent ends up as. A setuid-style effective-uid difference must
// therefore not satisfy this guard.
func verifyRunnerUID(uid int) error {
	if uid == int(defaultAgentUID) {
		return nil
	}
	return fmt.Errorf(
		"the runner must run as uid %d, but it is running as uid %d: the agent "+
			"image bakes the agent user, /nix and $HOME as uid %d, and podman's "+
			"--userns=keep-id maps the host uid into the container unchanged, so "+
			"an agent launched by this runner would not own /nix",
		defaultAgentUID, uid, defaultAgentUID)
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
