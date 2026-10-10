//go:build pgtest && unix

package server

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// A live minted Runner ID with "/" could equal a node's projected ID, so Serve
// refuses to start with clusters configured; a revoked one no longer counts.
func TestServeRefusesRunnerClustersWithSlashedMintedID(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	// PutTokenHash directly: the mint now refuses "/", which is the point.
	revokedHash := sha256.Sum256([]byte("revoked-slashed"))
	for _, row := range []struct {
		hash [32]byte
		id   string
	}{
		{sha256.Sum256([]byte("live-slashed")), "prod/ip-10-0-1-5_ec2_internal"},
		{revokedHash, "prod/other-node"},
		{sha256.Sum256([]byte("live-plain")), "runner-1"},
	} {
		if err := st.PutTokenHash(ctx, row.hash, store.Subject{Kind: store.SubjectRunner, ID: row.id}); err != nil {
			t.Fatalf("PutTokenHash(%q): %v", row.id, err)
		}
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte("account-slashed")), store.Subject{Kind: store.SubjectAccount, ID: "acct/1"}); err != nil {
		t.Fatalf("PutTokenHash(account): %v", err)
	}
	if err := st.RevokeToken(ctx, revokedHash); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if n, err := st.CountRunnerTokenIDsWithSlash(ctx); err != nil || n != 1 {
		t.Fatalf("CountRunnerTokenIDsWithSlash = (%d, %v), want (1, nil): only the live Runner row counts", n, err)
	}

	dir := t.TempDir()
	clusters := filepath.Join(dir, "runner-clusters.yaml")
	if err := os.WriteFile(clusters, []byte("clusters:\n  - name: prod\n    issuer: https://oidc.prod.example\n    namespace: compass-runner\n    serviceAccount: compass-runner\n"), 0o600); err != nil {
		t.Fatalf("writing cluster file: %v", err)
	}
	socketPath := filepath.Join(dir, "compass.sock")
	cfg := ServeConfig{
		SocketPath:         socketPath,
		DatabaseDSN:        dsn,
		Version:            "runner-clusters-test",
		RunnerClustersPath: clusters,
	}
	provisionMasterKeyProvider(t, &cfg)
	provisionNats(t, &cfg)
	err = Serve(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "runner id") {
		t.Fatalf("Serve = %v, want a refusal naming the slashed runner id", err)
	}
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket left behind after refused start (stat err = %v)", statErr)
	}
}
