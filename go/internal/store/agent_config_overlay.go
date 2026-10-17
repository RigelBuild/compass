package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// OverlayKey selects the tenant, user, or role config overlay.
// An empty Owner selects tenant; a non-empty Role selects that owner's role layer.
type OverlayKey struct {
	Owner AccountID
	Role  string
}

// PutAgentConfigOverlay validates and replaces the bundle selected by key.
// The returned version hashes the canonical bundle contents.
func (s *Store) PutAgentConfigOverlay(ctx context.Context, actor AccountID, key OverlayKey, bundle []byte) (version string, err error) {
	layer, err := overlayLayerForKey(key)
	if err != nil {
		return "", err
	}
	if actor == "" {
		return "", fmt.Errorf("%w: overlay writer account id is required", ErrInvalidArgument)
	}
	version, _, err = validateAndHashConfigBundle(bundle, layer)
	if err != nil {
		return "", err
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin config overlay write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	if layer == LayerUser || layer == LayerRole {
		locks, err := loadTenantConfigLocks(ctx, q, s.resolveTenant(ctx))
		if err != nil {
			return "", err
		}
		files, err := readConfigBundleMembers(bundle)
		if err != nil {
			return "", fmt.Errorf("store: read config overlay bundle: %w", err)
		}
		for member := range files {
			if locks.Covers(canonicalConfigPath(member)) {
				return "", fmt.Errorf("%w: config member %q is locked by the tenant", ErrInvalidArgument, member)
			}
		}
	}
	if key.Owner == "" {
		if err := q.PutAgentConfigOverlay(ctx, db.PutAgentConfigOverlayParams{
			TenantID: string(s.resolveTenant(ctx)), Column2: nil, Role: "", Version: version, Bundle: bundle, DeclaredBy: string(actor),
		}); err != nil {
			return "", fmt.Errorf("store: put tenant config overlay: %w", err)
		}
	} else {
		if err := q.PutOwnedAgentConfigOverlay(ctx, db.PutOwnedAgentConfigOverlayParams{
			TenantID: string(s.resolveTenant(ctx)), OwnerUserID: pgtype.Text{String: string(key.Owner), Valid: true}, Role: key.Role, Version: version, Bundle: bundle, DeclaredBy: string(actor),
		}); err != nil {
			return "", fmt.Errorf("store: put owned config overlay: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit config overlay: %w", err)
	}
	return version, nil
}

// DeleteAgentConfigOverlay removes the selected overlay, if present.
// Deleting an absent overlay succeeds without changing other layers.
func (s *Store) DeleteAgentConfigOverlay(ctx context.Context, key OverlayKey) error {
	if _, err := overlayLayerForKey(key); err != nil {
		return err
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin config overlay delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	if key.Owner == "" {
		_, err = q.DeleteAgentConfigTenantOverlay(ctx, string(s.resolveTenant(ctx)))
	} else {
		_, err = q.DeleteAgentConfigOwnedOverlay(ctx, db.DeleteAgentConfigOwnedOverlayParams{
			TenantID: string(s.resolveTenant(ctx)), OwnerUserID: pgtype.Text{String: string(key.Owner), Valid: true}, Role: key.Role,
		})
	}
	if err != nil {
		return fmt.Errorf("store: delete config overlay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit config overlay delete: %w", err)
	}
	return nil
}

// AgentConfigOverlayInfo returns version and member names without member contents.
func (s *Store) AgentConfigOverlayInfo(ctx context.Context, key OverlayKey) (AgentConfigInfoResult, error) {
	if _, err := overlayLayerForKey(key); err != nil {
		return AgentConfigInfoResult{}, err
	}
	var owner any
	if key.Owner != "" {
		owner = string(key.Owner)
	}
	row, err := s.q.AgentConfigOverlay(ctx, db.AgentConfigOverlayParams{
		TenantID: string(s.resolveTenant(ctx)), Column2: owner, Role: key.Role,
	})
	if err != nil {
		if noRows(err) {
			return AgentConfigInfoResult{}, fmt.Errorf("%w: config overlay", ErrNotFound)
		}
		return AgentConfigInfoResult{}, fmt.Errorf("store: read config overlay: %w", err)
	}
	info, err := configBundleMemberNames(row.Bundle)
	if err != nil {
		return AgentConfigInfoResult{}, err
	}
	info.Version = row.Version
	return info, nil
}

// EffectiveAgentConfig returns the merged bundle, or only its version on a match.
// A matching version does not read or validate any bundle contents.
func (s *Store) EffectiveAgentConfig(ctx context.Context, agent AccountID, ifVersion string) (version string, bundle []byte, err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", nil, fmt.Errorf("store: begin effective config snapshot: %w", err)
	}
	// Rollback is a no-op after Commit and releases the snapshot on every early return.
	defer func() { _ = tx.Rollback(ctx) }()
	if err := armTx(ctx, s, tx); err != nil {
		return "", nil, err
	}
	qtx := s.q.WithTx(tx)
	versionRow, err := qtx.EffectiveAgentConfigLayerVersions(ctx, db.EffectiveAgentConfigLayerVersionsParams{
		AccountID: string(agent), TenantID: string(s.resolveTenant(ctx)),
	})
	if err != nil {
		if noRows(err) {
			return "", nil, fmt.Errorf("%w: agent %q", ErrNotFound, agent)
		}
		return "", nil, fmt.Errorf("store: read effective config layer versions: %w", err)
	}
	version = EffectiveConfigVersion(versionRow.DefaultVersion, versionRow.TenantVersion, versionRow.UserVersion, versionRow.RoleVersion)
	if s.afterConfigVersionsRead != nil {
		s.afterConfigVersionsRead()
	}
	if ifVersion != "" && ifVersion == version {
		if err := tx.Commit(ctx); err != nil {
			return "", nil, fmt.Errorf("store: commit effective config snapshot: %w", err)
		}
		return version, nil, nil
	}
	layers, err := loadEffectiveConfigLayers(ctx, qtx, s.resolveTenant(ctx), agent)
	if err != nil {
		return "", nil, err
	}
	version = EffectiveConfigVersion(layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion)
	if err := tx.Commit(ctx); err != nil {
		return "", nil, fmt.Errorf("store: commit effective config snapshot: %w", err)
	}
	bundle, _, dropped, orphans, err := MergeConfigBundles(layers.bundles)
	if err != nil {
		return "", nil, fmt.Errorf("effective config for agent %s invalid (default %s, tenant %s, user %s, role %s): %s", agent, layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion, err.Error())
	}
	warnDroppedConfigMembers(ctx, dropped)
	warnEffectiveProfileAgentOrphans(ctx, orphans)
	return version, bundle, nil
}

// EffectiveAgentConfigMembers returns each winning member's layer in path order.
func (s *Store) EffectiveAgentConfigMembers(ctx context.Context, agent AccountID) (version string, members []EffectiveMember, err error) {
	layers, err := loadEffectiveConfigLayers(ctx, s.q, s.resolveTenant(ctx), agent)
	if err != nil {
		return "", nil, err
	}
	version = EffectiveConfigVersion(layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion)
	_, members, dropped, orphans, err := MergeConfigBundles(layers.bundles)
	if err != nil {
		return "", nil, fmt.Errorf("effective config for agent %s invalid (default %s, tenant %s, user %s, role %s): %s", agent, layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion, err.Error())
	}
	warnDroppedConfigMembers(ctx, dropped)
	warnEffectiveProfileAgentOrphans(ctx, orphans)
	return version, members, nil
}

type effectiveConfigLayers struct {
	bundles        LayerBundles
	defaultVersion string
	tenantVersion  string
	userVersion    string
	roleVersion    string
}

func loadEffectiveConfigLayers(ctx context.Context, q *db.Queries, tenant TenantID, agent AccountID) (effectiveConfigLayers, error) {
	rows, err := q.EffectiveAgentConfigLayers(ctx, db.EffectiveAgentConfigLayersParams{AccountID: string(agent), TenantID: string(tenant)})
	if err != nil {
		if noRows(err) {
			return effectiveConfigLayers{}, fmt.Errorf("%w: agent %q", ErrNotFound, agent)
		}
		return effectiveConfigLayers{}, fmt.Errorf("store: read effective config layers: %w", err)
	}
	if len(rows) == 0 {
		return effectiveConfigLayers{}, fmt.Errorf("%w: agent %q", ErrNotFound, agent)
	}
	var layers effectiveConfigLayers
	for _, row := range rows {
		switch row.Layer {
		case int16(LayerDefault):
			layers.bundles.Default, layers.defaultVersion = row.Bundle, row.Version
		case int16(LayerTenant):
			layers.bundles.Tenant, layers.tenantVersion = row.Bundle, row.Version
		case int16(LayerUser):
			layers.bundles.User, layers.userVersion = row.Bundle, row.Version
		case int16(LayerRole):
			layers.bundles.Role, layers.roleVersion = row.Bundle, row.Version
		}
	}
	return layers, nil
}

func overlayLayerForKey(key OverlayKey) (ConfigLayer, error) {
	if key.Owner == "" {
		if key.Role != "" {
			return 0, fmt.Errorf("%w: role overlay requires an owner", ErrInvalidArgument)
		}
		return LayerTenant, nil
	}
	if key.Role == "" {
		return LayerUser, nil
	}
	if !IsManagerRole(key.Role) {
		return 0, fmt.Errorf("%w: unknown config overlay role %q", ErrInvalidArgument, key.Role)
	}
	return LayerRole, nil
}

func loadTenantConfigLocks(ctx context.Context, q *db.Queries, tenant TenantID) (ConfigLocks, error) {
	bundle, err := q.AgentConfigOverlayLocks(ctx, string(tenant))
	if err != nil {
		if noRows(err) {
			return ConfigLocks{}, nil
		}
		return ConfigLocks{}, fmt.Errorf("store: read tenant config locks: %w", err)
	}
	if err := validateStoredConfigBundle(bundle, LayerTenant); err != nil {
		return ConfigLocks{}, fmt.Errorf("store: invalid stored tenant config bundle: %s", err.Error())
	}
	files, err := readConfigBundleMembers(bundle)
	if err != nil {
		return ConfigLocks{}, fmt.Errorf("store: read stored tenant config bundle: %s", err.Error())
	}
	body, ok := files[memberLocksYML]
	if !ok {
		return ConfigLocks{}, nil
	}
	locks, err := parseConfigLocks(body)
	if err != nil {
		return ConfigLocks{}, fmt.Errorf("store: invalid stored tenant config locks: %s", err.Error())
	}
	return locks, nil
}
