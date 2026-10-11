# Compass health endpoint

Tracker: RIG-5191

## Problem / Intent

Compass has no health endpoint. The stack probes liveness with `GetServerInfo`
over the socket (`HealthProber.Probe`, `go/internal/stack/adapters/health.go`).
On the network door that RPC needs a bearer (`classifyProcedure`,
`go/internal/auth/admin_gate.go`). Nothing answers "not serving" or reports an
attached Runner. This record adds the gRPC health service and a status-code
`/healthz` view of one checker.

## Global Constraints

- Go 1.26.0. Add `connectrpc.com/grpchealth v1.5.0` (Apache-2.0; needs connect
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

**Drain.** `drainDoors` calls `markDraining` before closing the buses and
calling `Shutdown`, which closes listeners at once. A prober rarely sees
NOT_SERVING until a drain grace or Watch lands with the multi-instance work
(DL-312). Without Watch, grpc-go client-side health reads `Unimplemented` as
healthy (`health/client.go`) and reports Ready through a drain.

**Runner.** `compass.runner` means "at least one Runner has a live Sessions
stream". The hub holds one Runner today ("single-Runner MVP"), so the checker
reads `Hub.RunnerEnrolled`; with several Runners (DL-312) it becomes "any
attached". Not `Hub.EnrolledRunners`: it keeps the last Runner after a
disconnect.

**Half-open links (deferred).** A dead link keeps `RunnerEnrolled` true until a
send fails or TCP gives up. The door's zero `net.ListenConfig`, like an adopted
`--listen-fd` listener, gives accepted connections Go's keepalive: 15s idle,
15s interval, 9 probes, about 150s on Linux. Queued unacked data falls to
retransmission timeouts instead, and behind a proxy keepalive sees the proxy.
`http.HTTP2Config.SendPingTimeout` would cover both; it needs its own issue.

**Probers.** Network-door rows assume OQ 3 A.

| Prober | Door | Reads NOT_SERVING |
| --- | --- | --- |
| `grpc-health-probe -tls`, or a TLS gRPC LB check | network | yes |
| kubelet `httpGet`, `scheme: HTTPS`, `/healthz` | network | yes (503) |
| status-code HTTP LB check on `/healthz` | network | yes (503) |
| `curl --unix-socket` on `/healthz` (exec probe) | socket | yes |
| stock kubelet `grpc` probe | none | does not work |

The kubelet `grpc` probe is plaintext; TLS needs the alpha
`GRPCContainerProbeTLS` gate. `HealthProber.Probe` keeps `GetServerInfo` for
the version.

## Alternatives considered

- **Connect GET only.** `connect.WithIdempotency` enables GET, but NOT_SERVING
  stays 200, so status-code probers read a drain as healthy.
- **`/healthz` with its own logic.** A second source of truth.
- **A drain grace inside `drainDoors`.** It runs after the serve-group cancel,
  when delivery, presence, forge lanes, usage sweepers and key refresh have
  stopped, so held-open doors accept writes this boot cannot finish. A correct
  grace precedes that cancel and fits the NATS fabric drain (30s default) into
  `serverDrainBudget` (30s): its own design.
- **StaticChecker plus a hub detach hook.** Gives Watch, but needs a new hub
  seam; the hub has only `SetRunnerReadyHook`.
- **A plaintext health-only listener** for the kubelet `grpc` probe. Deferred.
- **Unauthenticated `GetServerInfo`.** Exposes the version, rev and Runner IDs;
  no NOT_SERVING.
- **A per-Runner listener.** Runners dial out; it proves only the process is
  up.

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

  `serveHealthz` calls `Check` with `r.URL.Query().Get("service")` and maps
  `CodeNotFound` to 404 and any other error to 500. `buildDoors` creates one
  `serverHealth`, mounts it on `udsMux` and `devMux`, and passes it to
  `buildNetworkServer` (new last parameter `health *serverHealth`) for
  `netMux`. `drainDoors` calls `d.health.markDraining()` first. Update the
  existing `buildNetworkServer` and `drainSet` call sites. Amend
  `docs/specs/product/compass.md` §The network door to exempt both views from
  the bearer requirement, with a token-less health scenario. **Under OQ 3 B:**
  no spec change; the gRPC view gets
  `connect.WithInterceptors(otelIC, auth.BearerInterceptor(st))` (no
  `AdminGate`: it denies unknown procedures), and `/healthz` checks the bearer
  with `auth.ResolveToken`.
- **Red → green:**
  1. `TestServerHealthCheck` (`health_test.go`): over h2c, `""` and
     `compass.v1.CompassService` are SERVING and `nope` returns `CodeNotFound`.
     After `markDraining`, both are NOT_SERVING.
  2. `TestHealthzStatusMapping` (`health_test.go`, table, HTTP/1.1): GET
     returns 200 `serving`, HEAD 200 with an empty body, `?service=nope` 404
     and POST 405. After `markDraining`, GET returns 503 `not_serving` and
     HEAD 503 with an empty body.
  3. `TestDrainDoorsMarksNotServingBeforeShutdown` (`drain_test.go`): the door
     serves a listener wrapper whose `Close` records `Check("")`. `Shutdown`
     closes listeners synchronously but runs `RegisterOnShutdown` hooks in
     goroutines. After one 200 `GET /healthz`, `drainDoors` runs; the recorded
     status must be NOT_SERVING.
  4. `TestServeHealthOnEveryDoor` (`health_pgtest_test.go`): with no
     credentials, a gRPC Check is SERVING and `GET /healthz` returns 200 on the
     socket, dev and TLS doors. Under B, token-less TLS rows expect
     `Unauthenticated` and 401, and a bearer row expects SERVING.

### H2 — Runner service

- **Interfaces:** `const runnerHealthService = "compass.runner"`.
  `newServerHealth(hub *runnerhub.Hub) *serverHealth`. `Check` returns SERVING
  when `!h.draining.Load() && h.hub != nil && h.hub.RunnerEnrolled()`.
- **Red → green:** `TestServerHealthRunnerService` follows
  `TestGetAgentStatusRunnerEnrollment` (`go/server/service_agentstatus_test.go`)
  with a real hub via `runner.Dial` and `RunSessions`: NOT_SERVING after
  Enroll, SERVING on attach, NOT_SERVING on detach, with
  `/healthz?service=compass.runner` 503, 200, 503. After `markDraining` a
  second checker is NOT_SERVING while attached.

### H3 — Operator docs

- **Interfaces:** add `## Health checks` to `docs/self-host.md` after
  `## Running under systemd`: the three names, the prober table and a socket
  probe. The socket is the server's `--socket`, else `DefaultSocketPath`
  (`go/server/socket.go`) in the server's environment:
  `$XDG_RUNTIME_DIR/compass/server.sock` if that is absolute, else
  `$HOME/.compass/server.sock`. The socket is 0600, so probe as the server
  user. The sample unit (`User=compass`) has `HOME` from passwd and no
  `XDG_RUNTIME_DIR`:
  `sudo -u compass curl --unix-socket ~compass/.compass/server.sock http://localhost/healthz`.
  Use `compass.runner` for alerts, not restarts; a drain is rarely seen today.
- **Red → green:** before H1, that command returns 404. After H1, it prints
  `serving`. `rumdl check` passes.

## Tasks

- [ ] H1 — checker, both views on three doors, drain mark, spec amendment,
  dependency.
- [ ] H2 — `compass.runner` from `Hub.RunnerEnrolled`.
- [ ] H3 — `docs/self-host.md` health-check section.

## Open Questions

1. **Drain grace (load-bearing).** **A (recommended):** drop it; NOT_SERVING
   on drain ships but is rarely seen. **B:** keep it, before the serve-group
   cancel, with a compass-stack passthrough and a budget counting the fabric
   drain.
2. **HTTP prober contract (load-bearing).** **A (recommended):** add
   `/healthz`. **B:** Connect GET only; status-code probers get liveness only.
3. **Network-door auth (load-bearing).** `docs/specs/product/compass.md` §The
   network door says the server "SHALL authenticate every network-door RPC with
   a bearer token", and its token-less scenario rejects any RPC before the
   handler. An anonymous Check contradicts it.
   - **A (recommended):** anonymous on every door; amend that requirement to
     exempt `grpc.health.v1.Health/Check` and `GET /healthz`. Controls: the
     1 KiB read cap, the door's 10s header timeout and body-read deadline, no
     store read. No rate or concurrency limit: no network-door route has one
     (per-IP limits are a RIG-1298 follow-up), and a bogus bearer already
     costs a token-hash lookup. No span or log per Check; a flood shows only
     at the proxy or LB.
   - **B:** anonymous on socket and dev, bearer on the network door. The prober
     needs an account token from admin-only `IssueToken`, which also opens
     `SubscribeEvents` and every authenticated-open RPC; a scoped
     `SubjectService` token has no mint path. An AWS ALB health check cannot
     send headers; a kubelet `httpHeaders` token sits in the pod spec.
   - **Why A:** three statuses, no version or ID, and the door already serves
     unauthenticated routes (signed webhooks, the Linear session-link
     redirect). B puts a broad, long-lived credential in prober config.
4. **Does Runner NOT_SERVING change `""`?** Recommendation: no; the server
   still serves the UI and comms.
5. **Ping the database in Check?** Recommendation: no; a restart cannot fix a
   database outage. A later `compass.store` name could cover it.
