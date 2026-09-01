# microVM Runner V5 — preflight, boot canary, hard-fail startup gate

Status: PROPOSED — details the V5 milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V5,
microvm-runner.md:534-552; Approach (e), microvm-runner.md:211-236) and its
frozen KVM-absent hard-fail decision D3 (microvm-runner.md:693-703).
Authoritative issue scope: RIG-2496.

Ledger impact: none. V5 details the hard-fail posture D3 already froze
("`VerifyMicroVMSupport` (V5) fails Runner startup with an error naming the
missing capability and pointing at the fix", microvm-runner.md:693-697) plus
the V1-review-flagged startup reorder RIG-2496 carries; nothing here is a new
cross-cutting decision, and `docs/designs/DECISIONS.md` is untouched (its
microVM rows — e.g. DL-259, which *consumes* "microVM D3 hard-fail" — already
point at the parent).

## Problem / Intent

The Runner has no microVM preflight, and the preflight it does have runs for
the wrong backend. `go/cmd/compass-runner/main.go` runs the podman-specific
check **unconditionally at startup, ahead of backend selection**:

> "Ahead of every operator-input check: this validates an engine fact the
> whole launch path depends on … so the legible startup refusal must come
> first."
> `if err := runtime.NewPodmanCLI().VerifyUsernsRemapSupport(context.Background()); err != nil { return err }`
> (`go/cmd/compass-runner/main.go:94-104`)

while `backends.selectEngine()` — the `runtime.SelectBackend` call that
decides which backend actually runs (`main.go:153`, resolver at
`main.go:254-276`, `SelectBackend` at `go/internal/runtime/microvm.go:97-118`)
— happens ~50 lines later. So an operator on a podman-less KVM box selecting
`--backend microvm` is refused *by the podman check* — a confusing refusal for
a backend they did not choose (RIG-2496 § "Move the startup preflight behind
backend selection"). Meanwhile the microVM backend's own startup-failure axes
— `/dev/kvm` not openable, VMM userspace missing or below floor, guest image
assets absent — surface only deep inside the first session boot, exactly the
late-failure mode `VerifyUsernsRemapSupport` exists to prevent on podman
("learns the cause at startup rather than deep inside the first container
create", `go/internal/runtime/podman.go:497-503`). The config struct already
promises the fix: "the V5 preflight names any missing one at startup rather
than deep in a launch" (`go/internal/runtime/microvm.go:19-24`).

V5 delivers three things (parent Plan § V5, microvm-runner.md:534-552):

1. **`(*MicroVMRuntime) VerifyMicroVMSupport(ctx) error`** — the static
   preflight, mirroring `VerifyUsernsRemapSupport`'s
   name-the-floor-and-the-found error posture (`podman.go:504-521`).
2. **`(*MicroVMRuntime) BootCanary(ctx) (CanaryReport, error)`** with
   `CanaryReport{BootLatency time.Duration; GuestRSSBytes int64}` — the
   dynamic preflight that really boots a canary VM (VMM start → guest
   supervisor handshake over vsock → echo exec → teardown, under a deadline),
   proving the whole chain and producing the boot-latency + per-VM-RSS
   measurement the parent's observability (h) / benchmark (V8) consume.
3. **The hard-fail startup gate**: `main.go` selects the backend first, then
   runs the *selected* backend's preflight — podman check iff podman,
   `VerifyMicroVMSupport` (+ canary) iff microvm — aborting startup on any
   failure with the capability-naming error (D3), never degrading.

## Approach

Each subsection resolves one fork the parent's V5 plan leaves to detailing.
Every resolution is also listed in `## Open Questions` for the pre-freeze
batch, and the body designs against the recommended option.

### (a) The `main.go` reorder: selection outcome before the preflight decision

`selectEngine` is pure flag/env resolution — it reads the parsed backend flags
and their env fallbacks and constructs a runtime handle without touching the
engine (`main.go:254-276`; `SelectBackend` is a switch over `cfg.Backend`,
`microvm.go:109-118`; `NewMicroVMRuntime` just fills the struct,
`microvm.go:86-95`). Nothing prevents calling it immediately after
`flag.Parse` + logging setup. The reorder is therefore small and preserves the
legibility intent the current comment defends (`main.go:94-101`: the engine
refusal must precede the operator-input checks so a token-less operator on a
broken engine isn't sent chasing tokens first):

1. `--version` print → `slog` setup (unchanged, `main.go:86-92`);
2. `engine, err := backends.selectEngine()` — **moved up** from `main.go:153`.
   Its own errors (an unknown backend name, a non-numeric
   `COMPASS_MICROVM_CPUS`, `main.go:291-304`) are legible startup refusals in
   their own right;
3. the backend-gated preflight (below) — still ahead of the id/addr/token/
   image checks (`main.go:106-123`), so the refusal-ordering contract is kept
   *per selected backend*;
4. everything else unchanged.

**How main picks the preflight.** The frozen `ContainerRuntime` interface
gains nothing (the V3/V4-ratified discipline: capability probes are unexported
single-method interface assertions, never interface verbs —
microvm-v4-gateway-over-vsock.md § Global Constraints, "Frozen
`ContainerRuntime` interface untouched"). `main.go` grows one extracted,
hermetically testable helper:

```go
// verifyBackendPreflight runs the selected engine's startup preflight: the
// podman userns-remap check iff the engine is the podman backend, the microVM
// support check iff it is the microVM backend. Probed via unexported
// single-method interfaces so the frozen ContainerRuntime interface is
// untouched and a test fake can present either capability.
func verifyBackendPreflight(ctx context.Context, engine runtime.ContainerRuntime) error
```

with two unexported probe interfaces in `package main`:

```go
type microVMPreflighter interface{ VerifyMicroVMSupport(context.Context) error }
type podmanPreflighter interface{ VerifyUsernsRemapSupport(context.Context) error }
```

probed in that order (a type satisfying both is impossible today; first-match
keeps the dispatch deterministic). An engine satisfying neither probe is a
startup **error**, not a pass — a future third backend must declare its
preflight or be refused, never silently skip the gate (the D3 fail-closed
posture applied to the gate itself). Because the microVM probe is an interface
assertion rather than a direct call, `main.go` stays compilable even where the
unix-tagged method bodies are absent (`microvm_lifecycle.go:1-10` keeps the
untagged `runtime` package type-checking everywhere).

The ctx passed is the same top-of-`main` root the current preflight call uses
(`context.Background()` at `main.go:102` — the process root, before the
signal-bound ctx exists at `main.go:161`).

The canary's startup wiring rides the same gate: when the microVM probe
matches, `verifyBackendPreflight` runs `VerifyMicroVMSupport` **then**
`BootCanary` (§(e)/(f)), logging the returned `CanaryReport` via `slog` (the
V5-era consumer of the measurement; V8's benchmark surface takes it over
later). Rationale and cost in OQ-4.

### (b) `VerifyMicroVMSupport`: the check set, where it lives, and the shared pure core

**The check set** (parent 218-224, adjusted per (c)/(d) and the boot path as
built):

1. **`/dev/kvm` exists and is openable by the Runner uid.** An open-R/W probe,
   not a stat — "presence on the filesystem is not enough — the uid needs
   read/write access a VMM opens with" (`go/cmd/compass-stack/preflight.go:122-125`;
   same posture as `microvmtest.kvmOpenable`, `microvmtest.go:130-134`).
2. **VMM userspace binaries found and at/above pinned floors** — the trio the
   boot path actually resolves **from PATH**: `launch` does
   `exec.LookPath("virtiofsd")` / `exec.LookPath("passt")` /
   `exec.LookPath("cloud-hypervisor")` (`go/internal/runtime/microvm/launch.go:155,174,212`),
   by design ("Binaries are resolved from PATH here (not carried on
   BootConfig) so the harness stays a pure boot contract", `launch.go:127-130`).
   The preflight checks the same PATH resolution, so it gates what the boot
   consumes. Floors are the devenv.lock pins compass-stack already encodes:
   cloud-hypervisor 53.0.0, virtiofsd 1.14.0, passt 2025_09_19
   (`preflight.go:60-64`, pinned to `devenv.nix:206-218`). passt is included
   although the parent's clause names only "the VMM and `virtiofsd` binaries"
   (microvm-runner.md:221-223) — a flagged divergence (OQ-6): a passt-less
   host fails every boot at `launch.go:174-176`, so omitting it would be a
   preflight hole.
3. **Guest image assets present and verified**: `KernelImage`, `RootfsImage`,
   `InitrdImage` (`MicroVMConfig`, `microvm.go:25-50`) each set (a missing
   path is named with its flag/env knob, the `MicroVMConfig` doc's promise at
   `microvm.go:19-24`), present, and readable; content hash verification per
   (d).
4. **`RunRoot` usable**: set, creatable/writable, and short enough that a
   session's suffixed gateway socket path fits the AF_UNIX budget — the same
   `sunPathMax` comparison `Create` performs per-session
   (`microvm_lifecycle.go:62-67,243-256`), run once at startup against a
   worst-case (32-hex) session id so an over-long RunRoot is an operator error
   at startup, not at first Create. This is also where the "vsock
   prerequisite" lands — see (c).

Every failure returns an error naming **the missing capability and the fix**,
the D3 posture ("needs KVM — use the managed service or a KVM-capable host",
microvm-runner.md:693-703), in the mirror of podman's floor message shape
("podman %d.%d or newer is required … but this host has podman %s",
`podman.go:514-519`). It is a plain `error` return consumed by `main` as a
startup abort — there is no degrade signal for it to carry (D3).

**Where it lives.** A new `go/internal/runtime/microvm_preflight.go`
(`//go:build unix`, beside `microvm_lifecycle.go` per the established split:
untagged `microvm.go` holds what selection needs to type-check anywhere,
unix-tagged files hold the bodies, `microvm.go:3-11`). The method is on
`*MicroVMRuntime` (parent Interfaces, microvm-runner.md:545-549) so it reads
`m.config` directly.

**The shared pure core — the load-bearing convention fork.** compass-stack's
T9 install-time gate already carries verified, unit-tested check logic and
says on its face it is a placeholder for this record's gate:

> "there is NO VerifyMicroVMSupport function in the runtime lane today … These
> checks are T9's OWN, and are TO BE REPLACED by the runtime lane's eventual
> preflight gate once it lands."
> (`go/cmd/compass-stack/preflight.go:15-26`)

Its pure helpers — `versionFloor`/`microVMFloors`/`versionGroups`/`atLeast`/
`firstLine` (the version-floor comparator, `preflight.go:43-120`),
`decideKVM` (`preflight.go:122-135`), `decideVersion` (`preflight.go:155-175`)
— are exactly the decision logic `VerifyMicroVMSupport` needs, but they live
in `package main`, unimportable. Reimplementing them in the runtime lane would
be the second-convention-beside-an-existing-one the house style forbids;
V5 instead **extracts the pure core into a small shared leaf package** and
makes both gates consume it:

- New `go/internal/hostcheck` (untagged; pure decisions + the effectful
  `ProbeKVM`, mirroring how `microvmtest` keeps its non-test files untagged,
  `microvmtest.go:19-25`): `Result{Name, OK bool, Detail}`,
  `VersionFloor{Binary string, Fields []int, Display string}`,
  `MicroVMFloors []VersionFloor` (the one copy of the devenv.lock pins),
  `VersionGroups`, `AtLeast`, `FirstLine`, `DecideKVM(openErr error) Result`,
  `DecideVersion(f VersionFloor, lookErr, runErr error, output string) Result`,
  `ProbeKVM() error`. Existing unit tests move with the code.
  Two placement/scope notes: (i) `internal/hostcheck` (top-level) over
  `internal/runtime/hostcheck` (a subdir of the runtime lane) because
  compass-stack (`package main`, a non-runtime consumer) also imports it — a
  top-level leaf reads as the shared host-probe vocabulary it is; either is
  defensible, this is the call. (ii) `microvmtest`'s own KVM-open probe
  (`kvmOpenable`, `microvmtest.go:130-134`) is a third copy of the
  open-`/dev/kvm` posture; W1 folds it onto `hostcheck.ProbeKVM` too, so the
  one-copy argument this fork rests on is complete rather than leaving a
  test-only duplicate standing.
- `compass-stack/preflight.go` rewires onto `hostcheck` for those helpers and
  keeps only what is stack-specific: `decidePodman` (rootless-capability is a
  stack concern — postgres-in-a-container, `preflight.go:137-153`) and the
  print/exit surface. Its header's REPLACED-BY note is updated to point here:
  the *logic* is now the runtime lane's; the install-time command remains a
  thin consumer.
- `VerifyMicroVMSupport` consumes the same `hostcheck` decisions, adding only
  its runtime-config-specific axes (images, RunRoot) and the D3 error
  assembly. Effectful probes (`LookPath`, `--version` exec, file stat/hash)
  are injected through an unexported seam struct so every failure axis is
  hermetically unit-testable — the `parsePodmanVersion` testability split
  (`podman.go:523-527`) generalized.

Weighed in OQ-2; rejected alternatives in `## Alternatives considered`.

### (c) The vsock prerequisite under D1: there is no static host-device check

The parent's clause keeps a conditional: "the vsock prerequisite is present (a
host `/dev/vhost-vsock` for a kernel-vhost transport, *not* required for
cloud-hypervisor's userspace hybrid-vsock socket (D1))"
(microvm-runner.md:218-221; restated in Plan § V5 at 536-539). D1 forecloses
the condition: cloud-hypervisor is the only VMM ("cloud-hypervisor, and only
cloud-hypervisor", microvm-runner.md:642-670), and its hybrid vsock's host end
is an AF_UNIX socket the VMM itself serves — `Launch` passes
`--vsock cid=…,socket=<runtimeDir>/vsock.sock` (`launch.go:278-280`), the host
side dials/serves plain unix sockets (`dial.go:108-128`,
microvm-v4-gateway-over-vsock.md §(a)), and no `/dev/vhost-vsock` open ever
occurs on the host path (confirmed against the V2a spike per the parent).

**Resolution: the static "vsock prerequisite" check is deliberately empty.**
There is no host device or module to probe; a probe for `/dev/vhost-vsock`
would test a capability the boot path never uses and fail correct hosts. What
the vsock chain *statically* needs is exactly what check 4 in (b) already
covers — a writable RunRoot whose per-session socket paths fit the AF_UNIX
budget — and its *dynamic* proof is the canary's handshake-over-vsock, which
exercises the real VMM-served socket end to end (parent 225-229: the canary
"proves the whole chain — KVM, vsock, image, supervisor — not just binary
presence"). The record states this so a future reader doesn't re-add a
device check by pattern-matching other VMMs' requirements. Flagged as OQ-1
since it resolves a parent conditional to "no check" rather than a check.

### (d) Image verification: presence always; content hash against an optional manifest

The parent freezes "the guest kernel + rootfs image assets are present and
hash-verified" (microvm-runner.md:223-224), and the guest-image build was
made bit-reproducible *for* this: "pinning it … makes the image a pure
function of the closure — what lets V5's preflight hash-verify the asset"
(`guest-image/default.nix:375-380`), with a build-time double-pack `cmp` gate
whose failure message names V5 ("V5's preflight hash-verify depends on this
invariant", `default.nix:419-435`).

What the parent does not fix is the **trusted hash source**. The Runner
receives bare paths (`--microvm-kernel` / `$COMPASS_MICROVM_KERNEL` etc.,
`main.go:235-241,266-270`); nothing ships an expected digest today — the
guest-image lane publishes store paths, not a digest manifest, and a nix store
path for an input-addressed derivation does not authenticate file content at
use time. Three candidates weighed in OQ-3; the design is:

- New `MicroVMConfig.ImageManifest string` (+ `--microvm-image-manifest` /
  `$COMPASS_MICROVM_IMAGE_MANIFEST`): the path of a SHA-256 manifest file
  (`<hex digest>  <basename>` lines, `sha256sum` format) that the guest-image
  lane will emit alongside the images.
- Manifest **set** → each configured image is hashed (streaming SHA-256) and
  compared; a mismatch or a configured image absent from the manifest is a
  startup error naming the image, both digests, and the knob.
- Manifest **unset** → presence + readability checks only, plus one `slog`
  warning naming the un-verified state. This is the dev-box reality (operators
  point straight at `nix build` results; the CI leg exports raw store paths,
  `.github/workflows/ci.yml:753-804`) — hard-requiring a manifest in V5 would
  break every existing bring-up for an artifact no lane produces yet.

The unset-manifest arm is a flagged divergence from the parent's unconditional
"hash-verified" (OQ-3): verification is delivered, its *mandatoriness* is
staged behind the release/distribution lane actually shipping manifests.
Producing the manifest in the guest-image lane is follow-up work named in
OQ-3, not V5 scope (V5 touches no nix).

### (e) `BootCanary` boots through the runtime's own verbs, not a parallel Launch path

Two candidate shapes for the canary's boot path:

- **Option A (recommended): compose the backend's own lifecycle verbs.**
  `BootCanary` drives `m.Create` → `m.Start` → `m.Exec` (echo) → `m.Remove`
  on a reserved canary session, exactly as a real session runs. What this
  buys, verified against the code:
  - **It proves the production chain, not a lookalike.** `Start` is
    Launch → Health-poll under the boot deadline → nonce verification →
    `Provision` opening the exec gate (`microvm_lifecycle.go:354-416`,
    `awaitHealthy` at 431-458); the echo exec crosses the GuestExec surface
    sessions use (`Exec`, `microvm_lifecycle.go:476-502`). A canary that
    drove `microvm.Launch` (`launch.go:111`) + `GuestClient` (`dial.go:116`)
    directly would skip Provision, the nonce binding, and the exec-gate
    contract — passing on a host where real sessions still fail.
  - **Hermetic testability for free.** The canary inherits the
    `launchFunc`/`newGuestClient` seams (`microvm.go:77-84`,
    `installSeamDefaults` at `microvm_lifecycle.go:126-139`) that hermetic
    Start tests already override, so the canary's sequencing, deadline, and
    report assembly are unit-testable with no KVM; only the end-to-end proof
    is KVM-gated.
  - **Teardown is Remove's, not a second copy.** Start already tears down its
    own partial boot on any failure (`microvm_lifecycle.go:374-383`), and
    Remove is the idempotent teardown; the canary adds no new cleanup path.
  Mechanics: a canary `ContainerSpec` with a reserved name
  (`compass-canary-<8-hex random>` — outside the agent-session prefix so it
  cannot collide with `runner.AgentContainerNamePrefix` sessions), a **single
  throwaway workspace mount** — a freshly `os.MkdirTemp`'d host dir mounted
  read-write at `/workspace` (`Mount{HostPath: <tmp>, ContainerPath:
  workspaceMountPath, ReadOnly: false}`, `podman.go:62-65`, `workspaceMountPath`
  at `microvm_lifecycle.go:77`) — **not** an empty mount set. An empty set
  leaves `FSSharedDir=""` (`bootConfig`, `microvm_lifecycle.go:296`) and
  `Launch` unconditionally runs virtiofsd with `--shared-dir=` (`withFS` always
  true for `Launch`, `launch.go:112,154-166`), which the guest health gate then
  blocks on forever: `awaitHealthy` requires `workspace_mounted`
  (`microvm_lifecycle.go:440`), guestd's mount(2) of the `workspace` tag. Every
  KVM boot in the tree pointedly uses a throwaway shared dir for this reason
  (`boot_microvm_test.go:54,69`; `launch_teardown_test.go:61`); the canary
  boots the real virtio-fs share a session depends on, and removes the temp dir
  on teardown (joined into the result, never leaked). The zero-value
  `EgressPolicy` still emits the full default-deny ruleset
  (`microvm_lifecycle.go:160-166`), and `agentuid.AgentUID` is the exec uid
  (Provision requires non-zero, `microvm_lifecycle.go:389-391`; the canary
  execs as the uid sessions exec as). The echo is
  `Exec(ctx, id, NewExecSpec("echo", canary-nonce))` asserting exit 0 + the
  nonce on stdout. `Remove` always runs, its error joined into the result
  (never discarded).
  - **Report assembly.** `BootLatency` = wall time of the `Start` call
    (boot → healthy → provisioned — the "time to ready" a session actually
    waits; tighter sub-phase timing is V8's benchmark concern).
    `GuestRSSBytes` = the sum over `vm.PSS()` (kB → bytes; per-process PSS of
    vmm/virtiofsd/passt, `launch.go:457-487`), read after the echo while the
    VM is live — PSS, not summed RSS/VmHWM, because guest RAM is one shared
    mapping and PSS divides shared pages among mappers (`launch.go:457-462`);
    the field keeps the parent-frozen name `GuestRSSBytes` with a doc comment
    stating the PSS basis. `PSS` is reachable because `BootCanary` is
    in-package and the `guestVM` seam already carries it
    (`microvm_lifecycle.go:99-113`). PSS read errors on the sandboxed helpers
    are tolerated exactly as `PSS` itself tolerates them (best-effort,
    `launch.go:470-483`); a wholly-empty PSS map yields `GuestRSSBytes = 0`,
    reported not fatal — the canary's gate is the boot chain, the RSS is
    telemetry.
    The number undercounts by the passt share on a healthy host — passt sets
    `PR_SET_DUMPABLE=0`, so its `/proc/<pid>/smaps_rollup` Pss is unreadable
    and drops out of the sum (`launch.go:470-483`); the doc comment states this
    so V8's benchmark treats `GuestRSSBytes` as a vmm+virtiofsd-dominated lower
    bound, not an exact footprint.
- **Option B (rejected): drive `microvm.Launch` + `GuestClient` directly.**
  Smaller surface but a second boot convention beside the lifecycle's, with
  its own teardown copy, skipping Provision/nonce/exec-gate — weaker proof,
  more code, hermetic tests need new seams. Rejected; recorded in OQ-5.

### (f) The canary's deadline vs the ctx-lifetime footgun

The KVM suites carry a hard rule: never
`context.WithTimeout` + `defer cancel()` at VM-*session* scope — a bounded ctx
that outlives its function kills the VM mid-session, because every child is
`exec.CommandContext`-bound to it (`launch.go:163,187,220`); Start's own
Health poll therefore derives its deadline into a ctx that dies with the call
(`bootPollContext`, `microvm_lifecycle.go:460-468`) while the launch ctx stays
session-lived.

The canary is the case the footgun rule's scope condition excludes: **the
canary OWNS its VM's full lifetime inside one call.** No VM outlives
`BootCanary`, so a bounded ctx spanning boot → echo → teardown is not a
lifetime hazard — it is the required deadline (parent 225-229: "under a
deadline"). Design:

- `BootCanary` **derives** its bound from the caller's ctx (never a fresh
  root): the caller's deadline when it carries one, else a single
  `canaryDeadline = 90s` bound spanning the whole call. This is one shared
  bound, not a 60s-boot-plus-margin decomposition: `bootPollContext`
  (`microvm_lifecycle.go:460-468`) branches on whether the *caller's* ctx
  carries a deadline, and under the canary's bounded ctx it takes the
  `WithCancel` branch — so `Start`'s health poll inherits the canary's full
  90s, not the 60s `bootDeadline` (that floor applies only to a deadline-less
  caller). 90s is boot (≤60s worst case, `microvm_lifecycle.go:84-86`) plus
  headroom for provision + one echo exec + the severed teardown; a wedged boot
  burns the whole bound, then tears down (below). If sub-phase attribution is
  later wanted, `BootCanary` can carve an explicit boot sub-deadline before
  `Start` — not needed for the gate, whose verdict is pass/fail under one
  ceiling.
- That bounded ctx drives Create/Start/Exec — so a wedged boot dies with the
  deadline instead of hanging Runner startup.
- **Teardown is severed from the deadline**: `Remove` runs under
  `context.WithoutCancel(ctx)` with its own short grace bound, so a canary
  that *timed out mid-boot* still gets a clean teardown rather than an
  already-dead ctx — mirroring Start's own teardown defer
  (`microvm_lifecycle.go:377-383`, which uses `context.WithoutCancel` for
  exactly this reason). No leaked VMM on any exit path.
- The KVM-gated canary test still passes `t.Context()` as the *caller* ctx
  (the harness rule unchanged); the canary's internal bound composes under
  it.

Resolved as OQ-7's recommendation: the deadline governs the whole
boot→teardown as a derived ctx inside the call; the footgun rule is about
scopes that outlive the bound, which the canary structurally cannot.

### (g) Naming: `BootCanary` is not the existing "canary" test

`go/internal/microvmtest/canary_microvm_test.go` already owns the word: it is
"a SMOKE TEST OF THE ENABLEMENT WAVE, not a boot … It deliberately does NOT
spawn cloud-hypervisor or boot the guest" (`canary_microvm_test.go:10-25`) —
an env-resolution assertion (`TestCanaryMicroVMEnv`) that exists so the CI
KVM leg's assert-ran guard counts a real `microvmtest.Require` caller. V5's
`BootCanary` is a different artifact: a production `*MicroVMRuntime` method
that really boots. Disambiguation, so a future reader never conflates them:

- The method keeps the parent-frozen name `BootCanary`
  (microvm-runner.md:545-547); its doc comment names the distinction and
  cross-references `canary_microvm_test.go` explicitly (and vice versa: W3
  adds one sentence to the smoke test's header pointing at `BootCanary`).
- The KVM-gated test is `TestBootCanary` in `package runtime`
  (`boot_canary_microvm_test.go`, `//go:build microvm && unix`) — different
  package, different file, no shared name with `TestCanaryMicroVMEnv`.

## Alternatives considered

- **Reimplement the check logic in the runtime lane; leave compass-stack's
  copy as-is** ((b), OQ-2 option b). Two floor tables to drift apart (the
  exact failure the single `microVMFloors` pin exists to prevent,
  `preflight.go:56-59`), two version parsers, and a standing violation of the
  no-second-convention rule — rejected despite being the smallest diff.
- **compass-stack shells out to `compass-runner --preflight`** ((b), OQ-2
  option c). Honors "replaced by the runtime lane's gate" most literally, but
  inverts the dependency at install time (the stack gate now needs the runner
  binary built and on PATH to *check the host*), and T9's header explicitly
  chose self-containment to stay off the runtime lane's critical path.
  Rejected.
- **A `/dev/vhost-vsock` existence check** ((c)). Tests a device the CH boot
  path never opens; fails correct hosts. Rejected.
- **Hash images against their nix store path names** ((d)). Input-addressed
  store paths don't authenticate content at use time, and the Runner must not
  assume a nix store exists on the host. Rejected.
- **A canary that boots via `microvm.Launch` directly** ((e), OQ-5 option B).
  Rejected above.
- **A skip-canary escape hatch** (`--skip-boot-canary`). A degrade knob by
  another name; D3 says the knob collapses ("the `microvm.required` knob
  collapses to always-on: there is nothing to degrade *to*",
  microvm-runner.md:702-703). Rejected — see OQ-4 for the startup-latency
  cost this accepts.

## Global Constraints

Every task below inherits these.

- **No degrade, ever (D3).** A preflight or canary failure is a startup abort
  with an error naming the missing capability and the fix
  (microvm-runner.md:693-703); no fallback, no warn-and-continue, no skip
  flag. The error-message shape is `VerifyUsernsRemapSupport`'s
  name-the-floor-and-the-found posture (`podman.go:514-519`).
- **The podman path is byte-identical.** Podman-selected startup runs exactly
  today's `VerifyUsernsRemapSupport` call with today's semantics; no podman
  argv, check, or message changes. Existing hermetic runner suites run
  unchanged.
- **Frozen `ContainerRuntime` interface untouched.** Preflight and canary are
  `*MicroVMRuntime` methods reached from `main` via unexported single-method
  probe interfaces — the V3/V4-ratified discipline
  (microvm-v4-gateway-over-vsock.md § Global Constraints).
- **Version floors are the devenv.lock pins, stated once.** cloud-hypervisor
  53.0.0, virtiofsd 1.14.0, passt 2025_09_19 (`preflight.go:60-64`,
  `devenv.nix:206-218`); after W1 the single copy lives in
  `hostcheck.MicroVMFloors` and both gates consume it.
- **KVM-gated vs hermetic split.** Everything booting a VM carries
  `//go:build microvm && unix` and calls `microvmtest.Require(t)` first
  (`microvmtest.go:19-25,107-115`); the pure check helpers, the preflight
  failure axes (injected probes), the `main.go` gate dispatch, and the
  canary's sequencing (seam-faked) are tested hermetically.
- **The ctx-lifetime footgun.** Any KVM-touching test passes `t.Context()`
  to Start/BootCanary — never `context.WithTimeout` + `defer cancel()` at
  VM-session scope. The canary's internal bound is a *derived* ctx confined
  to the call (§(f)); per-exec deadlines stay separate short-lived contexts.
  Contexts are always threaded from the caller; the only roots are top-of-
  `main` and test roots.
- **CI microvm leg invocation unchanged**:
  `go test -tags microvm -race -v -timeout 15m ./...` with
  `COMPASS_REQUIRE_MICROVM=1` + `CGO_ENABLED=1`
  (`.github/workflows/ci.yml:753-754,804`); the canary integration test rides
  it, budgeted inside the existing 15m.
- **External-reference gate.** Compass-tracked files only; no private names
  beyond RIG-NNN.

## Plan

W1 (shared pure core + static preflight) and W2 (the `main.go` gate) are
hermetic and compile-independent; W3 (canary) consumes W1's floors only
incidentally and is hermetic-plus-KVM. **Landing order W1 → W2 is a
constraint, not a preference:** W2's fail-closed gate errors when neither probe
matches, so merging W2 before `VerifyMicroVMSupport` exists (W1) would make
`--backend microvm` un-startable in the intervening window (today it starts,
behind the wrong podman check, `main.go:102`). W2 *compiles* without W1 (the
microVM probe is an interface assertion), but must not *merge* before it. W2
ships the gate wired for both preflights; the canary call inside the gate lands
with W3.

### W1 — `hostcheck` extraction + `VerifyMicroVMSupport` (hermetic)

The (b)+(c)+(d) static preflight and the one-copy check core.

- **Interfaces:** produces
  - package `go/internal/hostcheck` (untagged):
    `type Result struct { Name string; OK bool; Detail string }`;
    `type VersionFloor struct { Binary string; Fields []int; Display string }`;
    `var MicroVMFloors []VersionFloor` (the devenv.lock pins, one copy);
    `func VersionGroups(s string) []int`;
    `func AtLeast(got, floor []int) bool`;
    `func FirstLine(s string) string`;
    `func DecideKVM(openErr error) Result`;
    `func DecideVersion(f VersionFloor, lookErr, runErr error, output string) Result`;
    `func ProbeKVM() error`
    — moved (not copied) from `compass-stack/preflight.go:43-191` with their
    unit tests; exported names, behavior identical;
  - `compass-stack/preflight.go` rewired onto `hostcheck` (keeps
    `decidePodman` + print/exit surface; header's REPLACED-BY note updated to
    point at the runtime gate);
  - `func (m *MicroVMRuntime) VerifyMicroVMSupport(ctx context.Context) error`
    in new `go/internal/runtime/microvm_preflight.go` (`//go:build unix`),
    checking, in order: KVM (`hostcheck.ProbeKVM` → `DecideKVM`); the PATH
    trio at floors (`hostcheck.MicroVMFloors` → `DecideVersion`); images
    present/readable + manifest hash per (d); RunRoot writable + worst-case
    suffixed-path `sunPathMax` budget. First failure returns the D3
    capability-naming error;
  - `MicroVMConfig.ImageManifest string` + `--microvm-image-manifest` /
    `$COMPASS_MICROVM_IMAGE_MANIFEST` threading in `main.go`'s
    `registerBackendFlags`/`selectEngine` (`main.go:226-276`);
  - an unexported probe seam
    `type preflightProbes struct { openKVM func() error; lookPath func(string) (string, error); version func(ctx context.Context, path string) (string, error); statImage func(string) error; hashImage func(string) (string, error) }`
    defaulted in the method, overridden in tests.
- **Test cycle (hermetic, no KVM):** (1) `hostcheck` unit tests moved intact
  (version-group rows incl. passt's date scheme, `AtLeast` edge rows,
  `DecideKVM`/`DecideVersion` verdicts); (2) per-axis preflight rows via
  injected probes: KVM open error names KVM + the fix; each trio binary
  absent/below-floor names the floor and the found version; each unset/absent
  image names its flag/env knob; manifest mismatch names image + both
  digests; unset manifest passes presence-only; over-long RunRoot names the
  budget and the knob; all-green returns nil; (3) `compass-stack` preflight
  tests still green after the rewire (behavior unchanged).

### W2 — the backend-gated startup preflight in `main.go` (hermetic)

The (a) reorder. Lands wired for both static preflights; the canary line is
added by W3.

- **Interfaces:** produces
  - the reorder: `backends.selectEngine()` moved ahead of the preflight
    (currently `main.go:153` and `main.go:94-104` respectively); the
    legibility comment rewritten for the per-backend contract;
  - `func verifyBackendPreflight(ctx context.Context, engine runtime.ContainerRuntime) error`
    in `package main`, with unexported probes
    `type microVMPreflighter interface { VerifyMicroVMSupport(context.Context) error }` and
    `type podmanPreflighter interface { VerifyUsernsRemapSupport(context.Context) error }`;
    neither-probe-matches is an error naming the engine type (fail-closed
    gate);
  - consumes `runtime.SelectBackend` (`microvm.go:109-118`) and both verify
    methods unchanged.
- **Test cycle (hermetic):** a startup test over fake engines proving
  RIG-2496's acceptance clause: (1) a fake exposing only the podman probe →
  exactly that probe called, the microVM one never; (2) a fake exposing only
  the microVM probe → exactly that called, podman never (the podman check
  does NOT run under `--backend microvm`, and vice versa); (3) a
  neither-probe fake → error; (4) a probe returning an error aborts startup
  with that error verbatim; (5) podman-selected default path exercises
  today's ordering (regression guard on the reorder).

### W3 — `BootCanary` + `CanaryReport` + startup wiring (hermetic + KVM-gated)

The (e)+(f)+(g) dynamic preflight.

- **Interfaces:** produces
  - `type CanaryReport struct { BootLatency time.Duration; GuestRSSBytes int64 }`
    (`GuestRSSBytes` doc-commented as PSS-based, §(e)) and
    `func (m *MicroVMRuntime) BootCanary(ctx context.Context) (CanaryReport, error)`
    in `microvm_preflight.go`: derive the bounded ctx (caller deadline else a
    single `canaryDeadline = 90 * time.Second` spanning the call, §(f)); Create
    the reserved-name canary spec with a throwaway `os.MkdirTemp` workspace
    mount at `/workspace` (NOT an empty mount set — §(e)), zero-value
    `EgressPolicy`, `agentuid.AgentUID`; Start (BootLatency = its wall time);
    echo-exec + assert nonce; read `session.vm.PSS()` and sum to bytes; Remove
    under `context.WithoutCancel(ctx)` + short grace and delete the temp dir,
    errors joined into the return, never discarded;
  - the W2 gate extended: microVM arm runs `VerifyMicroVMSupport` then
    `BootCanary`, `slog`-logging the report
    (`boot_latency`, `guest_rss_bytes`);
  - one sentence added to `canary_microvm_test.go`'s header cross-referencing
    `BootCanary` (§(g));
  - consumes `Create`/`Start`/`Exec`/`Remove` and the
    `launchFunc`/`newGuestClient` seams (`microvm.go:77-84`) unchanged.
- **Test cycle:**
  - **Hermetic (seam-faked):** (1) sequencing — Create→Start→Exec→Remove in
    order, report populated, session table empty after; (2) a Start failure
    yields the error, no session leaked; (3) an echo failure still Removes;
    (4) a canary over a caller ctx with a deadline honors it; without one,
    the internal bound applies; (5) the canary name never collides with the
    agent prefix.
  - **KVM-gated (`boot_canary_microvm_test.go`, `//go:build microvm && unix`,
    `microvmtest.Require(t)`, `t.Context()`):** (1) `TestBootCanary` — a real
    boot returns nil error, `BootLatency > 0` and under the deadline,
    `GuestRSSBytes > 0`, no VMM/virtiofsd/passt process and no runtime dir
    left after; (2) absent-KVM posture is covered hermetically in W1
    (`DecideKVM`) and by `Require`'s own gate — the D3 acceptance row
    "absent-KVM → startup fails with the capability-naming error" (parent
    550-552) is the W1 axis-test plus the W2 dispatch test composed.

## Tasks

- [ ] W1 — extract `go/internal/hostcheck` (floors/version comparator/KVM+
      version verdicts + `ProbeKVM`) out of `compass-stack/preflight.go`,
      rewire compass-stack onto it; add
      `(*MicroVMRuntime) VerifyMicroVMSupport(ctx) error` with injected
      probes covering KVM / PATH-trio floors / image presence +
      optional-manifest hash / RunRoot budget, D3 error posture;
      `MicroVMConfig.ImageManifest` + flag/env threading
- [ ] W2 — reorder `main.go`: `selectEngine` before the preflight;
      `verifyBackendPreflight(ctx, engine)` dispatching via unexported
      probes (podman check iff podman, microVM check iff microvm,
      neither ⇒ error); hermetic startup dispatch tests
- [ ] W3 — `BootCanary` + `CanaryReport` composing
      Create/Start/Exec(echo)/Remove under a derived bounded ctx with
      teardown severed via `WithoutCancel`; PSS-summed `GuestRSSBytes`;
      startup wiring + `slog` report; hermetic seam tests + KVM-gated
      `TestBootCanary`; canary-vs-smoke-test cross-references

## Open Questions

Batched for the pre-freeze ruling; the body designs against each
recommendation.

- **OQ-1 (load-bearing) — the "vsock prerequisite" resolves to NO static
  check.** The parent's clause conditions a `/dev/vhost-vsock` probe on "a
  kernel-vhost transport" (microvm-runner.md:218-221, 536-539) that D1
  forecloses: CH-only, userspace hybrid vsock, host end an AF_UNIX socket the
  VMM serves (`launch.go:278-280`; V4 §(a)). No host device is ever opened,
  so V5 checks nothing device-shaped statically; the vsock chain's static
  needs collapse into the RunRoot writability + AF_UNIX path-budget check,
  and its real proof is the canary's handshake (dynamic). Divergence: a
  parent-named check is resolved as deliberately empty rather than
  implemented. **Recommendation:** ratify — no `/dev/vhost-vsock` (or any
  vsock device/module) probe in `VerifyMicroVMSupport`.
- **OQ-2 (load-bearing) — `VerifyMicroVMSupport` shares compass-stack's
  check core via an extracted `go/internal/hostcheck` package.** Options:
  (a) extract the pure helpers (`versionFloor`/`microVMFloors`/
  `versionGroups`/`atLeast`/`decideKVM`/`decideVersion`,
  `preflight.go:43-175`) into a shared leaf package both gates consume — one
  floor table, one parser, tests move once; (b) reimplement in the runtime
  lane and leave T9's copy — smallest diff, but two floor tables to drift
  and a standing second convention; (c) compass-stack execs the runner's
  preflight — most literal "replaced by", but inverts the install-time
  dependency and breaks T9's deliberate self-containment. The T9 header
  explicitly marks its checks "TO BE REPLACED by the runtime lane's eventual
  preflight gate" (`preflight.go:15-26`). **Recommendation:** (a); T9 keeps
  only `decidePodman` + its print/exit surface, header note updated.
- **OQ-3 (load-bearing) — image hash verification is manifest-based and the
  manifest is optional in V5.** The parent freezes "present and
  hash-verified" (microvm-runner.md:223-224) and the image build is
  bit-reproducible for it (`guest-image/default.nix:375-380,419-435`) — but
  no lane ships an expected-digest artifact today, and the Runner gets bare
  paths (`main.go:266-270`). Options: (i) optional sidecar SHA-256 manifest
  (`MicroVMConfig.ImageManifest`): set ⇒ hash-verify hard-fail, unset ⇒
  presence+readability + a logged un-verified warning; (ii) mandatory
  manifest — honest to the parent's letter but breaks every current
  bring-up (dev boxes and the CI leg export raw store paths,
  `ci.yml:753-804`) until the release lane ships manifests; (iii)
  presence-only in V5, hashing deferred whole. **Recommendation:** (i), with
  the guest-image/release lane's manifest emission filed as a **named
  follow-up issue at the freeze→file→dispatch gate**, with manifest-mandatory
  tied to that issue's close — a concrete forcing event, not an open-ended
  "revisited later", so the parent's frozen "hash-verified" has a mechanism
  back to true. (The negative premise — no lane emits a digest manifest today
  — was reconfirmed this session by a `grep` of `guest-image/` finding only
  nix-internal `narHash`/`vendorHash`, `default.nix:55,99`, no expected-digest
  artifact.) Divergence from the parent's unconditional "hash-verified" is
  accepted and flagged here.
- **OQ-4 (load-bearing) — the boot canary runs at every microVM Runner
  startup, and there is no skip flag.** The parent says "at startup (and on
  demand)" (microvm-runner.md:225-229); the cost is one full VM
  boot→teardown (~60s worst case, bootDeadline
  `microvm_lifecycle.go:84-86`; typically far less) added to Runner startup
  on the microVM backend only. A skip flag would be a degrade knob D3
  retires ("there is nothing to degrade *to*", microvm-runner.md:702-703).
  Options: (i) always-on at startup, hard-fail (parent's letter);
  (ii) static-only at startup, canary on-demand/periodic (faster startup,
  but a KVM-regressed box then accepts sessions until the first boot
  fails — exactly the late failure D3 exists to prevent); (iii) always-on
  with a skip flag (rejected outright). **Recommendation:** (i); the
  `CanaryReport` is `slog`-logged at startup as the V5-era observability
  consumer, V8's benchmark takes the measurement surface over later.
- **OQ-5 (non-load-bearing) — canary mechanism: composed lifecycle verbs,
  not a direct `microvm.Launch` path.** Composing
  Create→Start→Exec→Remove proves the production chain (Provision, nonce,
  exec gate — `microvm_lifecycle.go:354-416`) and inherits the
  `launchFunc`/`newGuestClient` seams for hermetic testing
  (`microvm.go:77-84`); a direct-Launch canary is a second boot convention
  with weaker proof. **Recommendation:** composed verbs over a throwaway
  workspace mount (§(e)), reserved `compass-canary-<hex>` name outside the
  agent prefix.
- **OQ-6 (non-load-bearing) — passt joins the binary floor checks.** The
  parent names "the VMM and `virtiofsd` binaries" (microvm-runner.md:221-223)
  but the boot path hard-requires passt too (`exec.LookPath("passt")`,
  `launch.go:174-176`), and compass-stack's floor table already carries all
  three (`preflight.go:60-64`). Omitting passt would leave a preflight hole
  the first boot falls into. **Recommendation:** check the trio; flag as a
  (benign, additive) divergence from the parent's two-binary phrasing.
- **OQ-7 (non-load-bearing) — the canary's deadline is a derived bounded ctx
  spanning the whole call; the ctx-lifetime footgun does not apply.** The
  footgun (never `WithTimeout`+`defer cancel()` at VM-session scope) guards
  VMs that OUTLIVE the bounded scope; the canary's VM structurally cannot —
  it is created and removed inside `BootCanary`. The bound derives from the
  caller's ctx (else `canaryDeadline = 90s`), and teardown is severed via
  `context.WithoutCancel` + a short grace so a mid-boot timeout still tears
  down cleanly (mirroring `microvm_lifecycle.go:377-383,460-468`).
  **Recommendation:** ratify this reading; KVM tests keep passing
  `t.Context()` as the caller ctx.
- **OQ-8 (non-load-bearing) — dead `MicroVMConfig.VMMPath`/`VirtiofsdPath`
  fields vs the PATH-resolving boot.** The preflight checks PATH because the
  boot resolves PATH (`launch.go:127-130,155,174,212`); the config's
  `VMMPath`/`VirtiofsdPath` fields (`microvm.go:26-30`) are today consumed
  by no boot code (only tests copy them around), so a preflight honoring
  them would gate on paths the boot ignores. Reconciling the fields (thread
  them into `launch`, or retire them) is real but not V5 scope — V5 must not
  smuggle a boot-path change into a preflight record.
  **Recommendation:** preflight checks PATH (the boot truth); file the
  field reconciliation as follow-up at the freeze→file→dispatch gate.
- **OQ-9 (non-load-bearing) — V5 ships no on-demand canary surface; the
  canary runs at startup only.** The parent says the canary runs "at startup
  (and on demand)" (microvm-runner.md:225-229); V5 delivers the startup half
  and `slog`-logs the report, leaving the on-demand/periodic re-probe to V8's
  benchmark surface (§(a)). A parent-named capability (on-demand) is deferred,
  not implemented — flagged here so the deferral is explicit rather than
  resolved silently inside OQ-4. **Recommendation:** ship startup-only in V5;
  the on-demand surface lands with the V8 benchmark harness that consumes the
  same `CanaryReport`.
- **OQ-10 (non-load-bearing) — `GuestRSSBytes` reports PSS, not RSS, under a
  parent-frozen field name.** The parent's Interfaces sketch names the field
  `GuestRSSBytes` (microvm-runner.md:545-547); §(e) fills it with summed
  proportional-set-size (PSS), not resident-set-size, because guest RAM is one
  shared mapping and PSS is the honest per-VM share (`launch.go:459-464`). The
  name is kept for parent-surface stability with a doc comment stating the PSS
  basis, and the value undercounts by the passt share (`PR_SET_DUMPABLE=0`,
  `launch.go:470-483`). Redefining a frozen field's meaning is flagged rather
  than done silently. **Recommendation:** keep the frozen name; document the
  PSS basis + the passt undercount; revisit the name with V8 if its benchmark
  needs a different basis.
