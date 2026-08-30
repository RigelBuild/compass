package store

import "context"

// tenantContextKey is the private key under which a request's resolved TenantID
// is carried. The auth layer sets it per request after resolving token →
// account → tenant; the store reads it per write to stamp tenancy. Unexported
// so only this package can set or read it — tenant identity can never be
// spoofed through a request field (mirrors comms.actorContextKey).
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
