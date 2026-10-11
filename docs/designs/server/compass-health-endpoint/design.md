# Compass health endpoint

Tracker: RIG-5191

## Problem / Intent

Compass has no health endpoint. Liveness today is `GetServerInfo`
(`service.GetServerInfo`, `go/server/service.go`), which the stack calls over
the socket (`HealthProber.Probe`, `go/internal/stack/adapters/health.go`). On
the network door `classifyProcedure` (`go/internal/auth/admin_gate.go`) marks it
`authenticatedOpen`, so a prober without a token gets `Unauthenticated`. It has
no "not serving" answer, and nothing reports whether a Runner is attached. This
record adds the standard gRPC health service and a status-code `/healthz` view
of the same checker.

## Global Constraints

- Go 1.26.0. Add `connectrpc.com/grpchealth v1.5.0` (Apache-2.0, needs connect
  v1.11.0 or later; the repo has v1.20.0). Keep `go mod tidy -diff` clean.
  Refresh the shared `vendorHash` in `flake.nix` and `guest-image/default.nix`.
- Both views sit outside every interceptor chain: no bearer, no `AdminGate`, no
  otelconnect span. `classifyProcedure` does not change. Anonymous Checks are
  not traced, so a flood of them does not show in telemetry.
- One checker answers both views. `Check` reads state on each call: no polling
  goroutine, no cache.
- `Watch` is not implemented. No drain grace in this record.
- Every task is red→green: the failing test lands first.

## Approach

One `serverHealth` checker (`go/server/health.go`) implements
`grpchealth.Checker`. It has two views, mounted on the socket, dev and network
doors:

- **gRPC:** `grpc.health.v1.Health/Check`, from `grpchealth.NewHandler` with a
  1 KiB read cap.
- **HTTP:** `GET /healthz`, with an optional `?service=` that defaults to `""`.
  SERVING returns 200, NOT_SERVING returns 503 and an unknown service returns
  404. The body is the status text (`serving`, `not_serving`). The
  `GET /healthz` mux pattern also matches HEAD.

| Service | SERVING when | NOT_SERVING when |
| --- | --- | --- |
| `""` | the door answers | draining |
| `compass.v1.CompassService` | same as `""` | draining |
| `compass.runner` | at least one Runner is attached | none attached, or draining |

Any other name, including the other mounted services, returns NotFound. Probe
`""`.

`grpchealth.StaticChecker` does not fit, because the hub has an attach hook
(`SetRunnerReadyHook`) but no detach hook.

**Boot.** `Serve` opens the store and builds the doors before any door serves.
So a door that answers has an open store.

**Drain.** `drainDoors` calls `markDraining` before it closes the buses and
calls `Shutdown`. `Shutdown` closes the listeners right away, so a prober will
rarely see NOT_SERVING today. That changes when a drain grace or Watch lands
with the multi-instance load-balancer work (DL-312). Without Watch, grpc-go's
client-side health checking treats `Unimplemented` as healthy
(`health/client.go`), so those clients report Ready through a drain.

**Runner.** `compass.runner` means "at least one Runner has a live Sessions
stream". The hub holds one Runner today (`Hub.runner`, "single-Runner MVP"), so
the checker reads `Hub.RunnerEnrolled`. When the hub supports several Runners
(DL-312), the checker changes to "any attached" and the name keeps its meaning.
Two Runners that keep re-enrolling over each other leave the name SERVING while
each clears the other's bindings. `Hub.EnrolledRunners` is not used, because it
keeps the last Runner after a disconnect. **Deferred:** with a half-open TCP
link, `RunnerEnrolled` stays true until a send fails, because no door sets
`http.HTTP2Config.SendPingTimeout`. That lag delays the Runner alert, and the
fix needs its own issue.

**Probers.**

| Prober | Door | Reads NOT_SERVING |
| --- | --- | --- |
| `grpc-health-probe -tls`, or a TLS gRPC LB check | network | yes |
| kubelet `httpGet`, `scheme: HTTPS`, `/healthz` | network | yes (503) |
| status-code HTTP LB check on `/healthz` | network | yes (503) |
| `curl --unix-socket` on `/healthz` (exec probe) | socket | yes |
| stock kubelet `grpc` probe | none | does not work |

The kubelet `grpc` probe connects over plaintext. The network door accepts TLS
only, and the probe's TLS mode needs the alpha `GRPCContainerProbeTLS` gate.

**Unchanged.** `HealthProber.Probe` keeps `GetServerInfo`, because it needs the
version. Neither view returns a version or an ID.

## Alternatives considered

- **Connect GET only.** `connect.WithIdempotency(IdempotencyNoSideEffects)`
  enables GET on the gRPC view. In a throwaway smoke test on the pinned
  versions, GET returned 200 with the option and 405 without it. NOT_SERVING
  is still a 200 with a JSON body, so status-code probers read a drain as
  healthy.
- **`/healthz` with its own logic.** Rejected: it would be a second source of
  truth. The view only maps `serverHealth.Check`.
- **A drain grace inside `drainDoors`.** Rejected. `drainDoors` runs only after
  the serve group's context is cancelled. At that point the delivery consumer,
  presence, the forge lanes, the usage sweepers and the key refresh have
  stopped, so during the grace the doors would accept writes this boot cannot
  finish. A correct grace runs before that cancel. It must also fit the fabric
  drain (`closeFabric` calls the NATS `Drain`, whose ceiling defaults to 30s)
  into `serverDrainBudget` (30s). That work needs its own design.
- **StaticChecker plus a hub detach hook.** This would give Watch. It costs a
  new hub seam and sharing the single ready-hook slot, which the seed already
  uses.
- **A plaintext health-only listener.** It would make the stock kubelet `grpc`
  probe work. Deferred, because kubelet `httpGet` over HTTPS on `/healthz`
  already works.
- **Unauthenticated `GetServerInfo`.** It exposes the version, rev and Runner
  IDs, and it has no NOT_SERVING answer.
- **A per-Runner listener.** Runners dial out by design. A listener would show
  only that the Runner process is up, not that it is attached to this server.

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

  `serveHealthz` calls `Check` with `r.URL.Query().Get("service")`. It maps
  `CodeNotFound` to 404 and any other error to 500. `mount` registers both
  views. `buildDoors` creates one `serverHealth` and mounts it on `udsMux` and
  `devMux`. It also passes it to `buildNetworkServer` through a new last
  parameter, `health *serverHealth`, which mounts it on `netMux`.
  `drainDoors` calls `d.health.markDraining()` first. Update the existing
  `buildNetworkServer` and `drainSet` test call sites.
- **Red → green:**
  1. `TestServerHealthCheck` (`health_test.go`): over h2c, `""` and
     `compass.v1.CompassService` are SERVING and `nope` returns `CodeNotFound`.
     After `markDraining`, both are NOT_SERVING.
  2. `TestHealthzStatusMapping` (`health_test.go`, table, HTTP/1.1):
     `/healthz` returns 200 `serving`, `?service=nope` returns 404 and
     `POST /healthz` returns 405. After `markDraining`, `/healthz` returns 503
     `not_serving`.
  3. `TestDrainDoorsMarksNotServingBeforeShutdown` (`drain_test.go`): a
     `RegisterOnShutdown` hook on the door records `""` as NOT_SERVING.
  4. `TestServeHealthOnEveryDoor` (`health_pgtest_test.go`): with no
     credentials, a gRPC Check is SERVING and `GET /healthz` returns 200 on the
     socket, the dev door and the TLS door.

### H2 — Runner service

- **Interfaces:** `const runnerHealthService = "compass.runner"`. The
  constructor becomes `newServerHealth(hub *runnerhub.Hub) *serverHealth`.
  `Check` returns SERVING when
  `!h.draining.Load() && h.hub != nil && h.hub.RunnerEnrolled()`.
- **Red → green:** `TestServerHealthRunnerService` follows
  `TestGetAgentStatusRunnerEnrollment`
  (`go/server/service_agentstatus_test.go`). It drives a real hub through
  `runner.Dial` and `RunSessions`. The status is NOT_SERVING after Enroll,
  SERVING once the stream attaches, and NOT_SERVING after detach. Over the
  same steps, `/healthz?service=compass.runner` returns 503, then 200, then
  503. A second checker on the hub, after `markDraining`, is NOT_SERVING while
  the stream is attached.

### H3 — Operator docs

- **Interfaces:** add a `## Health checks` section to `docs/self-host.md`,
  after `## Running under systemd`. It lists the three names, the prober table
  from Approach, and the socket command
  `curl --unix-socket "$XDG_RUNTIME_DIR/compass/server.sock" http://localhost/healthz`
  (the default `--socket` path). It says to use
  `compass.runner` for alerts, not restarts, and that today drain is rarely
  seen.
- **Red → green:** before H1, that command returns 404. After H1, it prints
  `serving`. `rumdl check` passes.

## Tasks

- [ ] H1 — checker, gRPC and `/healthz` views on three doors, drain mark,
  dependency.
- [ ] H2 — `compass.runner` from `Hub.RunnerEnrolled`.
- [ ] H3 — `docs/self-host.md` health-check section.

## Open Questions

1. **Drain grace (load-bearing).** **A (recommended):** drop it from this
   record. NOT_SERVING on drain ships, but it is rarely seen until a grace or
   Watch arrives. **B:** keep it, moved before the serve-group cancel. B also
   needs a compass-stack passthrough and a budget that counts the fabric drain.
2. **HTTP prober contract (load-bearing).** **A (recommended):** add the
   `/healthz` view. **B:** Connect GET only. B means documenting body
   matching, and status-code probers then get liveness only.
3. **Unauthenticated on the network door?** Recommendation: yes. The answer is
   one of three statuses. A Check is one atomic load plus one read under the hub
   mutex. The signed webhooks already sit outside the gate.
4. **Does Runner NOT_SERVING change `""`?** Recommendation: no. Without a
   Runner, the server still serves the UI and comms, and the Runner reconnects
   to it.
5. **Ping the database in Check?** Recommendation: no. Anonymous callers could
   create pool load, and a restart cannot fix a database outage. A cached
   last-success timestamp would need a background pinger. If one is wanted,
   give it its own `compass.store` name.
