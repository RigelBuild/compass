-- Tokens record the tenant they were issued under, so the bearer door can scope a
-- request before any tenant GUC exists (tokens stays outside RLS for that reason).
-- Every token minted before this migration was issued under the bootstrap tenant,
-- because no request path set a tenant; a missing bootstrap row fails SET NOT NULL.

-- tokens holds one row per live credential, so the scan and lock are brief.
-- squawk-ignore adding-foreign-key-constraint
ALTER TABLE tokens ADD COLUMN tenant_id TEXT REFERENCES tenants (id) ON DELETE RESTRICT;

UPDATE tokens SET tenant_id = (SELECT id FROM tenants WHERE slug = 'default')
WHERE tenant_id IS NULL;

-- squawk-ignore adding-not-nullable-field
ALTER TABLE tokens ALTER COLUMN tenant_id SET NOT NULL;
