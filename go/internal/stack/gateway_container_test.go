//go:build unix

package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayContainerSpecBuildsFromConfig(t *testing.T) {
	cfg := Config{StateDir: "/state/../state", GatewayImage: "gateway:test"}
	spec, err := gatewayContainerSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(filepath.Clean(cfg.StateDir)))
	if want := "compass-gateway-" + hex.EncodeToString(hash[:6]); spec.Name != want {
		t.Errorf("Name = %q, want %q", spec.Name, want)
	}
	if spec.Image != cfg.GatewayImage || spec.TokenDir != filepath.Join(cfg.StateDir, "gateway") || spec.TokenFile != filepath.Join(cfg.StateDir, "gateway", "gateway.token") {
		t.Errorf("unexpected spec paths/image: %+v", spec)
	}
	if spec.Endpoint != "127.0.0.1:4100" || spec.HealthEndpoint != spec.Endpoint {
		t.Errorf("endpoints = %q, %q", spec.Endpoint, spec.HealthEndpoint)
	}
	wantEnv := map[string]string{"COMPASS_GATEWAY_BIND": "0.0.0.0:4000", "COMPASS_GATEWAY_DRAIN_MS": "20000", "COMPASS_GATEWAY_TOKEN_FILE": GatewayTokenMountPath}
	if len(spec.Env) != len(wantEnv) {
		t.Fatalf("Env = %#v", spec.Env)
	}
	for k, v := range wantEnv {
		if spec.Env[k] != v {
			t.Errorf("Env[%q] = %q, want %q", k, spec.Env[k], v)
		}
	}
	if spec.StopTimeout != 25*time.Second || spec.RestartRetries != 5 {
		t.Errorf("stop/restart = %s/%d", spec.StopTimeout, spec.RestartRetries)
	}
}

func TestGatewayContainerSpecRejectsMissingStateDir(t *testing.T) {
	if _, err := gatewayContainerSpec(Config{GatewayImage: "gateway:test"}); err == nil || !strings.Contains(err.Error(), "StateDir") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpRejectsMissingGatewayImageBeforeSpawn(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.GatewayImage = ""
	if _, err := Up(context.Background(), cfg, h.deps); err == nil || !strings.Contains(err.Error(), "--gateway-image") {
		t.Fatalf("Up error = %v", err)
	}
	if got := filterEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("started children before rejecting config: %v", got)
	}
}

func TestGatewayContainerSpecRejectsMissingImage(t *testing.T) {
	_, err := gatewayContainerSpec(Config{StateDir: "/state"})
	if err == nil || !strings.Contains(err.Error(), "--gateway-image") || !strings.Contains(err.Error(), "--gateway-external") {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewayContainerNameDeterministicPerStateDir(t *testing.T) {
	a, b := gatewayContainerName("/state/one"), gatewayContainerName("/state/two")
	if a != gatewayContainerName("/state/one") || a == b {
		t.Fatalf("names = %q, %q", a, b)
	}
}

func TestExternalGatewaySkipsGateway(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.ExternalGatewayURL = "http://gateway.example"
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(h.rec.snapshot(), "start llm-gateway") != 0 || countEvent(h.rec.snapshot(), "probe-gateway") != 0 {
		t.Fatalf("gateway events: %v", h.rec.snapshot())
	}
	if err := s.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayStartRecordsContainerEntry(t *testing.T) {
	cfg, h := newHarness(t)
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	if h.gateway.spec().Name == "" || h.gatewayProber.lastEndpoint() != h.gateway.spec().HealthEndpoint {
		t.Fatalf("spec = %+v", h.gateway.spec())
	}
	found := false
	for _, e := range s.pgids {
		if e.Component == ComponentGateway && e.Kind == entryContainer && e.ContainerName == h.gateway.spec().Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("gateway entry missing: %+v", s.pgids)
	}
	if err := s.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayStartFailureDrains(t *testing.T) {
	cfg, h := newHarness(t)
	h.gateway.startErr = errors.New("run failed")
	_, err := Up(context.Background(), cfg, h.deps)
	if err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("Up error = %v", err)
	}
	if countEvent(h.rec.snapshot(), "signal nats") != 1 || countEvent(h.rec.snapshot(), "start compass-server") != 0 {
		t.Fatalf("events: %v", h.rec.snapshot())
	}
}

func TestGatewayNeverReady(t *testing.T) {
	cfg, h := newHarness(t)
	h.gatewayProber.never = true
	start := time.Now()
	var calls int
	h.deps.Now = func() time.Time {
		calls++
		if calls > 5 {
			return start.Add(gatewayReadyPollBudget + time.Second)
		}
		return start
	}
	_, err := Up(context.Background(), cfg, h.deps)
	if err == nil || !strings.Contains(err.Error(), "gateway did not answer healthy") {
		t.Fatalf("Up error = %v", err)
	}
	if countEvent(h.rec.snapshot(), "signal llm-gateway") != 1 {
		t.Fatalf("events: %v", h.rec.snapshot())
	}
}

func TestGatewaySpawnAndDrainOrder(t *testing.T) {
	cfg, h := newHarness(t)
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	filtered := filterEvents(h.rec.snapshot())
	ni, gi, si := indexOf(filtered, "start nats"), indexOf(filtered, "start llm-gateway"), indexOf(filtered, "start compass-server")
	if !(ni >= 0 && ni < gi && gi < si) {
		t.Fatalf("spawn order: %v", filtered)
	}
	if err := s.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := h.rec.snapshot()
	si, gi, ni = indexOf(ev, "signal compass-server"), indexOf(ev, "signal llm-gateway"), indexOf(ev, "signal nats")
	if !(si >= 0 && si < gi && gi < ni) {
		t.Fatalf("drain order: %v", ev)
	}
}
func TestGatewayNameIsCleanNormalized(t *testing.T) {
	if gatewayContainerName("/state/../state") != gatewayContainerName("/state") {
		t.Fatal("name did not clean-normalize state dir")
	}
}
