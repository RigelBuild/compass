//go:build pgtest && unix

package server

// Store-gated SecretsService authz contracts: the user-only Set/Delete gate
// (the load-bearing regression, record §927), the user-AND-agent ListSecrets, the
// is_set-without-resolve invariant, delete-not-found, and the SecretsVersion bump
// on a successful write. They need a real Postgres because the write path declares
// a registry row and the authz gate reads the caller's account KIND from the store
// (user vs agent). Driven through the production bearer + admin-gate interceptor
// chain over a real connect client so the handler reads a genuine caller identity
// the same way the shipped door supplies it. Behind `pgtest && unix` (SKIP when no
// runtime).
//
// The resolver is a fake (no real SecretSpec provider in a unit test): Set/Delete
// are recorded no-ops, and Resolve FAILS LOUDLY if ListSecrets ever calls it — the
// is_set-without-resolve invariant is asserted by that fake, not just by reading
// the code.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// recordingResolver is a fake secrets.Resolver for the SecretsService tests. Set
// and Delete record their calls and succeed; Resolve fails loudly, so a test that
// wires this into ListSecrets proves the list path never resolves values to
// compute is_set (record §906-908 / brief item 7). Statuses returns a scripted
// value-free report — the server-secret list path's probe — and can be scripted
// to fail so a test proves a provider fault is not flattened into all-unset.
type recordingResolver struct {
	setErr      error
	setNames    []string
	setReasons  []string
	deleteNames []string
	resolveHit  bool
	statuses    []secrets.SecretStatus
	statusesErr error
	statusHit   bool
}

func (r *recordingResolver) Statuses(_ context.Context, _ string) ([]secrets.SecretStatus, error) {
	r.statusHit = true
	if r.statusesErr != nil {
		return nil, r.statusesErr
	}
	return r.statuses, nil
}

func (r *recordingResolver) Resolve(_ context.Context, _ string) ([]secrets.ResolvedSecret, error) {
	r.resolveHit = true
	return nil, errors.New("ListSecrets must not resolve values")
}

func (r *recordingResolver) Set(_ context.Context, name, _, reason string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.setNames = append(r.setNames, name)
	r.setReasons = append(r.setReasons, reason)
	return nil
}

func (r *recordingResolver) Delete(_ context.Context, name string) error {
	r.deleteNames = append(r.deleteNames, name)
	return nil
}

// recordingSignaler records SignalSecretsVersion calls so a test asserts a
// successful Set/Delete bumped the secrets version.
type recordingSignaler struct {
	calls int
}

func (r *recordingSignaler) SignalSecretsVersion() error {
	r.calls++
	return nil
}

// secretsFixture seeds a user and an agent (owned by the user), stands up the
// SecretsService behind the production bearer + admin-gate chain, and returns the
// wired client, bearer tokens for each, and the fake resolver/signaler the test
// asserts against.
type secretsFixture struct {
	client     compassv1connect.SecretsServiceClient
	userToken  string
	agentToken string
	userID     store.AccountID
	agentID    store.AccountID
	adminToken string
	// st is the live store, so a scope test can assert WHICH coordinate a write
	// landed at rather than only that the RPC returned OK.
	st       *store.Store
	resolver *recordingResolver
	// serverResolver is the SECOND fake, standing in for the server-secret
	// resolver. Kept distinct from resolver so a test can prove a server-secret
	// write lands ONLY on this one — the container-delivery registry must never
	// see it.
	serverResolver *recordingResolver
	signaler       *recordingSignaler
}

func newSecretsFixture(t *testing.T) secretsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "user", DisplayName: "user"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, store.NewAgent{Handle: "agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	userTok, err := auth.IssueAccountToken(ctx, st, user.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(user): %v", err)
	}
	agentTok, err := auth.IssueAccountToken(ctx, st, agent.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(agent): %v", err)
	}

	resolver := &recordingResolver{}
	serverResolver := &recordingResolver{}
	signaler := &recordingSignaler{}
	svc := newSecretsService(st, resolver, serverResolver, signaler)
	url := newSecretsH2CServer(t, svc,
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(admin.ID),
	)
	adminTok, err := auth.IssueAccountToken(ctx, st, admin.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}
	return secretsFixture{
		client:         newSecretsH2CClient(t, url),
		userToken:      userTok,
		agentToken:     agentTok,
		adminToken:     adminTok,
		userID:         user.ID,
		agentID:        agent.ID,
		st:             st,
		resolver:       resolver,
		serverResolver: serverResolver,
		signaler:       signaler,
	}
}

func setReq(bearer, name, value string) *connect.Request[compassv1.SetSecretRequest] {
	req := connect.NewRequest(&compassv1.SetSecretRequest{
		Name:     name,
		Value:    value,
		Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_ENV,
		Kind:     compassv1.SecretKind_SECRET_KIND_GENERIC,
	})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

func delReq(bearer, name string) *connect.Request[compassv1.DeleteSecretRequest] {
	req := connect.NewRequest(&compassv1.DeleteSecretRequest{Name: name})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

// scopedSetReq is setReq with an explicit D9 scope selector. setReq deliberately
// leaves scope unset, so it exercises the unspecified-means-user default.
func scopedSetReq(bearer, name, value string, scope compassv1.SecretScope) *connect.Request[compassv1.SetSecretRequest] {
	req := setReq(bearer, name, value)
	req.Msg.Scope = scope
	return req
}

func scopedDelReq(bearer, name string, scope compassv1.SecretScope) *connect.Request[compassv1.DeleteSecretRequest] {
	req := delReq(bearer, name)
	req.Msg.Scope = scope
	return req
}

func listReq(bearer string) *connect.Request[compassv1.ListSecretsRequest] {
	req := connect.NewRequest(&compassv1.ListSecretsRequest{})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

// TestSetSecretUserOnly is the load-bearing regression (record §927): an
// AGENT-token caller is CodePermissionDenied, a USER-token caller succeeds. The
// agent must never write a secret.
func TestSetSecretUserOnly(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	// Agent: rejected, and the resolver is never reached (no value written).
	_, err := f.client.SetSecret(ctx, setReq(f.agentToken, "AGENT_TRY", "v"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("SetSecret as agent code = %v, want PermissionDenied", got)
	}
	if len(f.resolver.setNames) != 0 {
		t.Fatalf("resolver.Set called %v on a rejected agent SetSecret, want none", f.resolver.setNames)
	}

	// User: succeeds and writes the value. The literal is hoisted because the
	// value-absence assertion below asserts on it — inlining it twice lets the
	// two drift, silently retiring that assertion.
	const secretValue = "postgres://x"
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", secretValue)); err != nil {
		t.Fatalf("SetSecret as user = %v, want success", err)
	}
	if len(f.resolver.setNames) != 1 || f.resolver.setNames[0] != "DB_URL" {
		t.Fatalf("resolver.Set names = %v, want [DB_URL]", f.resolver.setNames)
	}
	// The handler must hand the resolver a non-empty reason bound to the
	// AUTHENTICATED caller: the provider's require_reason policy refuses a
	// reasonless write outright, and the audit record is only useful if it names
	// which operator wrote the secret. The reason must never carry the value.
	if len(f.resolver.setReasons) != 1 || strings.TrimSpace(f.resolver.setReasons[0]) == "" {
		t.Fatalf("resolver.Set reasons = %q, want one non-empty reason", f.resolver.setReasons)
	}
	if !strings.Contains(f.resolver.setReasons[0], string(f.userID)) {
		t.Fatalf("resolver.Set reason = %q, want it to name the calling user %q", f.resolver.setReasons[0], f.userID)
	}
	if strings.Contains(f.resolver.setReasons[0], secretValue) {
		t.Fatalf("resolver.Set reason = %q, must never carry the secret value", f.resolver.setReasons[0])
	}
}

// TestSetSecretBumpsSecretsVersion: a successful Set bumps the secrets version
// (signals live sessions to re-fetch), a rejected one does not.
func TestSetSecretBumpsSecretsVersion(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	if _, err := f.client.SetSecret(ctx, setReq(f.agentToken, "NOPE", "v")); err == nil {
		t.Fatal("SetSecret as agent = nil, want PermissionDenied")
	}
	if f.signaler.calls != 0 {
		t.Fatalf("secrets version bumped %d times on a rejected write, want 0", f.signaler.calls)
	}
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v")); err != nil {
		t.Fatalf("SetSecret as user = %v, want success", err)
	}
	if f.signaler.calls != 1 {
		t.Fatalf("secrets version bumped %d times after a successful Set, want 1", f.signaler.calls)
	}
}

// TestSetSecretReSetRewrites: a re-Set of an already-declared name is a value
// rewrite (declare returns ErrConflict; the handler proceeds to resolver.Set), not
// a failure — the brief's conflict policy.
func TestSetSecretReSetRewrites(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v1")); err != nil {
		t.Fatalf("first SetSecret = %v, want success", err)
	}
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v2")); err != nil {
		t.Fatalf("re-SetSecret (value rewrite) = %v, want success", err)
	}
	if len(f.resolver.setNames) != 2 {
		t.Fatalf("resolver.Set called %d times across two Sets, want 2", len(f.resolver.setNames))
	}
}

// TestSetSecretRollsBackDeclarationOnWriteFailure: a FRESH declaration whose
// resolver.Set fails (a provider/exec fault) is rolled back — the RPC returns
// CodeUnavailable and no orphaned declaration survives. An orphan would be
// required=true in the resolve manifest and poison EVERY live session's
// FetchSecrets, so the surface must be left clean. A follow-up re-Set (once the
// provider recovers) then succeeds and the name appears, proving the failure left
// nothing behind.
func TestSetSecretRollsBackDeclarationOnWriteFailure(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	f.resolver.setErr = errors.New("provider down")

	_, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v"))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("SetSecret with a failing provider write code = %v, want Unavailable", got)
	}

	// The failed fresh write must leave no orphaned declaration behind.
	resp, err := f.client.ListSecrets(ctx, listReq(f.userToken))
	if err != nil {
		t.Fatalf("ListSecrets = %v, want success", err)
	}
	for _, s := range resp.Msg.GetSecrets() {
		if s.GetName() == "DB_URL" {
			t.Fatal("declaration survived a failed fresh write — orphan left behind")
		}
	}

	// Provider recovers: a re-Set of the same name now succeeds and appears,
	// proving the earlier failure left the surface clean (a fresh declaration).
	f.resolver.setErr = nil
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v")); err != nil {
		t.Fatalf("re-SetSecret after provider recovery = %v, want success", err)
	}
	resp, err = f.client.ListSecrets(ctx, listReq(f.userToken))
	if err != nil {
		t.Fatalf("ListSecrets after recovery = %v, want success", err)
	}
	var found bool
	for _, s := range resp.Msg.GetSecrets() {
		if s.GetName() == "DB_URL" {
			found = true
		}
	}
	if !found {
		t.Fatal("DB_URL absent after a successful re-Set, want present")
	}
}

// TestDeleteSecretUserOnly: an AGENT-token caller is CodePermissionDenied, a
// USER-token caller deletes a declared secret and bumps the version.
func TestDeleteSecretUserOnly(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	// Seed a declared secret as the user.
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v")); err != nil {
		t.Fatalf("seed SetSecret = %v", err)
	}
	versionBefore := f.signaler.calls

	// Agent: rejected, and the row survives (resolver.Delete never reached).
	_, err := f.client.DeleteSecret(ctx, delReq(f.agentToken, "DB_URL"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("DeleteSecret as agent code = %v, want PermissionDenied", got)
	}
	if len(f.resolver.deleteNames) != 0 {
		t.Fatalf("resolver.Delete called %v on a rejected agent DeleteSecret, want none", f.resolver.deleteNames)
	}

	// User: succeeds and bumps the version.
	if _, err := f.client.DeleteSecret(ctx, delReq(f.userToken, "DB_URL")); err != nil {
		t.Fatalf("DeleteSecret as user = %v, want success", err)
	}
	if f.signaler.calls != versionBefore+1 {
		t.Fatalf("secrets version bumped to %d after Delete, want %d", f.signaler.calls, versionBefore+1)
	}
}

// TestDeleteSecretNotFound: deleting a name that was never declared is
// CodeNotFound.
func TestDeleteSecretNotFound(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	_, err := f.client.DeleteSecret(ctx, delReq(f.userToken, "NEVER_DECLARED"))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("DeleteSecret of an undeclared name code = %v, want NotFound", got)
	}
}

// TestListSecretsUserAndAgent: both a user and an agent token succeed and get
// value-free SecretStatus with is_set=true for a declared row — and the resolver's
// Resolve is NEVER called (is_set is computed without fetching values).
func TestListSecretsUserAndAgent(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v")); err != nil {
		t.Fatalf("seed SetSecret = %v", err)
	}

	for _, tc := range []struct {
		name   string
		bearer string
	}{
		{"user", f.userToken},
		{"agent", f.agentToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.client.ListSecrets(ctx, listReq(tc.bearer))
			if err != nil {
				t.Fatalf("ListSecrets as %s = %v, want success", tc.name, err)
			}
			got := resp.Msg.GetSecrets()
			if len(got) != 1 {
				t.Fatalf("ListSecrets returned %d statuses, want 1", len(got))
			}
			s := got[0]
			if s.GetName() != "DB_URL" {
				t.Fatalf("status name = %q, want DB_URL", s.GetName())
			}
			if !s.GetIsSet() {
				t.Fatalf("status is_set = false, want true for a declared row")
			}
			if s.GetDelivery() != compassv1.SecretDelivery_SECRET_DELIVERY_ENV {
				t.Fatalf("status delivery = %v, want ENV", s.GetDelivery())
			}
		})
	}
	// The is_set-without-resolve invariant: ListSecrets never resolved any value.
	if f.resolver.resolveHit {
		t.Fatal("ListSecrets resolved values to compute is_set — must not fetch secrets to list them")
	}
}

func setServerReq(bearer, name, value string) *connect.Request[compassv1.SetServerSecretRequest] {
	req := connect.NewRequest(&compassv1.SetServerSecretRequest{Name: name, Value: value})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

func delServerReq(bearer, name string) *connect.Request[compassv1.DeleteServerSecretRequest] {
	req := connect.NewRequest(&compassv1.DeleteServerSecretRequest{Name: name})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

// TestSetServerSecretAdminOnly is the door-gate contract: the server-secret
// writes are ADMIN-only, unlike their user-facing siblings. A plain user token
// is refused even though it is a perfectly valid caller for SetSecret — a
// non-admin must never write a deployment-owned secret.
func TestSetServerSecretAdminOnly(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, token string }{
		{"user", f.userToken},
		{"agent", f.agentToken},
	} {
		_, err := f.client.SetServerSecret(ctx, setServerReq(tc.token, "SERVER_WEBHOOK_SECRET", "v"))
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("%s token: want CodePermissionDenied, got %v (err=%v)", tc.name, got, err)
		}
	}

	if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_WEBHOOK_SECRET", "v")); err != nil {
		t.Fatalf("admin token: %v", err)
	}
}

// TestSetServerSecretWritesOnlyTheServerResolver is the isolation proof (D6): a
// server-secret write must land on the SERVER resolver and never on the user
// resolver, whose manifest feeds the inject-all container-delivery path. A
// single shared resolver would deliver the deployment's App PEMs into every
// agent container.
func TestSetServerSecretWritesOnlyTheServerResolver(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_LINEAR_FORGE_CLIENT_SECRET", "v")); err != nil {
		t.Fatalf("SetServerSecret: %v", err)
	}
	if len(f.serverResolver.setNames) != 1 || f.serverResolver.setNames[0] != "SERVER_LINEAR_FORGE_CLIENT_SECRET" {
		t.Fatalf("server resolver sets = %v, want the one server secret", f.serverResolver.setNames)
	}
	if len(f.resolver.setNames) != 0 {
		t.Fatalf("user resolver was written: %v — a server secret must never reach the container registry", f.resolver.setNames)
	}

	// And it must not appear in the user-facing list, which is what the
	// container-delivery path and the CLI both read.
	resp, err := f.client.ListSecrets(ctx, listReq(f.userToken))
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	for _, s := range resp.Msg.GetSecrets() {
		if s.GetName() == "SERVER_LINEAR_FORGE_CLIENT_SECRET" {
			t.Fatal("server secret leaked into ListSecrets")
		}
	}
}

// TestServerSecretRejectsUnprefixedAndMasterKey pins the two argument guards:
// an unprefixed name cannot enter the server registry (it belongs to the user
// keyspace), and the reserved master key is never settable or deletable through
// the operator door — clobbering it would strand every encrypted row.
func TestServerSecretRejectsUnprefixedAndMasterKey(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	_, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "PLAIN_NAME", "v"))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("unprefixed: want CodeInvalidArgument, got %v (err=%v)", got, err)
	}
	if len(f.serverResolver.setNames) != 0 {
		t.Fatalf("unprefixed name reached the resolver: %v", f.serverResolver.setNames)
	}

	for _, call := range []struct {
		name string
		do   func() error
	}{
		{"set", func() error {
			_, e := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, store.MasterKeyName, "v"))
			return e
		}},
		{"delete", func() error {
			_, e := f.client.DeleteServerSecret(ctx, delServerReq(f.adminToken, store.MasterKeyName))
			return e
		}},
	} {
		if got := connect.CodeOf(call.do()); got != connect.CodeInvalidArgument {
			t.Fatalf("master-key %s: want CodeInvalidArgument, got %v", call.name, got)
		}
	}
	if len(f.serverResolver.setNames) != 0 || len(f.serverResolver.deleteNames) != 0 {
		t.Fatalf("master key reached the resolver: sets=%v deletes=%v",
			f.serverResolver.setNames, f.serverResolver.deleteNames)
	}
}

// TestSetServerSecretRollsBackDeclarationOnWriteFailure mirrors the user path's
// rollback discipline: a failed FRESH provider write must leave no orphan
// declaration behind, because an orphan is required=true in the resolve
// manifest and would fail the server's own boot resolve.
func TestSetServerSecretRollsBackDeclarationOnWriteFailure(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()
	f.serverResolver.setErr = errors.New("provider unreachable")

	_, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_APP_PEM", "v"))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v (err=%v)", got, err)
	}

	// The declaration must be gone: a second attempt sees a FRESH declare, not a
	// conflict, which is only true if the rollback happened.
	f.serverResolver.setErr = nil
	if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_APP_PEM", "v")); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

// TestSetServerSecretDoesNotBumpSecretsVersion pins the deliberate asymmetry
// with SetSecret: a server secret never reaches a live session's FetchSecrets,
// so waking every session to re-fetch would be pure churn.
func TestSetServerSecretDoesNotBumpSecretsVersion(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_WEBHOOK_SECRET", "v")); err != nil {
		t.Fatalf("SetServerSecret: %v", err)
	}
	if n := f.signaler.calls; n != 0 {
		t.Fatalf("signaler fired %d times, want 0 for a server secret", n)
	}
}

func listServerReq(bearer string) *connect.Request[compassv1.ListServerSecretsRequest] {
	req := connect.NewRequest(&compassv1.ListServerSecretsRequest{})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return req
}

// TestListServerSecretsReportsDeclaredButUnset is the core contract of the verb
// and the regression guard on HOW is_set is computed. A server secret's names
// are self-declared at boot while the operator populates values separately, so
// declared-but-unset is routine — and distinguishing it from set is the entire
// point.
//
// This is written to FAIL under the naive "resolve the declared set" route: the
// generated manifest marks every declared name required=true, so a value-
// resolving probe fails WHOLESALE (MissingRequiredError, nil set) the moment one
// declared name is unpopulated, erroring the whole call instead of reporting the
// mixed state below. A value-free report has no such failure mode.
func TestListServerSecretsReportsDeclaredButUnset(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	// Declare two names through the real write path, then script the provider
	// probe so exactly one of them holds a value.
	for _, name := range []string{"SERVER_APP_PEM", "SERVER_WEBHOOK_SECRET"} {
		if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, name, "v")); err != nil {
			t.Fatalf("SetServerSecret(%s): %v", name, err)
		}
	}
	f.serverResolver.statuses = []secrets.SecretStatus{
		{Name: "SERVER_APP_PEM", IsSet: true},
		{Name: "SERVER_WEBHOOK_SECRET", IsSet: false},
	}

	resp, err := f.client.ListServerSecrets(ctx, listServerReq(f.adminToken))
	if err != nil {
		t.Fatalf("ListServerSecrets: %v", err)
	}
	got := map[string]bool{}
	for _, s := range resp.Msg.GetServerSecrets() {
		got[s.GetName()] = s.GetIsSet()
	}
	if len(got) != 2 {
		t.Fatalf("got %d statuses, want 2: %v", len(got), got)
	}
	// Names go out in their STORED, prefixed form — the CLI does the strip.
	if !got["SERVER_APP_PEM"] {
		t.Error("SERVER_APP_PEM reported unset; the provider holds its value")
	}
	if got["SERVER_WEBHOOK_SECRET"] {
		t.Error("SERVER_WEBHOOK_SECRET reported set; it is declared but unpopulated")
	}
	if !f.serverResolver.statusHit {
		t.Error("is_set was not computed from a provider probe; a declared server secret's value is populated separately, so the registry row alone cannot answer it")
	}
	if f.serverResolver.resolveHit {
		t.Error("ListServerSecrets resolved VALUES to compute is_set — the probe must be value-free")
	}
}

// TestListServerSecretsProviderFailureIsNotAllUnset pins the distinction an
// operator's remedy depends on: a broken provider must not look like an
// unprovisioned one. Reporting everything unset on a probe fault would send the
// operator to re-populate secrets that are in fact already there.
func TestListServerSecretsProviderFailureIsNotAllUnset(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	if _, err := f.client.SetServerSecret(ctx, setServerReq(f.adminToken, "SERVER_APP_PEM", "v")); err != nil {
		t.Fatalf("SetServerSecret: %v", err)
	}
	f.serverResolver.statusesErr = errors.New("provider unreachable")

	resp, err := f.client.ListServerSecrets(ctx, listServerReq(f.adminToken))
	if err == nil {
		t.Fatalf("want an error on a provider fault, got a list: %v", resp.Msg.GetServerSecrets())
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("want CodeInternal, got %v (err=%v)", got, err)
	}
}

// TestListServerSecretsAdminOnly gates the LIST as tightly as the writes: the
// declared server-secret names are the deployment's own inventory, not
// something a plain user or an agent token may enumerate.
func TestListServerSecretsAdminOnly(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"user", f.userToken},
		{"agent", f.agentToken},
	} {
		_, err := f.client.ListServerSecrets(ctx, listServerReq(tc.token))
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("%s token: want CodePermissionDenied, got %v (err=%v)", tc.name, got, err)
		}
	}
	if _, err := f.client.ListServerSecrets(ctx, listServerReq(f.adminToken)); err != nil {
		t.Fatalf("admin token: %v", err)
	}
}

// D9 scope selector: an omitted scope must land at the CALLER's user coordinate,
// never the shared tenant one. This is the load-bearing default — a client built
// before the selector existed writes a private value, not a tenant-wide one.
func TestSetSecretOmittedScopeLandsAtCallerUserCoordinate(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "v")); err != nil {
		t.Fatalf("SetSecret(omitted scope): %v", err)
	}
	// An EXPLICIT user scope must reach the same coordinate as an omitted one;
	// otherwise the default and the named tier could drift apart unnoticed.
	if _, err := f.client.SetSecret(ctx, scopedSetReq(f.userToken, "API_KEY", "v", compassv1.SecretScope_SECRET_SCOPE_USER)); err != nil {
		t.Fatalf("SetSecret(explicit user scope): %v", err)
	}

	recs, err := f.st.SecretRecordsForAgent(ctx, f.agentID)
	if err != nil {
		t.Fatalf("SecretRecordsForAgent: %v", err)
	}
	found := 0
	for _, r := range recs {
		if r.Name != "DB_URL" && r.Name != "API_KEY" {
			continue
		}
		found++
		if r.ScopeKind != store.SecretScopeUser {
			t.Errorf("scope_kind = %d, want %d (user)", r.ScopeKind, store.SecretScopeUser)
		}
		if r.ScopeID != string(f.userID) {
			t.Errorf("scope_id = %q, want the caller %q", r.ScopeID, f.userID)
		}
	}
	if found != 2 {
		t.Fatalf("resolved %d of the 2 written names for the owning agent; got %d record(s)", found, len(recs))
	}
}

// A plain user may not write the shared tenant coordinate (D8's matrix), on
// either verb. Without this the selector would be a request field any caller
// could use to overwrite every other user's value.
func TestTenantScopeWriteRequiresAdmin(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	_, err := f.client.SetSecret(ctx, scopedSetReq(f.userToken, "DB_URL", "v", compassv1.SecretScope_SECRET_SCOPE_TENANT))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("SetSecret tenant scope as member: code = %v, want PermissionDenied (err %v)", got, err)
	}
	_, err = f.client.DeleteSecret(ctx, scopedDelReq(f.userToken, "DB_URL", compassv1.SecretScope_SECRET_SCOPE_TENANT))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("DeleteSecret tenant scope as member: code = %v, want PermissionDenied (err %v)", got, err)
	}

	if _, err := f.client.SetSecret(ctx, scopedSetReq(f.adminToken, "DB_URL", "v", compassv1.SecretScope_SECRET_SCOPE_TENANT)); err != nil {
		t.Fatalf("SetSecret tenant scope as admin: %v", err)
	}
}

// The isolation property that motivated D9: a user-scoped value is private to its
// owner's agents, while a tenant row stays shared. Asserted through the real
// resolution query, not by reading the row back by primary key.
func TestUserScopedSecretIsNotVisibleToAnotherUsersAgent(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	other, err := f.st.CreateUser(ctx, store.NewUser{Handle: "other", DisplayName: "other"})
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	otherAgent, err := f.st.CreateAgent(ctx, other.ID, store.NewAgent{Handle: "otheragent", DisplayName: "otheragent"})
	if err != nil {
		t.Fatalf("CreateAgent(other): %v", err)
	}

	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "PRIVATE_ONE", "v")); err != nil {
		t.Fatalf("SetSecret(user scope): %v", err)
	}
	if _, err := f.client.SetSecret(ctx, scopedSetReq(f.adminToken, "SHARED_ONE", "v", compassv1.SecretScope_SECRET_SCOPE_TENANT)); err != nil {
		t.Fatalf("SetSecret(tenant scope): %v", err)
	}

	names := resolvedNames(t, ctx, f, otherAgent.ID)
	if names["PRIVATE_ONE"] {
		t.Errorf("another user's agent resolved PRIVATE_ONE; user-scoped rows must not leak across users")
	}
	if !names["SHARED_ONE"] {
		t.Errorf("another user's agent did NOT resolve the tenant-scoped SHARED_ONE; tenant rows stay shared")
	}
}

// Re-setting a name that already has a tenant row writes a NEW user row and does
// NOT retire the shared one — it keeps resolving for every other user until an
// admin deletes it at tenant scope. Pins the real behavior against the intuition
// that a re-set privatizes a secret (record D9).
func TestUserScopeResetDoesNotRetireTheTenantRow(t *testing.T) {
	f := newSecretsFixture(t)
	ctx := context.Background()

	other, err := f.st.CreateUser(ctx, store.NewUser{Handle: "other", DisplayName: "other"})
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	otherAgent, err := f.st.CreateAgent(ctx, other.ID, store.NewAgent{Handle: "otheragent", DisplayName: "otheragent"})
	if err != nil {
		t.Fatalf("CreateAgent(other): %v", err)
	}

	if _, err := f.client.SetSecret(ctx, scopedSetReq(f.adminToken, "DB_URL", "shared", compassv1.SecretScope_SECRET_SCOPE_TENANT)); err != nil {
		t.Fatalf("SetSecret(tenant): %v", err)
	}
	if _, err := f.client.SetSecret(ctx, setReq(f.userToken, "DB_URL", "private")); err != nil {
		t.Fatalf("SetSecret(user re-set): %v", err)
	}

	if !resolvedNames(t, ctx, f, otherAgent.ID)["DB_URL"] {
		t.Errorf("the tenant row stopped resolving for another user after a user-scope re-set; a re-set must not retire the shared value")
	}
	recs, err := f.st.SecretRecordsForAgent(ctx, f.agentID)
	if err != nil {
		t.Fatalf("SecretRecordsForAgent: %v", err)
	}
	for _, r := range recs {
		if r.Name == "DB_URL" && r.ScopeKind != store.SecretScopeUser {
			t.Errorf("the setter's own agent resolved scope_kind %d, want %d (its private row shadows the tenant one)", r.ScopeKind, store.SecretScopeUser)
		}
	}
}

func resolvedNames(t *testing.T, ctx context.Context, f secretsFixture, agent store.AccountID) map[string]bool {
	t.Helper()
	recs, err := f.st.SecretRecordsForAgent(ctx, agent)
	if err != nil {
		t.Fatalf("SecretRecordsForAgent: %v", err)
	}
	out := make(map[string]bool, len(recs))
	for _, r := range recs {
		out[r.Name] = true
	}
	return out
}
