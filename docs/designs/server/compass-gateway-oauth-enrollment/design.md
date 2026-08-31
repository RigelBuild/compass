# User OAuth login / credential enrollment for the Compass LLM gateway (RIG-3050)

Status: Draft

Tracking: RIG-3050
Owner: compass-obs (design) → compass-server / gateway-TS / compass-ui (impl, per task)

## Problem / Intent

The frozen RIG-1715 gateway record
(`docs/designs/platform/compass-server-llm-gateway/design.md`) designs the
`gateway_credentials` value store, OAuth refresh, rotation, pools, and the
gateway READING credentials at request time — but assumes credentials are
already in the store (its T2 notes only that "A `compass.proto` change is
likely (today `SetSecretRequest` carries only kind + provider ...)",
`compass-server-llm-gateway/design.md:789-791`). Nothing designs how a user
authenticates their provider account INTO that store. OMP has the interactive
OAuth login flow, but it is local-CLI shaped: `OAuthCallbackFlow.login()`
holds the PKCE verifier and CSRF state in process memory across a single
awaited call and receives the provider redirect on a loopback `Bun.serve`
(`forks/oh-my-pi/packages/ai/src/registry/oauth/callback-server.ts:139-180,282-290`).
Compass is a server/web product: initiate and callback are two separate HTTP
requests, possibly to different Server instances — so the in-memory
verifier/state handoff must become server-persisted per-attempt state; the
loopback listener survives only where a locally-running client exists (the
native-app loopback method), with paste-code carrying the pure-web path.
This record designs that enrollment half: the UI connect-provider surface,
the initiate/complete RPCs, the completion methods (API-key paste,
paste-code, native-app loopback; device-code deferred), the write path into
the RIG-1715 `gateway_credentials` store — and the consumption-eligibility
policy Matt ruled on top: the eligibility matrix is per provider ToS tier
(Approach §Consumption eligibility). The OSS core's enrollment offers
every tier — the core IS the self-hosted product
(`docs/concepts/self-host-and-managed.md:10-15`) — and enforces the
matrix through an extensible `EnrollmentPolicy` seam whose default is
allow-all; the managed product denies restricted-tier subscription
enrollment by injecting a stricter policy out of tree
(§The EnrollmentPolicy seam, D-3).

## Approach

### The shape in one paragraph

Enrollment is split across the three tiers by what each already owns. The
**UI** (compass-ui) gets a "Providers" settings surface driving a new
`compass.v1` enrollment service. The **Server** (Go) owns all durable state:
a new `gateway_enrollment_attempts` table (the server-persisted replacement
for OMP's in-process verifier/state), the write path into the RIG-1715
`gateway_credentials` store, and the compass.v1 RPCs. The **gateway/TS tier**
owns the provider protocol: a narrow internal HTTP surface — authenticated
by a dedicated server→gateway enroll token (§the gateway/TS internal enroll
surface) — wrapping the fork's per-provider `generateAuthUrl`/`exchangeToken`
logic — the fork is the source of truth for provider OAuth quirks and updates
fast, so compass calls it rather than re-implementing it in Go (the same cost
argument that ratified RIG-1715 A1). The v1 completion mechanisms are
**paste-code** — on the web the user pastes the FULL redirect URL, the
easiest paste; the fork already parses it (`parseCallbackInput`,
`callback-server.ts:410-438`) — plus API-key paste, and **native-app local
loopback** for a locally-running client (the seamless path, D-1). A
Compass-hosted HTTPS callback is explicitly not pursued (Alternatives
considered §Compass-registered OAuth apps). Which credential kinds
enrollment offers at all is governed by the consumption-eligibility
matrix, enforced through the `EnrollmentPolicy` seam (§Consumption
eligibility, §The EnrollmentPolicy seam, D-3).

### Consumption eligibility (provider ToS tier) — D-3

Which credential kinds enrollment offers — and which the managed product
will hold at all — is gated by **(credential kind) × (provider ToS
tier)**. Anthropic's Feb 2026 legal/compliance policy
(<https://code.claude.com/docs/en/legal-and-compliance>) restricts consumer
Pro/Max OAuth to Claude Code + Claude.ai only and explicitly bars
third-party developers from offering "Claude.ai login" or routing
consumer-plan credentials on behalf of their users; OpenAI's posture is
less actively enforced but carries the same restricted-tier risk. So the
managed product does not touch those subscriptions. Permissive-tier
providers (e.g. GLM Coding, Kimi Code — providers whose growth strategy
welcomes third-party/managed use) are offered on managed server-side,
exactly as API keys are. The matrix (the POLICY — the enforcement
mechanism is §The EnrollmentPolicy seam):

| Credential kind | Managed | Self-host |
| --- | --- | --- |
| API key (any provider — commercial terms) | server-side (RIG-1715 as frozen) | yes |
| Subscription OAuth — permissive tier (e.g. GLM Coding, Kimi Code) | server-side | yes |
| Subscription OAuth — restricted tier (Anthropic, OpenAI) | NOT offered | yes (user's own box = their IP, their risk — the OMP posture) |
| Aggregators (OpenRouter etc. — the cheap-OSS price-sensitive path) | server-side | yes |

**This is a policy gate on top of the frozen RIG-1715 contract, NOT a
supersession.** RIG-1715's server-side consumption model composes
unchanged: the gateway remains "the only runtime holder of upstream
provider credentials at request time" (frozen §Credential storage and
rotation, `compass-server-llm-gateway/design.md:312-337`) and "OAuth
refresh stays gateway-side, as it already is" (`design.md:367`) — for
every credential a deployment is permitted to hold. The OSS core's own
enrollment permits every tier: every deployment of this repo's code is
self-host by definition ("the only product whose code is in this repo",
`docs/concepts/self-host-and-managed.md:10-15`) and the matrix's
Self-host column is all-yes, so the core never itself refuses a tier.
The Managed column is enforced by the managed control plane injecting a
stricter `EnrollmentPolicy` (deny restricted-tier subscription OAuth)
through the seam the core ships (§The EnrollmentPolicy seam), designed
out of tree in the private monorepo. No RIG-1715 ledger rows change;
this record adds decision rows only.

**Restricted tier = self-host only.** The user's own box is the server:
their credential, their IP, their risk — the same low-signal posture an
OMP user runs today. Completion there is native-app local loopback
(§Native-app local-loopback completion) or paste-code; the credential is
written to the self-host stack's own `gateway_credentials` store, never
to a managed Compass store.

**A provider capability flag drives it.** The fork's `ProviderDefinition`
already carries per-provider capability flags with a derived-set pattern —
`readonly pasteCodeFlow?: boolean` (`registry/types.ts:55`) feeding
`PASTE_CODE_LOGIN_PROVIDERS` (`registry/derived.ts:7-9`) — and that is the
natural home for the tier: a named addition
`subscriptionTosTier?: "permissive" | "restricted"` on `ProviderDefinition`
(equivalently a derived set), exposed through the gateway's internal
capabilities route (E2), resolved once by the Server from that route,
and threaded into the injected `EnrollmentPolicy` at each E3
consultation point (§The EnrollmentPolicy seam).
Exact tier assignments beyond
Anthropic/OpenAI = restricted are verified per provider at implementation
(OQ-8; default restricted until confirmed permissive).

Market context (external corroboration, not contract): Kodus — the one
comparable managed BYOK service (<https://kodus.io>, BYOK docs) — allows
subscription plans for GLM Coding + Kimi Code only, not Anthropic/OpenAI:
the responsible line is drawn exactly at ToS tier. Managed's
price-sensitive story stays intact without restricted subscriptions: API
keys + OpenRouter cheap-OSS + permissive-tier subscriptions.

### The EnrollmentPolicy seam — how the matrix is enforced

The core enforces the matrix through a small extensible seam, never a
deployment-mode flag — the RIG-1717 boundary rule: "Prefer a seam the
managed service can extend over a choice that only fits one product"
(`docs/concepts/self-host-and-managed.md:40`).

- **The interface (Go, Server-side):**
  `EnrollmentPolicy.ProviderAllowed(ctx, provider, credentialKind, tosTier) (allowed bool, reason string)`.
  The Server resolves `tosTier` ONCE per call from E2's capabilities
  route (the fork registry's `subscriptionTosTier`, the single source of
  truth — §Consumption eligibility) and hands it to the injected policy,
  so an out-of-tree policy decides on data it is given rather than
  re-deriving a provider→tier map that would drift from the registry.
  Consulted in exactly three places — E3's `ListProviders` (a denied
  provider × credential kind is filtered from the enrollable set),
  `BeginEnrollment` (a denied provider × credential kind is rejected
  with `FailedPrecondition`, before any attempt row is persisted), and
  `SetProviderApiKey` (a denied provider × `api_key` is rejected with
  `FailedPrecondition` before the direct credential write — so the
  matrix's `api_key` row is enforced server-side, not merely filtered
  from the UI list).
- **The default implementation is allow-all.** The OSS core is the
  self-hosted product (`self-host-and-managed.md:10-15`), and the matrix
  permits self-host every tier — so allow-all IS the intended core
  behavior, not a permissive fallback. There is no deployment-mode
  signal anywhere in the core.
- **Injected at Server construction** (config/DI beside the existing
  Server wiring), so a policy is always present; nothing is keyed on a
  runtime mode flag.
- **The managed implementation (restricted-tier ⇒ deny) is a
  private-monorepo concern — named here, designed there**
  (`self-host-and-managed.md:34-38`). The managed control plane injects
  its own policy the same way it extends the store with per-tenant pool
  routing ("per-tenant pool routing inside `Store` — keyed on the
  resolved tenant, a bounded named addition, not a behavioral fork",
  `compass-managed-multitenancy/design.md:102-106`).

### Why paste-code is the v1 completion path (the redirect-URI reality)

The naive web design — initiate returns an auth URL whose `redirect_uri`
points at a Compass HTTPS callback — does not work with the OAuth apps the
fork ships. Providers validate `redirect_uri` against a registered allowlist
bound to the client_id, and the fork's client_ids are CLI apps registered
with loopback redirects:

- openai-codex pins the exact loopback redirect because "the token exchange
  would fail with 403 because the redirect_uri no longer matches the
  registered allowlist entry"
  (`forks/oh-my-pi/packages/ai/src/registry/oauth/openai-codex.ts:147-150`,
  which pins ``redirectUri: `http://localhost:${CALLBACK_PORT}${CALLBACK_PATH}` ``).
- The flow base class refuses random-port fallback for the same reason: "The
  OAuth provider validates redirect URIs against its registered callback, so
  falling back to a random port would be rejected"
  (`registry/oauth/callback-server.ts:216-219`).
- Anthropic's client_id is a fixed CLI app id
  (`CLIENT_ID = decode("OWQx...")`, `registry/oauth/anthropic.ts:12-13`)
  whose fork flow targets `http://localhost:54545/callback`
  (`CALLBACK_PORT = 54545` / `CALLBACK_PATH = "/callback"`,
  `anthropic.ts:19-20`). For anthropic, HTTPS-redirect rejection is an
  **inference**, not observed provider behavior: `AnthropicOAuthFlow` uses
  the legacy constructor (`super(ctrl, CALLBACK_PORT, CALLBACK_PATH)`,
  `anthropic.ts:212`), which leaves `allowPortFallback: true`
  (`callback-server.ts:87` legacy branch; `?? true` at `:96`) — the fork
  happily falls back to a random loopback port, which only works if
  Anthropic validates redirects against the RFC 8252 §7.3 loopback
  carve-out (variable port, fixed loopback host) rather than one exact
  URI. That carve-out never extends to arbitrary HTTPS hosts, and there is
  no evidence any HTTPS redirect is allowlisted — so the rejection is
  well-founded, but it is weaker evidence than codex's documented 403.

So a Compass-hosted `https://<host>/oauth/callback` is rejected by every
day-1 provider unless Compass registers its OWN OAuth app per provider —
which, for Anthropic, is not offered at all: the claude.ai authorize
endpoint is what grants `user:inference` (`anthropic.ts:21-25`), and
Anthropic's Feb 2026 legal/compliance policy
(<https://code.claude.com/docs/en/legal-and-compliance>) bars third-party
developers from offering "Claude.ai login" or routing consumer-plan
credentials on behalf of their users — there is no third-party OAuth-app
registration granting `user:inference` (D-1).

Paste-code needs no redirect registration. Every day-1 provider already
supports it: the registry marks anthropic, openai-codex, devin, gitlab-duo,
gitlab-duo-workflow, google-antigravity, google-gemini-cli, and zai
`pasteCodeFlow: true` (`registry/anthropic.ts:24-25`,
`registry/openai-codex.ts:17-18`; the derived set,
`registry/derived.ts:7-9`), and OMP's own login synthesizes a paste prompt
for exactly this set ("Paste the authorization code (or full redirect
URL):", `auth-storage.ts:2726-2729`). Anthropic even requests display-mode
explicitly — `code: "true"` in the authorize params
(`registry/oauth/anthropic.ts:221-230`) — so claude.ai SHOWS the user the
code to copy, and the pasted `code#state` fragment carries the CSRF state
through the exchange (`exchangeToken` splits it,
`anthropic.ts:240-250`). The fork also ships the parser for a pasted full
redirect URL (`parseCallbackInput`, `callback-server.ts:410-438`).

Therefore: **v1 ships initiate + paste-code-complete + API-key paste**
(plus native-app loopback where a local client runs, below). The
web UX is: click Connect → new tab opens the provider auth URL → user
authorizes → provider page displays the code → user pastes it into the
Compass dialog → Server exchanges and stores. This is the same UX OMP's
setup wizard gives paste-code providers today
(`packages/coding-agent/src/modes/setup-wizard/scenes/sign-in.ts:189-190`),
re-homed to the web, and the dialog accepts the FULL redirect URL — the
easiest paste, and the ruled v1 web UX (D-2). The zero-paste upgrade is
the native-app local-loopback completion (below), not a Compass-hosted
callback — that path is dead (D-1; Alternatives considered).

### The pending-attempt state store (the crux, resolved)

OMP's `OAuthCallbackFlow.login()` holds everything in process memory: it
mints `state` (`generateState()`, `callback-server.ts:120-126`), the
provider flow mints the PKCE verifier into an instance field
(`this.#verifier = pkce.verifier`, `anthropic.ts:216-219`; PKCE = 96 random
bytes base64url + SHA-256 challenge, `registry/oauth/pkce.ts:5-18`), and
`login()` awaits the loopback callback before calling
`exchangeToken(code, state, redirectUri)` (`callback-server.ts:139-180`).
In Compass, initiate and complete are separate HTTP requests, so that
state becomes a Server-owned row:

```sql
-- gateway_enrollment_attempts (folded into the squashed migration per the
-- store convention; names final at implementation)
id             UUID PRIMARY KEY,          -- attempt id, returned to the UI
owner_user_id  TEXT NOT NULL,             -- the enrolling user (tenant key)
provider       TEXT NOT NULL,             -- e.g. "anthropic"
method         TEXT NOT NULL,             -- "paste_code" | "loopback"
state          TEXT NOT NULL UNIQUE,      -- CSRF nonce (round-tripped through the provider authorize step)
pkce_verifier  TEXT NOT NULL,             -- server-side only; never leaves the Server/gateway hop
redirect_uri   TEXT NOT NULL,             -- the exact value used at authorize time (must match at exchange)
created_at     TIMESTAMPTZ NOT NULL,
expires_at     TIMESTAMPTZ NOT NULL,      -- created_at + 5 min (OMP's DEFAULT_TIMEOUT, callback-server.ts:17)
consumed_at    TIMESTAMPTZ                -- single-use: set on first complete; re-use rejected
```

Properties: **single-use** (first completion consumes; a second submit with
the same state is rejected), **TTL 5 minutes** (matching OMP's
`DEFAULT_TIMEOUT = 300_000`, `callback-server.ts:17`; expired rows are
swept lazily on lookup + a periodic sweep), **user-scoped** (every
completion — paste or loopback — runs through the bearer-authenticated,
owner-checked `CompleteEnrollment`: the attempt's `owner_user_id` must
equal the authenticated caller; the Server exposes no unauthenticated
completion or callback endpoint). The verifier is a
credential-equivalent secret while live: it never appears in any RPC
response, only in the Server→gateway exchange call. The table carries the
tenant column shape the managed-multitenancy record prescribes
(denormalized tenant key + RLS-ready,
`docs/designs/infra/runtime/compass-managed-multitenancy/design.md:66-125`;
enforcement layer is the managed-multitenancy record's OQ-2 (RLS), still open — this table follows
whatever T2 there lands, and `owner_user_id` scoping is enforced in the
handler regardless).

### Where the provider protocol runs: the gateway/TS internal enroll surface

`generateAuthUrl` and `exchangeToken` are per-provider fork code with real
provider quirks (anthropic's identity bootstrap + org resolution on
exchange, `anthropic.ts:273-287`; codex's JWT parsing; the `code#state`
fragment splitting). Re-implementing them in Go buys nothing and creates
the upstream-chase treadmill Matt rejected for the gateway itself
(RIG-1715 ruling, `compass-server-llm-gateway/design.md:23-33`). So the
gateway process — already the Bun tier that owns the fork's OAuth code and
runs OAuth REFRESH today (`auth-storage.ts:1283-1284` via RIG-1715 RD-5) —
grows a narrow internal enrollment surface, mounted on its existing
listener beside `/healthz` and `/v1/*` (`auth-gateway/server.ts:769-806`),
authenticated by a **dedicated server→gateway enroll token** — NOT the
RIG-1715 stack token, which authenticates the opposite direction (the
gateway calling the Server's RPC-store: "The gateway's store
implementation calls a narrow, stack-token-authenticated Server surface",
`compass-server-llm-gateway/design.md:348-350`; the blast-radius statement
"a compromised gateway (holding one stack token)", `design.md:333-334`,
stays accurate only if that token does not also authorize inbound enroll
calls) — and never agent bearers. The auth seam is pinned to RIG-1715
T3's injected verifier, which REPLACES the gateway's flat bearer set
with a boot option `authorize(req) → CallerIdentity | null`
(`compass-server-llm-gateway/design.md:810-825`). The enroll token is a
distinct token CLASS the same verifier resolves on that inbound gate,
alongside the agent class T3 introduces (the stack token authenticates
the opposite direction, above, and never reaches this inbound gate), and
each route authorizes the class it
admits at a single post-gate step: `/internal/enroll/*` admits only the
enroll class (rejecting the agent class), and the agent-facing `/v1/*`
plus the stack-facing store surface reject the enroll class (so
it never widens the agent surface). Class authorization is one post-gate
step keyed on the verifier's result — an injection point (the RIG-1715
GC names the auth verifier as exactly such a point), NOT a per-handler
edit to the `/v1/*` fork handlers and NOT a pre-dispatch route-prefix
branch in the routing core — so the RIG-1715 GC holds ("compass touches
the gateway only at injection points ... never the routing core",
`compass-server-llm-gateway/design.md:687-690`). E2 sequences against
T3: it designs the enroll class into the same injected verifier T3
introduces (both replace the flat `server.ts:772` gate), not against the
vanilla flat bearer set. The routes:

- `POST /internal/enroll/authorize-url` —
  `{provider, state, redirectUri, pkceChallenge}` →
  `{url, instructions}`. Stateless: the Server mints state + PKCE and
  passes the challenge in.
- `POST /internal/enroll/exchange` —
  `{provider, code, state, redirectUri, pkceVerifier}` →
  `{credential: OAuthCredentials}` (the fork shape:
  refresh/access/expires + orgId/orgName/accountId/email/authorizedAt,
  `registry/oauth/types.ts:4-30`).
- `GET /internal/enroll/providers` — per-provider enrollment capabilities
  (`pasteCodeFlow`, `subscriptionTosTier`) from the fork registry, the
  single source of truth for the tier flag (§Consumption eligibility).

This requires a small, seam-shaped, upstreamable fork change: today the
flow classes mint PKCE internally and carry the verifier as instance state
(`AnthropicOAuthFlow.#verifier`, `anthropic.ts:207,216-219`), which cannot
survive two separate requests. The fork extension is a **stateless flow
seam**: per-provider `generateAuthUrl(state, redirectUri, {challenge})`
and `exchangeToken(code, state, redirectUri, {verifier})` entry points
(or constructor-injected PKCE) — the exchange bodies are already
effectively stateless (`exchangeToken` reads only `this.#verifier` beyond
its arguments, `anthropic.ts:240-262`; codex passes
`this.#pkce.verifier`, `openai-codex.ts:167-169`), so this is parameter
threading, not new protocol code. It matches the RIG-1715 fork
constraint: "compass touches the gateway only at injection points ...
never the routing core" (`compass-server-llm-gateway/design.md:687-690`).

The Server orchestrates: it owns the attempt row, calls the gateway for
the two protocol steps, and writes the resulting credential. The gateway
never touches the attempt table and holds no enrollment state — a gateway
restart mid-enrollment loses nothing.

### The compass.v1 enrollment surface (UI ↔ Server)

compass.v1 is the sole UI↔server door (`proto/compass/v1/compass.proto:1-4`),
so the UI drives enrollment through a new service (proto change +
`moon run compass-proto:gen`, never a hand stub). A NEW service rather than
extending `SecretsService`: the secrets registry is names-only by invariant
(`SetSecret` writes the resolver, "never values" persisted,
`compass.proto:175-189`; `go/server/secrets_service.go:10-16`), while
enrollment writes the RIG-1715 `gateway_credentials` VALUE store — mixing
the two services would blur the invariant RIG-1715 explicitly preserved
(`compass-server-llm-gateway/design.md:314-333`).

```proto
// User-only (agent tokens PermissionDenied), mirroring the
// SetSecret/DeleteSecret authz posture (secrets_service.go:10-13).
service ProviderEnrollmentService {
  // Providers enrollable per the injected EnrollmentPolicy (default
  // allow-all; §The EnrollmentPolicy seam) + the caller's connected state.
  rpc ListProviders(ListProvidersRequest) returns (ListProvidersResponse);
  // Mint state + PKCE, persist the attempt row, return the auth URL.
  // Rejects (FailedPrecondition) a provider × credential-kind the
  // injected EnrollmentPolicy denies (default allow-all) — checked
  // BEFORE the attempt row is persisted (D-3).
  rpc BeginEnrollment(BeginEnrollmentRequest) returns (BeginEnrollmentResponse);
  // Completion (owner-checked, always bearer-authenticated): a pasted
  // code (or full redirect URL), or the code a local loopback listener
  // caught (loopback method).
  rpc CompleteEnrollment(CompleteEnrollmentRequest) returns (CompleteEnrollmentResponse);
  // API-key variant: value straight into gateway_credentials (redacted on wire).
  rpc SetProviderApiKey(SetProviderApiKeyRequest) returns (SetProviderApiKeyResponse);
  // Disconnect: CAS-disable the credential row (RIG-1715 Disable semantics).
  rpc DeleteCredential(DeleteCredentialRequest) returns (DeleteCredentialResponse);
}

message BeginEnrollmentRequest  {
  string provider = 1;
  string method = 2;  // "paste_code" (default) | "loopback" (local client)
}
message BeginEnrollmentResponse {
  string attempt_id = 1;
  string auth_url = 2;      // browser target
  string instructions = 3;  // provider-specific paste guidance
  int64  expires_at_unix_ms = 4;
}
message CompleteEnrollmentRequest {
  string attempt_id = 1;
  string code = 2 [debug_redact = true]; // pasted code or full redirect URL (parseCallbackInput semantics), or the loopback-caught code
}
message CompleteEnrollmentResponse { ConnectedProvider credential = 1; }
message SetProviderApiKeyRequest {
  string provider = 1;
  string api_key = 2 [debug_redact = true];
}
message ConnectedProvider {
  string provider = 1;
  string credential_id = 2;
  string kind = 3;          // "oauth" | "api_key"
  string email = 4;         // display identity from the exchange, when present
  string org_name = 5;
  int64  expires_unix_ms = 6;    // access-token expiry (display only)
  int64  authorized_at_unix_ms = 7;
}
```

Token values (access/refresh/api_key) NEVER appear in any response —
`ConnectedProvider` is display metadata only, the same redaction posture as
`SecretStatus` ("names + set/unset + routing, NEVER the value",
`compass.proto:205-213`).

### Native-app local-loopback completion (the seamless path)

The zero-paste variant is OMP's own mechanism, kept where it is safe: a
loopback listener on the enrolling user's OWN machine. A locally-running
Compass client (native app / CLI) begins enrollment with
`method = "loopback"`; `BeginEnrollment` stores the provider's registered
loopback redirect URI (anthropic targets `http://localhost:54545/callback`
— `CALLBACK_PORT = 54545` / `CALLBACK_PATH = "/callback"`,
`anthropic.ts:19-20`) — the exact redirect the CLI client_ids are
registered with, so NO provider-side app registration is needed. The
client opens `auth_url`, runs the fork's loopback callback server
(`Bun.serve` bound to 127.0.0.1, `callback-server.ts:282-290`), catches
the redirect, verifies `state` (ignoring forgeable mismatches,
`callback-server.ts:342-347`), and submits the caught code through the
SAME bearer-authenticated, owner-checked `CompleteEnrollment` as the
paste path.

The security shape is load-bearing: OMP never had a login-CSRF exposure
because its callback lands on the enrolling user's own machine — a
loopback `Bun.serve` on 127.0.0.1 (`callback-server.ts:283-285`) — so the
loopback origin IS the session binding. This method keeps that binding
intact: a foreign attempt's redirect cannot land on the victim's
loopback, and completion is owner-checked regardless. The Server never
receives a provider redirect at all — no unauthenticated callback
endpoint, no parked-code state, nothing bearer-less that could write or
stage a credential. (A shared Compass-hosted HTTPS callback would drop
exactly this binding and reopen the vector; that variant is rejected —
Alternatives considered §Compass-registered OAuth apps.)

This is the primary completion for restricted-tier self-host enrollment
(§Consumption eligibility) and available wherever a local client runs;
web paste-code remains the universal fallback.

### Writing the credential (RIG-1715 store, consumed not redesigned)

The exchange result (fork `OAuthCredentials`,
`registry/oauth/types.ts:4-30`) maps onto a `gateway_credentials` row as
RIG-1715 defines it: OAuth-shaped payload (access/refresh/expiry), a
monotonic `version` for CAS, scope = owner-scoped by `owner_user_id`
(`compass-server-llm-gateway/design.md:324-333`). One gap: RIG-1715's Go
store surface is `List / UpdateOAuth / Disable`
(`design.md:799`) — read + refresh-write-back + disable, no CREATE. **This
record makes two named additions to that seam: the `Create` write** (and
the api_key insert path) **and the identity/display columns** backing
upsert-by-identity + the UI list — the frozen row shape is "api_key and
OAuth-shaped payloads (access/refresh/expiry), a monotonic `version` per
row supplying the CAS substrate, and a scope column"
(`design.md:324-331`), with no identity columns.
`Create(ctx, cred GatewayCredential) (id string, err error)` has
upsert-by-identity semantics — a re-login for the same
(owner_user_id, provider, orgId/accountId identity) REPLACES the row's
token payload (bumping `version`) rather than accreting duplicate rows,
mirroring how OMP's re-login overwrites a credential for the same
identity. The identity key pins NULL semantics (E1): identity components
are nullable in exactly the degraded cases — anthropic's
`resolveAccountIdentity` swallows bootstrap failures and returns a partial
identity (`catch { return identity; }`, `anthropic.ts:201-203`), and codex
refresh deliberately omits org fields ("Deliberately no org fields on the
result", `openai-codex.ts:374-376`) — and a Postgres unique index treats
NULLs as distinct, so the key is declared `UNIQUE NULLS NOT DISTINCT` (or
identity coalesced to `''` in the key) and degraded-identity re-logins
replace instead of accreting. Identity display fields
(email/orgName) are stored for the UI list. The
gateway's read path (RIG-1715 T2 `List`) picks the new row up on its next
snapshot with zero changes — enrollment composes with the frozen read
design instead of touching it. Deletion is RIG-1715's CAS `Disable`, so
disconnect never races a concurrent gateway refresh write-back.

### Variants: what ships v1, what defers

- **API-key paste — v1.** `SetProviderApiKey` consults the injected
  `EnrollmentPolicy` (`ProviderAllowed(ctx, provider, "api_key", tier)`)
  and rejects a denied provider with `FailedPrecondition` BEFORE writing;
  otherwise it writes an api_key-kind `gateway_credentials` row directly,
  with no attempt row (nothing to persist between requests). The existing
  `SECRET_KIND_PROVIDER` registry row
  (names-only) is unchanged by this record, per RIG-1715: "The
  `SECRET_KIND_PROVIDER` registry rows stay as they are ... the value store
  holds the credentials the gateway actually routes with"
  (`compass-server-llm-gateway/design.md:331-333`).
- **Paste-code OAuth — v1.** The primary web OAuth mechanism (above); the
  dialog accepts the full redirect URL (`parseCallbackInput`,
  `callback-server.ts:410-438`).
- **Native-app loopback OAuth — v1 where a local client runs** (E4;
  Approach §Native-app local-loopback completion): the seamless
  zero-paste path, and the primary completion for restricted-tier
  self-host enrollment. The Compass-hosted HTTPS callback variant is
  dropped (D-1; Alternatives considered).
- **Device-code OAuth — deferred; the named codex web upgrade (D-2).**
  Device-code needs NO redirect URI or registered app, and for codex it
  is a zero-paste flow the fork already ships
  fully server-drivable: `loginOpenAICodexDevice` posts the client_id to
  the device-usercode endpoint (`openai-codex.ts:251-255`), shows the user
  `Enter code: ${userCode}` at the provider device page
  (`openai-codex.ts:283-286`), polls, and the provider RETURNS both the
  code and the verifier in the poll response (`authorization_code?:
  string; code_verifier?: string`, `openai-codex.ts:319-322`) — no server
  PKCE state, no user paste-back; the user types a short code INTO the
  provider page instead of copying a long code OUT of it. Not universal:
  anthropic has no device flow (the fork's device-code consumers are codex
  and xai only; anthropic is callback/paste only). Deferred from v1 —
  paste-code covers the day-1 set (D-2) — but it stays the named
  zero-paste web upgrade for codex if the paste UX bar fails in practice
  (OQ-4); when promoted it is a fourth `method` on the same attempt table
  (the poll
  interval/expiry fields it needs are additive columns).

### Multi-tenancy and self-host

Enrollment is inherently per-user: every attempt and every written
credential carries `owner_user_id`, resolved server-side from the
authenticated caller (never a request field — the actor posture the
multitenancy record pins, `compass-managed-multitenancy/design.md:116-118`).
RPCs are user-only, agent tokens rejected, mirroring SetSecret
(`secrets_service.go:10-13`). Self-host is the single-team degenerate case:
the same surface, the operator's user enrolls the accounts — no separate
code path (the RIG-1717 "one architecture" posture the multitenancy record
carries, `design.md:122-126`); the core's enrollable set is uniform (the
allow-all default `EnrollmentPolicy`) — the managed product narrows it
by injecting a restricted-tier-deny policy out of tree
(§The EnrollmentPolicy seam), enforced server-side in E3, not merely
hidden by the UI. Org-shared credentials (the RIG-1715 shared
pool fallback) are out of enrollment-v1 scope: the shared-row write path is
an admin surface that lands with the managed plane's org entity
(`compass-server-llm-gateway/design.md:397-405`), and nothing here blocks
it — it is one more writer to the same store.

## Alternatives considered

### Re-implement token exchange in Go (rejected)

The Server could speak the provider token endpoints directly (they are
plain HTTPS form posts, e.g. `TOKEN_URL`, `anthropic.ts:15,252-265`). But
each provider carries drift-prone quirks the fork already encodes and
keeps current — anthropic's identity bootstrap fallback
(`fetchBootstrapIdentity`, `anthropic.ts:141-176`), the `code#state`
fragment contract (`anthropic.ts:243-250`), codex's registered-redirect
403 behavior (`openai-codex.ts:147-150`) — and RIG-1715 already ratified
"fork is source of truth, don't chase upstream in Go" for exactly this
code mass. The gateway internal surface costs three small routes; the Go
re-implementation costs a per-provider protocol port plus permanent sync.

### Gateway hosts initiate/callback directly (rejected)

Let the Bun gateway own the whole enrollment HTTP surface. Rejected on two
frozen contracts: the UI may only speak compass.v1 ("the single, owned
door", `compass.proto:1-4`; RIG-1715 GC: "The gateway's HTTP listener is
agent-facing, never UI-facing",
`compass-server-llm-gateway/design.md:691-694`), and the public HTTPS
ingress with TLS + operational posture is the Server's network door
(`go/server/network_door.go:3-7,60-65`). The gateway would also need the
attempt store (a second Postgres writer or a new RPC surface anyway) and
would hold durable enrollment state in the one tier designed to be
restartable. Keeping the gateway stateless-protocol-only preserves the
RIG-1715 topology.

### Reuse SecretsService.SetSecret for everything (rejected)

The precedent RIG-1715 pointed at (`SetSecretRequest`,
`compass.proto:191-198`) writes the names-only registry + resolver — a
different store with a never-persist-values invariant
(`secrets_service.go:10-16`). OAuth enrollment cannot ride it (there is no
"value" until the exchange completes, and the result is a multi-field
token payload, not one string), and stretching `SetSecret` with
enrollment fields would couple the two stores RIG-1715 deliberately kept
distinct. A dedicated service keeps each invariant crisp.

### Compass-registered OAuth apps + Server HTTPS callback (rejected)

An earlier draft designed a Server `GET /oauth/callback` on the network
door (park-then-confirm), gated on registering Compass-owned OAuth apps
per provider (old E4/OQ-1). Dropped. For Anthropic the registration path
does not exist: the Feb 2026 legal/compliance policy
(<https://code.claude.com/docs/en/legal-and-compliance>) restricts
consumer Pro/Max OAuth to Claude Code + Claude.ai, bars third parties
from offering "Claude.ai login" or routing consumer-plan credentials on
behalf of their users, and offers no third-party registration granting
`user:inference`; enforcement (the Jan 9 2026 server-side block) targets
clients spoofing the Claude Code client — the posture the fork embodies:
it reuses Claude Code's client_id
(`CLIENT_ID = decode("OWQx...")`, `registry/oauth/anthropic.ts:13`) and
sends the Claude Code wire fingerprint (UA `claude-code/2.1.165` via
`claudeCodeVersion = "2.1.165"`,
`providers/claude-code-fingerprint.ts:11`, and the 64k output clamp
"OAuth requests clamp to match the wire fingerprint",
`claude-code-fingerprint.ts:16-19`). With the registered-app path dead
for the flagship restricted provider — and unneeded for permissive/API-key
enrollment, which paste + loopback already cover — the callback's residual
value cannot pay its cost: re-homing the redirect to a shared HTTPS host
drops the loopback session binding OMP relies on
(`callback-server.ts:283-285`) and reopens a login-CSRF vector (attacker
begins enrollment, lures the victim into the attacker's `auth_url`; the
provider redirects the victim's grant onto the attacker's still-valid
`state`), which forced park-then-confirm machinery to close. Native-app
loopback delivers the same zero-paste UX with none of that.

### BYO-egress for restricted-tier subscriptions (rejected)

Routing restricted-provider subscription calls back out through the
user's native app, so egress leaves from the user's IP. Rejected: it does
not cure the ToS violation — Anthropic bars third-party *orchestration*
of consumer credentials on behalf of users (policy URL above), which
BYO-egress still is; only the egress IP changes, so it merely lowers the
detection signal while documenting intent to evade. It also reimposes the
always-on-user-machine friction managed exists to remove. Restricted-tier
subscription enrollment is instead denied on managed by the
managed-injected `EnrollmentPolicy` (out of tree; §Consumption
eligibility, D-3); the self-host core permits it — the user's own box IS
the server — and is the supported home for it.

### A managed/self-host deployment-mode flag in the OSS core (rejected)

Enforce the matrix's Managed column with a deployment-mode signal in
server config. Rejected (Matt): a mode flag in the OSS core invites
self-hosters to LARP as "managed" when managed is the closed-source
product, and it fails open when unset. The extensible `EnrollmentPolicy`
seam gives the core exactly ONE behavior — allow-all, the intended
self-host posture — and lets the managed product extend it out of tree,
matching the one-architecture-two-products boundary ("the core must not
assume it is single-tenant, nor assume it is managed. Prefer a seam the
managed service can extend over a choice that only fits one product",
`docs/concepts/self-host-and-managed.md:39-42`) and the
managed-extends-the-store precedent
(`compass-managed-multitenancy/design.md:102-106`).

## Global Constraints

- **Record placement + ledger**: this record lives under `docs/designs/server/`
  (RIG-2577 taxonomy: server-side domain + write paths + auth). `server/` is a
  governed ledger root: the record carries `DECISIONS.md` rows at freeze (the
  driver flips the ledger in the freeze PR; this draft does not write ledger
  rows).
- **Compass is PUBLIC.** Never name the private monorepo or other agent
  products; `moon run orion-ref-gate:check` enforces.
- **compass.v1 discipline**: any proto change goes through
  `proto/compass/v1/*.proto` + `moon run compass-proto:gen` — never a
  hand-written stub. compass.v1 is the sole UI↔server door
  (`compass.proto:1-4`).
- **Consumes, never redesigns, the frozen RIG-1715 record**: the
  `gateway_credentials` store shape (scope column, monotonic `version` CAS),
  own-before-shared pools, and the gateway RPC-store read path
  (`compass-server-llm-gateway/design.md:312-405,768-804`). This record's two
  named additions to that seam are the `Create` write AND the
  identity/display columns backing upsert-by-identity + the UI list
  (Approach §Writing the credential).
- **Consumption-eligibility invariant (D-3)**: the OSS core enforces the
  §Consumption eligibility matrix through the `EnrollmentPolicy` seam
  (default allow-all — self-host permits every tier), consulted in E3's
  `ListProviders`, `BeginEnrollment`, and `SetProviderApiKey` — the
  Server resolves each provider's ToS tier from E2 and threads it in;
  there is NO deployment-mode
  signal in the core. The managed restricted-tier-deny policy is
  injected out of tree (§The EnrollmentPolicy seam), so "a managed
  deployment never holds, refreshes, or routes a restricted-tier
  subscription-OAuth credential" holds by managed's injected policy
  denying enrollment, not by a core mode check. Enforced server-side in
  the enrollment handlers (E3), not merely hidden in the UI (E5). This
  is a policy gate ON TOP of the frozen RIG-1715 consumption model,
  which composes unchanged — NOT a supersession of RIG-1715 (its "OAuth
  refresh stays gateway-side" contract,
  `compass-server-llm-gateway/design.md:367`, holds as-is for every
  credential a deployment is permitted to hold).
- **Fork changes are seam-shaped and upstreamable** (RIG-1715 GC,
  `design.md:687-690`): the stateless-flow seam (explicit PKCE threading) and
  the internal enroll routes touch injection points only, never the routing
  core or provider protocol bodies.
- **Secrets redaction posture**: token values, auth codes, and PKCE verifiers
  are `[debug_redact]` on the wire where client-supplied, never logged, and
  never returned in any response (the `SecretStatus` precedent,
  `compass.proto:205-213`).
  Attempt rows are secret-bearing (state + PKCE verifier): excluded from
  debug dumps, log statements, and general query
  logging.
- **Enrollment RPCs are user-only**: agent tokens are `PermissionDenied`,
  mirroring SetSecret/DeleteSecret (`go/server/secrets_service.go:10-13`).
  `owner_user_id` is always server-derived from the authenticated caller,
  never a request field.
- **Attempt rows are single-use with a 5-minute TTL** (OMP's
  `DEFAULT_TIMEOUT = 300_000`, `callback-server.ts:17`).
- **Store discipline**: DDL folds into the squashed migration; new store code
  ships an in-memory reference + `pgtest` suite (DL-174 pyramid, per RIG-1715
  GC `design.md:704-706`); tenant-column shape follows the
  managed-multitenancy record (RLS-ready, enforcement per that record's OQ-2 (RLS) outcome).
- **Go code under `go/`**, gateway-side TS in the fork's compass wrapper, UI
  in `apps/ui` (SolidJS v2) — per-lane owners as tasked below.
- Markdownlint-clean record
  (`markdownlint-cli2 --config .markdownlint.json <file>`); Conventional
  Commits + `Co-authored-by: Matt Wilkinson <matt@rigel.build>` (driver-owned).

## Plan

Ordering: E1 and E2 are independent and parallel; E3 depends on both; E4
and E5 depend on E3. Day-1 provider set follows RIG-1715 RD-6 (anthropic,
openai/openai-codex, google) — for enrollment that means: API-key paste
for all three everywhere; anthropic + openai-codex subscription OAuth
(paste-code or loopback) permitted by the core's allow-all default
policy (restricted tier — denied on managed by the out-of-tree injected
policy, §Consumption eligibility); managed subscription OAuth activates
per permissive-tier provider as each is verified at implementation
(e.g. zai/GLM, already in the paste-code set — `registry/derived.ts:7-9`).

### E1 — Attempt store + gateway_credentials Create (Owner: compass-server)

The `gateway_enrollment_attempts` table (schema in Approach §pending-attempt
state store) with single-use consumption and TTL sweep, plus the `Create`
addition to the RIG-1715 credential-store seam: insert-or-replace by
credential identity (owner_user_id, provider, orgId/accountId), bumping the
monotonic `version` on replace, storing display identity (email/org_name)
alongside the token payload. The identity unique key pins NULL semantics —
`UNIQUE NULLS NOT DISTINCT` (or coalesce to `''`) — because identity
degrades to partial in real paths (anthropic bootstrap catch,
`anthropic.ts:201-203`; codex refresh omits org fields,
`openai-codex.ts:374-376`) and NULLs-as-distinct would accrete duplicates.
Also the api_key-kind insert used by E3's `SetProviderApiKey`.

Freeze cross-reference (freeze-PR guidance for the driver): this task
amends RIG-1715's frozen `gateway_credentials` shape — the platform
record describes the store with no identity columns
(`compass-server-llm-gateway/design.md:324-331`). At freeze, add a
cross-reference to RIG-1715's platform ledger (or an explicit amendment
note in the RIG-1715 record) recording that RIG-3050 extends
`gateway_credentials` with the identity/display columns
(email/org_name/orgId/accountId), the `Create` method, and the
`UNIQUE NULLS NOT DISTINCT` identity index — so a reader of the frozen
platform record is pointed at this server record for the current table
shape. This draft edits neither the RIG-1715 record nor `DECISIONS.md`.

Interfaces:

- Consumes: the RIG-1715 `gateway_credentials` shape (scope column,
  monotonic `version`, `compass-server-llm-gateway/design.md:324-333`);
  squashed-migration convention; the multitenancy tenant-column shape
  (`compass-managed-multitenancy/design.md:66-125`).
- Produces (Go, `go/internal/store` or the gateway-credentials package
  RIG-1715 T2 creates — land beside it):
  - `type EnrollmentAttempt struct { ID, OwnerUserID, Provider, Method, State, PKCEVerifier, RedirectURI string; CreatedAt, ExpiresAt time.Time; ConsumedAt *time.Time }`
  - `CreateAttempt(ctx context.Context, a EnrollmentAttempt) error`
  - `ConsumeAttempt(ctx context.Context, id, ownerUserID string) (EnrollmentAttempt, error)` —
    atomically sets `consumed_at` iff unconsumed + unexpired; `ErrGone`
    otherwise
  - `CreateCredential(ctx context.Context, c GatewayCredential) (id string, err error)` —
    upsert-by-identity (`UNIQUE NULLS NOT DISTINCT` key), version-bumping
    (the seam addition)
- Test cycle: in-memory ref + `pgtest` (DL-174): single-use race (two
  concurrent consumes, one wins), TTL expiry,
  upsert-replaces-not-duplicates,
  version bump on re-login incl. a partial-identity re-login (NULL/absent
  orgId/accountId) replacing rather than duplicating.

### E2 — Fork stateless-flow seam + gateway internal enroll routes (Owner: gateway-TS)

Fork side: the stateless PKCE seam — per-provider `generateAuthUrl` /
`exchangeToken` callable with an externally supplied challenge/verifier
instead of instance state (`AnthropicOAuthFlow.#verifier`,
`anthropic.ts:207,216-219`; `openai-codex.ts:152,167-169`), for the day-1
OAuth providers (anthropic, openai-codex), plus the
`subscriptionTosTier?: "permissive" | "restricted"` capability field on
`ProviderDefinition` (beside `pasteCodeFlow`, `registry/types.ts:55`) with
a derived set per the `PASTE_CODE_LOGIN_PROVIDERS` pattern
(`registry/derived.ts:7-9`). Upstreamable parameter
threading; no protocol-body changes. Gateway side: three routes on the
existing listener (`auth-gateway/server.ts:769-806`), authenticated by the
dedicated server→gateway enroll token (Approach §internal enroll surface —
distinct from the RIG-1715 stack token and never satisfiable by an agent
bearer):

- `POST /internal/enroll/authorize-url`:
  `{provider: string, state: string, redirectUri: string, pkceChallenge: string}` →
  `200 {url: string, instructions?: string}`
- `POST /internal/enroll/exchange`:
  `{provider: string, code: string, state: string, redirectUri: string, pkceVerifier: string}` →
  `200 {credential: OAuthCredentials}` (`registry/oauth/types.ts:4-30`) |
  `4xx {error: string}` classified (provider-denied vs bad-code vs
  transient), body never logged
- `GET /internal/enroll/providers`:
  `200 {providers: {id: string, pasteCodeFlow: boolean, subscriptionTosTier: "permissive" | "restricted"}[]}` —
  the per-provider capability surface E3's `ListProviders` merges
  (§Consumption eligibility). The route coalesces an absent
  `subscriptionTosTier` to `"restricted"` before serializing
  (`ProviderDefinition.subscriptionTosTier` is optional, matching
  optional `pasteCodeFlow`, `registry/types.ts:55`; OQ-8's
  default-restricted posture), so the wire field is always one of the
  two literals E3's consumer merges

Interfaces:

- Consumes: `generatePKCE()` shape (`registry/oauth/pkce.ts:5-18`, minting
  moves Server-side but the challenge format is this contract);
  per-provider flow classes (`anthropic.ts:206-288`,
  `openai-codex.ts:115-170`); `parseCallbackInput`
  (`callback-server.ts:410-438`) for pasted-full-URL handling at exchange.
- Produces: the three internal routes (fork compass-wrapper package, beside
  the RIG-1715 T1 boot entrypoint); the stateless-flow fork seam; the
  pinned enroll-token auth seam: the enroll token is a distinct token
  CLASS resolved by RIG-1715 T3's injected verifier (`authorize(req) →
  CallerIdentity | null`, which replaces the flat `opts.bearerTokens`
  gate, `compass-server-llm-gateway/design.md:810-825`), authorized
  per-route at a single post-gate step — `/internal/enroll/*` admits
  only the enroll class, and `/v1/*` plus the stack surface reject it.
  E2 sequences against T3 (both replace the flat `server.ts:772` gate),
  not against the vanilla flat bearer set.
- Test cycle: fork tests — authorize-url golden params per provider
  (client_id/scope/challenge/state round-trip, anthropic `code: "true"`,
  `anthropic.ts:221-230`); exchange against a fake token endpoint incl.
  `code#state` fragment splitting (`anthropic.ts:243-250`); 401 without
  the enroll token; 401 for a valid AGENT bearer on `/internal/enroll/*`
  (the negative that pins the token separation); 401 for the enroll
  token on a non-enroll route (the symmetric negative — the enroll
  token never widens the agent-facing surface).

### E3 — ProviderEnrollmentService: proto + Server handlers (Owner: compass-server; depends E1, E2)

The compass.v1 service (proto in Approach §compass.v1 enrollment surface) +
`moon run compass-proto:gen`, and the Go handlers wired beside
SecretsService on all three doors (socket/dev/network,
`go/server/serve.go:701-743`, `network_door.go:285-292`): user-only authz
(agent PermissionDenied per `secrets_service.go:10-13`); `BeginEnrollment`
mints state (16-byte hex, `callback-server.ts:120-126` semantics) + PKCE
server-side (Go crypto; format per `pkce.ts:5-18`: 96-byte base64url
verifier, S256 challenge), persists the attempt, calls the gateway
authorize-url route, and selects the redirect URI per method (loopback
method: the provider's registered loopback URI, `anthropic.ts:19-20`);
`CompleteEnrollment` consumes the attempt (owner-checked), accepts
code-or-full-redirect-URL (`parseCallbackInput` semantics) from paste or
the loopback client, parses the pasted input and REJECTS
(`FailedPrecondition`/`InvalidArgument`) if the returned state does not
equal the stored `attempt.state` BEFORE calling exchange — this check
lives in the Go handler and is never delegated to the exchange call,
because anthropic's `exchangeToken` overrides the passed state with the
`code#state` fragment (`anthropic.ts:243-250`), so the gateway exchange
does not validate it (PKCE remains the primary binding; this makes the
attempt `state` column's CSRF role real) — then calls the gateway
exchange and writes via
`CreateCredential`; `SetProviderApiKey` consults the policy then writes
directly (below); `ListProviders`
merges the enrollable registry (paste-code + api_key capability AND the
ToS-tier flag per provider, via E2's capabilities route) with the
caller's connected rows, filtered per the injected `EnrollmentPolicy`;
`DeleteCredential` calls RIG-1715 CAS `Disable`. **Eligibility gate
(D-3): the Server resolves each provider's ToS tier once from E2's
capabilities route and threads it into the injected `EnrollmentPolicy`
(`ProviderAllowed(ctx, provider, credentialKind, tosTier)`), consulted
at three points — `ListProviders` filters, and `BeginEnrollment` and
`SetProviderApiKey` reject (`FailedPrecondition`) a denied provider ×
credential kind (default allow-all — §The EnrollmentPolicy seam) — the
server-side enforcement of the §Consumption eligibility matrix. The
policy check runs FIRST: a denied `BeginEnrollment` returns
`FailedPrecondition` WITHOUT persisting an attempt row, and a denied
`SetProviderApiKey` rejects before the direct write. Credential-kind
branching (api_key vs subscription OAuth) happens here.**

Interfaces:

- Consumes: E1's store surface; E2's internal routes (via the gateway base
  URL the RIG-1715 T1/T2 wiring already carries, plus the dedicated
  server→gateway enroll token this record introduces — §the gateway/TS
  internal enroll surface; distinct from the stack token, which
  authenticates the opposite direction);
  `auth` caller-identity interceptors (`serve.go:700-704`).
- Produces: `proto/compass/v1/enrollment.proto` (service + messages as
  specified; field redaction annotations) + regenerated clients;
  `go/server/enrollment_service.go` implementing
  `compassv1connect.ProviderEnrollmentServiceHandler`; door registration on
  socket/dev/network muxes; `auth.classifyProcedure` entries — all five
  procedures `authenticatedOpen`, handler enforces user-only (the
  SecretsService pattern: the gate admits any authenticated account and
  the handler does the fine authz, `secrets_service.go:5-8`,
  `network_door.go:288-291`). Without classification a new procedure
  silently fail-closes to adminOnly on the network door
  (`go/internal/auth/admin_gate.go:44-46`), and
  `go/internal/auth/classify_exhaustive_test.go:61-62` reddens CI on any unclassified
  generated procedure.
- Test cycle: `pgtest` handler tests — happy paste-code path against a fake
  gateway; policy injection (the eligibility negative): with a
  deny-restricted `EnrollmentPolicy` injected, `ListProviders` omits the
  denied provider × credential-kind from the enrollable set,
  `BeginEnrollment` for a restricted-tier provider is rejected with NO
  attempt row persisted, and `SetProviderApiKey` for a policy-denied
  provider × `api_key` is rejected before any credential write, while the
  default allow-all policy includes it and admits both for the same
  provider;
  agent-token 403; expired/consumed/foreign-owner attempt
  rejection; state-mismatch rejection (pasted state != stored
  `attempt.state`, rejected before the gateway exchange is called);
  api_key path; no token value in any response (assert
  redaction); a network-door test that a NON-admin user clears the admin
  gate on all five procedures (pins the authenticatedOpen classification);
  proto-regen CI green.

### E4 — Native-app local-loopback completion (Owner: gateway-TS local-client listener + compass-server method plumbing; depends E3)

The seamless zero-paste method for a locally-running Compass client (D-1;
Approach §Native-app local-loopback completion): `BeginEnrollment` with
`method = "loopback"` stores the provider's registered loopback redirect
URI on the attempt (anthropic `http://localhost:54545/callback`,
`anthropic.ts:19-20`; codex's pinned loopback, `openai-codex.ts:147-150`);
the local client opens `auth_url`, runs the loopback listener the fork
already ships (`callback-server.ts:139-180,282-290` — reused, not
re-implemented), catches the provider redirect on the user's own
127.0.0.1, verifies `state`, and submits the caught code through the
bearer-authenticated, owner-checked `CompleteEnrollment` — the identical
completion path as paste. Primary completion for restricted-tier
self-host enrollment (§Consumption eligibility).

Interfaces:

- Consumes: E3 `BeginEnrollment`/`CompleteEnrollment`; the fork's
  loopback callback server (`callback-server.ts:139-180,282-290`); the
  providers' registered loopback redirects (`anthropic.ts:19-20`,
  `openai-codex.ts:147-150`).
- Produces: the `method = "loopback"` branch in `BeginEnrollment`
  (loopback redirect_uri selection — Server side, lands with E3's
  handlers); the local-client connect command (open auth_url → listen →
  `CompleteEnrollment(attempt_id, code)`); state verification on the
  caught redirect (ignore-forgeable posture, `callback-server.ts:342-347`).
- Test cycle: fake provider redirect against the local listener →
  `CompleteEnrollment` called with the caught code; state-mismatch
  redirect ignored; listener timeout aligned with the attempt TTL
  (`DEFAULT_TIMEOUT`, `callback-server.ts:17`); no code ever logged.

### E5 — UI Providers settings surface (Owner: compass-ui; depends E3)

A Providers section on the settings surface (beside the tracker-config
editor, `apps/ui/src/components/SettingsView.tsx:78-83`): provider list
reflecting policy-filtered eligibility from `ListProviders` (a
provider × credential kind the injected `EnrollmentPolicy` denies is
simply not offered — e.g. under a managed-injected deny policy a
restricted-tier provider shows API-key entry only, never a subscription
Connect), connected state (identity email/org, expiry, kind), Connect
(opens
`auth_url` in a new tab, then shows the paste-code dialog with
`instructions`, submits `CompleteEnrollment`), API-key entry, and
Disconnect (confirm → `DeleteCredential`). Attempt-expiry countdown from
`expires_at_unix_ms`; a failed/expired attempt offers re-begin. Clients via
the `@compass/client` factories/live-client seam
(`apps/ui/src/live/client.ts:43-52`) — the generated
ProviderEnrollmentService client added beside comms/compass; fakes via
`createRouterTransport` per the established test pattern
(`apps/ui/src/live/query.test.ts:17-28`).

Interfaces:

- Consumes: generated `ProviderEnrollmentClient` (E3);
  `LiveClients` construction (`live/client.ts:21-52`); SolidJS v2 idioms +
  the SettingsView draft/commit pattern (`SettingsView.tsx:78-83`).
- Produces: `ProvidersView` (or a SettingsView section) + paste-code
  dialog + api-key form; store accessors
  `store.providers(): ConnectedProvider[]`,
  `store.beginEnrollment(provider): Promise<BeginEnrollmentResponse>`,
  `store.completeEnrollment(attemptId, code)`,
  `store.setProviderApiKey(provider, key)`,
  `store.deleteCredential(credentialId)`.
- Test cycle: component tests over a `createRouterTransport` fake
  (begin→paste→connected; error surfaces on bad code; disconnect
  confirm); no token value ever rendered or stored client-side.

## Tasks

- [ ] E1 — Attempt store + `gateway_credentials` Create
      (Owner: compass-server) — attempt table, single-use consume, TTL,
      upsert-by-identity Create, in-memory ref + pgtest.
- [ ] E2 — Fork stateless-flow seam + gateway internal enroll routes
      (Owner: gateway-TS) — PKCE threading seam, `/internal/enroll/*`
      routes, server→gateway enroll-token auth, fork tests.
- [ ] E3 — ProviderEnrollmentService proto + handlers
      (Owner: compass-server; deps: E1, E2) — enrollment.proto + regen,
      user-only handlers on all doors (classified authenticatedOpen),
      paste-code + api_key + list + disconnect; server-side eligibility
      gate via the injected EnrollmentPolicy (D-3: default allow-all;
      managed injects restricted-tier deny out of tree).
- [ ] E4 — Native-app local-loopback completion
      (Owner: gateway-TS local client + compass-server; deps: E3) —
      loopback `method` in BeginEnrollment, local listener reusing the
      fork's callback server, completion via owner-checked
      `CompleteEnrollment`.
- [ ] E5 — UI Providers settings surface
      (Owner: compass-ui; deps: E3) — provider list, connect/paste dialog,
      api-key form, disconnect, router-transport-fake tests.

## Decisions (ruled by Matt)

- **D-1 — No Compass-registered OAuth apps; the Server HTTPS callback is
  dropped (resolves OQ-1).** There is no third-party OAuth-app
  registration granting Anthropic `user:inference`: the Feb 2026
  legal/compliance policy
  (<https://code.claude.com/docs/en/legal-and-compliance>) restricts
  consumer Pro/Max OAuth to Claude Code + Claude.ai and explicitly bars
  third-party developers from offering "Claude.ai login" or routing
  consumer-plan credentials on behalf of their users. "Register our own
  app" is therefore not a v1 option, and the callback endpoint it would
  have enabled is removed from the plan (Alternatives considered
  §Compass-registered OAuth apps). v1 OAuth completion is paste-code
  (web) + native-app local loopback (seamless).
- **D-2 — Paste-code passes the v1 web UX bar (resolves OQ-2).** The web
  paste is the FULL redirect URL — the easiest paste; the fork already
  parses it (`parseCallbackInput`, `callback-server.ts:410-438`) — and
  the seamless zero-paste path is native-app loopback (E4), not a hosted
  callback. Device-code stays the named codex upgrade if the paste bar
  fails in practice (OQ-4).
- **D-3 — Consumption eligibility is per provider ToS tier; the OSS core
  enforces it through an extensible `EnrollmentPolicy` seam (resolves
  OQ-7 and the egress strategy fork).** The §Consumption eligibility
  matrix is the policy: server-side operation of provider OAuth apps is
  accepted ONLY for the permitted tiers — API keys (commercial terms),
  permissive-tier subscriptions (provider-welcomed), and aggregators.
  The OSS core's own enrollment permits every tier (the default policy
  is allow-all: every deployment of this repo's code is self-host,
  where the user's own box is the server — their credential, their IP,
  their risk, the OMP posture); the managed product denies
  restricted-tier subscription OAuth (Anthropic, OpenAI) by injecting a
  stricter `EnrollmentPolicy` out of tree (§The EnrollmentPolicy seam).
  BYO-egress is rejected (Alternatives considered): it does not cure the
  ToS violation and reimposes the friction managed removes. RIG-1715's
  server-side consumption model composes UNCHANGED — this is a policy
  gate on top of the frozen contract, not a supersession (Approach
  §Consumption eligibility).

## Open Questions

### Non-load-bearing (designed-against defaults; deferrable)

- **OQ-3 — Attempt TTL sweep cadence.** Designed against: lazy expiry on
  lookup + a periodic sweep (interval free to pick at implementation; rows
  are tiny and single-use). No contract impact.
- **OQ-4 — Device-code method.** Deferred from v1 scope; with D-2 ruled
  the deferral is unconditional (it remains the named codex zero-paste
  web upgrade if the paste UX bar fails in practice — Approach
  §Variants). Mechanically additive when promoted: poll interval/expiry
  columns + a fourth `method` on the same attempt table.
- **OQ-5 — Org-shared credential enrollment (admin writes the shared org
  key).** Deferred to the managed plane's org entity per RIG-1715
  (`design.md:397-405`); `SetProviderApiKey` extends with a scope argument
  then. Nothing in this design blocks it.
- **OQ-6 — RLS enforcement on the attempt table.** Follows the
  managed-multitenancy record's OQ-2 outcome (RLS vs application-level,
  still Matt's to rule there); handler-level `owner_user_id` scoping is
  enforced here regardless, so this record is correct under either ruling.
- **OQ-8 — Exact per-provider ToS-tier assignments beyond
  Anthropic/OpenAI = restricted.** Non-load-bearing, designed against:
  every provider defaults to RESTRICTED until verified permissive at
  implementation (a per-provider terms check); GLM and Kimi are the
  expected permissive set (the Kodus precedent, Approach §Consumption
  eligibility) but each flag flips only after its own verification. The
  tier is data (`ProviderDefinition` capability field), not frozen
  architecture, so a reassignment never reopens this record.
