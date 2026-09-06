# Apple `container` as an embedded-macOS runner backend

Status: Active (Matt, 2026-09-05)
Linear: RIG-3238 (design)

Investigation + design record for RIG-3238: whether Apple `container`
(github.com/apple/container) becomes a supported backend behind the frozen
`ContainerRuntime`/`SelectBackend` seam for the Compass native app's embedded
macOS front door, and if so, the adoption sequencing. This record carries
Matt's RIG-3246 ruling plus an adoption plan whose BUILD (not direction) is
gated on the T-1 spike; it does not implement the backend.

## Problem / Intent

The embedded-macOS front door is expected to be Compass's most-used on-ramp:
brew install the app, launch, sign in, run agents locally — the same posture
as running OMP or any agentic dev environment. Today the designed macOS
runner substrate is podman-machine: ONE shared Linux VM hosting every agent
container (`docs/designs/ui/compass-native-embedded-revival/design.md:368`:
"Linux podman is native; on macOS the podman CLI drives a Linux VM, and a
fresh Mac has no machine"). That shape carries two structural costs on macOS:

1. **A weaker per-agent boundary than we can now get.** All agents share one
   VM kernel; isolation between agents inside the machine is rootless-podman
   isolation, not a hardware boundary. The microVM backend that supplies the
   hardware boundary elsewhere needs `/dev/kvm` and is off the table on Macs
   (embedded-revival funnel table, `design.md:66`: "the primary embedded
   target is macOS, where a microVM is not an option (no host `/dev/kvm`)").
2. **A known AF_UNIX socket-mount hazard across the host↔VM boundary.** The
   runner's per-container agent sockets are AF_UNIX bind-mounts whose source
   must be local to the container host
   (`go/internal/runner/gateway/socket.go:11-12`: "The listener is created at
   Provision (before `podman run`, so the bind-mount source exists)"), and
   the local-dev record already flags that "a macOS-side socket cannot cross
   the VM boundary as a bind-mount source"
   (`docs/designs/infra/ci/compass-local-dev/design.md:195-199`); the
   embedded-revival record carries the same hazard as its load-bearing OQ-7
   ("AF_UNIX sockets do not work across a virtiofs/VM boundary",
   `compass-native-embedded-revival/design.md:922-923`).

Apple `container` (github.com/apple/container, Apache-2.0, Swift) runs each
Linux container in its OWN lightweight VM over Virtualization.framework —
"it runs a lightweight VM for each container that you create … each container
has the isolation properties of a full VM"
(<https://github.com/apple/container/blob/main/docs/technical-overview.md>).
That is a per-agent hardware boundary on macOS with NO nested virtualization
(the VMs are first-level Virtualization.framework guests), sidestepping
exactly the microVM-on-Mac problem, with sub-second-class boot ("boot times
that are comparable to containers running in a shared VM", same doc). It
reached 1.0.0 on 2026-06-09 and is at 1.3.1 as of this writing
(<https://github.com/apple/container/releases>), Apple-silicon-only, supported
on macOS 26 (<https://github.com/apple/container#requirements>).

The embedded-revival record weighed this option pre-1.0 and deferred it
descriptively as its OQ-10 ("apple/container as a macOS backend?" —
recommendation "track apple/container as a post-1.0 alternative macOS
backend — revisit when it reaches 1.0",
`compass-native-embedded-revival/design.md:980-998`). 1.0 has shipped; this
record is that revisit. The question this record opened with — does Apple
`container` become a supported backend behind `SelectBackend`, and if so is it
the macOS engine or a staged bet behind podman-machine — is now RULED in
Approach: apple-container IS the macOS engine; only the flip timing and the
vsock hardware leg remain gated.

## Approach

**Ruling (Matt, RIG-3246, 2026-09-05): Apple `container` is THE macOS
embedded runtime — a new `AppleContainerCLI` backend behind the frozen
`SelectBackend` seam, the agent data path mirroring the microVM over vsock,
the runner staying host-side (darwin-native) driving it, and postgres +
collector moving onto apple-container too, so macOS carries NO podman.
podman-machine sunsets on macOS and Intel/macOS ≤ 15 is out of scope.** The
direction is committed; what stays gated is the ONE load-bearing hardware
unknown the T-1 spike owns — whether Virtualization.framework surfaces a
usable guest↔host vsock the guestd-style forwarder ports onto (OQ-11/12 fold
below). A green spike is build-then-flip; a red spike on the vsock leg
returns the transport question (not the direction) to Matt.

Matt's rulings, verbatim-anchored: OQ-9 "A, we have a mac mini box on the
Woodpecker CI … you should have ssh access to the mattmini to start testing"
(commit the self-hosted Apple-silicon host — done); OQ-10 "can sunset, Apple
has basically ended all support for Intel Macs … we don't need to support";
OQ-11/12 "Can mirror the microVM right? that uses vsock? … Keeping runner
host side makes sense — that's what we do for microVMs anyway too, so putting
the runner in a VM itself would be a departure from the other backends";
OQ-13 "If we do this, then all would be on apple container, no podman."

Sequencing (Matt's "stick with podman for now, apple-container later when we
have more resources", folded to spike-now / build-later): run the T-1 spike
NOW on the committed mac mini — it is cheap, one-time, and resolves the
vsock + uid + exec unknowns with real evidence — but defer the T-2..T-4 backend
build and the macOS-default flip until the spike proves the vsock leg and
resources allow. macOS embedded ships on podman-machine in the interim; the
spike de-risks the flip before any backend code lands. A spike falsification
of the uid round-trip (OQ-1), a workable vsock/socket transport (OQ-2/OQ-11),
or streaming-exec fidelity (OQ-4) returns that specific question to Matt with
the finding — but the DIRECTION (apple-container is the macOS engine) is his
ruling, not a spike pass/fail gate.

Reasoning, in order of weight:

1. **Strictly stronger isolation where it matters most.** The trust-model
   split (embedded-revival §Topology, DL-319) accepts rootless podman as a
   correct boundary for the operator's own code — but "correct" is a floor,
   not a ceiling. A per-agent VM boundary at podman-class boot cost is a
   straight upgrade on the most-used on-ramp, and it is the SAME isolation
   thesis the microVM backend carries on Linux ("Each container has the
   isolation properties of a full VM", technical-overview.md) — Apple
   `container` is the macOS-native expression of the microVM direction, not a
   third philosophy.
2. **No nested virt, no machine.** Virtualization.framework VMs are
   first-level guests; there is no `podman machine init` (multi-minute,
   multi-GB VM image download, embedded-revival OQ-2), no shared-VM resource
   sizing, no machine lifecycle for the app to ensure. `container system
   start` launches a launchd agent (`container-apiserver`,
   technical-overview.md) and containers boot on demand.
   Two notes, both now RULED (Resolved decisions). (i) Removing the machine
   also removes the Linux VM the runner runs IN on macOS today — Matt ruled
   the runner stays HOST-SIDE (darwin-native) driving apple-container, "that's
   what we do for microVMs anyway too", so this is the intended topology, not
   an unverified premise (OQ-12 resolved). (ii) Matt ruled postgres +
   collector move onto apple-container too ("all would be on apple container,
   no podman", OQ-13 resolved), so the "no machine / no podman" win covers the
   WHOLE macOS stack, not only the agent containers — the DL-260 podman shell
   for postgres is swapped for apple-container on macOS (T-2 scope).
3. **The seam was built for this.** `ContainerRuntime` is a frozen interface
   (`go/internal/runtime/podman.go:343-348`: "ContainerRuntime is the
   container engine seam … An interface so the Runner can hold a
   ContainerRuntime and tests can substitute a fake") and `SelectBackend`
   is an explicit switch (`go/internal/runtime/microvm.go:117-126`) whose
   error copy already anticipates growth ("accepted values are \"podman\"
   (default) and \"microvm\"", `microvm.go:124`). A third case + impl type
   is the designed extension path. Apple `container` is a DISTINCT non-podman
   CLI (its own argv grammar, its own `container-apiserver` service), so it
   is a new `ContainerRuntime` implementation — NOT a
   `PodmanCLI.WithProgram` swap, which only substitutes a podman-compatible
   binary path (`podman.go:433-435`: "WithProgram uses an explicit engine
   binary (e.g. an absolute path, or `docker` in a pinch)").

Why spike-first, then flip (not default-the-instant-it-builds):

- **One load-bearing hardware unknown gates the flip.** The direction is
  ruled, but whether Virtualization.framework surfaces a usable guest↔host
  vsock the guestd-style forwarder ports onto is unverified from docs alone
  (OQ-11/OQ-2). The T-1 spike proves it on the committed mac mini before any
  backend code lands; a red vsock leg returns the transport question (not the
  direction) to Matt.
- **Spike-now, build-later resourcing (Matt's ruling).** "Stick with podman
  for now, apple-container later when we have more resources" — the cheap,
  high-information move is to run the one-time spike now and defer the
  T-2..T-4 build + the macOS-default flip until the spike proves it and
  resources allow. macOS embedded ships on podman-machine in the interim.
- **Intel / macOS ≤ 15 sunset (OQ-10 ruled).** Apple silicon + macOS 26 only;
  "Apple has basically ended all support for Intel Macs … we don't need to
  support" (Matt). podman-machine is NOT a permanent second macOS arm — it is
  the interim substrate until apple-container flips, and sunsets on macOS
  after. There is no permanent two-backend macOS matrix.
- **The embedded-revival plan is in flight.** Its T-6 podman-machine spike and
  provisioning path are that record's committed interim v1
  (`compass-native-embedded-revival/design.md:661-680`); this record ADDS the
  apple-container backend and the flip is its own later gate (T-5) once the
  spike proves the vsock leg.

### Where the backend plugs in

- **`SelectBackend`** grows a case: `"apple-container"` →
  `NewAppleContainerCLI(cfg.AppleContainer)`, beside the existing
  `case "", "podman"` / `case "microvm"` arms (`microvm.go:118-125`), with
  the error copy extended to name three accepted values. `BackendConfig`
  (`microvm.go:63-69`) gains an `AppleContainer AppleContainerConfig` field
  mirroring how `MicroVM MicroVMConfig` rides beside `Backend`.
- **The impl type** is `AppleContainerCLI`, a subprocess-driving
  `ContainerRuntime` shaped like `PodmanCLI` (program + timeout,
  `podman.go:421-425`), speaking the `container` CLI: `create`/`start`/
  `exec`/`stop`/`rm`/`inspect` exist with familiar semantics
  (<https://github.com/apple/container/blob/main/docs/command-reference.md>).
  Argv builders split from process-spawning exactly as `createArgs` is split
  today ("Split out so the argv assembly is unit-testable without spawning
  podman", `podman.go:455-457`).
- **Gateway socket transport: vsock, mirroring the microVM (Matt ruled OQ-11).**
  The per-session agent↔host gateway socket rides guest↔host **vsock**, the
  same pattern the microVM backend already proves in-tree: guestd binds the
  fixed AF_UNIX rendezvous and splices every accepted conn to an AF_VSOCK dial
  of the host gateway (`go/internal/guestd/gateway_proxy.go:29-33`, `:205-213`;
  `mdlayher/vsock` is already a dep, `go/go.mod:26`). The load-bearing spike
  unknown is whether Virtualization.framework's `VZVirtioSocketDevice` vsock is
  reachable through Apple's `container` CLI so the guestd-style forwarder ports
  onto it (T-1(b)); a raw AF_UNIX bind-mount is NOT assumed to work through
  virtiofs. Because the transport is vsock (not a raw mount), the runner stays
  HOST-SIDE (darwin-native) and dials the guest over vsock — Matt's OQ-12
  ruling, symmetric with the microVM backend.
- **Egress arming model:** host-side exec, the podman model. Apple
  `container` supports `--cap-add NET_ADMIN`
  (<https://github.com/apple/container/blob/main/docs/runtime-configuration.md>)
  and each guest has its own network namespace inside its own VM, so
  `AgentRuntime.armEgress`'s nft exec path (`go/internal/runtime/agent.go:
  319-328`) runs unchanged. `AppleContainerCLI` therefore does NOT implement
  the `inGuestEgressArmer` marker — `provision` probes for it and arms
  host-side when absent (`agent.go:308-312`). Because the gateway rides vsock
  (a virtio device, not an in-namespace IP hop), the guest nft default-deny
  ruleset does NOT need a gateway carve-out for the socket — the vsock channel
  is out-of-band of the guest's netfilter, dissolving the OQ-2/OQ-3 coupling
  the raw-mount branch carried. The spike (T-1(c)) still confirms nft exists
  in the guest and NET_ADMIN is absent from the default cap set
  (runtime-configuration.md), arming via the PRODUCTION identity — the image's
  default user (uid 1000) with CAP_NET_ADMIN, no `--user`, per
  `agent.go:321-325` — and re-checks default-deny leaves DNS + the vsock
  gateway reachable.
- **MountLabel** returns `""` — no SELinux inside the guest images by
  default; the microVM backend already established that an empty label is
  the correct no-relabel answer (`go/internal/runtime/
  microvm_lifecycle.go:718-720` returns `"", nil`; its test pins "the empty
  answer is correct", `microvm_lifecycle_test.go:308-312`).
- **Resize** returns `ErrResizeNotImplemented` like `PodmanCLI` does today
  (`podman.go:387-396`: the verb is "additively reserved" and "no caller
  invokes it yet"). Apple `container` sets `--cpus`/`--memory` at create
  (command-reference.md) but has no documented live-update verb; the spike
  records whether one exists.

### Sequencing

Spike first (T-1), then the backend implementation (T-2 — including moving
postgres + collector onto apple-container), contract-suite coverage (T-3),
app/preflight wiring (T-4), and a separate default-flip gate (T-5). The spike
is the only task that runs today; the direction is ruled, so T-2 onward are
gated only on the spike proving the load-bearing vsock/uid/exec unknowns
(OQ-1/OQ-2/OQ-4) on the committed mac mini. A spike falsification of one of
those returns that specific transport/mechanism question to Matt with the
finding before implementation lands — the direction (apple-container is the
macOS engine, runner host-side) is Matt's ruling, not a spike gate.

## Alternatives considered

### Stay podman-machine-only (rejected)

The zero-new-work option: embedded-revival's §A5 ships podman-machine and
nothing changes. Rejected as the END state, accepted as the v1 state. It
leaves the most-used on-ramp on the weakest isolation shape in the fleet
(shared-VM rootless podman) while a per-agent hardware boundary is available
at comparable cost, and it leaves the OQ-7 socket-transport hazard
(`compass-native-embedded-revival/design.md:909-926`) as a permanent
workaround rather than dissolving it: the ruled vsock transport (OQ-11) takes
the agent socket off the virtiofs/AF_UNIX path entirely — each Apple-container
VM reaches the host over guest↔host vsock, out-of-band of the mount boundary
the hazard lives on. It also forfeits the "no machine init" first-run win:
podman-machine's first launch
is a multi-minute multi-GB VM download with resource-floor tuning
(embedded-revival §A5), which Apple `container` simply does not have.

### Apple `container` as the immediate macOS default (rejected)

Flip `SelectBackend`'s darwin default to the new backend as soon as it
exists. Rejected: (a) the platform floor excludes Intel Macs and macOS ≤ 15,
so podman-machine ships as the INTERIM darwin substrate (it sunsets after the
flip — OQ-10 ruled — not a permanent per-host arm); (b) the load-bearing
hardware unknown (vsock through the `container` CLI, OQ-2/OQ-11) plus the uid
and streaming-exec unknowns (OQ-1/OQ-4) are unverified, and a front-door
default is exactly the place an unverified assumption does the most damage
(the embedded-revival record's own OQ-8 lesson: a silent deep failure on the
easy front door "defeats the record's easy-front-door thesis"); (c) it would
churn embedded-revival's in-flight T-6 plan. The default flip is this record's
T-5, its own gated decision with spike + soak evidence behind it — the
direction (apple-container is the macOS engine) is ruled, only the flip TIMING
is gated.

### Apple `container` behind a config flag, indefinitely (rejected)

Ship the backend, never make it the default — a permanent expert option.
Rejected: it takes on the full maintenance cost of a third backend (contract
suite, CI lane, version-floor policing) while delivering the isolation
upgrade only to users who know to ask for it — the inverted priority for a
front door whose whole thesis is zero-config. If the spike and soak validate
the backend, defaulting it on capable hosts is where the value is; if they
don't, the backend should not ship at all. The flag exists only as the
STAGING mechanism between T-2 and T-5, not as an end state.

### Wrap it via `PodmanCLI.WithProgram` (rejected)

Point the existing podman CLI runtime at the `container` binary. Rejected on
grounds of fact: `WithProgram` swaps a podman-COMPATIBLE binary
(`podman.go:433-435` — "an explicit engine binary (e.g. an absolute path, or
`docker` in a pinch)"), and Apple `container` is not one. Its argv grammar
differs where the runtime depends on podman specifics: no `--userns=keep-id`
(OQ-1), different `inspect` output shape (no `{{.MountLabel}}` — the format
string pinned at `podman.go:838-840` targets podman's inspect JSON),
different create/exec flag surfaces. A new impl type is smaller than a
compatibility shim inside `PodmanCLI`.

### Adopt the Containerization Swift package directly (rejected)

Drive Virtualization.framework through apple/containerization instead of the
`container` CLI. Rejected: the runner is Go; embedding a Swift package means
a bridge process — which is what the `container` CLI already is, maintained
by Apple, with a stability contract ("stability … is only guaranteed within
patch versions" pre-1.0; post-1.0 the CLI is the supported surface,
apple/container README §Project Status). Every existing backend drives a
subprocess (`PodmanCLI`) or a local control plane (`MicroVMRuntime`); a CLI
backend follows the established pattern and keeps the ONE-version rule
trivially satisfiable (a version-floor probe on one binary, like
`VerifyUsernsRemapSupport`, `podman.go:497-504`).

## Global Constraints

- **Platform floor:** Apple silicon only; macOS 26 for support (the tool
  "is supported on macOS 26"; macOS 15 runs with documented degradations —
  container-to-container networking absent, single default network —
  <https://github.com/apple/container/blob/main/docs/technical-overview.md>
  §macOS 15 limitations). This record's floor is macOS 26 + Apple silicon.
  Intel Macs and macOS ≤ 15 are OUT OF SCOPE (Matt ruled OQ-10 — "Apple has
  basically ended all support for Intel Macs"); they run the interim
  podman-machine substrate until apple-container flips, never a permanent
  second arm.
- **Version floor:** `container` ≥ 1.0.0 (1.0.0 removed the v0 XPC APIs and
  froze the config surface — "Removed compatibility with application major
  version 0 XPC APIs",
  <https://github.com/apple/container/releases/tag/1.0.0>). The backend
  verifies the floor at startup with a `container --version` probe, the
  `VerifyUsernsRemapSupport` pattern (`podman.go:497-518`): a legible
  refusal naming required and found versions, never a deep create failure.
- **ONE version rule:** exactly one supported `container` version floor at a
  time; raising it is a deliberate change with a changelog entry, not a
  drive-by.
- **The substrate invariant holds:** no daemon-as-root, no rootful fallback
  (`podman.go:24`: "no daemon, no root, no rootful fallback").
  `container-apiserver` is a per-user launchd agent, not a root daemon
  (technical-overview.md: "a launch agent that launches when you run the
  `container system start` command") — inside the invariant. The installer
  requiring admin once to place files under /usr/local is an install-time
  cost, not a runtime posture.
- **The `ContainerRuntime` interface stays frozen.** The new backend
  implements all nine `ContainerRuntime` verbs, Resize included as the
  additively-reserved one (`podman.go:348-397`), and adds NO verbs. Any
  backend-specific need rides the off-interface marker pattern
  (`podman.go:399-406`) or the config struct, never an interface change.
- **Public-repo boundary:** this record describes the OSS product's own
  embedded mode only; no managed end-state or rollout rides here.
- **Interim podman-machine, not a permanent second backend.** Until the T-5
  flip, `SelectBackend`'s darwin resolution keeps podman-machine as the
  default and `"apple-container"` is opt-in config — the spike-now/build-later
  sequencing (Approach). After the flip, apple-container is the macOS default
  and podman-machine SUNSETS on macOS (Matt ruled OQ-10) — there is NO
  permanent two-backend macOS matrix. The third-backend maintenance cost
  (contract suite, darwin lane, version-floor policing) is carried only for the
  interim + the flip, converging on one macOS engine, not held forever.
- **ID allocation + freeze order (ledger-collision guard).** This record lands
  as **DL-330**. At freeze, main's ledger tail was DL-328 (DL-325 runner
  trust-split, DL-326 session-volume clone, DL-327 token-subject, DL-328
  gateway-creds encryption all landed) and the in-flight sibling stack-supervision
  record (#872, RIG-3239) claims DL-329, so DL-330 is the next free number — the
  record's originally-claimed DL-326 was taken by the session-volume-clone row,
  and the driver took the next free id per this guard. DL-330's Decision cell
  cites the runner trust-model split by name+issue (RIG-3070 / DL-325, now
  landed), so the immutable cell cannot be falsified by merge order. The driver
  MUST re-grep main's then-current ledger tail immediately before landing and
  take the next free id if DL-330 is taken.

## Plan

Five tasks. T-1 is the gating spike; T-2..T-4 are the adoption sequence; T-5
is the default-flip decision, deliberately separated so shipping the backend
and defaulting it are two rulings with their own evidence. Every task is a
PR-sized slice with its own test cycle. **A hardware prerequisite runs under
the whole plan: T-1's spike AND T-3's live contract suite both CREATE
Virtualization.framework VMs, which a GitHub-hosted arm64 macOS runner cannot
do (it is itself a VM — nested virt is unsupported, see T-3). So the plan
needs a committed Apple-silicon/macOS-26 execution host (a physical box or a
self-hosted runner); until one is named, T-1 is not "runnable now" in CI and
the live legs of T-1/T-3 have no substrate. The hardware is now COMMITTED —
Matt ruled OQ-9 A (the mac mini on Woodpecker, ssh access provisioned).**

### T-1 — Spike: prove the six unknowns on real hardware (gates all)

- **Do:** on an Apple-silicon macOS 26 box with `container` ≥ 1.0.0, script
  and record:
  (a) **uid mapping** (OQ-1): run the compass-agent image with
  `--uid 1000 --gid 1000`, bind-mount a host dir, write a file from the
  container, `stat` it on the host — record the host-side owner. Determine
  whether guest-written files land as the invoking macOS user (the property
  `--userns=keep-id:uid=,gid=` supplies on podman, `podman.go:25-27`) or as
  a fixed/root uid, and whether `/nix` + `$HOME` baked at uid 1000 are
  usable.
  (b) **vsock gateway channel** (OQ-2): the ruled transport is guest↔host
  vsock (OQ-11), so the load-bearing probe is whether
  Virtualization.framework's `VZVirtioSocketDevice` vsock is reachable through
  Apple's `container` CLI. Run the guestd-style unix→vsock forwarder shape
  (`go/internal/guestd/gateway_proxy.go:29-33`, `:205-213`) against the
  per-session gateway-socket contract (`go/internal/runner/gateway/socket.go:8-13`)
  and record connect + round-trip OVER VSOCK. A raw AF_UNIX virtiofs
  bind-mount is NOT assumed to work; probe it only as an explicitly-secondary
  datapoint, not the gate.
  (c) **egress arming** (OQ-3): `container run --cap-add NET_ADMIN` an image
  with nft, run `EgressPolicy.NftScript()`'s grammar as the PRODUCTION arming
  identity — the image's default user (uid 1000) with CAP_NET_ADMIN, NO
  `--user` (mirroring `agent.go:321-325`, where arming execs as the default
  user, not root) — and verify default-deny + allowlist behavior and that a
  capability-less user cannot edit the ruleset (the integrity model,
  `go/internal/runtime/egress.go:6-10`). Re-run default-deny WITH the vsock
  gateway channel from (b): confirm the vsock hop stays reachable (it is
  out-of-band of the guest's netfilter — a virtio device, not an in-namespace
  IP hop — so the ruleset needs no gateway carve-out, per §Where the backend
  plugs in) and that DNS stays reachable. This re-confirms the OQ-2/OQ-3
  coupling is dissolved by the vsock transport, not a fork to resolve.
  (d) **streaming exec** (OQ-4): `container exec -i` a long-lived process,
  verify live stdout/stderr streaming, stdin delivery, kill semantics, and
  a distinguishable exit code on signal — the `ChildHandle` kill/wait
  contract (`podman.go:217-233`, `253-273`).
  (e) **timings + stability notes** (OQ-5): container create→running wall
  time cold and warm, memory per idle container VM, any CLI output-format
  surprises vs the command reference.
  (f) **runner-on-darwin** (OQ-12): does `compass-runner` build and run
  NATIVELY on macOS 26 driving apple-container — with the gateway socket on
  the darwin `sun_path` budget (`socket.go:138-139`) and no podman
  host-capability preflight (`main.go:89-100`)? Removing podman-machine
  removes the Linux VM the runner runs IN today (`compass-local-dev:199,204`
  ruled the runner runs INSIDE the VM, not natively); this leg confirms the
  ruled host-side topology (OQ-12) on real hardware, not a detail. Record
  build + run verdict.
  Record findings in this directory as `spike-findings.md`.
- **Interfaces:** consumes the `container` CLI (`run`/`create`/`exec`/
  `stop`/`rm`/`inspect`, command-reference.md) and the compass-agent GHCR
  image. Produces `spike-findings.md` with a pass/fail verdict per OQ and
  measured numbers. No repo code changes.
- **Test cycle:** the spike script IS the test; it must run green end to end
  on the target hardware and its transcript lands in the findings doc.

### T-2 — `AppleContainerCLI` backend + `SelectBackend` case

- **Do:** add `go/internal/runtime/applecontainer.go` (+ `_darwin` split
  only if a unix-only dependency forces it; the CLI driver itself is
  plain Go): type `AppleContainerCLI{program string, timeout time.Duration}`
  mirroring `PodmanCLI` (`podman.go:421-425`), argv builders split from
  spawning (the `createArgs` discipline, `podman.go:455-462`), implementing
  all nine `ContainerRuntime` verbs (`podman.go:348-397`): Create/Start/
  Exec/ExecStreaming/Stop/Remove/Exists/MountLabel/Resize. Additionally the
  two OFF-interface podman surfaces the embedded stack drives on macOS. (1)
  The image adapter's `imageCLI` requires `ImageExists` + `Pull`
  (`go/internal/stack/adapters/image.go:27-30`), which today only `PodmanCLI`
  supplies (`podman.go:681-684` Pull, `:713` ImageExists) — reachable in
  production only through `ImageEnsurer`'s podman-hardwired constructor
  (`image.go:44-45`), the `imageCLI` interface being its test seam. T-2
  parameterizes this so the embedded stack's image-ensure path works against
  this backend too.
  (2) The stack's postgres and collector containers run a SEPARATE hard-coded
  podman shell (`postgres_container.go:246-259` + `:84`,
  `collector_container.go:62`), deliberately independent of `internal/runtime`.
  Per Matt's OQ-13 ruling ("all would be on apple container, no podman"), T-2
  ports these onto apple-container on macOS too — the podman shell and the
  podman-pinned `ImageEnsurer` constructor (`image.go:44-45`) gain an
  apple-container path so a macOS embedded host needs NO container engine but
  apple-container (DL-260's postgres-is-a-container shape holds; only the engine
  driving it changes on macOS). MountLabel returns
  `("", nil)` (the microVM precedent, `microvm_lifecycle.go:718-720`);
  Resize returns `ErrResizeNotImplemented` (the PodmanCLI posture,
  `podman.go:394-395`). Uid handling per T-1's (a) findings. NO
  `inGuestEgressArmer` marker — host-side arming per `agent.go:308-312`. Add
  `AppleContainerConfig` to `BackendConfig` (`microvm.go:63-69`) and the
  `case "apple-container"` arm to `SelectBackend` (`microvm.go:118-125`),
  extending the unknown-backend error copy to name all three values. Add
  `VerifyAppleContainerSupport(ctx)` — version-floor probe via
  `container --version`, the `VerifyUsernsRemapSupport` shape
  (`podman.go:497-518`).
- **Interfaces:** produces `NewAppleContainerCLI(cfg AppleContainerConfig)
  *AppleContainerCLI` satisfying `runtime.ContainerRuntime`
  (`podman.go:348-397`), `func (a *AppleContainerCLI)
  VerifyAppleContainerSupport(ctx context.Context) error`, and the widened
  `SelectBackend(cfg BackendConfig) (ContainerRuntime, error)`. Consumes
  T-1's findings for argv specifics.
- **Test cycle:** unit tests over the argv builders (no binary spawned —
  the `TestCreateArgsRemapsUserns` pattern, `podman_test.go:99-104`);
  `SelectBackend` table test for the new case + error copy; version-parse
  and floor-comparison tests mirroring the podman floor tests
  (`podman_test.go:107-111`). Module gates green.

### T-3 — Contract-suite + integration coverage

- **Do:** extend the backend contract suite (`go/internal/runtime/
  contract_suite_test.go`, the `backendCaps`-parameterized rows, e.g.
  `rowMountLabel` at `:301`) with an Apple-container backend entry, gated on
  `container` being usable (the `podmanUsable()` skip pattern,
  `userns_remap_test.go:80-82`). Port the three-case userns/bind-mount
  round-trip suite (`userns_remap_test.go:12-16`) to whatever uid mechanism
  T-1 established. Egress integrity test mirroring
  `egress_integrity_podman_test.go:83-87`. **CI substrate: the live suite
  needs a real Virtualization.framework host and therefore CANNOT run on a
  GitHub-hosted macOS runner** — hosted arm64 macOS runners are themselves
  VMs and do not support the nested virtualization `container` requires
  (GitHub confirmed this unresolved for macOS-15/26 arm64 runners,
  actions/runner-images#13505, closed 2026-01-08: "nothing we can do yet …
  due to the limitation of Apple's Virtualization Framework"). DL-263's
  existing darwin lane is a `macos-14` COMPILE+BUNDLE sweep, not a
  live-daemon lane, and `macos-14` is below this record's macOS-26 floor
  anyway. So T-3 wires: (1) a COMPILE gate for the backend on the existing
  darwin lane (it builds; it does not exercise `container`), and (2) the live
  contract suite gated behind `container` being usable (the `podmanUsable()`
  skip pattern, `userns_remap_test.go:80-82`) — green only where a committed
  Apple-silicon/macOS-26 host or self-hosted runner exists (OQ-9), and a
  legible skip everywhere else, including hosted CI.
- **Interfaces:** consumes T-2's backend + the contract suite's
  `backendCaps` row model. Produces the suite entry, the ported remap and
  egress tests, the darwin compile gate, and the live-suite skip-guard.
- **Test cycle:** the suite itself, green on a committed Virtualization host
  (OQ-9) and skipping legibly on hosted CI and non-darwin.

### T-4 — Embedded-app wiring: backend selection + preflight

- **Do:** in the embedded pipeline (embedded-revival T-2's revived
  `go/cmd/compass-app` + `go/internal/preflight`), add darwin backend
  resolution: config-selected backend threads through to the runner's
  `BackendConfig.Backend`; preflight gains an `apple-container` probe
  (binary present + `container system start` state + version floor via
  T-2's verify) as the FATAL check when that backend is selected,
  mirroring the podman-version delta-4 check (embedded-revival §A3). The
  `container system start`-not-running case is an ENSURE step (start it,
  re-probe), the §A5 machine-ensure pattern, not a hard refusal.
  T-4 wires the runner as a first-class darwin-native host process driving
  apple-container over vsock — Matt's OQ-12 ruling (runner stays host-side).
  This is the ruled topology, not a conditional that re-opens on a spike
  finding; T-1(f) confirms `compass-runner` builds + runs on macOS 26 (it is
  already `//go:build unix`, `compass-runner/main.go:1`), and the vsock gateway
  channel (T-1(b)) is how the host-side runner reaches the guest. The scope is
  the darwin-native runner process + config-threading + the preflight probe.
- **Interfaces:** consumes T-2's `VerifyAppleContainerSupport` and the
  embedded-revival preflight `Deps` seam. Produces the backend-selection
  config key (documented in the self-host doc) and the preflight adapter.
  **BLOCKED ON the embedded-revival stack (its T-2 revived `compass-app` +
  the preflight `Deps` seam) merging — that stack is at its review gate as of
  this record, so the `Deps` seam shape is not yet frozen. If review reshapes
  that seam, T-4's Interfaces re-syncs to the merged shape before execution;
  T-4 does NOT start against the unmerged shape.**
- **Test cycle:** unit tests over the injected probe seam (absent binary /
  stopped service / below-floor / healthy); manual smoke on a macOS 26 box:
  embedded launch with the backend selected → agent session runs.

### T-5 — Default-flip gate (its own ruling, not part of this freeze)

- **Trigger (bound, not "after soak"):** the decision brief goes to Matt no
  later than 6 weeks after the first released version carrying the opt-in
  backend, or immediately if a blocking defect surfaces first — driver-owned,
  so the flip cannot die quietly in an unbounded soak.
- **Acceptance bar (the flip lands only if ALL hold):** (1) cold-boot
  container-create→running p50 within podman-machine's first-agent latency
  and warm p50 no worse than podman's in-machine `podman run` (T-1(e)
  numbers as the baseline); (2) idle per-container-VM memory bounded and
  total-across-a-typical-agent-day below the shared podman-machine VM's
  fixed footprint, OR a documented restart mitigation (OQ-7); (3) zero
  contract-suite regressions across the soak window on the committed
  Virtualization host; (4) no open high/critical `container` upstream defect
  on the exec/kill or socket paths. Missing any one keeps the opt-in default
  off and returns the brief to Matt.
- **Do:** when the trigger fires, bring Matt the flip decision with the bar's
  evidence attached: the flip makes apple-container the macOS default and
  begins podman-machine's macOS sunset (OQ-10) — Intel/macOS ≤ 15 are out of
  scope, not a second supported arm. The flip itself is one `SelectBackend`
  darwin-resolution change plus doc updates, landed only on that ruling.
- **Interfaces:** consumes T-1 numbers + soak evidence. Produces the
  decision brief and, on approval, the default change.
- **Test cycle:** the T-3 suite re-run as the flip's regression gate; the
  embedded smoke procedure run with NO backend config (proving the new
  default path).

## Tasks

- [ ] **T-1** Spike on Apple-silicon/macOS 26: uid mapping, vsock gateway
  channel, egress arming, streaming exec, timings, runner-on-darwin —
  `spike-findings.md` recorded with per-OQ verdicts.
- [ ] **T-2** `AppleContainerCLI` backend + `SelectBackend`
  `"apple-container"` case + version-floor verify; argv/table tests green.
- [ ] **T-3** Contract-suite entry, ported remap/egress tests, darwin CI
  lane.
- [ ] **T-4** Embedded-app backend selection + preflight probe/ensure;
  macOS smoke green.
- [ ] **T-5** Default-flip decision brief to Matt; flip landed only on his
  ruling.

## Open Questions

All spike-resolvable questions are ALSO T-1 line items; they are listed here
because an executor building against this record before the spike lands
would hit real ambiguity on each.

### OQ-1 [load-bearing] — host-user ↔ agent-uid mapping equivalent

The podman backend's file-ownership contract is `--userns=keep-id:uid=,gid=`:
"the invoking host user is mapped to the baked agent uid; files the agent
writes in a bind-mount still map back to the invoking user on the host"
(`podman.go:25-27`; emitted at `podman.go:471`; hard 4.3+ floor,
`podman.go:487-495`). Apple `container` has `--uid`/`--gid`/`--user` flags
setting the PROCESS identity (command-reference.md), but **no documented
userns-remap equivalent, and the host-side ownership of guest-written
bind-mount files is undocumented** — its volumes doc shows a root-owned view
from inside the container (volumes.md example: `-rw-r--r-- 1 root root`)
without stating the host-side ownership of guest writes. UNVERIFIED. If
guest writes land host-side as the invoking user (plausible: the virtiofs
server runs as the user's launchd agent), the contract holds with zero
remap machinery; if they land as root or a fixed uid, the checkout-dir
round-trip breaks and the backend needs an ownership-fixup design or a
named-volume workspace instead of a bind mount. T-1(a) resolves; the answer
shapes T-2's Create argv and possibly the workspace-mount model.

### OQ-2 [spike-confirms] — the vsock gateway channel through Virtualization.framework

The transport is RULED: the per-session agent↔host gateway socket rides
guest↔host vsock, mirroring the microVM's guestd forwarder
(`go/internal/guestd/gateway_proxy.go:29-33`, `:205-213`; Matt's OQ-11
ruling, Resolved decisions). What the spike confirms is the ONE hardware
unknown: whether Virtualization.framework's `VZVirtioSocketDevice` vsock is
reachable through Apple's `container` CLI so the guestd-style unix→vsock
forwarder ports onto it (a raw AF_UNIX virtiofs bind-mount is NOT assumed to
work; `container`'s `--publish-socket`/`--ssh` socket features prove host↔guest
forwarding exists as a first-class mechanism, command-reference.md /
host-integration.md). T-1(b) records connect + round-trip over the vsock
channel. If the vsock leg is NOT reachable through the CLI, the transport
question (not the apple-container direction) returns to Matt with the finding
— the microVM lane already proves vsock works under Virtualization.framework
in principle, so the risk is CLI surface, not the hypervisor.

### OQ-3 [load-bearing] — egress arming: does the nft host-exec model hold?

The design assumes the podman arming model: `--cap-add NET_ADMIN`, root
entrypoint arms nft, agent runs capability-less (`egress.go:6-10`;
`agent.go:319-328`). Apple `container` supports `--cap-add`
(runtime-configuration.md) and NET_ADMIN is absent from its default cap set
(same doc), matching podman's posture. UNVERIFIED: whether the guest kernel
ships nftables support, whether `container exec` as root can arm before the
agent exec starts, and whether the per-VM vmnet interface behaves under
default-deny (containers get an IP on a shared vmnet subnet, networking.md —
the ruleset must not sever DNS; the agent socket rides the ruled vsock
transport (OQ-11), out-of-band of the guest's netfilter, so it needs no
ruleset carve-out — see §Where the backend plugs in). T-1(c) resolves. If
in-guest arming can't precede the agent
exec, the fallback is the microVM model — arm at boot via a custom init
image (`--init-image`, runtime-configuration.md) and expose the
`EgressArmedInGuest` marker (`agent.go:298-300`) — a bigger T-2.

### OQ-4 [load-bearing] — streaming exec fidelity vs the kill/wait handle

`ExecStreaming` must return live stdio pipes plus a kill/wait handle whose
Wait distinguishes crash from deliberate kill (`podman.go:361-369`,
`ChildHandle`, `podman.go:264-273`). The podman impl gets this from a local
`podman exec --interactive` child process. `container exec` exists with
`-i`/`-t` (command-reference.md), and driving it as a local child gives the
same `*exec.Cmd` handle shape — but whether its exit-code propagation
preserves signal-vs-exit distinction through the XPC/VM hop, and whether
stdio stays live for hours without the apiserver recycling the session, is
UNVERIFIED. T-1(d) resolves. Note also 1.0's own fix "kill: Wait for
container to exit after sigkill" (apple/container#1589, 1.0.0 release
notes) — kill semantics were still being corrected at 1.0, so the spike
must test against the current release, not docs.

### OQ-5 [non-load-bearing] — post-1.0 stability in practice for a front-door dependency

1.0.0 (2026-06-09) declared the pre-1.0 breaking-change window closed
(README §Project Status guaranteed stability only within patch versions
PRE-1.0; 1.0 removed the v0 XPC APIs and noted "A subsequent release will
introduce a version on the API itself", 1.0.0 release notes). Releases since
are 1.1.0/1.2.x/1.3.1 (latest 1.3.1) plus a `0.44.0` tag published out of
band in the release feed (<https://github.com/apple/container/releases>) — a
pre-1.0 version string that the ≥ 1.0.0 floor rejects outright, so it needs
no interpretation here. Whether minor releases hold the CLI surface stable in
practice — output formats were still churning at 1.0 ("Cleaned up structured
(JSON, YAML, TOML) output shape", 1.0.0 notes) — is a standing dependency of
the committed direction, not a fork: Matt ruled apple-container the front-door
macOS engine at RIG-3246 with that release-discipline dependency inherent. The
version-floor probe (T-2) + pinned-floor policy (Global Constraints) is the
standing mitigation; T-5's flip brief carries the measured stability evidence.

### OQ-6 [non-load-bearing] — backend name string

The record uses `"apple-container"` for the `BackendConfig.Backend` value
(explicit vendor prefix; `"container"` is too generic beside `"podman"`/
`"microvm"`). Bikeshed-level; T-2 freezes whatever Matt prefers.

### OQ-7 [non-load-bearing] — idle-VM memory growth over long sessions

The Virtualization framework "implements only partial support for memory
ballooning … memory pages freed to the Linux operating system … are not
relinquished to the host. If you run many memory-intensive containers, you
may need to occasionally restart them" (technical-overview.md §Releasing
container memory). Agent sessions are long-lived; a many-agent embedded day
could accrete host memory. Not load-bearing for adoption (podman-machine's
one big VM has its own fixed-size cost), but T-1(e) should measure it and
T-5's brief should carry the number.

### OQ-8 [non-load-bearing] — Rosetta/x86 images

`container run --rosetta` exists (command-reference.md) but the
compass-agent image is built for the host arch and embedded macOS is
Apple-silicon-only under this record's floor, so no x86 path is needed.
Noted so nobody re-opens it as a gap.

## Resolved decisions

All five Matt-fork open questions were ruled on RIG-3246 (2026-09-05); the
ruling reorients this record from "opt-in behind podman-machine, two permanent
backends" to "apple-container is THE macOS engine over vsock, runner
host-side, no podman, Intel/podman-machine sunset". Folded into Approach +
Global Constraints; recorded here.

- **OQ-9 (commit Apple-silicon Virtualization hardware?) — RULED: A, the mac
  mini on Woodpecker** (Matt): "A, we have a mac mini box on the Woodpecker CI,
  we can set that up for this … you should have ssh access to the mattmini to
  start testing." The T-1 spike + T-3 live contract suite run on the committed
  self-hosted Apple-silicon/macOS-26 host; hosted arm64 macOS runners cannot
  (nested-virt unsupported, actions/runner-images#13505). GHA keeps the Linux
  lanes; the mac mini serves only the darwin spike now + the darwin contract
  suite later. The spike is a one-time manual run, not a standing gate.
- **OQ-10 (does podman-machine sunset on macOS?) — RULED: yes, sunset; Intel
  out of scope** (Matt): "can sunset, Apple has basically ended all support for
  Intel Macs, we don't need to support." No permanent two-backend macOS matrix;
  podman-machine is the interim substrate until the apple-container flip, then
  sunsets. macOS ≤ 15 + Intel are out of scope.
- **OQ-11 (settle the darwin socket transport once?) — RULED: yes, vsock,
  mirroring the microVM** (Matt): "Can mirror the microVM right? that uses
  vsock?" The gateway socket rides guest↔host vsock via the guestd-style
  forwarder (`go/internal/guestd/gateway_proxy.go:29-33`, `:205-213`;
  `mdlayher/vsock`, `go/go.mod:26`). This settles the transport for the
  apple-container lane and dissolves the OQ-2/OQ-3 raw-mount coupling; the one
  spike unknown is whether Virtualization.framework's vsock is reachable through
  the `container` CLI (OQ-2, spike-confirms).
- **OQ-12 (does removing podman-machine strand the runner?) — RULED: no, the
  runner stays host-side (darwin-native)** (Matt): "Keeping runner host side
  makes sense — that's what we do for microVMs anyway too, so putting the runner
  in a VM itself would be a departure from the other backends." The runner runs
  natively on darwin and drives apple-container host-side, dialing the guest
  over vsock — symmetric with every other backend, not stranded. This overrides
  the older compass-local-dev in-VM-runner ruling
  (`docs/designs/infra/ci/compass-local-dev/design.md:194-205`) and agrees with
  embedded-revival OQ-7's darwin-host-process topology; T-1(f) confirms the
  runner builds + runs on macOS 26. (`compass-runner/main.go:1` is
  `//go:build unix`, compiles on darwin; `gateway/socket.go:138-139` already
  carries a darwin `sun_path` budget — the pieces are close.)
- **OQ-13 (does postgres also move off podman on macOS?) — RULED: yes, all on
  apple-container, no podman** (Matt): "If we do this, then all would be on
  apple container, no podman." Postgres + the OTel collector move onto
  apple-container on macOS too (T-2 scope): the podman-hardwired stack shell
  (`postgres_container.go:246-259` + `:84`, `collector_container.go:62`) and the
  podman-pinned `ImageEnsurer` constructor (`internal/stack/adapters/image.go:
  42-44`) get an apple-container path so a macOS embedded host needs NO container
  engine but apple-container. This supersedes the record's earlier "postgres
  stays podman / split posture" framing.
