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

**AAD binds a ciphertext to its row.** Additional authenticated data is
`"compass/user-secret/v1\x00" || tenant_id || "\x00" || name || "\x00" ||
decimal(key_version)`.
The domain-separation label means a `gateway_credentials` ciphertext (same
key family, future table) can never decrypt as a user secret; the name means
swapping two rows' ciphertext columns fails authentication instead of
returning the wrong secret under the right name; the key_version binds the
row's claim about which key generation encrypted it. `delivery`/`kind`
routing columns stay OUTSIDE the AAD: they are mutable metadata a re-declare
may legitimately change without re-encrypting, and tampering with them
misroutes but never discloses.

**`tenant_id` is in the AAD from the first row, by decision (Matt,
2026-09-11).** It is not load-bearing today and is deliberately included
anyway. The `secrets` table is `name TEXT PRIMARY KEY` with no `tenant_id` in
the key (`go/internal/store/migrations/0001_init.sql`) even though it sits in
the RLS `tenant_tables` set (`go/internal/store/rls_pgtest_test.go`) — a
pre-existing wart this record does NOT fix. Two consequences follow from it.
First, the `ON CONFLICT (name)` upsert inherits the failure
`TestForgeAuthoredTwoTenantsSameCoordinate` already documents: tenant B's
write hits tenant A's RLS-invisible row and Postgres rejects it, so tenant B
cannot declare a name tenant A holds. Second, if that PK is later corrected to
`(tenant_id, name)`, two tenants' same-named rows would otherwise carry
IDENTICAL AAD under the shared master key, and a cross-TENANT ciphertext swap
would authenticate cleanly — exactly the row-portability the AAD exists to
kill. Binding the tenant now costs one field; retrofitting it later means
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
defense-in-depth against a writer, not a freshness guarantee. Renames cannot
occur: `name` is the primary key, `SetSecret` upserts by name, and no rename
verb exists, so the name-in-AAD binding is stable for a row's lifetime.

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
(`go/internal/runnerhub/handler.go`) already consumes only `Resolve`, so the
narrowed interface fits it unchanged.

The user side gets a CONCRETE type, not a second implementation of a shared
write interface — there is exactly one DB-backed store and no second backing
to swap in, so an interface would be weightless:

```go
// go/internal/secrets/storeresolver.go (new)
type StoreResolver struct { /* st *store.Store; key envelope.Key; keyVersion int16 */ }

func NewStoreResolver(st *store.Store, key envelope.Key, keyVersion int16) *StoreResolver
// Resolve decrypts every row and maps delivery/kind via the existing
// deliveryFromStore/kindFromStore edge maps; satisfies Resolver.Resolve.
func (r *StoreResolver) Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error)
// Statuses satisfies Resolver.Statuses: every stored row is set by
// construction (NOT NULL value), so it maps rows to IsSet=true without
// decrypting.
func (r *StoreResolver) Statuses(ctx context.Context, reason string) ([]SecretStatus, error)
// Upsert validates, encrypts, and transactionally upserts declaration+value.
// Named Upsert (not Set) so StoreResolver does not accidentally satisfy the
// deleted write half of the old Resolver interface — the read pair is all it
// shares with Resolver.
func (r *StoreResolver) Upsert(ctx context.Context, actor store.AccountID, name, value string, delivery DeliveryKind, kind SecretKind, provider, host string) error
// Remove transactionally removes declaration+value; ErrNotFound maps out.
func (r *StoreResolver) Remove(ctx context.Context, actor store.AccountID, name string) error
```

Crypto stays in the `secrets`/`envelope` layer; `store` sees only
ciphertext+nonce bytes (it stays the leaf the package doc requires: "store
stays a leaf and the two evolve independently").

`StoreResolver.Statuses` will have no caller: `ListSecrets` computes `is_set`
from the store directly (every user row is set by construction). It exists only
to satisfy the `Resolver` interface `Handler.FetchSecrets` is typed against, so
a reviewer need not hunt for its caller.

**The Version contract is preserved, with no stored hash.** `Resolve` keeps
returning `ResolvedSecret` with `Version: Version(value)` — the SHA-256
content hash (`Version`, `go/internal/secrets/secrets.go`) — computed AFTER
decrypt, per resolve. Storing the hash in a column would recreate exactly the
offline confirmation oracle the `Version` doc comment warns about ("the hash
cannot serve as an offline confirmation oracle for a low-entropy secret") —
in the DB dump this record exists to make worthless. One SHA-256 per secret
per fetch is noise next to the AES decrypt beside it. T6 rotation-diff
semantics are unchanged: a same-value re-set still hashes identically.

### A5 — Re-pointing the callers

- **`Handler.FetchSecrets`** (`go/internal/runnerhub/handler.go`): wiring-only
  change. `buildSecretResolvers` (`go/server/serve.go`) returns the
  `*StoreResolver` as the container-side value; the handler's
  `h.resolver.Resolve(ctx, "runner fetch")` call and its
  CodeFailedPrecondition nil-resolver posture are byte-for-byte unchanged.
  The hot-read FFI motivation is dissolved: the hot path is now one indexed
  SELECT plus in-process AES, no subprocess and no FFI.
- **`SetSecret`** (`go/server/secrets_service.go`): the declare-then-Set
  two-system flow (`s.store.DeclareSecret` + `s.resolver.Set`) with its
  ErrConflict re-set branch and rollback-on-fresh-write-failure collapses
  into one `StoreResolver.Upsert` upsert transaction. Re-set of an existing name
  stays a value rewrite (UPSERT). The user-only gate (`requireUser`), empty
  value rejection, `secretRoutingFromProto`, and `bumpSecretsVersion` are
  unchanged.
- **`DeleteSecret`** (`go/server/secrets_service.go`): one transaction
  deleting the row (declaration and value are the same row). PR #1066's
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

// UserSecretAAD builds the canonical user-secret AAD:
// "compass/user-secret/v1\x00" + tenantID + "\x00" + name + "\x00" +
// strconv.FormatInt(int64(keyVersion), 10). tenantID is bound as the empty
// string when a row has no tenant scope, so the AAD's shape never varies.
func UserSecretAAD(tenantID, name string, keyVersion int16) []byte
```

Tests (red first): round-trip; distinct nonces/ciphertexts across two
encrypts of one plaintext; tamper on each of ciphertext/nonce/aad fails
with ErrDecrypt; wrong key fails; `String`/`GoString`/`json.Marshal` of Key
leak nothing; UserSecretAAD is injective across (name, version) pairs that
concatenate ambiguously (the `\x00` separator test).

### T2 — Store: value columns + upsert/read queries + `ShadowsServerSecretPrefix`

Amend `0001_init.sql` (`secrets` table + comment rewrite, A1). The two value
columns `value_ciphertext`/`value_nonce` land NULLABLE here (A1 transitional
posture): T2 keeps the value-free `InsertSecret`/`DeclareSecret` path live for
its T5 caller, and that path writes no value columns, so an unconditional NOT
NULL now would fail every live `SetSecret` and the existing DeclareSecret
pgtests at runtime. T5 tightens the two columns to NOT NULL once the upsert is
the only writer. Extend `go/internal/store/queries/secrets.sql` ADDITIVELY: add
`UpsertSecret :exec` (INSERT … ON CONFLICT (name) DO UPDATE on
value/nonce/key_version/delivery/kind/provider/host/updated_at) and
`SecretRecords :many` (rows with the value columns). `InsertSecret`,
`DeclaredSecrets`, and the `DeclareSecret` store method STAY in T2 — their
caller is the T5 `SetSecret` handler, so deleting them here would break code
that still compiles against them; they are removed in T5 once that caller
migrates (F2 re-sequence). `DeleteSecret :execrows` keeps its shape. Regenerate
sqlc. Add the ported case-folding predicate.

Interfaces:

```go
// go/internal/store/secrets.go

// SecretRecord is SecretDeclaration plus the at-rest value columns. It
// carries CIPHERTEXT only — store never sees plaintext.
type SecretRecord struct {
    SecretDeclaration
    ValueCiphertext []byte
    ValueNonce      []byte
    KeyVersion      int16
}

// UpsertSecret validates name/kind-routing at the door (as DeclareSecret
// did) and transactionally writes declaration+value in one row. A fresh
// name inserts; an existing name is a value rewrite. Reserved-prefix names
// (ShadowsServerSecretPrefix) are ErrInvalidArgument.
func (s *Store) UpsertSecret(ctx context.Context, actor AccountID, name string, delivery SecretDelivery, kind SecretKind, provider, host string, ciphertext, nonce []byte, keyVersion int16) error

// DeleteSecretDeclaration keeps its signature; deleting the row now deletes
// the value with it. Absent name stays ErrNotFound.
func (s *Store) DeleteSecretDeclaration(ctx context.Context, actor AccountID, name string) error

// SecretRecords returns every row with its ciphertext, name-ordered — the
// StoreResolver read. DeclaredSecrets (names-only view) survives for the
// `declarations` interface the SERVER SpecResolver still consumes.
func (s *Store) SecretRecords(ctx context.Context) ([]SecretRecord, error)

// go/internal/store/server_secrets.go

// ShadowsServerSecretPrefix reports whether name case-folds onto a reserved
// server-secret prefix (the PR #1066 reject-wide predicate; admit stays the
// byte-exact HasServerSecretPrefix).
func ShadowsServerSecretPrefix(name string) bool
```

Tests (red first, pgtest): upsert-then-read round-trips ciphertext bytes;
re-upsert rewrites; delete removes row (ErrNotFound after); raw SELECT of
`value_ciphertext` for a known plaintext never contains the plaintext bytes
(asserted at the store layer with a real envelope-encrypted value);
ShadowsServerSecretPrefix case variants. `DeclareSecret`/`InsertSecret` stay
live through T2 (removed in T5 with their caller), so no retirement test here.

### T3 — `StoreResolver` + interface split

Land `StoreResolver` (signatures in A4) ADDITIVELY: it implements the two
`Resolver` read methods (`Resolve`, `Statuses`) plus its own concrete
`Upsert`/`Remove` writes, whose signatures deliberately do NOT match the old
`Resolver.Set`/`Delete` — so `StoreResolver` satisfies exactly the read pair and
nothing of the write half. `secrets.Resolver` KEEPS its full four-method shape
in T3 (`Resolve`, `Statuses`, `Set`, `Delete`) so every existing caller still
compiles, and `SpecResolver` keeps its write half. The shrink to the read pair
and the deletion of `SpecResolver.Set`/`Delete`/`setArgs`/`WithCLI` are deferred
to T6 (F2 re-sequence). `DeclareServerSecret` and the server `declarations`
view are untouched.

Tests (red first): `StoreResolver.Resolve` returns decrypted values with
`Version == secrets.Version(value)` and mapped delivery/kind; a row whose
ciphertext was swapped with another row's fails Resolve with an error naming
the row, not a wrong value; `Statuses` reports IsSet=true per row without
decrypt (assert by corrupting a ciphertext and observing Statuses still
succeed); `Upsert` round-trips through a real store; compile-time: `var _
Resolver = (*SpecResolver)(nil)` (the spec side still satisfies the wide
interface in T3). No `var _ Resolver = (*StoreResolver)(nil)` assertion — the
store resolver is not the wide interface, only the read pair, so that assertion
would fail to compile.

### T4 — Boot wiring: master-key resolve + tripwire + resolver swap + reserved-name rename

In `go/server/serve.go`: resolve the master key through the server
resolver at startup, build `envelope.Key`, verify/write `server_key_state`
(new sqlc queries `GetServerKeyState :one` / `UpsertServerKeyState :exec`
in a new `queries/server_key_state.sql`), and change `buildSecretResolvers`
to return the `StoreResolver` on the container side.

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

Rewrite the user handlers in `go/server/secrets_service.go` onto
`StoreResolver` (A5): the `SetSecret` handler calls `StoreResolver.Upsert` (one
upsert, rollback machinery deleted); the `DeleteSecret` handler calls
`StoreResolver.Remove`, with the reserved-prefix reject
(`ShadowsServerSecretPrefix` → CodeInvalidArgument) ahead of any store
call. `secretsService.resolver` field becomes `*secrets.StoreResolver`;
`serverResolver` stays `secrets.Resolver`.

Once `SetSecret` no longer calls `s.store.DeclareSecret`, remove the now-unused
`DeclareSecret` store method and its `InsertSecret` query (kept live through
T2–T4 per the F2 re-sequence). With the value-free insert gone and the upsert —
which always writes both value columns — the sole writer, amend `0001_init.sql`
in the same task to tighten `value_ciphertext`/`value_nonce` to `NOT NULL` (the
final-schema constraint A1 defers out of T2). Regenerate sqlc for the tightened
columns.

Interfaces:

```go
func newSecretsService(st *store.Store, user *secrets.StoreResolver, serverResolver secrets.Resolver, signaler secretsSignaler) *secretsService
```

Tests (red first, pgtest — fixture swaps the user fake for the real
StoreResolver, A8): SetSecret persists an encrypted value a follow-up
FetchSecrets-path Resolve returns; re-set rewrites and bumps the version
signal; DeleteSecret removes it (NotFound after); agent-token callers still
PermissionDenied; reserved-prefix set AND delete rejected case-insensitively;
empty value rejected before any row exists.

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

- [ ] T1 — `go/internal/envelope`: AES-256-GCM seam, unit-tested, no Postgres.
- [ ] T2 — Store: value columns on `secrets` (NULLABLE in T2, tightened to NOT
      NULL in T5), transactional upsert/delete, sqlc regen,
      `ShadowsServerSecretPrefix`.
- [ ] T3 — Additive interface split: `StoreResolver` lands implementing the two
      `Resolver` read methods plus its own `Upsert`/`Remove`; `Resolver` keeps
      its full shape (the shrink and `SpecResolver` write-half deletion defer to
      T6, F2).
- [ ] T4 — Boot: master-key resolve, `server_key_state` tripwire,
      `buildSecretResolvers` swap, and the `COMPASS_` reserved-name rename
      machinery (CHECK, prefix set, `masterKeyName`/`masterKeyCLIName`, strip
      list) that makes `COMPASS_MASTER_KEY` constructible and resolvable (F1).
- [ ] T5 — Service cutover: `SetSecret`/`DeleteSecret` on the DB, PR #1066
      delete semantics folded; `DeclareSecret`/`InsertSecret` removed with
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
- pgtest suites run under `COMPASS_TEST_USE_CONTAINER=1`; fail-closed guards
  ordered after the store fixture (A8).
- `rule://red-green-testing`, `rule://no-inert-gating` (T6 removes the write
  path in the same PR that makes it unreachable — no dormant flag),
  `rule://go-no-fmt-print-logging`.
- Public proto changes were ask-first; Matt ruled deletion (D4), so the two
  server-secret RPCs and their four messages are removed in T6.

## Ledger delta

`Ledger-impact: adds DL-350..DL-356 to docs/designs/DECISIONS.md (Server &
store section); FLIPS the Status cell of DL-328 to Superseded by DL-355.`

Highest existing id verified this session: DL-349.

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
| DL-350 | User-provided secret VALUES move into the Postgres `secrets` table as AES-256-GCM ciphertext columns (`value_ciphertext`, `value_nonce`, `key_version`) on the existing declaration row — one row per secret, single-transaction Set/Delete, the non-atomic declare/set/rollback trio deleted. Values never transit secretspec on the user path again. Grounded in a primary-source survey of comparable OSS projects (Woodpecker CI and Drone CI both persist user-set secrets in their own DB; Drone's external secret plugin interface is read-only `Find` with writes landing in its own store) | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-351 | At-rest crypto is the new `go/internal/envelope` package: AES-256-GCM, single master key (no per-row DEKs), internally-generated random 96-bit nonce per encryption (no caller nonce parameter, so reuse is structurally impossible), AAD = domain label + tenant_id + name + key_version binding each ciphertext to its row and its tenant; the master key is the `COMPASS_MASTER_KEY` boot secret (renamed per DL-355) resolved via secretspec, tripwired through `server_key_state`. Go stdlib crypto only — the first cryptography in the Go tree. Accepted leaks, stated not silent: GCM is length-preserving so a dump reveals each value's byte length, and Go offers no guaranteed key zeroization | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-352 | Key rotation is deferred to a later record; the schema reserves it now via per-row `key_version`, `server_key_state.key_version`, version-carrying AAD, and a key-as-parameter envelope API, so rotation lands as an online row-at-a-time re-encrypt with no schema change. Interim key-incident procedure until that record lands: wipe and re-set every secret, acceptable only under the pre-production single-operator posture — the `server_key_state` tripwire otherwise turns an out-of-band key change into a permanent boot failure | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-353 | `secrets.Resolver` shrinks to the read pair (`Resolve`+`Statuses`); the user path is the concrete `StoreResolver` (DB-backed, no interface for its writes); `SpecResolver`'s Set/Delete/CLI write machinery is deleted outright, since boot secrets become read-only. Every deletion is sequenced AFTER its last caller is gone, so each task compiles standalone | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-354 | PR #1066's reviewed delete semantics fold in transformed: the `HasServerSecretPrefix` (byte-exact admit) vs `ShadowsServerSecretPrefix` (case-folding reject) predicate split is carried to every user-path door; the declaration-first ordering and no-rollback/CodeUnavailable recovery-hint posture are SUBSUMED by the single-row transactional delete, because no two-system half-failure exists to recover from once the value lives in the same row as its declaration | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-355 | Master-key custody is operator-seeded, not compass-written: DL-328's zero-human-step auto-provision into a WRITABLE provider is superseded, because making boot secrets read-only removes the write path it needs (and it was never implemented — `go/internal/envelope` was absent and `server_key_state` had no non-test readers). First-run generation is a standalone `compass` CLI verb that mints a 256-bit key and seeds the configured provider; the nix/devenv seed script invokes that same verb, so a standard deploy needs no explicit step and a hand-rolled one needs a single documented command. Boot only reads, failing closed naming the verb. The key is renamed `GATEWAY_CREDENTIALS_MASTER_KEY` → `COMPASS_MASTER_KEY` and shared with the future `gateway_credentials` store, which MUST adopt its own distinct AAD domain label; a new `COMPASS_` reserved prefix joins the existing two in the CHECK constraint and both prefix predicates, without which the renamed key is undeclarable and therefore unresolvable | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |
| DL-356 | `SetServerSecret`/`DeleteServerSecret` and their four request/response messages are DELETED from `proto/compass/v1/compass.proto`, together with the `compass server-secret set` verb, the two admin-gate procedures, and the generated Go connect + both TypeScript surfaces — a clean public-API cutover, not a CodeUnimplemented stub. Grounded on no consumer needing a runtime WRITE: server secret VALUES are in fact read at runtime (`newDeclaredSecretResolver` returns a per-call `Resolve` closure; the webhook secret is resolved on every unauthenticated `POST /webhooks/github` before the HMAC check, TTL-cached at `forgeTokenTTL`), which is precisely why provider-side rotation with the secretspec CLI suffices without any RPC. `ListServerSecrets`/`Statuses` survive; the unrelated store-layer `DeleteServerSecret` sqlc query (declaration removal) is untouched | Active (Matt, 2026-09-11) | [user-secret store](server/compass-user-secret-store.md#resolved-decisions) |

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
