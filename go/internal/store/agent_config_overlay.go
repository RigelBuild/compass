package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type OverlayKey struct {
	Owner AccountID
	Role  string
}

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
	var locks ConfigLocks
	if layer == LayerUser || layer == LayerRole {
		locks, err = loadTenantConfigLocks(ctx, q, s.resolveTenant(ctx))
		if err != nil {
			return "", err
		}
		files, err := readConfigBundleMembers(bundle)
		if err != nil {
			return "", fmt.Errorf("store: read config overlay bundle: %w", err)
		}
		for member := range files {
			if locks.Covers(member) {
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

func (s *Store) EffectiveAgentConfig(ctx context.Context, agent AccountID, ifVersion string) (version string, bundle []byte, err error) {
	layers, err := loadEffectiveConfigLayers(ctx, s.q, s.resolveTenant(ctx), agent)
	if err != nil {
		return "", nil, err
	}
	if s.afterConfigVersionsRead != nil {
		s.afterConfigVersionsRead()
		layers, err = loadEffectiveConfigLayers(ctx, s.q, s.resolveTenant(ctx), agent)
		if err != nil {
			return "", nil, err
		}
	}
	version = EffectiveConfigVersion(layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion)
	bundle, _, dropped, err := MergeConfigBundles(layers.bundles)
	if err != nil {
		return "", nil, fmt.Errorf("effective config for agent %s invalid (default %s, tenant %s, user %s, role %s): %s", agent, layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion, err.Error())
	}
	warnDroppedConfigMembers(ctx, dropped)
	if ifVersion != "" && ifVersion == version {
		return version, nil, nil
	}
	return version, bundle, nil
}

func (s *Store) EffectiveAgentConfigMembers(ctx context.Context, agent AccountID) (version string, members []EffectiveMember, err error) {
	layers, err := loadEffectiveConfigLayers(ctx, s.q, s.resolveTenant(ctx), agent)
	if err != nil {
		return "", nil, err
	}
	version = EffectiveConfigVersion(layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion)
	_, members, dropped, err := MergeConfigBundles(layers.bundles)
	if err != nil {
		return "", nil, fmt.Errorf("effective config for agent %s invalid (default %s, tenant %s, user %s, role %s): %s", agent, layers.defaultVersion, layers.tenantVersion, layers.userVersion, layers.roleVersion, err.Error())
	}
	warnDroppedConfigMembers(ctx, dropped)
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
	files, err := readConfigBundleMembers(bundle)
	if err != nil {
		return ConfigLocks{}, fmt.Errorf("store: read tenant config bundle: %w", err)
	}
	body, ok := files["locks.yml"]
	if !ok {
		return ConfigLocks{}, nil
	}
	locks, err := parseConfigLocks(body)
	if err != nil {
		return ConfigLocks{}, fmt.Errorf("store: parse stored tenant config locks: %w", err)
	}
	return locks, nil
}
