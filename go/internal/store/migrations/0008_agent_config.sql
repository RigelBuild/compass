-- 0008_agent_config: the Server-side fleet CONFIG-BUNDLE store (SEA-1624 T1).
-- This table holds the ONE fleet-wide agent config bundle — the gzip-tarball of
-- skills/, extensions/, and mcp/ material every agent materializes into its
-- scoped config dir (T3/T4) — beside the secrets NAMES registry (0002_secrets).
-- Unlike secrets (a set of named rows), config is a SINGLETON: there is exactly
-- one current bundle for the whole fleet, so this table holds at most one row.
--
-- Singleton pattern, and load-bearing:
--   * singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton) — the PK is a
--     constant TRUE, so a second INSERT collides on the primary key and the
--     CHECK forbids any other value ever reaching the column. Together they pin
--     the table to a single row. PutAgentConfig upserts on this key
--     (INSERT ... ON CONFLICT (singleton) DO UPDATE), which is the whole-bundle
--     replace below.
--   * CURRENT-ONLY retention — there is no history. A new bundle REPLACES the
--     current row in place (the upsert), superseding the prior one atomically;
--     the store never keeps a superseded bundle. Point-in-time rollback is a
--     named FUTURE seam (a versions table), not a gap here.
--   * version IS the content hash — not a monotonic counter. It is the canonical
--     sha256 over the bundle's DECOMPRESSED, metadata-zeroed (path, bytes)
--     content (see agent_config.go canonicalConfigVersion): tar member ordering,
--     mtimes/uid/gid, and gzip framing never perturb it, so a re-put of
--     byte-identical CONTENT yields a stable version and agents can skip a
--     redundant re-materialize by comparing versions.
--   * bundle content is CREDENTIAL-FREE by MVP rule (CD-3) — secrets ride the
--     separate names-registry + SecretSpec resolve path (0002_secrets), never
--     this bundle. This table stores only the non-secret config material.

CREATE TABLE agent_config_bundle (
    -- singleton: the one-row pin. Always TRUE; the PK + CHECK make a second row
    -- impossible, so this table is the fleet's single current config bundle.
    singleton  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    -- version: the canonical content hash of the DECOMPRESSED bundle (sha256
    -- hex), validated and computed at the store door (PutAgentConfig). Stable
    -- across transport re-packing of identical content; the fleet's cache key.
    version    TEXT NOT NULL,
    -- bundle: the gzip-tarball bytes as delivered — transport for the skills/,
    -- extensions/, and mcp/ material. Validated at the door (whitelisted top
    -- dirs, no path escapes, size/count caps, valid mcp JSON) before it can
    -- reach this row. BYTEA: this is the Postgres store (see 0002_secrets).
    bundle     BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
