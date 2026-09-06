# gateway_credentials at-rest encryption + master-key custody

Status: Active

Tracking: RIG-2863 (parent RIG-1715)

Addendum to the frozen record
[`compass-server-llm-gateway`](./compass-server-llm-gateway/design.md)
(§Credential storage and rotation, L312–373). Scope: encryption-at-rest for the
`gateway_credentials` value store ONLY. This record does not re-open the
store's shape, the CAS `version` discipline, the scope column, the
RPC-vs-Postgres-direct wiring, or the stack-token channel — those stay as the
frozen record decided. Ships as its own PR; frozen on merge; the
credential-store build proceeds against it.
One deliberate scope expansion, Matt-ruled (D6/T0): server-only secrets get a
physically SEPARATE `server_secrets` store (mechanism C1) — a new table plus
a second resolver instance — so the master key and the existing
PEM/webhook/Linear server secrets are structurally undeliverable to agent
containers (the table split PLUS the F1 reserved-prefix partition — server-secret
names carry a reserved prefix the user declare/set path rejects, making the two
keyspaces disjoint by construction; the split alone is necessary but not
sufficient; see D6).
C1 needs no public proto ENUM change: it REMOVES the
`SECRET_DELIVERY_SERVER_ONLY` proto/store/secrets enum additions (and the
FetchSecrets filter) the earlier delivery-kind mechanism required, and leaves
the container FetchSecrets path byte-for-byte unchanged. It DOES add two
admin-gated methods to `SecretsService` (`proto/compass/v1/compass.proto`,
the public codegen lane) — an additive, non-breaking service-method addition,
not a change to an existing wire enum.

## Problem / Intent

The gateway record adds `gateway_credentials`, the FIRST value-persisting store
in compass — deliberately breaking the names-only half of the secrets
invariant. The invariant it breaks is stated at
`go/internal/secrets/secrets.go:20-22`:

> "Values live only in the provider and this process's memory during a
> resolve; they are never persisted by Compass and never logged."

The frozen record specifies the new store's shape ("api_key and OAuth-shaped
payloads (access/refresh/expiry), a monotonic `version` per row supplying the
CAS substrate, and a scope column", design.md:324-326) but says nothing about
encryption at rest. A security red-team flagged this HIGH (CWE-311/312/522/532):
a plaintext value column makes every `pg_dump`, read replica, backup, and
operator `SELECT` a bulk all-tenant credential exfiltration path. The record
already concedes the blast radius is total at the process level
(design.md:333-337):

> "Blast radius, stated: a compromised gateway (holding one stack token) can
> read every tenant's provider credentials — the isolation boundary is the
> per-tenant pool scoping enforced server-side on the credential-list surface
> (below), not the TS process, which by design holds them all."

That concession covers a compromised *gateway process*; it does not license a
plaintext *database*. This addendum adds the missing at-rest-encryption and
master-key-custody decision before the store is built.

## Approach

Matt has ruled the core approach; this section records it as decisions.

### D1 — Application-layer AES-256-GCM envelope encryption

The credential value payload (the full api_key / OAuth
access+refresh+expiry JSON) is encrypted application-side before it reaches
Postgres: AES-256-GCM, a fresh random 96-bit nonce per encryption (per write,
never reused), authenticated. The `gateway_credentials` row stores ONLY
`value_ciphertext` + `value_nonce`
(+ `key_version`, D4); no plaintext value column exists in any migration
version — the store is born encrypted. A DB dump, replica, or SELECT alone
yields nothing usable.

Why the fresh-nonce discipline is absolute, quantified: reusing a 96-bit GCM
nonce under one key forfeits both confidentiality AND authenticity — a
two-message catastrophe (keystream XOR + authentication-key recovery), not a
gradual weakening. Random 96-bit nonces are collision-safe only to ~2^32
encryptions per key (NIST SP 800-38D). The write rate is not one-per-credential:
every hourly OAuth refresh write-back is a fresh encryption ("OAuth
access/refresh tokens are refreshed by the gateway on the hour",
design.md:322-323). At plausible scale (10^3–10^4 credentials, hourly) that is
~10^7–10^8 encryptions/year — comfortably under 2^32, so v1's single live key
has years of headroom; key rotation (OQ-1) is the nonce-budget release valve
when scale grows, which is itself part of why OQ-1's deferral is safe.

This is the first at-rest crypto in the Go tree — verified by grep this
session: no `crypto/aes` / `cipher.NewGCM` usage exists anywhere under `go/`
(existing `crypto/rand` uses are IDs/tokens/certs only, e.g.
`go/internal/store/ids.go:4`). The envelope helper is therefore a new (small,
justified) package: no existing abstraction carries symmetric at-rest crypto,
and the seam must be reusable if a later store ever needs the same discipline.

### D2 — Master key auto-provisioned into the existing SecretSpec provider

The master key never touches the DB and never requires a human step
(rule://no-human-clicks). Every running Compass already has exactly one
configured SecretSpec provider — the seam described at
`go/internal/secrets/secrets.go:10-13`:

> "this package reads that registry, generates the SecretSpec manifest the
> resolver resolves against, calls SecretSpec to resolve the actual values
> from the configured provider (keyring/1Password/Vault/…)"

On boot, Compass resolves the declared master-key secret
(`GATEWAY_CREDENTIALS_MASTER_KEY`). If absent, it generates a fresh 256-bit
key from `crypto/rand` and — serialized against concurrent booters through a
Postgres advisory lock (T2) — provisions it:

- writes the value into the provider via `secrets.Resolver.Set`
  (`go/internal/secrets/resolver.go:234`, `func (r *SpecResolver) Set(ctx
  context.Context, name, value, reason string) error` — "Set writes value into
  the provider for name via the pinned CLI, feeding the value on stdin (never
  argv, so it is not visible in the host process list)",
  resolver.go:211-212);
- registers the name in the SEPARATE `server_secrets` store (D6) via the
  server-internal `DeclareServerSecret` (T0) — a mirror of
  `store.DeclareSecret` (`go/internal/store/secrets.go:82`, `func (s *Store)
  DeclareSecret(ctx context.Context, actor AccountID, name string, delivery
  SecretDelivery, kind SecretKind, provider, host string) error` — "It
  stores NO value — the value lives in the SecretSpec provider",
  secrets.go:74-75) MINUS the delivery/kind routing parameters, which do not
  exist for server secrets (they are never container-delivered and never
  reach the T5 materializer) — never into the user `secrets` table, whose
  every row rides the container-delivery manifest;
- re-resolves and byte-compares before the key is ever used to encrypt (the
  read-back verify, T2).

Thereafter the key is resolved at boot — through the SERVER-SECRET resolver
instance (D6), the second `SpecResolver` reading `server_secrets` — and held
in process memory only, following the existing declared-secret boot-resolve
pattern the Forge App PEM uses (`newDeclaredSecretResolver`,
`go/server/serve.go:1469`: "resolves the declared server_only secret NAME to
its raw value bytes on each call", serve.go:1463-1464 — a "server_only" that
is convention-only prose today; D6 makes it a physically separate store).
Unlike the webhook secret's per-request resolve, the master key is resolved
once at startup and cached for the process lifetime
(a decrypt happens on every credential read; a provider round-trip per decrypt
would be the same amplification `cachedWebhookSecret` exists to prevent,
serve.go:1484-1491).

### D3 — Managed plane: KMS as just another provider URI, zero core change

The managed plane points the ONE declared master-key secret at a KMS-backed
SecretSpec provider URI. The core stays cloud-agnostic; there is no KMS SDK,
no cloud-conditional code path, and no self-host/managed fork in the crypto
code. KMS-grade custody is a deployment configuration, not a code change.

### D4 — Key versioning column from day one

Each row carries a `key_version` (smallint, starts at 1) identifying which
master-key generation encrypted it. Whether v1 ships active rotation is an
open question (OQ-1), but the column is load-bearing NOW: adding it later
means a schema migration plus a backfill under ambiguity about which key
encrypted which row. Cheap at creation, expensive retrofit.

### D5 — Redaction discipline: Stringer alone is not enough

Breaking the never-persisted half of the invariant does not touch the
never-logged half — but the established Stringer pattern
(`ResolvedSecret.String()`/`GoString()`, `go/internal/secrets/secrets.go:150-156`:

> `func (s ResolvedSecret) String() string { return fmt.Sprintf("ResolvedSecret{name: %q, kind: %d, delivery: %d, value: <redacted>}", …) }`
> `func (s ResolvedSecret) GoString() string { return s.String() }`

) covers only fmt-verb formatting — and slog under a TextHandler, which
formats via the fmt path. It does NOT cover a slog `JSONHandler` (reflection
over exported fields, ignores `String()`), `json.Marshal`, or direct field
access — and T4's decrypted-payload type is exactly the OAuth JSON shape that
gets marshaled toward the gateway RPC, so it will have serializable fields.
A one-line handler swap must not silently un-redact the fleet.

Therefore the value-bearing types this record adds MUST either:

- keep secret fields UNEXPORTED with accessor methods (the `envelope.Key`
  posture: unexported `[32]byte`, reflection-proof), or — where the payload
  shape needs exported fields — implement `slog.LogValuer` AND a redacting
  `MarshalJSON` alongside the `String()`/`GoString()` pair; and
- never serve as the RPC serialization type: the gateway-bound response is
  built by an explicit proto/DTO conversion, never `json.Marshal` of the
  payload type itself.

The store never logs a row's value fields, and error paths wrap without
embedding plaintext or key material.

### D6 — A physically separate `server_secrets` store (mechanism C1, Matt-ruled)

The threat D2 must defeat: the declared-secrets registry is inject-all by
design — "NO per-agent grant column (the MVP injects the whole store into
every agent; per-agent scoping is a named FUTURE seam)"
(`go/internal/store/migrations/0001_init.sql:391-393`). FetchSecrets resolves
the WHOLE registry with no filter — `resolved, err := h.resolver.Resolve(ctx,
"runner fetch")` then `for _, s := range resolved { out = append(out,
resolvedSecretToProto(s)) }` (`go/internal/runnerhub/handler.go:303-311`) —
and the Runner materializes every resolved value into the container pre-exec
(`resolved, err := h.link.FetchSecretsByContainer(ctx, name)` →
`h.materializer.Install(ctx, handle.ID(), handle.HomeDir(),
handle.WorkspaceUID(), resolved)`, `go/internal/runner/host.go:378-383`).
Declared into the `secrets` table at all, the master key — which decrypts
every tenant credential — would land on disk inside every untrusted agent
sandbox, defeating the encryption entirely.

**Matt ruled: a separate store, not a delivery flag.** Server-only secrets
get their OWN physically separate table, `server_secrets`, so they can NEVER
get mixed up with user secrets that are delivered to agent containers.
Container delivery becomes default-CLOSED by construction, not
default-open-minus-a-filter.

Why the table boundary IS the delivery boundary: the delivery surface is the
resolver's MANIFEST. `SpecResolver` reads its declared set through the
`declarations` interface — `DeclaredSecrets(ctx context.Context)
([]store.SecretDeclaration, error)` (`go/internal/secrets/resolver.go:35-37`;
the `store declarations` struct field, resolver.go:66) — and `buildManifest`
"renders the SecretSpec manifest TOML for a declared set: one `[project]`
block and one `[profiles.<profile>]` block with every declared name as a
required key" (resolver.go:108-110; the function, resolver.go:114-137);
`Resolve` can only return names present in that manifest. Today ONE resolver
instance (`resolver := secrets.NewSpecResolver(st, secretsStateDir(cfg))`,
`go/server/serve.go:528`) serves BOTH the container path (FetchSecrets →
`Resolve`, handler.go:303-311) and the boot consumers (PEM/webhook via
`newDeclaredSecretResolver` → `Resolve`, serve.go:1469-1481). C1 splits the
READ: a store view over `server_secrets` (`ServerDeclaredSecrets`, T0) feeds
a SECOND `SpecResolver` instance. The container resolver keeps reading
`secrets`; its manifest never contains a server-secret name, so a server
secret is undeliverable to containers via the container manifest — there is
nothing to filter. That structural property holds only while no name is
ever present in BOTH tables. F1 makes that structural, by NAME construction:
every server-secret name carries a RESERVED PREFIX (the master-key family keeps
`GATEWAY_CREDENTIALS_`; the six configured forge secrets carry `SERVER_`), and
the two store doors enforce the partition with a pure string check — the admin
`SetServerSecret`/`DeclareServerSecret` path REQUIRES a reserved server-secret
prefix, and the user `SetSecret`/`DeclareSecret` path REJECTS any name carrying
one. The two keyspaces are therefore disjoint by name construction: no name
declared THROUGH EITHER GUARDED DOOR can land in both tables in either order, so
the both-tables end state — a user `SetSecret` minting a shadow `secrets` row
under a server-secret name, or an admin `SetServerSecret` declaring under a name
already live in `secrets` — is UNREACHABLE, not merely healed after the fact.
The guards are prospective (they gate declares, not existing rows), so the claim
is stated against the clean pre-production baseline this record's wipe
establishes: the DB is wiped and re-declared (Matt-ruled — single operator, no
live data), so no legacy `secrets` row carrying a reserved prefix pre-exists to
break the partition, and none can be created thereafter. F1 needs no cross-table
membership SELECT (so no cross-tenant RLS-visibility question) and no boot
reconcile: the partition is enforced at declaration time on both doors. The
table split alone is necessary but not sufficient; the reserved-prefix partition
is what makes it sufficient. The prefix check is a name test only — it does not
widen the underlying declare/set/rollback trio's single-writer contract ("is
not atomic and assumes no concurrent same-name writer", secrets_service.go:88-91),
which the existing `SetSecret` path already carries; per-name write atomicity
across paths is deferred to the multi-writer work that comment flags, tracked
with the per-tenant defer (RIG-3237).

C1 keeps the SAME SecretSpec profile for both instances — the shared project
is `manifestProject = "compass"` (resolver.go:19) and the profile
`defaultProfile = "default"` (resolver.go:23; `WithProfile` exists,
resolver.go:87, but C1 does not use it), and BY DEFAULT one provider URI
configures both resolver instances (F2 WIRING SEAM) — so the provider keyspace
is shared unless the operator opts Layer B onto a different provider.
Because the reserved-prefix partition renames the six forge secrets under
`SERVER_`, and the pre-production deployment carries no live data worth
preserving, the existing six declarations are simply WIPED; their NAMES are
re-declared at boot under their prefixed names from the resolved forge config
(OQ-4), and the operator re-supplies each VALUE in the writable provider under
the prefixed name (name-keyed provider, so the rename requires a provider
write) BEFORE the server boots — not an in-place row move, and not a
running-server RPC call (the RPC/CLI is the rotation surface, not the first-boot
bootstrap path; F2).

**C1 REMOVES the public proto ENUM change** the previous delivery-kind mechanism
required: no `SECRET_DELIVERY_SERVER_ONLY` proto enum value, no
`SecretDeliveryServerOnly` store enum, no secrets-package
`DeliveryServerOnly`, no widened migration CHECK, and no FetchSecrets
delivery filter. The container FetchSecrets path is byte-for-byte UNCHANGED.
The delivery enum gains no new value in any of the four representations —
two live delivery kinds throughout (plus the proto3 `UNSPECIFIED` zero
sentinel, which is not a delivery kind):
`delivery SMALLINT NOT NULL CHECK (delivery IN (0, 1))` (0001_init.sql:399);
`SecretDeliveryFile SecretDelivery = 0` / `SecretDeliveryEnv SecretDelivery
= 1` (`go/internal/store/secrets.go:18-25`); `DeliveryFile` / `DeliveryEnv`
(`go/internal/secrets/secrets.go:39-46`); `SECRET_DELIVERY_UNSPECIFIED = 0;
SECRET_DELIVERY_FILE = 1; SECRET_DELIVERY_ENV = 2;`
(`proto/compass/v1/compass.proto:160-164`).

Writes into `server_secrets` go through a NEW admin-gated
`SetServerSecret`/`DeleteServerSecret` RPC (T0) — mirroring the user
`SetSecret` declare-then-Set flow (`go/server/secrets_service.go:92`) minus
delivery/kind, classified `adminOnly` in `classifyProcedure`
(`go/internal/auth/admin_gate.go:47`, :27; "An unrecognized path (ok=false)
is treated as adminOnly — fail closed, never admit an unknown method as
open", admin_gate.go:44-46). Today's user-facing `SetSecret` cannot declare a
row *as* a server secret — `secretRoutingFromProto` admits only File/Env
delivery (secrets_service.go:275-284), with no server-only delivery value — so
operators declare server secrets through the new RPC. It CAN, however, mint an
ordinary `secrets` row under a NAME that collides with a server secret
(delivery=File/Env), which is exactly why the F1 prefix guard is mandatory on
this user path and not merely on the admin RPC — the user path rejects any
reserved-prefixed name, so the collision cannot be declared.

This retroactively fixes a pre-existing exposure: the App PEM, webhook
signing, and Linear OAuth secrets are server_only by convention only
("Declared server_only secret NAME holding the PRIMARY App PEM private key",
`go/cmd/compass-server/main.go:414-417`; webhook :418-421; Linear :435-450;
boot-resolved by name via `newDeclaredSecretResolver`, serve.go:1469-1481)
and today ride the same inject-all path into every agent container. They are
re-declared under `SERVER_`-prefixed names in the separate `server_secrets`
store in this PR chain (OQ-4, RESOLVED) — the pre-production deployment is
wiped, the NAMES re-declared at boot from the resolved forge config, and the
VALUES re-populated in the writable provider (the admin `SetServerSecret` RPC /
`compass server-secret` CLI serve rotation on a running server, not first-boot
bootstrap; F2), not migrated in place.

### D7 — One seal/open path for `api_key` and OAuth payloads

Both stored payload shapes — an OAuth token bundle and a bare `api_key` — are
sealed and opened through the SAME envelope path (D1); there is no plaintext
branch and no per-shape code fork. Both are secrets of the same sensitivity: a
stored `api_key` is as long-lived and as disclosure-critical as an OAuth
refresh token, and a split path would leave the most static secret class in
plaintext for zero benefit. T3's single encrypted column and T4's single
wiring path encode this. (Matt-ruled; resolves OQ-3.)

## Alternatives considered

The core choice (envelope encryption + auto-provisioned key in the existing
provider) is Matt-ruled and not re-opened; the custody fork and the
server-secret containment-mechanism fork are recorded here because the
rejected branches are the ones a future reader will reach for first.

- **Mechanism A: a real SERVER_ONLY delivery kind + FetchSecrets filter —
  REJECTED (superseded by Matt's C1 ruling).** The previously folded design:
  mint `SERVER_ONLY` across the four delivery representations (migration
  CHECK widen, store enum, secrets package, a PUBLIC proto enum value) plus
  a delivery filter in the FetchSecrets handler skipping SERVER_ONLY rows
  before the append (handler.go:307-310). Rejected because it is
  default-OPEN minus a subtractive filter: server and user secrets share one
  table and one resolver manifest, and one edit — a dropped filter clause, a
  mis-mapped enum arm — re-exposes the master key to every container. It
  also costs a public proto ENUM widening — a change to an existing wire
  contract — where C1's proto delta is two additive `SecretsService` methods.
- **Mechanism C2: separate table + a separate SecretSpec PROFILE —
  considered, DEFERRED.** The same `server_secrets` table, but the server
  resolver pinned to its own profile (`WithProfile`, resolver.go:87) so even
  the provider keyspace is isolated. Fullest isolation — but it is NOT the
  cheaper-migration loser it might appear: under the reserved-prefix partition
  BOTH mechanisms now require a provider write under new keys (the six values
  are re-provisioned regardless), so the honest distinction is WHERE the write
  lands, not WHETHER one is needed. C1 writes under new NAMES in the shared
  profile via a pure string check at the DECLARATION layer, with no second
  profile to configure or operate; C2 would write under the same names in a
  new PROFILE, additionally isolating the provider keyspace (closing the
  residual named below). The pre-production wipe premise (D6) makes either
  write a re-declare, not an in-place migration. C1 was ruled sufficient; the
  profile split remains available later if provider-keyspace isolation is ever
  needed. Named residual under C1: the shared profile leaves the provider
  keyspace a shared mutable surface — no longer reachable by the user
  `SetSecret` path (the F1 prefix guard rejects reserved-prefixed names there),
  but still reachable by an operator's out-of-band `secretspec set` against the
  shared provider — so all SEVEN server-secret names' provider values (the
  master key plus the six re-declared names: primary and reviewer App PEM, the
  GitHub webhook secret, and the three Linear secrets — client id, client
  secret, and webhook) are
  guarded by the
  F1 reserved-prefix partition (T0, both paths — the user path rejects
  reserved-prefixed names, the admin path requires them) at the DECLARATION
  layer rather than at the provider-keyspace layer; the boot read-back verify
  additionally covers the master key. The F2 optional Layer-B provider split
  already delivers provider-keyspace isolation for the user `SetSecret` path
  when an operator opts into it (a per-resolver URI, not a second profile); C2
  would make that isolation STRUCTURAL and unconditional (a dedicated profile
  for all seven names, not an operator opt-out).
  Acceptable with the guard in place; the follow-up (per-tenant credential
  at-rest isolation + gateway-topology exposure) is where C2 is reconsidered
  (RIG-3237).
- **pgcrypto / key-in-DB — REJECTED.** `pgp_sym_encrypt` or a key stored in a
  Postgres table/GUC puts the key inside the same blast radius as the
  ciphertext: one `pg_dump` carries both, reducing the encryption to obfuscation.
  The red-team requirement is specifically that the key lives OUTSIDE the DB.
- **Require an external cloud KMS — REJECTED for the OSS core.** A generic
  self-hoster has no AWS/GCP KMS and must not need one; they have already
  configured exactly one SecretSpec provider (keyring / 1Password / Vault /
  env) to run Compass at all. Mandating KMS either forks the code
  (self-host vs managed paths) or gates self-hosting on a cloud account.
  The chosen approach *subsumes* this alternative: the managed plane gets
  KMS-backed custody by pointing the declared secret at a KMS provider URI
  (D3) — KMS becomes deployment config, not a code requirement.
- **Plaintext now + filed follow-up — REJECTED.** Ships a known HIGH hole and
  creates a migration burden (encrypt-in-place over live rows) that the
  build-it-encrypted-first ordering avoids entirely. Matt ruled the encryption
  record lands BEFORE the store is built.

## Plan

### Global Constraints

- Go server under `go/` (gateway itself is TS/Bun, but this record's code is
  entirely Go server + store side); existing lint/test discipline applies.
- AES-256-GCM only; nonces from `crypto/rand`, 96-bit, fresh per encryption,
  never counter-derived; key is 256-bit from `crypto/rand`.
- The master key NEVER appears in the DB, in logs, in argv (Set feeds stdin,
  resolver.go:211-212), or in error strings.
- Auto-provisioning is zero-human-step (rule://no-human-clicks): first boot
  generates, stores, and declares the key with no operator action.
- The names-only invariant (`secrets.go:20-22`) is preserved for EVERYTHING
  except `gateway_credentials` values; both declared-name registries
  (`secrets` AND the new `server_secrets`) stay names-only — the master
  key's declaration row is names-only like any other. `server_key_state` is
  not a declared-name registry and holds no value: a salted digest of the
  master key plus its salt, which discloses nothing about a 256-bit random
  key — the names-only invariant is untouched by it.
- Every value-bearing type redacts under `%s`/`%v`/`%#v` (D5).
- The master key lives in the separate `server_secrets` store (D6): its name
  never appears in the container resolver's manifest, so it is NEVER
  materialized into an agent container — a structural property of the split
  store, not a filter to maintain.
- Every Seal binds AAD = the row's stable identity (T4); ciphertexts are not
  portable between rows.
- Every `file:line` citation in this record is anchored by its SYMBOL name; the
  line numbers are a convenience that drifts — re-grep the symbol before trusting
  a line.

### T0 — The `server_secrets` store: table, resolver split, admin RPC (prerequisite)

Implements D6 (mechanism C1). PREREQUISITE of T2 — the key cannot be
declared into a store that does not exist.

- `Interfaces:`
  - Migration (a NEW migration file, never an edit to 0001): `CREATE TABLE
    server_secrets` — names-only, mirroring `secrets`
    (0001_init.sql:393-425) MINUS the container-delivery routing: `name TEXT
    PRIMARY KEY` (same env-var-name grammar, validated at the store door);
    `declared_by TEXT REFERENCES accounts (id) ON DELETE RESTRICT`,
    NULLABLE — `NULL` = server-provisioned (the master key; contrast
    `secrets.declared_by`, which is `NOT NULL REFERENCES accounts (id) ON
    DELETE RESTRICT`, 0001_init.sql:410 — justification below);
    `created_at`/`updated_at`. NO `delivery` column (server secrets are
    never container-delivered — that IS the point) and NO `kind` column
    (they never reach the T5 materializer).
  - Key-state table (same migration): a single-row `server_key_state` table
    (`id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1)`, `key_version SMALLINT
    NOT NULL DEFAULT 1`, `key_fingerprint BYTEA NOT NULL`, `fingerprint_salt
    BYTEA NOT NULL`, `updated_at`) holds the T2/T5 key-swap tripwire's
    non-secret salted digest. It is NOT a declared-name registry, so the
    names-only invariant (§Global Constraints) is untouched — a salted digest
    of a key is not that key's VALUE. Same-migration `GRANT SELECT, INSERT,
    UPDATE ON server_key_state TO compass_app, compass_system` (the grant is
    not inherited — see the `server_secrets` grant rationale below). It is
    likewise **bucket-A infrastructure** (deployment-global, no `tenant_id`,
    RLS NOT enabled) and joins `server_secrets` in the `bucketA` allow-list
    (rls_pgtest_test.go:543) so the self-auditing RLS guard test stays a
    conscious gate.
  - Grants + RLS (mandatory — the new-table integration the shipped
    multi-tenancy regime, RIG-3106, requires): the same new migration
    issues `GRANT SELECT, INSERT, UPDATE, DELETE ON server_secrets TO
    compass_app, compass_system`. This is NOT inherited: 0001's grant is a
    one-time `ON ALL TABLES IN SCHEMA` snapshot (0001_init.sql:896) and there
    is no `ALTER DEFAULT PRIVILEGES` anywhere in the migrations, so a table
    created by a later migration receives no grant and every server-side
    resolve would fail `permission denied for table server_secrets` (fails
    closed, but breaks the T2 boot path). RLS posture: `server_secrets` is
    declared **bucket-A infrastructure** (Matt-ruled) — NO `tenant_id`
    column, RLS NOT enabled: the master key and the PEM/webhook/Linear
    secrets are deployment-global (the master key decrypts EVERY tenant's
    credentials), so there is no tenant to scope by. T0 adds `server_secrets`
    to the `bucketA` allow-list in go/internal/store/rls_pgtest_test.go:543
    so the self-auditing RLS guard test stays a CONSCIOUS gate (that test
    keys off `tenant_id`, so a bucket-A table is otherwise invisible to it —
    the allow-list edit is what records the deliberate exemption). Live
    cross-tenant EXPOSURE (one shared gateway process holding all tenants'
    creds) is a gateway-topology property of the parent record
    (design.md:333-337), out of scope here and tracked as a follow-up
    (RIG-3237).
  - declared_by nullability, justified: the boot provisioner declares the
    master key with no human actor. `NULL` = server-provisioned is honest
    provenance; attributing the row to the bootstrap-admin account
    (available at that point in boot, serve.go:358-362) would falsify the
    audit trail and couple key provisioning to account-bootstrap ordering.
    Operator-declared rows (the new RPC) always carry the caller's account
    id; `ON DELETE RESTRICT` still protects them.
  - Store: `DeclareServerSecret(ctx, actor, name)` /
    `DeleteServerSecretDeclaration(ctx, actor, name)` mirroring
    `DeclareSecret` / `DeleteSecretDeclaration` (store/secrets.go:82, :160)
    minus delivery/kind/provider/host (actor nullable-empty for the
    server-provisioned path). `DeclareServerSecret` carries the F1 PREFIX guard
    at the store door — it REQUIRES the name to carry a reserved server-secret
    prefix (`SERVER_` or `GATEWAY_CREDENTIALS_`), rejecting any unprefixed name —
    so no writer (the admin RPC, or any future second writer) can create a
    `server_secrets` row under a name the user keyspace owns. Combined with the
    user path's rejection of those same prefixes (below), the two doors partition
    the keyspace by name, so F1 holds at BOTH doors, not only at the RPC — a pure
    string check, no cross-table read, no transaction spanning both tables. It
    ALSO adds the read
    `ServerDeclaredSecrets(ctx)` —
    a thin store view whose `DeclaredSecrets(ctx context.Context)
    ([]store.SecretDeclaration, error)` method (the `declarations`
    interface shape, resolver.go:35-37) reads `server_secrets`, mapping
    rows to `store.SecretDeclaration` with generic kind / zero delivery
    (the resolver uses only the name to build its manifest), so
    `NewSpecResolver` is reused UNCHANGED.
  - Resolver split: a SECOND `SpecResolver` instance (the SERVER resolver)
    constructed over that view — SAME profile (`defaultProfile = "default"`,
    resolver.go:23) and project (`manifestProject = "compass"`,
    resolver.go:19), its own manifest state dir. The container resolver's
    CONSTRUCTION at serve.go:528 is unchanged — no filter, nothing to filter
    — but its boot CONSUMERS must be re-pointed: because the six server-secret
    names are re-declared under `SERVER_`-prefixed names in `server_secrets`
    (not `secrets`), every boot consumer that
    still resolves those names through the serve.go:528 instance breaks,
    because that instance's manifest no longer declares them. THREE failure
    modes verified in the tree: (a) HARD BOOT FAILURE — `validateForgeSecret`
    (serve.go:1539-1549) returns `forge secret %q not declared` for an absent
    name and is called on the primary App PEM (:1013) and the App webhook
    secret (:1016) — both inside `buildBoardWebhookWiring` under
    `buildForgeReadWiring`, propagated to `failStartup`, so every GitHub-App
    deployment fails its first post-upgrade boot; (b) SILENT SECURITY
    DEGRADATION — `forgeSecretDeclared` (serve.go:1746-1757) returns
    `(false, nil)` for an absent name rather than an error, and its caller
    `buildLinearWebhookWiring` maps that to a legitimate off-state — `if
    !declared { return nil, nil }` (serve.go:1082-1083) — so the Linear webhook
    ingress silently unmounts, and `buildLinearTokenSource` (serve.go:1699-1717)
    drops the Linear write + notify lanes the same way (`return nil, nil` at
    :1716);
    (c) SILENT CAPABILITY LOSS — `wireForgeWriteCaller` (:587) resolves the
    forge-write set and gates on `forgeWritesEnabled` (:1583, over
    `forgeWriteAppsConfigured` :270-275): with BOTH PEM names absent from the
    container resolver's set the predicate reads both-absent,
    `warnPartialForgeWriteSecrets` returns SILENTLY (:1826-1827, `havePrimary
    == haveReviewer`), `wireForgeWriteCaller` returns nil (:1585),
    `hub.SetForgeCaller` is NEVER called, and the reviewer-PEM
    `validateForgeSecret` at :1638 is UNREACHABLE — so agent forge WRITES
    fail-closed to `CodeUnavailable` fleet-wide with no error at boot.
    (Ordering note: (c) is MASKED by (a) on a FULL no-repoint T0 —
    `forgeWriteAppsConfigured` gates `havePrimary` on `App.AppID != 0`
    (serve.go:272), which is exactly `boardIngestionEnabled()` (serve.go:224-226),
    so any writes-enabled deployment hard-fails at the primary-App-PEM
    `validateForgeSecret` (serve.go:1013) before reaching `wireForgeWriteCaller`
    (:587). (c) is the PARTIAL-repoint mode: it becomes live the moment the read
    path is repointed and the write path is not — which is precisely why the
    write lane needs its own positive assertion rather than inheriting the read
    lane's.) So T0
    has an explicit CONSUMER-REPOINT deliverable with TWO seams. (1) Resolver
    instance: thread the SERVER resolver instance (not the serve.go:528
    container instance) into `buildForgeReadWiring` (serve.go:567),
    `buildLinearWebhookWiring`, `buildLinearTokenSource`, `wireForgeWriteCaller`
    (:587) and `buildForgeWriteService`, and into the `newDeclaredSecretResolver`
    / `newCachedWebhookSecret` closures (serve.go:1025/1039/1085/1644) that mint
    the App token + verify webhook HMAC. (2) Resolve-side NAME prefix: the
    prefix RENAMES the six (`LINEAR_FORGE_CLIENT_SECRET` →
    `SERVER_LINEAR_FORGE_CLIENT_SECRET`), and the provider keyspace is keyed by
    NAME, so the DECLARE side (`SetServerSecret`, T0) and the RESOLVE side must
    agree on the prefixed name or every server resolve misses. `resolved()`
    (serve.go:232-246) applies NO prefix — its output feeds operator-facing text
    (log/error lines, the `warnPartialForgeWriteSecrets` and
    `buildLinearTokenSource` partial-config Warn strings, serve.go:1719-1721 /
    1833-1834), which must stay unprefixed. But that same unprefixed name is ALSO
    compared against the SERVER resolver's resolved set at several sites, and a
    comparison against a `SERVER_`-prefixed resolved set MUST use the prefixed
    name or it never matches. So the seam is an INVARIANT, not a hand-listed set:
    every argument matched against a resolved `secrets.ResolvedSecret.Name` is
    `serverSecretName()`-wrapped, and every string that reaches a log, an error,
    or operator config is not. The wrapped comparison sites are
    `validateForgeSecret` (serve.go:1013/1016), `newDeclaredSecretResolver` /
    `newCachedWebhookSecret` (:1025/1039/1085/1638/1644 plus
    `buildLinearTokenSource`'s id+secret pair), `secretDeclared` inside
    `forgeWriteAppsConfigured` (serve.go:272-273 — the write-Apps gate: it scans
    the resolved set by `s.Name == name`, serve.go:280-290, so an unwrapped name
    would leave `havePrimary`/`haveReviewer` false forever and silently disable
    forge writes fleet-wide), and `forgeSecretDeclared` (serve.go:1078 — the
    Linear webhook mount gate, another `s.Name == name` scan, serve.go:1746-1757;
    unwrapped it unmounts the webhook even after the value is provisioned). The
    same helper applies the prefix at the `SetServerSecret` provisioning site, so
    the declared and resolved names agree by construction.
    `buildLinearWebhookWiring` is called from INSIDE `buildDoors` (serve.go:788),
    whose single `resolver` parameter (:677) ALSO feeds `buildNetworkServer`
    (:803) → `runnerhub.NewMountedHandler` (network_door.go:313) — the container
    FetchSecrets path that MUST keep reading `secrets`; so `buildDoors` takes
    BOTH resolver instances (or receives a pre-built `linearWebhookHandler`
    from Serve), because swapping its one `resolver` argument would repoint the
    container manifest at `server_secrets` and deliver every server secret into
    every agent container, inverting D6.
  - RPC: `SetServerSecret`/`DeleteServerSecret` on the secrets service —
    mirrors the `SetSecret` declare-then-Set flow and its rollback
    discipline (secrets_service.go:92-145) minus delivery/kind, targeting
    `server_secrets`; admin-gated (`adminOnly` in `classifyProcedure`,
    admin_gate.go:47). Carries the F1 PREFIX guard: it REQUIRES the declared
    name to carry a reserved server-secret prefix (`SERVER_` for the six forge
    secrets, `GATEWAY_CREDENTIALS_` for the master-key family), rejecting any
    unprefixed name with an actionable error. This is the admin half of the
    structural partition (D6): because the user path (below) rejects those same
    prefixes, a reserved-prefix name can only ever live in `server_secrets` and
    an unprefixed name can only ever live in `secrets` — the two keyspaces are
    disjoint by name, so no name DECLARED THROUGH EITHER GUARDED DOOR is ever
    live in both tables in either order (against the wiped pre-production
    baseline, D6), with no cross-table read. The reserved master-key name
    `GATEWAY_CREDENTIALS_MASTER_KEY` is additionally rejected on
    `SetServerSecret`/`DeleteServerSecret` (rotation is OQ-1 machinery, never a
    raw overwrite), so an admin cannot clobber the auto-provisioned key.
  - User-path prefix guard (F1 — mandatory, NOT admin-RPC-only, and
    NOT dependent on the shared-keyspace default): C1 shares the provider
    keyspace by DEFAULT (§D2 read-back verify, F2 WIRING SEAM), under which the
    `authenticatedOpen` user `secretsService.SetSecret`/`DeleteSecret` path
    (any authenticated account, admin_gate.go:122-125) can overwrite a
    server secret's provider value AND — absent the guard — mint a shadow
    `secrets` row under a server-secret name, which the inject-all path then
    delivers into every container. So the user path is the other half of the
    partition: `SetSecret`/`DeleteSecret` (and `DeclareSecret` at the store
    door, so the shadow row can never be created at all) MUST REJECT any name
    carrying a reserved server-secret prefix (`SERVER_` or
    `GATEWAY_CREDENTIALS_`) — checked BEFORE `resolver.Set`/`Delete`
    (secrets_service.go:124/208). A pure string check, not a membership SELECT:
    it needs no read of `server_secrets` and no cross-tenant visibility. A T0
    deliverable on the existing user path, not only the new
    admin RPC.
  - Value custody + operator provisioning (Matt-ruled, F2): the VALUES of all
    server secrets — the six forge secrets and the `GATEWAY_CREDENTIALS_`
    master key — live in an operator-chosen WRITABLE SecretSpec provider, not
    in the DB. The self-hosted default is `age://` (an age-encrypted file with
    a local age identity key: writable, so the master key can mint into it;
    encrypted at rest, which this whole record is about; headless- and
    container-safe with no D-Bus dependency, unlike `keyring`). A deployment
    with a cloud secret store points the SERVER resolver at it instead
    (`awssm` (any pin) or `awsps` (0.18+) on AWS, `akv` (any pin) or `aac`
    (0.20+) on Azure), and one already running
    Vault/OpenBao uses that — the provider is a per-resolver-INSTANCE config
    choice (`WithProvider`, resolver.go:82/84), not a hardcode, so this is a
    recommended default rather than a fixed backend. Two things make this an
    EXECUTABLE T0 deliverable rather than prose: (1) DEPENDENCY PREREQUISITE —
    `age` is a secretspec 0.17+ provider behind an `age` build feature, but
    the repo pins `github.com/cachix/secretspec/secretspec-go` at v0.15.0
    (go/go.mod:22), where `age://` does not resolve and T2's master-key
    write-back has no writable target. This gates the self-hosted `age://`
    DEFAULT specifically, NOT T0/T2 wholesale: on the current pin the matrix
    already marks Write yes for `keyring`, `dotenv`, `pass`, `gopass`
    (0.15+), `awssm`, `akv` and `vault`, so a deployment pointing the
    SERVER resolver at a cloud store or HashiCorp Vault needs NO bump (OpenBao,
    like `age`, is 0.17+); what
    v0.15 lacks is `age` (0.17+), with `pass`/`gopass` the self-hosted
    encrypted-at-rest alternatives available today (`age://` is the ruled
    default — a single local identity file rather than a GPG keyring). The
    bump covers TWO separate closures, since the SDK read path and the CLI
    write path are distinct binaries: (a) the Go module `secretspec-go` `>=
    0.17` for the READ path (`b.Load()`, resolver.go:170); AND (b) the
    `secretspec` CLI BINARY the WRITE path shells via `resolver.Set`
    (`exec.CommandContext(ctx, r.cli, args...)`, resolver.go:270; `r.cli`
    defaults to bare `"secretspec"` on PATH, resolver.go:29/100) — also `>=
    0.17` built with the `age` feature and pinned explicitly via `WithCLI`
    (resolver.go:90) into the Server's closure so the read and write halves
    cannot drift to different provider capability sets (a separate
    prerequisite PR, Matt-ruled). (2) WIRING SEAM —
    today serve.go:528 constructs the single resolver with NO provider option
    (the SDK default chain); T0 threads the operator-configured URI, from a NEW
    server flag/env (defaulting to the `age://` path), through
    `secrets.WithProvider(<URI>)` (resolver.go:84). By DEFAULT the SAME URI
    configures BOTH resolver instances — the SERVER (server-secret) resolver
    AND the container/user resolver at serve.go:528 — so the provider
    keyspace is shared by construction and the F1 guard + D2 read-back below are
    the primary controls on it. An operator MAY OPTIONALLY point the
    container/user (Layer B) resolver at a DIFFERENT provider (a second
    flag/env); doing so isolates the two keyspaces, under which F1 and D2 remain
    as defense-in-depth rather than becoming redundant (F1 is a
    declaration-layer NAME partition independent of provider; D2's read-back
    defends the master key regardless of who else can write the keyspace). The
    shared default is the ruled baseline; the split is the operator's opt-out.
    The master key is server-minted on first boot and written back to that
    provider (T2, unchanged). The six forge secrets are operator-SUPPLIED
    values: the operator populates them in the provider — for the `age://`
    default, deploy tooling seeds the age file; `compass server-secret set`
    (the T0 CLI below) writes a value through `resolver.Set` for rotation on a
    running server. The prefix RENAMES them (`LINEAR_FORGE_CLIENT_SECRET` →
    `SERVER_LINEAR_FORGE_CLIENT_SECRET`) and the provider keyspace is keyed by
    NAME (`setArgs`, resolver.go:315-321), so a value is populated under the
    prefixed name via the `serverSecretName()` seam (CONSUMER-REPOINT above).
    The NAMES are declared into `server_secrets` at boot from the RESOLVED
    config `cfg.Forge.resolved()` (serve.go:232-246) — the same accessor every
    live forge consumer uses — gated on the SAME enablement predicates that
    gate the forge lanes (`boardIngestionEnabled`, serve.go:224-226, plus the
    Linear-configured gate), so a deployment running neither App nor Linear
    declares and requires nothing. This boot declare runs on EVERY boot, so it
    is idempotent by construction: it tolerates `ErrConflict` from the
    non-idempotent `DeclareServerSecret` (secrets.go:100-102) as the
    already-declared no-op — the same tolerated-conflict arm `SetSecret` uses
    (secrets_service.go:116-117) — so every boot after the first re-declares
    the six names cleanly rather than surfacing a duplicate-name error as a
    startup failure. Because the operator populates the provider DIRECTLY (an
    age file or a cloud secret store — the provider MUST be writable, since
    the master key is server-minted and written back through `resolver.Set`;
    `env` is read-only in secretspec and so is NOT a valid SERVER-resolver
    provider even though it would suffice for the six operator-supplied forge
    values) BEFORE the server runs, the six values are present at first boot
    with NO running server required to bootstrap them — the R16
    chicken-and-egg (a running server needed to reach the provisioning RPC) is
    dissolved. `validateForgeSecret` (serve.go:1013/1016) keeps its hard-fail,
    but it is now a clean STATIC deploy-time error: a configured App whose
    forge value is absent from the provider fails startup with an actionable
    "set `SERVER_<NAME>` in the provider" message, fixed by populating the
    provider and rebooting — never by reaching a not-yet-running RPC. The
    complete set is SIX names: the PRIMARY App PEM (`appKeySecret`,
    main.go:414-417), the webhook secret (`appWebhook`, main.go:418-421), the
    REVIEWER App PEM (`reviewerAppKeySecret`, main.go:430; consumed
    serve.go:1644), and the three Linear secrets (client id, client secret,
    webhook — per `cfg.Forge.resolved()`, provenance main.go:435-450).
    Provisioning is idempotent at the RPC layer: `SetServerSecret` tolerates a
    re-provision of an already-declared name — it rewrites the provider value,
    mirroring `SetSecret`'s ErrConflict-tolerant arm
    (secrets_service.go:116-117), rather than erroring — while store
    `DeclareServerSecret` itself mirrors the non-idempotent `DeclareSecret`
    (ErrConflict on a duplicate name, secrets.go:100-102). Closes the
    pre-existing inject-all exposure (D6) for every configured name once
    provisioned. Same PR chain: OQ-4, RESOLVED.
- Consumes: nothing from T1-T5 (pure prerequisite).
- Tests: a secret declared in `server_secrets` is ABSENT from a FetchSecrets
  response (because it lives in the other table — no filter involved) while
  `secrets` rows in the same boot ARE present; the server resolver instance
  RESOLVES it; the container resolver's generated manifest never contains its
  name; the new RPC rejects a non-admin caller; the reserved master-key name
  is rejected on SetServerSecret/DeleteServerSecret AND on the user-path
  SetSecret/DeleteSecret (a non-admin authenticated user calling SetSecret
  with a `GATEWAY_CREDENTIALS_`-prefixed name is rejected and the provider
  value is unchanged — F1); the server resolver can READ `server_secrets`
  through the normal compass_app store path (the GRANT is present — F3); all
  six configured names (primary + reviewer App PEM, webhook, and the three
  Linear secrets) resolve through the server resolver under their
  `SERVER_`-prefixed names and are gone from FetchSecrets; a ROTATION of any
  of the six through the `SetServerSecret` RPC rewrites its provider value and
  is picked up on the next resolve (the RPC is the rotation path, not the
  first-boot bootstrap path); and provisioning is IDEMPOTENT at the RPC layer
  — a re-provision of the same name through `SetServerSecret` succeeds and
  rewrites the provider value (mirroring `SetSecret`'s ErrConflict-tolerant
  arm, secrets_service.go:116-117), with no duplicate-row error surfaced to
  the caller; store `DeclareServerSecret` itself returns ErrConflict on a
  duplicate name exactly as `DeclareSecret` does (secrets.go:100-102). A
  server whose App is UNCONFIGURED (no `--forge-app-id`) and whose Linear is
  unset declares and requires none of the six and boots clean. A SECOND boot
  of a configured deployment (the six names already present in
  `server_secrets`) declares idempotently and boots clean — red if the boot
  declare surfaces `ErrConflict` from `DeclareServerSecret` as a startup error
  rather than tolerating it as the already-declared no-op. A server with the
  App CONFIGURED and the six present in the provider boots with the forge
  lanes live; with the App configured but a forge value ABSENT from the
  provider, boot fails with an actionable static "set `SERVER_<NAME>` in the
  provider" error (`validateForgeSecret`, serve.go:1013/1016) — fixed by
  populating the provider and rebooting, not by reaching a running-server RPC.
  The master-key write-back and the `compass server-secret set` rotation path
  both resolve their provider (`age://` on the self-hosted default) through
  the STAGED CLI binary, not only the SDK: red if the write path shells a
  `secretspec` older than 0.17 (or one built without the `age` feature),
  which surfaces as an unknown-provider error from `resolver.Set` rather than
  a successful encrypted write — the assertion that closes the two-closure
  gap the version prerequisite exists to cover.
  A deployment with the Linear pair configured under the DEFAULT names
  (`defaultForgeLinearClientIDSecretName` /
  `defaultForgeLinearClientSecretName`) and NO flag/env set is provisioned and
  resolved under their `SERVER_`-prefixed names via the `serverSecretName()`
  seam and is absent from FetchSecrets (red if the resolve side reads the raw
  flag/env layer instead of the prefixed `resolved()` name). CONSUMER-REPOINT
  positive assertions (the (b)/(c) silent modes need them, since an absent
  name is indistinguishable from a legitimate off-state): boot a server with
  all six names provisioned and assert the GitHub App token source mints, the
  GitHub and Linear webhook handlers are MOUNTED (non-nil), the Linear token
  source is non-nil, AND the forge WRITE caller is MOUNTED
  (`hub.SetForgeCaller` called / `RelayForgeCall` does not fail-close to
  `CodeUnavailable`) — i.e. the forge read AND write lanes still wire off the
  server resolver, not just that the names are in `server_secrets`. F1 PREFIX
  PARTITION assertions: (user path) a non-admin authenticated caller invoking
  `SetSecret`/`DeclareSecret` with a `SERVER_`- or
  `GATEWAY_CREDENTIALS_`-prefixed name is REJECTED before `resolver.Set`, no
  `secrets` row is created, and the provider value is unchanged; (admin path)
  an admin invoking `SetServerSecret` with an UNPREFIXED name is REJECTED
  before the `server_secrets` insert and before `resolver.Set`, no
  `server_secrets` row is created. Together these assert the two keyspaces are
  disjoint by name, so no name can ever be live in both tables — the
  structural F1 property, checked with pure string tests, no cross-table or
  cross-tenant read.

### T1 — Envelope-crypto helper package

New package `go/internal/secrets/envelope` (child of the secrets seam it
serves; no import cycle — it depends on nothing in `secrets`).

- `Interfaces:`
  - `type Key struct{ /* unexported [32]byte */ }` — redacts under
    `%s`/`%v`/`%#v` (`String()`/`GoString()` → `"envelope.Key{<redacted>}"`).
  - `func NewKey() (Key, error)` — 32 bytes from `crypto/rand`.
  - `func KeyFromBytes(b []byte) (Key, error)` — errors unless len == 32.
  - `func (k Key) Seal(plaintext, aad []byte) (ciphertext, nonce []byte, err error)`
    — AES-256-GCM, fresh 96-bit random nonce per call; `aad` is
    authenticated, not encrypted (binds the ciphertext to its row identity —
    T4).
  - `func (k Key) Open(ciphertext, nonce, aad []byte) ([]byte, error)` — GCM
    auth failure (tamper OR aad mismatch) returns an error naming no
    plaintext/key material.
  - Key encoding for provider storage: base64(std) of the 32 raw bytes
    (SecretSpec values are strings; `Set` rejects empty, resolver.go:239-241).
- Consumes: `crypto/aes`, `crypto/cipher`, `crypto/rand` only.
- Tests: round-trip; tamper (flip a ciphertext/nonce byte → error); `Open`
  under a different `aad` → error; nonce uniqueness across calls; redaction
  of `Key` under all three verbs; `KeyFromBytes` length validation.

### T2 — Master-key boot-provision seam

Boot-time resolve-or-provision, in the server wiring next to the existing
declared-secret consumers (`go/server/serve.go`). DEPENDS ON T0 (the
`server_secrets` store and its resolver instance must exist before the key
is declared into it) and T1.

- `Interfaces:`
  - `func provisionGatewayMasterKey(ctx context.Context, resolver secrets.Resolver, st *store.Store) (envelope.Key, error)`
    — `resolver` is the SERVER-SECRET resolver instance (T0). Resolve
    `GATEWAY_CREDENTIALS_MASTER_KEY` through it; on absence:
    `envelope.NewKey()` → `resolver.Set(ctx, name, encodedKey, "compass:
    provision gateway credentials master key")`
    (resolver.go:234; the value rides stdin, never argv,
    resolver.go:211-212) → `st.DeclareServerSecret(ctx, "", name)` with
    `declared_by = NULL` (server-provisioned; T0's nullable FK). No
    delivery, no kind — those columns do not exist on `server_secrets`.
  - **Concurrency — advisory-lock serialized (mandatory):** the whole
    resolve→generate→Set→Declare sequence runs under a Postgres advisory
    lock (`pg_advisory_xact_lock` on a constant key) — Postgres is the one
    store all instances share. This replaces the draft's Set-then-Declare
    with tolerated ErrConflict, which was a check-then-set race: two
    concurrently booting instances both resolve-absent and both Set (last
    writer wins in the provider); the loser's Declare hits ErrConflict, is
    tolerated, and that instance proceeds to encrypt with a key the
    provider no longer holds — silently undecryptable rows, discovered at
    read time. The write path this builds on explicitly disclaims
    concurrent safety: "The declare/set/rollback trio is not atomic and
    assumes no concurrent same-name writer (the single-Runner MVP:
    SetSecret is user-driven CLI)" (go/server/secrets_service.go:88-91).
    Ordering inside the lock stays Set-before-Declare: the inverse leaves a
    crash-window orphan declaration, and "an orphaned declaration is
    required=true in the resolve manifest and would poison EVERY live
    session's FetchSecrets" (secrets_service.go:86-88) — under C1 the
    blast radius shifts but stays severe: an orphaned `server_secrets`
    declaration is required=true in the SERVER resolver's manifest and
    would fail every server-side resolve (master key, PEM, webhook,
    Linear). A crash between Set
    and Declare converges on the next boot (the undeclared name does not
    resolve, so the provisioner re-generates, re-Sets, and Declares — no
    row was ever encrypted under the orphaned value).
  - **Bounded critical section (mandatory — a stuck provider must not wedge
    the fleet):** the provider round-trips inside the lock inherit only the
    caller's ctx, which at boot is long-lived — but the two halves bound
    DIFFERENTLY. `SpecResolver.Set` IS ctx-bounded: it shells out via
    `exec.CommandContext(ctx, r.cli, …)` (resolver.go:270), so a ctx deadline
    genuinely kills it. `SpecResolver.Resolve` is NOT: it threads ctx only into
    `DeclaredSecrets` (resolver.go:145); the actual provider round-trip is
    `b.Load()` (resolver.go:170), whose SDK signature carries NO ctx
    (`func (b *Builder) Load() (*Resolved, error)`, verified against
    secretspec-go v0.20.0 secretspec.go:293, the current pin — `Load` still
    carries no ctx after the bump, so the goroutine-offload design below
    stands) and which blocks in an uncancellable
    FFI call
    (`nativeResolve` → `C.secretspec_resolve`, binding_cgo.go:30 /
    binding_purego.go:118). A hung provider (1Password awaiting biometric
    approval, an unreachable Vault, a half-open TCP) would otherwise hold the
    transaction-scoped lock indefinitely, and because the key is a shared
    constant EVERY other booting instance blocks on it — one stuck provider
    becomes a fleet-wide boot wedge. So the provisioner (1) derives a ctx with
    an explicit timeout (mirror the 30s `&http.Client{Timeout: 30 *
    time.Second}` precedent that already bounds the Linear boot mint at
    serve.go:1731) and, because the Resolve-side call cannot be cancelled, RUNS
    THE ctx-LESS `Resolve` ON ITS OWN GOROUTINE AND SELECTS ON THAT CTX (`Set` is
    ctx-bounded and stays on the parent, inside the lock) — so the
    provisioner returns a diagnosable bounded startup error (naming the hung
    provider) and its transaction is rolled back, RELEASING the xact-scoped
    advisory lock, while the orphaned FFI goroutine is knowingly leaked for the
    process's remaining boot-failing lifetime (acceptable: the boot is aborting
    anyway). The PARENT goroutine owns the transaction (`pgx.Tx` is not
    concurrency-safe): it takes the advisory lock, offloads ONLY the ctx-less
    provider READ (`Resolve`) and NEVER a provider write, and on the timeout
    branch performs the `Rollback` itself and discards the buffered result
    without acting on it. A `Set` reached after the parent's Rollback would land
    OUTSIDE the released advisory lock and could overwrite a key another booter
    has already provisioned and begun sealing rows under — reintroducing the
    silently-undecryptable-rows failure the lock exists to prevent — so the
    offloaded goroutine performs no `Set`; the parent runs the (ctx-bounded)
    `Set` itself, in-lock, only on the success branch. The offloaded call
    reports through a BUFFERED (cap-1) channel so the orphaned FFI goroutine can
    complete its send and exit rather than blocking forever on an abandoned
    receiver (bounding the leak on a crash-looping boot). And (2)
    acquires the lock with `pg_try_advisory_xact_lock` in a
    bounded retry loop (or sets a session `lock_timeout`) so a booter that
    cannot get the lock fails closed with a diagnosable startup error naming
    the contended provisioning lock rather than parking forever. Test (T2): a
    provider that never returns yields a bounded, diagnosable boot failure
    whose transaction (and advisory lock) is released even though the provider
    call itself cannot be cancelled, and a second instance blocked on the lock
    also fails bounded rather than hanging.
  - **Read-back verify, every boot:** after provisioning AND on every
    subsequent boot, re-resolve the name and byte-compare against the key
    the process is about to encrypt with; on mismatch, refuse to serve
    gateway-credential writes (fail closed). This re-resolve is the SAME
    uncancellable `Load` (resolver.go:170) and uses the SAME bounded-offload
    path as the provisioning resolve (timeout ctx + own goroutine + buffered
    cap-1 channel), so on a steady-state boot — key already provisioned,
    nothing to serialize — a hung provider still yields a bounded, diagnosable
    startup error rather than a parked process. This is necessary because the
    provider keyspace is a shared mutable surface under the DEFAULT single-URI
    wiring (F2): C1 pins BOTH resolver instances to the same SecretSpec
    project + profile (`manifestProject = "compass"`, `defaultProfile =
    "default"`,
    resolver.go:19/23) and, by default, the same provider URI, so the master
    key's provider VALUE is reachable by any writer of that keyspace — an
    operator's out-of-band `secretspec set`, AND (absent the guard below) the
    user-driven `SetSecret` RPC. The verify is retained even when an operator
    opts Layer B onto a separate provider (where the user path can no longer
    reach the master key's keyspace): it still defends the master key against an
    operator's out-of-band overwrite and against a swapped provider file, so it
    is a control on the master key's integrity, not solely a consequence of
    keyspace sharing. The verify is a tripwire,
    not a boundary: on a plain boot the resolved value IS "the key the
    process is about to encrypt with", so a byte-compare cannot by itself
    distinguish "my key" from "a swapped key". T5 strengthens it by binding
    a non-secret key fingerprint (a salted digest, persisted in the T0
    `server_key_state` row — `key_fingerprint`/`fingerprint_salt`, not a
    declared-name registry so the names-only invariant is untouched) so a
    swapped provider value is DETECTED at boot rather than silently adopted.
  - **Master-key overwrite guard (F1 prefix partition — on EVERY
    provider-writing path):** C1 splits the DECLARATION registries (two tables)
    but NOT, by DEFAULT, the provider keyspace (one shared profile AND — absent an
    operator Layer-B split, F2 — one shared provider URI, above). So the isolation
    C1 buys is that server secrets are structurally undeliverable to CONTAINERS
    (the manifest separation, §Mechanism C1) — it does NOT by itself make the
    master key's provider value unreachable from the user path. The user
    `SetSecret`/`DeleteSecret` RPC (`authenticatedOpen`, any authenticated
    account — admin_gate.go:122-125) declares into `secrets` but then calls
    `resolver.Set`, which shells `secretspec set <NAME> --profile default`
    (resolver.go:315-321) against the keyspace that is SHARED under the default
    single-URI wiring (F2) — so absent a guard a user
    calling `SetSecret` with name `GATEWAY_CREDENTIALS_MASTER_KEY` would
    OVERWRITE the master key's provider value (the running process keeps its
    cached key, but the next boot adopts the attacker-chosen key → every existing
    row fails GCM auth, every new row is sealed under a known key: the exact
    bulk-disclosure D1 prevents) — and the guard is retained under an operator
    Layer-B split too, where the user path can no longer reach the master key's
    keyspace: F1 is a declaration-layer NAME partition independent of provider,
    so it does not become redundant. The F1 prefix guard (D6) closes this: the
    master-key name carries the reserved `GATEWAY_CREDENTIALS_` prefix, which the
    user `secretsService.SetSecret`/`DeleteSecret` path REJECTS BEFORE
    `resolver.Set`/`Delete` — a T0/T2 deliverable, the SAME string check that
    enforces the keyspace partition, so no separate membership read is needed.
    Tested red-green (a non-admin user calling `SetSecret` with a
    `GATEWAY_CREDENTIALS_`-prefixed name is rejected and the provider value is
    unchanged). Rotation is OQ-1's machinery, never a raw overwrite through
    either surface. The name-keyed global user delete ("a row is keyed by name
    alone, not (actor, name)", go/internal/store/secrets.go:150-153) is why the
    guard covers the delete path too, once a real provider hard-delete lands.
  - **Nil-resolver deployment:** a server built with no secrets surface is
    legitimate today ("resolver may be nil on a server built with no
    secrets surface (FetchSecrets then fails CodeFailedPrecondition rather
    than panicking)", go/internal/runnerhub/handler.go:52-54; agents still
    start, go/internal/runner/host.go:367-370). On such a server the
    gateway credential store is NOT constructed; enabling the gateway is a
    configuration error naming the missing secrets surface; a no-gateway
    boot proceeds unchanged.
  - Resolve path mirrors `newDeclaredSecretResolver` (serve.go:1469) but is
    pointed at the SERVER-SECRET resolver instance and invoked once at
    boot; the decoded `envelope.Key` is held in memory for the process
    lifetime.
  - Boot fails closed: a resolve/provision fault is a startup error, never a
    fall-back-to-plaintext.
- Consumes: T0's `server_secrets` store + server resolver instance, T1
  `envelope`, `secrets.Resolver`, `store.DeclareServerSecret`.
- Produces: the process-lifetime `envelope.Key` handed to T4.
- Tests: fresh-boot provisions (Set + Declare called, key usable);
  second-boot resolves without Set; two-writer interleaving (concurrent
  provisioners converge on ONE key both read back identically);
  Set-succeeded/Declare-crashed reboot converges; read-back mismatch →
  fail closed; reserved-name SetServerSecret/DeleteServerSecret →
  actionable reject;
  nil-resolver + gateway enabled → configuration error naming the missing
  surface; nil-resolver without gateway → boot proceeds unchanged;
  provider fault → boot error.

### T3 — Schema: ciphertext columns on gateway_credentials

The `gateway_credentials` migration (owned by the store build the frozen
record plans) carries, for the value payload:

- `Interfaces:` columns `value_ciphertext BYTEA NOT NULL`,
  `value_nonce BYTEA NOT NULL`, `key_version SMALLINT NOT NULL DEFAULT 1`.
  NO plaintext value column exists in any migration version — the store is
  born encrypted (per the rejected plaintext-first alternative).
- The row's non-secret metadata (provider, scope, owner_user_id, `version`
  CAS counter, expiry timestamp if queried-on) stays plaintext-queryable;
  ONLY the credential value payload is inside the envelope. Expiry inside
  vs. beside the ciphertext is settled by the store build; default: beside
  (the gateway lists refreshable rows by expiry without decrypting).
- **Dependency edge, explicit:** this migration task DEPENDS ON T1+T2
  merged (same PR chain or a stated blocking dependency in the store
  build's tracker) — the encrypt-before-any-row ordering must be
  structural, not a prose sentence. `gateway_credentials` exists nowhere
  under `go/` today (grep this session, zero matches), so the ordering is
  currently satisfiable with no existing-rows hazard. T5's
  ciphertext-at-rest assertion is the named CI tripwire that reds if a
  plaintext value column ever appears.
- Tests: migration applies; NOT NULL enforced.

### T4 — Wire crypto into the CredentialStore read/write path

Every write path (initial credential save, gateway OAuth-refresh write-back
per design.md:367-371) seals before INSERT/UPDATE; every read path opens
after SELECT. This sits server-side under the RPC surface the frozen record
recommends (design.md:348-358), so the TS gateway never sees the key or the
crypto — it receives plaintext credentials over the stack-token-authenticated
RPC exactly as the frozen record already specifies.

- `Interfaces:` the store's credential accessors take/return the decrypted
  payload type (per D5: secret fields unexported with accessors, or
  `slog.LogValuer` + redacting `MarshalJSON` alongside
  `String`/`GoString`); the gateway-bound RPC response is built by an
  explicit proto/DTO conversion, never by marshaling the payload type. The
  `envelope.Key` is a construction-time dependency of the store/service
  wrapper, not a per-call parameter. **AAD binds row identity:** every
  Seal/Open passes `aad = row primary key + key_version` — the STABLE row
  identity, never the CAS `version` counter (which increments per write).
  Without AAD an attacker with DB write access can swap two rows'
  ciphertext+nonce pairs and both still authenticate — a cross-tenant
  credential substitution crossing exactly the boundary the frozen record
  names ("the isolation boundary is the per-tenant pool scoping enforced
  server-side", design.md:333-337). Writes stamp the current `key_version`;
  reads select the key by the row's `key_version` (v1: single live key — a
  mismatched version is a diagnosable error naming the expected/found
  version numbers, never key material; see OQ-1).
- Consumes: T1, T2's key, T3's columns.
- Tests: see T5.

### T5 — Tests: ciphertext-at-rest + redaction assertions

- `Interfaces:` (test-only)
  - **Ciphertext-at-rest assertion (the named CI tripwire, T3):** write a
    credential through the store, then read the raw row via SQL — assert
    the known plaintext substring (e.g. the api key literal) appears
    NOWHERE in any column; assert `value_ciphertext != plaintext` and
    decrypt-with-key round-trips. This is the guard that reds if a
    plaintext value column ever appears.
  - **Row-swap assertion (AAD):** swap two rows' ciphertext+nonce pairs at
    the SQL level — `Open` MUST fail for both rows (the AAD binds row
    identity).
  - **Redaction assertion (beyond fmt verbs):** format the
    decrypted-payload type and `envelope.Key` under `%s`, `%v`, `%+v`,
    `%#v`; `json.Marshal` the payload type; log it through a
    `slog.JSONHandler` capture — assert no secret bytes appear in ANY of
    these. The fmt verbs alone mirror the existing pattern's intent
    (secrets.go:155-156) but do not cover the reflection paths (D5).
  - **Key-swap tripwire (F1):** persist a non-secret key fingerprint (a
    salted digest of the master key, written to the T0 `server_key_state` row —
    `key_fingerprint` + `fingerprint_salt`, alongside `key_version`) at provision
    time; the boot read-back verify recomputes it and refuses to serve on
    mismatch. Test: swap the
    provider value out-of-band, reboot → boot fails closed (the fingerprint
    distinguishes "a key" from "my key", which a bare byte-compare cannot).
  - Refresh write-back path: refreshed OAuth tokens land re-sealed with a
    fresh nonce (nonce differs from the previous row state).
  - Wrong-key / tampered-row read → error, no partial plaintext; a
    key_version-mismatch error is distinguishable from a GCM auth failure
    (names the versions, never key material).

## Tasks

- [ ] T0 — `server_secrets` store (mechanism C1): new migration (table +
      single-row `server_key_state` table for the key-swap tripwire digest +
      GRANT SELECT/INSERT/UPDATE/DELETE on `server_secrets` and
      SELECT/INSERT/UPDATE on `server_key_state` to compass_app/compass_system
      + bucketA allow-list edit in rls_pgtest_test.go),
      `ServerDeclaredSecrets` store view + second SpecResolver instance (same
      profile), admin-gated SetServerSecret/DeleteServerSecret RPC carrying
      the F1 PREFIX guard (REQUIRE a reserved server-secret prefix — `SERVER_`
      or `GATEWAY_CREDENTIALS_` — checked before the `server_secrets` insert
      and before `resolver.Set`, AND at the `store.DeclareServerSecret` store
      door, so no writer can create a `server_secrets` row under an unprefixed
      name) AND the matching user-path half on
      `secretsService.SetSecret`/`DeleteSecret` and `store.DeclareSecret`
      (REJECT any reserved-prefixed name before `resolver.Set`/`Delete`) —
      together the STRUCTURAL F1 PREFIX PARTITION, the two doors making the
      keyspaces disjoint by name so no name is ever live in both tables in
      either order, a pure string check with no cross-table read; PLUS the
      reserved `GATEWAY_CREDENTIALS_MASTER_KEY` name rejected on the admin
      path too (rotation is OQ-1 machinery, not a raw overwrite), and operator
      provisioning of the SIX server-secret values (primary + reviewer App
      PEM, webhook, three Linear): their VALUES live in an operator-chosen
      WRITABLE SecretSpec provider (self-hosted default `age://` — writable,
      encrypted-at-rest, headless; a cloud store or Vault/OpenBao where
      present — F2) resolved by a SERVER resolver constructed
      `secrets.WithProvider(<operator-configured URI>)` (resolver.go:84) off a
      NEW server flag/env for that URI (serve.go:528's container resolver
      unchanged), populated by the operator directly (deploy tooling seeds the
      age file) or, for rotation on a running server, through the NEW `compass
      server-secret set/list/delete` CLI (go/cmd/compass/, mirroring
      `newSecretCmd` secret.go:42-49 — value read from stdin never argv per
      secret.go:39-41 — dialing the admin
      `SetServerSecret`/`DeleteServerSecret` RPCs over `dialSecretsClient`),
      which is idempotent at the RPC layer (a re-provision rewrites the
      provider value, tolerating ErrConflict per secrets_service.go:116-117;
      store `DeclareServerSecret` itself is non-idempotent, mirroring
      `DeclareSecret`); the NAMES are declared into `server_secrets` at boot
      from the resolved forge config on the ordinary compass_app store path
      (`server_secrets` is bucket-A, RLS NOT enabled, so NO BYPASSRLS and NO
      cross-tenant read is needed) — a configured App whose forge value is
      absent from the provider fails startup with an actionable static "set
      `SERVER_<NAME>` in the provider" error (`validateForgeSecret`,
      serve.go:1013/1016), fixed by populating the provider and rebooting,
      never by reaching a running-server RPC; an unconfigured App/Linear
      declares and requires nothing and boots clean — plus the
      `serverSecretName()` prefix seam applied as an INVARIANT (every argument
      compared against a resolved `s.Name` is prefix-wrapped — including
      `secretDeclared` in `forgeWriteAppsConfigured` serve.go:272-273 (BOTH
      the primary and reviewer arms) and `forgeSecretDeclared` serve.go:1078 —
      while every log/error/operator-config string is not) so declared and
      resolved names agree — PLUS the doc-comment repairs T0's own changes
      require: the five "three/3 procedures authenticatedOpen" SecretsService
      comments (admin_gate.go:116-121, network_door.go:288-290,
      secrets_service.go:6, serve.go:555, serve.go:749-750) are restated as
      three authenticatedOpen + two adminOnly once the two admin methods land;
      the `adminOnly` type doc (admin_gate.go:22-26) widens its "privileged
      CompassService agent-session RPCs (and token issuance)" enumeration to
      admit the two `SecretsService` server-secret writes; and the
      `SecretsService` service comment
      (proto/compass/v1/compass.proto:175-179) is restated to cover the two
      admin-gated server-secret methods and to note their authz is door-side
      (`adminOnly` in `classifyProcedure`), not handler-side — this one
      propagates through `moon run compass-proto:gen` into two checked-in TS
      gen trees — `packages/compass-client/src/gen` (the PUBLIC lane,
      buf.gen.yaml) and `packages/compass-agent/src/gen` (the internal-only
      agent lane, buf.gen.agent-ts.yaml, proto/moon.yml:35-41) — which the
      drift gate does NOT catch as staleness on its own (it regenerates and
      diffs, so stale comment text regenerates identically and passes).
      (Opportunistic in the same edit, flagged pre-existing not T0-caused:
      admin_gate.go:40-41's "every generated CompassService and CommsService
      procedure" phrasing is already stale — the switch covers SecretsService
      too — as is the identical phrasing at classify_exhaustive_test.go:45-46;
      correct both in the same pass, but neither gates T0.) Red-green tests:
      absent-from-FetchSecrets + server-resolver-resolves +
      user-path-prefix-guard (a `SetSecret` on a reserved-prefixed name is
      rejected, no `secrets` row created) + admin-path-prefix-guard (a
      `SetServerSecret` on an UNPREFIXED name is rejected, no `server_secrets`
      row created) + provision-idempotent (a re-provision of the same name via
      `SetServerSecret` succeeds and rewrites the provider value, no
      duplicate-row error to the caller) + boots-from-provider (an
      unconfigured App/Linear boots clean requiring none of the six; a
      configured App with the six present in the provider boots with the lanes
      live; a configured App with a forge value absent from the provider fails
      startup with the actionable static error). DEPENDENCY PREREQUISITE
      (gating for the self-hosted `age://` default path only, Matt-ruled
      separate PR): bump `github.com/cachix/secretspec/secretspec-go` from the
      pinned v0.15.0 (go/go.mod:22) to `>= 0.17` with the `age` build
      feature, covering BOTH closures the two code paths shell separately —
      the Go module for the SDK READ path (`b.Load()`, resolver.go:170) AND
      the `secretspec` CLI BINARY the WRITE path runs (`resolver.Set` shells
      `r.cli`, resolver.go:270/29), the latter pinned via `WithCLI`
      (resolver.go:90) into the Server's closure so read and write cannot drift
      to different provider capabilities. `age` is a 0.17+ build-feature
      provider, so on the current pin the `age://` default does not resolve
      and T2's master-key write-back has no writable target; a deployment on a
      cloud store (`awssm`/`akv`) or HashiCorp Vault — all Write-capable at
      the current pin — needs no bump (OpenBao does NOT qualify: `openbao://` is a
      0.17+ provider, with 0.16 routing it through `vault`, so an OpenBao
      deployment rides the same bump as `age://`). GATING for T2 ONLY on the `age://`
      path. Proto
      delta: two additive `SecretsService` methods, no enum change — the
      checked-in public gen trees are drift-gated, so this needs the
      `compass.proto` edit + `moon run compass-proto:gen`, and both new
      procedure paths MUST be added to `classifyProcedure` as `adminOnly` or
      `classify_exhaustive_test` reds CI
      (`go/internal/auth/classify_exhaustive_test.go`).
- [ ] T1 — `go/internal/secrets/envelope`: Key/NewKey/KeyFromBytes/Seal/Open
      (AAD-carrying) + redaction + unit tests
- [ ] T2 — boot resolve-or-provision seam via the server-secret resolver
      (advisory-lock serialized, read-back verify + key-fingerprint tripwire,
      nil-resolver gating, fail-closed boot; the F1 prefix guards are T0's, on BOTH
      the admin SetServerSecret/DeleteServerSecret RPC AND the authenticatedOpen
      user SetSecret/DeleteSecret path).
      DEPENDS ON T0 + T1.
- [ ] T3 — `gateway_credentials` migration columns: `value_ciphertext`,
      `value_nonce`, `key_version`; no plaintext column ever. DEPENDS ON
      T1+T2 merged (the store build's migration blocks on them).
- [ ] T4 — seal/open wiring in the CredentialStore read/write + refresh
      write-back paths; AAD = row PK + key_version; key as
      construction-time dependency
- [ ] T5 — ciphertext-at-rest assertion (CI tripwire), row-swap AAD test,
      redaction incl. json.Marshal + slog JSONHandler, tamper/wrong-key
      tests, and the key-swap tripwire (persist a salted key fingerprint at
      provision; boot recompute + fail-closed on mismatch).

## Open Questions

- **OQ-1 (deferrable, recommendation: defer): master-key rotation +
  row re-encryption.** How does the master key rotate and how are existing
  rows re-encrypted? Recommendation: the `key_version` column lands at v1
  (D4 — load-bearing now because retrofitting it is a migration under
  ambiguity), but active rotation machinery — lazy re-encrypt on next write
  plus a one-shot re-encrypt sweep — is DEFERRABLE to a follow-up record; v1
  runs a single live key version and treats a version mismatch as an error.
  Why deferral is SAFE, not merely convenient: (1) provider key loss makes
  every row unrecoverable, but the blast radius is BOUNDED — gateway
  credentials are re-obtainable from their owners (re-enter the API key,
  re-run the OAuth grant; the store holds api_key and OAuth-shaped payloads
  a user configures, parent design.md:324-326), so the worst case is a
  fleet re-authentication event, never permanent data loss; (2) rotation is
  also the nonce-budget release valve, and D1's write-rate arithmetic shows
  years of headroom under the 2^32 bound. Carried into T4/T5 regardless of
  deferral: a wrong-key / key_version-mismatch decrypt error MUST be
  operator-distinguishable from row corruption — the error names the
  expected/found key_version numbers, never key material.
- **OQ-2 — RESOLVED (Matt; mechanism updated by the C1 ruling): master-key
  containment.** The master key is declared into the physically separate
  `server_secrets` store that D6/T0 build, boot-resolved through the
  server-secret resolver instance (mirroring the Forge App PEM's
  serve.go:1469 pattern), and never materialized into an agent container —
  its name is not in the container resolver's manifest at all. An earlier
  fold resolved this with a minted SERVER_ONLY delivery kind + FetchSecrets
  filter; Matt's C1 ruling replaced that with the separate store (see
  Alternatives, mechanism A) and REMOVED the public proto ENUM change it
  required (the C1 admin RPC is an additive service-method delta, not a wire
  enum change).
- **OQ-3 — RESOLVED (Matt: yes, D7): api_key rows are encrypted identically
  to OAuth rows.** One seal/open code path for both payload shapes; both are
  secrets of the same sensitivity (a stored api_key is as long-lived and as
  disclosure-critical as an OAuth refresh token), and a split path would
  leave the most static secret class in plaintext for zero benefit. T3's
  single-column schema and T4's single wiring path encode this — there is no
  plaintext branch.
- **OQ-4 — RESOLVED (Matt: same PR chain, mechanism C1): the
  PEM/webhook/Linear server-secret rows move to `server_secrets` in THIS
  record's PR chain**, closing the live pre-existing exposure (they ride
  the inject-all path into every agent container today, D6). Because the
  reserved-prefix partition (D6) renames the six under `SERVER_`, and the
  deployment is pre-production, the ALREADY-declared rows are WIPED and
  re-provisioned under their prefixed names rather than migrated in place: the
  shared profile is name-keyed, so the rename requires the operator to populate
  each value in the provider under the new prefixed name (a provider write, not
  a free row move). The NAMES are declared into `server_secrets` at boot from
  the resolved forge config, gated on the same predicates that enable the forge
  lanes; the VALUES live in an operator-chosen writable provider (self-hosted
  default `age://`, gated on the `>= 0.17` secretspec bump T0 requires — F2;
  a cloud store or Vault/OpenBao where present), populated
  by the operator directly (deploy tooling seeds the age file) or through the
  `compass server-secret` CLI / admin `SetServerSecret` RPC (T0) for rotation on
  a running server. Because the provider is populated BEFORE the server runs, the
  six values are present at first boot with no running server required to
  bootstrap them (Matt-ruled — the server must be able to boot with the secrets
  set through the provider/config, not depend on a running server to reach a
  provisioning RPC).
- **OQ-5 — WITHDRAWN (Matt: prefix partition, no reconcile): the DL-315
  BYPASSRLS-allow-list widening is no longer needed.** An earlier draft of this
  record moved the six server-secret rows with a boot reconcile that had to run
  under `store.WithSystemRole` (BYPASSRLS) to see cross-tenant `secrets` rows,
  which would have added a BYPASSRLS entrypoint beyond the set DL-315 governs.
  Matt's reserved-prefix partition (D6) removed the reconcile entirely — server
  secrets are re-declared under `SERVER_`-prefixed names on the ordinary
  compass_app store path (`server_secrets` is bucket-A, no RLS), so this record
  adds NO cross-tenant BYPASSRLS site and needs no DL-315 amendment. The encryption row landed as
  DL-328 (concurrent merges took the intervening numbers: RIG-3070's
  runner-trust-model split took DL-325, RIG-3170's runner-records scrub chain
  took DL-326 for the P2 session-volume clone model, and the sibling
  SubjectService PR takes DL-327). This record adds no BYPASSRLS site,
  so there is no separate DL-315-amendment row. The separate, PRE-EXISTING observation that DL-315's own
  entrypoint enumeration is stale (the shipped `hub.go:814` forge-notification-ack
  arm is a BYPASSRLS site DL-315's four names omit — `WithSystemRole`'s own doc
  comment already lists it, tenant_tx.go:41-47) is not this record's to fix, since
  this record no longer touches the BYPASSRLS surface; it is left as a pre-existing
  DL-315 ledger-prose observation for a separate follow-up, not tracked by this
  record.
