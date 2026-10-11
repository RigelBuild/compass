# Elastic session runtime — amendment: the microVM production-readiness bar

> **Design amendment.** Rules OQ-1 of
> `docs/designs/infra/runtime/compass-runner-adoption-strategy/design.md`,
> whose T1 consumes it.

Ruled: pending Matt (proposed 2026-10-11; decision DL-486)
Tracking: RIG-3070 (runner adoption strategy)
Amends: V8 W6 and W7 (`microvm-v8-acceptance-suite-benchmarks.md`)
Refs: RIG-2499 (V8), RIG-2498 (V7), RIG-3272, DL-259, DL-325

## Problem / Intent

Podman stays the floor "until a replacing backend clears a stated
production-readiness bar (OQ-1)" (adoption record, § Decision — podman).
OQ-1's four criteria cannot fail as written, and criterion 4 defers its numbers
here. On `main`, V7 (RIG-2498) and V8 (RIG-2499) have not landed (no
`compass.microvm.` in `go/`), and `TestContractSuite_Podman` runs in no CI lane.
A `compass-stack` host's metrics reach no store: the bundled collector "exports
NOWHERE until an export endpoint is configured" (`collectorConfigYAML`,
`go/internal/stack/collector_container.go`).

## Approach

The bar is met when all four criteria hold for one pinned `main` commit, the
soak's build (OQ-2). Certification changes no default (DL-325): `SelectBackend`
keeps `case "", "podman":` (`go/internal/runtime/microvm.go`).

**1. Acceptance suites green in CI.** Job `microvm` (`.github/workflows/ci.yml`)
passes, with its skip guard and V8 W7's presence check (names from source,
`--- PASS:` required, `--- SKIP:` rejected at any depth). This bar widens W7's
enumerated file set to all nine `//go:build microvm` test files under `go/`.
The skip allowlist closes at three, each keyed on test name plus skip-message
prefix: `TestInterTenantHostNetworkUnreachable/metadata_endpoint` ("host
metadata endpoint 169.254.169.254:80 did not answer"; V8 W1 names neither, so
this bar fixes the pair); `TestMicroVMVolumeQuotaEnforcedInGuest` ("no
project-quota'd filesystem available"; its headroom skip stays red); and
`TestInGuestEgressGatewayDoesNotReachHost` ("host has no IPv4 default-route
gateway").

*Green*: job `microvm` succeeded on the pinned commit's run and on the 20 most
recent `main` push and nightly runs ending at certification, spanning at least
7 days. A run superseded in its `concurrency` group is skipped. A `microvm` job
cancelled by `timeout-minutes` is red, as is a run green only on re-run after a
test failure. Not counted, and re-run: `microvm` skipped because `setup`
failed, a failed step before "microVM suites", and `microvmtest.Require`'s
withheld-KVM fatal ("requires KVM but /dev/kvm is not openable"). Every other
red counts, even a registry pull limit: a network-flaky test gets fixed.

**2. Exec and teardown parity.** The judge is `runContractSuite`
(`go/internal/runtime/contract_suite_test.go`), "one table-driven body proving
MicroVMRuntime and PodmanCLI behave identically". Both legs run its 12
unconditional rows; microVM's flags register five more
(`non_numeric_user_refused`, `output_cap_truncation`, `command_capadd_ignored`,
`stop_grace_powers_off`, `default_deny_at_start`). Every registered row passes
on the pinned commit with no `--- SKIP:` beneath. MicroVM's `backendCaps` sets
exactly eight flags: six for V2b's divergences 1-4, 6 and 7, plus
`refusesRootExec` and `gracefulStopPowersOff`; a ninth needs an amendment.
Divergence 5 ("multi-mount specs beyond one workspace share are refused") has
no row, so V2b's "Every item is asserted by a contract-suite row" fails for it.
The bar accepts the "two mounts" subtest of `TestCreateRefusesInexpressibleMount`
(`go/internal/runtime/microvm_lifecycle_test.go`, `//go:build unix`, run by the
untagged `compass-go:test` lane): `Create` returns `*UnsupportedMountError`
naming the second mount. The podman leg runs in W7's `-tags "microvm podman"`
step, `-v` to a log, `-run` widened to `TestContractSuite_Podman` and
`TestPerAgentContainerLifecycle`, each `--- PASS:` with no `--- SKIP:` beneath.

**3. Documented self-host bring-up.** DL-259 T9's shape
(`docs/designs/infra/release/compass-distribution/design.md`), in two forms:

- *Once, on the docs path.* A fresh x86_64 host with `/dev/kvm` (bare metal or
  nested virtualization) follows `docs/self-host.md` and
  `docs/self-host-guest-image.md` verbatim: `compass-stack preflight
  --runtime-backend microvm` exits 0 with every line `[PASS]`; `up
  --runtime-backend microvm --guest-artifact <ref>` reaches Ready and logs
  `microvm boot canary passed`; a TLS client gets one agent turn's reply on the
  operator's model key; after `down`, no `cloud-hypervisor`, `virtiofsd` or
  `passt` process survives. The transcript goes on RIG-3070.
- *On every `main` run.* OQ-4's microVM e2e leg: `stack.Up` with
  `RuntimeBackend: "microvm"`, one turn, `Down`, and the same process check.

**4. Dogfood soak.** OQ-1's host runs the pinned build for 14 consecutive days
(OQ-2), installed on day 0 over the prior build's state dir (the upgrade path).
Journal counts read the stack unit only (`journalctl -u compass-stack`). The
window extends until this load floor holds:

- 50 sessions on at least 10 distinct days, 10 over an hour, 4 at peak
  concurrency: the server's `compute_usage_events` `start`/`end` pairs for the
  soak Runner's `runner_id`, each with a turn: its transcript
  (`store.SessionTranscript`) holds an assistant entry;
- 2 Runner restarts (each logs `microvm boot canary passed`), one with a session
  open, after which no earlier trio process survives and the run root holds no
  prior session dir.

No detector watches a live session's I/O or connects, so a daily timer in its
own unit runs, from the pinned commit's checkout, `COMPASS_REQUIRE_MICROVM=1 go
test -tags microvm -v -run '<names>' ./internal/runtime/ ./internal/runner/`,
`<names>` derived from V8's W1-W3 files and `egress_inguest_microvm_test.go`;
criterion 1's presence check and allowlist read its log. Any one of these
resets the window to day 0, and a new window opens on the fix's pinned build:

1. a criterion-1 or criterion-2 red that reproduces on the pinned commit (a
   `main` flake breaks criterion 1's streak, not the soak);
2. a probe red: any derived name without `--- PASS:`, whether guest escape,
   cross-session reach, egress breach or another failure;
3. a Q-budget breach (OQ-5): 3 of the last 20 `microvm session booted`
   `boot_duration` values over the boot budget; 3 of the last 20 valid memory
   samples over the memory budget; or 2 consecutive canaries' `guest_rss_bytes`
   over it. A valid sample is a session's first `microvm session memory` line
   carrying both `vmm_pss_kb` and `virtiofsd_pss_kb`;
4. a session loss (OQ-3): a `microvm session died` line or a failed canary. A
   planned restart does not count: VMs are "killed and rebooted on next
   request, not adopted" (`microvm-runner.md`, § (f));
5. a `compass.microvm.orphans.reaped{outcome=possibly_live}` point;
6. a `compass.runner.session.starts{backend=podman}` point (a fallback);
7. a no-data day: no probe result; the day's increase in the
   `compass.microvm.boot.duration{outcome=ok}` histogram count, summed across
   Runner restarts, below that day's `microvm session booted` lines; or, once
   20 sessions have stayed up at least 60 s (V7's `pssSampleInterval`, so each
   saw a tick), fewer than 15 valid memory samples among the last 20. Until
   then memory coverage is pending and the window cannot close.

**After certification** the soak keeps running on each new pinned build. A
reset lapses certification until a window closes clean. A `devenv.lock` or
`flake.lock` bump (VMM trio or guest kernel) opens a new window; certification
holds unless it resets.

## Alternatives considered

- **Prose criteria only.** Rejected: an unquantified "green" or "soak" cannot fail.
- **Absolute budgets now.** Rejected: the parent defers the Q-budget, "Set from
  V2a/V8 measurements on real hardware, not invented here" (`microvm-runner.md`).
- **Podman contract leg in the `e2e` job.** Rejected: a third package breaks
  "2 x 20m = 40m leaves headroom" under `timeout-minutes: 60`.
- **A daily p95 of `compass.microvm.boot.duration`.** Rejected: V7 sets no
  buckets, so the SDK defaults (`0, 5, 10, …` seconds) put every boot in one.

## Tasks

- [ ] Matt rules OQ-1 to OQ-5. The PR stays draft until then: DL-486's pending
      status breaks the decisions README's `Active (<who>, YYYY-MM-DD)`. At
      ruling, `Ruled:` becomes `Matt (<date>)`, DL-486 `Active (Matt, <date>)`.
- [ ] V7 and V8 W1-W7 land, V8 amended: W1 runs the metadata leg as subtest
      `metadata_endpoint` with criterion 1's skip prefix; W6 adds `start_ms`
      (OQ-5); W7 takes criterion 1's file set and allowlist keys, and
      criterion 2's podman step.
- [ ] Fix the "7 conceded divergences" comments in `contract_microvm_test.go`
      and `contract_suite_test.go` to criterion 2's eight flags.
- [ ] OQ-4: guest config delivery (V4 OQ-3's follow-up), a live-flip parity
      test, the microVM e2e leg (`timeout-minutes: 30` re-budgeted), and the
      two podman tests in W7's `-run`.
- [ ] RIG-3272 part 2: backend-aware `compass-stack preflight`.
- [ ] File the gap: `--otel-external` sets no `OTEL_EXPORTER_OTLP_ENDPOINT`
      for children (`ExternalOTLPEndpoint`, `go/internal/stack/config.go`).
- [ ] Freeze the Q-budget here (OQ-5); run criterion 3's docs path; set up the
      soak host, probe timer and queries (OQ-1); run the soak.

## Open Questions

- **OQ-1 (load-bearing) — the soak host and its signal sink.** The runner
  exports only to `os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")` (`setupOtel`,
  `go/cmd/compass-runner/main.go`), and stack children inherit the unit's
  environment (`cmd.Env = append(os.Environ(), spec.Env...)`,
  `go/internal/stack/adapters/process.go`). (a) A `compass-stack up
  --runtime-backend microvm` host under `docs/self-host.md`'s systemd unit, with
  `Environment=OTEL_EXPORTER_OTLP_ENDPOINT=<retained backend>`, `--otel-external`
  at it, and the journal kept: criterion 3's shipped path. (b) The `devenv up`
  loop, whose `compass-runner` (`devenv.nix`) passes no `--backend`. (c) With
  either, a no-data day resets (trigger 7). **Recommendation: (a) + (c).**
- **OQ-2 (load-bearing) — the certified build and the window.** (a) Pin one
  build and certify it. A redeploy restarts the window when its diff touches
  `go/internal/runtime/`, `go/cmd/compass-runner/`, `go/cmd/compass-guestd/`,
  `guest-image/`, `devenv.lock` (read by `guest-image/default.nix` and
  `tools/toolchain/microvm-vmm-env.nix`) or `flake.lock` (the installed
  `compass-stack-env` trio). (b) Follow `main`; each backend-touching deploy
  needs N clean days before close, so a day-13 regression certifies after N
  days. Length: 7 days sees too few restarts and weekly cycles; 30 delays
  certification for little signal. **Recommendation: (a), 14 days.**
- **OQ-3 (load-bearing) — session-loss strictness.** (a) A rate: at most k
  backend-attributed losses per 1,000 sessions, against a podman runner over the
  same period. It needs a podman loss counter; V7 W5 designs only
  `compass.runner.session.starts{backend,outcome}`. (b) Zero tolerance. An
  exemption (host OOM-kill, host reboot, kernel update) needs a linked issue
  Matt ratifies, never a self-filed note. The window runs on while he rules; a
  rejected exemption resets it from the event's day. **Recommendation: (b).**
- **OQ-4 (load-bearing) — parity scope.** OQ-1 item 2 asks for proof "under the
  same acceptance suite the podman backend is judged by". Podman is also judged
  by `TestConfigMountIsReadOnlyAndFlipVisibleLive` and
  `TestAgentExecDropsNetAdminInNetAdminContainer` (`//go:build podman`, in no CI
  lane today) and by the e2e harness (`TestHarnessCore`, `TestComms*`), which
  has no microVM mode. The microVM leg "refuses the socket/config bind mounts"
  (`go/internal/runner/host.go`), and V4 (OQ-3) calls deferred fleet config "a
  real capability gap vs podman". (a) Keep the contract suite; list each
  excluded podman suite with its reason. (b) Add a microVM `TestHarnessCore` leg
  after guest config delivery and a live-flip parity test, and the two podman
  tests to W7's `-run`. **Recommendation: (b):** the only CI check of vsock
  Provision under `stack.Up`, and criterion 3's CI form. Cost: the fixture
  mounts `models.yml` at `cannedAgentDir`, which `workspaceShare` refuses, and
  dials `pastaHostGateway` (`go/e2e/fixture.go`). The NET_ADMIN test stays
  podman-only (microVM ignores `CapAdd`).
- **OQ-5 (load-bearing) — the Q-budget basis.** V8's `boot_ms` spans `Create` to
  one echo `Exec`, but the canary (`report.BootLatency = time.Since(start)`,
  `go/internal/runtime/microvm_preflight.go`) and V7's `boot_duration` time
  `Start` alone, hence W6's `start_ms`. V8 says "the `passt` key will routinely
  be ABSENT", so the memory basis is `pss_kb["cloud-hypervisor"] +
  pss_kb["virtiofsd"]` over iterations whose `pss_incomplete` ⊆ {`passt`}. The
  soak reads the same two processes in kB: V7's `vmm_pss_kb + virtiofsd_pss_kb`
  from each session's first sample (nearest V8's post-boot reading), and the
  canary's `guest_rss_bytes` ÷ 1024, which omits passt ("its smaps_rollup Pss is
  unreadable and drops out of the sum", `CanaryReport`). (i) Freeze both budgets
  once at 1.5× the medians of `start_ms` and two-process PSS over the first 14
  `microvm-bench-report` artifacts from `main` push or schedule runs after W7.
  If a 20-boot calibration on the soak host before day 0 differs by more than
  2×, it is the basis, with Matt's sign-off on a linked issue. (ii) Drop
  trigger 3; the report stays informational. **Recommendation: (i).**

Spec-impact: none. Ledger-impact: DL-486 (new, proposed). Refs RIG-3070
RIG-2499 RIG-2498 RIG-3272
