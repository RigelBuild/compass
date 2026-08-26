# microVM Runner V2b — in-guest supervisor + vsock exec control plane

Status: PROPOSED — details the V2b milestone under the frozen parent [microvm-runner.md](./microvm-runner.md) (its Plan § V2b, microvm-runner.md:466-490).

## Problem / Intent

V2a proved the channel: a nix-packed guest boots rootless under
cloud-hypervisor, `compass-guestd` runs as PID 1, and one `GuestControl.Health`
round-trip answers over hybrid vsock. V2b is the control plane proper — the
parent's schedule-critical milestone (microvm-runner.md:466-476): grow
`compass-guestd` into the real in-guest supervisor serving an exec surface
(`Exec`/`ExecStream`/`Signal`/`Provision`, grown additively onto the V2a proto
seed, `proto/compass/v1/guest_control.proto:20-22`), and implement
`MicroVMRuntime`'s Create/Start/Exec/ExecStreaming/Stop/Remove against it so
the microVM backend behaves identically to `PodmanCLI` through the
`ContainerRuntime` surface (`go/internal/runtime/podman.go:303-352`). Every
method today is a typed stub ("returned by every MicroVMRuntime method until
the in-guest control plane lands", `go/internal/runtime/microvm.go:48-55`);
V2b fills them. Egress arming stays V3's; the gateway transport stays V4's.

## Approach

Each subsection resolves one fork the parent's V2b plan leaves to detailing
(microvm-runner.md:466-490). Every resolution is also listed in
`## Open Questions` for the pre-freeze batch; the body designs against the
recommended option so the record is coherent.

### (a) `GuestControl` proto growth: the exec surface, additively

The V2a seed reserved exactly this growth: "V2a seeds it with the single spike
RPC; V2b grows the service additively (Exec/ExecStream/Signal/Provision) with
no wire break" (`proto/compass/v1/guest_control.proto:20-22`). V2b adds four
RPCs and their messages to the existing file — a new-RPC/new-message-only
change, so `buf breaking` is satisfied by construction, and the file stays on
the internal-only lane (`buf.gen.internal-go.yaml:44-51` lists it; output goes
to `go/internal/gen`, "NEVER add `--include-imports`",
`buf.gen.internal-go.yaml:34`). No TS lane, no public `go/gen` output.

```proto
service GuestControl {
  rpc Health(HealthRequest) returns (HealthResponse);           // V2a, unchanged
  rpc Exec(ExecRequest) returns (ExecResponse);                 // one-shot
  rpc ExecStream(stream ExecStreamRequest)
      returns (stream ExecStreamResponse);                      // bidi streaming
  rpc Signal(SignalRequest) returns (SignalResponse);           // kill an exec / stop the guest
  rpc Provision(ProvisionRequest) returns (ProvisionResponse);  // V2b: gate transition; V3: nft arm
}
```

**One naming deviation from the parent's sketch, flagged (OQ-B).** The parent
sketched `ExecStream(stream ExecStreamFrame) returns (stream ExecStreamFrame)`
(microvm-runner.md:480). A single `ExecStreamFrame` in both positions violates
buf's `RPC_REQUEST_STANDARD_NAME`/`RPC_RESPONSE_STANDARD_NAME` rules — the very
rules the V2a seed chose its message names to satisfy ("`Health` ->
`HealthRequest`/`HealthResponse` already satisfies buf's
RPC_REQUEST/RESPONSE_STANDARD_NAME — no carve-out",
`guest_control.proto:27-28`) — and the two directions carry disjoint payloads
anyway (stdin down, stdout/stderr/exit up). V2b uses
`ExecStreamRequest`/`ExecStreamResponse`, each a `oneof` of direction-specific
frames. A naming refinement, not a semantic contradiction of the frozen parent.

**Messages.** The one-shot pair mirrors `ExecSpec`/`ExecOutput`
(`podman.go:117-130,156-160`) field for field:

```proto
message ExecRequest {
  repeated string command = 1;          // ExecSpec.Command
  optional uint32 uid = 2;              // ExecSpec.User; absent = session default uid (see (b))
  optional string workdir = 3;          // ExecSpec.Workdir
  map<string, string> env = 4;          // ExecSpec.Env, merged over the session base env
  optional bytes stdin = 5;             // ExecSpec.Stdin — body over the wire, NEVER guest argv
  uint32 timeout_seconds = 6;           // host's per-command cap, enforced guest-side too
}
message ExecResponse {
  bytes stdout = 1;
  bytes stderr = 2;
  int32 exit_code = 3;                  // non-zero exit is a RESPONSE, never a Connect error
}
```

A non-zero exit travels in `ExecResponse.exit_code`, never as a Connect error —
the wire mirror of the interface contract ("A non-zero exit is a successful
runtime call returning a failed command (ExecOutput.ExitCode), not an error;
only a spawn failure or timeout is an error", `podman.go:310-313`). A Connect
error from `Exec` means the exec could not be *attempted* (spawn failure,
refused uid, unprovisioned gate) — exactly the cases `PodmanCLI.Exec` folds
into its non-nil error return (`podman.go:517-520`).

The streaming pair carries the whole `StreamingExec` lifecycle over one bidi
stream:

```proto
message ExecStreamRequest {
  oneof frame {
    StartExec start = 1;                // first frame, exactly once
    bytes stdin = 2;                    // stdin bytes, any number of frames
    StdinClose stdin_close = 3;         // half-close: guest closes the child's stdin pipe
  }
}
message StartExec {
  repeated string command = 1;          // StreamingExecSpec.Command
  optional uint32 uid = 2;              // StreamingExecSpec.User; absent = session default uid
  optional string workdir = 3;          // StreamingExecSpec.Workdir
  map<string, string> env = 4;          // StreamingExecSpec.Env
}
message StdinClose {}
message ExecStreamResponse {
  oneof frame {
    ExecStarted started = 1;            // first frame: the exec_id Signal targets
    bytes stdout = 2;
    bytes stderr = 3;
    ExecExit exit = 4;                  // terminal frame, exactly once
  }
}
message ExecStarted { string exec_id = 1; }
message ExecExit {
  int32 exit_code = 1;                  // meaningful when signal == 0
  int32 signal = 2;                     // non-zero when the exec died by signal (e.g. SIGKILL)
}
```

`StreamingExecSpec` deliberately carries no stdin script (`podman.go:165-169`:
"a streaming exec keeps live stdio pipes for a long-running process (the
agent), for which a one-shot stdin feed is meaningless") — but the *pipes*
include a live stdin writer (`StreamingIO.Stdin io.WriteCloser`,
`podman.go:203-207`), so the stream's `stdin`/`stdin_close` frames exist to
carry that pipe, not a script.

**Kill rides `Signal`, not the stream.** `ChildHandle` separates Kill from
Wait ("this handle can only signal or await the process", `podman.go:209-211`),
and the session manager calls Kill/Terminate from the teardown path while the
stream's consumer may be mid-send. A connect-go client stream's `Send` is not
safe for concurrent use, so multiplexing kill onto the request stream would
force a lock around every stdin write. Instead the `started` frame returns an
`exec_id`, and kill is a separate unary:

```proto
message SignalRequest {
  string exec_id = 1;                   // "" targets the guest itself (Stop, see (d))
  int32 signal = 2;                     // SIGKILL for ChildHandle.Kill; SIGTERM for Stop
}
message SignalResponse {}
```

Signalling an already-exited exec succeeds as a no-op — matching "killing an
already-exited process is not an error" (`podman.go:222-224`). Wait remains
"read the stream to its `exit` frame"; the terminal frame always arrives
(guestd emits it from its own reap, see (b)), so the host's Wait never hangs on
a killed child.

**`Provision` is defined here, filled in V3.** The parent assigns the
`ProvisionRequest{NftScript}` *handling* to V3 (microvm-runner.md:498-501) but
lists the RPC in V2b's produced surface (microvm-runner.md:478-482). V2b
defines the messages and uses `Provision` as the exec-gate transition ((b));
the nft-script branch errors as unimplemented until V3:

```proto
message ProvisionRequest {
  string nft_script = 1;                // V3: EgressPolicy.NftScript(); empty in V2b
  uint32 default_exec_uid = 2;          // the session's agent uid (ContainerSpec.UID)
  map<string, string> base_env = 3;     // ContainerSpec.Env, the base env every exec inherits
}
message ProvisionResponse {}
```

### (b) `compass-guestd` grown into the supervisor

V2a's guestd is a fail-closed PID-1 boot sequence that ends by serving
`Health` and idling ("(5) serve the vsock Health handshake, (6) idle inside
serve until ctx is cancelled", `go/internal/guestd/guestd.go:90-98`). V2b keeps
steps 1-4 byte-identical — API mounts, cmdline, DHCP net bringup, virtio-fs
mount, each still fail-closed — and replaces "idle" with the supervisor: the
full `GuestControl` handler served over the same AF_VSOCK h2c door
(`guestd/vsock.go:41-46` — the mux gains the new RPCs; the h2c wiring and
graceful drain are unchanged).

**Boot order and the armed gate.** The guest boot order the parent fixes for
V2b is "mount virtio-fs, arm egress per V3, then serve exec"
(microvm-runner.md:468-469), and the parent's integrity model requires "Only
after a successful arm does the supervisor accept exec requests"
(microvm-runner.md:152-155). V2b builds that gate structurally as a supervisor
state machine — `booting → ready → provisioned` — even though the nft arm
itself is V3's:

- `ready` (net + mount succeeded): `Health` answers; `Exec`/`ExecStream` are
  REFUSED with a typed Connect error (failed-precondition).
- `Provision` transitions `ready → provisioned`: it records the session's
  default exec uid and base env, and — when `nft_script` is non-empty — runs
  the arm step. In V2b a non-empty script is an error (unimplemented; the host
  never sends one until V3); V3 replaces that branch with the real `set -eu`
  fail-closed arm (`egress.go:76-79`). A failed Provision leaves the gate
  closed and the host tears the VM down — the same posture as a failed arm
  today ("the script aborts non-zero and the caller tears the container down
  rather than running it unfirewalled", `egress.go:77-79`).
- `provisioned`: exec accepted. `MicroVMRuntime.Start` calls Provision as its
  final step ((c)), so by the time `Start` returns, the microVM session is in
  the same state a started podman container is in after `provision` runs
  (`agent.go:288-298`).

This means the GUEST side of V3 lands as a handler-body change plus the
`AgentRuntime` routing — the gate, the state machine, and the wire surface are
already in place. The HOST side is NOT yet plumbed, and V2b makes one
structural bet worth flagging: `Start(ctx, id)` (frozen, `podman.go:310`)
carries no spec, yet Start is where V2b buries the `Provision` call, so V3's
`ProvisionRequest.nft_script` must come from somewhere Start can reach. On
podman the arm rides a post-Start root-capable `Exec` from
`AgentRuntime.provision` (`agent.go:293-307`) — an exec path the microVM
backend REFUSES (uid-0/caps). So V3 cannot reuse that seam: the intended data
path is `ContainerSpec` growing an egress field captured at Create and
delivered by Start's Provision call (podman.go:89-116 has no egress field
today). V2b does not build that field, but it records the assumption here so
V3's designer inherits it explicitly rather than discovering the gap; if the
parent's "AgentRuntime routing" instead means a new backend-visible seam, that
is V3's open question to resolve, not a silent V2b premise.

**Exec spawning and supervision.** guestd is guest PID 1, so it is already
every orphan's reaper. Each exec is spawned as a direct child in its own
process group; guestd owns the child's stdio pipes and pumps them onto the
stream (`stdout`/`stderr` frames), reaps the child on exit, and emits the
terminal `exit` frame from the reaped status. Each child is bound to its
request/stream context: if the `ExecStream` breaks (host disconnect, ctx
cancel) or a one-shot `Exec` RPC's context is cancelled before the child
exits, guestd SIGKILLs and reaps the child rather than leaking it until VM
teardown — the guest-side half of the host's ctx-cancel-terminates-the-exec
contract ((c), `podman.go:321-323`). This child-of-guestd shape IS
the D4 agent supervision seam ("`compass-guestd` is *also* the in-guest Go
process that supervises the compass-agent (restart/health/lifecycle)",
microvm-runner.md:708-711): the agent arrives as an `ExecStream` child
(`engine.ExecStreaming(ctx, id, env.execSpec())`, `agent_exec.go:140`), its
death is observed by guestd's reap and reported as the `exit` frame, and the
*restart policy* stays the host Runner's — exactly as it is on podman, where
the Runner watches `ChildHandle.Wait` "to tell an unexpected agent exit
(crash) from a deliberate stop/teardown kill" (`podman.go:230-233`). guestd
adds no autonomous restart in V2b.

**uid enforcement.** The parent's Global Constraint: "the guest supervisor
refuses exec specs requesting uid 0 or capabilities"
(microvm-runner.md:358-360). guestd rejects any `Exec`/`StartExec` whose `uid`
is 0 with a typed Connect error, before spawning anything. An *absent* uid
resolves to the session's default exec uid delivered by `Provision`
(`default_exec_uid`, the `ContainerSpec.UID` — the baked agent uid,
`podman.go:109-113`) — mirroring podman's "Nil runs as the image's default
user (for the compass-agent image that is uid 1000, not root)"
(`podman.go:119-121`), with the default supplied per session instead of baked
into an image. The default uid is validated non-zero at Provision. Every
spawned child gets an empty capability set (no ambient caps; guestd sets
no inheritable caps), preserving "the agent then runs as a non-root user whose
capability set is empty, so it cannot flush or edit the ruleset"
(`egress.go:7-9`).

**Peer-CID authentication.** The parent freezes it: "The supervisor
authenticates its vsock peer: it accepts control requests only from the host
(CID 2) and refuses the in-guest loopback (CID 1)"
(microvm-runner.md:158-164). V2b implements it at the listener: the accepted
connection's remote vsock address is checked before any HTTP byte is read, and
a non-host CID is closed immediately. This lands in V2b (not V8, which only
*probes* it) because V2b is what turns the port from a Health responder into
an exec surface worth escalating to.

**Env base.** `ContainerSpec.Env` on podman is set on the container and thus
visible to execs; on the microVM backend the same base env arrives via
`Provision.base_env` and guestd merges it under each exec's own `env` map
(exec-specific keys win). Host-side assembly stays deterministic exactly as
the podman argv is (`sortedEnv`, `podman.go:805-807`) — ordering is a map on
the wire, so determinism matters only for logging/tests.

### (c) `MicroVMRuntime` methods against the vsock service

The nine frozen signatures (`microvm.go:71-116`, `var _ ContainerRuntime =
(*MicroVMRuntime)(nil)`) are filled by translating each verb onto V2a's
harness + the (a) service. `MicroVMRuntime` grows a per-session state table
(`ContainerID → *session`), where a `session` holds the V2a `BootConfig`
(`go/internal/runtime/microvm/config.go:21-35`), the running `*microvm.VM`
handle, the `GuestControl` client, and the runtime dir. The `microvm` package
"depends on nothing in go/internal/runtime, so importing it there introduces
no cycle" (`config.go:5-7`) — V2b is the planned importer.

- **`Create(ctx, ContainerSpec) (ContainerID, error)`** allocates without
  booting (mirroring `podman create`): mint a session id (the `ContainerID` —
  there is no engine to print one, so the backend generates a random hex id
  and derives the runtime dir from it), create the per-session runtime dir
  (`<runroot>/microvm/<session>/` — the layout V7 formalizes with pidfiles,
  microvm-runner.md:589-591), assemble the `BootConfig` (kernel/initrd/rootfs
  from `MicroVMConfig` (`microvm.go:25-35`), fresh AF_UNIX socket paths inside
  the runtime dir, a fresh guest CID ≥ 3 and vsock port, `FSSharedDir` from
  the spec's workspace mount), and record `spec.UID` + `spec.Env` for
  Provision. `spec.Command` is IGNORED on this backend: the sleep-loop
  keep-alive exists only because podman needs a main process ("Keep the
  container alive so the Runner can exec into it", `agent.go:269-271`); a VM's
  keep-alive is the VMM process itself, and guestd is the guest's PID 1.
  `spec.CapAdd` is likewise ignored — it exists to let the podman entrypoint
  arm nft (`podman.go:95-98`); in the microVM model "CAP_NET_ADMIN never has
  to be granted to the workload boundary at all" (microvm-runner.md:167-170).
  Both ignores are asserted (not silent) in the contract suite. Mount
  handling: see OQ-C — V2b boots the single workspace share
  (`Mount{HostPath, ContainerPath}` → `FSSharedDir`/mount point), and refuses
  a spec with mounts it cannot express rather than dropping one.
- **`Start(ctx, id)`** boots and provisions: `microvm.Launch(ctx, cfg)`
  (`launch.go:99-106` — starts virtiofsd, passt, cloud-hypervisor, with the
  no-orphan error path), then polls `Health` under a boot deadline until
  `net_provisioned && workspace_mounted` (the V2a fail-closed proof,
  `guestd.go:11-15`), then calls `Provision(default_exec_uid, base_env)` to
  open the exec gate ((b)). Any step failing tears down what started
  (`vm.Shutdown`, `launch.go:327-333`) before returning the error — the
  transactional posture of `createAndStart`'s remove-on-start-failure
  (`agent.go:278-284`), adapted: on this backend the boot IS Start, so Start
  cleans its own partial boot and `Remove` stays idempotent.
- **`Exec(ctx, id, ExecSpec) (ExecOutput, error)`** maps the spec onto
  `ExecRequest` field-for-field ((a)): `User` (a numeric-string uid on every
  Runner callsite, e.g. `AsUser(strconv.FormatUint(uint64(uid), 10))`,
  `agent.go:249-250`) parses to the `uid` field — a non-numeric User is a
  host-side error; `Stdin` goes to `stdin` bytes (preserving WriteAgentFile's
  invariant: "the body is fed over stdin, never argv — argv is visible in the
  container's process list while stdin is not", `agent.go:238-246` — on this
  backend the wire replaces the pipe, and guestd feeds the bytes to the
  child's stdin pipe, so the body still never appears in the guest's process
  list). The response maps to `ExecOutput{Stdout, Stderr, ExitCode}`
  (`podman.go:156-160`); a guest-side refusal (gate closed, uid 0) or
  transport failure maps to a non-nil error, tagged with the same
  `CommandError`/`TimeoutError` discipline (`podman.go:273-296`) so
  `requireSuccess`/`atStage` callers (`agent.go:136-146`) behave identically.
  The per-command timeout is enforced host-side (ctx deadline on the RPC) and
  mirrored guest-side (`timeout_seconds`) so a wedged child cannot outlive its
  caller's interest — matching `defaultCommandTimeout`'s posture ("a hung
  podman … must surface as an error, never block the calling task forever",
  `podman.go:354-357`).
- **`ExecStreaming(ctx, id, StreamingExecSpec) (*StreamingExec, error)`**
  opens the (a) bidi stream, sends `start`, and awaits the `started` frame —
  only then does it return, so a spawn failure surfaces as the non-nil error
  the contract demands (`SpawnError`-equivalent) rather than a broken pipe on
  first read. It returns a real `*StreamingExec` (`podman.go:250-257`): the
  `StreamingIO` pipes are `io.Pipe` pairs pumped by a per-exec goroutine that
  writes `stdin` frames from the Stdin pipe (Close → `stdin_close`) and
  demuxes `stdout`/`stderr` frames onto the respective read pipes, closing
  them when the `exit` frame arrives — so `AgentStream`'s drain model ("Drain
  both pipes continuously so a full OS pipe buffer can never stall the agent",
  `agent_exec.go:150-152`) and its EOF-driven teardown ordering
  (`agent_exec.go:192-199`) work unchanged above the seam. The `ChildHandle`
  problem: the podman handle wraps a live `*exec.Cmd`
  (`podman.go:217-220`), which does not exist host-side here. Rather than
  widen the frozen interface, `ChildHandle` gains a second internal
  construction over a `killFunc`/`waitFunc` pair (same exported
  Kill/Wait/Terminate surface). Kill → `Signal(exec_id, SIGKILL)` issued with
  a short internal deadline and NEVER blocking the caller past it: podman's
  Kill is an instantaneous local `h.cancel()` (`podman.go:214-217`), so the
  microVM Kill must not block on a wedged transport in the teardown path —
  transport errors are ignored (the VMM-kill escalation in `Stop` is the
  backstop), and Wait still returns via the demux goroutine's exit/error path
  even when Kill's RPC never lands. Wait → block until the demux goroutine
  sees `exit`, returning nil on exit 0 and otherwise an error carrying the
  code/signal. Crucially, that error is the portable deliberate-kill error
  OQ-G/U3b introduces (`runtime.ExitStatusError`), NOT a fabricated
  `*exec.ExitError` — `*exec.ExitError` embeds `*os.ProcessState`, which a
  remote `waitFunc` cannot construct, so `Stop`'s `isDeliberateKill` check can
  only recognize the microVM SIGKILL once U3b widens it
  (`agent_exec.go:200-207,232-239`). Cancelling the parent ctx kills the exec
  host-side (aborting the stream) AND guest-side: guestd binds each child to
  its request/stream context and SIGKILLs + reaps it when the stream breaks
  ((b)), so no guest process is orphaned — matching "the exec is still bound
  to ctx, so cancelling it terminates the process" (`podman.go:321-323`)
  end-to-end, not merely host-side.
- **`Exists(ctx, name) (bool, error)`** answers a NAME query (`podman.go:337`)
  from the session table — which V2b keys/indexes by `spec.Name`, not only the
  random session id, and records `spec.Name` at Create so a name probe
  resolves (plus the on-disk runtime dir, so a crashed-Runner orphan is
  visible pre-V7). podman's `container exists` probe (`podman.go:632-653`) has
  no engine equivalent here. Duplicate-name Create is refused with a typed
  error, matching podman's engine (which rejects a second container of the
  same name) — the retry cleanliness of `createAndStart` and agentHost's
  per-name locking (`agent.go:278-284`) lean on that refusal.
- **`MountLabel(ctx, id)`** returns `""` per the parent's Q-mountlabel
  deferral ("Resolved during V2b by making `MountLabel` return the empty
  label and routing the config materializer accordingly",
  microvm-runner.md:843-848). The consumer already treats empty as skip-chcon
  (`config_materialize.go:185-188`, `if mcsLabel != ""`), so the routing needs
  no change — asserted by a contract-suite row.
- **`Resize`** keeps returning `ErrResizeNotImplemented` — the same posture as
  `PodmanCLI.Resize` (`podman.go:621-623`); the behavior is C3's (D5).

### (d) Stop/Remove teardown (parent §(f))

- **`Stop(ctx, id, timeout)`** is graceful-then-kill, mirroring podman's
  `--time` semantics (`podman.go:580-590`): send `Signal(exec_id: "", signal:
  SIGTERM)` — the empty exec_id targets the guest itself. Note the mechanism
  is NOT V2a's `signal.NotifyContext` path (`main.go:37`, a Unix-signal
  handler): the Stop trigger is an RPC, so guestd wires the empty-exec_id
  `Signal` to cancel the supervisor's serving context, SIGTERMs its children,
  and drains. Then — because guestd is guest PID 1 and a PID-1 process *exit*
  panics the kernel (`main.go:11-13`), which cloud-hypervisor never observes
  as a VMM exit (the vCPU wedges absent `panic=`/reset handling) — the drain
  ends in an explicit `reboot(RB_POWER_OFF)` (PID-1-legal), which the VMM DOES
  observe as guest shutdown and exits on. So the host awaits a real VMM exit up
  to `timeout`; past it, kill the VMM outright. Without the power-off, every
  graceful Stop would burn the full timeout and the SIGTERM preamble would be
  dead weight. The V2a harness already implements the kill half: "the VMM is
  killed first (a VM gets no graceful drain)" (`launch.go:327-333`) — V2b adds
  the graceful preamble in front of it and keeps `vm.Shutdown`'s reap of
  virtiofsd/passt and socket/pidfile removal as the second phase. In-flight
  execs fail with the distinguishable error the parent requires
  (microvm-runner.md:244-248): their streams break and the demux goroutines
  emit the typed error.
- **`Remove(ctx, id)`** force-kills if still running (`vm.Shutdown` is
  `sync.Once`-guarded and safe to call twice, `launch.go:330-331`), then
  deletes the runtime dir and drops the session-table entry. Idempotent on an
  already-dead or already-removed VM ("Remove is idempotent on an already-dead
  VM", microvm-runner.md:248) — so `Teardown`'s Stop-then-Remove sequence
  (`agent.go:216-220`) and `createAndStart`'s best-effort remove
  (`agent.go:281-283`) both hold.
- **Mid-session death** (VMM dies, virtiofsd dies) is detected by V2b only as
  broken streams/RPCs failing with typed errors; the supervision matrix —
  liveness watching, orphan reaping at startup, metrics — is V7's
  (microvm-runner.md:583-598). V2b MUST NOT leave anything V7 would have to
  undo: every process handle and pidfile it creates lives in the V7-shaped
  runtime dir layout.

### (e) The exec-transport authentication fork

Resolved as: structural identity is sufficient; add a boot nonce as a cheap
hardening.

The V2a proto header flags it: "When V2b adds the higher-value RPCs
(Exec/Signal/Provision), that assumption must be revisited explicitly: an Exec
surface may warrant the host authenticating guestd's identity beyond reaching
it on the expected socket" (`guest_control.proto:43-46`). Revisited here,
explicitly. Threat model, both directions:

- **Host authenticating the guest (the flagged direction).** The host reaches
  guestd through an AF_UNIX socket ONLY the VMM serves (`--vsock cid=…,
  socket=<path>`, `dial.go:17-20`), living in the per-session runtime dir the
  backend itself created with the invoking user's permissions. There is no
  network hop, no shared namespace, no multi-writer path: the socket path IS
  the session's structural identity, the same argument the V2a header makes
  for Health (`guest_control.proto:40-43`). What could sit behind that socket
  other than guestd? Only a compromised *guest* (the workload escalated to
  guest root or replaced guestd). But a guest that owns guest-root already
  owns everything guest-side authentication could protect — the exec results,
  the workspace, the session's own data. A credential would not distinguish
  "guestd" from "guest-root malware holding guestd's memory", because any
  secret guestd can present, a guest-root attacker can extract. So
  host-authenticates-guest adds no security boundary that the VM boundary +
  socket ownership do not already provide.
- **Guest authenticating the host (the direction that actually matters).**
  The exec surface's real exposure is an unauthorized CALLER: an in-guest
  process dialing the supervisor port over vsock loopback and requesting a
  uid-0 exec or a re-arm. That is the parent's frozen peer-CID check
  (microvm-runner.md:158-164), implemented in V2b ((b)) and probed by V8's
  escalation test (microvm-runner.md:618-621). Host-side, the AF_UNIX socket
  is 0700-dir-protected per session; any same-uid process that can open it
  can also just kill the VMM — same-uid is outside the threat model, as it is
  for podman's own control socket.

**Recommendation (OQ-A, load-bearing): no credential protocol in V2b.**
Peer-CID checking (frozen, guest-side) plus structural socket identity
(host-side) is the boundary. One cheap hardening is worth its two lines: the
host generates a random **boot nonce** per session, passes it on the kernel
cmdline (`compass.boot_nonce=<hex>`, beside the existing
`compass.vsock_port`, `launch.go:226-229`), and guestd echoes it in
`HealthResponse`. Start verifies the echo before Provision. This is not
authentication against an adversary — the cmdline is host-controlled and
guest-readable — it is *liveness/identity binding*: it proves the guest
answering the handshake is the one booted from THIS BootConfig, catching a
stale VMM on a recycled socket path or a crash-recovery mixup (the V7 orphan
class) structurally rather than by timing. Its scope is honestly narrow: it
binds identity ONCE, at Start's Health poll, and no subsequent RPC carries
it, so it catches the boot-time stale-VMM/recycled-socket class and nothing
after Provision — post-Start RPC identity remains structural-socket-plus-
peer-CID only. Given session dirs already use fresh random hex ids, that
boot-time class is itself narrow; the nonce is two lines for a real if thin
invariant, not theater. Rejected alternatives: mTLS over
vsock (a per-session CA and cert plumbing to defeat an attacker who, holding
guest root, has already won) and a bearer token for the host→guest direction
(same key-extraction objection, plus a secret on the cmdline is
guest-world-readable via /proc/cmdline — worse than no secret). If Matt rules
the other way, the nonce slot is where a real credential would ride, so the
design degrades gracefully into that ruling.

## Global Constraints

Every task below inherits these; they restate the parent's binding decisions
in V2b-concrete form.

- **The `ContainerRuntime` contract is the acceptance bar.** Every filled
  method behaves identically to `PodmanCLI` through the interface
  (`podman.go:303-352`), specifically: a non-zero exec exit is a successful
  call returning `ExecOutput.ExitCode`, never an error (`podman.go:310-313`);
  `ExecStreaming` returns live `StreamingIO` pipes + a `ChildHandle` whose
  Kill/Wait/Terminate semantics match (`podman.go:209-248`), including
  Wait-after-SIGKILL yielding a deliberate-kill error `isDeliberateKill`
  recognizes — via the portable exit-signal error taxonomy OQ-G/U3b introduces,
  since a remote `waitFunc` cannot forge the `*exec.ExitError` the frozen check
  matches today (`agent_exec.go:232-239`); exec stdin bodies never appear in
  any process list (`agent.go:238-246`); uid-0 exec is refused
  (microvm-runner.md:358-360); `Remove` is idempotent; `Resize` returns
  `ErrResizeNotImplemented` (`podman.go:608-614`); `MountLabel` returns `""`
  (microvm-runner.md:843-848).
- **Acknowledged divergences from byte-identical podman behavior.** The
  identity claim above is behavioral, not byte-for-byte; V2b concedes a bounded
  set of divergences, collected here so U5 has its exclusion list and the claim
  stays honest: (1) one-shot exec output is capped (OQ-E, 8 MiB) and truncation
  is an explicit error, where podman's `spawnCapture` is unbounded
  (`podman.go:689-699`); (2) `ExecSpec.User` must be a numeric uid (a
  non-numeric User is a host-side error), where podman resolves image user
  names; (3) `MountLabel` returns `""` (no MCS labelling on this backend); (4)
  `spec.Command` and `spec.CapAdd` are ignored-and-asserted (no keep-alive
  process, no capability grant to the workload); (5) multi-mount specs beyond
  one workspace share are refused, not dropped (OQ-C); (6) the deliberate-kill
  error is a portable type, not `*exec.ExitError` (OQ-G/U3b). Every item is
  asserted by a contract-suite row, so a divergence that silently widens is a
  test failure.
- **Additive, buf-breaking-safe proto; internal-go-only.**
  `guest_control.proto` grows by new RPCs, new messages, and one additive
  field on `HealthResponse` (the boot-nonce echo, (e)) — the existing `Health`
  request/response FIELDS and their semantics are unchanged, so the change is
  wire-compatible and `buf breaking`-safe, but the Health *response message* is
  not byte-identical (it gains `boot_nonce`). Generation stays exclusively on
  `buf.gen.internal-go.yaml` (`buf.gen.internal-go.yaml:44-51`) into
  `go/internal/gen`; the file never enters the public or TS lanes
  (`guest_control.proto:13-18`). New M-mappings are NOT needed (the file is
  already listed); `--include-imports` stays forbidden
  (`buf.gen.internal-go.yaml:34-41`).
- **KVM-gated vs hardware-independent test split.** Everything that boots a
  VM carries `//go:build microvm && unix` and calls `microvmtest.Require(t)`
  first (skip-on-absent-KVM, hard-fail under `COMPASS_REQUIRE_MICROVM=1`,
  `microvmtest.go:107-128`). Everything else — proto round-trips, spec→message
  mapping, the stream demux/pipe pump, the ChildHandle semantics, guestd's
  gate state machine, uid refusal — runs hermetically over in-memory
  transports (the V2a pattern: "a hermetic round-trip of the generated
  Health client/handler over the fake transport (no KVM needed)",
  microvm-v2a-guest-image-boot-spike.md:527-530). The shared contract suite is
  table-driven with the microVM rows KVM-gated and the podman rows gated on
  rootless podman, per the parent's V2b test cycle (microvm-runner.md:485-490).
- **Fail-closed gate ordering (parent §(c)).** guestd serves exec ONLY in the
  `provisioned` state; the state machine ships in V2b with the arm branch
  stubbed for V3, and no code path may accept an exec before Provision
  succeeds (microvm-runner.md:152-155).
- **Peer-CID authentication is in-scope.** The vsock listener refuses non-host
  CIDs before reading application bytes (microvm-runner.md:158-164); V8 only
  probes what V2b builds.
- **Rootless is hard (parent GC).** Every host-side process V2b spawns — VMM,
  virtiofsd, passt — runs as the invoking user via the V2a harness
  (`launch.go:99-106`); no new capability, no rootful helper
  (microvm-runner.md:379-385).
- **No podman-path regression.** V2b touches `MicroVMRuntime`, the `microvm`
  package, `guestd`, and the proto; `PodmanCLI` and every caller above the
  interface are unchanged, and the existing suite keeps running unchanged
  against the container backend (microvm-runner.md:397-402). **One scoped
  exception, gated on OQ-G:** if Matt rules to widen `isDeliberateKill` for a
  portable exit-signal error (U3b), that one runner-side symbol changes — but
  additively (the `*exec.ExitError` branch stays, guarded by a podman-row
  regression test), so the podman byte-path itself does not regress. Absent
  that ruling, this constraint holds verbatim and the microVM Stop cannot
  distinguish deliberate kill from crash (OQ-G option 2).
- **V7-shaped residue.** Per-session state on disk lands in the
  `<runroot>/microvm/<session>/` layout V7 formalizes
  (microvm-runner.md:589-591), so the crash-recovery task extends rather than
  migrates it.
- **External-reference gate.** This record and every artifact it plans are
  Compass tracked files: no private-monorepo names, hostnames, or tracker
  slugs beyond RIG-NNN.

## Plan

Tasks are ordered by dependency. U1 (proto) gates everything; U2 (guestd
supervisor) and U3 (host exec client layer) both compile against U1's
generated code and proceed in parallel — U3 is testable hermetically against a
fake `GuestControl` server, not against U2. U4 (lifecycle methods) consumes
U2+U3; U5 (the contract suite) consumes U4 and is the milestone's acceptance
gate.

### U1 — `GuestControl` exec surface (proto + codegen)

Grow `proto/compass/v1/guest_control.proto` per (a): `Exec`, `ExecStream`,
`Signal`, `Provision` and their messages, doc-comments carrying the
non-zero-exit and uid-refusal contracts onto the wire surface, and the header's
auth paragraph (`guest_control.proto:40-46`) rewritten to record the (e)
resolution. Regenerate the internal lane.

- **Interfaces:** produces the (a) message set verbatim
  (`ExecRequest{command, uid, workdir, env, stdin, timeout_seconds}`,
  `ExecResponse{stdout, stderr, exit_code}`,
  `ExecStreamRequest{oneof: StartExec|stdin bytes|StdinClose}`,
  `ExecStreamResponse{oneof: ExecStarted{exec_id}|stdout|stderr|ExecExit{exit_code, signal}}`,
  `SignalRequest{exec_id, signal}`, `SignalResponse{}`,
  `ProvisionRequest{nft_script, default_exec_uid, base_env}`,
  `ProvisionResponse{}`) and the regenerated
  `compassv1internalconnect.GuestControlClient`/`GuestControlHandler` in
  `go/internal/gen`. Consumes the V2a seed unchanged
  (`guest_control.proto:47-57`).
- **Test cycle (hardware-independent):** `buf lint` + `buf breaking` green
  (additive change against main); the moon proto gate green; a compile check
  that the V2a `Health` client/handler call sites (`guestd/vsock.go:43`,
  `dial.go:108-116`) build unchanged against the regenerated code.

### U2 — guestd supervisor: gate, exec spawning, Signal, peer-CID

Grow `go/internal/guestd` per (b): the `booting → ready → provisioned` state
machine, the full handler (Exec/ExecStream/Signal/Provision beside Health),
child spawning as uid with empty caps, stdio pumping onto the stream, PID-1
reaping feeding the `exit` frame (including reaping a child whose stream broke
or whose RPC ctx was cancelled — each spawned child is bound to its
request/stream context and SIGKILLed + reaped when that context dies), uid-0
refusal, the peer-CID listener check, the boot-nonce echo in `HealthResponse`
((e)), and the RPC-driven shutdown path: a `Signal("", SIGTERM)` cancels the
supervisor's serving context, guestd SIGTERMs its children and drains, then —
because guestd is guest PID 1 and a PID-1 process *exit* panics the kernel
rather than stopping the VM (`main.go:11-13`) — it ends in an explicit
`reboot(RB_POWER_OFF)` (PID-1-legal) that cloud-hypervisor observes as guest
shutdown and exits on, so the host's `Stop` sees a real VMM exit within
`timeout` instead of always falling through to the hard kill.

- **Interfaces:** produces the full
  `compassv1internalconnect.GuestControlHandler` implementation in `guestd`
  (replacing the Health-only `healthService`, `vsock.go:27-33`), a
  `supervisor` type owning the exec table (`exec_id → child`), and the
  cmdline addition `compass.boot_nonce=<hex>` parsed beside
  `compass.vsock_port` (`guestd/cmdline.go`). Consumes U1's generated
  handler interface; the existing boot steps (`guestd.go:90-98`) unchanged
  ahead of the serve step.
- **Test cycle (hardware-independent):** hermetic handler tests over an
  in-memory listener (the V2a `serveHandshake` split exists for exactly this,
  `vsock.go:35-41`): gate refusal before Provision; uid-0 refusal; default-uid
  resolution; stdin bytes reach the child and never its argv (probe with a
  `sh -c 'cat'` child reading stdin); non-zero child exit → `ExecResponse.
  exit_code`, not a handler error; stream demux ordering (started first, exit
  terminal, exactly once); Signal on a live child kills it and the exit frame
  carries the signal; Signal on an exited exec_id is a no-op success; a broken
  ExecStream / cancelled Exec RPC SIGKILLs and reaps the bound child (kill the
  client conn mid-exec, assert the child dies — no guest-side orphan);
  peer-CID policy unit-tested as a pure accept/refuse function (the AF_VSOCK
  bind itself is integration-proven in U5). Real-child tests spawn ordinary
  host processes — no KVM needed. The `reboot(RB_POWER_OFF)` shutdown path is
  KVM-integration-proven in U4's Stop-grace row (it requires PID 1 in a real
  guest), not here.

### U3 — host exec layer: ExecClient over the V2a dialer

A `GuestExec` layer in `go/internal/runtime/microvm` wrapping U1's client over
the existing `GuestClient` transport (`dial.go:108-128`): one-shot exec,
the stream-to-`StreamingIO` pump, and the killFunc/waitFunc pair the
`ChildHandle` adaptation consumes ((c)).

- **Interfaces:** produces `microvm.GuestExec` with
  `Exec(ctx, ExecCall) (ExecResult, error)` (plain structs mirroring the
  proto, so `go/internal/runtime` types don't leak into this package) and
  `ExecStream(ctx, StreamCall) (*GuestStream, error)` where `GuestStream`
  exposes `Stdin io.WriteCloser`, `Stdout/Stderr io.ReadCloser`,
  `Kill(sig int) error`, and `Wait() ExitStatus`; produces the `runtime`
  package's second `ChildHandle` constructor
  (`newChildHandleFuncs(kill func() error, wait func() error) *ChildHandle` —
  unexported, same file as the type, exported surface unchanged,
  `podman.go:209-248`). Consumes U1's generated client + `DialGuest`/
  `GuestClient` (`dial.go:14-27,108-116`).
- **Test cycle (hardware-independent):** round-trip against a fake
  `GuestControl` server on an in-memory/unix listener: pipe pump correctness
  (interleaved stdout/stderr frames arrive on the right pipes, EOF on exit
  frame), stdin Close → `stdin_close` frame, Kill → Signal RPC issued and
  Wait unblocked by the exit frame, `waitFunc` returns the portable
  exit-signal error (U3b) on a signalled exit so `isDeliberateKill` accepts
  it, ctx cancellation kills the exec and the fake server observes the
  stream break, timeout maps to `TimeoutError`.

### U3b — runner deliberate-kill error taxonomy (OQ-G; gated on Matt's ruling)

Per OQ-G: the frozen `isDeliberateKill` (`agent_exec.go:232-239`) matches only
a concrete `*exec.ExitError`, which a remote-exec `waitFunc` cannot fabricate,
so without this task the microVM Wait error can never be recognized as a
deliberate kill and every microVM-agent teardown surfaces as an error. This
task exists ONLY if Matt rules to amend the "No podman-path regression"
constraint (OQ-G option 1); it is the one plan item gated on that ruling.

- **Interfaces:** produces an exported, backend-portable deliberate-kill error
  in `go/internal/runtime` — `ExitStatusError{Code int; Signal syscall.Signal}`
  (or the interface `interface{ ExitSignal() (syscall.Signal, bool) }`) — and
  WIDENS `isDeliberateKill` to `errors.As` on it FIRST, falling back to the
  existing `*exec.ExitError` branch so the podman byte-path is byte-unchanged.
  U3's `waitFunc` constructs it for a signalled guest exit; `PodmanCLI`'s Wait
  path is left untouched (its `*exec.ExitError` still matches the fallback).
  Consumes nothing beyond the existing runner package.
- **Test cycle (hardware-independent):** unit tests that `isDeliberateKill`
  accepts (i) a real `*exec.ExitError` from a SIGKILLed local child (the
  podman-path regression guard) and (ii) the portable `ExitStatusError` with
  `Signal == SIGKILL`, and rejects a non-signal exit and an unrelated error.
  No KVM, no backend.

### U4 — `MicroVMRuntime` lifecycle methods

Create/Start/Exec/ExecStreaming/Stop/Remove/Exists/MountLabel.

Fill the stubs per (c)/(d): session table + runtime dir, BootConfig assembly,
Launch + Health-poll + nonce check + Provision in Start, spec translation onto
U3, graceful Stop, idempotent Remove, `Exists` from the session table,
`MountLabel` → `""`.

- **Interfaces:** produces the filled methods behind the frozen signatures
  (`microvm.go:71-116` — no signature change) and the extended
  `MicroVMConfig` fields V2b needs beyond the V1 four
  (`microvm.go:25-35`: VMMPath/VirtiofsdPath/KernelImage/RootfsImage): at
  minimum `InitrdImage string` (load-bearing per V2a §(a)), `RunRoot string`
  (the per-session dir root), `DefaultCPUs int`, `DefaultMemoryMB int` —
  flagged OQ-D. Consumes U2 (guest behavior), U3 (`GuestExec`), the V2a
  harness (`Launch`/`Shutdown`/`BootConfig`, `launch.go`, `config.go`), and
  `ContainerSpec`/`ExecSpec`/`StreamingExecSpec` unchanged.
- **Test cycle:** hardware-independent: spec→BootConfig assembly (paths, CID/
  port allocation, mount→FSSharedDir, refusal of inexpressible specs per
  OQ-C), spec→ExecRequest mapping incl. numeric-uid parsing and env merge,
  Create-without-boot state, duplicate-name Create refused (typed error,
  `spec.Name` collision — matching podman's engine), `Exists` answers a NAME
  query from the `spec.Name`-keyed session table (not the random session id),
  Remove idempotence on a never-started session, `MountLabel` empty. KVM-gated:
  Create→Start→Exec(echo)→Stop→Remove end-to-end on real hardware; Start
  failure (bad rootfs path) leaves no processes and no runtime dir; Stop grace:
  a SIGTERM-honoring guest powers off (`reboot(RB_POWER_OFF)`) and the VMM
  exits before the kill escalation — proving the graceful preamble is not dead
  weight that always burns the full timeout.

### U5 — the shared `ContainerRuntime` contract suite

The parent's V2b acceptance (microvm-runner.md:485-490): one table-driven
suite asserting `MicroVMRuntime` and `PodmanCLI` behave identically through
the interface, run against both backends.

- **Interfaces:** produces `go/internal/runtime/contract_test.go`-class
  shared suite parameterized over a `ContainerRuntime` factory; the podman
  rows gate on rootless podman availability (the existing suite's pattern),
  the microVM rows on `microvmtest.Require` (`microvmtest.go:107-128`).
  Consumes U4 and the existing `PodmanCLI`.
- **Test cycle (the suite IS the cycle; microVM rows KVM-gated):** exec exit
  codes (0, non-zero, both as successful calls); stdin feeding
  (`WriteAgentFile`'s script-over-stdin shape end-to-end: file lands 0600
  with the exact body, `agent.go:247-258`); streaming stdio (write stdin,
  read echoed stdout, interleaving); kill/wait (Kill mid-stream → Wait
  returns the deliberate-kill error `isDeliberateKill` accepts — via the
  portable exit-signal error taxonomy OQ-G/U3b lands, asserted for BOTH
  backends so the podman byte-path is proven unregressed,
  `agent_exec.go:232-239`); ctx-cancel terminates a streaming exec AND the
  guest child is reaped (no orphan survives host-side cancel, the U2
  reap-on-broken-stream rule proven end-to-end); uid enforcement (uid-0
  refused on microVM; the podman row asserts its equivalent posture);
  non-numeric User errors; duplicate-name Create refused with the typed error
  podman's engine produces (`spec.Name` collision); Stop/Remove idempotence;
  Stop grace (a SIGTERM-honoring guest powers off before the kill escalation,
  proving `reboot(RB_POWER_OFF)` is observed as VMM exit, not the full-timeout
  fall-through); Exists before/after Remove; `spec.Command`/`CapAdd` ignore
  rows for the microVM backend (asserted, not silent). Plus the
  boot-latency/RSS numbers recorded from the end-to-end rows, feeding Q-budget
  (microvm-runner.md:839-842).

## Tasks

- [ ] U1 — `GuestControl` exec surface: Exec/ExecStream/Signal/Provision
      messages + regenerated internal-go code (buf-breaking-safe, additive)
- [ ] U2 — guestd supervisor: ready→provisioned gate, exec spawning +
      reaping (incl. reap-on-broken-stream), Signal, uid-0 refusal, peer-CID
      check, boot-nonce echo, RPC-driven shutdown via `reboot(RB_POWER_OFF)`
- [ ] U3 — host exec layer: `GuestExec` one-shot + stream-to-StreamingIO
      pump + killFunc/waitFunc ChildHandle over the V2a dialer
- [ ] U3b — runner deliberate-kill error taxonomy (OQ-G, gated on Matt's
      ruling): exported portable exit-signal error + widened `isDeliberateKill`
      with a podman-row regression test
- [ ] U4 — `MicroVMRuntime` lifecycle: Create/Start/Exec/ExecStreaming/
      Stop/Remove/Exists/MountLabel behind the frozen signatures (Exists +
      dup-name Create keyed on `spec.Name`)
- [ ] U5 — shared ContainerRuntime contract suite (podman + microVM rows;
      microVM rows KVM-gated) + Q-budget numbers

## Open Questions

Batched for the pre-freeze ruling; the body designs against each
recommendation.

- **OQ-A (load-bearing) — exec-transport authentication.** The V2a proto
  header requires this to be revisited when Exec/Signal/Provision land
  (`guest_control.proto:43-46`). Options: (1) **no credential protocol** —
  guest-side peer-CID refusal (frozen, microvm-runner.md:158-164) plus
  host-side structural socket identity (the per-session AF_UNIX endpoint in a
  runtime dir only the Runner's uid can reach), hardened with a host-minted
  boot nonce on the kernel cmdline that guestd echoes in `HealthResponse` and
  `Start` verifies before opening the exec gate — an identity *binding*
  (right guest on the right socket), not a secret; (2) a bearer token /
  per-session mTLS over the vsock hop. **Recommend (1)**: any secret guestd
  can present, a guest-root attacker can extract, so (2) buys no boundary the
  VM + socket ownership don't already provide, at real plumbing cost; and a
  cmdline-delivered secret is guest-world-readable (/proc/cmdline), making it
  actively misleading. Full reasoning in Approach (e). If ruled toward (2),
  the nonce slot is where the credential rides — no structural rework.
- **OQ-B (load-bearing, small) — `ExecStream` message naming deviates from
  the frozen parent's sketch.** The parent sketched `ExecStream(stream
  ExecStreamFrame) returns (stream ExecStreamFrame)`
  (microvm-runner.md:480); a shared frame message violates buf's
  RPC_REQUEST/RESPONSE_STANDARD_NAME rules the V2a seed deliberately
  satisfies (`guest_control.proto:27-28`), and the two directions carry
  disjoint payloads. **Recommend `ExecStreamRequest`/`ExecStreamResponse`**
  with direction-specific oneofs (Approach (a)) — a detailing refinement of a
  sketch, not a contradiction of a decision, but it touches the frozen
  parent's text so it is batched for an explicit OK.
- **OQ-C (load-bearing) — mount expressiveness in V2b, and who owns the real
  mount shapes.** podman accepts arbitrary bind mounts
  (`ContainerSpec.Mounts`, `podman.go:100-103`); the microVM backend has
  exactly one virtio-fs share in the V2a harness (the `workspace` tag,
  `config.go:29-31`). The current producer of `ContainerSpec.Mounts` is
  `agentHost.Provision`, which on EVERY launch unconditionally appends two
  mounts — the gateway socket (`host.go:177`) and the read-only agent-config
  tree (`host.go:193`) — plus any operator `SpecDefaults.Mounts` (`spec.go:30`).
  Two consequences the backend must face squarely: (i) that is a multi-mount
  spec TODAY, on every provision, not a V4-only case; (ii) none of those mounts
  is the agent workspace — on podman the checkout and home live inside
  container storage (`Workspace.CheckoutDir`, `spec.go:88-92`), not in `Mounts`,
  so no current caller produces the single read-write share the microVM
  `Create` consumes (`Mount{HostPath, ContainerPath}` → `FSSharedDir`). Options:
  (1) V2b supports exactly ONE read-write workspace share and REFUSES any spec
  carrying a mount it cannot express (a typed error naming the unsupported
  mount); (2) N virtiofsd instances, one per mount, now. **Recommend (1)** —
  refuse-don't-drop keeps the contract honest and avoids per-mount virtiofsd
  supervision V7 would reshape — but it makes the current `agentHost.Provision`
  spec inexpressible on this backend until migrations land, so the plan MUST
  name their owners. The load-bearing sub-fork: which milestone (a
  microVM-aware `SpecBuilder`? a V2c? V4?) (i) delivers agent config to the
  guest without a bind mount, and (ii) produces the workspace-share mount the
  microVM `Create` consumes? The socket mount retires into V4's
  gateway-over-vsock; the config and workspace shares have no named owner today.
  The contract suite pins the refusal; the plan needs the owners.
- **OQ-D (load-bearing, small) — `MicroVMConfig` growth.** V1 froze four
  fields (VMMPath/VirtiofsdPath/KernelImage/RootfsImage, `microvm.go:25-35`);
  V2b needs at minimum `InitrdImage` (the pinned kernel boots via the V2a
  module initramfs — load-bearing, microvm-v2a §(a)), `RunRoot`, and default
  guest sizing (`DefaultCPUs`/`DefaultMemoryMB`, hotplug-grown later per D5).
  Additive config-struct growth mirroring the V2a BootConfig ratification
  (its OQ-E). **Recommend ratifying the U4 field set.**
- **OQ-E (non-load-bearing) — exec output size bounds AND stdin bounds.**
  `ExecResponse` buffers a one-shot exec's stdout/stderr in memory on both
  ends, as podman's `spawnCapture` does host-side today (`podman.go:689-699`).
  guestd caps each captured OUTPUT stream (recommend 8 MiB, generous over the
  largest current one-shot consumer — provisioning scripts and file writes) and
  reports a truncated capture as an explicit error rather than silent
  truncation; any large-output path belongs on `ExecStream`. The INPUT
  direction is unbounded on podman today: `WriteAgentFile` feeds whole
  session-transcript-class bodies over `ExecRequest.stdin` ("as sensitive as
  the aggregate env file", `agent.go:241-246`), so guestd's connect-go server
  sets an explicit `ReadMaxBytes` on the request message sized to the largest
  legitimate agent-file body (recommend 16 MiB) rather than letting the default
  4 MiB connect cap reject a large write with an opaque error. Flagged because
  both cap VALUES are judgment calls; the mechanisms are not.
- **OQ-F (non-load-bearing) — CID/port allocation scheme.** One VMM per
  session means guest CID uniqueness only matters per-host for observability;
  the hybrid transport addresses by socket path, not CID (`dial.go:17-20`).
  Recommend: fixed guest CID 3 for every VM, fixed guestd port, uniqueness
  carried entirely by the per-session socket paths. Simplest thing that can
  work; nothing routes on CID.
- **OQ-G (load-bearing, needs a scope ruling) — the deliberate-kill error
  taxonomy crosses the interface.** The contract requires
  `ChildHandle.Wait`-after-SIGKILL to yield an error `isDeliberateKill`
  accepts, so `AgentStream.Stop` reports a deliberate teardown as success and
  any other exit as a failure (`agent_exec.go:200-207`). But `isDeliberateKill`
  matches a CONCRETE `*exec.ExitError` and reads `exitErr.Sys().(syscall.
  WaitStatus)` (`agent_exec.go:232-239`). `*exec.ExitError` embeds
  `*os.ProcessState`, which has unexported fields and no public constructor: a
  `waitFunc` reporting a REMOTE guest child's exit cannot fabricate one, so the
  microVM Wait error can NEVER satisfy `isDeliberateKill`, and every deliberate
  microVM-agent teardown would surface as an error. The record's own U5 kill/
  wait row (U5, "Kill mid-stream → Wait returns the deliberate-kill error
  `isDeliberateKill` accepts") cannot pass as long as `isDeliberateKill` stays
  `*exec.ExitError`-only. Fixing it requires touching a symbol ABOVE the
  interface — which the "No podman-path regression" Global Constraint currently
  scopes out ("`PodmanCLI` and every caller above the interface are
  unchanged"). Options: (1) introduce an exported, backend-portable
  deliberate-kill error the runner recognizes —
  `runtime.ExitStatusError{Code int; Signal syscall.Signal}` (or an interface
  `interface{ ExitSignal() (syscall.Signal, bool) }`) — and WIDEN
  `isDeliberateKill` to `errors.As` on it FIRST, falling back to the existing
  `*exec.ExitError` branch so the podman byte-path stays untouched; both
  backends' Wait errors satisfy it (podman by wrapping/extending; microVM by
  constructing it in `waitFunc`). ~10 host-side lines, guarded by a podman-row
  regression test. (2) leave `isDeliberateKill` alone and accept that microVM
  Stop cannot distinguish deliberate kill from crash — rejected: it breaks the
  crash-vs-stop signal D4 depends on (`podman.go:230-233`). **Recommend (1),
  but it AMENDS the no-caller-change constraint**, so it needs Matt's explicit
  OK and its own task. This is the one genuine scope fork: U3's `waitFunc`, the
  U5 kill/wait row, and the Global Constraint text all depend on the ruling.
  The record below is written against (1) (a new U3b task owns the runner
  change); if Matt rules (2) or a different error shape, U3b and the U5 row
  change accordingly.

### Ledger assessment

`Ledger-impact: none` recommended. This record details forks the frozen
parent already framed (its §(b) exec surface, §(c) gate + uid posture, §(f)
teardown, D4's supervisor split) and resolves them within the parent's
decisions. Two resolutions are genuinely new cross-record calls: OQ-A's
no-credential-plus-boot-nonce auth resolution of the proto header's flagged
question, and OQ-G's shared deliberate-kill error taxonomy on the
`ContainerRuntime` surface. Both are candidates for DL rows if Matt wants them
citable outside this record's lineage; both bind surfaces this record and its
parent own (the GuestControl transport; the runtime error contract), so the
recommendation is to keep them here. The caller owns the ledger delta at PR
time; if Matt promotes either, this PR adds the DL row and the `Ledger-impact:`
trailer flips accordingly.
