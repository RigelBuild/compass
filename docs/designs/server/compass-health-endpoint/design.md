# Compass health endpoint

Tracker: RIG-5191

## Problem / Intent

Compass has no health endpoint. The stack probes liveness with `GetServerInfo`
over the socket (`HealthProber.Probe`, `go/internal/stack/adapters/health.go`);
on the network door it needs a bearer (`auth.BearerInterceptor` in
`buildNetworkServer`, `go/server/network_door.go`). Nothing reports "not serving" or an attached
Runner. This record adds the gRPC health service and a `/healthz` view of one
checker.

## Global Constraints

- Go 1.26.0. Add `connectrpc.com/grpchealth v1.5.0` (Apache-2.0; connect
  v1.11.0 or later, the repo has v1.20.0). Keep `go mod tidy -diff` clean and
  refresh the shared `vendorHash` in `flake.nix` and `guest-image/default.nix`.
- Both views are anonymous on every door (Rulings) and sit outside every
  interceptor chain. `classifyProcedure` does not change.
- One checker answers both views. `Check` reads state on each call: no polling
  goroutine, no cache, no store read. Only the door-limits record's over-budget
  path caches (Rulings).
- `Watch` is not implemented. No drain grace in this record.
- Every task is red→green: the failing test lands first.

## Approach

One `serverHealth` checker (`go/server/health.go`) implements
`grpchealth.Checker`. It has two views:

- **gRPC:** `grpc.health.v1.Health/Check` and `Health/Watch`, from
  `grpchealth.NewHandler` with a 1 KiB read cap. Watch answers `Unimplemented`.
- **HTTP:** `GET /healthz`, with an optional `?service=` that defaults to `""`.
  SERVING returns 200, NOT_SERVING 503 and an unknown service 404. The body is
  `serving` or `not_serving`. The `GET` pattern also matches HEAD.

Both mount on the socket, dev and network doors.

| Service | SERVING when | NOT_SERVING when |
| --- | --- | --- |
| `""` | the door answers | draining |
| `compass.v1.CompassService` | same as `""` | draining |
| `compass.runner` | at least one Runner is attached | none attached, or draining |

Any other name returns NotFound.

**Boot.** `Serve` opens the store and builds the doors before any door serves.

**Drain.** `drainDoors` marks draining, closes the buses, then calls
`Shutdown`, which closes listeners at once; H1 pins only the mark before
`Shutdown`. Probers rarely see NOT_SERVING until a drain grace (Rulings)
or Watch lands (DL-312). Without Watch, grpc-go client-side health treats
`Unimplemented` as Ready (`health/client.go`) through a drain.

**Runner.** `compass.runner` means "at least one Runner has a live Sessions
stream". The hub holds one Runner today, so the checker reads
`Hub.RunnerEnrolled`; with several Runners (DL-312) it becomes "any attached".
Not `Hub.EnrolledRunners`: it keeps the last Runner after a disconnect.

**Half-open links (deferred).** A dead link keeps `RunnerEnrolled` true until a
send fails or TCP gives up: Go's default keepalive detects it in about 150s on
Linux; queued data falls to retransmission timeouts. `SendPingTimeout` covers
queued data and L4 passthrough; an HTTP/2-terminating proxy answers PING
itself, so that path needs a Sessions heartbeat.

**Probers.** A gRPC Check returns NOT_SERVING inside an OK response, so only
probers that read it see a drain.

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
- **Bearer on the network door.** Any account bearer opens every
  authenticated-open RPC, and ALB health checks cannot send headers.

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
  `drainSet` call sites. Amend `docs/specs/product/compass.md` §The network
  door to exempt Health `Check`, `Watch` and `GET`/`HEAD /healthz` from the bearer,
  with a token-less health scenario.
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
     socket, dev and TLS doors, and a token-less Watch is `Unimplemented`.

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
  listener policy. Point status-code LB checks at `/healthz`. The door-limits
  record (RIG-5243) limits both health routes, serving a cached status over
  budget (pending RIG-5302); rate-limit them upstream until it lands. Alert on
  `compass.runner`.
- **Red → green:** before H1, that command returns 404. After H1, it prints
  `serving`. `rumdl check` passes.

## Tasks

- [ ] H1 — checker, both views on three doors, drain mark, dependency, §The
  network door amendment and token-less health scenario.
- [ ] H2 — `compass.runner` from `Hub.RunnerEnrolled`.
- [ ] H3 — `docs/self-host.md` health-check section.

## Rulings

Matt ruled on RIG-5230, 2026-10-12.

1. **Drain grace: A, out of scope.** A grace would mark NOT_SERVING, wait,
   then cancel the serve group; it needs its own design.
2. **HTTP prober contract: A.** Add `/healthz`.
3. **Network-door auth: A.** Both views are anonymous on every door, and H1
   amends §The network door, which says the server "SHALL authenticate every
   network-door RPC with a bearer token". Per-request bounds: the 1 KiB read
   cap, header timeout, body-read deadline, no store read. No per-Check span or
   log. The door-limits record
   (`docs/designs/server/compass-network-door-limits/design.md`, RIG-5243)
   covers both health routes as one more limited route group. Over budget,
   health serves a cached status (pending RIG-5302), bounding the `Hub.mu` and
   router-mutex contention of a `compass.runner` flood.
4. **Settled: Runner NOT_SERVING leaves `""` SERVING.** The server still
   serves the UI and comms.
5. **Settled: Check never pings the database.** A restart cannot fix a
   database outage.
