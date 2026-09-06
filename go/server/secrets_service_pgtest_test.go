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
// compute is_set (record §906-908 / brief item 7).
type recordingResolver struct {
	setErr      error
	setNames    []string
	setReasons  []string
	deleteNames []string
	resolveHit  bool
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
	resolver   *recordingResolver
	signaler   *recordingSignaler
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
	signaler := &recordingSignaler{}
	svc := newSecretsService(st, resolver, signaler)
	url := newSecretsH2CServer(t, svc,
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(admin.ID),
	)
	return secretsFixture{
		client:     newSecretsH2CClient(t, url),
		userToken:  userTok,
		agentToken: agentTok,
		userID:     user.ID,
		resolver:   resolver,
		signaler:   signaler,
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
