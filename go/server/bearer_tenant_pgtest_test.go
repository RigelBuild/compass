//go:build pgtest && unix

package server

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestBearerTokenScopesCallerToIssuingTenant(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "bootstrap-admin", DisplayName: "Bootstrap admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	tenantPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(tenantPool.Close)
	const tenantB store.TenantID = "tenant-b"
	if _, err := tenantPool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		string(tenantB), string(tenantB), "Tenant B", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert tenant B: %v", err)
	}

	tenantCtx := store.WithTenant(ctx, tenantB)
	caller, err := st.CreateUser(tenantCtx, store.NewUser{Handle: "tenant-b-user", DisplayName: "Tenant B user"})
	if err != nil {
		t.Fatalf("CreateUser(tenant B): %v", err)
	}
	token, err := auth.IssueAccountToken(tenantCtx, st, caller.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(tenant B): %v", err)
	}

	dir := t.TempDir()
	certPath, keyPath, roots := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)
	socketPath := filepath.Join(dir, "compass.sock")
	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: dsn,
		Version:     "tenant-b-bearer-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    filepath.Join(dir, "state"),
		AdminHandle: "bootstrap-admin",
	})
	waitServing(t, socketPath)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"},
		Protocols:       protocols,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := compassv1connect.NewCommsServiceClient(&http.Client{Transport: transport}, "https://"+addr)

	rpcCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	req := connect.NewRequest(&compassv1.ListAccountsRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := client.ListAccounts(rpcCtx, req)
	if err != nil {
		t.Fatalf("ListAccounts with tenant B bearer: %v", err)
	}

	var foundCaller, foundBootstrapAdmin bool
	for _, account := range resp.Msg.GetAccounts() {
		switch account.GetId() {
		case string(caller.ID):
			foundCaller = true
		case string(admin.ID):
			foundBootstrapAdmin = true
		}
	}
	if !foundCaller {
		t.Fatalf("ListAccounts did not return issuing-tenant account %q: %v", caller.ID, resp.Msg.GetAccounts())
	}
	if foundBootstrapAdmin {
		t.Fatalf("ListAccounts exposed bootstrap-tenant account %q to tenant B", admin.ID)
	}
}
