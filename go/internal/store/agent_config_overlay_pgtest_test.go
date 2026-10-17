//go:build pgtest

package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAgentConfigOverlayPutInfoDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-admin", DisplayName: "overlay-admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	bundle := mkBundle(t, map[string]string{"skills/x/SKILL.md": "user skill"})
	version, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, bundle)
	if err != nil {
		t.Fatalf("tenant PutAgentConfigOverlay: %v", err)
	}
	if version == "" {
		t.Fatal("overlay version is empty")
	}
	info, err := s.AgentConfigOverlayInfo(ctx, OverlayKey{})
	if err != nil {
		t.Fatalf("AgentConfigOverlayInfo: %v", err)
	}
	if info.Version != version || len(info.Skills) != 1 || info.Skills[0] != "x" {
		t.Fatalf("overlay info = %+v, want version %q and skill x", info, version)
	}
	if err := s.DeleteAgentConfigOverlay(ctx, OverlayKey{}); err != nil {
		t.Fatalf("DeleteAgentConfigOverlay: %v", err)
	}
	if _, err := s.AgentConfigOverlayInfo(ctx, OverlayKey{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("overlay info after delete = %v, want ErrNotFound", err)
	}
}

func TestEffectiveAgentConfigOverlayLayers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-layer-admin", DisplayName: "overlay-layer-admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-layer-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-layer-agent", DisplayName: "agent", Role: "manager"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	defaultBundle := mkBundle(t, map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"default"}}}`})
	if _, err := s.PutAgentConfig(ctx, admin.ID, defaultBundle); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	layers := []struct {
		name  string
		actor AccountID
		key   OverlayKey
		value string
	}{
		{name: "tenant", actor: admin.ID, key: OverlayKey{}, value: "tenant"},
		{name: "user", actor: owner.ID, key: OverlayKey{Owner: owner.ID}, value: "user"},
		{name: "role", actor: owner.ID, key: OverlayKey{Owner: owner.ID, Role: "manager"}, value: "role"},
	}
	for _, layer := range layers {
		bundle := mkBundle(t, map[string]string{"mcp/fetch.json": `{"mcpServers":{"fetch":{"command":"` + layer.value + `"}}}`})
		if _, err := s.PutAgentConfigOverlay(ctx, layer.actor, layer.key, bundle); err != nil {
			t.Fatalf("PutAgentConfigOverlay(%s): %v", layer.name, err)
		}
	}
	version, got, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig: %v", err)
	}
	if version == "" || len(got) == 0 {
		t.Fatalf("effective version=%q bundle bytes=%d", version, len(got))
	}
	files := bundleFiles(t, got)
	if want := `{"mcpServers":{"fetch":{"command":"role"}}}`; files["mcp/fetch.json"] != want {
		t.Fatalf("effective mcp/fetch.json = %q, want %q", files["mcp/fetch.json"], want)
	}
	matched, bundle, err := s.EffectiveAgentConfig(ctx, agent.ID, version)
	if err != nil {
		t.Fatalf("EffectiveAgentConfig(ifVersion): %v", err)
	}
	if matched != version || bundle != nil {
		t.Fatalf("ifVersion response version=%q bundle=%v, want matching version and nil bundle", matched, bundle)
	}
	_, members, err := s.EffectiveAgentConfigMembers(ctx, agent.ID)
	if err != nil {
		t.Fatalf("EffectiveAgentConfigMembers: %v", err)
	}
	if len(members) != 1 || members[0].Path != "mcp/fetch.json" || members[0].Layer != LayerRole {
		t.Fatalf("effective members = %+v, want role mcp/fetch.json", members)
	}
}

func TestEffectiveAgentConfigDoesNotApplyRoleWhenAgentRoleEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-empty-role-admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-empty-role-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-empty-role-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	roleBundle := mkBundle(t, map[string]string{"skills/role/SKILL.md": "role-only"})
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID, Role: "manager"}, roleBundle); err != nil {
		t.Fatalf("PutAgentConfigOverlay(role): %v", err)
	}
	_, got, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig: %v", err)
	}
	if len(bundleFiles(t, got)) != 0 {
		t.Fatalf("empty-role agent received role overlay: %v", bundleFiles(t, got))
	}
}

func TestEffectiveAgentConfigDoesNotApplyUnknownLegacyRole(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := mustUser(t, s, "overlay-unknown-role-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-unknown-role-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE agent_accounts SET role = 'legacy-manager' WHERE account_id = $1", string(agent.ID)); err != nil {
		t.Fatalf("set legacy role fixture: %v", err)
	}
	roleBundle := mkBundle(t, map[string]string{"skills/role/SKILL.md": "role-only"})
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID, Role: "manager"}, roleBundle); err != nil {
		t.Fatalf("PutAgentConfigOverlay(role): %v", err)
	}
	_, got, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig: %v", err)
	}
	if len(bundleFiles(t, got)) != 0 {
		t.Fatalf("unknown legacy role received role overlay: %v", bundleFiles(t, got))
	}
}

func TestAgentConfigOverlayLocksAndLayerScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-lock-admin", DisplayName: "overlay-lock-admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-lock-owner")
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, mkBundle(t, map[string]string{"locks.yml": "paths:\n  - skills/web/\n  - settings/config.yml\n"})); err != nil {
		t.Fatalf("put tenant locks: %v", err)
	}
	for _, tt := range []struct {
		name string
		key  OverlayKey
		path string
	}{
		{name: "prefix lock", key: OverlayKey{Owner: owner.ID}, path: "skills/web/SKILL.md"},
		{name: "exact lock", key: OverlayKey{Owner: owner.ID, Role: "manager"}, path: "settings/config.yml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			actor := owner.ID
			if tt.key.Owner == "" {
				actor = admin.ID
			}
			_, err := s.PutAgentConfigOverlay(ctx, actor, tt.key, mkBundle(t, map[string]string{tt.path: "value: locked\n"}))
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("locked PutAgentConfigOverlay error = %v, want ErrInvalidArgument", err)
			}
		})
	}
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID, Role: "not-a-role"}, mkBundle(t, map[string]string{"skills/web/SKILL.md": "x"})); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("off-taxonomy role error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Role: "manager"}, mkBundle(t, map[string]string{"skills/other/SKILL.md": "x"})); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("role without owner error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{Owner: owner.ID}, mkBundle(t, map[string]string{"skills/other/SKILL.md": "store accepts the supplied key"})); err != nil {
		t.Fatalf("Store rejected a valid supplied owner key from another actor: %v", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, mkBundle(t, map[string]string{"skills/other/SKILL.md": "x"})); err != nil {
		t.Fatalf("admin tenant overlay: %v", err)
	}
}
func TestPutAgentConfigOverlayRejectsInvalidStoredTenantLocksInternally(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-corrupt-locks-admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-corrupt-locks-owner")
	validTenant := mkBundle(t, map[string]string{"locks.yml": "paths: []\n"})
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, validTenant); err != nil {
		t.Fatalf("Put tenant overlay: %v", err)
	}
	for _, tc := range []struct {
		name   string
		bundle []byte
	}{
		{name: "invalid stored member path", bundle: mkBundle(t, map[string]string{"unknown/a.txt": "invalid"})},
		{name: "invalid stored locks content", bundle: mkBundle(t, map[string]string{"locks.yml": "paths:\n  - skills/web\n"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, `UPDATE agent_config_overlays SET bundle = $1 WHERE tenant_id = $2 AND owner_user_id IS NULL AND role = ''`, tc.bundle, string(s.resolveTenant(ctx))); err != nil {
				t.Fatalf("corrupt tenant overlay row: %v", err)
			}
			_, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, mkBundle(t, map[string]string{"skills/web/SKILL.md": "user"}))
			if err == nil || errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("PutAgentConfigOverlay with corrupt stored tenant bundle error = %v, want non-wrapping internal error", err)
			}
		})
	}
}
func TestEffectiveAgentConfigRejectsInvalidStoredTenantBundleInternally(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-read-corrupt-admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-read-corrupt-owner")
	agent := mustAgent(t, s, owner.ID, "overlay-read-corrupt-agent")
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, mkBundle(t, map[string]string{"locks.yml": "paths: []\n"})); err != nil {
		t.Fatalf("Put tenant overlay: %v", err)
	}
	corrupt := mkBundle(t, map[string]string{"unknown/a.txt": "invalid"})
	if _, err := s.pool.Exec(ctx, `UPDATE agent_config_overlays SET bundle = $1 WHERE tenant_id = $2 AND owner_user_id IS NULL AND role = ''`, corrupt, string(s.resolveTenant(ctx))); err != nil {
		t.Fatalf("corrupt tenant overlay row: %v", err)
	}
	if _, _, err := s.EffectiveAgentConfig(ctx, agent.ID, ""); err == nil || errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("EffectiveAgentConfig with corrupt tenant bundle error = %v, want non-wrapping internal error", err)
	}
}

func TestAgentConfigOverlayTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA := s.EffectiveTenant(context.Background())
	tenantB := seedTenant(t, s, "overlay-isolation-b")
	ctxA := WithTenant(context.Background(), tenantA)
	ctxB := WithTenant(context.Background(), tenantB)
	adminA, err := s.BootstrapAdmin(ctxA, NewUser{Handle: "overlay-isolation-admin-a", DisplayName: "overlay-isolation-admin-a"})
	if err != nil {
		t.Fatalf("BootstrapAdmin A: %v", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctxA, adminA.ID, OverlayKey{}, mkBundle(t, map[string]string{"skills/a/SKILL.md": "a"})); err != nil {
		t.Fatalf("PutAgentConfigOverlay A: %v", err)
	}
	if _, err := s.AgentConfigOverlayInfo(ctxB, OverlayKey{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B overlay info = %v, want ErrNotFound", err)
	}
}

func TestEffectiveAgentConfigNoOverlayReturnsDefaultBytes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-default-admin", DisplayName: "overlay-default-admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-default-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-default-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	defaultBundle := mkBundle(t, map[string]string{"skills/default/SKILL.md": "default"})
	if _, err := s.PutAgentConfig(ctx, admin.ID, defaultBundle); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	_, got, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig: %v", err)
	}
	if !bytes.Equal(got, defaultBundle) {
		t.Fatalf("default-only effective bytes differ from stored default")
	}
}

func TestEffectiveAgentConfigLockAddedAfterWriteDropsStoredOverlay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-late-lock-admin", DisplayName: "overlay-late-lock-admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-late-lock-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-late-lock-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	defaultBundle := mkBundle(t, map[string]string{"skills/web/SKILL.md": "default"})
	if _, err := s.PutAgentConfig(ctx, admin.ID, defaultBundle); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, mkBundle(t, map[string]string{"skills/web/SKILL.md": "user"})); err != nil {
		t.Fatalf("user overlay: %v", err)
	}
	if _, err := s.PutAgentConfigOverlay(ctx, admin.ID, OverlayKey{}, mkBundle(t, map[string]string{"locks.yml": "paths:\n  - skills/web/\n"})); err != nil {
		t.Fatalf("tenant lock: %v", err)
	}
	_, _, err = s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig with late lock: %v", err)
	}
	_, members, err := s.EffectiveAgentConfigMembers(ctx, agent.ID)
	if err != nil {
		t.Fatalf("EffectiveAgentConfigMembers with late lock: %v", err)
	}
	if len(members) != 1 || members[0].Layer != LayerDefault {
		t.Fatalf("locked user member did not fall back to default: %+v", members)
	}
}

func TestEffectiveAgentConfigRejectsStoredBundleThatBypassesDoor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := mustUser(t, s, "overlay-bypass-door-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-bypass-door-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	bundle := mkBundle(t, map[string]string{"unknown/a.txt": "stored path outside config roots"})
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_config_overlays (tenant_id, owner_user_id, role, version, bundle, declared_by)
		VALUES ($1, $2, '', 'invalid', $3, $2)`, string(s.resolveTenant(ctx)), string(owner.ID), bundle); err != nil {
		t.Fatalf("insert bypassed overlay row: %v", err)
	}
	if _, _, err := s.EffectiveAgentConfig(ctx, agent.ID, ""); err == nil || errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("EffectiveAgentConfig(bypassed row) error = %v, want internal validation error", err)
	}
}

func TestEffectiveAgentConfigMatchingVersionSkipsCorruptStoredRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-ifversion-bypass-admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-ifversion-bypass-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-ifversion-bypass-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	valid := mkBundle(t, map[string]string{"skills/x/SKILL.md": "valid"})
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, valid); err != nil {
		t.Fatalf("PutAgentConfigOverlay: %v", err)
	}
	version, _, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig(valid row): %v", err)
	}
	invalid := mkBundle(t, map[string]string{"unknown/a.txt": "stored path outside config roots"})
	if _, err := s.pool.Exec(ctx, `UPDATE agent_config_overlays SET bundle = $1 WHERE tenant_id = $2 AND owner_user_id = $3 AND role = ''`, invalid, string(s.resolveTenant(ctx)), string(owner.ID)); err != nil {
		t.Fatalf("replace row around door: %v", err)
	}
	matched, bundle, err := s.EffectiveAgentConfig(ctx, agent.ID, version)
	if err != nil {
		t.Fatalf("EffectiveAgentConfig(matching ifVersion with corrupt row): %v", err)
	}
	if matched != version || bundle != nil {
		t.Fatalf("matching response version=%q bundle=%v, want matching version and nil bundle", matched, bundle)
	}
	if _, _, err := s.EffectiveAgentConfig(ctx, agent.ID, "different-version"); err == nil || errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("EffectiveAgentConfig(non-matching ifVersion with corrupt row) error = %v, want internal validation error", err)
	}
}

func TestEffectiveAgentConfigUsesSingleSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name      string
		wantMatch bool
		ifVersion func(string) string
	}{
		{name: "matching ifVersion", wantMatch: true, ifVersion: func(version string) string { return version }},
		{name: "non-matching ifVersion", ifVersion: func(string) string { return "different-version" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			_, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-snapshot-admin", DisplayName: "admin"})
			if err != nil {
				t.Fatalf("BootstrapAdmin: %v", err)
			}
			owner := mustUser(t, s, "overlay-snapshot-owner")
			agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-snapshot-agent", DisplayName: "agent"})
			if err != nil {
				t.Fatalf("CreateAgent: %v", err)
			}
			first := mkBundle(t, map[string]string{"skills/x/SKILL.md": "first"})
			second := mkBundle(t, map[string]string{"skills/x/SKILL.md": "second"})
			firstLayerVersion, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, first)
			if err != nil {
				t.Fatalf("PutAgentConfigOverlay(first): %v", err)
			}
			firstVersion := EffectiveConfigVersion("", "", firstLayerVersion, "")
			var secondVersion string
			var writeErr error
			hookCalled := false
			s.afterConfigVersionsRead = func() {
				hookCalled = true
				s.afterConfigVersionsRead = nil
				secondVersion, writeErr = s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, second)
			}
			version, got, err := s.EffectiveAgentConfig(ctx, agent.ID, tt.ifVersion(firstVersion))
			if err != nil {
				t.Fatalf("EffectiveAgentConfig(raced): %v", err)
			}
			if !hookCalled {
				t.Fatal("afterConfigVersionsRead hook was not called")
			}
			if writeErr != nil {
				t.Fatalf("racing PutAgentConfigOverlay: %v", writeErr)
			}
			if secondVersion == "" || secondVersion == firstLayerVersion {
				t.Fatalf("racing overlay version = %q, want a different non-empty version", secondVersion)
			}
			if tt.wantMatch {
				if version != firstVersion || got != nil {
					t.Fatalf("matching response version=%q bundle=%v, want snapshot version %q and nil bundle", version, got, firstVersion)
				}
				return
			}
			files := bundleFiles(t, got)
			if version != firstVersion || files["skills/x/SKILL.md"] != "first" {
				t.Fatalf("non-matching response version=%q member=%q; want snapshot version %q and first bundle", version, files["skills/x/SKILL.md"], firstVersion)
			}
		})
	}
}

func TestEffectiveProfileAgentOrphansAreWarned(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-orphan-admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner := mustUser(t, s, "overlay-orphan-owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "overlay-orphan-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	defaultBundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "agents/impl.md", content: "---\nname: impl\n---\n"},
		tarEntry{name: "profiles/default/profile.yml", content: "models:\n  agents:\n    impl: selector\n"},
	)
	if _, err := s.PutAgentConfig(ctx, admin.ID, defaultBundle); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	userBundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tarEntry{name: "agents/impl.md", content: "---\nname: replacement\n---\n"})
	if _, err := s.PutAgentConfigOverlay(ctx, owner.ID, OverlayKey{Owner: owner.ID}, userBundle); err != nil {
		t.Fatalf("PutAgentConfigOverlay: %v", err)
	}
	var log bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldLogger)
	if _, _, err := s.EffectiveAgentConfig(ctx, agent.ID, ""); err != nil {
		t.Fatalf("EffectiveAgentConfig: %v", err)
	}
	if !strings.Contains(log.String(), "effective config profile references missing agent") || !strings.Contains(log.String(), "agent=impl") {
		t.Fatalf("orphan warning = %q, want warning naming missing agent impl", log.String())
	}
}
func TestAgentConfigOverlayUserAndAgentIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.BootstrapAdmin(ctx, NewUser{Handle: "overlay-isolation-admin", DisplayName: "admin"}); err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	userA := mustUser(t, s, "overlay-isolation-user-a")
	userB := mustUser(t, s, "overlay-isolation-user-b")
	agentA1 := mustAgent(t, s, userA.ID, "overlay-isolation-agent-a1")
	agentA2 := mustAgent(t, s, userA.ID, "overlay-isolation-agent-a2")
	agentB := mustAgent(t, s, userB.ID, "overlay-isolation-agent-b")
	bundle := mkBundle(t, map[string]string{"skills/private/SKILL.md": "only user A"})
	if _, err := s.PutAgentConfigOverlay(ctx, userA.ID, OverlayKey{Owner: userA.ID}, bundle); err != nil {
		t.Fatalf("PutAgentConfigOverlay for user A: %v", err)
	}
	for _, agent := range []Account{agentA1, agentA2} {
		_, got, err := s.EffectiveAgentConfig(ctx, agent.ID, "")
		if err != nil {
			t.Fatalf("EffectiveAgentConfig(%s): %v", agent.Handle, err)
		}
		if bundleFiles(t, got)["skills/private/SKILL.md"] != "only user A" {
			t.Fatalf("agent %s did not receive its owner's overlay", agent.Handle)
		}
	}
	_, got, err := s.EffectiveAgentConfig(ctx, agentB.ID, "")
	if err != nil {
		t.Fatalf("EffectiveAgentConfig for user B: %v", err)
	}
	if len(bundleFiles(t, got)) != 0 {
		t.Fatalf("user B received user A's overlay: %v", bundleFiles(t, got))
	}
}
