//go:build pgtest && unix

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

func buildAgentRepositoryDoors(t *testing.T) (context.Context, *serveDoors, string, string) {
	t.Helper()
	ctx := context.Background()
	st := forgeTestStore(t)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "repo-door-admin", DisplayName: "Admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "repo-door-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	if _, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "repo-admin-worker", DisplayName: "Admin Worker"}); err != nil {
		t.Fatalf("CreateAgent(admin): %v", err)
	}
	if _, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "worker", DisplayName: "Worker"}); err != nil {
		t.Fatalf("CreateAgent(owner): %v", err)
	}
	for _, scope := range []store.ForgeScope{
		{AccountID: admin.ID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "admin/base"},
		{AccountID: owner.ID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/base"},
	} {
		if err := st.GrantForgeScope(ctx, scope); err != nil {
			t.Fatalf("GrantForgeScope(%s): %v", scope.AccountID, err)
		}
	}
	if err := declareServerSecretNames(ctx, st, ServeConfig{Forge: ForgeConfig{
		App: ForgeAppConfig{AppID: 42, InstallationID: 7, AppPrivateKeySecret: "APP_KEY", AppWebhookSecretName: "APP_WEBHOOK"},
	}}); err != nil {
		t.Fatalf("declareServerSecretNames: %v", err)
	}
	bus := events.NewBus[busPayload]()
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	commsSvc := comms.NewComms(st, commsBus, nil, admin.ID)
	brd := board.NewProjection(bus)
	issueBrd := board.NewIssueProjection(bus, st)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, commsSvc, nil)
	svc := newService("door-test", bus, st, hub, brd, issueBrd, tail)
	container := secrets.NewStoreResolver(st, secretsFixtureKey(t), 1)
	secretsSvc := newSecretsService(st, container, &fakeResolver{}, nil)
	devListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dev: %v", err)
	}
	netListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		if closeErr := devListener.Close(); closeErr != nil {
			t.Fatalf("listen network: %v (closing dev listener: %v)", err, closeErr)
		}
		t.Fatalf("listen network: %v", err)
	}
	certPath, keyPath, _ := writeSelfSignedCert(t, t.TempDir())
	tlsConfig, err := loadNetworkTLS(&TLSConfig{CertPath: certPath, KeyPath: keyPath})
	if err != nil {
		t.Fatalf("loadNetworkTLS: %v", err)
	}
	devHTTP := netip.MustParseAddrPort(devListener.Addr().String())
	appKey := testAppPEM(t)
	serverResolver := &fakeResolver{resolved: []secrets.ResolvedSecret{
		{Name: serverSecretName("APP_KEY"), Value: appKey},
		{Name: serverSecretName("APP_WEBHOOK"), Value: "webhook-test-secret"},
	}}
	doors, err := buildDoors(ctx, ServeConfig{StateDir: t.TempDir(), DevHTTP: &devHTTP, Forge: ForgeConfig{
		App: ForgeAppConfig{AppID: 42, InstallationID: 7, AppPrivateKeySecret: "APP_KEY", AppWebhookSecretName: "APP_WEBHOOK"},
	}}, svc, commsSvc, secretsSvc, hub, st, admin.ID, "", container, serverResolver, devListener, netListener, tlsConfig,
		nil, nil, nil, envelope.Key{}, 0)
	if err != nil {
		if closeErr := devListener.Close(); closeErr != nil {
			t.Fatalf("buildDoors: %v (closing dev listener: %v)", err, closeErr)
		}
		if closeErr := netListener.Close(); closeErr != nil {
			t.Fatalf("buildDoors: %v (closing network listener: %v)", err, closeErr)
		}
		t.Fatalf("buildDoors: %v", err)
	}
	t.Cleanup(func() {
		for _, door := range []*http.Server{doors.uds, doors.dev, doors.net} {
			if door != nil {
				if err := door.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("closing assembled door: %v", err)
				}
			}
		}
		for _, listener := range []net.Listener{devListener, netListener} {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("closing door listener: %v", err)
			}
		}
	})
	if doors.uds == nil || doors.dev == nil || doors.net == nil {
		t.Fatalf("assembled doors = uds:%t dev:%t net:%t, want all three", doors.uds != nil, doors.dev != nil, doors.net != nil)
	}
	adminToken, err := auth.IssueAccountToken(ctx, st, admin.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}
	ownerToken, err := auth.IssueAccountToken(ctx, st, owner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(owner): %v", err)
	}
	return ctx, &doors, adminToken, ownerToken
}
