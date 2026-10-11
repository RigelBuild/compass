# Compass health endpoint

Tracker: RIG-5191

## Problem / Intent

Compass has no health endpoint. The stack probes liveness with `GetServerInfo`
over the socket (`HealthProber.Probe`, `go/internal/stack/adapters/health.go`);
on the network door it needs a bearer (`classifyProcedure`,
`go/internal/auth/admin_gate.go`). Nothing reports "not serving" or an attached
Runner. This record adds the gRPC health service and a `/healthz` view of one
checker.

## Global Constraints

- Go 1.26.0. Add `connectrpc.com/grpchealth v1.5.0` (Apache-2.0; connect
  v1.11.0 or later, the repo has v1.20.0). Keep `go mod tidy -diff` clean and
  refresh the shared `vendorHash` in `flake.nix` and `guest-image/default.nix`.
- Network-door auth follows the OQ 3 ruling; the Plan assumes A and states the
  B delta. On the socket and dev doors both views sit outside every interceptor
  chain. `classifyProcedure` does not change.
- One checker answers both views. `Check` reads state on each call: no polling
  goroutine, no cache, no store read.
- `Watch` is not implemented. No drain grace in this record.
- Every task is red→green: the failing test lands first.

## Approach

One `serverHealth` checker (`go/server/health.go`) implements
`grpchealth.Checker`. It has two views:

- **gRPC:** `grpc.health.v1.Health/Check`, from `grpchealth.NewHandler` with a
  1 KiB read cap.
- **HTTP:** `GET /healthz`, with an optional `?service=` that defaults to `""`.
  SERVING returns 200, NOT_SERVING 503 and an unknown service 404. The body is
  `serving` or `not_serving`. The `GET` pattern also matches HEAD.

Both mount on the socket and dev doors, and on the network door per OQ 3.

| Service | SERVING when | NOT_SERVING when |
| --- | --- | --- |
| `""` | the door answers | draining |
| `compass.v1.CompassService` | same as `""` | draining |
| `compass.runner` | at least one Runner is attached | none attached, or draining |

Any other name returns NotFound.

**Boot.** `Serve` opens the store and builds the doors before any door serves.

**Drain.** `drainDoors` calls `markDraining` first, then closes the buses and
calls `Shutdown`, which closes listeners at once, so probers rarely see
NOT_SERVING until a drain grace or Watch lands (DL-312). Without Watch, grpc-go
client-side health treats `Unimplemented` as Ready (`health/client.go`) through
a drain.

**Runner.** `compass.runner` means "at least one Runner has a live Sessions
stream". The hub holds one Runner today, so the checker reads
`Hub.RunnerEnrolled`; with several Runners (DL-312) it becomes "any attached".
Not `Hub.EnrolledRunners`: it keeps the last Runner after a disconnect.

**Half-open links (deferred).** A dead link keeps `RunnerEnrolled` true until a
send fails or TCP gives up. Accepted connections (default `net.ListenConfig`
or adopted `--listen-fd`) get Go's keepalive (15s idle, 15s interval, 9
probes): about 150s on Linux. Queued unacked data falls to retransmission
timeouts; behind a proxy, keepalive sees the proxy.
`http.HTTP2Config.SendPingTimeout` covers queued data and L4 passthrough; an
HTTP/2-terminating proxy answers PING itself, needing an application-level
Sessions heartbeat. Each is its own issue.

**Probers** (network rows assume OQ 3 A). A gRPC Check returns NOT_SERVING
inside an OK response, so only probers that read it see a drain.

| Prober | Door | Sees NOT_SERVING |
| --- | --- | --- |
| `grpc-health-probe -tls`, blackbox_exporter `grpc` | network | yes |
| status-code-only gRPC LB check (ALB gRPC) | network | no |
| kubelet `httpGet`, `scheme: HTTPS`, `/healthz` | network | yes (503) |
| status-code HTTP LB check on `/healthz` | network | yes (503) |
| `curl --unix-socket` on `/healthz` (exec probe) | socket | yes |
| stock kubelet `grpc` probe | none | does not work |

The kubelet `grpc` probe is plaintext (TLS needs an alpha gate).
`HealthProber.Probe` keeps `GetServerInfo` for the version.

## Alternatives considered

- **Connect GET only.** `connect.WithIdempotency` enables GET, but NOT_SERVING
  stays 200.
- **`/healthz` with its own logic.** A second source of truth.
- **A drain grace inside `drainDoors`.** It runs after the serve-group cancel
  stopped delivery, presence, forge lanes, sweepers and key refresh, so the
  doors would accept writes this boot cannot finish. A correct grace precedes
  that cancel and fits the 30s NATS drain into the 30s `serverDrainBudget`.
- **StaticChecker plus a hub detach hook.** Gives Watch; needs a new hub seam.
- **Unauthenticated `GetServerInfo`.** Exposes the version, rev and Runner IDs.

## Plan

### H1 — Server health, both views, drain transition

- **Interfaces:**

  ```go
  // go/server/health.go
  const healthzPattern = "GET /healthz"
  const healthMaxReadBytes = 1 << 10
  type serverHealth struct {
      draining atomic.Bool
      grpcPath string
      grpc     http.Handler
  }
  func newServerHealth() *serverHealth
  func (h *serverHealth) Check(ctx context.Context, req *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error)
  func (h *serverHealth) markDraining()
  func (h *serverHealth) serveHealthz(w http.ResponseWriter, r *http.Request)
  func (h *serverHealth) mount(mux *http.ServeMux)
  // go/server/serve.go: serveDoors.health, drainSet.health *serverHealth
  ```

  `serveHealthz` calls `Check` with `r.URL.Query().Get("service")`;
  `CodeNotFound` maps to 404, any other error to 500. `buildDoors` creates one
  `serverHealth` for `udsMux`, `devMux` and, through a new last parameter
  `health *serverHealth` on `buildNetworkServer`, `netMux`. `drainDoors` calls
  `d.health.markDraining()` first. Update the `buildNetworkServer` and
  `drainSet` call sites. **Under OQ 3 A:** amend
  `docs/specs/product/compass.md` §The network door to exempt both views from
  the bearer, with a token-less health scenario. **Under OQ 3 B:** no spec
  change; the gRPC view gets
  `connect.WithInterceptors(otelIC, auth.BearerInterceptor(st))` and
  `/healthz` checks `auth.ResolveToken`. No `AdminGate`: it classifies unknown
  procedures admin-only, admitting only the bootstrap admin.
- **Red → green:**
  1. `TestServerHealthCheck` (`health_test.go`): over h2c, `""` and
     `compass.v1.CompassService` are SERVING and `nope` returns `CodeNotFound`.
     After `markDraining`, both are NOT_SERVING.
  2. `TestHealthzStatusMapping` (`health_test.go`, table, HTTP/1.1): GET
     returns 200 `serving`, HEAD 200 with an empty body, `?service=nope` 404
     and POST 405. After `markDraining`, GET returns 503 `not_serving` and
     HEAD 503 with an empty body.
  3. `TestDrainDoorsMarksNotServingBeforeShutdown` (`drain_test.go`): the door
     serves a listener wrapper whose `Close` records `Check("")`, since
     `Shutdown` closes listeners synchronously but runs `RegisterOnShutdown`
     hooks in goroutines. After one 200 `GET /healthz`, `drainDoors` runs; the
     recorded status must be NOT_SERVING. This pins the mark before
     `Shutdown`, not before the bus closes.
  4. `TestServeHealthOnEveryDoor` (`health_pgtest_test.go`): with no
     credentials, a gRPC Check is SERVING and `GET /healthz` returns 200 on the
     socket, dev and TLS doors. Under B, token-less TLS rows expect
     `Unauthenticated` and 401, and a bearer row expects SERVING.

### H2 — Runner service

- **Interfaces:** `const runnerHealthService = "compass.runner"`.
  `newServerHealth(hub *runnerhub.Hub) *serverHealth`. `Check` returns SERVING
  when `!h.draining.Load() && h.hub != nil && h.hub.RunnerEnrolled()`.
- **Red → green:** `TestServerHealthRunnerService` follows
  `TestGetAgentStatusRunnerEnrollment` with a real hub: NOT_SERVING after
  Enroll, SERVING on attach, NOT_SERVING on detach;
  `/healthz?service=compass.runner` 503, 200, 503. Draining: NOT_SERVING while
  attached.

### H3 — Operator docs

- **Interfaces:** add `## Health checks` to `docs/self-host.md` after
  `## Running under systemd`: names, prober table, socket probe.
  `compass-stack up --socket` sets the socket. Unset, `compass-stack`
  resolves `DefaultSocketPath` in its own environment and passes it as the
  server's `--socket` (`serverSpec`, `go/internal/stack/spec.go`):
  `$XDG_RUNTIME_DIR/compass/server.sock` if absolute, else
  `$HOME/.compass/server.sock`. The socket is 0600; probe as the stack user.
  The sample unit (`User=compass`) has passwd `HOME` and no `XDG_RUNTIME_DIR`:
  `sudo -u compass curl --unix-socket ~compass/.compass/server.sock http://localhost/healthz`.
  The network door requires TLS 1.3 (`loadNetworkTLS`); an ALB needs a TLS 1.3
  listener policy. Point status-code LB checks at `/healthz`, and rate-limit
  both routes upstream until RIG-1298 lands. Alert on `compass.runner`.
- **Red → green:** before H1, that command returns 404. After H1, it prints
  `serving`. `rumdl check` passes.

## Tasks

- [ ] H1 — checker, both views on three doors, drain mark, dependency. Under
  OQ 3 A, the §The network door amendment and token-less health scenario.
- [ ] H2 — `compass.runner` from `Hub.RunnerEnrolled`.
- [ ] H3 — `docs/self-host.md` health-check section.

## Open Questions

1. **Drain grace (load-bearing).** **A (recommended):** drop it; the mark ships
   but is rarely seen. **B:** a grace before the serve-group cancel, budgeting
   the fabric drain.
2. **HTTP prober contract (load-bearing).** **A (recommended):** add
   `/healthz`. **B:** Connect GET only; status-code probers get liveness only.
3. **Network-door auth (load-bearing).** `docs/specs/product/compass.md` §The
   network door says the server "SHALL authenticate every network-door RPC with
   a bearer token", and its token-less scenario rejects any RPC before the
   handler. An anonymous Check contradicts it.
   - **A (recommended):** anonymous on every door; amend that requirement to
     exempt `grpc.health.v1.Health/Check` and `GET /healthz`. Per-request
     bounds: the 1 KiB read cap, header timeout, body-read deadline, no store
     read. Residual: no rate or concurrency limit (no network-door route has
     one), no per-Check span or log, and a `compass.runner` Check takes
     `Hub.mu` and the router mutex, contending with Runner traffic under a
     flood. RIG-1298's per-IP limits and aggregate visibility must cover both
     routes; until then the operator rate-limits them upstream.
   - **B:** anonymous on socket and dev, bearer on the network door. Any valid
     account bearer works (the state-dir bootstrap-admin token or an
     `IssueToken` one); each opens every authenticated-open RPC, and the
     bootstrap one is admin. No scoped `SubjectService` mint path exists. ALB
     health checks cannot send headers; kubelet `httpHeaders` puts the token
     in the pod spec.
   - **Why A:** three statuses, no version or ID; the door already serves
     unauthenticated routes (signed webhooks, the Linear session-link
     redirect). B puts a broad, long-lived credential in prober config.
4. **Does Runner NOT_SERVING change `""`?** No; the server still serves the UI
   and comms.
5. **Ping the database in Check?** No; a restart cannot fix a database outage.
