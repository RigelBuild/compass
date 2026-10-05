//go:build unix

package adapters

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

type gatewayTestHealth struct {
	url  string
	code int
	err  error
}

func (h *gatewayTestHealth) get(_ context.Context, url string) (int, error) {
	h.url = url
	return h.code, h.err
}
func gatewayTestSpec(t *testing.T) stack.GatewayContainerSpec {
	t.Helper()
	root := t.TempDir()
	tokenDir := filepath.Join(root, "gateway")
	return stack.GatewayContainerSpec{Name: "compass-gateway-test", Image: "gateway:test", TokenDir: tokenDir, TokenFile: filepath.Join(tokenDir, "gateway.token"), Endpoint: "127.0.0.1:4000", HealthEndpoint: "127.0.0.1:4000", Env: map[string]string{"COMPASS_GATEWAY_BIND": "0.0.0.0:4000", "COMPASS_GATEWAY_DRAIN_MS": "20000", "COMPASS_GATEWAY_TOKEN_FILE": stack.GatewayTokenMountPath}, StopTimeout: 25 * time.Second, RestartRetries: 5}
}

func TestGatewayRunArgsContract(t *testing.T) {
	spec := gatewayTestSpec(t)
	got := gatewayRunArgs(spec)
	want := []string{"run", "--detach", "--replace", "--name", spec.Name, "--stop-timeout", "25", "--restart", "on-failure:5", "-p", "127.0.0.1:4000:4000", "-e", "COMPASS_GATEWAY_BIND=0.0.0.0:4000", "-e", "COMPASS_GATEWAY_DRAIN_MS=20000", "-e", "COMPASS_GATEWAY_TOKEN_FILE=" + stack.GatewayTokenMountPath, "-v", spec.TokenFile + ":" + stack.GatewayTokenMountPath + ":ro,Z", spec.Image}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for _, arg := range got {
		if arg == "--rm" {
			t.Fatal("gateway run must not use --rm")
		}
	}
}

func TestGatewayStartCreatesAndReusesToken(t *testing.T) {
	spec := gatewayTestSpec(t)
	cli := &gatewayFakeCLI{}
	c := &GatewayContainer{cli: cli, health: &gatewayTestHealth{}}
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(spec.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(b))) != 64 {
		t.Fatalf("token length = %d", len(strings.TrimSpace(string(b))))
	}
	info, err := os.Stat(spec.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := os.WriteFile(spec.TokenFile, []byte("existing-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(spec.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "existing-token\n" {
		t.Fatalf("token overwritten: %q", b)
	}
}

func TestGatewayStartRegeneratesEmptyToken(t *testing.T) {
	spec := gatewayTestSpec(t)
	if err := os.MkdirAll(spec.TokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.TokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &GatewayContainer{cli: &gatewayFakeCLI{}, health: &gatewayTestHealth{}}
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(spec.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(b))) != 64 {
		t.Fatalf("token length = %d", len(strings.TrimSpace(string(b))))
	}
}

func TestGatewayRunFailurePropagates(t *testing.T) {
	spec := gatewayTestSpec(t)
	cli := &gatewayFakeCLI{runErr: errors.New("run failed")}
	c := &GatewayContainer{cli: cli, health: &gatewayTestHealth{}}
	if _, err := c.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("Start error = %v", err)
	}
}

func TestGatewayProbeHealthyAndUnhealthy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		wantErr bool
	}{{"healthy", http.StatusOK, false}, {"unhealthy", http.StatusServiceUnavailable, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := &gatewayTestHealth{code: tc.code}
			g := &GatewayContainer{health: h}
			err := g.ProbeGateway(context.Background(), "127.0.0.1:4000")
			if h.url != "http://127.0.0.1:4000/healthz" {
				t.Fatalf("url = %q", h.url)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("ProbeGateway error = %v", err)
			}
		})
	}
}

func TestGatewaySignalStopsThenRemoves(t *testing.T) {
	cli := &gatewayFakeCLI{}
	p := &gatewayProcess{cli: cli, name: "gateway", stopTimeout: 25 * time.Second}
	if err := p.Signal(context.Background(), stack.SignalTerm); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cli.ops, []string{"stop gateway", "remove gateway"}) {
		t.Fatalf("ops = %v", cli.ops)
	}
	if err := p.Signal(context.Background(), stack.SignalKill); err == nil {
		t.Fatal("SignalKill accepted")
	}
}

type gatewayFakeCLI struct {
	ops    []string
	args   []string
	runErr error
}

func (f *gatewayFakeCLI) run(_ context.Context, args []string) error {
	f.ops = append(f.ops, "run")
	f.args = append([]string(nil), args...)
	return f.runErr
}
func (f *gatewayFakeCLI) wait(_ context.Context, name string) error {
	f.ops = append(f.ops, "wait "+name)
	return nil
}
func (f *gatewayFakeCLI) stop(_ context.Context, name string, _ time.Duration) error {
	f.ops = append(f.ops, "stop "+name)
	return nil
}
func (f *gatewayFakeCLI) term(_ context.Context, name string) error {
	f.ops = append(f.ops, "term "+name)
	return nil
}
func (f *gatewayFakeCLI) remove(_ context.Context, name string) error {
	f.ops = append(f.ops, "remove "+name)
	return nil
}
func (f *gatewayFakeCLI) removeExited(_ context.Context, name string) error {
	f.ops = append(f.ops, "remove-exited "+name)
	return nil
}
func (f *gatewayFakeCLI) exists(_ context.Context, _ string) (bool, error) { return false, nil }

var _ containerCLI = (*gatewayFakeCLI)(nil)

func TestGatewayExistsAssumesPresentOnEngineError(t *testing.T) {
	c := &GatewayContainer{cli: &gatewayExistsErrorCLI{}}
	if !c.Exists(context.Background(), "gateway") {
		t.Fatal("Exists reported absent when engine errored")
	}
}

type gatewayExistsErrorCLI struct{}

func (*gatewayExistsErrorCLI) run(context.Context, []string) error               { return nil }
func (*gatewayExistsErrorCLI) wait(context.Context, string) error                { return nil }
func (*gatewayExistsErrorCLI) stop(context.Context, string, time.Duration) error { return nil }
func (*gatewayExistsErrorCLI) term(context.Context, string) error                { return nil }
func (*gatewayExistsErrorCLI) remove(context.Context, string) error              { return nil }
func (*gatewayExistsErrorCLI) removeExited(context.Context, string) error        { return nil }
func (*gatewayExistsErrorCLI) exists(context.Context, string) (bool, error) {
	return false, errors.New("engine unavailable")
}
func TestGatewayControllerDispatchesStopAndRemove(t *testing.T) {
	ctx := context.Background()
	cli := &gatewayFakeCLI{}
	c := &GatewayContainer{cli: cli}
	if err := c.Stop(ctx, "gateway"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveExited(ctx, "gateway"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "gateway"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cli.ops, []string{"term gateway", "remove-exited gateway", "remove gateway"}) {
		t.Fatalf("controller ops = %v", cli.ops)
	}
}
