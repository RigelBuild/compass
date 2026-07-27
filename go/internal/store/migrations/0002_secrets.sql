-- 0002_secrets: the Server-side secrets NAMES registry (SEA-1327 T3). This
-- table defines the DECLARED set of secrets — their names and how each is
-- delivered/routed — and NOTHING about their values. Values live only in the
-- SecretSpec provider (keyring/1Password/Vault/…); the Server resolves them at
-- fetch time and never persists them. The registry exists because the
-- SecretSpec resolver is manifest-driven and exposes no enumeration, so the
-- Server generates the manifest it resolves against from these rows
-- (compass-agent-container-runtime.md §T3, Decision 2).
--
-- Deliberately absent, and load-bearing:
--   * NO value column — encryption-at-rest is the provider's job, not ours.
--   * NO per-agent grant column — the MVP injects the whole store into every
--     agent (inject-all); per-agent scoping is a named FUTURE seam that adds a
--     filter to FetchSecrets without reshaping this table.

CREATE TABLE secrets (
    -- The secret's name, validated at the store door against SecretSpec's
    -- env-var-name grammar (^[A-Za-z_][A-Za-z0-9_]*$) before it can reach a
    -- row: it later becomes a path segment under $HOME/.compass/secrets/ and a
    -- line in a root-adjacent setup script (T5), so it is constrained at the
    -- door, not escaped downstream. Unique — the declared set is a set of names.
    name        TEXT PRIMARY KEY,
    -- delivery: the load-bearing file-vs-env split that determines how a secret
    -- rotates (0 file, 1 env). Stored as the small int the secrets resolve
    -- surface uses, CHECK-pinned so a bad value can never reach a row.
    delivery    SMALLINT NOT NULL CHECK (delivery IN (0, 1)),
    -- kind: the routing class the T5 materializer switches on (0 generic,
    -- 1 provider/LLM, 2 gh). Provider rows carry a non-empty provider; gh rows
    -- carry a non-empty host.
    kind        SMALLINT NOT NULL CHECK (kind IN (0, 1, 2)),
    -- provider: the SecretSpec/SDK provider id for a provider (LLM) secret, so
    -- T5 routes it to the AuthStorage seed rather than a generic channel. Empty
    -- for non-provider kinds.
    provider    TEXT NOT NULL DEFAULT '',
    -- host: the forge host for a gh secret (default github.com), so T5 routes it
    -- to the gh hosts.yml placement. Empty for non-gh kinds.
    host        TEXT NOT NULL DEFAULT '',
    -- declared_by: the account that declared this secret (the write path is
    -- user-only, enforced at the RPC edge in T7). FK ON DELETE RESTRICT so a
    -- referenced account cannot be orphaned out from under a declaration.
    declared_by TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- kind↔provider/host invariant, enforced (not merely documented): a
    -- provider row (kind=1) carries a non-empty provider and no host; a gh row
    -- (kind=2) carries a non-empty host and no provider; a generic row (kind=0)
    -- carries neither. Without this a malformed row (e.g. a provider kind with
    -- an empty provider) persists silently and misroutes at the T5 materializer;
    -- the CHECK fails it at write time. DeclareSecret guards the same invariant
    -- so a caller gets ErrInvalidArgument, not a raw constraint violation.
    CONSTRAINT secrets_kind_routing CHECK (
        (kind = 0 AND provider = '' AND host = '')
        OR (kind = 1 AND provider <> '' AND host = '')
        OR (kind = 2 AND host <> '' AND provider = '')
    )
);

CREATE INDEX secrets_declared_by_idx ON secrets (declared_by);
