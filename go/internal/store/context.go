package store

import "context"

// tenantContextKey privately carries the tenant resolved by authentication, so
// callers cannot spoof tenant identity through a request field.
type tenantContextKey struct{}

// WithTenant returns a context carrying t as the resolved tenant. The auth
// interceptor calls it after resolving a token to a tenant; tests call it to
// exercise a specific tenant.
func WithTenant(ctx context.Context, t TenantID) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, t)
}

// TenantFromContext reports the resolved tenant set on ctx, if any. On the OSS
// single-tenant path no interceptor sets one, so the store falls back to the
// bootstrap tenant (Store.resolveTenant); the bool distinguishes an unset
// context from a deliberately-set tenant.
func TenantFromContext(ctx context.Context) (TenantID, bool) {
	t, ok := ctx.Value(tenantContextKey{}).(TenantID)
	return t, ok
}

// agentSessionContextKey carries the posting agent session into store writes
// that take no session parameter, such as the ask mirrors AppendMessage queues.
type agentSessionContextKey struct{}

// WithAgentSession returns a context carrying the verified posting session id.
func WithAgentSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, agentSessionContextKey{}, sessionID)
}

// AgentSessionFromContext reports the posting session set on ctx, or "".
func AgentSessionFromContext(ctx context.Context) string {
	id, _ := ctx.Value(agentSessionContextKey{}).(string)
	return id
}
