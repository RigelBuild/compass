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

// natsTestSpec is a representative resolved nats spec. The config and data dirs
// are real temp paths so Start's MkdirAll + WriteFile succeed.
func natsTestSpec(t *testing.T) stack.NatsContainerSpec {
	t.Helper()
	root := t.TempDir()
	return stack.NatsContainerSpec{
		Name:            "compass-nats-deadbeef",
		Image:           "docker.io/library/nats@sha256:abc",
		ConfigDir:       filepath.Join(root, "nats-config"),
		ConfigYAML:      "port: 4222\n",
		DataDir:         filepath.Join(root, "nats"),
		ClientEndpoint:  "127.0.0.1:4222",
		MonitorEndpoint: "127.0.0.1:8222",
		StopTimeout:     20 * time.Second,
	}
}

// TestNatsRunArgsContract pins the exact `podman run` argv the nats contract
// requires: the auto-remove/replace/name/stop-timeout flags, both ports
// published on the loopback and mapped to the server's fixed client + monitoring
// ports, the read-only config bind-mount, the read-WRITE JetStream data mount,
// and the explicit `-c` server arg that overrides the image's default CMD.
func TestNatsRunArgsContract(t *testing.T) {
	spec := natsTestSpec(t)
	configFile := filepath.Join(spec.ConfigDir, "nats-server.conf")
	got := natsRunArgs(spec, configFile)
	want := []string{
		"run", "--detach",
		"--rm",
		"--replace",
		"--name", "compass-nats-deadbeef",
		"--stop-timeout", "20",
		"-p", "127.0.0.1:4222:4222",
		"-p", "127.0.0.1:8222:8222",
		"-v", configFile + ":/etc/compass-nats/nats-server.conf:ro,Z",
		"-v", spec.DataDir + ":/var/lib/nats:Z",
		"docker.io/library/nats@sha256:abc",
		"-c", "/etc/compass-nats/nats-server.conf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("natsRunArgs mismatch:\n got  %v\n want %v", got, want)
	}
}

// TestNatsRunArgsMountModes pins the load-bearing asymmetry between the two
// bind-mounts, which a plain argv comparison would let a future edit invert: the
// config is read-ONLY (the container must never rewrite the core-rendered
// config), while the JetStream data mount is read-WRITE (durable stream state
// that must survive a container replace — a `:ro` there would make nats-server
// fail to open its store, and an in-container-layer store would silently lose
// every stream on restart).
func TestNatsRunArgsMountModes(t *testing.T) {
	spec := natsTestSpec(t)
	configFile := filepath.Join(spec.ConfigDir, "nats-server.conf")
	args := natsRunArgs(spec, configFile)

	var mounts []string
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
	}
	if len(mounts) != 2 {
		t.Fatalf("natsRunArgs has %d bind mounts, want exactly 2 (config + JetStream data): %v", len(mounts), mounts)
	}

	var configMount, dataMount string
	for _, m := range mounts {
		switch {
		case strings.HasPrefix(m, configFile+":"):
			configMount = m
		case strings.HasPrefix(m, spec.DataDir+":"):
			dataMount = m
		}
	}
	if configMount == "" || dataMount == "" {
		t.Fatalf("could not identify both mounts in %v", mounts)
	}
	if !strings.HasSuffix(configMount, ":ro,Z") {
		t.Errorf("config mount %q is not read-only; want a :ro,Z suffix", configMount)
	}
	if strings.Contains(dataMount, ":ro") {
		t.Errorf("JetStream data mount %q is read-only; it must be read-WRITE (durable stream state)", dataMount)
	}
	// The data mount target must be exactly the store_dir the core renders into
	// the config, or the server writes its store into the container layer.
	if !strings.HasPrefix(dataMount, spec.DataDir+":"+stack.NatsStoreDir) {
		t.Errorf("JetStream data mount %q does not target the rendered store_dir %q", dataMount, stack.NatsStoreDir)
	}
}

// TestNatsStartWritesConfigAndRuns pins Start's side effects: it creates both
// bind-mount source dirs, writes the generated config to
// <config dir>/nats-server.conf, and issues exactly the run argv, returning a
// Process handle.
func TestNatsStartWritesConfigAndRuns(t *testing.T) {
	spec := natsTestSpec(t)
	cli := &fakeContainerCLI{}
	nc := &NatsContainer{cli: cli, health: &fakeHealthGetter{code: 200}}

	p, err := nc.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("Start returned a nil Process")
	}
	configFile := filepath.Join(spec.ConfigDir, "nats-server.conf")
	body, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if string(body) != spec.ConfigYAML {
		t.Errorf("config file body = %q, want the spec ConfigYAML %q", body, spec.ConfigYAML)
	}
	// The JetStream data dir is the other bind-mount SOURCE: podman fails with a
	// statfs error if it does not pre-exist, so Start must create it too.
	info, err := os.Stat(spec.DataDir)
	if err != nil {
		t.Fatalf("JetStream data dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("JetStream data dir %q is not a directory", spec.DataDir)
	}
	if !reflect.DeepEqual(cli.runArgs, natsRunArgs(spec, configFile)) {
		t.Errorf("Start ran %v, want the nats argv", cli.runArgs)
	}
}

// TestNatsStartRunFailurePropagates pins that a failed `podman run` surfaces as
// an error, not a phantom Process handle.
func TestNatsStartRunFailurePropagates(t *testing.T) {
	spec := natsTestSpec(t)
	cli := &fakeContainerCLI{runErr: errors.New("podman: pull denied")}
	nc := &NatsContainer{cli: cli, health: &fakeHealthGetter{code: 200}}

	if _, err := nc.Start(context.Background(), spec); err == nil {
		t.Fatal("Start() = nil error on a failed run, want the run error")
	}
}

// TestNatsProbeHealthy pins the NatsProber seam: a 200 is healthy (nil error)
// and the probe GETs the monitoring endpoint's /healthz path — the server's own
// readiness verdict, not a bare TCP dial.
func TestNatsProbeHealthy(t *testing.T) {
	hg := &fakeHealthGetter{code: http.StatusOK}
	nc := &NatsContainer{cli: &fakeContainerCLI{}, health: hg}

	if err := nc.ProbeNats(context.Background(), "127.0.0.1:8222"); err != nil {
		t.Fatalf("ProbeNats(200) = %v, want nil", err)
	}
	if hg.lastURL != "http://127.0.0.1:8222/healthz" {
		t.Errorf("probed %q, want http://127.0.0.1:8222/healthz", hg.lastURL)
	}
}

// TestNatsProbeUnhealthy pins that a non-200 status and a transport error (a
// refused dial while the container is still booting) are both not-yet-ready: a
// non-nil error the core's poll retries.
func TestNatsProbeUnhealthy(t *testing.T) {
	t.Run("non-200 status", func(t *testing.T) {
		nc := &NatsContainer{cli: &fakeContainerCLI{}, health: &fakeHealthGetter{code: 503}}
		if err := nc.ProbeNats(context.Background(), "127.0.0.1:8222"); err == nil {
			t.Fatal("ProbeNats(503) = nil, want a not-ready error")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		nc := &NatsContainer{cli: &fakeContainerCLI{}, health: &fakeHealthGetter{err: errors.New("dial refused")}}
		if err := nc.ProbeNats(context.Background(), "127.0.0.1:8222"); err == nil {
			t.Fatal("ProbeNats(dial error) = nil, want a not-ready error")
		}
	})
}

// TestNatsControllerDispatch pins the ContainerController seam this adapter also
// fills: Exists reads the fake's existence map, Stop and Remove drive the
// respective podman calls by name.
func TestNatsControllerDispatch(t *testing.T) {
	cli := &fakeContainerCLI{existsResp: map[string]bool{"compass-nats-x": true}}
	nc := &NatsContainer{cli: cli, health: &fakeHealthGetter{}}

	if !nc.Exists("compass-nats-x") {
		t.Error("Exists(present) = false, want true")
	}
	if nc.Exists("absent") {
		t.Error("Exists(absent) = true, want false")
	}
	if err := nc.Stop("compass-nats-x", 20*time.Second); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if !reflect.DeepEqual(cli.stopped, []string{"compass-nats-x"}) {
		t.Errorf("stop calls = %v, want one stop", cli.stopped)
	}
	if err := nc.Remove("compass-nats-x"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if !reflect.DeepEqual(cli.removed, []string{"compass-nats-x"}) {
		t.Errorf("remove calls = %v, want one remove", cli.removed)
	}
}

// TestNatsExistsAssumesPresentOnEngineError pins the stranded-container guard: a
// genuine podman engine error makes Exists report PRESENT, so entryAlive still
// builds a teardown target instead of dropping a live container that still holds
// the JetStream store.
func TestNatsExistsAssumesPresentOnEngineError(t *testing.T) {
	cli := &fakeContainerCLI{existsErr: errors.New("podman daemon wedged")}
	nc := &NatsContainer{cli: cli, health: &fakeHealthGetter{}}
	if !nc.Exists("compass-nats-x") {
		t.Fatal("Exists on engine error = false, want true (assume present, drive teardown)")
	}
}
