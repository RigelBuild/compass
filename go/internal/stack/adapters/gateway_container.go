//go:build unix

package adapters

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

const gatewayHealthTimeout = 5 * time.Second

// GatewayContainer is the podman-backed gateway child: start, teardown by name, and health probe.
type GatewayContainer struct {
	cli    containerCLI
	health healthGetter
}

var (
	_ stack.GatewayContainer    = (*GatewayContainer)(nil)
	_ stack.ContainerController = (*GatewayContainer)(nil)
	_ stack.GatewayProber       = (*GatewayContainer)(nil)
)

// NewGatewayContainer builds the adapter over the host podman.
func NewGatewayContainer() (*GatewayContainer, error) {
	return &GatewayContainer{cli: newPodmanExec(), health: &httpHealthGetter{client: &http.Client{Timeout: gatewayHealthTimeout}}}, nil
}

// Start ensures the bearer token file, then runs the container detached.
// It returns at launch; ProbeGateway is the readiness gate.
func (c *GatewayContainer) Start(ctx context.Context, spec stack.GatewayContainerSpec) (stack.Process, error) {
	if err := os.MkdirAll(spec.TokenDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating gateway token dir %q: %w", spec.TokenDir, err)
	}
	if err := ensureGatewayToken(spec.TokenFile); err != nil {
		return nil, fmt.Errorf("ensuring gateway token %q: %w", spec.TokenFile, err)
	}
	if err := c.cli.run(ctx, gatewayRunArgs(spec)); err != nil {
		return nil, fmt.Errorf("podman run gateway %q: %w", spec.Name, err)
	}
	return &gatewayProcess{cli: c.cli, name: spec.Name, stopTimeout: spec.StopTimeout}, nil
}

// ensureGatewayToken keeps a non-empty token across restarts and mints a 0600 one otherwise.
func ensureGatewayToken(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the spec-built token file under the stack state dir
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(raw[:])), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Exists reports presence; an engine error counts as present so teardown still runs.
func (c *GatewayContainer) Exists(ctx context.Context, name string) bool {
	present, err := c.cli.exists(ctx, name)
	return err != nil || present
}

// Stop is the ContainerController graceful stop: the stop signal, sent without waiting.
func (c *GatewayContainer) Stop(ctx context.Context, name string) error {
	return c.cli.term(ctx, name)
}

// RemoveExited removes the gateway once it has exited; it runs without --rm.
func (c *GatewayContainer) RemoveExited(ctx context.Context, name string) error {
	return c.cli.removeExited(ctx, name)
}

// Remove is the ContainerController hard kill (`podman rm -f`).
func (c *GatewayContainer) Remove(ctx context.Context, name string) error {
	return c.cli.remove(ctx, name)
}

// ProbeGateway returns nil once GET /healthz answers 200.
func (c *GatewayContainer) ProbeGateway(ctx context.Context, endpoint string) error {
	url := "http://" + endpoint + "/healthz"
	code, err := c.health.get(ctx, url)
	if err != nil {
		return fmt.Errorf("gateway health GET %q: %w", url, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("gateway health %q returned status %d, want 200", url, code)
	}
	return nil
}

func gatewayRunArgs(spec stack.GatewayContainerSpec) []string {
	args := make([]string, 0, 14+2*len(spec.Env))
	args = append(args, cmdPodmanRun, flagPodmanDetach, flagPodmanReplace, flagPodmanName, spec.Name, flagPodmanStopTimeout, strconv.FormatInt(stopSeconds(spec.StopTimeout), 10), flagPodmanRestart, "on-failure:"+strconv.Itoa(spec.RestartRetries), "-p", spec.Endpoint+":4000")
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, flagPodmanEnv, key+"="+spec.Env[key])
	}
	args = append(args, flagPodmanVolume, spec.TokenFile+":"+stack.GatewayTokenMountPath+":ro,Z", spec.Image)
	return args
}

// gatewayProcess is the in-process handle; the container has no --rm, so SignalTerm also removes it.
type gatewayProcess struct {
	cli         containerCLI
	name        string
	stopTimeout time.Duration
}

var _ stack.Process = (*gatewayProcess)(nil)

func (p *gatewayProcess) Signal(ctx context.Context, sig stack.ProcessSignal) error {
	if sig != stack.SignalTerm {
		return fmt.Errorf("unknown process signal %d", int(sig))
	}
	if err := p.cli.stop(ctx, p.name, p.stopTimeout); err != nil {
		return fmt.Errorf("podman stop gateway %q: %w", p.name, err)
	}
	if err := p.cli.remove(ctx, p.name); err != nil {
		return fmt.Errorf("podman remove gateway %q: %w", p.name, err)
	}
	return nil
}
func (p *gatewayProcess) Wait(ctx context.Context) error { return p.cli.wait(ctx, p.name) }
func (*gatewayProcess) Pid() int                         { return 0 }
