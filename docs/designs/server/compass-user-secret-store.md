# Compass user-secret store (Postgres, encrypted at rest)

Status: Draft

Tracking: RIG-3655 (research record: RIG-3637)

Sibling record: [`compass-gateway-credentials-at-rest-encryption`](./compass-gateway-credentials-at-rest-encryption.md)
(the frozen at-rest-encryption + master-key-custody addendum). This record
REUSES its crypto rulings (AES-256-GCM, master-key custody in the SecretSpec
provider, `key_version` from day one, the `server_key_state` tripwire) and
applies them to a NEW value store: user-provided secrets. It does not re-open
that record's decisions.

Matt's rulings (2026-09-11), taken as given — this record designs the HOW:

- **D1** User-provided secret writes land in Postgres; compass owns the store.
- **D2** Boot-time secrets stay on secretspec, read via CLI subprocess; the
  FFI/cdylib work is cancelled.
- **D3** The DB encryption key is itself a boot secret read from secretspec.
- **D4** Boot secrets become read-only — the server-secret write path is
  dropped.
- **D5** RIG-3601 (cdylib staging) is cancelled.
- **D6** PR #1066 (provider hard-delete) is held as draft; its delete semantics
  fold into this work.

## Problem / Intent

User-provided secrets today are names-only rows in Postgres
(`store.SecretDeclaration`, `go/internal/store/secrets.go`: "one names-only
registry row: a declared secret's name, how it is delivered/routed, and who
declared it — NEVER its value") with the VALUES held in an external SecretSpec
provider, written through a subprocess CLI (`SpecResolver.Set`,
`go/internal/secrets/resolver.go`: "Set writes value into the provider for
name via the pinned CLI, feeding the value on stdin"). No comparable OSS
project runs this shape (RIG-3637); Matt ruled user-secret values move into a
compass-owned Postgres store, encrypted at rest, while boot secrets stay on
secretspec read-only. A DB dump, replica, or operator `SELECT` must not be a
secret compromise.

## Approach

### A1 — Value columns on the existing `secrets` table, born encrypted

The user registry table `secrets` (`go/internal/store/migrations/0001_init.sql`,
`CREATE TABLE secrets`) gains three columns; no separate values table:

```sql
value_ciphertext BYTEA,               -- AES-256-GCM ciphertext of the UTF-8 value
value_nonce      BYTEA,               -- 96-bit nonce, fresh per encryption
key_version      SMALLINT NOT NULL DEFAULT 1  -- which key generation encrypted this row
```

`value_ciphertext`/`value_nonce` are `NOT NULL` in the FINAL schema (declare and
set are ONE operation on the user path: `ListSecrets` already documents "a
declared row means SetSecret declared AND wrote it (the flow is
declare-then-set), so is_set=true for every declared row" —
`go/server/secrets_service.go`, `ListSecrets`; a user row is born with its
value, and declared-but-unset is a server-secret state only). But the NOT NULL
is added in T5, NOT T2. **Transitional posture (see T2):** T2 lands the two
value columns NULLABLE, because T2 deliberately KEEPS the value-free
`InsertSecret` query and `DeclareSecret` store method (their caller, the
`SetSecret` handler, does not migrate until T5). That retained value-free insert
writes no value columns, so an unconditional NOT NULL in T2 would make every
live `SetSecret` RPC and the existing DeclareSecret pgtests hit a NOT NULL
violation at runtime through T2–T4. T5 removes `InsertSecret`/`DeclareSecret`,
cuts `SetSecret` over to the upsert (which always writes both value columns),
and tightens the two columns to NOT NULL in the same task — so the constraint
arrives exactly when every writer satisfies it. One row per secret also
collapses the documented non-atomic declare/set/rollback trio (`SetSecret` doc:
"The declare/set/rollback trio is not atomic and assumes no concurrent
same-name writer") into a single transactional upsert — the rollback machinery
and the orphaned-declaration hazard are deleted, not ported.

The `0001_init.sql` comment "Deliberately absent and load-bearing: NO value
column (encryption-at-rest is the provider's job)" is rewritten in the same
migration edit: the invariant it protected is deliberately broken by ruling
D1, exactly as the sibling record broke it for `gateway_credentials`. The
never-LOGGED half of the invariant (`go/internal/secrets/secrets.go` package
doc) is untouched and still binds every new type.

The repo carries a single squashed migration file (`0001_init.sql` is the only
file under `go/internal/store/migrations/`; its comments record prior
squashes, e.g. "the container hop that 0003 introduced was collapsed out").
The sibling record's cutover posture is Matt-ruled wipe-and-redeclare ("the DB
is wiped and re-declared (Matt-ruled — single operator, no live data)"), so
this record amends `0001_init.sql` in place rather than adding `0002` (a
recorded deferral — see `## Resolved decisions`, D5).

sqlc work implied: `go/internal/store/queries/secrets.sql` replaces
`InsertSecret`/`DeclaredSecrets` with upsert/read-with-values queries (T2),
then regenerate the `go/internal/store/db` package. The hand-written `Store`
methods keep the door-side validation posture the sqlc adoption preserved
("the hand-written Store methods keep their signatures, the door-side
validation … and the ErrConflict/ErrInvalidArgument/ErrNotFound mapping",
`queries/secrets.sql` header).

### A2 — Encryption: AES-256-GCM, one master key, `key_version`, row-bound AAD

**Primitive: AES-256-GCM** (stdlib `crypto/aes` + `cipher.NewGCM`), matching
the sibling record's frozen D1 so the tree carries ONE at-rest primitive.
There is no crypto in the Go tree today (verified this session: zero non-test
hits for `aes.`/`NewCipher`/`gcm`/`nacl`/`sealedbox` under `go/internal` and
`go/server`), so the AEAD helper is a new, small, deliberately justified
package `go/internal/envelope` — the reusable seam the sibling record already
calls for ("the envelope helper is therefore a new (small, justified)
package … the seam must be reusable if a later store ever needs the same
discipline"). The later `gateway_credentials` build reuses it unchanged.

**Single master key, not per-row DEKs.** The envelope alternative (a per-row
data key wrapped by a KEK) buys two things: per-row rewrap without touching
ciphertexts, and KMS-unwrap granularity. Neither is live here: rotation
rewrites rows anyway to bump `key_version` (A3), the managed plane gets
KMS-grade custody by pointing the ONE key at a KMS-backed provider URI (the
sibling record's D3, zero core change), and per-row DEKs double the crypto
surface (two encrypt paths, two failure modes) for a table whose write rate is
human-driven. The sibling record ruled single-key + `key_version` for the far
hotter `gateway_credentials` write load; the colder table does not earn more
machinery. See Alternatives.

**Nonce strategy: fresh random 96-bit nonce per encryption, never reused,
stored beside the ciphertext.** Random 96-bit nonces are collision-safe to
~2^32 encryptions per key (NIST SP 800-38D, quantified in the sibling
record). User-secret writes are human-driven RPC calls (`SetSecret` is
"user-driven CLI", `go/server/secrets_service.go`) — orders of magnitude
below the hourly-refresh load the sibling record already accepted under the
same bound. Nonce reuse is structurally prevented, not procedurally: the
nonce is generated inside `envelope.Key.Encrypt` from `crypto/rand` on every
call and returned to the caller — no caller-supplied nonce parameter exists,
so no caller can reuse one.

**AAD binds a ciphertext to its row.** Additional authenticated data is the
five-field A9 tuple
`"compass/user-secret/v1\x00" || tenant_id || "\x00" || decimal(scope_kind) ||
"\x00" || scope_id || "\x00" || name || "\x00" || decimal(key_version)`,
the two SMALLINTs rendered `strconv.FormatInt(int64(v), 10)`.
The domain-separation label means a `gateway_credentials` ciphertext (same
key family, future table) can never decrypt as a user secret; the name means
swapping two rows' ciphertext columns fails authentication instead of
returning the wrong secret under the right name; the key_version binds the
row's claim about which key generation encrypted it. `delivery`/`kind`
routing columns stay OUTSIDE the AAD: they are mutable metadata a re-declare
may legitimately change without re-encrypting, and tampering with them
misroutes but never discloses.

The scope pair is bound for the same reason the identity fields are: a
ciphertext must not be re-scopable by a column rewrite (promoting an
agent-scoped row to tenant scope, or moving it to another agent, fails
authentication). And it is bound NOW, before any migration ships, because the
AAD is baked into every ciphertext — a scope field added LATER would force a
re-encrypt of every row. Every field is bound unconditionally: a tenant row
binds `scope_id` as the empty string, so the AAD's field count never varies,
and the `\x00` separators keep the encoding injective.

**`tenant_id` is in the AAD from the first row, by decision (Matt,
2026-09-11).** It is not load-bearing today and is deliberately included
anyway. The `secrets` PRIMARY KEY carries no `tenant_id` — pre-A9 it was
`name` alone, post-A9 it is `(name, scope_kind, scope_id)`
(`go/internal/store/migrations/0001_init.sql`) — even though the table sits in
the RLS `tenant_tables` set (`go/internal/store/rls_pgtest_test.go`), a
pre-existing wart this record does NOT fix. Two consequences follow from it.
First, the `ON CONFLICT (name, scope_kind, scope_id)` upsert inherits the
failure `TestForgeAuthoredTwoTenantsSameCoordinate` already documents: tenant
B's write hits tenant A's RLS-invisible row and Postgres rejects it, so
tenant B cannot declare a coordinate tenant A holds. Second, if that PK is
later corrected to include `tenant_id`, two tenants' same-named rows would
otherwise carry IDENTICAL AAD under the shared master key, and a
cross-TENANT ciphertext swap would authenticate cleanly — exactly the
row-portability the AAD exists to kill. Binding the tenant now costs one
field; retrofitting it later means
re-encrypting every row, the same "cheap at creation, expensive retrofit"
logic this record already accepts for `key_version`. Where a row has no
tenant scope, the empty string is bound explicitly rather than the field being
omitted, so the AAD's shape never varies.

**Threat-model boundary.** The AAD binds identity, NOT freshness. It defeats
cross-row substitution (swapping two rows' ciphertext columns) and cross-domain
substitution (a `gateway_credentials` ciphertext decrypting as a user secret).
It does NOT defeat rollback: a write-capable attacker can replace a row's
`(value_ciphertext, value_nonce)` with an OLDER pair for the same `name` and
`key_version`, and that stale value authenticates. Rollback-to-a-prior-value by
a DB writer is explicitly OUT of scope — the Problem scopes the threat to a
read-only DB dump, replica, or operator `SELECT`; the row-binding AAD is
defense-in-depth against a writer, not a freshness guarantee. Renames and
re-scopes cannot occur: `(name, scope_kind, scope_id)` is the primary key
(A9), the upsert conflicts on exactly that key, and no rename or re-scope
verb exists, so every AAD-bound identity field is stable for a row's lifetime.

**Accepted leak: value length.** AES-GCM is length-preserving, so a DB dump
reveals every secret's byte length. This is an accepted leak; no padding is
added.

**Key custody (D3).** The key is a boot secret resolved from secretspec
through the SERVER resolver under the reserved name `COMPASS_MASTER_KEY`
(renamed from the gateway spelling `GATEWAY_CREDENTIALS_MASTER_KEY`, because
it is now shared with the future `gateway_credentials` store rather than
owned by it). The claim that the master key's guards "already exist" is FALSE:
they exist for the OLD spelling only, and two of them actively REJECT the new
one. The `server_secrets_reserved_prefix` CHECK in `0001_init.sql` admits only
`SERVER\_%` and `GATEWAY\_CREDENTIALS\_%` (the backslash escapes the LIKE `_`
wildcard so a near-miss like `SERVERX_Y` is rejected); `serverSecretPrefixes`
(`go/internal/store/server_secrets.go`) holds those same two prefixes, so
`HasServerSecretPrefix` rejects a `COMPASS_`-prefixed name and
`DeclareServerSecret` errors on it; and `SpecResolver.Resolve` builds its
manifest from the DECLARED registry, so an undeclared name never resolves.
`COMPASS_MASTER_KEY` carries neither existing prefix, so its row is
unconstructible, the key never resolves, and boot fails closed FOREVER — even
after the seed verb runs. The fix is the smallest change honoring Matt's
rename: add a `COMPASS_` reserved prefix alongside the existing two (as
`name LIKE 'COMPASS\_%'` in the CHECK, escaped in the same style so it never
admits a near-miss like `COMPASSX_Y`) and keep the neutral spelling. That
reserved-name machinery — the CHECK, the prefix set,
`masterKeyName`, `masterKeyCLIName`, and the tripwire — lands in T4 (see Plan);
the `server_key_state` tripwire ("Boot recomputes the digest and fails closed
on a mismatch", `0001_init.sql`) is updated for the new name there. One master
key is SHARED between the user-secret store and the future `gateway_credentials`
store (D3): one custody story, one tripwire, one rotation procedure; the AAD
domain-separation label keeps the two ciphertext domains disjoint under the
shared key. That disjointness binds the FUTURE `gateway_credentials` build to
adopt its OWN distinct AAD domain label, and holds only if it does. The cost of
one shared key is a wider blast radius: a compromised master key exposes BOTH
ciphertext domains at once — the accepted tradeoff for a single custody story.
The key is resolved ONCE at boot and held in process memory (unexported
`[32]byte` inside `envelope.Key` — reflection-proof, the sibling record's D5
posture), because a decrypt happens on every `FetchSecrets`. Go offers no
guaranteed memory zeroization, so the key lives in the long-lived process for
its lifetime — the accepted posture — and it also transits the secretspec
subprocess read at boot.

**Boot tripwire.** Boot resolves the key, computes the salted digest, and
verifies it against `server_key_state` (table exists; no Go code reads it yet
— verified: no non-test references outside the migration and its pgtest).
First boot writes the row; a later mismatch fails startup closed, catching a
swapped key before it decrypts anything. Missing key: fail startup with an
actionable message naming the exact provisioning command — the standalone
`compass` key-generation verb (D1/D2), which the operator or the nix/devenv
seed script runs before boot.

### A3 — Key rotation: deferred to a later record; the schema reserves it now

Active rotation is explicitly DEFERRED — this record ships no re-encrypt
machinery. What is reserved NOW so deferring stays cheap:

- `key_version SMALLINT` on every value row (which generation encrypted it) —
  the retrofit the sibling record's D4 calls "cheap at creation, expensive
  retrofit".
- `server_key_state.key_version` (exists) as the active-generation pointer.
- AAD includes the key_version, so a rotated row's ciphertext authenticates
  its own generation claim.
- The envelope API takes the key as a parameter per call (`envelope.Key`
  value, not package state), so a rotation pass holding TWO keys (old for
  read, new for write) needs no API change.

The deferred record's shape, stated so the reservation is checkable: rotation
is an online, row-at-a-time re-encrypt (read under `key_version=N`, write
under `N+1`, one row per transaction — no long table lock), with both keys
resolvable during the window under versioned secretspec names, finishing by
bumping `server_key_state`. Operator action: provision the new key value,
run the rotation verb, retire the old value. None of that is built here.

**Interim key-incident procedure.** Until that rotation record lands, the
deferred rotation machinery does not exist, so any out-of-band key change
(operator error, provider restore from backup, compromise response) trips
`server_key_state` into a PERMANENT boot failure. The documented response is to
wipe and re-set every secret — acceptable under the pre-production
single-operator posture (no live data), and no better recovery exists until
rotation ships.

### A4 — The resolver seam: split the interface, delete the write half

After this record plus D4, the four-method `secrets.Resolver`
(`go/internal/secrets/resolver.go`: `Resolve`, `Set`, `Delete`, `Statuses`)
is wrong for BOTH sides:

- The boot/server side needs only reads: `Resolve` (the five boot callers in
  `go/server/serve.go` — `validateForgeSecret`, `newDeclaredSecretResolver`,
  `wireForgeWriteCaller`, `buildLinearTokenSource`, `forgeSecretDeclared`)
  plus `Statuses` (the surviving `ListServerSecrets` probe,
  `s.serverResolver.Statuses(...)` in `go/server/secrets_service.go`).
- The user side stops touching secretspec entirely.

So `secrets.Resolver` SHRINKS to the read pair and stays the shared name:

```go
// go/internal/secrets/resolver.go — the read-only seam, post-D4.
type Resolver interface {
    Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error)
    Statuses(ctx context.Context, reason string) ([]SecretStatus, error)
}
```

`SpecResolver` keeps implementing it. Its `Set`/`Delete` methods, `setArgs`,
the CLI-shelling write machinery, `WithCLI`, and the write-path tests are
DELETED — but only in T6, after their last callers are gone (the user handlers
migrate in T5, the server handlers are removed in T6). Until then `Resolver`
keeps `Set`/`Delete` and `SpecResolver` keeps its write half, so every
intermediate task compiles (Plan dependency order, F2 sequencing). D4 is what
makes the deletion total rather than partial. `Handler.FetchSecrets`
(`go/internal/runnerhub/handler.go`) stops consuming `Resolver` entirely
under the A9 scope amendment: it is retyped to a narrow package-local
`ResolveFor` seam (A5), the same narrow-by-design pattern its
`AgentConfigStore` dependency already uses in that file — so the T6 shrink
leaves `SpecResolver` as the interface's only implementor.

The user side gets a CONCRETE type, not a second implementation of a shared
write interface — there is exactly one DB-backed store and no second backing
to swap in, so an interface would be weightless:

```go
// go/internal/secrets/storeresolver.go (new)
type StoreResolver struct { /* st *store.Store; key envelope.Key; keyVersion int16 */ }

func NewStoreResolver(st *store.Store, key envelope.Key, keyVersion int16) *StoreResolver
// ResolveFor resolves the ONE most-specific row per name visible to agent
// (A9: agent > user > tenant, collapsed in SQL by SecretRecordsForAgent),
// decrypts each, and maps delivery/kind via the existing
// deliveryFromStore/kindFromStore edge maps. The container-path read.
func (r *StoreResolver) ResolveFor(ctx context.Context, agent store.AccountID, reason string) ([]ResolvedSecret, error)
// Upsert validates, encrypts, and transactionally upserts declaration+value
// at (name, scopeKind, scopeID). Named Upsert (not Set): nothing of its
// signature matches the old Resolver write half.
func (r *StoreResolver) Upsert(ctx context.Context, actor store.AccountID, name string, scopeKind int16, scopeID, value string, delivery DeliveryKind, kind SecretKind, provider, host string) error
// Remove transactionally removes declaration+value at (name, scopeKind,
// scopeID); ErrNotFound maps out.
func (r *StoreResolver) Remove(ctx context.Context, actor store.AccountID, name string, scopeKind int16, scopeID string) error
```

Crypto stays in the `secrets`/`envelope` layer; `store` sees only
ciphertext+nonce bytes (it stays the leaf the package doc requires: "store
stays a leaf and the two evolve independently").

`StoreResolver` implements NO interface (A9): the handler consumes
`ResolveFor` through its own narrow seam, and `ListSecrets` computes `is_set`
from the store directly (every user row is set by construction) — so the old
plan's `Resolve`/`Statuses` pair, which existed only to satisfy the
`Resolver` typing `Handler.FetchSecrets` carried, is not built at all.

**The Version contract is preserved, with no stored hash.** `ResolveFor` keeps
returning `ResolvedSecret` with `Version: Version(value)` — the SHA-256
content hash (`Version`, `go/internal/secrets/secrets.go`) — computed AFTER
decrypt, per resolve. Storing the hash in a column would recreate exactly the
offline confirmation oracle the `Version` doc comment warns about ("the hash
cannot serve as an offline confirmation oracle for a low-entropy secret") —
in the DB dump this record exists to make worthless. One SHA-256 per secret
per fetch is noise next to the AES decrypt beside it. T6 rotation-diff
semantics are unchanged: a same-value re-set still hashes identically.

### A5 — Re-pointing the callers

- **`Handler.FetchSecrets`** (`go/internal/runnerhub/handler.go`): the A9
  scoped read. `buildSecretResolvers` (`go/server/serve.go`) still returns
  the `*StoreResolver` as the container-side value, but the handler field is
  retyped from `secrets.Resolver` to a narrow package-local seam carrying
  `ResolveFor` (the `AgentConfigStore` pattern in the same file), the two
  authz lookups (`HasLiveSession`/`HasContainerBinding`,
  `go/internal/runnerhub/relay_comms.go`) become account-RETURNING lookups
  (their maps already hold the account and discard it — A9), and the call
  becomes `ResolveFor(ctx, agentAccount, "runner fetch")`. The
  CodeFailedPrecondition nil-resolver posture is unchanged. The hot-read FFI
  motivation is dissolved: the hot path is now one indexed SELECT plus
  in-process AES, no subprocess and no FFI.
- **`SetSecret`** (`go/server/secrets_service.go`): the declare-then-Set
  two-system flow (`s.store.DeclareSecret` + `s.resolver.Set`) with its
  ErrConflict re-set branch and rollback-on-fresh-write-failure collapses
  into one `StoreResolver.Upsert` upsert transaction. Re-set of an existing name
  stays a value rewrite (UPSERT). Scope on this verb: `SetSecret` writes the
  TENANT row (`scope_kind = 0`, `scope_id = ''`), preserving today's
  observable inject-all behavior — a wire surface for writing user- or
  agent-scoped rows, and who may write each tier, is an open question
  (`## Open questions`), deliberately not smuggled into this amendment. The
  user-only gate (`requireUser`), empty value rejection,
  `secretRoutingFromProto`, and `bumpSecretsVersion` are unchanged.
- **`DeleteSecret`** (`go/server/secrets_service.go`): one transaction
  deleting the row (declaration and value are the same row), addressed at
  the tenant coordinate (`scope_kind = 0`, `scope_id = ''`) until the scope
  wire surface exists, matching `SetSecret`. PR #1066's
  reviewed semantics fold in as follows — carried forward: the guard that a
  reserved-prefix name never reaches the user delete door, split as
  `HasServerSecretPrefix` (byte-exact ADMIT check at the server door,
  `go/internal/store/server_secrets.go`) vs a ported
  `ShadowsServerSecretPrefix` (case-FOLDING REJECT check at the user door —
  not in tree today, verified; it lives in the held PR's diff and is
  re-implemented here as a `store` predicate). Subsumed, not ported: the
  declaration-first ordering and the no-rollback / `CodeUnavailable` +
  recovery-hint posture existed because declaration (Postgres) and value
  (provider) were two systems that could fail independently; with both in
  one row there is no half-deleted state to order or to recover from. An
  absent name stays `CodeNotFound`.
- The case-fold rationale survives even though the shared-provider-keyspace
  threat it guarded is gone from the user path: the F1 name partition is
  still what keeps a reserved-prefix name out of the container-delivered
  table, so the user door keeps rejecting reserved names case-insensitively
  (defense in depth at both `Upsert` and `Remove`).

### A6 — Removing the boot write path (D4)

Deleted: `SetServerSecret` and `DeleteServerSecret` handler bodies and their
`s.serverResolver.Set(...)` / `s.serverResolver.Delete(...)` calls
(`go/server/secrets_service.go`), the `compass server-secret set` CLI verb
(`newServerSecretSetCmd` / `runServerSecretSet` / `serverSecretWireName`,
`go/cmd/compass/server_secret.go`), and the entire `SpecResolver` write half
(A4). Surviving: `ListServerSecrets` (reads `serverResolver.Statuses`) and
the `server-secret list` verb (`newServerSecretListCmd`), including the
seed-script-consumed `<BARE_NAME>: set|unset` rendering contract
(`renderServerSecretStatus`).

**Public proto impact — decided: deletion (D4).** `SetServerSecret` /
`DeleteServerSecret` RPCs and their four request/response messages
(`proto/compass/v1/compass.proto`, `SecretsService`) are DELETED, not stubbed.
This is grounded on no server secret needing a runtime WRITE. The earlier
draft claimed every server secret is boot-time and the only post-boot resolver
call is the value-free `Statuses`; that is FALSE (verified). Several server
secrets are RE-READ at runtime through the resolver:
`newDeclaredSecretResolver` (`go/server/serve.go`) returns a closure calling
`Resolve` per invocation; the webhook-secret resolver runs on EVERY request to
the unauthenticated `POST /webhooks/github`, BEFORE the HMAC check, TTL-cached
at `forgeTokenTTL = 5 * time.Minute`; the App-key resolver reads the PEM on
every token mint. What none of them needs is a runtime WRITE — the runtime
readers all go through the resolver, so provider-side rotation (operator writes
the provider with the secretspec CLI; the lazy re-resolve picks it up within
the TTL) replaces the write RPCs. `Statuses` survives as the sole post-boot
call the surviving `ListServerSecrets` still needs. The pre-removal caller
check found only `go/cmd/compass/server_secret.go`, its tests, and
`go/internal/auth/admin_gate.go`. The user-facing
`SetSecretRequest`/`DeleteSecretRequest` shapes are UNCHANGED (name, value,
delivery, kind — all still meaningful), so no user-path proto edit exists.

How boot secrets get VALUES once the RPC write path is gone: the operator
seeds them into the configured provider, and compass only ever reads. The
master key itself is operator-seeded via the standalone `compass`
key-generation verb (D1/D2), which the nix/devenv seed script invokes; boot
only reads it, failing closed naming that verb.

### A7 — Migration of existing data: nothing in the DB; provider values re-set

Verified from code: the registry is names-only — `secrets` has no value
column (`0001_init.sql`), `DeclareSecret` "stores NO value — the value lives
in the SecretSpec provider" (`go/internal/store/secrets.go`), and `Resolve`
fetches values from the provider per call. So there is NO database data to
migrate. User secret VALUES do live in the external provider keyspace and
are NOT carried over mechanically: per the sibling record's Matt-ruled
cutover posture ("single operator, no live data" — wipe and re-declare), the
operator re-runs `compass secret set` for each user secret after the cutover
deploys, and the old provider entries are left to the operator to garbage-
collect with the `secretspec` CLI. No import tool is built (a recorded
deferral — see `## Resolved decisions`, D5).

### A8 — Test strategy (red-green)

Per `rule://red-green-testing`: each task lands its failing test first.

- `envelope`: pure unit tests, no Postgres — round-trip, AAD mismatch fails
  authentication (row-swap simulation: decrypt row A's ciphertext under row
  B's name-AAD), tampered ciphertext fails, wrong key fails, two encrypts of
  one plaintext yield distinct nonces and ciphertexts, key type redacts under
  `%v`/`%#v`/`json.Marshal`.
- Store + service: pgtest suites (`pgtest.RequireDSN`), run with
  `COMPASS_TEST_USE_CONTAINER=1` locally. Ordering constraint, stated
  because it bites: `RequireDSN` SKIPS in a container-less sandbox and
  hard-fails only under `COMPASS_REQUIRE_LIVE` (`go/internal/pgtest/pgtest.go`)
  — so any fail-closed guard a test adds (e.g. asserting the fixture built a
  resolver with a key) MUST come AFTER the store fixture call, or the guard
  fires in sandboxes where the suite is defined to skip.
- The service fixture upgrades from `recordingResolver` fakes
  (`go/server/secrets_service_pgtest_test.go`) to the REAL `StoreResolver`
  over the pgtest store for the user path — the fixture already has a real
  store, so the tests get realer, not fakier. The server-side
  `recordingResolver` survives only for the read/`Statuses` surface.
- Red-green anchors: ciphertext-at-rest test (raw `SELECT value_ciphertext`
  never contains the plaintext bytes); row-swap test red before AAD lands;
  reserved-prefix delete-door tests red before the ported
  `ShadowsServerSecretPrefix` guard lands; boot tripwire test red before the
  digest check lands.

### A9 — Scope model: tenant / user / agent, most-specific-wins

Matt ruled (2026-09-11) that user secrets are scoped at THREE levels —
tenant, user, and agent — replacing the flat per-tenant namespace the
sections above were first written against. This section is the normative
amendment; the task deltas it implies are folded into T1/T2/T3/T4/T5 below,
and the questions it opens live in `## Open questions`.

**Schema.** `secrets` gains two scope columns and a composite primary key,
in the same `0001_init.sql` edit that lands A1's value columns (T2):

```sql
scope_kind SMALLINT NOT NULL CHECK (scope_kind IN (0, 1, 2)),  -- 0 tenant, 1 user, 2 agent
scope_id   TEXT NOT NULL DEFAULT '',  -- '' for a tenant row; the owning accounts.id for user/agent
PRIMARY KEY (name, scope_kind, scope_id),
-- scope shape, enforced in the secrets_kind_routing style: a tenant row
-- carries no id; a user/agent row must carry one. UpsertSecret guards the
-- same invariant at the door so a caller gets ErrInvalidArgument.
CONSTRAINT secrets_scope_shape CHECK (
    (scope_kind = 0 AND scope_id = '')
    OR (scope_kind IN (1, 2) AND scope_id <> '')
)
```

A tenant-scoped row is a real shared VALUE, not a declaration placeholder —
several users share one secret through it — so today's flat rows map onto
scope 0 with no semantic change. **`scope_id` carries NO foreign key to
`accounts (id)`, by decision.** A tenant row's `scope_id` is `''`, which can
never satisfy an FK, and Postgres has no conditional (partial) FK to exempt
it; the alternatives — a sentinel account row, or splitting scoped rows into
an FK-carrying table of their own — respectively pollute the accounts table
and re-create the two-table consistency problem A1 deletes. Referential
integrity for user/agent `scope_id` is enforced at the store door instead:
`UpsertSecret` resolves the target account (and its subtype) in the writing
transaction, the `secrets_scope_shape` CHECK holds the shape, and
`declared_by` (FK, ON DELETE RESTRICT) still ties every row to a real
account. What door-side enforcement does NOT give is delete-time behavior —
what happens to an agent-scoped row when its agent account is deleted is an
open question, not silently decided here.

**Resolution: most-specific-wins, collapsed in SQL.** The injected
environment holds ONE value per name: an agent-scoped row shadows the
same-named user row, which shadows the tenant row (`agent > user > tenant`).
`FetchSecrets` resolves for one agent account, and the read is a single
query — the store-layer `SecretRecordsForAgent` (T2):

```sql
-- $1 = the calling agent's account id. DISTINCT ON keeps the first row per
-- name under the ORDER BY; scope_kind DESC is the precedence (2 > 1 > 0).
SELECT DISTINCT ON (s.name) s.*
  FROM secrets s
  JOIN agent_accounts a ON a.account_id = $1
 WHERE (s.scope_kind = 0 AND s.scope_id = '')
    OR (s.scope_kind = 1 AND s.scope_id = a.owner_user_id)
    OR (s.scope_kind = 2 AND s.scope_id = a.account_id)
 ORDER BY s.name, s.scope_kind DESC;
```

The user tier resolves through `agent_accounts.owner_user_id`
(`0001_init.sql`: `owner_user_id TEXT NOT NULL REFERENCES user_accounts
(account_id)`) — agent → owning user is a single FK hop, so no second query
exists and no caller-supplied user id exists to get wrong. The collapse
happens in SQL (`DISTINCT ON` ordered by the numeric `scope_kind`, whose
0/1/2 encoding IS the precedence), not in Go: shadowed rows never leave
Postgres, so the resolver never decrypts a value it would discard, and the
precedence lives in one ORDER BY instead of a Go merge a future writer can
reorder.

**The agent identity is already at the call site.** `FetchSecrets`
(`go/internal/runnerhub/handler.go`) authorizes via `Hub.HasLiveSession` /
`Hub.HasContainerBinding` (`go/internal/runnerhub/relay_comms.go`), which
look up `h.sessionAccounts` / `h.containerAccounts` — both
`map[string]store.AccountID` (`go/internal/runnerhub/hub.go`) — and DISCARD
the account they find. The agent account identity is therefore ALREADY
resolved at the exact place the scoped read needs it; scoping costs a lookup
that returns the account instead of a bool, not a new identity channel. The
two membership checks become account-returning lookups (the session side
already has the private `accountForSession`; the container side gains its
analogue), and the handler passes the account to `ResolveFor` (A4/A5). The
handler comment "same inject-all set for either (no per-agent
differentiation in the MVP)" names exactly the seam this amendment fills.

**RLS interaction: scope is an ADDITIONAL filter inside a tenant, never a
substitute.** `secrets` sits in the `tenant_tables` array in `0001_init.sql`,
so it carries `ENABLE`+`FORCE ROW LEVEL SECURITY` under the
`tenant_isolation` policy, and `TestRLSCatalogEnabledAndForced`
(`go/internal/store/rls_pgtest_test.go`) asserts both flags from the live
`pg_class` catalog for every table carrying a `tenant_id` column — adding
scope columns changes neither the `tenant_id` column that puts `secrets` in
that set nor the policy, so the table cannot silently drop out of RLS. Every
scoped query above runs UNDER the tenant policy: RLS bounds the visible rows
to the tenant first, and the scope predicate narrows within them. A scope
bug can at worst leak a secret to the wrong agent WITHIN a tenant; it can
never cross a tenant boundary — that guarantee stays RLS's, untouched.

**What does NOT change.** The encryption primitive (AES-256-GCM, A2), the
single master key and its custody (D3, DL-355), `key_version` and the
deferred rotation posture (A3), the read-only boot path (D4), and the
deletion sequencing discipline (every deletion lands after its last caller
is gone — Plan) are all untouched. The scope model changes WHICH row a
caller sees and what identifies a row, never how a value is protected.

## Alternatives considered

### Per-row data keys wrapped by a KEK (full envelope)

Each row gets its own random DEK encrypting the value; the KEK (from
secretspec) wraps each DEK. Rejected: its two benefits are dead here. (1)
Rotation without touching ciphertexts — but our rotation (A3) rewrites rows
anyway to bump `key_version`, and a KEK-only rewrap still touches every row's
wrapped-DEK column, the same row-count of writes. (2) KMS-unwrap-per-row
custody — but the managed plane gets KMS custody by provider URI on the ONE
key (sibling record D3) with zero core change. Cost: a second ciphertext
column, two chained AEAD operations per read on the `FetchSecrets` hot path,
and double the crypto code to review in a tree that had none yesterday. The
sibling record already weighed this shape for a hotter table and ruled
single-key + `key_version`.

### A separate `secret_values` table keyed by name

Keeps the names-only `secrets` table byte-identical. Rejected: it re-creates
the two-write consistency problem this design deletes (declaration row +
value row = the declare/set/rollback trio again, now with FK ordering), and
the "names-only registry" invariant is already broken by ruling D1 — there
is nothing left for the split to protect. One row per secret is the shape
that makes `SetSecret`/`DeleteSecret` single-transaction.

### pgcrypto / pgsodium (encrypt inside Postgres)

Rejected: the key transits into the SQL layer, so it appears in query text,
`pg_stat_statements`, and server logs — the DB-dump threat model this record
exists for extends to the DB host, which application-side encryption keeps
key-free. Also adds an extension dependency the migrations don't carry today.

### XChaCha20-Poly1305 instead of AES-GCM

Larger (192-bit) nonce space removes the 2^32 birthday bound. Rejected: the
bound is not binding at this table's human-driven write rate, it would make
two at-rest primitives coexist against the sibling record's frozen
AES-256-GCM ruling, and it pulls `golang.org/x/crypto` where stdlib
suffices.

### Keep the four-method `Resolver` with a DB-backed second implementation

Rejected: after D4 no caller uses `Set`/`Delete` on the spec side (the boot
path is read-only), and the user side's writes take `actor` and routing
parameters the provider write never had — so `StoreResolver` names them
`Upsert`/`Remove`, distinct concrete methods that share only the read pair with
`Resolver`. Keeping the wide interface would force dead `Set`/`Delete` methods
on `SpecResolver` and mismatched signatures no `StoreResolver` write could
satisfy. The resolved design shrinks the interface to what both sides share —
reads — and lets the user path keep its own richer write surface off the
interface.

## Plan

Dependency order: T1 → T2 → T3 → T4 → T5 → T6, with T7 (docs/ledger) last. The
order is compile-safe by construction: additive work comes first and every
DELETION is deferred to the task AFTER its last caller is gone. T2 ADDS the
value columns and upsert/read queries but KEEPS `InsertSecret`/`DeclareSecret`
(their caller is removed in T5); T3 ADDS `StoreResolver` and keeps the wide
`Resolver` (its `Set`/`Delete` and `SpecResolver`'s write half are removed in
T6, after the user handlers migrate in T5 and the server handlers are deleted
in T6). Each task is one PR-sized unit that compiles on its own and carries its
own red-green cycle.

### T1 — `envelope` package: AEAD seam

New package `go/internal/envelope`. AES-256-GCM; key held unexported;
nonce generated internally per call; AAD caller-supplied.

Interfaces:

```go
package envelope

// Key is a 256-bit AES-GCM key. The bytes are unexported ([32]byte) so no
// reflection-based logger or marshaler can reach them.
type Key struct{ /* k [32]byte */ }

// NewKey copies raw (must be exactly 32 bytes) into a Key; the caller's
// slice may be zeroed afterward.
func NewKey(raw []byte) (Key, error)

// Fingerprint returns the salted SHA-256 digest of the key under salt —
// the non-secret server_key_state tripwire value.
func (k Key) Fingerprint(salt []byte) []byte

// Encrypt seals plaintext under aad with a fresh random 96-bit nonce,
// returning (nonce, ciphertext). Nonce reuse is structurally impossible:
// there is no nonce parameter.
func (k Key) Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error)

// Decrypt opens ciphertext under nonce and aad. Any tamper — ciphertext,
// nonce, or aad (wrong row identity) — returns ErrDecrypt.
func (k Key) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error)

// ErrDecrypt is the single opaque decrypt failure; it never wraps cipher
// internals or carries plaintext/key material.
var ErrDecrypt = errors.New("envelope: decrypt failed")

// UserSecretAAD builds the canonical user-secret AAD (the A9 tuple):
// "compass/user-secret/v1\x00" + tenantID + "\x00" +
//   decimal(scopeKind) + "\x00" + scopeID + "\x00" +
//   name + "\x00" + decimal(keyVersion)
// scopeKind/keyVersion render via strconv.FormatInt(int64(v), 10). Every
// field is bound unconditionally — a tenant row binds scopeID as "", a
// scope-less deployment binds tenantID as "" — so the AAD's field count
// never varies, and the \x00 separators keep the encoding injective.
func UserSecretAAD(tenantID string, scopeKind int16, scopeID, name string, keyVersion int16) []byte
```

Tests (red first): round-trip; distinct nonces/ciphertexts across two
encrypts of one plaintext; tamper on each of ciphertext/nonce/aad fails
with ErrDecrypt; wrong key fails; `String`/`GoString`/`json.Marshal` of Key
leak nothing; UserSecretAAD is injective across field tuples that
concatenate ambiguously (the `\x00` separator test), including the scope
pair — e.g. a scopeID whose tail could re-split as a name prefix.

### T2 — Store: value columns + upsert/read queries + `ShadowsServerSecretPrefix`

Amend `0001_init.sql` (`secrets` table + comment rewrite, A1) — the same edit
lands the A9 scope schema: `scope_kind SMALLINT NOT NULL CHECK (scope_kind IN
(0, 1, 2))`, `scope_id TEXT NOT NULL DEFAULT ''`, the PK widened from `name`
to `(name, scope_kind, scope_id)`, and the `secrets_scope_shape` CHECK. The
two value columns `value_ciphertext`/`value_nonce` land NULLABLE here (A1
transitional posture): T2 keeps the value-free `InsertSecret`/`DeclareSecret`
path live for its T5 caller, and that path writes no value columns, so an
unconditional NOT NULL now would fail every live `SetSecret` and the existing
DeclareSecret pgtests at runtime (the retained value-free insert writes the
tenant coordinate `(0, '')`, so the new PK and CHECKs hold for it unchanged).
T5 tightens the two columns to NOT NULL once the upsert is the only writer.
Extend `go/internal/store/queries/secrets.sql` ADDITIVELY: add
`UpsertSecret :exec` (INSERT … ON CONFLICT (name, scope_kind, scope_id) DO
UPDATE on value/nonce/key_version/delivery/kind/provider/host/updated_at) and
`SecretRecordsForAgent :many` (the A9 DISTINCT ON collapse — tenant rows +
the agent's owner-user rows via `agent_accounts.owner_user_id` + the agent's
own rows, one most-specific row per name). `InsertSecret`,
`DeclaredSecrets`, and the `DeclareSecret` store method STAY in T2 — their
caller is the T5 `SetSecret` handler, so deleting them here would break code
that still compiles against them; they are removed in T5 once that caller
migrates (F2 re-sequence). `DeleteSecret :execrows` gains the scope pair in
its WHERE (a name alone no longer identifies a row). Regenerate sqlc. Add
the ported case-folding predicate.

Interfaces:

```go
// go/internal/store/secrets.go

// SecretRecord is SecretDeclaration plus the at-rest value columns. It
// carries CIPHERTEXT only — store never sees plaintext.
type SecretRecord struct {
    SecretDeclaration
    ScopeKind       int16
    ScopeID         string
    ValueCiphertext []byte
    ValueNonce      []byte
    KeyVersion      int16
}

// UpsertSecret validates name/kind-routing and the A9 scope shape at the
// door (tenant ⇒ scopeID == ""; user/agent ⇒ scopeID is an existing account
// of that subtype, resolved in the writing transaction — no FK, A9) and
// transactionally writes declaration+value in one row. A fresh coordinate
// inserts; an existing (name, scopeKind, scopeID) is a value rewrite.
// Reserved-prefix names (ShadowsServerSecretPrefix) are ErrInvalidArgument.
func (s *Store) UpsertSecret(ctx context.Context, actor AccountID, name string, scopeKind int16, scopeID string, delivery SecretDelivery, kind SecretKind, provider, host string, ciphertext, nonce []byte, keyVersion int16) error

// DeleteSecretDeclaration gains the scope pair — a name alone no longer
// identifies a row; deleting the row deletes the value with it. An absent
// coordinate stays ErrNotFound.
func (s *Store) DeleteSecretDeclaration(ctx context.Context, actor AccountID, name string, scopeKind int16, scopeID string) error

// SecretRecordsForAgent returns the ONE most-specific row per name visible
// to agent (the A9 DISTINCT ON collapse), name-ordered, ciphertext only —
// the StoreResolver.ResolveFor read. DeclaredSecrets (names-only view)
// survives for the `declarations` interface the SERVER SpecResolver still
// consumes.
func (s *Store) SecretRecordsForAgent(ctx context.Context, agent AccountID) ([]SecretRecord, error)

// go/internal/store/server_secrets.go

// ShadowsServerSecretPrefix reports whether name case-folds onto a reserved
// server-secret prefix (the PR #1066 reject-wide predicate; admit stays the
// byte-exact HasServerSecretPrefix).
func ShadowsServerSecretPrefix(name string) bool
```

Tests (red first, pgtest): upsert-then-read round-trips ciphertext bytes;
re-upsert rewrites; delete removes row (ErrNotFound after); A9 precedence —
one name at all three scopes yields exactly the agent row for that agent,
the user row for a sibling agent of the same owner with no agent row, and
the tenant row for an unrelated agent; a user/agent write naming an unknown
scope_id is rejected at the door; the scope-shape CHECK rejects a
direct bad-shape write; raw SELECT of `value_ciphertext` for a known
plaintext never contains the plaintext bytes (asserted at the store layer
with a real envelope-encrypted value); ShadowsServerSecretPrefix case
variants. `DeclareSecret`/`InsertSecret` stay live through T2 (removed in T5
with their caller), so no retirement test here.

### T3 — `StoreResolver` + interface split

Land `StoreResolver` (signatures in A4, as amended by A9) ADDITIVELY: the
scoped `ResolveFor` read plus its own concrete `Upsert`/`Remove` writes.
`StoreResolver` implements NO interface (A9): the handler consumes
`ResolveFor` through the narrow runnerhub seam that lands with the T4
wiring, and nothing of its signatures matches the old
`Resolver.Set`/`Delete`. `secrets.Resolver` KEEPS its full four-method shape
in T3 (`Resolve`, `Statuses`, `Set`, `Delete`) so every existing caller still
compiles, and `SpecResolver` keeps its write half. The shrink to the read pair
and the deletion of `SpecResolver.Set`/`Delete`/`setArgs`/`WithCLI` are deferred
to T6 (F2 re-sequence). `DeclareServerSecret` and the server `declarations`
view are untouched.

Tests (red first): `StoreResolver.ResolveFor` returns decrypted values with
`Version == secrets.Version(value)` and mapped delivery/kind, and exactly
one value per name under the A9 precedence (agent shadows user shadows
tenant, driven through a real store with all three scopes populated); a row
whose ciphertext was swapped with another row's fails ResolveFor with an
error naming the row, not a wrong value; `Upsert` round-trips through a real
store; compile-time: `var _ Resolver = (*SpecResolver)(nil)` (the spec side
still satisfies the wide interface in T3).

### T4 — Boot wiring: master-key resolve + tripwire + resolver swap + reserved-name rename

In `go/server/serve.go`: resolve the master key through the server
resolver at startup, build `envelope.Key`, verify/write `server_key_state`
(new sqlc queries `GetServerKeyState :one` / `UpsertServerKeyState :exec`
in a new `queries/server_key_state.sql`), and change `buildSecretResolvers`
to return the `StoreResolver` on the container side.

The A9 handler retype lands with this swap: `Handler.resolver` becomes the
narrow package-local `ResolveFor` seam (A5), and the two hub authz lookups
(`HasLiveSession`/`HasContainerBinding`) become account-returning lookups so
`FetchSecrets` passes the agent account it already authorized.

Reserved-name rename (F1 — the row is unconstructible without this). Add a
`COMPASS_` reserved prefix so `COMPASS_MASTER_KEY` is both declarable and
resolvable:

- Extend the `server_secrets_reserved_prefix` CHECK in `0001_init.sql` to admit
  `COMPASS\_%` (escaped in the existing style, so `_` is a literal not a LIKE
  wildcard) alongside `SERVER\_%` and `GATEWAY\_CREDENTIALS\_%` (edited in the
  same squashed file T2 amended).
- Add the `COMPASS_` prefix constant to `serverSecretPrefixes`
  (`go/internal/store/server_secrets.go`); `HasServerSecretPrefix` and the
  ported `ShadowsServerSecretPrefix` both read that set.
- Extend the `bareServerSecretName` strip list in
  `go/cmd/compass/server_secret.go` to strip `COMPASS_` too.
- Rename `masterKeyName` (`go/server/secrets_service.go`) and `masterKeyCLIName`
  (`go/cmd/compass/server_secret.go`) from `GATEWAY_CREDENTIALS_MASTER_KEY` to
  `COMPASS_MASTER_KEY`. Its post-T6 home: `resolveMasterKey` (below, in
  `serve.go`) still needs the constant, but `masterKeyName`'s only current
  consumers are the `SetServerSecret`/`DeleteServerSecret` master-key-refusal
  guards, which T6 deletes — so `masterKeyName` leaves `secrets_service.go` when
  those guards go, and the constant lives beside the other reserved-name
  constants in `go/internal/store/server_secrets.go` (next to `ServerSecretPrefix`
  / `GatewayCredentialsPrefix`), which `resolveMasterKey` reads.

Interfaces:

```go
// buildSecretResolvers now returns the DB-backed user resolver and the
// spec-backed read-only server resolver.
func buildSecretResolvers(st *store.Store, cfg ServeConfig, key envelope.Key, keyVersion int16) (container *secrets.StoreResolver, server secrets.Resolver)

// resolveMasterKey resolves COMPASS_MASTER_KEY through the
// server resolver, decodes it (64 hex chars -> 32 bytes), and verifies the
// server_key_state tripwire (first boot writes the row; mismatch fails
// startup). Absent key fails startup with the provisioning command named.
func resolveMasterKey(ctx context.Context, st *store.Store, server secrets.Resolver) (envelope.Key, int16, error)
```

Tests (red first, pgtest): declaring `COMPASS_MASTER_KEY` in `server_secrets`
SUCCEEDS (the extended CHECK and `HasServerSecretPrefix` now admit it) and boot
resolves it end to end — the regression guard that the rename does not leave an
unconstructible, never-resolving row (F1). First boot writes the tripwire row;
a second boot under the same key passes; a swapped key fails startup before any
decrypt; an absent key fails with the actionable message naming the seed verb.
The fail-closed assertions sit AFTER the pgtest fixture (A8 ordering).

### T5 — Service cutover: SetSecret/DeleteSecret on the DB

Add a `SecretScope` enum and a `scope` field to
`SetSecretRequest`/`DeleteSecretRequest` (`proto/compass/v1/compass.proto`),
regenerating the Go connect and both TypeScript surfaces. Rewrite the user
handlers in `go/server/secrets_service.go` onto `StoreResolver` (A5) with the
D9 coordinate resolution: unspecified/user scope writes
`(scope_kind=1, scope_id=caller)`; tenant scope writes `(0, "")` and requires
`UserRoleAdmin`, else `CodePermissionDenied`. `requireUser` already fetches the
caller's account, so it returns the role rather than issuing a second
`GetAccount`. `SetSecret` calls `StoreResolver.Upsert` with the rollback
machinery deleted; `DeleteSecret` calls `StoreResolver.Remove` at the same
resolved coordinate, with the reserved-prefix reject
(`ShadowsServerSecretPrefix` → CodeInvalidArgument) ahead of any store call.
`secretsService.resolver` becomes `*secrets.StoreResolver`; `serverResolver`
stays `secrets.Resolver`.

Once `SetSecret` no longer calls `s.store.DeclareSecret`, remove the now-unused
`DeclareSecret` store method and its `InsertSecret` query (kept live through
T2–T4 per the F2 re-sequence). With the value-free insert gone and the upsert —
which always writes both value columns — the sole writer, amend `0001_init.sql`
in the same task to tighten `value_ciphertext`/`value_nonce` to `NOT NULL` (the
final-schema constraint A1 defers out of T2). Regenerate sqlc for the tightened
columns.

Interfaces:

```go
func newSecretsService(
    st *store.Store,
    user *secrets.StoreResolver,
    serverResolver secrets.Resolver,
    signaler secretsSignaler,
) *secretsService
```

Tests (red first, pgtest — fixture swaps the user fake for the real
StoreResolver, A8): SetSecret persists an encrypted value a follow-up
FetchSecrets-path ResolveFor returns; re-set rewrites and bumps the version
signal; DeleteSecret removes it (NotFound after); agent-token callers still
PermissionDenied; reserved-prefix set AND delete rejected case-insensitively;
empty value rejected before any row exists. D9 adds: an omitted scope lands at
the CALLER's user coordinate, not `(0, "")`; a non-admin requesting tenant
scope is PermissionDenied on both set and delete; an admin requesting tenant
scope succeeds; and the isolation property that motivated D9 — user A's
user-scoped secret is NOT resolved by user B's agent, while a tenant row IS
resolved by both.

### T6 — D4 removal: server-secret write path

Delete `SetServerSecret`/`DeleteServerSecret` handler bodies AND their two
RPCs plus the four request/response messages
(`proto/compass/v1/compass.proto`, `SecretsService`) — a clean deletion, not
a stub left returning an unimplemented code (D4). Delete
`newServerSecretSetCmd`/`runServerSecretSet`/`serverSecretWireName` and the
set-verb wiring in `newServerSecretCmd` (`go/cmd/compass/server_secret.go`),
and the two `server-secret` procedures in `go/internal/auth/admin_gate.go`.
With the `SetServerSecret`/`DeleteServerSecret` guards gone, `masterKeyName`
loses its last consumer in `secrets_service.go`; move the renamed constant to
`go/internal/store/server_secrets.go` beside `ServerSecretPrefix` /
`GatewayCredentialsPrefix`, where `resolveMasterKey` reads it (T4).
`server-secret list` / `ListServerSecrets` SURVIVE, as does the value-free
`Statuses` probe. The store-layer `DeleteServerSecret` sqlc query is a
DIFFERENT thing (declaration removal) and is untouched.
Delete the write-path tests; keep/extend the `ListServerSecrets` ones.

Deferred deletions land here (F2 re-sequence), now that their last callers are
gone: shrink `secrets.Resolver` to the read pair `Resolve`+`Statuses` (A4), and
delete `SpecResolver.Set`/`Delete`/`setArgs`/`WithCLI` and their tests (the
`SetServerSecret`/`DeleteServerSecret` handlers deleted above were their last
callers).

Regenerate the generated surfaces that still carry the deleted messages: the Go
connect stubs and procedure constants
(`go/gen/compass/v1/compassv1connect/compass.connect.go`), and both TypeScript
packages (`packages/compass-agent/src/gen/compass/v1/compass_pb.ts` and
`packages/compass-client/src/gen/compass/v1/compass_pb.ts`).

Interfaces: none new — this task only removes.

Tests (red first): `server-secret list` renders unchanged; the surviving
Statuses probe still distinguishes set/unset; the deleted RPCs and CLI verb
are gone from the generated surface — proto, the Go connect stubs and procedure
constants (`go/gen/compass/v1/compassv1connect/compass.connect.go`), and both
TS `compass_pb.ts` packages (`packages/compass-agent`, `packages/compass-client`).

### T7 — Docs + ledger

Rewrite the invariant prose this record obsoletes: the `secrets` table
comment (T2 does the SQL side), the `go/internal/secrets/secrets.go`
package doc ("Values live only in the provider…" — now false for user
secrets), and the `SetSecret`/`DeleteSecret` handler docs. Append the
ledger rows (below) to `docs/designs/DECISIONS.md` in the design PR itself
(the ledger flip travels with the record, per `skill://design`).

## Tasks

- [ ] T1 — `go/internal/envelope`: AES-256-GCM seam with the five-field A9
      `UserSecretAAD`, unit-tested, no Postgres.
- [ ] T2 — Store: value columns on `secrets` (NULLABLE in T2, tightened to NOT
      NULL in T5) plus the A9 scope schema (`scope_kind`/`scope_id`, PK
      `(name, scope_kind, scope_id)`, `secrets_scope_shape`), transactional
      upsert/delete, the `SecretRecordsForAgent` collapse query, sqlc regen,
      `ShadowsServerSecretPrefix`.
- [ ] T3 — Additive split: `StoreResolver` lands with the scoped `ResolveFor`
      plus its own `Upsert`/`Remove`, implementing no interface (A9);
      `Resolver` keeps its full shape (the shrink and `SpecResolver`
      write-half deletion defer to T6, F2).
- [ ] T4 — Boot: master-key resolve, `server_key_state` tripwire,
      `buildSecretResolvers` swap, the A9 handler retype (narrow `ResolveFor`
      seam, account-returning hub lookups), and the `COMPASS_` reserved-name
      rename machinery (CHECK, prefix set, `masterKeyName`/`masterKeyCLIName`,
      strip list) that makes `COMPASS_MASTER_KEY` constructible and
      resolvable (F1).
- [ ] T5 — Service cutover: `SetSecret`/`DeleteSecret` on the DB at the
      tenant coordinate, PR #1066 delete semantics folded;
      `DeclareSecret`/`InsertSecret` removed with
      their caller; `value_ciphertext`/`value_nonce` tightened to NOT NULL now
      the upsert is the sole writer.
- [ ] T6 — D4: server-secret write path removed (the two RPCs and their four
      messages DELETED, not stubbed; RPC deletion is D4), CLI `set` verb
      deleted, `list` survives; the deferred `Resolver` shrink + `SpecResolver`
      write-half deletion (from T3); Go connect + both TS `compass_pb.ts`
      surfaces regenerated.
- [ ] T7 — Invariant-prose rewrite + ledger delta.

## Global Constraints

- Go stdlib crypto only (`crypto/aes`, `crypto/cipher`, `crypto/rand`,
  `crypto/sha256`); no third-party crypto dependency.
- AES-256-GCM with a fresh internally-generated random 96-bit nonce per
  encryption; no caller-suppliable nonce anywhere in the API.
- No plaintext secret value, key byte, or version hash is ever logged,
  formatted, or marshaled: unexported key bytes; `ResolvedSecret`'s existing
  redaction posture binds every new type (sibling record D5).
- The reserved-prefix partition holds at every user-path door: admit checks
  byte-exact (`HasServerSecretPrefix`), reject checks case-folding
  (`ShadowsServerSecretPrefix`).
- `secrets.Version` stays SHA-256-of-value, computed at resolve time, never
  stored.
- Store layer sees ciphertext only; crypto lives in `envelope`/`secrets`.
- Public proto changes were ask-first; Matt ruled deletion (D4), so the two
  server-secret RPCs and their four messages are removed in T6.

## Ledger delta

`Ledger-impact: adds DL-350..DL-356, DL-360..DL-363 and DL-368 to docs/designs/DECISIONS.md (Server &
store section); FLIPS the Status cell of DL-328 to Superseded by DL-355.`

Highest existing id verified this session: DL-356 (DL-350..DL-356 landed with
the original record; the A9 scope amendment adds DL-360..DL-362; D8 adds
DL-363; D9 adds DL-368 (364..367 were taken on main while this was in review).

DL-368 supersedes a clause of two rows this same record adds, so those clauses
are amended in place rather than the rows flipped: DL-361's "stay pinned to the
tenant coordinate" and DL-363's store-door enforcement point. Both stay Active
— only those clauses are replaced; the rest of each decision stands.

DL-328's `Status` becomes `Superseded by DL-355 (Matt, 2026-09-11)` — it is the
only existing row this record touches. The full-supersession form loses no
truth: DL-328's key-custody/provisioning half is replaced by DL-355, and its
envelope crypto (AES-256-GCM, the `value_ciphertext`/`value_nonce`/`key_version`
columns, row-binding AAD) is carried forward as a live decision in its own right
by DL-351, so nothing DL-328 decided is lost when the row flips. DL-328's record
header (`compass-gateway-credentials-at-rest-encryption.md`) moves from `Active`
to `Active (key-custody clause superseded by the user-secret store record)`.

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-350 | User-provided secret VALUES move into the Postgres `secrets` table as AES-256-GCM ciphertext columns (`value_ciphertext`, `value_nonce`, `key_version`) on the existing declaration row — one row per secret, single-transaction Set/Delete, the non-atomic declare/set/rollback trio deleted. Values never transit secretspec on the user path again. Grounded in a primary-source survey of comparable OSS projects (Woodpecker CI and Drone CI both persist user-set secrets in their own DB; Drone's external secret plugin interface is read-only `Find` with writes landing in its own store) | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-351 | At-rest crypto is the new `go/internal/envelope` package: AES-256-GCM, single master key (no per-row DEKs), internally-generated random 96-bit nonce per encryption (no caller nonce parameter, so reuse is structurally impossible), AAD = domain label + tenant_id + name + key_version binding each ciphertext to its row and its tenant; the master key is the `COMPASS_MASTER_KEY` boot secret (renamed per DL-355) resolved via secretspec, tripwired through `server_key_state`. Go stdlib crypto only — the first cryptography in the Go tree. Accepted leaks, stated not silent: GCM is length-preserving so a dump reveals each value's byte length, and Go offers no guaranteed key zeroization | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-352 | Key rotation is deferred to a later record; the schema reserves it now via per-row `key_version`, `server_key_state.key_version`, version-carrying AAD, and a key-as-parameter envelope API, so rotation lands as an online row-at-a-time re-encrypt with no schema change. Interim key-incident procedure until that record lands: wipe and re-set every secret, acceptable only under the pre-production single-operator posture — the `server_key_state` tripwire otherwise turns an out-of-band key change into a permanent boot failure | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-353 | `secrets.Resolver` shrinks to the read pair (`Resolve`+`Statuses`); the user path is the concrete `StoreResolver` (DB-backed, no interface for its writes); `SpecResolver`'s Set/Delete/CLI write machinery is deleted outright, since boot secrets become read-only. Every deletion is sequenced AFTER its last caller is gone, so each task compiles standalone | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-354 | PR #1066's reviewed delete semantics fold in transformed: the `HasServerSecretPrefix` (byte-exact admit) vs `ShadowsServerSecretPrefix` (case-folding reject) predicate split is carried to every user-path door; the declaration-first ordering and no-rollback/CodeUnavailable recovery-hint posture are SUBSUMED by the single-row transactional delete, because no two-system half-failure exists to recover from once the value lives in the same row as its declaration | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-355 | Master-key custody is operator-seeded, not compass-written: DL-328's zero-human-step auto-provision into a WRITABLE provider is superseded, because making boot secrets read-only removes the write path it needs (and it was never implemented — `go/internal/envelope` was absent and `server_key_state` had no non-test readers). First-run generation is a standalone `compass` CLI verb that mints a 256-bit key and seeds the configured provider; the nix/devenv seed script invokes that same verb, so a standard deploy needs no explicit step and a hand-rolled one needs a single documented command. Boot only reads, failing closed naming the verb. The key is renamed `GATEWAY_CREDENTIALS_MASTER_KEY` → `COMPASS_MASTER_KEY` and shared with the future `gateway_credentials` store, which MUST adopt its own distinct AAD domain label; a new `COMPASS_` reserved prefix joins the existing two in the CHECK constraint and both prefix predicates, without which the renamed key is undeclarable and therefore unresolvable | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-356 | `SetServerSecret`/`DeleteServerSecret` and their four request/response messages are DELETED from `proto/compass/v1/compass.proto`, together with the `compass server-secret set` verb, the two admin-gate procedures, and the generated Go connect + both TypeScript surfaces — a clean public-API cutover, not a CodeUnimplemented stub. Grounded on no consumer needing a runtime WRITE: server secret VALUES are in fact read at runtime (`newDeclaredSecretResolver` returns a per-call `Resolve` closure; the webhook secret is resolved on every unauthenticated `POST /webhooks/github` before the HMAC check, TTL-cached at `forgeTokenTTL`), which is precisely why provider-side rotation with the secretspec CLI suffices without any RPC. `ListServerSecrets`/`Statuses` survive; the unrelated store-layer `DeleteServerSecret` sqlc query (declaration removal) is untouched | Active (Matt, 2026-09-11) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |
| DL-360 | User secrets are scoped at THREE levels — tenant (0), user (1), agent (2) — via `scope_kind SMALLINT` + `scope_id TEXT` (empty string for a tenant row, the owning `accounts.id` for user/agent rows) with the `secrets` PK widened to `(name, scope_kind, scope_id)`; a tenant row is a real shared VALUE several users resolve, not a declaration placeholder. The scope↔id shape is CHECK-enforced (`secrets_scope_shape`); `scope_id` carries NO FK to `accounts` — a tenant row's `''` can never satisfy one and Postgres has no conditional FK — so user/agent referential integrity is enforced at the store door in the writing transaction | Active (Matt, 2026-09-11) | [user-secret store §A9](compass-user-secret-store.md#a9--scope-model-tenant--user--agent-most-specific-wins) |
| DL-361 | Secret resolution is most-specific-wins — `agent > user > tenant`, ONE value per name in the injected environment — collapsed in SQL (`DISTINCT ON` ordered by `scope_kind DESC`, the numeric encoding being the precedence) so shadowed rows never leave Postgres or get decrypted; `FetchSecrets` resolves per agent account using the identity the runnerhub authz maps (`sessionAccounts`/`containerAccounts`) already hold and previously discarded, the user tier reached through the single `agent_accounts.owner_user_id` FK hop. Scope is an ADDITIONAL filter inside a tenant — RLS tenant isolation stays the outer boundary, never replaced. The existing `SetSecret`/`DeleteSecret` verbs stay pinned to the tenant coordinate, preserving inject-all behavior until a scope wire surface is ruled | Active (Matt, 2026-09-11) | [user-secret store §A9](compass-user-secret-store.md#a9--scope-model-tenant--user--agent-most-specific-wins) |
| DL-362 | The canonical user-secret AAD is the five-field tuple `"compass/user-secret/v1\x00" + tenantID + "\x00" + decimal(scopeKind) + "\x00" + scopeID + "\x00" + name + "\x00" + decimal(keyVersion)` (Go: `UserSecretAAD(tenantID string, scopeKind int16, scopeID, name string, keyVersion int16) []byte`; SMALLINTs rendered `strconv.FormatInt(int64(v), 10)`), every field bound unconditionally (a tenant row binds scopeID as the empty string) with `\x00` separators keeping the encoding injective. Fixed BEFORE any migration ships because the AAD is baked into every ciphertext — a scope field added later would force a re-encrypt of every row. Refines DL-351's four-field AAD clause; DL-351's other rulings stand | Active (Matt, 2026-09-11) | [user-secret store §A9](compass-user-secret-store.md#a9--scope-model-tenant--user--agent-most-specific-wins) |
| DL-363 | Writing a tenant-scoped user-secret row requires an admin (`store.UserRoleAdmin`, `go/internal/store/types.go`), reusing the existing role elevation rather than introducing a permission concept: tenant (0) admin-only, user (1) and agent (2) writable by the owning user or an admin. The check lands in the store door inside the same writing transaction as DL-360's FK-substitute referential checks, so one place enforces both. READS are deliberately asymmetric — a plain user's agent resolves tenant rows, which is the point of a shared tenant value under DL-361; reading a shared secret is the feature, writing one is the privileged act. The wire surface for a scoped write stays undecided (a scope selector on `SetSecretRequest` is a public-proto fork) | Active (Matt, 2026-09-12) | [user-secret store §D8](compass-user-secret-store.md#resolved-decisions) |
| DL-368 | `SetSecretRequest`/`DeleteSecretRequest` gain a `SecretScope scope` selector, and the default is USER scope — an unspecified scope writes `(scope_kind=1, scope_id=caller)`, so a client that omits the field gets the private-by-default coordinate rather than a tenant-wide value every other user's agents resolve. Tenant scope is explicit and requires `store.UserRoleAdmin`, checked at the RPC edge (where `requireUser`'s existing `GetAccount` already holds the role) rather than the store door, which keeps DL-360's scope-shape and referential checks. Agent scope gets no wire surface: agents hold no write door, so an agent-scoped write has no authenticated writer to authorize. SUPERSEDES DL-361's "pinned to the tenant coordinate" clause and DL-363's enforcement point, keeping DL-363's authorization matrix. Corrects a factual error in D8: no admin check existed on the user-secret write path — `classifyProcedure` returns `authenticatedOpen` for both verbs — so T5 adds the gate rather than documenting one. Behavior change stated not silent: today's tenant-wide rows become per-user on re-set | Active (Matt, 2026-09-12) | [user-secret store](compass-user-secret-store.md#resolved-decisions) |

## Resolved decisions

Every load-bearing question below was answered by Matt on 2026-09-11 and is
folded into the sections above. Recorded here because each changes a decision
the draft argued for, and because D1 supersedes part of a frozen record.

- **D1 — DL-328's key-custody clause is SUPERSEDED (was OQ-1).** DL-328
  specifies the master key is "auto-provisioned zero-human-step into an
  operator-chosen WRITABLE SecretSpec provider" and rotated through
  `SetServerSecret` / the `compass server-secret` CLI. D4 (boot secrets
  read-only) removes the write path all three of those need, so the clause is
  unimplementable as frozen. This record supersedes the custody and
  provisioning half of DL-328; DL-328's envelope crypto (AES-256-GCM,
  `value_ciphertext`/`value_nonce`/`key_version`, row-binding AAD) is kept and
  reused verbatim, carried forward as DL-351, a live decision in its own right.
  Nothing regresses: DL-328's provisioner was never implemented
  (`go/internal/envelope` absent; `server_key_state` has no non-test readers
  outside generated `models.go`). Because the crypto survives as DL-351, the
  ledger flips DL-328's `Status` cell to `Superseded by DL-355` outright — there
  is no partial-status form, and nothing DL-328 decided is lost by the full
  flip.
- **D2 — First-run key generation is a CLI verb, invoked by the seed script.**
  The key must exist before the server boots, so the server cannot mint it.
  One implementation — a standalone `compass` verb that generates a 256-bit
  key and seeds it into the configured provider (or prints the exact command)
  — and the nix/devenv seed script calls that same verb. Standard deploys get
  it with no explicit step; a hand-rolled deploy gets one documented command.
  Boot still only ever reads, failing closed naming the verb when the key is
  absent.
- **D3 — One master key, renamed to a neutral name (was OQ-3).** Shared with
  the future `gateway_credentials` store, AAD domain labels keeping the
  ciphertext domains disjoint. Because it no longer belongs to the gateway,
  it is renamed from `GATEWAY_CREDENTIALS_MASTER_KEY` to a neutral name
  (`COMPASS_MASTER_KEY`) rather than kept as a misnomer. The rename is
  operator-visible and lands with the reserved-name guard, the tripwire, and
  the seed verb in one change.
- **D4 — `SetServerSecret` / `DeleteServerSecret` and their four messages are
  DELETED (was OQ-2).** The draft recommended keeping them as
  `CodeUnimplemented` stubs; Matt ruled for the clean cutover, on the
  grounding that **no server secret needs a runtime WRITE**. The earlier draft
  claimed every server secret is boot-time and the sole post-boot resolver call
  is the value-free `Statuses`; that is FALSE (verified). Several server secrets
  are RE-READ at runtime through the resolver: `newDeclaredSecretResolver`
  (`go/server/serve.go`) returns a closure calling `Resolve` per invocation; the
  webhook-secret resolver runs on EVERY request to the unauthenticated
  `POST /webhooks/github`, BEFORE the HMAC check, TTL-cached at
  `forgeTokenTTL = 5 * time.Minute`; the App-key resolver reads the PEM on every
  token mint. What none of them needs is a runtime WRITE — the runtime readers
  all go through the resolver, which is exactly why provider-side rotation
  suffices without the RPCs: the operator writes the provider with the secretspec
  CLI and the lazy re-resolve picks up the new value within the TTL. `Statuses`
  survives as the sole post-boot call, and `ListServerSecrets` with it.
  Callers checked before removal: only `go/cmd/compass/server_secret.go` (its
  `set` verb, itself deleted), that file's tests, and the procedure list in
  `go/internal/auth/admin_gate.go`. **The store-layer `DeleteServerSecret`
  sqlc query is a DIFFERENT thing** (declaration removal, backing
  `DeleteServerSecretDeclaration`) and is untouched.
- **D5 — Migration shape and cutover import (were OQ-4, OQ-5).** Both
  non-load-bearing deferrals stand as written: amend the squashed
  `0001_init.sql` in place, and no import tool (the registry is names-only, so
  there is nothing in the DB to migrate; the operator re-sets values by hand
  under the pre-production wipe posture). A one-shot import can be added to T5
  if cutover volume demands it.
- **D6 — `tenant_id` is bound into the AAD from the first row (red-team F4).**
  The adversarial pass found the AAD omitted the tenant, masked today by the
  `secrets` table's pre-existing `name TEXT PRIMARY KEY` (no `tenant_id` in the
  key) despite the table sitting in the RLS `tenant_tables` set. Matt ruled to
  include it now. It buys nothing today and costs one field; the alternative —
  declaring user secrets single-tenant-per-deployment — would mean
  re-encrypting every row if the managed plane ever shares this table. The
  underlying PK wart is NAMED but deliberately NOT fixed here: correcting it to
  `(tenant_id, name)` would pull a pre-existing schema defect and the
  declarations path into this change. Until it is fixed, the `ON CONFLICT
  (name)` upsert keeps the limitation `TestForgeAuthoredTwoTenantsSameCoordinate`
  documents — tenant B cannot declare a name tenant A holds.
- **D7 — Secrets are scoped at three levels: tenant / user / agent (Matt,
  2026-09-11; amendment A9).** Matt ruled the flat per-tenant namespace this
  record froze with is replaced by three scope tiers with most-specific-wins
  resolution (`agent > user > tenant`, one value per name). Normative detail
  in A9: the `(name, scope_kind, scope_id)` PK, the `secrets_scope_shape`
  CHECK, no FK on `scope_id`, the SQL `DISTINCT ON` collapse, the
  account-returning runnerhub lookups, and the widened five-field
  `UserSecretAAD` — fixed BEFORE any migration ships, because a scope field
  added later would force re-encrypting every row. This widens D6's AAD from
  four fields to five and the upsert's conflict target from `(name)` to the
  full PK; D6's tenant-wart caveat stands — `tenant_id` is still not in the
  key, so `TestForgeAuthoredTwoTenantsSameCoordinate`'s cross-tenant
  limitation persists at the widened coordinate. Ledger: DL-360..DL-362.
- **D8 — Writing a tenant-scoped row requires an admin; reads do not (Matt,
  2026-09-12, was OQ-2's first half).** A plain user may NOT write a
  tenant-scoped row. The check reuses the existing `store.UserRoleAdmin`
  elevation (`go/internal/store/types.go`, with `UserRoleMember` the
  least-privilege default and `adminByHandle` already refusing non-admins) —
  no new permission concept. The matrix: tenant (0) admin-only; user (1) and
  agent (2) writable by the owning user or an admin. It lands in the store
  door, in the same writing transaction as A9's FK-substitute referential
  checks, so one place enforces both. READS stay deliberately asymmetric: a
  plain user's agent resolves tenant rows, which is the entire point of a
  shared tenant value under DL-361 — reading a shared secret is the feature,
  writing one is the privileged act. D9 supersedes D8's ENFORCEMENT POINT (the
  role check lands at the RPC edge, not the store door) while keeping its
  matrix intact. This also bears on T2's two `(0, "")` placeholders in
  `go/server/secrets_service.go`. D8 originally claimed they were "already
  admin-gated at the door" — that is FALSE, corrected by D9: no admin check
  exists on the user-secret write path. T5 adds one. Ledger: DL-363.
- **D9 — `SetSecret`/`DeleteSecret` carry an explicit scope selector; the
  default is USER scope (Matt, 2026-09-12, closes OQ "what wire surface
  carries a scoped write").** Two corrections to D8 land with this ruling.

  First, a **factual error in D8**: it asserts the two `(0, "")` coordinates in
  `go/server/secrets_service.go` "are already admin-gated at the door, so T5
  verifies and documents that gate rather than adding one." They are NOT.
  `classifyProcedure` (`go/internal/auth/admin_gate.go`) returns
  `authenticatedOpen{}` for `SetSecretProcedure` and `DeleteSecretProcedure`,
  and `requireUser` checks only that the caller is not an agent — it never
  reads `UserRole`. No admin check exists anywhere on the user-secret write
  path. T5 ADDS the gate; it does not document an existing one.

  Second, D8 + the pinned tenant coordinate would have made `SetSecret`
  **admin-only in practice**, silently removing a shipped user-facing verb from
  every ordinary user: `CreateUser` always seeds `UserRoleMember`
  (`go/internal/store/accounts.go`), the only `UserRoleAdmin` account is the
  bootstrap one (`adminByHandle`), and NO role-promotion path exists — the
  proto calls elevation "a separate admin-authorized path (deferred)"
  (`proto/compass/v1/comms.proto`, `CreateUser`). So an admin-only tenant write
  plus a tenant-pinned verb equals a verb no user can reach and no admin can
  grant.

  The ruling: **per-user credentials are the intended behavior** — a
  user-scoped credential must NOT be visible to another user's agents.
  `SetSecretRequest`/`DeleteSecretRequest` gain a `SecretScope scope` field
  (proto enum mirroring `store.SecretScope*`), and the handler resolves the
  coordinate from it:

  - `SECRET_SCOPE_UNSPECIFIED` (0) and `SECRET_SCOPE_USER` both write
    `(scope_kind=1, scope_id=caller)`. The unspecified default is USER, not
    tenant, so an old client that omits the field gets the private-by-default
    coordinate rather than silently writing a value every other user's agents
    resolve. This is the one deliberate behavior change: today's rows are
    tenant-wide, and a pre-existing tenant row keeps resolving (it still wins
    nothing — user scope outranks it under DL-361) until re-set.
  - `SECRET_SCOPE_TENANT` writes `(0, "")` and **requires `UserRoleAdmin`**,
    per D8's matrix. A non-admin requesting tenant scope is
    `CodePermissionDenied`.

  The admin check lives at the RPC edge (it needs the caller's role, which the
  handler already fetches in `requireUser` via `GetAccount`), while the store
  door keeps D8's scope-shape and referential checks. This splits D8's "one
  place enforces both" — the role is an identity property known at the edge,
  not a row property, and re-reading the account inside the write transaction
  would add a query to every write to re-derive what the caller already holds.
  `SECRET_SCOPE_AGENT` gets no wire surface here: agents hold no write door
  (`requireUser` rejects agent tokens), so an agent-scoped write has no
  authenticated writer to authorize. Ledger: DL-368.

## Open questions

The 2026-09-11 scope ruling makes the following genuinely ambiguous. Each
needs a Matt ruling; none is silently decided by this amendment.

- **What `declared_by` means beside `scope_id`.** `declared_by` (FK to
  `accounts`, `0001_init.sql`) was the whole ownership story when a name had
  one row. For a user-scoped row the declarer is usually the scope owner; for
  an agent-scoped row it cannot be the agent (agents hold no write door
  today); for a tenant row there is no single owner. Is `declared_by` pure
  provenance (whoever wrote the row), or does it carry authorization weight
  (only the declarer may rewrite/delete)? This record treats it as
  provenance only.
- ~~**What wire surface carries a scoped write.**~~ RESOLVED by D9
  (2026-09-12): a `SecretScope scope` field on
  `SetSecretRequest`/`DeleteSecretRequest`, defaulting to USER scope, with
  tenant scope admin-gated.
- **Lifecycle of scoped rows when their account goes away.** `scope_id`
  carries no FK (A9), so deleting an agent account neither cascades nor
  RESTRICTs its agent-scoped secret rows — they linger as unreachable
  ciphertext. Options: a door-side sweep on account deletion, a trigger, or
  accepting lingering rows under the pre-production wipe posture. The same
  question holds for user deletion and user-scoped rows (`declared_by`'s
  ON DELETE RESTRICT already blocks deleting an account that DECLARED rows,
  which may mask this in practice).
- **Whether `DeleteSecret` needs scope addressing before the write surface
  lands.** With writes pinned to the tenant coordinate the pinned delete is
  consistent; but the moment any user/agent row exists (e.g. operator-seeded),
  the name-only wire delete cannot address it.
