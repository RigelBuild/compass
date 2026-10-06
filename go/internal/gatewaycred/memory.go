package gatewaycred

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/RigelBuild/compass/go/internal/store"
)

type memoryCredential struct {
	credential Credential
	created    uint64
	disabledAt time.Time
	cause      string
}

// Memory is a tenant-isolated in-memory CredentialStore and PoolResolver.
type Memory struct {
	mu         sync.Mutex
	tenants    map[store.TenantID]map[string]memoryCredential
	agentOwner map[store.AccountID]store.AccountID
	created    uint64
}

var (
	_ CredentialStore = (*Memory)(nil)
	_ PoolResolver    = (*Memory)(nil)
)

// NewMemory returns an empty credential store.
func NewMemory() *Memory {
	return &Memory{
		tenants:    make(map[store.TenantID]map[string]memoryCredential),
		agentOwner: make(map[store.AccountID]store.AccountID),
	}
}

// SetAgentOwner registers the owner used to resolve this agent's pool.
func (m *Memory) SetAgentOwner(agent, owner store.AccountID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agentOwner == nil {
		m.agentOwner = make(map[store.AccountID]store.AccountID)
	}
	m.agentOwner[agent] = owner
}

// Create stores a credential at version 1 and assigns its id.
func (m *Memory) Create(ctx context.Context, c Credential) (Credential, error) {
	if err := validateCredential(c); err != nil {
		return Credential{}, err
	}
	id, err := credentialID()
	if err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: create id: %w", err)
	}
	c.ID = id
	c.Version = 1
	c = cloneCredential(c)

	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.tenant(ctx)
	m.created++
	rows[id] = memoryCredential{credential: c, created: m.created}
	return cloneCredential(c), nil
}

// List returns enabled own and shared credentials, optionally filtered by provider.
func (m *Memory) List(ctx context.Context, owner store.AccountID, provider string) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.tenant(ctx)
	out := make([]memoryCredential, 0, len(rows))
	for _, row := range rows {
		c := row.credential
		if !row.disabledAt.IsZero() || (c.Scope == ScopeOwn && c.OwnerUserID != owner) || (provider != "" && c.Provider != provider) {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].credential.Provider != out[j].credential.Provider {
			return out[i].credential.Provider < out[j].credential.Provider
		}
		if out[i].created != out[j].created {
			return out[i].created < out[j].created
		}
		return out[i].credential.ID < out[j].credential.ID
	})
	credentials := make([]Credential, 0, len(out))
	for _, row := range out {
		credentials = append(credentials, cloneCredential(row.credential))
	}
	return credentials, nil
}

// UpdateOAuth replaces an OAuth grant in the agent's active pool.
func (m *Memory) UpdateOAuth(ctx context.Context, agent store.AccountID, id string, tok OAuthToken, expected int64) (int64, error) {
	if id == "" || agent == "" || expected == 0 || tok.Access == "" {
		return 0, fmt.Errorf("%w: id, agent, expected version, and access token are required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.tenant(ctx)
	row, ok := rows[id]
	owner, knownAgent := m.agentOwner[agent]
	if !ok || !row.disabledAt.IsZero() || !knownAgent || !m.inPool(rows, row, owner) {
		return 0, ErrNotFound
	}
	if row.credential.oauth == nil {
		return 0, ErrFailedPrecondition
	}
	if row.credential.Version != expected {
		return 0, ErrVersionConflict
	}
	merged := mergeOAuth(*row.credential.oauth, tok)
	row.credential = NewOAuthCredential(row.credential, merged)
	row.credential.Version++
	rows[id] = row
	return row.credential.Version, nil
}

// Disable removes a credential from every pool and records its cause.
func (m *Memory) Disable(ctx context.Context, agent store.AccountID, id, cause string, expected int64) error {
	if id == "" || agent == "" || expected == 0 {
		return fmt.Errorf("%w: id, agent, and expected version are required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.tenant(ctx)
	row, ok := rows[id]
	owner, knownAgent := m.agentOwner[agent]
	if !ok || !row.disabledAt.IsZero() || !knownAgent || !m.inPool(rows, row, owner) {
		return ErrNotFound
	}
	if row.credential.Version != expected {
		return ErrVersionConflict
	}
	row.disabledAt = time.Now()
	row.cause = cause
	row.credential.Version++
	rows[id] = row
	return nil
}

// Pool resolves own-before-shared credentials for each provider.
func (m *Memory) Pool(ctx context.Context, agent store.AccountID) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.agentOwner[agent]
	if agent == "" || !ok {
		return nil, ErrNotFound
	}
	rows := m.tenant(ctx)
	selected := make([]memoryCredential, 0, len(rows))
	for _, row := range rows {
		if !row.disabledAt.IsZero() {
			continue
		}
		c := row.credential
		if c.Scope == ScopeOwn && c.OwnerUserID == owner {
			selected = append(selected, row)
			continue
		}
		if c.Scope == ScopeShared && !hasEnabledOwn(rows, owner, c.Provider) {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].credential.Provider != selected[j].credential.Provider {
			return selected[i].credential.Provider < selected[j].credential.Provider
		}
		if selected[i].created != selected[j].created {
			return selected[i].created < selected[j].created
		}
		return selected[i].credential.ID < selected[j].credential.ID
	})
	out := make([]Credential, 0, len(selected))
	for _, row := range selected {
		out = append(out, cloneCredential(row.credential))
	}
	return out, nil
}

func (m *Memory) tenant(ctx context.Context) map[string]memoryCredential {
	if m.tenants == nil {
		m.tenants = make(map[store.TenantID]map[string]memoryCredential)
	}
	tenant, _ := store.TenantFromContext(ctx)
	rows := m.tenants[tenant]
	if rows == nil {
		rows = make(map[string]memoryCredential)
		m.tenants[tenant] = rows
	}
	return rows
}

func (m *Memory) inPool(rows map[string]memoryCredential, candidate memoryCredential, owner store.AccountID) bool {
	c := candidate.credential
	if c.Scope == ScopeOwn {
		return c.OwnerUserID == owner
	}
	return c.Scope == ScopeShared && !hasEnabledOwn(rows, owner, c.Provider)
}

func hasEnabledOwn(rows map[string]memoryCredential, owner store.AccountID, provider string) bool {
	for _, row := range rows {
		c := row.credential
		if row.disabledAt.IsZero() && c.Scope == ScopeOwn && c.OwnerUserID == owner && c.Provider == provider {
			return true
		}
	}
	return false
}

func validateCredential(c Credential) error {
	if c.Provider == "" {
		return fmt.Errorf("%w: provider is required", ErrInvalidArgument)
	}
	if (c.Scope == ScopeOwn && c.OwnerUserID == "") || (c.Scope == ScopeShared && c.OwnerUserID != "") || (c.Scope != ScopeOwn && c.Scope != ScopeShared) {
		return fmt.Errorf("%w: scope and owner do not match", ErrInvalidArgument)
	}
	apiKey := c.apiKey != "" && c.oauth == nil
	oauth := c.oauth != nil && c.oauth.Access != "" && c.apiKey == ""
	if apiKey == oauth {
		return fmt.Errorf("%w: exactly one credential value is required", ErrInvalidArgument)
	}
	return nil
}

func mergeOAuth(old, next OAuthToken) OAuthToken {
	if next.Access == "" {
		next.Access = old.Access
	}
	if next.Refresh == "" {
		next.Refresh = old.Refresh
	}
	if next.ExpiresUnixMs == 0 {
		next.ExpiresUnixMs = old.ExpiresUnixMs
	}
	if next.EnterpriseURL == "" {
		next.EnterpriseURL = old.EnterpriseURL
	}
	if next.ProjectID == "" {
		next.ProjectID = old.ProjectID
	}
	if next.Email == "" {
		next.Email = old.Email
	}
	if next.AccountID == "" {
		next.AccountID = old.AccountID
	}
	if next.APIEndpoint == "" {
		next.APIEndpoint = old.APIEndpoint
	}
	if next.OrgID == "" {
		next.OrgID = old.OrgID
	}
	if next.OrgName == "" {
		next.OrgName = old.OrgName
	}
	if next.AuthorizedAtUnixMs == 0 {
		next.AuthorizedAtUnixMs = old.AuthorizedAtUnixMs
	}
	if next.Region == "" {
		next.Region = old.Region
	}
	if next.InferenceRegion == "" {
		next.InferenceRegion = old.InferenceRegion
	}
	if next.ActiveOrganizationID == "" {
		next.ActiveOrganizationID = old.ActiveOrganizationID
	}
	return next
}

func cloneCredential(c Credential) Credential {
	if c.oauth != nil {
		tok := *c.oauth
		c.oauth = &tok
	}
	return c
}
