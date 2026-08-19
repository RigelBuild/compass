package store

// The hermetic default-gate half of the DL-174 pair for the DL-055 ownership
// index (design docs/designs/product/compass-forge-write-path/design.md §T7):
// the pure-Go contract that needs no Postgres — the pre-DB argument guards, the
// empty-clientRequestID always-miss short-circuit, and the NULL client_request_id
// mapping. The real-Postgres row contracts live in the pgtest sibling
// (forge_authored_pgtest_test.go). context.Background is the test root.

import (
	"context"
	"errors"
	"testing"
)

// TestAuthoredArtifactValid pins the pre-DB argument guards (Store.valid),
// exercised without a database: each malformed field is ErrInvalidArgument
// before any pool call, and a fully-populated artifact passes.
func TestAuthoredArtifactValid(t *testing.T) {
	good := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 1, AgentAccountID: "agent", OwnerUserID: "owner",
	}
	if err := good.valid(); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}

	tests := []struct {
		name string
		mut  func(a *AuthoredArtifact)
	}{
		{"zero provider", func(a *AuthoredArtifact) { a.Provider = ForgeProviderUnspecified }},
		{"empty host", func(a *AuthoredArtifact) { a.Host = "" }},
		{"empty repo", func(a *AuthoredArtifact) { a.Repo = "" }},
		{"zero kind", func(a *AuthoredArtifact) { a.Kind = ForgeArtifactKindUnspecified }},
		{"empty agent", func(a *AuthoredArtifact) { a.AgentAccountID = "" }},
		{"empty owner", func(a *AuthoredArtifact) { a.OwnerUserID = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			tc.mut(&bad)
			if err := bad.valid(); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("%s: err = %v, want errors.Is(_, ErrInvalidArgument)", tc.name, err)
			}
		})
	}
}

// TestAuthoredArtifactByRequestIDEmptyKeyMiss proves the empty-clientRequestID
// always-miss short-circuit returns (ok=false, nil) BEFORE any DB round trip —
// a null-key row carries no key to match. Runs against a poolless Store, so a
// pool call would panic; reaching a clean miss proves the short-circuit.
func TestAuthoredArtifactByRequestIDEmptyKeyMiss(t *testing.T) {
	s := &Store{} // nil pool: the short-circuit must not touch it
	_, ok, err := s.AuthoredArtifactByRequestID(context.Background(), "agent", "")
	if err != nil {
		t.Fatalf("empty-key lookup: %v", err)
	}
	if ok {
		t.Fatal("empty clientRequestID = hit, want always-miss")
	}
}

// TestNullIfEmpty pins the NULL client_request_id mapping: "" becomes a typed
// nil (SQL NULL, so null-key rows never collide under the partial unique memo
// index), a non-empty key is passed through by value.
func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Fatalf("nullIfEmpty(\"\") = %v, want nil (SQL NULL)", *got)
	}
	got := nullIfEmpty("req-1")
	if got == nil || *got != "req-1" {
		t.Fatalf("nullIfEmpty(%q) = %v, want a pointer to it", "req-1", got)
	}
}
