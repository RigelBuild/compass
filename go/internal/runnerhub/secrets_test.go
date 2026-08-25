//go:build unix

package runnerhub

// FetchSecrets authz + no-log, and the SecretsVersion emit seam. Every case pins
// a behavior a plausible bug would break:
//   - FetchSecrets rejects a session NOT bound in the hub with CodePermissionDenied
//     (the record's foreign-session rejection), and returns the resolved set for a
//     bound live session — driven over the real wire through newMountedH2CServer.
//   - The resolved proto set never stringifies a value (the no-log posture).
//   - SignalSecretsVersion pushes a SecretsVersion to every live session with a
//     monotonic token that is neither the value nor its content hash.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeResolverSecrets is a hand-written secrets.Resolver: Resolve returns a fixed
// set (and records that it was called), Set/Delete are no-op successes. It lets
// the FetchSecrets seam test drive the resolve path without a real SecretSpec
// provider. resolveErr, when set, makes Resolve fail.
type fakeResolverSecrets struct {
	set          []secrets.ResolvedSecret
	resolveErr   error
	resolveCalls int
}

func (f *fakeResolverSecrets) Resolve(_ context.Context, _ string) ([]secrets.ResolvedSecret, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.set, nil
}

func (f *fakeResolverSecrets) Set(_ context.Context, _, _ string) error { return nil }
func (f *fakeResolverSecrets) Delete(_ context.Context, _ string) error { return nil }

// runnerResolverForFetch is the token resolver the FetchSecrets door uses: it
// accepts a single Runner token and rejects everything else, modelling the real
// kind-gate contract the seam tests already rely on.
func runnerResolverForFetch() *fakeResolver {
	return &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
}

// TestFetchSecretsUnboundSessionPermissionDenied pins the record's session-binding
// authz (§756-762, §964): a Runner requesting a session_id that is NOT a live
// session bound in the hub is rejected CodePermissionDenied — a foreign session
// can never pull the secret set.
func TestFetchSecretsUnboundSessionPermissionDenied(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	// No session bound: the hub has no live session for "sess-unbound".
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{{Name: "A", Value: "v"}}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{SessionId: "sess-unbound"}))
	if err == nil {
		t.Fatal("FetchSecrets for an unbound session = nil, want PermissionDenied")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("FetchSecrets unbound session code = %v, want PermissionDenied", got)
	}
	// The resolver must not be reached when authz fails.
	if resolver.resolveCalls != 0 {
		t.Fatalf("resolver.Resolve called %d times on a rejected fetch, want 0", resolver.resolveCalls)
	}
}

// TestFetchSecretsBoundSessionReturnsResolvedSet pins the happy path: a bound live
// session gets the resolved set, mapped to the proto ResolvedSecret with the
// delivery/kind enums translated at the edge.
func TestFetchSecretsBoundSessionReturnsResolvedSet(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-1")
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{
		{Name: "DB_URL", Value: "postgres://secret", Version: "v1", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
		{Name: "GH_TOKEN", Value: "ghp_x", Version: "v2", Delivery: secrets.DeliveryFile, Kind: secrets.SecretGH, Host: "github.com"},
	}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	resp, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{SessionId: "sess-1"}))
	if err != nil {
		t.Fatalf("FetchSecrets for a bound session = %v, want the resolved set", err)
	}
	got := resp.Msg.GetSecrets()
	if len(got) != 2 {
		t.Fatalf("resolved set len = %d, want 2", len(got))
	}
	if got[0].GetName() != "DB_URL" || got[0].GetValue() != "postgres://secret" || got[0].GetVersion() != "v1" {
		t.Fatalf("first resolved secret = %+v, want DB_URL/postgres://secret/v1", got[0])
	}
	if got[0].GetDelivery() != 2 { // SECRET_DELIVERY_ENV
		t.Fatalf("DB_URL delivery = %v, want ENV (2)", got[0].GetDelivery())
	}
	if got[1].GetKind() != 3 || got[1].GetHost() != "github.com" { // SECRET_KIND_GH
		t.Fatalf("GH_TOKEN kind/host = %v/%q, want GH (3)/github.com", got[1].GetKind(), got[1].GetHost())
	}
}

// TestFetchSecretsResolveErrorInternal: a bound live session whose resolve FAILS
// maps to CodeInternal (handler.go's "resolving secrets" path). This exercises the
// resolveErr seam on the fake resolver so a resolve fault is surfaced loudly, not
// swallowed as an empty set.
func TestFetchSecretsResolveErrorInternal(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-1")
	resolver := &fakeResolverSecrets{resolveErr: errors.New("resolve boom")}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{SessionId: "sess-1"}))
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("FetchSecrets with a failing resolve code = %v, want Internal", got)
	}
	if resolver.resolveCalls != 1 {
		t.Fatalf("resolver.Resolve called %d times, want 1", resolver.resolveCalls)
	}
}

// TestFetchSecretsByBoundContainerReturnsResolvedSet pins the pre-exec path: a
// container with a recorded container→account binding (the Provision..Start
// window, before any session) resolves the set via the container_name selector.
func TestFetchSecretsByBoundContainerReturnsResolvedSet(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("cont-1", testAgentAccount)
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{{Name: "A", Value: "v", Version: "v1"}}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	resp, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{ContainerName: "cont-1"}))
	if err != nil {
		t.Fatalf("FetchSecrets for a bound container = %v, want the resolved set", err)
	}
	if got := resp.Msg.GetSecrets(); len(got) != 1 || got[0].GetName() != "A" {
		t.Fatalf("resolved set = %+v, want the single secret A", got)
	}
}

// TestFetchSecretsUnboundContainerPermissionDenied: a container_name with no
// recorded binding is rejected CodePermissionDenied, and the resolver is never
// reached — the pre-exec analogue of the unbound-session rejection.
func TestFetchSecretsUnboundContainerPermissionDenied(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{{Name: "A", Value: "v"}}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{ContainerName: "cont-unbound"}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("FetchSecrets unbound container code = %v, want PermissionDenied", got)
	}
	if resolver.resolveCalls != 0 {
		t.Fatalf("resolver.Resolve called %d times on a rejected fetch, want 0", resolver.resolveCalls)
	}
}

// TestFetchSecretsMissingSelectorInvalidArgument: a request with no selector set
// is CodeInvalidArgument — a contract skew, never a silent empty set.
func TestFetchSecretsMissingSelectorInvalidArgument(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{{Name: "A", Value: "v"}}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("FetchSecrets with no selector code = %v, want InvalidArgument", got)
	}
	if resolver.resolveCalls != 0 {
		t.Fatalf("resolver.Resolve called %d times on a rejected fetch, want 0", resolver.resolveCalls)
	}
}

// TestFetchSecretsBothSelectorsInvalidArgument: the flat selector shape lets a
// caller set both fields on the wire, so the handler must reject that as an
// ambiguous request (CodeInvalidArgument) rather than silently preferring one.
func TestFetchSecretsBothSelectorsInvalidArgument(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("cont-1", testAgentAccount)
	bindSession(hub, "sess-1")
	resolver := &fakeResolverSecrets{set: []secrets.ResolvedSecret{{Name: "A", Value: "v"}}}
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, resolver)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{SessionId: "sess-1", ContainerName: "cont-1"}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("FetchSecrets with both selectors code = %v, want InvalidArgument", got)
	}
	if resolver.resolveCalls != 0 {
		t.Fatalf("resolver.Resolve called %d times on a rejected fetch, want 0", resolver.resolveCalls)
	}
}

// TestFetchSecretsNoResolverFailedPrecondition: a handler with no resolver wired
// fails closed with CodeFailedPrecondition rather than returning an empty set as
// if there were no secrets — a wiring bug must be loud. The code is deliberately
// distinct from the CodeUnavailable connect-go synthesizes for a transport
// fault, so the Runner can tolerate a genuine no-secrets-surface server at Start
// without also tolerating a transient outage as "no secrets".
func TestFetchSecretsNoResolverFailedPrecondition(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-1")
	url := newMountedH2CServerWithResolver(t, hub, runnerResolverForFetch().resolve, nil)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.FetchSecrets(context.Background(), connect.NewRequest(&compassv1internal.FetchSecretsRequest{SessionId: "sess-1"}))
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("FetchSecrets with no resolver code = %v, want FailedPrecondition", got)
	}
}

// TestResolvedSecretMappingRedactsValue pins the no-log posture at the mapping
// edge: the resolve-surface ResolvedSecret redacts its value under %v/%s (its
// String omits value AND version), so a mapped set logged by accident cannot leak
// a value. This asserts the source type the handler maps FROM does not stringify.
func TestResolvedSecretMappingRedactsValue(t *testing.T) {
	s := secrets.ResolvedSecret{Name: "DB_URL", Value: "postgres://secret", Version: "deadbeef"}
	for _, verb := range []string{"%v", "%s", "%#v"} {
		out := fmt.Sprintf(verb, s)
		if strings.Contains(out, "postgres://secret") {
			t.Fatalf("ResolvedSecret under %s leaked its value: %q", verb, out)
		}
		if strings.Contains(out, "deadbeef") {
			t.Fatalf("ResolvedSecret under %s leaked its version: %q", verb, out)
		}
	}
}

// TestSignalSecretsVersionPushesMonotonicToken pins the emit seam: after binding
// two live sessions and attaching a live send, SignalSecretsVersion pushes a
// SecretsVersion to EACH, the token is monotonic across calls (a second call
// yields a strictly greater token), and the token is neither the value nor a
// content hash — an opaque counter.
func TestSignalSecretsVersionPushesMonotonicToken(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-a")
	bindSession(hub, "sess-b")
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	rec := newRecordingSend()
	router.attach(rec.send)
	defer router.detach(errStreamClosed)

	if err := hub.SignalSecretsVersion(); err != nil {
		t.Fatalf("SignalSecretsVersion #1 = %v, want nil", err)
	}
	// Signals are queued-not-pushed; gate on the sender draining both to the wire.
	waitRecorded(t, rec, 2)
	first := secretsVersionsPushed(t, rec)
	// One signal per live session.
	if len(first) != 2 {
		t.Fatalf("first signal pushed %d SecretsVersion frames, want 2 (one per live session)", len(first))
	}
	// Every frame in one call carries the SAME token, addressed per session.
	sessions := map[string]bool{}
	token1 := first[0].GetVersion()
	for _, v := range first {
		if v.GetVersion() != token1 {
			t.Fatalf("SecretsVersion tokens differ within one call: %q vs %q", v.GetVersion(), token1)
		}
		sessions[v.GetSessionId()] = true
	}
	if !sessions["sess-a"] || !sessions["sess-b"] {
		t.Fatalf("signalled sessions = %v, want both sess-a and sess-b", sessions)
	}

	if err := hub.SignalSecretsVersion(); err != nil {
		t.Fatalf("SignalSecretsVersion #2 = %v, want nil", err)
	}
	waitRecorded(t, rec, 3)
	all := secretsVersionsPushed(t, rec)
	token2 := all[len(all)-1].GetVersion()
	if token2 == token1 {
		t.Fatalf("second version token = %q, want a DIFFERENT (greater) token than %q", token2, token1)
	}
	if !tokenGreater(t, token2, token1) {
		t.Fatalf("version token not monotonic: %q is not greater than %q", token2, token1)
	}
	// Opaque: the token is not a value nor a content-hash of any value.
	if strings.Contains(token1, "sess") || len(token1) == 64 /* sha256 hex */ {
		t.Fatalf("version token %q looks value-derived, want an opaque counter", token1)
	}
}

// TestSignalSecretsVersionNoLiveSessionsIsNoop: a signal with no live sessions
// (nothing bound) pushes nothing and is a clean success — a bump with no one to
// notify is not an error.
func TestSignalSecretsVersionNoLiveSessionsIsNoop(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, _ := hub.routerFor("any")
	rec := newRecordingSend()
	router.attach(rec.send)
	defer router.detach(errStreamClosed)

	if err := hub.SignalSecretsVersion(); err != nil {
		t.Fatalf("SignalSecretsVersion with no live sessions = %v, want nil", err)
	}
	if got := len(secretsVersionsPushed(t, rec)); got != 0 {
		t.Fatalf("pushed %d frames with no live sessions, want 0", got)
	}
}

// TestSignalSecretsVersionNoRunnerIsNoop: with no Runner enrolled at all, a signal
// is a clean no-op (best-effort — the Runner re-fetches on reconnect).
func TestSignalSecretsVersionNoRunnerIsNoop(t *testing.T) {
	hub := newHubOnly() // no enroll
	if err := hub.SignalSecretsVersion(); err != nil {
		t.Fatalf("SignalSecretsVersion with no runner = %v, want nil", err)
	}
}

// secretsVersionsPushed extracts every SecretsVersion command the recorder saw.
func secretsVersionsPushed(t *testing.T, rec *recordingSend) []*compassv1internal.SecretsVersion {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []*compassv1internal.SecretsVersion
	for _, cmd := range rec.sent {
		if sv := cmd.GetSecretsVersion(); sv != nil {
			out = append(out, sv)
		}
	}
	return out
}

// tokenGreater reports whether the decimal token b is numerically greater than a.
func tokenGreater(t *testing.T, b, a string) bool {
	t.Helper()
	bi, aErr := parseUint(b)
	ai, bErr := parseUint(a)
	if aErr != nil || bErr != nil {
		t.Fatalf("version tokens not decimal: %q, %q", a, b)
	}
	return bi > ai
}

func parseUint(s string) (uint64, error) {
	var n uint64
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("non-digit")
		}
		n = n*10 + uint64(r-'0')
	}
	return n, nil
}
