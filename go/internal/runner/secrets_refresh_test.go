//go:build unix

package runner

// agentHost.RefreshSecrets: the SecretsVersion-driven fetch+materialize leg. A
// live session's secrets are fetched over the link and installed into its
// container over the stdin-exec channel (the git-credential posture). Every case
// pins a contract a plausible bug would break: a refresh for an unknown session
// must fail (not materialize into the wrong container); a bound session must run
// the materialize exec as the agent uid in its $HOME.

import (
	"context"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
	"strconv"
	"testing"
)

// newRefreshHostFixture builds an agentHost whose link serves FetchSecrets from
// the given fake server, plus a recording exec engine so a materialize's exec is
// observable. Returns the host and the engine.
func newRefreshHostFixture(t *testing.T, fetch *fakeFetchServer) (SessionHost, *recordingExecRuntime) {
	t.Helper()
	engine := newRecordingExecRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, fetch))
	var n int
	newID := func() string { n++; return "sess-" + strconv.Itoa(n) }
	cfg := AgentHostConfig{RuntimeDir: t.TempDir()}
	host := NewSessionHost(link, rt, registry, engine, cfg2SpecBuilder(), cfg, discardLoggerRunner(), newID)
	return host, engine
}

// cfg2SpecBuilder returns a spec builder producing the standard cont-1 spec.
func cfg2SpecBuilder() SpecBuilder { return &fakeSpecBuilder{spec: liveSpec()} }

// TestRefreshSecretsUnknownSessionErrors: a refresh for a session the host does
// not know fails, so a stray signal can never materialize into a wrong or
// non-existent container.
func TestRefreshSecretsUnknownSessionErrors(t *testing.T) {
	host, _ := newRefreshHostFixture(t, &fakeFetchServer{})
	err := host.RefreshSecrets(context.Background(), "no-such-session")
	if err == nil {
		t.Fatal("RefreshSecrets(unknown) = nil, want an error")
	}
}

// TestRefreshSecretsMaterializesForBoundSession: a live session's secret set is
// fetched and installed into its container over an exec running as the agent uid
// in its $HOME (the git-credential posture). A file secret in the set lands as a
// materialize exec carrying an sh -s stdin script.
func TestRefreshSecretsMaterializesForBoundSession(t *testing.T) {
	fetch := &fakeFetchServer{secrets: []*compassv1internal.ResolvedSecret{
		{
			Name: "DB_URL", Value: "postgres://db", Version: "v1",
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_FILE,
			Kind:     compassv1.SecretKind_SECRET_KIND_GENERIC,
		},
	}}
	host, engine := newRefreshHostFixture(t, fetch)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"})
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	if err := host.RefreshSecrets(ctx, sessionID); err != nil {
		t.Fatalf("RefreshSecrets = %v, want nil", err)
	}
	if fetch.lastSessionID != sessionID {
		t.Fatalf("FetchSecrets requested session %q, want the live session %q", fetch.lastSessionID, sessionID)
	}

	specs := engine.execSnapshot()
	var found bool
	for _, spec := range specs {
		if spec.Stdin == nil {
			continue
		}
		found = true
		if spec.User == nil || *spec.User != "1000" {
			t.Fatalf("materialize exec User = %v, want the agent uid 1000", spec.User)
		}
		if spec.Workdir == nil || *spec.Workdir != "/home/agent" {
			t.Fatalf("materialize exec Workdir = %v, want /home/agent", spec.Workdir)
		}
	}
	if !found {
		t.Fatal("no materialize exec (sh -s with a stdin script) ran for the bound session")
	}
}

// recordingExecRuntime is a ContainerRuntime whose ExecStreaming delegates to a
// real terminatable child (so Start's agent relay works) and whose one-shot Exec
// records the spec (so a materialize's exec is observable). It composes the
// stub-streaming child with an exec recorder.
type recordingExecRuntime struct {
	*stubStreamingRuntime
	execSpecsOneShot []runtime.ExecSpec
}

func newRecordingExecRuntime(t *testing.T) *recordingExecRuntime {
	t.Helper()
	return &recordingExecRuntime{stubStreamingRuntime: newStubStreamingRuntime(t)}
}

func (r *recordingExecRuntime) Exec(_ context.Context, _ runtime.ContainerID, spec runtime.ExecSpec) (runtime.ExecOutput, error) {
	r.mu.Lock()
	r.execSpecsOneShot = append(r.execSpecsOneShot, spec)
	r.mu.Unlock()
	return runtime.ExecOutput{}, nil
}

func (r *recordingExecRuntime) execSnapshot() []runtime.ExecSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.ExecSpec(nil), r.execSpecsOneShot...)
}
