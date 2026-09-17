# Design: Tier-2 forge e2e leg, agent to server (RIG-3823)

Status: Active

## Problem / Intent

An agent's forge tool call is tested at every seam and assembled nowhere: each layer is verified against a fake of its neighbour, and the podman e2e lane has no forge leg at all (`grep -Ric forge go/e2e/` matches zero files). A `ForgeCallRequest` oneof arm routed to the wrong handler, a field dropped between the TS tool and the proto, or a registry resolving the wrong coordinate is invisible today. DL-210 split forge verification into two tiers; tier 1 (provider to real forge API, the `livegithub` oracle) is done. This record designs tier 2: one podman e2e leg proving a real container agent's forge tool call traverses agent → gateway → runner → hub → `forgeService` → the real provider client, with its payload intact at the provider's HTTP boundary — with no forge credentials and no live egress.

## Approach

### What exists today (grounding)

**Four seams, four fakes.** `packages/compass-agent/src/forge.test.ts` drives the twelve tools against a `FakeTransport`; `go/internal/runner/gateway/forge_test.go` drives the gateway against a `fakeForgeRelay`; the seventeen `TestForgeTransition*` cases in `go/server/forge_transition_test.go` drive the server arm over `forge.FakeProvider` and a fake store; `go/internal/forge/livegithub_test.go` drives the providers against the real APIs. The widest hermetic assembly is `forgeE2EWire` in `go/server/forge_e2e_pgtest_test.go`, which its header scopes precisely:

> the WHOLE agent-initiated forge-WRITE wire over a REAL per-container AgentGateway socket, chokepoint mounted via hub.SetForgeCaller over forge.FakeProvider fakes. Drives every hop: agent -> AgentGateway.Forge -> Runner -> RelayForgeCall -> Hub -> forgeService.

Its "agent" is a test-authored proto sent over the socket, and its provider is a fake. Two hops are therefore never exercised together: the TS tool's parameter-to-proto mapping inside the real container, and the real `*forge.GitHub` client's HTTP request shape.

**The failure class is real.** RIG-3331 T7 found `workflowStatesQuery` in `go/internal/forge/linear.go` declaring its team variable as `String!` where Linear requires `ID` — an HTTP 400 on the first hop of every Linear transition. `main` now carries the fix ("Linear rejects a String! variable in that position outright (http 400)"), but the bug survived three PRs because every test above the provider asserted against a fake that accepted the malformed query, and golden replay pins response bodies, never request text. The same shape as RIG-3326, where a test passed while production was wired to nothing.

**The dispatch under test.** `ExecuteForgeCallAsAccount` in `go/server/forge.go` is the oneof switch:

> case *compassv1internal.ForgeCallRequest_CreateIssue: return s.createIssue(…) … case *compassv1internal.ForgeCallRequest_TransitionIssueState: return s.transitionIssueState(…) … case *compassv1internal.ForgeCallRequest_GetIssue: return s.getIssue(…)

and the TS side of the same field set, `createForgeTools` in `packages/compass-agent/src/forge.ts`:

> case: "createIssue", value: create(CreateIssueRequestSchema, { repo: params.repo, title: params.title, body: params.body ?? "", labels: params.labels ?? [] })

**The harness pattern to copy.** `go/e2e/cannedmodel.go` is the model backend the podman legs settle on, and its header states the posture this record inherits:

> This file is DELIBERATELY UNTAGGED (no `//go:build podman`): the stub is pure net/http with no container dependency, so it compiles in the hermetic (non-podman) unit lane where cannedmodel_test.go exercises it, AND is consumed by the podman-tagged fixture.

`TestCommsPostMessageThroughAgentLoop` in `go/e2e/legcomms_test.go` is the canonical tool-call leg: `NewFixture(ctx, t, WithCannedScript(CannedToolCall("comms_post_message", postArgsJSON), CannedText(settleReply)))`, then `AwaitTurnSettled`, then store-side reads via `store.Open(ctx, f.DSN())`.

**How the server is configured in the harness.** `serverSpec` in `go/internal/stack/spec.go` passes only `--socket / --database / --listen / --tls-cert / --tls-key`, and `stack.Config` has no forge field. But `ProcessSupervisor.Start` in `go/internal/stack/adapters/process.go` inherits the harness process environment:

> cmd.Env = append(os.Environ(), spec.Env...)

and every forge flag in `registerForgeFlags` (`go/cmd/compass-server/main.go`) defaults to an environment variable — `COMPASS_FORGE_HOST`, `COMPASS_FORGE_APP_ID`, `COMPASS_FORGE_INSTALLATION_ID`, `COMPASS_FORGE_APP_KEY_SECRET`, `COMPASS_FORGE_APP_WEBHOOK_SECRET`, `COMPASS_FORGE_REVIEWER_APP_ID`, `COMPASS_FORGE_REVIEWER_APP_INSTALLATION_ID`, `COMPASS_FORGE_REVIEWER_APP_KEY_SECRET` — plus `COMPASS_SECRET_PROVIDER` for `ServeConfig.SecretProvider`. The CI e2e step already uses exactly this channel for the master key ("Stage the secretspec cdylib and seed a throwaway master key" in `.github/workflows/ci.yml` writes `COMPASS_MASTER_KEY=…` to a dotenv and exports `COMPASS_SECRET_PROVIDER=dotenv://…`). The agent container does NOT inherit this environment: `podman.go` in `go/internal/runtime` passes only `spec.Env` as explicit `-e` pairs.

### Decision 1: the boundary is a loopback HTTPS forge stub, not `forge.FakeProvider`

The server is a child process in this lane, so the pgtest wire's `hub.SetForgeCaller` seam is unreachable; the only injectable seam is configuration. And the class of bug this record exists for — a malformed request the fake accepts — lives in the bytes the real provider client emits. So tier 2 stops at the HTTP boundary: a host-side loopback HTTPS server shaped like the GitHub REST API (the "forge stub"), which the real `*forge.GitHub` dials because `apiBase` in `go/internal/forge/github.go` derives the base from the configured host:

> github.com maps to api.github.com; a GHES host maps to https://\<host\>/api/v3.

The stub records every request (method, path, `Authorization`, body) and serves the minimal JSON the client decodes (`ghIssue` needs `number`, `title`, `body`, `state`, `html_url`). Missing rate-limit headers are the no-signal case (`remainingHeader` returns -1; `gateBlocked` never arms), so the stub emits none. The stub is untagged, with a hermetic unit test, exactly like `cannedmodel.go`.

### Decision 2: App credentials are feasible with throwaway keys (the load-bearing feasibility)

`buildForgeWriteService` in `go/server/serve.go` fail-fasts without a primary App client, and App auth mints installation tokens against the forge host. Both are satisfiable inside the lane, with no real credential:

1. **The mint endpoint derives from the same host.** `appAPIBase` in `go/internal/forge/githubapp.go` mirrors `apiBase`, and `mint` POSTs to `%s/app/installations/%d/access_tokens` on it, then decodes `installationToken{Token, ExpiresAt}` and fails only when `out.Token == ""`. A stub answering that path with `{"token":"…","expires_at":"…"}` completes the mint. The JWT the client sends is signed with the configured key but the stub never verifies it — the stub asserts transport, not GitHub's auth.
2. **Any RSA key mints.** `PrivateKey` on `GitHubAppConfig` is a lazily resolved PEM; the parser accepts PKCS#1 or PKCS#8. `testAppPEM` in `go/server/serve_forge_armed_pgtest_test.go` already generates a throwaway `rsa.GenerateKey` PEM for the armed-server pgtest; the fixture does the same. A key generated per run and registered nowhere is not a credential.
3. **The secret path is the dotenv provider.** `buildServerSecretResolver` resolves through `cfg.SecretProvider`; `declareServerSecretNames` declares the App key, App webhook and reviewer key names wrapped by `serverSecretName` (`SERVER_` + name), and `buildManifest` in `go/internal/secrets/resolver.go` marks every declared name `required = true`, so the dotenv must hold all three plus `COMPASS_MASTER_KEY`. `writeDeclaredDotenv` and `dotenvValue` in the armed pgtest show the exact escaping a multiline PEM needs in dotenv. The fixture's postgres is private and fresh, so a fresh master key passes `reconcileKeyState` as a first boot.
4. **Mint is lazy.** `Token` on `appTokenSource` mints on first use, and no boot path calls it: `buildBoardIngestLane` seeds only `cfg.Forge.SeedRepos` (left empty), and both reconcilers sweep `ListEnabledRepos`/notify targets from the store, which hold no rows. The first request the stub sees is the leg's own mint, so the stub's request log is a complete ledger of forge traffic.
5. **Boot gates.** `forgeWritesEnabled` requires both App ids non-zero and both key names declared; `validateForgeSecret` resolves them once. The fixture sets all eight `COMPASS_FORGE_*` variables above; `COMPASS_FORGE_REPOS` stays unset.

### Decision 3: TLS trust rides `SSL_CERT_FILE`

`NewGitHub` and `NewAppTokenSource` fall back to `&http.Client{Timeout: 30 * time.Second}` — system roots, no injectable pool from config. The Go toolchain's `loadSystemRoots` in `crypto/x509/root.go` honours `SSL_CERT_FILE` ("If set this overrides the system default"), so the fixture exports the stub's self-signed PEM through that variable in the harness environment the server inherits. Nothing else is affected: the harness clients (`newAuthedClients` in `go/e2e/clients.go`) and the runner both pin the stack anchor through `runner.NewCATrustClient`, which builds its own pool, and the container never sees the variable. The stub's certificate comes from `certgen.Generate([]string{"127.0.0.1"}, 0)` in `go/internal/certgen` — the same generator that mints the stack's own anchor, so one certificate convention.

Adding a `--forge-ca` flag or a `Client` on `ForgeConfig` would be production surface for a test-only need; rejected (OQ-2 records the alternative).

### Decision 4: three arms earn the leg; one leg, one stack

The leg scripts three tool calls in one agent turn, chosen so every transport variant and every payload shape crosses the wire once:

| Tool | Why it earns a slot | What the stub sees |
| --- | --- | --- |
| `forge_create_issue` | The create arm: stamp (`StampOwner`), F3 dedup, DL-055 row; `title`/`body`/`labels` cross TS → proto → JSON | `POST /api/v3/repos/{repo}/issues` with the stamped body |
| `forge_get_issue` | The read arm; owner header stripped on return; a `uint64` number crosses as a `bigint` | `GET /api/v3/repos/{repo}/issues/{n}` |
| `forge_transition_issue_state` | The RIG-3331 arm that was dead; `state` + `close_reason` → `state_reason`; `forge_state_transitions` memo | `PATCH /api/v3/repos/{repo}/issues/{n}` with `{"state":"closed","state_reason":"not_planned"}` |

PR, review and comment arms add no new transport variant; subscribe is store-only. A stack bring-up costs minutes, so this is one own-stack leg (`NewFixture`, not `sharedFixture`: the leg sets process environment with `t.Setenv`, which forbids `t.Parallel`; no e2e leg is parallel today).

### Decision 5: the tool call comes from the canned model, not a direct gateway call

The pgtest wire already drives a hand-built `ForgeCallRequest` through gateway → hub → service. What tier 2 adds is the real container agent turning model-emitted arguments into that proto, so the leg drives the turn through `WithCannedScript` exactly as the comms leg does. The stub is deterministic (it returns a fixed issue number), so the get and transition turns hard-code that number.

### Decision 6: Linear is out of this leg's reach without a production seam

`buildLinearTokenSource` in `go/server/serve.go` builds `linearagent.NewTokenSource(clientID, clientSecret, &http.Client{…}, "")`, and `NewTokenSource` in `go/internal/linearagent/client.go` resolves the empty URL to the constant `https://api.linear.app/oauth/token`, then performs a boot-time `tokens.Token(ctx)` that fails `Serve` on error. `buildLinearNotifyLane` hard-codes `host = "linear.app"` and builds `forge.NewLinear(forge.LinearConfig{Token: tokens, Log: log})` with no `Host`, although `LinearConfig` accepts one. So configuring Linear in the lane would egress to `api.linear.app` at boot. A Linear leg needs a config-exposed endpoint/token-URL override first; that is a design ruling on production config, parked as OQ-1, not smuggled into this record.

A related finding from the same reading, since verified hop-by-hop against `origin/main` and filed as **RIG-3870**: `buildForgeWriteService` registers the Linear write coordinate with an empty host (`forgeCoordinate{provider: LINEAR}`), `register` makes that empty string the provider's `defaultHost` so the A3 empty-host fallback cannot rescue it, `resolve` carries it into `resolvedForge.host`, and `record` passes it to `RecordAuthoredArtifact`, whose `validCoordinate` in `go/internal/store/forge_cursors.go` rejects `host == ""` with "forge host is required". A Linear `forge_create_issue` therefore creates the issue on Linear and then returns an in-band `invalid_argument` with no DL-055 row and no F3 memo. It is out of this record's scope and tracked separately.

### Redden proof

The acceptance criterion is that a mis-plumbed arm or a dropped field turns the leg red. Mapping:

- Dropped or renamed TS field (`labels`, `close_reason`) → the stub's recorded JSON lacks it → assertion on the decoded body fails.
- Wrong oneof arm on either side (e.g. `GetIssue` dispatched to `listIssues`) → wrong method or path in the stub log → fails.
- Wrong coordinate or host → connection refused → in-band `ForgeCallError` → the transcript never carries the tool's ack text → fails.
- Wrong credential role (reviewer token on an author write) → the stub's per-App mint tokens differ and the `Authorization` assertion fails.

T3 proves two of these by throwaway mutation before the PR is promoted. One constraint the harness already carries: the TS under test is whatever `compass-agent:latest` holds, which CI rebuilds from the tree when the image inputs change (the `image_affected` step in `ci.yml`) and pulls otherwise.

## Global Constraints

- **No forge credentials, no live egress.** Keys are generated per run and registered nowhere; the stub binds loopback; `EgressAllow` and the container's firewall are untouched because only the server process dials the stub. `COMPASS_FORGE_REPOS` stays unset so no boot sweep exists.
- **Stub untagged, leg tagged.** `forgestub.go` and its unit test have no build tag; the leg is `//go:build podman` and an own-stack `NewFixture` leg with no `t.Parallel`.
- **Environment reaches the server through inheritance** (`t.Setenv` before `NewFixture`); no change to `stack.Config`, `serverSpec`, `go/server/**` or `go/internal/forge/**`.
- **Citations** in code comments and the PR use `` `symbol` in `path` ``; comments 1–2 lines (hard ceiling 4); markdown passes `rumdl check`.
- **Assertion posture** mirrors the existing legs: bounded waits (`AwaitTurnSettled`, `awaitTranscriptPersisted`), no sleeps, store reads via `store.Open(ctx, f.DSN())`, test root `context.Background()` with every bound derived from it.
- **Names.** Secret names are the leg's own (`FORGE_APP_PRIVATE_KEY`, `FORGE_APP_WEBHOOK_SECRET`, `FORGE_REVIEWER_APP_PRIVATE_KEY`), written to the dotenv wrapped as `SERVER_<name>`; the repo is `e2e-owner/e2e-repo`; the stub's issue number is a package constant.

## Plan

### T0 — the forge stub (`go/e2e/forgestub.go`, untagged)

A loopback HTTPS server shaped like the GitHub REST API on `/api/v3`, recording every request. Serves: `POST /app/installations/{id}/access_tokens` → 201 `{"token": <per-App token>, "expires_at": now+1h}`; `POST /repos/{owner}/{name}/issues` → 201 `ghIssue` with the fixed number and the echoed title/body; `GET /repos/{owner}/{name}/issues/{n}` → 200; `PATCH …/issues/{n}` → 200 echoing the requested `state`. Anything else → 404 with a body naming the path (a test bug, never a hang — the canned model's exhaustion posture). Certificate from `certgen.Generate`, PEM written under a `t.TempDir()`.

Interfaces:

```go
// newForgeStub starts the stub on a loopback TLS listener; t.Cleanup closes it.
func newForgeStub(t *testing.T) *forgeStub
func (s *forgeStub) Host() string                 // "127.0.0.1:<port>" — the COMPASS_FORGE_HOST value
func (s *forgeStub) CAPath() string               // PEM path for SSL_CERT_FILE
func (s *forgeStub) MintToken(appID int64) string // deterministic per-App installation token
func (s *forgeStub) Requests() []forgeStubRequest // copy, in arrival order

type forgeStubRequest struct {
	Method, Path, Authorization string
	Body                        []byte
}

const forgeStubIssueNumber uint64 = 4242
```

Hermetic test (`forgestub_test.go`): a real `forge.NewAppTokenSource` + `forge.NewGitHub` pointed at `Host()` with a client trusting `CAPath()` performs a mint and a `CreateIssue`, and the recorded requests carry the expected method, path, bearer and body. That test is the stub's own contract and proves the provider-client compatibility before any container is involved.

### T1 — fixture option `WithForgeStub` (`go/e2e/fixture.go`)

`configureForgeStub(t, stub)` mirrors `configureCannedModel`: generates two RSA PEMs (primary App id 1001, reviewer App id 1002, installation ids 1 and 2), writes a dotenv (`COMPASS_MASTER_KEY` fresh 64-hex + the three `SERVER_`-wrapped keys, values escaped as `dotenvValue` does), and `t.Setenv`s `COMPASS_SECRET_PROVIDER=dotenv://<path>`, `SSL_CERT_FILE=<stub CA>`, `COMPASS_FORGE_HOST=<stub host>` and the seven App variables. `Fixture` gains `ForgeStub() *forgeStub`. The option must run before `stack.Up` (env is captured at `Start`).

Interfaces:

```go
func WithForgeStub() fixtureOption
func (f *Fixture) ForgeStub() *forgeStub
```

The small dotenv escaping helper duplicates `dotenvValue` from the server package's test file (unexported, un-importable); the copy names its twin in a one-line comment.

### T2 — the leg (`go/e2e/legforge_test.go`, `//go:build podman`)

`TestForgeCallsThroughAgentLoop`: skip guard, `ctx := context.Background()`, `NewFixture(ctx, t, WithForgeStub(), WithCannedScript(create, get, transition, CannedText(settleReply)))`, agent create/provision/session/tail/post as in the comms leg, `AwaitTurnSettled`, `awaitTranscriptPersisted(ctx, st, sessionID, settleReply)`. Assertions, in order:

1. `f.ForgeStub().Requests()` is exactly four: one mint for App 1001, then `POST`, `GET`, `PATCH` on the repo paths; every API request's `Authorization` equals `"Bearer " + MintToken(1001)`.
2. The `POST` body decodes to `title` verbatim, `labels` verbatim, and a `body` that contains the original text and exactly one `<!-- compass:owner ` header naming the leg agent's handle.
3. The `PATCH` body is `{"state":"closed","state_reason":"not_planned"}`.
4. The transcript contains `Created issue #4242 in e2e-owner/e2e-repo:` (the `createAck` text) and the transition ack.
5. Store: `AuthoredArtifactByCoordinate(ctx, ForgeProviderGitHub, stub.Host(), repo, ForgeArtifactKindIssue, 4242)` names the leg agent; `ConsumeStateTransition(ctx, …, "closed", fresh)` returns the same account.

Canned arguments are the JSON the model would emit, e.g. `{"repo":"e2e-owner/e2e-repo","title":"tier-2 leg","body":"hello from the leg","labels":["e2e"]}` and `{"repo":"e2e-owner/e2e-repo","issue_number":4242,"state":"closed","close_reason":"not_planned"}`.

### T3 — redden proof (throwaway, recorded in the PR body)

Two mutations, each reverted before push: drop `labels` from the `createIssue` mapping in `forge.ts` (rebuild the image, run the leg → red on assertion 2); swap the `ForgeCallRequest_GetIssue` case body for `s.listIssues` in `go/server/forge.go` (run the leg → red on assertion 1). Paste the two failure lines into the PR body. No permanent test is added for the mutations.

### T4 — ledger row (proposed for the driver; this record does not edit `DECISIONS.md`)

DL-next: "Tier-2 forge e2e stops at the provider's HTTP boundary: the podman lane dials a loopback HTTPS forge stub through the real provider client and App-token mint path, with per-run generated keys that are never registered; forge credentials never enter the deterministic tier, and Linear stays on tiers 1 and golden until a config-exposed endpoint override exists."

## Tasks

- [ ] T0 — `go/e2e/forgestub.go` + `forgestub_test.go` (untagged): loopback HTTPS GitHub-shaped stub with request log, mint endpoint, and the hermetic provider-client round-trip test.
- [ ] T1 — `WithForgeStub` fixture option: throwaway RSA keys, dotenv, `t.Setenv` of the forge/secret/TLS variables, `Fixture.ForgeStub()`.
- [ ] T2 — `TestForgeCallsThroughAgentLoop`: three-call canned turn, stub/transcript/store assertions.
- [ ] T3 — two throwaway mutations proving the leg reddens; evidence in the PR body.
- [ ] T4 — ledger row proposed to the driver.

## Open Questions

- **OQ-1 (Matt): Linear coverage.** Options: (a) add a config-exposed Linear endpoint + token-URL override (`ForgeConfig` → `LinearConfig.Host` and `NewTokenSource`'s `tokenURL`) as its own issue, then a Linear leg against a GraphQL-shaped stub; (b) leave Linear on tier 1 and golden replay. Recommendation: (a), scheduled after this leg lands. Note that RIG-3870 (the empty-host `record` path in Decision 6) needs its own pgtest regardless of this ruling — that bug is not gated on a Linear e2e leg.
- **OQ-2 (Matt): trust anchor.** `SSL_CERT_FILE` in the inherited environment (recommended, zero production surface) versus a `--forge-ca` flag on the server.
- **OQ-3 (Matt): environment delivery.** `t.Setenv` inside the fixture option (recommended; no `stack.Config` change) versus a `ServerEnv` field on `stack.Config` threaded through `serverSpec`.
- **OQ-4: leg scope.** Three arms as designed (recommended) versus adding comment/PR arms, which add assertions but no new transport variant.

This record runs over the design-record target; the extra length sits in *Decision 2* (the App-credential feasibility the brief required resolved from code rather than asserted) and *Decision 6* (the Linear infeasibility and the latent finding), both of which a reader must have to accept the tiering.
