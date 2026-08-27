//go:build unix

package adapters

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// fakeHealthGetter records the health-probe URL and returns a canned status and
// error so the probe's healthy/unhealthy verdict is unit-testable without a live
// collector.
type fakeHealthGetter struct {
	lastURL string
	code    int
	err     error
}

func (f *fakeHealthGetter) get(_ context.Context, url string) (int, error) {
	f.lastURL = url
	return f.code, f.err
}

// collectorTestSpec is a representative resolved collector spec. The config dir
// is a real temp dir so Start's MkdirAll + WriteFile succeed.
func collectorTestSpec(t *testing.T) stack.CollectorContainerSpec {
	t.Helper()
	root := t.TempDir()
	return stack.CollectorContainerSpec{
		Name:           "compass-otel-collector-deadbeef",
		Image:          "docker.io/otel/opentelemetry-collector-contrib@sha256:abc",
		ConfigDir:      filepath.Join(root, "collector"),
		ConfigYAML:     "receivers:\n  otlp:\n",
		GRPCEndpoint:   "127.0.0.1:4317",
		HTTPEndpoint:   "127.0.0.1:4318",
		HealthEndpoint: "127.0.0.1:13133",
		StopTimeout:    10 * time.Second,
	}
}

// TestCollectorRunArgsContract pins the exact `podman run` argv the T4 collector
// contract requires: the auto-remove/replace/name/stop-timeout flags, the three
// published loopback ports mapped to the container's fixed OTLP + health ports,
// the read-only config bind-mount, and the image ref — with NO data volume (D3
// drops rather than buffering to disk).
func TestCollectorRunArgsContract(t *testing.T) {
	spec := collectorTestSpec(t)
	configFile := filepath.Join(spec.ConfigDir, "config.yaml")
	got := collectorRunArgs(spec, configFile)
	want := []string{
		"run", "--detach",
		"--rm",
		"--replace",
		"--name", "compass-otel-collector-deadbeef",
		"--stop-timeout", "10",
		"-p", "127.0.0.1:4317:4317",
		"-p", "127.0.0.1:4318:4318",
		"-p", "127.0.0.1:13133:13133",
		"-v", configFile + ":/etc/otelcol-contrib/config.yaml:ro,Z",
		"docker.io/otel/opentelemetry-collector-contrib@sha256:abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collectorRunArgs mismatch:\n got  %v\n want %v", got, want)
	}
	// No data volume: exactly one -v (the config bind), never a PGDATA-style
	// data mount — the D3 drop-not-buffer posture.
	vCount := 0
	for _, a := range got {
		if a == "-v" {
			vCount++
		}
	}
	if vCount != 1 {
		t.Fatalf("collectorRunArgs has %d bind mounts, want exactly 1 (config only, no data volume)", vCount)
	}
}

// TestCollectorStartWritesConfigAndRuns pins Start's side effects: it creates
// the config dir, writes the generated config to <dir>/config.yaml, and issues
// exactly the run argv, returning a Process handle.
func TestCollectorStartWritesConfigAndRuns(t *testing.T) {
	spec := collectorTestSpec(t)
	cli := &fakeContainerCLI{}
	cc := &CollectorContainer{cli: cli, health: &fakeHealthGetter{code: 200}}

	p, err := cc.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("Start returned a nil Process")
	}
	configFile := filepath.Join(spec.ConfigDir, "config.yaml")
	body, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if string(body) != spec.ConfigYAML {
		t.Errorf("config file body = %q, want the spec ConfigYAML %q", body, spec.ConfigYAML)
	}
	if !reflect.DeepEqual(cli.runArgs, collectorRunArgs(spec, configFile)) {
		t.Errorf("Start ran %v, want the T4 argv", cli.runArgs)
	}
}

// TestCollectorStartRunFailurePropagates pins that a failed `podman run` surfaces
// as an error, not a phantom Process handle.
func TestCollectorStartRunFailurePropagates(t *testing.T) {
	spec := collectorTestSpec(t)
	cli := &fakeContainerCLI{runErr: errors.New("podman: pull denied")}
	cc := &CollectorContainer{cli: cli, health: &fakeHealthGetter{code: 200}}

	if _, err := cc.Start(context.Background(), spec); err == nil {
		t.Fatal("Start() = nil error on a failed run, want the run error")
	}
}

// TestCollectorProbeHealthy pins the CollectorProber seam: a 200 is healthy
// (nil error) and the probe GETs the health endpoint's "/" path.
func TestCollectorProbeHealthy(t *testing.T) {
	hg := &fakeHealthGetter{code: http.StatusOK}
	cc := &CollectorContainer{cli: &fakeContainerCLI{}, health: hg}

	if err := cc.ProbeCollector(context.Background(), "127.0.0.1:13133"); err != nil {
		t.Fatalf("ProbeCollector(200) = %v, want nil", err)
	}
	if hg.lastURL != "http://127.0.0.1:13133/" {
		t.Errorf("probed %q, want http://127.0.0.1:13133/", hg.lastURL)
	}
}

// TestCollectorProbeUnhealthy pins that a non-200 status and a transport error
// are both not-yet-ready (non-nil error the core's poll retries).
func TestCollectorProbeUnhealthy(t *testing.T) {
	t.Run("non-200 status", func(t *testing.T) {
		cc := &CollectorContainer{cli: &fakeContainerCLI{}, health: &fakeHealthGetter{code: 503}}
		if err := cc.ProbeCollector(context.Background(), "127.0.0.1:13133"); err == nil {
			t.Fatal("ProbeCollector(503) = nil, want a not-ready error")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		cc := &CollectorContainer{cli: &fakeContainerCLI{}, health: &fakeHealthGetter{err: errors.New("dial refused")}}
		if err := cc.ProbeCollector(context.Background(), "127.0.0.1:13133"); err == nil {
			t.Fatal("ProbeCollector(dial error) = nil, want a not-ready error")
		}
	})
}

// TestCollectorControllerDispatch pins the ContainerController seam this adapter
// also fills: Exists reads the fake's existence map, Stop and Remove drive the
// respective podman calls by name.
func TestCollectorControllerDispatch(t *testing.T) {
	cli := &fakeContainerCLI{existsResp: map[string]bool{"compass-otel-collector-x": true}}
	cc := &CollectorContainer{cli: cli, health: &fakeHealthGetter{}}

	if !cc.Exists("compass-otel-collector-x") {
		t.Error("Exists(present) = false, want true")
	}
	if cc.Exists("absent") {
		t.Error("Exists(absent) = true, want false")
	}
	if err := cc.Stop("compass-otel-collector-x", 10*time.Second); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if !reflect.DeepEqual(cli.stopped, []string{"compass-otel-collector-x"}) {
		t.Errorf("stop calls = %v, want one stop", cli.stopped)
	}
	if err := cc.Remove("compass-otel-collector-x"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if !reflect.DeepEqual(cli.removed, []string{"compass-otel-collector-x"}) {
		t.Errorf("remove calls = %v, want one remove", cli.removed)
	}
}

// TestCollectorExistsAssumesPresentOnEngineError pins the stranded-container
// guard: a genuine podman engine error makes Exists report PRESENT, so
// entryAlive still builds a teardown target instead of dropping a live
// container.
func TestCollectorExistsAssumesPresentOnEngineError(t *testing.T) {
	cli := &fakeContainerCLI{existsErr: errors.New("podman daemon wedged")}
	cc := &CollectorContainer{cli: cli, health: &fakeHealthGetter{}}
	if !cc.Exists("compass-otel-collector-x") {
		t.Fatal("Exists on engine error = false, want true (assume present, drive teardown)")
	}
}
