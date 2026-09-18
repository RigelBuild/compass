# Design: Tier-2 forge e2e leg, agent to server (RIG-3823)

Status: Active

## Problem / Intent

An agent's forge tool call is tested at every seam and assembled nowhere: each layer is verified against a fake of its neighbour, and the podman e2e lane has no forge leg at all. `grep -Ric forge go/e2e/` matches exactly one line across 27 files, and it is a comment in `NewFixture` explaining why the fixture's dotenv is complete *because* it configures no forge — no e2e leg exercises a forge call. A `ForgeCallRequest` oneof arm routed to the wrong handler, a field dropped between the TS tool and the proto, or a registry resolving the wrong coordinate is invisible today. DL-210 split forge verification into two tiers; tier 1 (provider to real forge API, the `livegithub` oracle) is done. This record designs tier 2: one podman e2e leg proving a real container agent's forge tool call traverses agent → gateway → runner → hub → `forgeService` → the real provider client, with its payload intact at the provider's HTTP boundary — with no forge credential and no live egress.

## Approach

### What exists today (grounding)

**Four seams, four fakes.** `packages/compass-agent/src/forge.test.ts` drives the twelve tools against a `FakeTransport`; `go/internal/runner/gateway/forge_test.go` drives the gateway against a `fakeForgeRelay`; the thirteen `TestForgeTransition*` functions (22 subtest leaves) in `go/server/forge_transition_test.go` drive the server arm over `forge.FakeProvider` and a fake store; `go/internal/forge/livegithub_test.go` drives the providers against the real APIs. The widest hermetic assembly is `forgeE2EWire` in `go/server/forge_e2e_pgtest_test.go`, which its header scopes precisely:

> the WHOLE agent-initiated forge-WRITE wire over a REAL per-container AgentGateway socket, chokepoint mounted via hub.SetForgeCaller over forge.FakeProvider fakes. Drives every hop: agent -> AgentGateway.Forge -> Runner -> RelayForgeCall -> Hub -> forgeService.

Its "agent" is a test-authored proto sent over the socket, and its provider is a fake. Two hops are therefore never exercised together: the TS tool's parameter-to-proto mapping inside the real container, and the real `*forge.GitHub` client's HTTP request shape.

**The failure class is real.** RIG-3331 T7 found `workflowStatesQuery` in `go/internal/forge/linear.go` declaring its team variable as `String!` where Linear requires `ID` — an HTTP 400 on the first hop of every Linear transition. `main` now carries the fix ("Linear rejects a String! variable in that position outright (http 400)"), but the bug survived because tests above the provider asserted against a fake that accepted the malformed query, and golden replay pins response bodies, never request text.

**The dispatch under test.** `ExecuteForgeCallAsAccount` in `go/server/forge.go` is the oneof switch, and `createForgeTools` in `packages/compass-agent/src/forge.ts` maps tool parameters into the same field set:

> case: "createIssue", value: create(CreateIssueRequestSchema, { repo: params.repo, title: params.title, body: params.body ?? "", labels: params.labels ?? [] })

**The harness pattern to copy.** `go/e2e/cannedmodel.go` is deliberately untagged for the same reason the forge stub is: its pure `net/http` server is unit-tested without podman and consumed by the tagged fixture. `TestCommsPostMessageThroughAgentLoop` in `go/e2e/legcomms_test.go` is the canonical tool-call leg.

**How the server is configured in the harness.** `serverSpec` in `go/internal/stack/spec.go` passes only `--socket / --database / --listen / --tls-cert / --tls-key`, and `stack.Config` has no forge field. `ProcessSupervisor.Start` inherits the harness process environment:

> cmd.Env = append(os.Environ(), spec.Env...)

The forge flags in `registerForgeFlags` (`go/cmd/compass-server/main.go`) resolve from `COMPASS_FORGE_*`; the fixture scrubs unrelated forge and Linear variables, then sets only the stub host, App ids, installation ids, and secret names before `stack.Up`. The CI e2e step no longer exports the master key or secret provider; commit `c328d25d` moved that responsibility into the fixture's dotenv, which this design extends. The agent container does not inherit the server environment.

### Decision 1: the boundary is a loopback HTTPS forge stub, not `forge.FakeProvider`

The server is a child process in this lane, so the pgtest wire's `hub.SetForgeCaller` seam is unreachable; the only injectable seam is configuration. And the class of bug this record exists for — a malformed request the fake accepts — lives in the bytes the real provider client emits. So tier 2 stops at the HTTP boundary: a host-side loopback HTTPS server shaped like the GitHub REST API (the "forge stub"), which the real `*forge.GitHub` dials because `apiBase` in `go/internal/forge/github.go` derives the base from the configured host:

> github.com maps to api.github.com; a GHES host maps to https://\<host\>/api/v3.

The stub records every request (method, path, `Authorization`, body) and serves the minimal JSON the client decodes (`ghIssue` needs `number`, `title`, `body`, `state`, `html_url`). Missing rate-limit headers are the no-signal case (`remainingHeader` returns -1; `gateBlocked` never arms), so the stub emits none. The stub is untagged, with a hermetic unit test, exactly like `cannedmodel.go`.

### Decision 2: App credentials are feasible with throwaway keys (the load-bearing feasibility)

`buildForgeWriteService` in `go/server/serve.go` fail-fasts without a primary App client, and App auth mints installation tokens against the forge host. Both are satisfiable inside the lane, with no real credential:

1. **The mint endpoint derives from the same host.** `appAPIBase` in `go/internal/forge/githubapp.go` mirrors `apiBase`, and `mint` POSTs to `%s/app/installations/%d/access_tokens` on it. With the non-`github.com` stub host, that means `/api/v3/app/installations/{id}/access_tokens`; the REST issue routes use the same prefix. The response decodes `installationToken{Token, ExpiresAt}` and fails only when `out.Token == ""`. A stub answering that path with `{"token":"…","expires_at":"…"}` completes the mint. The JWT is signed with the configured key but the stub never verifies it.
2. **Any RSA key mints.** `PrivateKey` on `GitHubAppConfig` is a lazily resolved PEM; the parser accepts PKCS#1 or PKCS#8. `testAppPEM` in `go/server/serve_forge_armed_pgtest_test.go` already generates a throwaway `rsa.GenerateKey` PEM for the armed-server pgtest; the fixture does the same. A key generated per run and registered nowhere is not a credential.
3. **The secret path extends the fixture's existing dotenv — it cannot be a second provider.** `NewFixture` in `go/e2e/fixture.go` writes one provider file and pins it on `stack.Config`; `serverSpec` passes it as an explicit flag, so `COMPASS_SECRET_PROVIDER` cannot replace it. `WithForgeStub` records a forge option in `fixtureConfig`; at the existing dotenv write site, `NewFixture` starts the stub, generates the two keys, appends the three `SERVER_`-wrapped entries using the `dotenvValue` escaping pattern, sets the forge variables and `COMPASS_FORGE_CA`, stores the stub in the eventual `Fixture`, then calls `stack.Up`. The option owns cleanup through `t.Cleanup`; it does not set a second provider. `buildManifest` requires every declared name, so declarations and values are written together.
4. **Mint is lazy.** `Token` on `appTokenSource` mints on first use, and no boot path calls it: `buildBoardIngestLane` seeds only `cfg.Forge.SeedRepos` (left empty), and both reconcilers sweep empty store targets. The first request the stub sees is the leg's own mint, so the stub's request log is a complete ledger of forge traffic.
5. **Boot gates.** `forgeWritesEnabled` requires both App ids non-zero and both key names declared; `validateForgeSecret` resolves them once. The fixture sets all eight `COMPASS_FORGE_*` variables above; `COMPASS_FORGE_REPOS` and all Linear selector variables are scrubbed.

### Decision 3: TLS trust uses a dedicated forge CA flag

`NewGitHub` and `NewAppTokenSource` use default clients without an injectable pool. Add a dedicated `--forge-ca`/`COMPASS_FORGE_CA` server setting that installs the stub PEM as the forge clients' trust pool only. The harness clients and runner continue to pin the stack anchor through `runner.NewCATrustClient`. The stub certificate comes from `certgen.Generate([]string{"127.0.0.1"}, 0)` in `go/internal/certgen`.

### Decision 4: five arms earn the leg; one leg, one stack

The leg scripts five tool calls in one agent turn, chosen so every transport variant and payload shape crosses the wire once:

| Tool | Why it earns a slot | What the stub sees |
| --- | --- | --- |
| `forge_create_issue` | Create arm: stamp, F3 dedup, DL-055 row; `title`/`body`/`labels` cross TS → proto → JSON | `POST /api/v3/repos/{repo}/issues` |
| `forge_get_issue` | Read arm; owner header stripped on return; a `uint64` number crosses as a `bigint` | `GET /api/v3/repos/{repo}/issues/{n}` |
| `forge_transition_issue_state` | RIG-3331 arm; `state` + `close_reason` → `state_reason`; transition memo | `PATCH /api/v3/repos/{repo}/issues/{n}` |
| `forge_comment_on_issue` | Comment write body crosses the author client and stamped-body path | `POST /api/v3/repos/{repo}/issues/{n}/comments` |
| `forge_create_pull_request` | PR create adds the PR payload and result arm | `POST /api/v3/repos/{repo}/pulls` |

The get arm reaches the provider because the issue projection is empty in this lane: no forge subscriptions, webhook deliveries, or ingest rows exist. Review adds no new transport variant and subscribe is store-only. This remains one own-stack leg using `NewFixture`; the fixture option sets environment before `stack.Up`.

### Decision 5: the tool call comes from the canned model, not a direct gateway call

The pgtest wire already drives a hand-built `ForgeCallRequest` through gateway → hub → service. What tier 2 adds is the real container agent turning model-emitted arguments into that proto, so the leg drives the turn through `WithCannedScript` exactly as the comms leg does. The stub is deterministic (it returns a fixed issue number), so the get and transition turns hard-code that number.

### Decision 6: Linear follows after a production endpoint seam

`buildLinearTokenSource` in `go/server/serve.go` uses a hardcoded token URL and performs a boot-time mint; a later issue will expose the API host and token URL before adding a GraphQL-shaped stub leg. This record keeps Linear on tier 1 and golden replay until that seam lands. RIG-3870 remains a separate pgtest because its empty-host write-coordinate failure is independent of this e2e leg.

### Redden proof

The acceptance criterion is that a mis-plumbed arm or dropped field turns the leg red:

- Dropped `labels` or `close_reason` → the stub's recorded JSON lacks the field → its body assertion fails.
- A valid wrong-arm mutation constructs a `ListIssuesRequest` from the GetIssue repo; the list path differs from `/issues/{n}` → request-log assertion fails.
- A wrong coordinate or host → connection refused → in-band `ForgeCallError` → the bounded turn never settles.
- A reviewer-token-on-author-write mutation adds a second mint and changes the bearer → the exact request ledger fails.

T3 records these four throwaway mutations and their failure lines in the PR body. The TS image must be rebuilt when the image inputs change; the `image_affected` step in `ci.yml` controls that path.

## Global Constraints

- **No forge credentials, no live egress.** Keys are generated per run and registered nowhere; the stub binds loopback. Ambient forge and Linear variables are scrubbed; only loopback forge values and the dedicated forge CA are set. `COMPASS_FORGE_REPOS` stays unset.
- **Stub untagged, leg tagged.** `forgestub.go` and its unit test have no build tag; the leg is `//go:build podman`.
- **Environment reaches the server through inheritance** (`t.Setenv` inside the fixture option, before `stack.Up`). The dedicated forge CA is a new server/stack configuration surface; the implementation updates its flag/config plumbing and forge client construction.
- **Assertions** use bounded waits, no sleeps or retries, and store reads through `store.Open(ctx, f.DSN())`.
- **Citations** use `` `symbol` in `path` ``; comments stay within the repository's 1–2 line norm; markdown passes `rumdl check`.

### T0 — the forge stub (`go/e2e/forgestub.go`, untagged)

A loopback HTTPS server records every request and serves the GitHub-shaped `/api/v3` routes emitted by the real client: `POST /api/v3/app/installations/{id}/access_tokens` → 201 with a token keyed by installation id; `POST /api/v3/repos/{owner}/{name}/issues` → 201; `GET /api/v3/repos/{owner}/{name}/issues/{n}` → 200; `PATCH /api/v3/repos/{owner}/{name}/issues/{n}` → 200; `POST /api/v3/repos/{owner}/{name}/issues/{n}/comments` → 201; and `POST /api/v3/repos/{owner}/{name}/pulls` → 201. Each response includes `number`, `title`, `body`, `state`, `html_url`, `user.login`, and labels where applicable, so every `ghIssue` decode is observable. Anything else returns 404. Certificate from `certgen.Generate`, written under `t.TempDir()`.

Interfaces:

```go
func newForgeStub(t *testing.T) *forgeStub
func (s *forgeStub) Host() string
func (s *forgeStub) CAPath() string
func (s *forgeStub) MintToken(installationID int64) string
func (s *forgeStub) Requests() []forgeStubRequest
const forgeStubIssueNumber uint64 = 4242
```

The hermetic provider-client test uses a custom trust client pointed at `CAPath`, performs a mint and `CreateIssue`, and asserts the recorded `/api/v3` paths, bearer, and body.

### T1 — fixture option `WithForgeStub` (`go/e2e/fixture.go`)

`WithForgeStub()` records a forge-enabled option. At the existing dotenv write site, `NewFixture` constructs the stub, generates two RSA PEMs (primary App 1001 / installation 1 and reviewer App 1002 / installation 2), appends the three `SERVER_`-wrapped key entries using `dotenvValue` escaping, sets `COMPASS_FORGE_HOST`, the seven App variables, and `COMPASS_FORGE_CA`, and stores the stub in `Fixture`. It scrubs all unrelated `COMPASS_FORGE_*`, `COMPASS_SECRET_PROVIDER`, and Linear selector variables before setting the intended values. `t.Cleanup` closes the stub. This runs before `stack.Up`; no option accesses a future dotenv path.

Interfaces:

```go
func WithForgeStub() fixtureOption
func (f *Fixture) ForgeStub() *forgeStub
```

The dotenv helper duplicates the unexported `dotenvValue` escaping pattern from `go/server/serve_forge_armed_pgtest_test.go`. The leg's five canned arguments use distinct payloads. The stub echoes meaningful `number`, `title`, `body`, `state`, `html_url`, `user.login`, and labels; assertions check decoded GET/PATCH/PR results, not only request logs. Capture `fresh := time.Now().Add(-time.Minute)` immediately before triggering the turn, then require `ConsumeStateTransition(ctx, ForgeProviderGitHub, stub.Host(), repo, ForgeArtifactKindIssue, 4242, "closed", fresh)` to return the agent account with `ok=true` and `err=nil`.

### T2 — the leg (`go/e2e/legforge_test.go`, `//go:build podman`)

Drive five scripted tools from the canned model: create issue, get issue, transition issue, comment on issue, and create pull request. Use the existing agent create/provision/session/tail/post sequence and bounded transcript waits. Assert the request ledger has one installation-token mint plus the five API calls, with the expected paths and author bearer; assert create, transition, comment, and PR bodies plus decoded response fields; assert the create ack and fence-independent transition state text; and assert the authored-artifact row plus `ConsumeStateTransition` with `ok=true`, `err=nil`, and `fresh` captured before the scripted turn.

### T3 — redden proof (throwaway, recorded in the PR body)

Run four valid mutations, reverting each before push: drop `labels`; drop `close_reason`; dispatch GetIssue through a constructed `ListIssuesRequest`; and use the reviewer client for an author write. Rebuild the image where required, run the leg, and record each bounded failure line. A wrong host mutation is covered by the request ledger and needs no separate image build.

### T4 — ledger row

Add the exact tier-2 boundary row to `DECISIONS.md` in the existing forge ledger section before implementation begins. The row states that the podman lane uses a loopback HTTPS stub through the real provider client and App-token mint path, with per-run generated keys, no forge credentials in the deterministic tier, and Linear deferred until endpoint and token-URL overrides exist.

## Tasks

- [ ] T0 — untagged forge stub and hermetic provider-client test.
- [ ] T1 — fixture option, dotenv extension, forge CA setting, and scrubbed environment.
- [ ] T2 — five-call canned agent loop and request/decode/store assertions.
- [ ] T3 — four valid throwaway mutations with recorded red failures.
- [ ] T4 — exact ledger row before implementation.

## Open Questions

None for this leg. Linear endpoint/token-URL overrides and a later GraphQL stub leg are a follow-up design; RIG-3870 remains a separate pgtest.
