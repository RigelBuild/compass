# microVM Runner V4 — AgentGateway over host-side vsock

Status: PROPOSED — details the V4 milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V4,
microvm-runner.md:508-532; Approach (b), microvm-runner.md:105-137) and its
frozen VMM/transport decision D1 (cloud-hypervisor hybrid vsock,
microvm-runner.md:665-670).

Ledger impact: none. V4 details the transport D1 already froze ("D1 also fixes
the **host-side vsock transport shape**: cloud-hypervisor uses hybrid vsock",
microvm-runner.md:665-670); nothing here is a new cross-cutting decision, and
`docs/designs/DECISIONS.md` is untouched.

## Problem / Intent

A session running inside a microVM cannot reach its Runner today. The
agent→Runner gateway (Comms / Lifecycle / Forge / Publish /
PostConversationFrame / Control) is a per-container AF_UNIX socket the host
serves at Provision and **bind-mounts into the container**
("`spec.Mounts = append(spec.Mounts, listener.Mount(agentSocketMountPath))`",
`go/internal/runner/host.go:177`), and a bind mount does not cross a VM
boundary. Worse, the microVM backend actively refuses the mount: `Create`
validates the mount set "down to the single read-write workspace share"
(`go/internal/runtime/microvm_lifecycle.go:221-244`), so the socket mount (and
the config mount beside it, `host.go:177,193`) fails every microVM Provision
with an `UnsupportedMountError` before a VM ever boots.

V4 puts the gateway on the per-session virtio-vsock channel the parent's
Approach (b) reserved for it: "The host Runner serves the *same generated
handler* over the host-side vsock transport instead of the AF_UNIX path; the
in-guest agent dials vsock instead of the unix socket"
(microvm-runner.md:125-127). No proto, handler, or relay change — the
transport swaps under `compassv1internalconnect.NewAgentGatewayHandler`
(bound in `gateway/gateway.go:313-316` with `WithReadMaxBytes`), and the
podman path stays byte-identical.

## Approach

Each subsection resolves one fork the parent's V4 plan leaves to detailing.
The load-bearing serving-mechanism fork is (a)-(b); every resolution is also
listed in `## Open Questions` for the pre-freeze batch, and the body designs
against the recommended option.

### (a) The CH guest→host mechanism — verified, and it is NOT the CONNECT-preamble path

The parent says the existing serving path "largely transfers" because "the
host end is an AF_UNIX socket and a `CONNECT <port>` line selects the
guest-side port" (microvm-runner.md:510-515). That sentence describes the
**host→guest** direction — the one V2b already built, where the host is the
client: `DialGuest` "opens the host end of a cloud-hypervisor hybrid vsock and
steers a connection to the guest's `port`" by "writing a `CONNECT <port>\n`
preamble line before any application data"
(`go/internal/runtime/microvm/dial.go:14-23`).

V4 is the **inverse** direction — the in-guest agent is the client, the host
Runner is the server — and CH's hybrid vsock uses a different mechanism there,
verified against CH `docs/vsock.md` (read this session, § "Connecting from
Guest to Host"):

> "This first requires a listening UNIX socket on the host side. The UNIX
> socket path has to be constructed by using the socket path used at the VM
> launch time with appended `_` and the port number to be used on the guest
> side. As in the example above, if we'd intended to connect from the guest to
> the port `1234`, the Unix socket path on the host side would be
> `/tmp/ch.vsock_1234`. Also note that the CID used on the guest side is the
> well known CID value `2`."

So for guest-initiated connections there is **no preamble in either
direction**: the host binds a plain AF_UNIX listener at
`<--vsock socket path>_<port>`, the guest dials `AF_VSOCK CID 2, <port>`, and
the VMM's muxer connects to the suffixed host socket and splices bytes
transparently (CH's implementation "is based on the Firecracker
implementation", CH docs/vsock.md, which specifies the same suffixed-listener
scheme). The docs' own example starts the host listener *after* VMM boot
(`socat - UNIX-LISTEN:/tmp/ch.vsock_1234`), so the listener need only exist by
the time the guest first dials, not at `--vsock` device creation.

Consequences the design leans on:

- The parent's "largely transfers" conclusion is **confirmed, for a stronger
  reason than it gives**: the host-side server is an ordinary AF_UNIX
  `net.Listener` serving h2c — exactly what `listenAgentSocket` already builds
  (`gateway/socket.go:105-187`) — with zero vsock-specific framing. The
  CONNECT-preamble machinery stays where V2b put it, on the host→guest client
  path only.
- The guest-side dial target is `(CID 2, port)`, an AF_VSOCK address — which
  the Bun/Node agent cannot dial natively; see (d).
- The suffixed path is **derived from the session's `--vsock socket=` path**
  (`VsockSocket: filepath.Join(runtimeDir, "vsock.sock")`,
  `microvm_lifecycle.go:208`), so it necessarily lives in the per-session
  runtime dir. That anchors the identity story in (e).

### (b) Host serving: `gateway.Serve` reused verbatim over the suffixed path — no sibling constructor

The record's central call. Three candidates:

- **Option A (recommended): reuse `gateway.Serve` as-is.** `Serve(ctx, path,
  containerName, deps)` already takes an arbitrary socket path, builds the
  Gateway with the container identity bound at construction, mounts the
  generated handler with the `maxAgentMessageBytes` read bound, and hands the
  path to `listenAgentSocket` (`gateway/gateway.go:298-326`). Every mechanism
  in `listenAgentSocket` transfers unchanged to the suffixed path:
  - the sun_path cap check (`socket.go:147-149`) — the suffixed path is a
    fresh AF_UNIX bind like any other;
  - `MkdirAll` + `Chmod` 0700 of the parent dir (`socket.go:151-159`) — the
    parent dir is the session runtime dir, already created 0700 by `Create`
    (`microvm_lifecycle.go:159-161`), so the chmod is idempotent;
  - stale-socket reclaim by uid (`socket.go:189-218`) — a crashed-Runner
    leftover at the suffixed path is the same hazard class;
  - the 0600 socket file mode (`socket.go:34-37`) — on the host side only the
    VMM (running as the same uid) ever connects, so owner-only is correct and
    incidentally stricter than needed.
  The **only** V4-specific code on the host serving path is the path
  derivation, one pure function in the `microvm` package:
  `GatewaySocketPath(vsockSocket string, port uint32) string` returning
  `vsockSocket + "_" + strconv.FormatUint(uint64(port), 10)` — the CH contract
  from (a), stated once.
- **Option B (rejected): a sibling vsock-transport constructor in `gateway`**
  (the parent's Interfaces sketch: "a vsock-transport constructor (sibling of
  `Serve`, `socket.go`)", microvm-runner.md:524-527). Verified against the
  code, there is nothing for a sibling to *do differently*: the transport IS
  an AF_UNIX listener (a), and `Serve` is already path-parameterized. A
  sibling that duplicates `Serve` minus the `Mount` helper would be a second
  convention beside an existing one. The parent sketched the interface before
  the direction inversion was verified; the detailing outcome is that the
  sketch collapses into reuse. Flagged against the frozen parent's phrasing in
  OQ-2.
- **Option C (rejected): serve from the `runtime` layer** (the backend owns
  the VM, so let `MicroVMRuntime` serve the gateway beside it). Violates
  layering: the Gateway's `Deps` are Runner-layer objects — the `ServerLink`
  client satisfying `CommsRelay`/`LifecycleRelay`/… (`host.go:913`) — and
  `go/internal/runtime` cannot import `go/internal/runner`. The runtime layer
  exposes the *endpoint*; the runner serves on it, exactly as it does for
  podman paths today.

### (c) Where the host wires it: `agentHost.Provision`, gated by a backend probe

Today `Provision` serves the socket **before** `Launch` ("it is served from
Provision so a call arriving before Start binds a session fails closed rather
than finding no listener", `host.go:152-162`) and then appends two mounts the
microVM backend refuses (Problem above). On the microVM backend the suffixed
path is not knowable before `Launch` — it derives from the session runtime dir
that `Create` mints (`microvm_lifecycle.go:159`) — so the order inverts:
Launch first, then serve. That is safe: the in-guest agent process does not
exist until the session-Start exec (`agentCommand`, the argv "the Runner execs
to start the first-party agent", `runner/agent_exec.go:29-32`), and a hostile
in-guest early dialer just gets a failed vsock connect while the suffixed
listener is absent — fail-closed, the same posture as today's
pre-Start `CodePermissionDenied` window.

The true window ordering is one step finer than "Launch first, then serve":
guestd's `/run/compass/agent.sock` forwarder comes up **inside Launch
itself** — the guest `Provision` that starts it is called from
`MicroVMRuntime.Start` ("client :=
`microvm.GuestClient(session.cfg.VsockSocket, session.cfg.VsockPort)`; …
`client.Provision(ctx, …)`", `microvm_lifecycle.go:304-308`) — so the
in-guest forwarder exists *before* the host suffixed listener does. That
window is fail-closed too: no agent process exists until the session-Start
exec, and an early direct vsock dial reaches the VMM's muxer, which fails to
`connect()` the absent suffixed socket and refuses the connection — no
phantom success, no wedge. Serving *before* Launch was considered and not
taken: the suffixed path derives from the runtime dir `Create` mints inside
Launch ("`runtimeDir := filepath.Join(m.config.RunRoot, "microvm",
string(id))`", `microvm_lifecycle.go:159-161`), so it is not knowable
pre-Launch, and restructuring to expose it early would buy no security — the
window is already fail-closed. Nor does the vsock arm need `serveSocket`'s
idempotent-reuse check ("`if listener, served := h.sockets[containerName];
served { … return listener, nil }`", `host.go:905-910`): a provision retry of
a live name never reaches `Serve`, because `Create` refuses the duplicate
first ("Refuse a duplicate name under the same lock that inserts …
`return "", &DuplicateNameError{Name: spec.Name}`",
`microvm_lifecycle.go:176-188`).

Mechanism, mirroring V3's marker-probe precedent (the smallest-blast-radius
shape ratified there: "one marker method + one guarded call site",
microvm-v3-egress-in-guest.md:138-163):

- `MicroVMRuntime` gains one exported method,
  `AgentGatewayEndpoint(name string) (socketPath string, ok bool)`, returning
  `microvm.GatewaySocketPath(session.cfg.VsockSocket, agentGatewayVsockPort)`
  for the named session — NOT on the frozen `ContainerRuntime` interface.
- `agentHost` probes its engine via an unexported single-method interface,
  `type vsockGatewayEngine interface { AgentGatewayEndpoint(string) (string, bool) }`.
  - Probe **absent** (podman, every fake): today's path byte-identical —
    `serveSocket` before Launch, socket + config mounts appended
    (`host.go:173-193`).
  - Probe **present**: Provision skips `serveSocket`, skips **both** mounts
    (the socket mount is replaced by vsock; the config mount is deferred, (f)),
    calls `h.runtime.Launch`, resolves the endpoint by the returned handle
    name, and then serves `gateway.Serve(ctx, suffixedPath, name, deps)` —
    recording the listener in the **same** `h.sockets` map keyed by container
    name (`host.go:143-144,917-919`).
- Failure symmetry: a `Serve` failure after a successful Launch tears the
  launched session down with the exact call Remove's teardown leg uses —
  resolve the handle and `h.runtime.Teardown(ctx, handle)` ("if handle, ok :=
  `h.registry.Resolve(containerName)`; ok { … `h.runtime.Teardown(ctx,
  handle)`", `host.go:572-575`; Teardown = stop + remove + deregister,
  `runtime/agent.go:216-219`) — so no VM outlives a session whose gateway
  never came up. In that state `h.sockets` and `h.configVersions` hold
  nothing yet for the name (the vsock arm records the listener only after a
  successful `Serve` and skips the version seed, `host.go:201-207`), so no
  `closeSocket` or version cleanup is owed.
- Everything downstream is untouched: `BindSession` / `RetireSession` /
  `SendControl` operate on the recorded listener (`socket.go:260-295`),
  `Session(containerName)` resolves identity exactly as today
  (`host.go:217-226`), and `closeSocket` at Remove/Close drains and removes
  the suffixed socket file (`SocketListener.Close` "finally removes the socket
  file", `socket.go:220-255`).

### (d) The in-guest dial path: guestd bridges `/run/compass/agent.sock` → vsock; the agent is byte-identical

The parent sketches "the in-guest agent's dial target … is a **new** value
injected via the exec environment" (microvm-runner.md:519-522). Detailing
finds a hard constraint the sketch predates: the agent's transport is
`@connectrpc/connect-node`'s `createGrpcTransport`/`Http2SessionManager`
(`packages/compass-agent/src/transport/index.ts:14-18`), which rides the
Node/Bun `net` stack — and neither Node nor Bun can open an `AF_VSOCK` socket
(no vsock address family in either runtime's socket API; supporting one would
mean a native addon in the agent). [INFERENCE from the runtimes' documented
socket APIs; the import cited is the session-read ground for *what* the agent
dials with.] An env-injected "vsock port" would be a value the agent cannot
use.

**Resolution: guestd owns a tiny unix→vsock forwarder, and the agent keeps
its fixed path.** The agent's rendezvous is a deliberate constant — "the agent
takes no per-session socket configuration, so this constant IS the rendezvous"
(`packages/compass-agent/src/cli.ts:86-89`,
`AGENT_SOCKET_PATH = "/run/compass/agent.sock"`) — and V4 preserves it:

- During `Provision` (after the V3 arm, before the `stateProvisioned`
  transition), guestd creates `/run/compass` and listens on
  `/run/compass/agent.sock`, mode 0600, chowned to the request's
  `default_exec_uid` — the same owner posture the container socket has today
  (0600 agent-owned, `gateway/socket_podman_test.go:64-65`). A listen/chown
  failure fails `Provision`, and the exec gate stays closed (the V3
  fail-closed posture, microvm-v3-egress-in-guest.md:281-285).
- Each accepted connection is forwarded by a **lazy per-connection dial** to
  `AF_VSOCK (CID 2, gateway port)` via the `mdlayher/vsock` package guestd
  already depends on (`go/internal/guestd/vsock.go:15`), splicing bytes both
  ways. Lazy dialing removes any boot-ordering hazard: the host serves the
  suffixed listener in Provision (c), the agent first connects at
  session-Start exec time, and a dial before the host listener exists fails
  that one connection visibly rather than wedging the proxy.
- The gateway port reaches guestd the way the control port already does: a
  kernel-cmdline parameter. `Launch` appends
  `compass.vsock_port=<n>` today (`microvm/launch.go:255-256`); V4 appends
  `compass.gateway_port=<n>` beside it from a new `BootConfig.GatewayPort`
  field, and guestd parses it with a sibling of `parseVsockPort`
  (`guestd/cmdline.go:12-19,21-60` — same uint32/zero/`VMADDR_PORT_ANY`
  validation). The parameter is **optional**: absent (a V2a-era cmdline, a
  hermetic harness) means no proxy is started — the V2b/V3 suites keep
  booting unchanged — and `Launch` always appends it in production.
- The forwarder is plumbing, not surface: it is never entered in the exec
  table, is unreachable from the wire, and the GuestControl peer-CID gate
  (`guestd/vsock.go:40-43`) is unaffected — the proxy *dials out* to the host
  CID; it accepts nothing over vsock. The forwarder's 0600 mode is not the
  whole access story, though: the host gateway port is a **guest-global
  capability** — an AF_VSOCK `connect(CID 2, 1025)` needs no privilege and no
  filesystem path, so any in-guest uid can reach the host Gateway directly,
  bypassing the forwarder entirely; the coarsened host-side identity binding
  this implies is ruled on in OQ-7.

This is a deliberate, flagged divergence from the frozen parent's
env-injection sentence (OQ-1): the *contract* the parent wants — the agent
reaches its Runner over the per-session vsock channel — is met, with the agent
binary and its fixed-path contract untouched on both backends, and no
`COMPASS_*` env addition. The rejected alternative (teach the agent vsock via
a native addon or a bundled forwarder-in-the-exec-env) adds a runtime
dependency to every agent build to solve a problem only the guest has, and
still needs an in-guest component to own the socket file; guestd is already
the in-guest root component with a boot-ordered lifecycle.

### (e) Identity binding: the suffixed path IS the session; the port is fixed

Today "the socket IS the container's identity: one Gateway serves one
container's socket, so the container name is fixed at construction, never read
off the request" (`gateway/gateway.go:13-18`). The parent expected vsock to
change this to "per-session-port→VM identity, assigned and recorded by the
backend at boot" (microvm-runner.md:135-137) — a port-allocation table.
Verified against the as-built V2b transport, no such table exists and none is
needed: with hybrid vsock **the host-side endpoint is still a per-session
AF_UNIX path**, because the suffix base is the session's own
`<runtimeDir>/vsock.sock` (`microvm_lifecycle.go:208`). V2b already ratified
exactly this identity model for the control port: "guestVsockPort is the fixed
port guestd serves the control plane on inside every session VM. Per-session
uniqueness is carried entirely by the AF_UNIX socket paths under the session
runtime dir, never the port (OQ-F)" (`microvm_lifecycle.go:48-52`).

V4 therefore mirrors it: one fixed guest-side port,
`agentGatewayVsockPort uint32 = 1025` (a `runtime` const beside
`guestVsockPort = 1024`), identical in every VM; the per-session suffixed path
`<RunRoot>/microvm/<id>/vsock.sock_1025` carries identity; and the Gateway is
constructed with the session's container name exactly as on podman, so
`SessionForContainer` (`gateway/gateway.go:83-90`) and the fail-closed
no-session window are untouched. The parent's "assigned and recorded by the
backend at boot" is satisfied degenerately — the backend records the endpoint
by recording the runtime dir — and the divergence from its
per-session-*port* phrasing is flagged in OQ-1.

A hypothetical second guest-initiated channel simply takes port 1026 with its
own suffixed path; cross-session port collision is structurally impossible —
one VMM per session, one `--vsock socket=` per VMM (`VsockSocket:
filepath.Join(runtimeDir, "vsock.sock")`, `microvm_lifecycle.go:208`), so
identical ports never share a suffix base.

Two path-budget facts, handled in W2:

- `listenAgentSocket`'s own sun_path check still guards the bind ("`if
  len(path) > sunPathMax`" ⇒ the error naming the limit and both knobs,
  `socket.go:147-149`) — but on this path it fires only at post-Launch
  `Serve`, i.e. after a full VM boot. W2 therefore adds a pre-boot guard:
  `Create` checks the suffixed path's length against the same budget where
  `bootConfig` mints the base ("`VsockSocket: filepath.Join(runtimeDir,
  "vsock.sock")`", `microvm_lifecycle.go:208`) — one length comparison — so
  an over-long RunRoot fails with the operator-actionable error **before**
  any VM boots, not after a ~60s boot it then tears down.
- The suffixed name `vsock.sock_1025` (15 bytes) overtakes `virtiofsd.sock`
  (14) as the widest per-session socket leaf, so the KVM harness's budget
  comment and short-RunRoot helper — "the widest is
  `<RunRoot>/microvm/<32-hex session id>/virtiofsd.sock`, a 56-byte tail"
  (`microvm_lifecycle_microvm_test.go:25-29`) — must be updated to the 57-byte
  tail.

### (f) The config mount: deferred, safe-degraded — not V4 scope

Provision's second refused mount is the read-only fleet-config tree
(`host.go:178-193`). Config delivery into the guest is its own slice (it wants
a virtio-fs share or a control-plane push, V6/V2b territory — no frozen plan
names it yet), and V4 must not smuggle it in. Skipping it is **safe by the
agent's own contract**: "UNCONFIGURED — no `current` symlink, or the whole
mount absent — is a VALID empty state, not an error: every reader is tolerant
(absent/malformed → empty, NEVER throws)"
(`packages/compass-agent/src/config-reader.ts:22-24`). So on the vsock-gateway
path Provision also skips `configMaterializerFor(...).Materialize` and its
mount, and a microVM agent boots with empty fleet config until the follow-up
slice lands. Flagged as a load-bearing deferral in OQ-3 with the follow-up
filing named in the freeze→file→dispatch gate.

The deferral is honest **only with a matching gate on the refresh path** —
skipping the mount at Provision is not boot-only. `RefreshConfig` fans out
over every live session ("`targets := make([]target, 0, len(h.sessions)); for
_, s := range h.sessions {`", `host.go:741-751`) and runs each leg through
`refreshOneContainer` (`host.go:777-818`): `MountLabel` — which the microVM
backend answers with the empty label ("`return "", nil`",
`microvm_lifecycle.go:602-604`), harmless — then `Materialize`, which
succeeds into a host tree nothing mounts into the guest, then the version
compare. Because the vsock-gateway Provision skips the materialize block that
seeds the version map ("`h.configVersions[spec.Name] = mount.Version`",
`host.go:205-207`), `lastVersion` is `""` for every microVM session, the
compare always sees a change, and the leg runs `reloadLocked` — "the slow
Stop + StartAgent" (`host.go:838-845`) — restarting **every live microVM
agent mid-session on the first fleet ConfigVersion publish**, delivering
nothing, and again on every subsequent bump. So the refresh path is
probe-gated in W2 with the same (c) probe: a session whose engine passes the
`vsockGatewayEngine` assertion is skipped by the fleet-config fan-out — no
Materialize, no Stop+StartAgent churn — until the config-delivery slice gives
the refresh something real to deliver (OQ-3).

### (g) Non-goals, restated as testable checks

- **vsock is not IP.** The guest's nft ruleset governs the netns; the vsock
  device is not addressable from it. The parent's non-goal check
  (microvm-runner.md:530-532) becomes W3's assertion that a firewalled guest
  cannot reach the gateway via any IP route while the vsock path works.
- **No proto/handler/relay change.** `Gateway`, `ControlSender`, and the
  Server relays are transport-agnostic (microvm-runner.md:133-135); V4 touches
  no `.proto` and no generated binding — the handler mount stays
  `NewAgentGatewayHandler(g, connect.WithReadMaxBytes(maxAgentMessageBytes))`
  (`gateway/gateway.go:313-316`).
- **No second gateway convention.** Podman keeps the bind-mounted socket;
  microVM gets the suffixed listener; both are `gateway.Serve` on different
  paths.

## Global Constraints

Every task below inherits these.

- **The generated handler is bound unchanged.** The one production handler
  mount is `compassv1internalconnect.NewAgentGatewayHandler(g,
  connect.WithReadMaxBytes(maxAgentMessageBytes))`
  (`gateway/gateway.go:313-316`); V4 adds no second binding and no proto
  change.
- **The podman path is byte-identical.** No change to `serveSocket`, the
  mount list Provision builds on podman, `AGENT_SOCKET_PATH`, or any podman
  argv; fakes don't implement the (c) probe, so every existing hermetic runner
  suite runs unchanged.
- **Frozen `ContainerRuntime` interface untouched.** The (c) probe is a
  marker/endpoint method on `MicroVMRuntime` + an unexported assertion in
  `agentHost`, never an interface verb — the V3-ratified discipline
  (microvm-v3-egress-in-guest.md:300-302).
- **Fail-closed at every window.** No listener ⇒ the guest dial fails; no
  bound session ⇒ `CodePermissionDenied` (`gateway/gateway.go:33-37`); a
  gateway-proxy listen failure in guestd fails Provision and the exec gate
  stays closed; a host `Serve` failure after Launch tears the session down.
- **KVM-gated vs hermetic split** (microvm-ci-dev-enablement.md:295-301):
  everything booting a VM carries `//go:build microvm && unix` and calls
  `microvmtest.Require(t)` first; the path derivation, the probe/skip logic,
  the guestd proxy, and the suffixed-path serving are tested hermetically.
- **AF_UNIX budget guarded.** The suffixed path rides `listenAgentSocket`'s
  existing sun_path check (`socket.go:147-149`); the KVM harness's
  widest-tail accounting is updated for `vsock.sock_1025`.
- **External-reference gate.** Compass-tracked files only; no private names
  beyond RIG-NNN.

## Plan

W1 (guestd: gateway proxy) and W2 (host: endpoint + serving + probe) are
independent until W3 integrates them; both are hermetically testable. W3
(KVM-gated e2e + non-goal checks) is the milestone's acceptance gate and
consumes W1+W2.

### W1 — guestd: `compass.gateway_port` + the unix→vsock forwarder

The (d) guest half: parse the new cmdline parameter, and start the agent
socket forwarder during a successful `Provision`.

- **Interfaces:** produces
  - `gatewayPortKey = "compass.gateway_port"` and
    `parseGatewayPort(cmdline string) (uint32, bool, error)` in
    `go/internal/guestd/cmdline.go` — same tokenizing/validation as
    `parseVsockPort` (`cmdline.go:21-60`: last occurrence wins, reject 0 /
    `VMADDR_PORT_ANY` / overflow), but **optional**: an absent key returns
    `(0, false, nil)` (no proxy; hermetic/V2a-era cmdlines stay valid), while
    a present-but-malformed value is an error (fail-closed boot, the
    `parseVsockPort` posture);
  - the parsed port threaded from `run`'s cmdline step into the supervisor as
    a new field (beside `defaultExecUID`, `supervisor.go:107-108` vicinity);
  - the forwarder on the supervisor,
    `startGatewayProxy(ctx context.Context, socketPath string, uid uint32, port uint32) (io.Closer, error)`,
    invoked inside `Provision` after the V3 arm and before the
    `stateProvisioned` transition: `MkdirAll("/run/compass", 0o755)`, listen
    AF_UNIX at `/run/compass/agent.sock`, `Chmod` 0600 + `Chown` to
    `default_exec_uid`, then an accept loop that per-connection dials
    `vsock.Dial(2, port, nil)` (`github.com/mdlayher/vsock`, already a guestd
    dependency, `guestd/vsock.go:15`) and splices both directions
    (`io.Copy` ×2, half-close on either EOF, both conns closed on ctx
    cancel). A listen/chown error ⇒ `Provision` returns `CodeInternal` and
    state stays `stateReady` (exec refused) — the V3 fail-closed shape. A
    zero `gatewayPort` (key absent) ⇒ no proxy, Provision otherwise
    unchanged;
  - a seam field `dialGateway func(port uint32) (net.Conn, error)` beside the
    V3 `armFunc` precedent (microvm-v3-egress-in-guest.md:324-336) so the
    forward loop is hermetically testable with no AF_VSOCK.
- **Test cycle (hermetic, no KVM):** (1) cmdline rows — absent key ⇒
  `(0, false, nil)`; valid ⇒ port; `0`/`4294967295`/overflow/non-numeric ⇒
  error (mirror `cmdline_test.go:23-93`); (2) with an injected `dialGateway`
  returning one end of a `net.Pipe`, a client connecting to the proxy socket
  has its bytes spliced both ways and a dial error closes only that
  connection; (3) `Provision` with a configured gateway port starts the
  proxy (socket exists, 0600, owner = `default_exec_uid`) and a listen
  failure (pre-occupied path) fails Provision with the gate still closed;
  (4) `Provision` with no gateway port starts no proxy and still opens the
  gate (the V2b/V3 hermetic suites stay green unchanged).

### W2 — host: `GatewaySocketPath` + `BootConfig.GatewayPort` + probe-gated serving in Provision

The (b)+(c)+(e) host half: the path derivation, the cmdline append, the
endpoint probe, and the Provision rewiring — with the podman path
byte-identical.

- **Interfaces:** produces
  - `func GatewaySocketPath(vsockSocket string, port uint32) string` in
    `go/internal/runtime/microvm` (the (a) CH contract:
    `vsockSocket + "_" + port`);
  - `BootConfig.GatewayPort uint32` (`microvm/config.go:21-35`), appended by
    `Launch` to the cmdline as `compass.gateway_port=<n>` beside the existing
    `compass.vsock_port` append (`microvm/launch.go:255-256`); zero ⇒ not
    appended (harness compatibility);
  - `agentGatewayVsockPort uint32 = 1025` (a `runtime` const beside
    `guestVsockPort`, `microvm_lifecycle.go:48-52`), set into
    `BootConfig.GatewayPort` by `bootConfig`
    (`microvm_lifecycle.go:200-219`);
  - `func (m *MicroVMRuntime) AgentGatewayEndpoint(name string) (string, bool)`
    — resolves the session by `name` (the `Exists` lookup shape) and returns
    `GatewaySocketPath(session.cfg.VsockSocket, agentGatewayVsockPort)`;
    `(“”, false)` for an unknown name. NOT on `ContainerRuntime`;
  - in `go/internal/runner/host.go`:
    `type vsockGatewayEngine interface { AgentGatewayEndpoint(string) (string, bool) }`,
    asserted on `h.engine` at the top of `Provision`
    (`host.go:163-208`). Probe absent ⇒ today's body byte-identical. Probe
    present ⇒ skip `serveSocket`, skip the socket mount **and** the config
    materialize+mount ((f)), `Launch`, resolve
    `AgentGatewayEndpoint(handle.Name())`, then
    `gateway.Serve(ctx, suffixedPath, handle.Name(), deps)` with the same
    `Deps` literal as `serveSocket` (`host.go:913`), recording the listener
    in `h.sockets[handle.Name()]` under `h.mu`; a Serve failure tears the
    launched session down via the Remove teardown path before returning;
  - the refresh gate ((f)): `RefreshConfig`'s fan-out (or
    `refreshOneContainer`) skips a session whose engine passes the same
    `vsockGatewayEngine` probe, so a fleet ConfigVersion signal never
    Stop+StartAgent-churns a microVM session it cannot deliver config to
    (`host.go:741-751,777-818`);
  - the pre-boot sun_path guard ((e)): `Create` rejects a
    `GatewaySocketPath(VsockSocket, agentGatewayVsockPort)` longer than the
    AF_UNIX budget where `bootConfig` mints the base
    (`microvm_lifecycle.go:208`) — one length comparison, failing before any
    boot rather than at post-Launch Serve;
  - one `slog` line on the probe-present serve path (container name +
    suffixed path) — `serveSocket` logs nothing today (`host.go:905-921`), so
    this is the first log anchor a KVM triage of the vsock gateway gets;
  - the widest-tail budget update in the KVM harness comment + helper
    (`microvm_lifecycle_microvm_test.go:25-29`): `vsock.sock_1025`, 57-byte
    tail.
  Consumes `gateway.Serve` (`gateway/gateway.go:298-326`) and
  `SocketListener.Close` (`socket.go:220-255`) unchanged.
- **Test cycle (hermetic):** (1) `GatewaySocketPath` string rows (the CH
  suffix contract, including a port with all digits significant); (2)
  `bootConfig` sets `GatewayPort = 1025` and `Launch`'s argv/cmdline assembly
  carries `compass.gateway_port=1025` (the `microvm_lifecycle_test.go:31-58`
  BootConfig-assembly shape); (3) `AgentGatewayEndpoint` returns the suffixed
  path for a created session and `false` for an unknown name; (4) an
  `agentHost` over a fake engine WITHOUT the probe provisions exactly as
  today — socket served pre-Launch, both mounts appended (existing
  `host_test.go` rows green unchanged: the podman regression guard); (5) an
  `agentHost` over a fake engine WITH the probe: no pre-Launch serve, the
  spec reaching the engine carries **only** the workspace mount, the listener
  recorded post-Launch serves the real generated handler at the fake's
  suffixed path (dialable via `runnertest.DialAgentSocket`,
  `runnertest/socket.go:81-99` — the path is a plain AF_UNIX socket, so the
  hermetic gateway client needs no vsock), and `Remove`/`Close` still tear
  the listener down (`host_test.go:425-459` shape); (6) a fake whose
  `Serve` path is made to fail (a pre-occupied suffixed path) has the
  launched session torn down via `h.runtime.Teardown` and Provision erring;
  (7) a fake-probe engine with a live session + one `RefreshConfig` pass
  asserts NO Reload/Stop+StartAgent is issued for the probed session, while a
  no-probe fake still refreshes exactly as today; (8) an over-long RunRoot
  fails at `Create` with the operator-actionable budget error, before any
  Launch or Serve.

### W3 — KVM-gated end-to-end: in-guest round-trip + non-goal checks

The parent's V4 test cycle (microvm-runner.md:528-532) on hardware, as a
`//go:build microvm && unix` suite beside `contract_microvm_test.go`, each
test opening with `microvmtest.Require(t)`.

- **Interfaces:** consumes W1+W2 through the public `MicroVMRuntime` /
  `agentHost` surfaces, `microvm.Launch` + `GuestClient` for direct guest
  driving (the `boot_microvm_test.go:32-65` harness pattern), and the
  in-guest exec surface (`GuestExec`) for the guest-side probe. Produces the
  KVM-gated test files only — no new production code.
- **Test cycle (KVM-gated):**
  1. **Host-side serving over the real suffixed socket:** boot a session
     through the full backend; assert the host listener exists at
     `<runtimeDir>/vsock.sock_1025` (0600, Runner-owned) and answers a
     Comms call dialed host-side via `runnertest.DialAgentSocket` against a
     fake Server relay — the `e2e_transport_test.go:5-12` flow with the
     dial target swapped to the suffixed path.
  2. **The vsock leg, in-guest:** exec (agent uid, via the gate-opened exec
     surface) a probe that connects to `/run/compass/agent.sock` inside the
     guest and drives one Comms round-trip + one Publish frame + a Control
     subscribe against the host's real Gateway (fake Server relay behind
     it), mirroring the leg set of `e2e_transport_test.go`. The probe is a
     small script over the guest toolchain's own runtime (the rootfs
     carries the agent toolchain closure,
     microvm-ci-dev-enablement.md:252-267); its exact vehicle is OQ-5.
  3. **Fail-closed windows:** a guest dial before the host serves the
     suffixed listener fails the connection (no wedge, no phantom success);
     a Comms call before Start binds the session is refused
     `CodePermissionDenied` (the `gateway.go:33-37` contract, now proven
     over vsock).
  4. **Non-goal: vsock is not IP.** From inside the armed guest netns
     (default-deny per V3), assert no IP destination reaches the gateway —
     and that check 2 still passes — proving the vsock path is orthogonal
     to the egress seal (microvm-runner.md:530-532).
  5. **Teardown symmetry:** `Remove` leaves no suffixed socket file and no
     listener; the session runtime dir is gone (the
     `microvm_lifecycle_microvm_test.go` Start-teardown assertions extended
     to the gateway socket).

## Tasks

- [ ] W1 — guestd parses `compass.gateway_port` and `Provision` starts the
      0600 agent-uid unix→vsock forwarder at `/run/compass/agent.sock`
      (lazy per-connection `vsock.Dial(2, port)`), fail-closed, no-proxy
      when the key is absent
- [ ] W2 — `microvm.GatewaySocketPath` + `BootConfig.GatewayPort` +
      `agentGatewayVsockPort = 1025`; `MicroVMRuntime.AgentGatewayEndpoint`;
      `agentHost.Provision` probe-gates the microVM path (no refused mounts,
      `gateway.Serve` on the suffixed path post-Launch, teardown-symmetric);
      `RefreshConfig` probe-gated (no config-churn restarts); pre-boot
      sun_path guard in `Create`; serve-path slog anchor; podman path
      byte-identical
- [ ] W3 — KVM-gated e2e: suffixed-socket serving, in-guest
      Comms/Publish/Control round-trip over vsock, fail-closed windows,
      vsock-is-not-IP non-goal check, teardown symmetry

## Open Questions

Batched for the pre-freeze ruling; the body designs against each
recommendation.

- **OQ-1 (load-bearing) — two divergences from the frozen parent's V4 prose.**
  (i) The parent injects the dial target "via the exec environment"
  (microvm-runner.md:519-522); as detailed, the agent cannot dial AF_VSOCK
  from Bun/Node (§(d)), so the design keeps the agent's fixed
  `/run/compass/agent.sock` contract (`cli.ts:86-89`) and has guestd bridge
  it to vsock — no new env var, agent byte-identical. (ii) The parent expects
  "per-session-port→VM identity, assigned and recorded by the backend at
  boot" (microvm-runner.md:135-137); as detailed, hybrid vsock makes the
  host endpoint a per-session AF_UNIX *path*, so the port is fixed
  (§(e)) — the exact identity model V2b froze for the control port
  (`microvm_lifecycle.go:48-52`). Both meet the parent's contract by a
  different mechanism than its sketch; under the flag-don't-silently-resolve
  posture a detailing record surfaces them. **Recommendation:** ratify both
  readings.
- **OQ-2 (load-bearing) — reuse `gateway.Serve` vs the parent's sketched
  sibling constructor.** The parent's Interfaces line promises "a
  vsock-transport constructor (sibling of `Serve`, `socket.go`)"
  (microvm-runner.md:524-527). Verified against CH's guest→host mechanism
  (§(a)), the host transport is a plain AF_UNIX listener and `Serve` is
  already path-parameterized (`gateway/gateway.go:298-326`), so the sibling
  would duplicate `Serve` with no behavioral difference; the V4-specific
  surface shrinks to `microvm.GatewaySocketPath` + the (c) probe.
  **Recommendation:** reuse `Serve` verbatim; no sibling constructor.
- **OQ-3 (load-bearing) — fleet config on microVM is deferred, and the
  deferral is honest ONLY WITH the W2 refresh gate.** The probe-gated
  Provision skips the config materialize+mount the microVM backend refuses
  today (`host.go:178-193`, `microvm_lifecycle.go:221-244`), so a microVM
  agent boots with the agent-side VALID-empty config state
  (`config-reader.ts:22-24`) until a config-delivery slice lands. But
  "safe-degraded" is a boot-time property only: the skipped block is also
  what seeds `h.configVersions` (`host.go:205-207`), so without the matching
  gate on `RefreshConfig`'s fan-out (§(f)) every fleet ConfigVersion publish
  Stop+StartAgent-restarts every live microVM agent mid-session while
  delivering nothing (`host.go:777-818,838-845`) — broken-in-steady-state,
  not degraded. The refresh gate is therefore in-scope W2 work, not part of
  the deferral. Boot-time empty config remains strictly better than today
  (Provision fails outright) but is a real capability gap vs podman.
  **Recommendation:** accept the config-delivery deferral for V4 *with* the
  refresh gate landed in W2; file the config-delivery follow-up issue at the
  freeze→file→dispatch gate so it is tracked work, not a silent gap.
- **OQ-4 (non-load-bearing) — the gateway port value and its cmdline
  optionality.** `1025` (beside guestd's `1024`), carried as
  `compass.gateway_port` mirroring `compass.vsock_port`
  (`launch.go:255-256`, `cmdline.go:12-19`); absent-key ⇒ no proxy, so
  V2a/V2b/V3 harness cmdlines stay valid. Any non-reserved uint32 works —
  identity rides the path, not the port (§(e)).
  **Recommendation:** `agentGatewayVsockPort = 1025`, optional key.
- **OQ-5 (non-load-bearing) — the W3 in-guest probe vehicle.** The
  round-trip needs a Connect h2c client inside the guest. Candidates: a bun
  one-liner over the toolchain runtime the rootfs already carries
  (microvm-ci-dev-enablement.md:252-267), or a small Go probe binary added
  to the guest image for tests. The bun script needs no image change; the Go
  probe is sturdier against toolchain drift. **Recommendation:** the bun
  script first; fall back to a test-only Go probe if the toolchain runtime
  proves awkward to drive headless — W3's implementer decides on hardware,
  neither choice touches production surface.
- **OQ-6 (non-load-bearing) — proxy concurrency bounds.** The forwarder
  accepts unboundedly and each connection costs two goroutines + one host
  vsock connect. The only in-guest client is the agent (single tenant, agent
  uid), and the host side already bounds per-message size
  (`gateway.go:44-53`); a connection-count cap would defend against a
  runaway in-guest process at the cost of wedging a legitimate reconnect
  storm. **Recommendation:** no cap in V4; revisit under V8's adversarial
  probes if the escalation suite wants one.
- **OQ-7 (load-bearing) — the microVM gateway's host-side identity binding is
  coarser than podman's.** On podman, access to the agent socket is
  filesystem-enforced: a 0600 socket inside a 0700 Runner-owned dir
  ("socketFileMode … owner read/write only. Meaningful only because the
  parent dir is not traversable by other host users (socketDirMode)",
  `gateway/socket.go:34-42`). On microVM, an AF_VSOCK `connect(CID 2, 1025)`
  in the guest needs no privilege and no filesystem path, so ANY in-guest uid
  can reach the host Gateway directly, bypassing guestd's 0600
  `/run/compass/agent.sock` forwarder entirely — the effective host-side
  binding coarsens from "the agent uid in this container" to "anything in
  this VM". The Gateway still fails closed pre-Start (no bound session ⇒
  `CodePermissionDenied`, `gateway/gateway.go:33-37`), and the guest is
  single-tenant with the exec surface running everything at
  `default_exec_uid`, so this is defensible — but it is a security-posture
  ruling, not an implementation detail, and the record must not make it
  silently. **Recommendation:** ratify the coarsened binding as accepted V4
  posture; name in-guest AF_VSOCK fencing (a guest-side LSM/seccomp fence on
  the gateway port for non-guestd uids) as future V8 adversarial-hardening
  work, NOT V4 scope.
