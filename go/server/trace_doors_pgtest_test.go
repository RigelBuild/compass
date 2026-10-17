//go:build pgtest && unix

package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/secrets"
)

// An auth rejection still counts: the span interceptor is outermost on every mount.
func TestServerDoorsEmitSpanForEveryServiceMount(t *testing.T) {
	ctx := t.Context()
	st, adminID, _ := newNetworkStore(t)
	key, err := envelope.NewKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("envelope.NewKey: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	brd := board.NewProjection(bus)
	issueBrd := board.NewIssueProjection(bus, st)
	tail := newSessionTail()
	commsSvc := comms.NewComms(t.Context(), st, commsBus, nil, adminID)
	hub := newRunnerHub(st, brd, tail, commsSvc, nil)
	svc := newService("trace-door-test", bus, st, hub, brd, issueBrd, tail)
	resolver := secrets.NewStoreResolver(st, key, 1)
	secretsSvc := newSecretsService(st, resolver, &fakeResolver{}, nil)
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	devListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dev: %v", err)
	}
	t.Cleanup(func() { _ = devListener.Close() })
	netListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen network: %v", err)
	}
	t.Cleanup(func() { _ = netListener.Close() })
	containerResolver := secrets.NewStoreResolver(st, key, 1)
	gotDoors, err := buildDoors(ctx, ServeConfig{StateDir: t.TempDir()}, svc, commsSvc, secretsSvc, hub, st,
		adminID, "", containerResolver, &fakeResolver{}, devListener, netListener, nil,
		nil, nil, nil, key, 1)
	if err != nil {
		t.Fatalf("buildDoors: %v", err)
	}
	t.Cleanup(func() {
		for _, server := range []*http.Server{gotDoors.uds, gotDoors.dev, gotDoors.net} {
			if server != nil {
				_ = server.Close()
			}
		}
	})
	if gotDoors.dev == nil || gotDoors.net == nil {
		t.Fatalf("built doors = uds:%t dev:%t net:%t, want all three", gotDoors.uds != nil, gotDoors.dev != nil, gotDoors.net != nil)
	}
	type door struct {
		name string
		http http.Handler
	}
	doorsToTest := []door{{name: "socket", http: gotDoors.uds.Handler}, {name: "dev", http: gotDoors.dev.Handler}, {name: "network", http: gotDoors.net.Handler}}
	for _, currentDoor := range doorsToTest {
		for _, serviceName := range []string{"CompassService", "CommsService", "SecretsService", "UsageService", "AgentRepositoryService"} {
			name, service := currentDoor.name, serviceName
			t.Run(name+"/"+service, func(t *testing.T) {
				recorder.Reset()
				code := callDoorProbe(t, currentDoor.http, service)
				if code == http.StatusNotFound {
					t.Fatalf("%s RPC returned 404; the service is not mounted", service)
				}
				assertOneServerSpan(t, recorder)
			})
		}
	}

	for _, serviceName := range []string{"RunnerService", "GatewayCredentials", "GatewayRegistry", "GatewayAuth"} {
		service := serviceName
		t.Run("network/"+service, func(t *testing.T) {
			recorder.Reset()
			code := callDoorProbe(t, gotDoors.net.Handler, service)
			if code == http.StatusNotFound {
				t.Fatalf("%s RPC returned 404; the service is not mounted", service)
			}
			assertOneServerSpan(t, recorder)
		})
	}
}

func callDoorProbe(t *testing.T, handler http.Handler, serviceName string) int {
	t.Helper()
	path := doorProbe(serviceName)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(doorProbeBody(serviceName)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec.Code
}

func doorProbe(service string) string {
	var path string
	switch service {
	case "CompassService":
		path = compassv1connect.CompassServiceGetServerInfoProcedure
	case "CommsService":
		path = compassv1connect.CommsServiceListAccountsProcedure
	case "SecretsService":
		path = compassv1connect.SecretsServiceListServerSecretsProcedure
	case "UsageService":
		path = compassv1connect.UsageServiceGetUsageSeriesProcedure
	case "AgentRepositoryService":
		path = compassv1connect.AgentRepositoryServiceListAgentRepositoriesProcedure
	case "RunnerService":
		path = compassv1internalconnect.RunnerServiceEnrollProcedure
	case "GatewayCredentials":
		path = compassv1internalconnect.GatewayCredentialsListCredentialPoolProcedure
	case "GatewayRegistry":
		path = compassv1internalconnect.GatewayRegistryGetGatewayModelRegistryProcedure
	case "GatewayAuth":
		path = compassv1internalconnect.GatewayAuthVerifyAgentTokenProcedure
	default:
		panic("unhandled service " + service)
	}
	return path
}

func doorProbeBody(service string) string {
	switch service {
	case "CompassService":
		return `{"sessionId":"missing-session"}`
	case "RunnerService":
		return `{"runnerId":"missing-runner"}`
	case "GatewayCredentials":
		return `{"pool":"trace-probe"}`
	case "GatewayRegistry":
		return `{"name":"trace-probe"}`
	case "GatewayAuth":
		return `{"token":"trace-probe"}`
	default:
		return "{}"
	}
}

func assertOneServerSpan(t *testing.T, recorder *tracetest.SpanRecorder) {
	t.Helper()
	var serverSpans []sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			serverSpans = append(serverSpans, span)
		}
	}
	if len(serverSpans) != 1 {
		t.Fatalf("recorded server spans = %+v, want one", serverSpans)
	}
}
