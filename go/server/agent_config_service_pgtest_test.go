//go:build pgtest && unix

package server

// Store-gated CompassService config-declaration handler contracts (RIG-1625 T2):
// PutAgentConfig persists + returns the store version and emits a ConfigVersion
// signal carrying that version; GetAgentConfigInfo returns an empty-but-valid
// response on an unconfigured fleet and the bucketed member NAMES (never content)
// on a configured one; DeleteAgentConfig clears the fleet and emits an
// EMPTY-version signal. They need a real Postgres because PutAgentConfig writes
// the singleton row and the handler reads a genuine caller identity. Driven
// through the production bearer interceptor over a real connect client so the
// handler resolves the caller the same way the shipped door supplies it. The
// signaler is a recorder so a test asserts the emit path fired with the right
// version. Behind `pgtest && unix` (SKIP when no runtime).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// recordingConfigSignaler records SignalConfigVersion calls so a test asserts a
// successful Put/Delete emitted the right version to live sessions.
type recordingConfigSignaler struct {
	versions []string
}

func (r *recordingConfigSignaler) SignalConfigVersion(version string) error {
	r.versions = append(r.versions, version)
	return nil
}

// configFixture stands up the CompassService behind the production bearer chain
// with a recording config signaler wired in, and returns the client, the caller's
// bearer token, and the recorder + store the test asserts against.
type configFixture struct {
	client   compassv1connect.CompassServiceClient
	token    string
	signaler *recordingConfigSignaler
	store    *store.Store
}

func newConfigFixture(t *testing.T) configFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	user, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "operator", DisplayName: "operator"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	tok, err := auth.IssueAccountToken(ctx, st, user.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("config-test", bus, st, nil, nil, nil, nil)
	signaler := &recordingConfigSignaler{}
	svc.signaler = signaler // white-box: inject the recorder as the emit sink

	url := newH2CTestServerWithInterceptors(t, svc, auth.BearerInterceptor(st))
	return configFixture{
		client:   newH2CClient(t, url),
		token:    tok,
		signaler: signaler,
		store:    st,
	}
}

// authReq wraps a request with the fixture caller's bearer token.
func authReq[T any](f configFixture, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+f.token)
	return req
}

// mkConfigBundle gzip-tars a set of (name, content) files for a handler test.
func mkConfigBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("write header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestPutAgentConfigReturnsVersionAndSignals: a valid bundle is persisted, the
// handler returns the store's content version, and the same version is emitted on
// the ConfigVersion signal (live Runners re-fetch).
func TestPutAgentConfigReturnsVersionAndSignals(t *testing.T) {
	f := newConfigFixture(t)
	bundle := mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review"})

	resp, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: bundle}))
	if err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	if resp.Msg.GetVersion() == "" {
		t.Fatal("PutAgentConfig returned an empty version")
	}
	if len(f.signaler.versions) != 1 {
		t.Fatalf("signaler saw %d emits, want 1", len(f.signaler.versions))
	}
	if f.signaler.versions[0] != resp.Msg.GetVersion() {
		t.Fatalf("signalled version %q != returned version %q", f.signaler.versions[0], resp.Msg.GetVersion())
	}
}

// TestPutAgentConfigIdempotentRePutDoesNotResignal: a re-Put of byte-identical
// content is idempotent at the content-hash store — it yields the same version and
// replaces the row in place, so the handler must NOT re-prod the fleet with a
// second ConfigVersion signal for a version it already holds.
func TestPutAgentConfigIdempotentRePutDoesNotResignal(t *testing.T) {
	f := newConfigFixture(t)
	bundle := mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review"})

	resp, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: bundle}))
	if err != nil {
		t.Fatalf("PutAgentConfig (first): %v", err)
	}
	v := resp.Msg.GetVersion()

	resp2, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: bundle}))
	if err != nil {
		t.Fatalf("PutAgentConfig (re-Put): %v", err)
	}
	if resp2.Msg.GetVersion() != v {
		t.Fatalf("re-Put version %q != first version %q", resp2.Msg.GetVersion(), v)
	}
	if len(f.signaler.versions) != 1 {
		t.Fatalf("signaler saw %d emits after an identical re-Put, want 1", len(f.signaler.versions))
	}
}

// TestPutAgentConfigChangedContentResignals: a Put whose content DIFFERS from the
// stored bundle yields a new version and MUST re-prod the fleet — this pins the
// guard's signalling true-branch over a real (non-empty) prior version, the exact
// path a dedupe regression would silently break (leaving the fleet on stale config
// while the idempotent-case test stays green).
func TestPutAgentConfigChangedContentResignals(t *testing.T) {
	f := newConfigFixture(t)

	respA, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review"})}))
	if err != nil {
		t.Fatalf("PutAgentConfig (A): %v", err)
	}
	v1 := respA.Msg.GetVersion()

	respB, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review v2"})}))
	if err != nil {
		t.Fatalf("PutAgentConfig (B): %v", err)
	}
	v2 := respB.Msg.GetVersion()

	if v2 == v1 {
		t.Fatalf("changed content yielded the same version %q — expected a new one", v2)
	}
	if len(f.signaler.versions) != 2 {
		t.Fatalf("signaler saw %d emits after a changed-content Put, want 2", len(f.signaler.versions))
	}
	if f.signaler.versions[1] != v2 {
		t.Fatalf("second signal version %q != changed-content version %q", f.signaler.versions[1], v2)
	}
}

// TestPutAgentConfigInvalidBundleIsInvalidArgument: a bundle that fails the store
// door (not a gzip tarball) is CodeInvalidArgument, and no signal fires.
func TestPutAgentConfigInvalidBundleIsInvalidArgument(t *testing.T) {
	f := newConfigFixture(t)
	_, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: []byte("not a gzip tarball")}))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("invalid bundle code = %v, want InvalidArgument", code)
	}
	if len(f.signaler.versions) != 0 {
		t.Fatalf("signaler fired on a rejected Put: %v", f.signaler.versions)
	}
}

// TestGetAgentConfigInfoUnconfiguredIsEmpty: an unconfigured fleet reports an
// empty-but-valid response (empty version, empty name lists), never an error.
func TestGetAgentConfigInfoUnconfiguredIsEmpty(t *testing.T) {
	f := newConfigFixture(t)
	resp, err := f.client.GetAgentConfigInfo(context.Background(),
		authReq(f, &compassv1.GetAgentConfigInfoRequest{}))
	if err != nil {
		t.Fatalf("GetAgentConfigInfo on unconfigured fleet: %v", err)
	}
	if resp.Msg.GetVersion() != "" ||
		len(resp.Msg.GetSkills()) != 0 ||
		len(resp.Msg.GetExtensions()) != 0 ||
		len(resp.Msg.GetMcpServers()) != 0 {
		t.Fatalf("unconfigured info = %+v, want all-empty", resp.Msg)
	}
}

// TestGetAgentConfigInfoReturnsBucketedNames: a configured bundle reports its
// version and the member NAMES bucketed by top dir — never content.
func TestGetAgentConfigInfoReturnsBucketedNames(t *testing.T) {
	f := newConfigFixture(t)
	bundle := mkConfigBundle(t, map[string]string{
		"skills/review/SKILL.md":       "# review",
		"skills/triage/SKILL.md":       "# triage",
		"extensions/cotal/main.js":     "x",
		"mcp/linear.json":              `{"a":1}`,
		"prompts/supervisor/SYSTEM.md": "# supervisor",
		"prompts/owner/SYSTEM.md":      "# owner",
	})
	put, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: bundle}))
	if err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}

	resp, err := f.client.GetAgentConfigInfo(context.Background(),
		authReq(f, &compassv1.GetAgentConfigInfoRequest{}))
	if err != nil {
		t.Fatalf("GetAgentConfigInfo: %v", err)
	}
	if resp.Msg.GetVersion() != put.Msg.GetVersion() {
		t.Errorf("info version %q != put version %q", resp.Msg.GetVersion(), put.Msg.GetVersion())
	}
	if got := join(resp.Msg.GetSkills()); got != "review,triage" {
		t.Errorf("skills = %q, want review,triage", got)
	}
	if got := join(resp.Msg.GetExtensions()); got != "cotal" {
		t.Errorf("extensions = %q, want cotal", got)
	}
	if got := join(resp.Msg.GetMcpServers()); got != "linear" {
		t.Errorf("mcp_servers = %q, want linear", got)
	}
	if got := join(resp.Msg.GetPrompts()); got != "owner,supervisor" {
		t.Errorf("prompts = %q, want owner,supervisor", got)
	}
}

// TestDeleteAgentConfigClearsAndSignalsEmpty: Delete clears the fleet (a
// subsequent GetInfo is empty) and emits an EMPTY-version ConfigVersion signal.
func TestDeleteAgentConfigClearsAndSignalsEmpty(t *testing.T) {
	f := newConfigFixture(t)
	bundle := mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review"})
	if _, err := f.client.PutAgentConfig(context.Background(),
		authReq(f, &compassv1.PutAgentConfigRequest{Bundle: bundle})); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}

	if _, err := f.client.DeleteAgentConfig(context.Background(),
		authReq(f, &compassv1.DeleteAgentConfigRequest{})); err != nil {
		t.Fatalf("DeleteAgentConfig: %v", err)
	}
	// The Put emitted the content version; the Delete emitted the empty marker.
	if n := len(f.signaler.versions); n != 2 {
		t.Fatalf("signaler saw %d emits, want 2 (put + delete)", n)
	}
	if last := f.signaler.versions[1]; last != "" {
		t.Fatalf("delete emitted version %q, want the empty cleared-marker", last)
	}

	resp, err := f.client.GetAgentConfigInfo(context.Background(),
		authReq(f, &compassv1.GetAgentConfigInfoRequest{}))
	if err != nil {
		t.Fatalf("GetAgentConfigInfo after delete: %v", err)
	}
	if resp.Msg.GetVersion() != "" || len(resp.Msg.GetSkills()) != 0 {
		t.Fatalf("info after delete = %+v, want empty", resp.Msg)
	}
}

// TestAgentConfigWriteRPCsDenyNonAdminToken pins the network-door admin gate for
// the two config-write RPCs: a valid NON-admin (member) bearer is denied
// CodePermissionDenied on both PutAgentConfig and DeleteAgentConfig. The gate
// short-circuits BEFORE the handler runs (so the bundle is never validated on
// this path), which is why it uses the full network-door chain via
// networkDoorHandler rather than the handler-contract newConfigFixture (that
// fixture installs only the bearer interceptor, not the AdminGate). Mirrors the
// per-RPC denial PATTERN of the SecretsService agent-token tests — but asserts
// the error code only, since the gate denies before the handler here, whereas
// those deny inside the handler and so also assert no side effect. This reddens
// if the gate ever stops classifying either RPC adminOnly.
func TestAgentConfigWriteRPCsDenyNonAdminToken(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)
	memberTok, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("rig1658-denial-test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	t.Run("PutAgentConfig as non-admin is PermissionDenied", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.PutAgentConfigRequest{
			Bundle: mkConfigBundle(t, map[string]string{"skills/review/SKILL.md": "# review"}),
		})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.PutAgentConfig(ctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin token on PutAgentConfig = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})

	t.Run("DeleteAgentConfig as non-admin is PermissionDenied", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.DeleteAgentConfigRequest{})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.DeleteAgentConfig(ctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin token on DeleteAgentConfig = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
	})
}

// join renders a name slice for a stable equality assertion.
func join(names []string) string {
	return strings.Join(names, ",")
}
