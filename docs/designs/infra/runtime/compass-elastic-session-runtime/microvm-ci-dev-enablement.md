# microVM CI + Dev Enablement

Status: PROPOSED — this record freezes on merge.

Parent: [microvm-runner.md](./microvm-runner.md) (RIG-2394, frozen) — this
record enables that record's KVM-gated impl tasks (V2a→V8) to be developed and
CI-verified across the compass dev shell and GitHub Actions, with a self-hosted
fallback if hosted KVM proves unreliable; it changes no decision the parent
froze.

## Problem / Intent

The frozen microVM Runner record chose cloud-hypervisor as the sole VMM (D1,
`microvm-runner.md:642-670`), booting a guest kernel + rootfs over virtio-fs +
vsock with a passt/gvproxy-class userspace net backend (D6,
`microvm-runner.md:733-750`). V1 (the `MicroVMRuntime` seam + backend
selection) is shipped and hardware-independent, but every subsequent slice —
V2a's boot spike through V8's acceptance suite — must actually boot a
cloud-hypervisor microVM, which needs `/dev/kvm`. That environment exists
nowhere today: the dev shell (`devenv.nix`) carries neither `cloud-hypervisor`
nor `virtiofsd` (verified: `command -v cloud-hypervisor` / `command -v
virtiofsd` both absent in the shell, 2026-08-22, though the dev box itself has
`/dev/kvm` crw-rw-rw- root:kvm and `/dev/vhost-vsock` crw-rw-rw-, VMX on all
cores); and compass CI runs a single `gates` job on `ubuntu-latest`
(`.github/workflows/ci.yml:96`) that installs no VMM and enables no KVM. This
record decides how the microVM backend gets developed locally and CI-verified,
unblocking V2a→V8.

## Approach

Four decisions, one per fork. Together they produce a **two-tier test posture**
the frozen record already anticipates: hardware-independent unit/contract tests
run in the moon battery everywhere; boot/integration tests are tagged and run
KVM-backed — "an integration test (KVM-gated, skipped where absent)"
(`microvm-runner.md:462`), "KVM-gated for the microVM rows"
(`microvm-runner.md:487`). This record turns that anticipation into a named
mechanism and the environments that exercise both tiers. The load-bearing
choice is where the KVM tests actually boot: **GitHub Actions, on the free
`ubuntu-latest` runner compass CI already uses** — with a self-hosted Woodpecker
lane held as the documented fallback if hosted KVM proves unreliable.

### E-D1 — dev prerequisites: userspace binaries in `devenv.nix`, host-level enablement in the host config (F1)

**A split, at the userspace/host boundary.** cloud-hypervisor, virtiofsd, and
passt are ordinary user binaries — they `open(2)` `/dev/kvm` but need no
capability, module, or device node of their own ("runs rootless as an ordinary
process", `microvm-runner.md:660-661`; the whole backend is rootless by Global
Constraint, `microvm-runner.md:379-385`). Userspace-only tools belong in the
dev shell, where every developer and the CI toolchain bootstrap get them from
one pin:

- **`devenv.nix`:** append `pkgs.cloud-hypervisor`, `pkgs.virtiofsd`, and
  `pkgs.passt` via `lib.optionals pkgs.stdenv.isLinux [ … ]` **outside** the
  parsed `with pkgs; [ … ]` literal — the exact posture `xvfb-run` and
  `chromium` already use, because the toolchain-parity gate resolves every
  bare attr in the literal on macOS too, where these Linux-only packages do
  not exist (`devenv.nix:127-144`). At the current devenv.lock nixpkgs pin
  these resolve to `cloud-hypervisor-52.0`, `virtiofsd-1.13.3`, and
  `passt-2025_09_19.623dbf6` (verified by `nix eval` against nixpkgs,
  2026-08-22 — re-resolve against the repo's own pin at implementation).
  Version floors are the V5 preflight's job ("binary versions",
  `microvm-runner.md:536-539`), not the shell's: the shell provides one pinned
  version; `VerifyMicroVMSupport` enforces the floor wherever the Runner runs.
- **`passt`, not gvproxy.** D6 named the *class* ("passt/gvproxy-class",
  `microvm-runner.md:733-750`), not the member; E1 instantiates it as `passt`
  because passt is C (no Go runtime in the guest net path), packaged in
  nixpkgs, and free of the podman-machine coupling gvproxy carries. The choice
  is one attr; V2a may revisit if its spike finds passt wanting.
- **Host config (nix-config repo, out of this repo):** the truly host-level
  bits — the `kvm-intel` module and `/dev/kvm` device with permissions the
  invoking uid can open — for the **local dev box** (and, if the Woodpecker
  fallback is ever adopted, its runner host). GitHub's hosted runners handle
  their own KVM enablement in-workflow (E-D2), so this host declaration is a
  dev-box/fallback concern, not a GHA one. Note `/dev/vhost-vsock` is **not**
  required: cloud-hypervisor's hybrid vsock is a userspace AF_UNIX socket,
  "*not* required for cloud-hypervisor's userspace hybrid-vsock socket (D1)"
  (`microvm-runner.md:536-539`), so the host obligation is `/dev/kvm` alone.
  The dev box already satisfies this (fact above); the host-config task is to
  make it *declared* rather than incidental. Preferred permission posture:
  `kvm` group 0660 + user membership rather than the current world-RW node
  (decided — E6, OQ3 resolved).

*Alternative weighed:* everything (including binaries) in the host flake.
Rejected: it would make the VMM version a per-host fact instead of a
repo-pinned one — CI and every developer must resolve the same
`cloud-hypervisor` derivation the V5 preflight's floor was tested against, and
`devenv.lock` is the only pin both sides share (the same one-pin argument the
shell already makes for postgres, `devenv.nix:99-113`).

### E-D2 — GHA runs the KVM tests as a required leg on the free `ubuntu-latest` job (F2)

**The KVM tests run in GitHub Actions**, on the same free `ubuntu-latest`
`gates` job compass CI already uses (`ci.yml:96`) — as a required leg, not a
best-effort skip. This reverses the intuition that hosted runners cannot do
KVM; the 2026 reality is more specific:

- GitHub's docs classify nested virtualization on hosted runners as
  "technically possible" but "not officially supported … experimental and done
  at your own risk" (GitHub Actions hosted-runner docs, verified 2026-08-22).
  There is no SLA.
- In practice it works, and the ecosystem relies on it. Determinate Systems
  builds real KVM-dependent artifacts — NixOS disk images — on the free
  `ubuntu-latest` runner, **unguarded**, on `pull_request`/`merge_group`/`push`
  (`DeterminateSystems/nixos-amis/.github/workflows/publish.yml:6-9,64,80-84`:
  `runner: ubuntu-latest` + `nix-installer-action` + `extra-system-features =
  kvm`). Note the KVM build runs on PRs too, in an empty environment;
  `environment: production` (`publish.yml:54`) attaches only on push-to-main and
  gates the AWS upload credentials, not the KVM build — so the KVM build is
  unguarded even on PRs. Their Nix installer action enables `/dev/kvm` by
  default ("if the host supports it", `nix-installer-action/action.yml`),
  degrading cleanly to a logged "KVM is not available" when it cannot rather
  than failing the step
  (`nix-installer-action/src/index.ts:530-540,862-967`). Where they use larger
  runners for VM work it is for cores/RAM on heavy NixOS `vm-test` batteries
  (`nix-installer/ci.yml:502-510`), not because free-runner KVM is unreliable.

So compass treats free-runner KVM the way the ecosystem does: **assume it is
present and require it.** The rare run where GitHub withholds `/dev/kvm` reds
the leg; a re-run clears it. If that proves frequent, the Woodpecker fallback
(E-D3) is ready. Mechanism:

- **KVM enablement step.** Before the KVM suites, the `gates` job runs the udev
  group-perms step the ecosystem standardizes on —
  `KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"` into
  `/etc/udev/rules.d/`, then `udevadm control --reload-rules` + `udevadm
  trigger --name-match=kvm` — so the runner uid can `open(2)` `/dev/kvm`. This
  is the same rule the Determinate action writes
  (`nix-installer-action/src/index.ts:883-957`); compass installs Nix via
  `cachix/install-nix-action` (`ci.yml:150`), so it writes the rule itself
  rather than adopting the Determinate action (a larger CI change; see
  Alternatives). The world-RW `MODE="0666"` here is deliberate and distinct
  from E-D1/E6's tighter `kvm`-group 0660 for the dev box: a GHA runner is an
  ephemeral single-tenant throwaway where a world-RW `/dev/kvm` is harmless,
  whereas the persistent dev box warrants the group posture.
- **Build tag `microvm`.** Every boot/integration test (V2a's boot spike, V2b's
  microVM contract rows, V3/V5/V6/V7/V8's KVM suites) is build-tagged
  `//go:build microvm && unix`, mirroring the `pgtest` convention exactly: "The
  suites are build-tagged `pgtest`, so the moon battery's `go test ./...` never
  compiles them" (`ci.yml:42-44`; existing tags at e.g.
  `go/internal/auth/harness_pgtest_test.go:1`). The moon `compass-go:test` task
  (`go test -race ./...`, `go/moon.yml:151-161`) therefore never compiles the
  KVM tests, and the one-job moon battery is unchanged. The KVM suites run in a
  **dedicated job step** with `-tags microvm`, exactly as the pgtest suites run
  in their own "Real-Postgres suites" step *after* the battery
  (`ci.yml:256-282`) — inside the one `gates` job, no new job, no matrix.
- **Runtime gate inside the tag.** Tagged tests call a shared helper (new
  package `go/internal/microvmtest`) that skips when `/dev/kvm` is not
  openable — and turns that skip into a **hard failure** when
  `COMPASS_REQUIRE_MICROVM=1` is set, mirroring `pgtest.RequireDSN` +
  `COMPASS_REQUIRE_LIVE` ("a skip would silently pass the suite without
  exercising anything", `go/internal/pgtest/pgtest.go:62-68,99-111`). The GHA
  step sets `COMPASS_REQUIRE_MICROVM=1` so a KVM-less run reds rather than
  silently passing — the same posture the pgtest step takes with
  `COMPASS_REQUIRE_LIVE=1` (`ci.yml:268`), including its companion
  "assert-it-ran-rather-than-skipped" guard (`ci.yml:284-300`).
- **Every-PR exposure.** The KVM step, like the pgtest step, runs on every PR
  regardless of affected-detection (it is a job step, not an affected-gated moon
  task), so a docs- or UI-only PR that touches no Go still pays the enable +
  substitute + boot cost, and a rare withheld-`/dev/kvm` red can block any PR —
  cleared by a re-run. This is the same all-PR exposure the pgtest
  service-container step already carries; it is a deliberate ONE-JOB
  consequence, not new risk this record introduces.
- **What the moon battery still verifies:** the untagged tier — V1's selection
  tests, V2b's contract-suite rows against the podman backend and fakes, V4's
  hermetic gateway suites "over … a vsock-shaped `net.Listener` fake where CI
  lacks the VMM" (`microvm-runner.md:528-529`), V5's preflight unit tests with
  fake probes (`microvm-runner.md:550-551`). The tagged tier is compiled and
  run by the dedicated KVM step above.

*Alternatives weighed:*

- *Best-effort skip on GHA (soft-gate).* Run the KVM suites when `/dev/kvm`
  happens to be present, skip otherwise, with a nightly hard sweep. Rejected in
  favor of the assume-KVM required posture: the evidence (Determinate shipping
  production artifacts unguarded on free runners) says KVM is present the vast
  majority of the time, so a soft gate trades near-certain per-PR coverage for
  the small cost of an occasional re-run — and a soft gate lets a real
  regression merge on the run where KVM was absent. If reds from withheld KVM
  become frequent, revisit this or the Woodpecker fallback.
- *Larger GHA runners.* KVM is officially supported there, but GitHub docs are
  explicit that larger runners are "always billed at the per-minute rate" for
  public repos too (GitHub larger-runner docs, verified 2026-08-22). Rejected:
  the free runner works; pay only if it stops.
- *Switching compass CI to `DeterminateSystems/determinate-nix-action`* (which
  enables KVM by default). Rejected for this record: it swaps the repo's nix
  installer wholesale — a bigger CI change than adding one udev step, and one
  that touches every job's nix bootstrap. The udev step is the minimal change;
  the action swap is a separate proposal if wanted later.

### E-D3 — Woodpecker on compass as the documented fallback (F3)

If hosted-runner KVM proves unreliable in practice — frequent reds from
withheld `/dev/kvm`, or GitHub tightening access — the fallback is to **enroll
the compass repo itself on the self-hosted Woodpecker fleet** and run the KVM
leg there. This is explicitly *compass's own* CI, not the managed service's:
compass features are tested in compass. The fleet has a registered self-hosted
Linux CI agent (x86_64, KVM-capable, docker backend) already, so the hardware
is in place; what is missing is a compass pipeline (no `.woodpecker*` config
exists in this repo — verified). The step shape:

```yaml
microvm-tests:
  image: <a nix-capable CI image>
  devices:
    - /dev/kvm
  environment:
    COMPASS_REQUIRE_MICROVM: "1"
  commands:
    - nix build .#compass-guest-kernel .#compass-guest-rootfs  # substituted, not rebuilt (E-D4)
    - cd go && go test -tags microvm -race ./...
```

The fleet runs the docker backend (confirmed) with the repo marked *trusted*,
so the per-step `devices: [/dev/kvm]` mapping grants exactly that one device —
never `privileged: true`. The contract this fallback fixes is: the step grants
`/dev/kvm` (and only `/dev/kvm` — `/dev/vhost-vsock` is not needed, per E-D1),
sets `COMPASS_REQUIRE_MICROVM=1` so skips fail loudly, and realizes the
guest-image derivations from the binary cache before the test run. This is the
same env + tag contract E-D2's GHA step uses, so adopting the fallback is a
lane swap, not a redesign. It also realizes V8's "CI job (KVM-labeled runner)"
(`microvm-runner.md:606-608`).

*Separate concern — the managed service:* the managed service is built out of
tree and will need its own KVM CI when its buildout starts. That is deferred to
the managed-service control-plane work
(the fleet control plane, RIG-2485), not this record — this record keeps
compass's own tests in compass.

### E-D4 — guest image: derivations beside the agent image, kernel from nixpkgs, a project binary cache (F4)

V2a *produces* `compass-guest-kernel` and `compass-guest-rootfs`
(`microvm-runner.md:456-460`); this record fixes where they live, what they
reuse, and how CI avoids rebuilding them:

- **Placement: a standalone `guest-image/` directory**, sibling of
  `agent-image/`, exposing the two attrs from a `default.nix`. Not a container
  build — no devenv container module involved — but the same
  standalone-not-a-moon-project posture `agent-image/` established, plus a
  thin **`compass-guest-image` moon project** whose `ci` task `nix build`s
  both attrs with `inputs` globs scoped to `guest-image/**` (+ the reused
  agent-image inputs), so a guest-image PR pays the build pre-merge and every
  other PR skips it via the affected gate (`ci.yml:25-36`) — the same
  pre-merge-build-gate move the agent image made
  (`compass-agent-image-publish.md:179-181`).
- **Kernel: the root `devenv.lock`-pinned nixpkgs kernel** (`pkgs.linuxPackages`'
  bzImage, direct-kernel boot — cloud-hypervisor boots an uncompressed/bzImage
  kernel directly, no bootloader). This makes the kernel **free in CI**: it is
  substituted from cache.nixos.org, never built. A size/boot-time-optimized
  custom config is a deliberate deferral (non-load-bearing Open Question) —
  Q-budget's measurements (`microvm-runner.md:839-842`) will say whether it
  matters.
- **Rootfs: reuse the agent image's toolchain closure.** `agent-image/`
  already assembles exactly the payload the guest needs — the toolchain
  derivation + the bundled agent entrypoint (`agent-image/devenv.nix:34-36`,
  `agent-image/toolchain.nix`) with the agent uid-1000 identity
  (`agent-image/devenv.nix:83-101`). `compass-guest-rootfs` imports that same
  `toolchain.nix`, passing **root's** `pkgs` — so the rootfs pins to the root
  `devenv.lock` (the E3 inputs pin), diverging from agent-image's own nixpkgs
  pin, a parity note V2a must honor (one closure, two artifact shapes: OCI
  layers there, a block/filesystem image here — the exact format (erofs vs
  ext4) and the writable-`/nix` mechanism the toolchain's in-guest `nix` needs
  (`agent-image/toolchain.nix:47-53`) are V2a's to decide, not this
  record's) and adds what the guest additionally needs: the
  `compass-guestd` binary as init, the egress prerequisites — "`nft`,
  `getent`, `awk`, and a writable `/etc/resolv.conf`"
  (`microvm-runner.md:446-449`) — and the virtio-fs mount unit for the stable
  session path.
- **Caching: a self-hosted nix binary cache.** `ci.yml` already names its
  substituters explicitly and refuses `accept-flake-config` precisely so cache
  trust lives in a reviewed file (`ci.yml:156-168`). The self-hosted fleet
  already runs a nix binary cache (attic, S3/R2-backed) for its own first-party
  closures, with a public/main push path separated from a PR-scoped internal
  one and no push token ever handed to a fork PR. This record **reuses that
  cache** rather than standing up hosted cachix: extend the same
  `extra_nix_config` block with that cache's public substituter + trusted key
  so the GHA job pulls the rootfs pre-built (the kernel needs none —
  `cache.nixos.org`). Population stays a **main-only** workflow (`push:
  branches: [main]` + path filter + `workflow_dispatch`, the least-privilege
  separate-workflow shape the agent-image publish lane established,
  `compass-agent-image-publish.md:139-191`; the push token is a repo secret
  PR events never see). If the rootfs is small enough to build inside the KVM
  step under budget, the cache is an optimization rather than a hard dependency
  — a call V2a's build-mechanism spike settles.

## Global Constraints

- **ONE JOB, NOT A MATRIX — inviolable.** No new GHA job, no matrix entry, no
  project/task enumeration in workflow YAML (`ci.yml:4-23`). The KVM suites run
  as **steps within the existing `gates` job**, exactly as the pgtest suites do
  (`ci.yml:256-300`) — never a new job or matrix leg. The tagged tests stay out
  of the *moon battery* by build tag; they enter CI as a dedicated `go test
  -tags microvm` step, not a workflow conditional. Any new publish/cache-push
  lane is a **separate least-privilege workflow**, never a widened gate token
  (`compass-agent-image-publish.md:139-191`).
- **The two-tier tag rule.** Anything that opens `/dev/kvm`, spawns
  cloud-hypervisor/virtiofsd/passt, or requires the guest image is tagged
  `//go:build microvm && unix` and calls `microvmtest.Require(t)` first.
  Untagged tests MUST run on a KVM-less box. A tagged test MUST self-skip
  where KVM is absent and MUST hard-fail under `COMPASS_REQUIRE_MICROVM=1`
  (the `pgtest`/`COMPASS_REQUIRE_LIVE` posture,
  `go/internal/pgtest/pgtest.go:62-68`). The GHA KVM step sets the require flag.
- **One nix pin.** VMM/virtiofsd/passt versions and the guest kernel come from
  the `devenv.lock`-pinned nixpkgs (or repo-pinned inputs) — never a per-host
  install, `go install`, or GHA `setup-*` action. Version *floors* are
  enforced at runtime by V5's `VerifyMicroVMSupport`, not by the shell.
- **Cache trust lives in reviewed files.** Substituters + trusted public keys
  are named in `ci.yml`'s `extra_nix_config`; `accept-flake-config` stays off
  (`ci.yml:156-168`). Adding a cache is a workflow-file change, reviewed as
  such.
- **No secrets on PR events.** The cache-push credential lives only in the
  main-only push workflow; PR runs (including fork PRs) are pull-only against
  public caches. The KVM enablement step needs no secret.
- **Toolchain-parity gate discipline.** Linux-only packages are appended
  outside the parsed `with pkgs; [ … ]` literal in `devenv.nix`
  (`devenv.nix:127-144`); nothing here may break the parity gate's macOS
  resolution.
- **Rootless everywhere.** No CI step or dev instruction may require root
  beyond the one-time host-config declaration (E-D1) and the GHA KVM-enable
  step (which uses the runner's passwordless sudo, not the test processes'); the
  test processes run as the invoking user, matching the backend's own
  constraint (`microvm-runner.md:379-385`).
- **This record adds no product behavior.** It changes packaging, CI, and test
  scaffolding only; the microVM backend's own contracts stay owned by
  `microvm-runner.md` V1-V8.

## Plan

Ordered by dependency. E1+E2 unblock local development of V2a immediately;
E3-E5 land the CI legs; E6 is the host declaration tracked here but executed in
its owning repo.

### E1 — dev shell: VMM toolset

Add cloud-hypervisor, virtiofsd, and passt to the compass dev shell as
Linux-only appends, with a rationale comment following the xvfb-run/chromium
pattern (`devenv.nix:127-144`).

- **Interfaces:** produces `cloud-hypervisor`, `virtiofsd`, `passt` on the dev
  shell PATH (nix attrs `pkgs.cloud-hypervisor`, `pkgs.virtiofsd`,
  `pkgs.passt`, appended via `lib.optionals pkgs.stdenv.isLinux`). Consumes
  nothing new. Deliberately excluded from the toolchain-parity attrs
  (Linux-only, same as chromium); the KVM CI step realizes them out-of-band
  from the same pin (E5), exactly as the chromium e2e step does
  (`ci.yml:213-224`).
- **Test cycle:** `devenv shell` on Linux resolves all three binaries
  (`command -v`); `bun tools/toolchain/parity.ts` stays green; on macOS the
  shell still evaluates (parity gate unbroken).

### E2 — `microvmtest` gate package

The shared runtime gate the tag rule requires: `go/internal/microvmtest` with
`Require(t *testing.T)` — skips with a named reason when `/dev/kvm` is not
openable by the current uid, fails when `COMPASS_REQUIRE_MICROVM=1`, and
locates the VMM/virtiofsd binaries and guest-image paths from env with PATH
fallbacks.

- **Interfaces:** produces `microvmtest.Require(t *testing.T) Env` with
  `Env{KernelImage, RootfsImage, VMMPath, VirtiofsdPath string}`; env vars
  `COMPASS_REQUIRE_MICROVM`, `COMPASS_TEST_GUEST_KERNEL`,
  `COMPASS_TEST_GUEST_ROOTFS` (defaulting to the `nix build` result symlinks);
  the `microvm` build-tag convention documented in the package comment.
  Consumed by every V2a+ integration test. The pure decision function is
  split from the `*testing.T` dispatch for unit-testing, mirroring
  `decideDSNSource` (`go/internal/pgtest/pgtest.go:81-97`).
- **Test cycle:** untagged unit tests over the decision function (absent
  device → skip; absent + require → fail; present → proceed); the package
  compiles untagged so the moon battery type-checks it everywhere.

### E3 — guest-image derivations + moon build gate

`guest-image/default.nix` exposing `compass-guest-kernel` (pinned-nixpkgs
bzImage) and `compass-guest-rootfs` (agent-image toolchain closure +
`compass-guestd` init + `nft`/`getent`/`awk` + writable `/etc/resolv.conf` +
virtio-fs mount of the stable session path), plus the `compass-guest-image`
moon project gating the build pre-merge.

- **Interfaces:** produces nix attrs `compass-guest-kernel`,
  `compass-guest-rootfs` (the V2a interface, `microvm-runner.md:456-460` —
  V2a consumes these instead of inventing its own); imports
  `agent-image/toolchain.nix` unchanged (`agent-image/devenv.nix:34-36`);
  produces `guest-image/moon.yml` with a `ci` task (`nix build` both attrs)
  and `inputs` globs covering the full rootfs closure — `guest-image/**`,
  `agent-image/toolchain.nix`, `agent-image/entrypoint.nix`,
  `tools/toolchain/toolchain-tools.nix`, `tools/toolchain/versions/**` (the
  repo-pinned bun the closure resolves through, `agent-image/toolchain.nix:45`),
  `packages/compass-agent/**`, `package.json`, `bun.lock`, plus `devenv.lock` +
  `devenv.yaml`. `agent-image/toolchain.nix` is a `{ pkgs, ... }` function
  (`agent-image/toolchain.nix:35-36`), so `guest-image/default.nix` calls it
  with **root's** `pkgs`: the kernel and the whole rootfs closure resolve from
  the *root* `devenv.lock` pin (the one in this inputs list), not agent-image's
  own pin — a deliberate divergence V2a must honor for parity. A glob that
  misses one of these silently skips a build that should have run
  (`ci.yml:29-33`) and leaves the E4 cache stale — the same
  closure-approximation the publish lane documents
  (`compass-agent-image-publish.md:198-217`). The final glob set is contingent
  on V2a's image-shape/build-mechanism choice (erofs vs ext4; whether the
  `forks/**` inputs the publish lane lists enter the rootfs closure).
- **Test cycle:** `nix build ./guest-image#compass-guest-kernel
  #compass-guest-rootfs` succeeds on Linux; `moon run compass-guest-image:ci`
  green; an unrelated-path PR does not schedule it (affected check). Boot
  verification itself is V2a's job, not this task's.
- **Sequencing note:** lands with a stub `compass-guestd` (V2a grows the real
  one); the derivation shape and the moon gate are this record's, the boot
  behavior is V2a's.

### E4 — binary cache substituter + main-only push lane

The self-hosted cache the GHA job substitutes the rootfs from, and the workflow
that populates it.

- **Interfaces:** adds the cache name + public key to `ci.yml`'s
  `extra_nix_config` (`ci.yml:165-168`, one reviewed line each for
  `extra-substituters` / `extra-trusted-public-keys`); produces
  `.github/workflows/push-guest-image-cache.yml` (`push: branches: [main]`
  with the E3 path filter + `workflow_dispatch`; `contents: read` only; the
  push token as a repo secret), building both attrs and pushing the closure.
  Consumes E3's attrs.
- **Test cycle:** a `workflow_dispatch` run populates the cache; a follow-up
  CI run's log shows the rootfs substituted, not built; a fork-PR-shaped run
  (no secret) still substitutes read-only.

### E5 — GHA KVM leg: enablement step + tagged suites in the `gates` job

The primary KVM test lane: two steps added to the existing `ubuntu-latest`
`gates` job (`ci.yml`), mirroring the pgtest step pair (`ci.yml:256-300`).

- **Interfaces:** produces (1) a "Enable KVM" step writing the udev group-perms
  rule + `udevadm reload/trigger` (E-D2); (2) a "microVM suites" step
  (`working-directory: go`) that realizes E3's attrs from E4's cache, puts the
  E1 VMM binaries on PATH out-of-band (the chromium-e2e pattern,
  `ci.yml:213-224`), and runs `go test -tags microvm -race -timeout <n>m ./...`
  with `COMPASS_REQUIRE_MICROVM=1` + the capture-then-exit redirect the pgtest
  step uses (`ci.yml:271-282`); (3) an "assert the microVM suites ran rather
  than skipped" guard deriving its skip-text + package-count from source, the
  pgtest guard's exact shape (`ci.yml:284-300`). Consumes E2's env contract,
  E3's attrs, E4's cache.
- **Test cycle:** on a PR the step runs the tagged suites KVM-backed and the
  assert-ran guard proves they executed (not skipped); a deliberately broken
  boot test reds the step; a run where GitHub withholds `/dev/kvm` reds via the
  require flag (the accepted rare cost, E-D2).

### E6 — host KVM declaration (nix-config repo)

Declare what today is incidental: KVM enablement + device permissions on the
**local dev box** (and the Woodpecker runner host if the fallback is adopted),
in the host nix config. GHA handles its own KVM in-workflow (E5), so this is a
dev-box/fallback concern.

- **Interfaces:** produces host-config settings: `kvm-intel` module,
  `/dev/kvm` udev rule (`kvm` group, 0660 recommended — OQ3) + user/agent
  membership in `kvm`. Explicitly NOT produced: `/dev/vhost-vsock`
  enablement (not needed — hybrid vsock is userspace,
  `microvm-runner.md:536-539`, contingent on V2a's spike confirming) or any
  VMM binary (E1 owns binaries).
- **Test cycle:** after converge, the invoking uid opens `/dev/kvm` (the exact
  check V5's preflight performs, `microvm-runner.md:536`); local `go test -tags
  microvm` boots a VM on the converged dev box.

## Tasks

- [ ] E1 — dev shell: cloud-hypervisor + virtiofsd + passt (Linux-only appends)
- [ ] E2 — `go/internal/microvmtest` gate package + `microvm` build-tag convention
- [ ] E3 — `guest-image/` derivations + `compass-guest-image` moon build gate
- [ ] E4 — binary cache substituter + main-only push workflow + `ci.yml` lines
- [ ] E5 — GHA KVM leg: enable-KVM step + tagged `go test -tags microvm` suites + assert-ran guard in the `gates` job
- [ ] E6 — host KVM declaration: device/module/group in nix-config (dev box + Woodpecker-fallback host)

## Open Questions

Resolved (2026-08-22 — grounding the CI reality and the self-hosted fleet):

- **OQ1 — where the KVM tests run → RESOLVED (E-D2).** GitHub Actions, on the
  free `ubuntu-latest` job, as a required leg (assume-KVM, like Determinate's
  production NixOS-image build). Woodpecker on compass is the documented
  fallback (E-D3) if hosted KVM proves unreliable; not the primary.
- **OQ3 — `/dev/kvm` permission posture on the dev box → RESOLVED (E6).** The
  local/self-hosted boxes run testing environments only (production runs on
  hyperscalers), so E6 declares `kvm`-group `0660` + explicit membership rather
  than the incidental world-RW node. (GHA runners set their own perms via the
  E5 udev step.)
- **OQ4 — cache hosting → RESOLVED (E-D4).** The self-hosted fleet already runs
  a nix binary cache (attic, S3/R2-backed) for its first-party closures, with a
  public/main push path separated from a PR-scoped internal one and no push
  token ever handed to a fork PR. This record reuses that cache rather than
  standing up hosted cachix; E-D4/E4 wire its public substituter + key into
  `ci.yml`.

Non-load-bearing (documented deferrals):

- **OQ2 — Woodpecker-on-compass pipeline shape.** Only relevant if the E-D3
  fallback is adopted: the exact `.woodpecker` pipeline file + how it reuses
  E4's cache config. Deferred until the fallback is triggered; the step
  contract (E-D3) is fixed.
- **OQ5 — custom guest kernel config.** The pinned-nixpkgs kernel is
  substituted free but generic; a minimal virtio-only config would cut boot
  time and rootfs closure. Defer until Q-budget's V2a/V8 measurements
  (`microvm-runner.md:839-842`) show the generic kernel misses the budget.
- **OQ6 — nightly KVM-sweep need.** With the KVM suites running on every compass
  PR (E-D2), a separate nightly sweep is unnecessary; the "CI (full sweep)"
  step (`ci.yml:245-253`, `moon run :ci` gated `if: github.event_name !=
  'pull_request'`) already re-runs the whole battery on each landing to main and
  on the nightly schedule (`ci.yml:62-66`). Revisit only if the assume-KVM
  posture is downgraded to best-effort.
