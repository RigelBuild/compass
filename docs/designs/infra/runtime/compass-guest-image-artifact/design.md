# Compass guest image artifact

Status: Draft

Tracking: RIG-3775

Owner: compass-managed

## Problem

The microVM backend's agent toolchain is baked into the guest root filesystem
at build time: `guest-image/default.nix` imports `agent-image/entrypoint.nix` +
`agent-image/toolchain.nix` and packs the result into a reproducible erofs
image. Two costs. First, two sources of truth for "the agent": the container
backends resolve the published agent OCI image at runtime, while the guest
re-derives the same closure from source — and feeds those imports the ROOT
`devenv.lock` nixpkgs rather than `agent-image`'s own pin (the pin divergence
`guest-image/default.nix`'s own header flags), so the same agent can differ
across backends. Second, an agent bump is a full Runner rollout:
`runner-image/moon.yml` declares `/guest-image/**` an input,
`tools/runner-image/build.ts` realises the closure, and the Dockerfile does
`COPY store /nix/store` with `GUEST_ROOTFS`/`GUEST_KERNEL`/`GUEST_INITRD`
frozen into ENV — so a toolchain change reships the Runner binary,
cloud-hypervisor, virtiofsd, passt, the kernel and the initrd (a multi-GB
closure) and cycles every node.

This record designs two things: (a) derive the guest root filesystem FROM the
published agent OCI image, so agent contents have one source of truth and the
pin divergence closes; (b) publish the three guest assets (kernel, rootfs
erofs, initrd) as a digest-pinned OCI artifact, so an agent bump republishes
only that artifact instead of the whole Runner image.

## Approach

### (a) The rootfs derives from the published agent image, by pinned digest

The guest rootfs stops importing `agent-image/toolchain.nix` with root's
`pkgs`. Instead, `guest-image/default.nix` consumes the **published**
`ghcr.io/rigelbuild/compass-agent` image — the exact bytes the container
backends run — as a **fixed-output derivation** pinned by digest, unpacks its
layers, overlays the nix-derived boot layer, and packs the union with the
existing deterministic erofs flags.

Why the published image and not the in-repo nix expression: the divergence to
close is between what the container backends RUN (the published artifact) and
what the guest ships. Re-importing the expression under `agent-image`'s own
lock would align the two nix pins but still prove nothing about the published
bytes. Pinning the published digest guarantees this much, and no more: the
guest rootfs derives from a published, provenance-checked agent image,
reproducibly pinned. It does NOT enforce deploy-time equality — the container
backends run whatever digest the operator deploys (`configSpecBuilder.BuildSpec`
sets `Image: d.Image`, the operator default), the guest runs whatever
`agent-oci.lock` pins, and the lock trails every agent publish by one
pin-bump cycle BY DESIGN. The residual gap is narrow and observable: the
guest artifact's agent-digest provenance annotation (§(b)) names the digest
the rootfs derived from, so `compass-stack` can compare it against the
deployed agent image digest and warn on skew — that warning is in T4's
scope. The pin also gives (b) its decoupling for free: an agent bump
publishes a new agent image, then a pin bump rebuilds and republishes only
the guest artifact.

**Hermeticity (resolves recon question 3).** The image enters the nix build
as fixed-output fetches: one FOD per layer blob, each keyed by the sha256
descriptor digest the pin tool copied out of the pinned manifest. A registry
blob's content IS its descriptor digest, so there is no re-serialization
step between the registry and the fetch key. The build is
offline-reproducible from any substituter, and an out-of-band re-tag cannot
change the bytes — a blob that does not match its descriptor fails closed. A
whole-archive `dockerTools.pullImage`-style fetch was rejected: its narHash
covers the archive AS SKOPEO WRITES IT, and that byte layout is not
guaranteed stable across skopeo/nixpkgs moves, so a root `devenv.lock` bump
could redden the guest gate with ZERO change to the published image.
Consuming the nix2container derivation directly (evaluating `agent-image`'s
devenv container build in-eval) was weighed and rejected — see
§Resolved decisions.

**Reproducibility enforcement (a Global Constraint, not documentation).** The
pin lives in a committed lock file, `guest-image/agent-oci.lock`:

```json
{
  "repo": "ghcr.io/rigelbuild/compass-agent",
  "tag": "git-<sha12>",
  "digest": "sha256:<64 hex>",
  "layers": ["sha256:<64 hex>", "..."]
}
```

Only the pin tool (`tools/guest-image/pin-agent-image.ts`, T1) writes it, and
the tool enforces provenance: the repo MUST be exactly
`ghcr.io/rigelbuild/compass-agent` (the repo `agent-image/publish.sh`
publishes), the tag MUST match `^git-[0-9a-f]{12}$` (the immutable per-push
tag that lane mints), and the digest MUST be what the registry currently
resolves for that tag. An arbitrary registry tag or repo never enters the
lock, so the erofs stays exactly as reproducible as the nix2container artifact
it derives from — bit-stable given the lock. The nix build re-checks the shape
(repo + tag regex) at eval time, so a hand-edited lock fails the gate too.

**The boot layer is still nix-derived and overlays the image.** An agent OCI
image carries none of the boot contract, so the existing nix boot layer is
overlaid on the unpacked image tree, winning on conflicts: the real
`compass-guestd` as `/sbin/init` (and on PATH), the kernel's full
`/lib/modules` tree from the SAME root-pinned kernel derivation the initrd
uses, kmod's modprobe at `/sbin/modprobe` (the request_module helper), and the
writable regular-file `/etc/resolv.conf`. The boot contract is unchanged: the
erofs mounts read-only as the overlay LOWER on `/dev/vda` by the initrd
(modprobing `virtio_pci`, `virtio_blk`, `erofs`, `overlay`), the filesystem
UUID stays fixed, and the pack keeps `-T0 --all-root -U <uuid> --workers=1`
plus the pack-twice `cmp` determinism check. `--all-root` deliberately
discards the agent image's uid-1000 `/nix` ownership: guest processes run as
root (guestd is PID 1), unlike the container backends, so root-owned trees are
correct here and ownership never becomes a determinism variable.

**The load-bearing userland is asserted, not assumed.** `/bin/sh` is
load-bearing (every microVM `Start` spawns `/bin/sh -c` to arm egress —
`go/internal/guestd/supervisor.go` — so a missing one is a total-backend
outage), and the egress arm needs `nft`, `getent`, `awk` on the guest PATH.
Today those ride the toolchain closure. After the cutover they ride the agent
image's layers — the same layers that already satisfy the container egress
contract — and the rootfs derivation adds a **derivation-time contract check**
that fails the build unless the unpacked tree resolves `bin/sh`, `bin/nft`,
`bin/getent`, `bin/awk`, and `bin/compass-agent`. An agent-image regression
that drops one becomes a red guest-image gate, never a boot-time outage.

**Kernel and initrd are out of scope of the derivation change.** Both stay
nix-derived from the root pin exactly as today (the kernel is substituted from
cache.nixos.org; the initrd's module-set check is unchanged).

**Where the derived build runs (resolves recon question 2).** The
`guest-image` moon gate keeps `runInCI: true`. The only network the build
needs is the FOD fetch, and fixed-output derivations are exactly the network
nix sandboxes permit — the same mechanism the file's existing
`builtins.fetchTarball` nixpkgs fetch already uses in that gate. The gate's
`inputs` change shape: `/agent-image/toolchain.nix`,
`/agent-image/entrypoint.nix`, `/packages/compass-agent/**`, `/package.json`,
and `/bun.lock` leave the list (the rootfs no longer reads them);
`agent-oci.lock` joins it. An agent-source change stops rescheduling the
guest build — correct, because it no longer changes the output; the pin bump
that follows the publish does.

**Known constraint: an agent change and its guest rootfs cannot land in one
PR.** The agent image publishes on merge to main, and only a published
digest can enter the lock — so the flow is two merges: the agent change
merges and publishes, then the pin-bump PR merges and drives the guest gate
plus the artifact republish. Between the two merges the guest runs the
previous pin; the latency is one publish plus one Renovate pin-bump cycle.
This is a known structural cost of deriving from the published image, not a
bug. It does NOT block pre-merge CI and is not circular: the gate fetches
the PREVIOUSLY pinned digest, which exists and is public, and the agent
source paths leave the gate's `inputs` (T2), so an agent-only PR does not
reschedule the guest build at all. §Resolved decisions records the weighed
alternative that removes this constraint, and why it lost.

### (b) The guest assets publish as a digest-pinned, non-runnable OCI artifact

A new publish lane (`tools/guest-image/publish.ts` + `publish-core.ts`, T3)
packages the three realised assets as one OCI artifact and pushes it to
`ghcr.io/rigelbuild/compass-guest-image`, reusing the shape the runner-image
lane established: build produces a local OCI layout, a pure unit-tested core
does every mapping and fail-closed check, the push is digest-asserted, and the
deployable reference on stdout is `repo@sha256:…` — a tag is only a
build-addressability handle, because GHCR has no server-side tag immutability.

**Marked as an artifact, not an image.** Nothing can `docker run` an erofs
blob, so the manifest says so structurally: an OCI image manifest with
`artifactType: application/vnd.compass.guest-image.v1`, the OCI 1.1 **empty
config** (`application/vnd.oci.empty.v1+json`, the 2-byte `{}` blob), and
three layers with dedicated media types —
`application/vnd.compass.guest-kernel.v1` (the bzImage),
`application/vnd.compass.guest-rootfs.v1+erofs`, and
`application/vnd.compass.guest-initrd.v1+cpio.zst`. A runtime that is handed
this reference refuses at the config media type; no runnable config exists to
misuse. Annotations carry provenance: the source commit
(`org.opencontainers.image.revision`), the agent image digest the rootfs
derived from, and one `sha256:<hex>` annotation per layer basename — the same
sha256sum-format facts `--microvm-image-manifest` consumes, so a materialiser
can write the Runner's manifest file straight from the manifest it pulled.

**Transport.** The lane assembles the OCI layout itself (blobs + manifest +
index are plain files and JSON — pure, unit-testable core) and pushes with the
`skopeo` the root dev shell already provisions
(`inputs.nix2container...skopeo-nix2container`, `devenv.nix`), via
`skopeo copy oci:<layout> docker://<repo>:git-<sha12>`. The lane keeps the
agent-image lane's `:git-<sha12>` immutability guard (proceed / skip-identical
/ abort-on-ambiguity) and the publish-lane posture of scanning what ships
before pushing — here the config is empty and the layers are opaque binaries,
so the scan runs the name-pattern check over the annotation set. Publishing
runs in a workflow job (network + GHCR credentials), never in the moon gate —
`runner-image/moon.yml`'s no-`ci`-task posture is respected; the guest lane's
gate cost stays the existing `guest-image:build`.

### The Runner contract does not change (ruled)

The Runner keeps taking three FILE PATHS — `--microvm-kernel` /
`--microvm-rootfs` / `--microvm-initrd`, verified on disk (and hash-verified
against `--microvm-image-manifest` when set) by
`verifyImages` in `go/internal/runtime/microvm_preflight.go`. That is already
a deploy-time seam and works identically on a laptop, a bare host, and a
cluster. No registry client, pull credentials, or image cache enters the Go
runtime. The artifact is ADDITIVE: `tools/runner-image/build.ts` keeps
realising and baking the three assets, so the baked-into-the-Runner-image
materialisation stays fully supported — the air-gapped path, and the zero-step
default for a cluster deployment.

**Cross-copy divergence is an operability trap, named out loud.** After (b),
two copies of the guest assets exist: one baked into the Runner image, one
published as the artifact — and nothing detects divergence between them.
`verifyImages` (`go/internal/runtime/microvm_preflight.go`) binds a triple
to its sha256sum manifest, but the manifest travels WITH each copy, so a
Runner image baked at commit A plus a materialised artifact from commit B
verify clean on BOTH sides while the operator believes they match. Only one
copy is live per Runner process, so this is not a correctness bug — but it
reproduces this record's own two-sources-of-truth problem at the asset
level. Cheap mitigations, folded into the plan: the Runner image build
stamps the image with the digest of the guest assets it baked;
`compass-stack` warns when a baked copy is present but a different artifact
digest is materialised (T4); and the runbook states one deployment uses one
materialisation source (T5).

### Materialisation is a per-deployment strategy (resolves recon question 1)

`compass-stack` today does NOT assume the Runner image carries the guest
assets — it never sets them at all: `go/internal/stack/spec.go`'s
`runnerSpec` passes no backend and no microVM flags, and the only microVM
touchpoint under `go/cmd/compass-stack` is `preflight.go`'s host-capability
probe. So self-hosted materialisation is net-new work in the stack lane (T4),
not a change to an existing assumption.

The **core strategy**: when a deployer selects the microVM backend,
`compass-stack up` materialises the artifact — resolve the digest-pinned
reference, fetch the manifest and three blobs (two anonymous HTTPS GETs per
blob against the OCI distribution API; a bounded fetch helper in the stack
package, not a registry client in the Runner), verify each blob's sha256
against its descriptor, atomically rename into a content-addressed state dir
(`<state>/guest-image/<digest>/`), write the sha256sum manifest file, and pass
the three paths plus `--microvm-image-manifest` to the runner spec. A
re-`up` with the same digest is a no-op (the content-addressed dir already
exists and verifies).

The **air-gapped strategy**: `--microvm-guest-dir <path>` points the stack at
a pre-materialised directory (files carried from the Runner image, a release
bundle, or an offline transfer) and skips all fetching. No pull ever becomes
mandatory.

The **seam an operator of a fleet extends**: because the Runner consumes
paths, any deployment mechanism that puts verified files on the node before
the Runner starts — baking them into the Runner image, a node pre-load, an
init step — satisfies the same contract with zero Runner changes. That
extension is a deployment concern and is not designed here.

## Alternatives considered

- **Per-workload agent images under microVM — ruled OUT on evidence.** Nothing
  can request one: `configSpecBuilder.BuildSpec` sets `Image: d.Image` (the
  operator default, `go/internal/runner/spec.go`), and
  `ProvisionAgentWorkspaceRequest` (`proto/compass/v1/compass.proto`) carries
  only `agent_handle` — there is no image field on the request. The backend
  would have nothing to apply either: `Create` assembles the BootConfig from
  the operator-configured kernel/rootfs/initrd paths and never reads
  `spec.Image` (`go/internal/runtime/microvm_lifecycle.go`), so the guest
  agent comes from the baked rootfs rather than a per-request pull. A
  per-workload pull would add pull credentials, a per-node conversion step,
  and a disk cache with eviction — to serve a knob every caller sets
  identically. The guest agent stays pinned per deployment by the rootfs the
  Runner was given.
- **Re-importing `agent-image`'s nix expression under its own lock.** Closes
  the nixpkgs-pin divergence but not the real one: the container backends run
  the published artifact, and two builds of "the same" expression at
  different times still skew. Rejected for the published-digest FOD.
- **Consuming the nix2container derivation in-eval.** Stronger than a flat
  rejection: hermetic, no registry round-trip, and it lets an agent change
  and its guest rootfs land ATOMICALLY in one PR — removing the two-merge
  constraint in §(a) entirely. Its real cost is the agent-image closure (the
  dominant CI cost, ~90 minutes cold) entering the pre-merge guest gate.
  Weighed as a genuine fork and ruled against: the pinned digest keeps the
  gate's cost flat and the pin reviewable (see §Resolved decisions).
- **A runnable OCI image carrying the assets as ordinary layers.** Pullable by
  any engine, but invites `docker run` and misstates what it is; the empty
  config + artifactType manifest is strictly clearer and equally pullable.
  Rejected.
- **Replacing the baked Runner-image assets with mandatory artifact pull.**
  Breaks air-gapped self-hosted deployments outright. Rejected; the artifact
  is additive.

## Global Constraints

- **Air-gapped self-hosted must keep working with no registry access.** The
  baked-into-the-Runner-image materialisation and the `--microvm-guest-dir`
  bypass are supported paths, not fallbacks. No task may make a pull
  mandatory.
- **Reproducibility is enforced, not documented.** The rootfs OCI input is
  ONLY the nix2container artifact from the `compass-agent` publish lane:
  `guest-image/agent-oci.lock` is written only by the pin tool, which rejects
  any repo other than `ghcr.io/rigelbuild/compass-agent` and any tag not
  matching `^git-[0-9a-f]{12}$`; the nix eval re-checks the shape; the
  per-layer fixed-output fetches pin the bytes to the lock's descriptor
  digests. The erofs pack keeps `-T0 --all-root -U <fixed uuid>
  --workers=1` and the pack-twice `cmp` check.
- **No registry client, pull credentials, or image cache in the Go runtime.**
  The Runner's microVM contract stays three file paths plus the optional
  sha256sum manifest, verified by `verifyImages` in
  `go/internal/runtime/microvm_preflight.go`. Fetching lives in the stack
  (deploy time) and in publish tooling only.
- **The boot contract is frozen.** erofs lower on `/dev/vda`, initrd modprobe
  set (`virtio_pci`, `virtio_blk`, `erofs`, `overlay`, plus the runtime set),
  fixed filesystem UUID, `/sbin/init` = `compass-guestd`, and a present
  `/bin/sh` (total-backend outage if missing — asserted at derivation time,
  with `nft`/`getent`/`awk`/`compass-agent`).
- **Scripts with real logic are TypeScript run by bun, never bash** (the
  `no-bash-gate` CI task); `: any` / `as any` are banned. Pure cores split
  from I/O shells, unit-tested, mirroring `tools/runner-image/publish-core.ts`.
- **Publishing never enters the moon gate.** The publish lane runs in a
  workflow job with network + GHCR credentials; `runner-image/moon.yml`'s
  deliberate no-`ci`-task posture stands.
- **Version floor: skopeo with OCI 1.1 artifact support** (empty-config
  artifact manifests round-trip through `skopeo copy`) — satisfied by the
  `skopeo-nix2container` package the root dev shell pins; the publish lane
  asserts the copy's digest, so a transport that mutated the manifest fails
  closed.
- **Digest is the deployed contract.** Tags (`:git-<sha12>`, `:latest`) are
  build-addressability handles only; everything that consumes the artifact
  pins `repo@sha256:…`.

## Plan

### T1 — Agent-image pin: `agent-oci.lock` + pin tool

Add `guest-image/agent-oci.lock` (repo, tag, digest, layers — the JSON shape
in §(a)) and `tools/guest-image/pin-agent-image.ts` with a pure core
(`pin-core.ts`): validate repo equality and tag shape, parse the skopeo
inspect output, copy the manifest's layer descriptor digests into the lock,
compute the lock delta, refuse a digest the registry does not currently
resolve for the tag. The I/O shell drives `skopeo inspect --raw
docker://<repo>:<tag>` and rewrites the lock.

**Renovate lockstep.** The precedent is NOT the digest-only
`DefaultPostgresImage` pin (`go/internal/stack/postgres_image.go`), where a
regex rewrite completes the update. This lock carries fetch-key fields (the
layer descriptor set) that Renovate cannot compute: a bare `customManager`
bump would move tag + digest and leave the descriptors describing the OLD
manifest, so the fixed-output fetches would fail on EVERY Renovate PR. The
correct precedent is the devenv-lock pair — a `customManager` PLUS a
`postUpgradeTasks` relock (`tools/renovate/refresh-devenv-lock.ts`, whose
header names this exact failure class: a rev-only rewrite moves the rev and
nothing else, leaving the paired fields stale). Add a solo-branch
packageRule in the `config.json5` devenv-fork shape (own groupName,
`executionMode: "branch"`, `fileFilters` naming exactly
`guest-image/agent-oci.lock`) whose postUpgradeTask reruns the pin tool in a
self-gating relock mode after the regex bump, plus the paired
`bot-config.json5` allowlist entry.

This task is honestly TWO concerns and is sized as such: the pin tool, and
the Renovate lockstep — which rides the repo's Renovate subsystem and
carries that subsystem's own heavy test conventions
(`tools/renovate/*.test.ts`).

Interfaces: `guest-image/agent-oci.lock` (JSON: `{ repo: string; tag: string;
digest: string; layers: string[] }`); `tools/guest-image/pin-core.ts`
exporting `validatePin(lock: unknown): PinLock` (throws on shape/provenance
violation) and `lockFromInspect(repo: string, tag: string, manifest:
unknown): PinLock`; CLI `bun tools/guest-image/pin-agent-image.ts --tag
git-<sha12>` plus a `--relock` mode (self-gates: exits 0 when the lock is
already consistent, the `refresh-devenv-lock.ts` posture); Renovate: a
`customManagers` regex entry over `guest-image/agent-oci.lock` on a `docker`
datasource tracking `ghcr.io/rigelbuild/compass-agent`. Detection keys on the
moving `:latest` digest, because the pinned tag is a per-commit-immutable
`git-<sha12>` that no datasource can order; the relock then derives the
immutable tag from it, which is sound because the publish lane asserts
`:latest` and `:git-<sha12>` share a config digest and fails closed otherwise.
Plus a solo-branch packageRule with `postUpgradeTasks.commands: ["bun
tools/guest-image/pin-agent-image.ts --relock"]` and `fileFilters:
["guest-image/agent-oci.lock"]`, and the matching `bot-config.json5`
`allowedCommands` entry.

Test cycle: unit tests over `pin-core.ts` (provenance rejections: wrong repo,
non-`git-` tag, malformed digest, digest/tag mismatch; descriptor
extraction; happy-path lock rendering); Renovate lockstep unit tests
following the `refresh-devenv-lock.test.ts` conventions (rule shape pinned
in `config.json5`, allowlist pairing asserted, relock self-gate). One live
smoke run of the CLI against the current published tag, output committed as
the initial lock.

### T2 — Derive the rootfs from the pinned OCI image

Rework `guest-image/default.nix`: drop the `../agent-image/entrypoint.nix` +
`../agent-image/toolchain.nix` imports; add per-layer fixed-output fetches
of the locked image's blobs (manifest digest + `layers` descriptor digests
from `agent-oci.lock`, eval-time shape re-check),
layer unpack into a rootfs tree, the derivation-time userland contract check
(`bin/sh`, `bin/nft`, `bin/getent`, `bin/awk`, `bin/compass-agent`), and the
boot-layer overlay (guestd `/sbin/init` + PATH link, `/lib/modules` from the
root-pinned kernel, `/sbin/modprobe`, writable `/etc/resolv.conf`) winning on
path conflicts. Keep the erofs pack flags, fixed UUID, and pack-twice `cmp`
verbatim. Update `guest-image/moon.yml` inputs: remove the agent-source
entries, add `agent-oci.lock`. Kernel and initrd derivations unchanged.

Interfaces: `guest-image/default.nix` attrs unchanged
(`compass-guest-kernel`, `compass-guest-rootfs`, `compass-guest-initrd`; the
moon `build` command is byte-identical); consumes
`guest-image/agent-oci.lock`; `guest-image/moon.yml` `tasks.build.inputs`
gains `agent-oci.lock`, drops `/agent-image/toolchain.nix`,
`/agent-image/entrypoint.nix`, `/packages/compass-agent/**`, `/package.json`,
`/bun.lock`. Fetch mechanism, decided here: per-layer fixed-output blob
fetches keyed by the lock's `layers` descriptor digests — chosen over a
whole-archive pull because an archive narHash tracks skopeo's byte layout
and can drift on a root `devenv.lock` bump with zero change to the
published image; per-layer descriptor fetches cannot (§(a) Hermeticity).

Test cycle: the derivation-time contract check and determinism `cmp` run on
every build; `nix build -f default.nix compass-guest-rootfs` twice proves
FOD-substituted rebuild stability; the existing `ci / microvm` KVM boot leg
boots the derived rootfs end to end (egress arm exercises `/bin/sh` + `nft`),
which is the load-bearing proof the userland contract survived the cutover.

### T3 — Guest artifact publish lane

Add `tools/guest-image/publish.ts` + `publish-core.ts` + `moon.yml` wiring
under a new `guest-image` publish workflow job. The pure core maps three
realised asset paths to an OCI layout plan: empty config
(`application/vnd.oci.empty.v1+json`), `artifactType:
application/vnd.compass.guest-image.v1`, three layer descriptors with the
§(b) media types and per-layer sha256 annotations, provenance annotations
(source sha, agent image digest read from `agent-oci.lock`), and the
name-pattern scan over annotations (reusing `SECRET_NAME_PATTERN`'s shape
from `tools/runner-image/publish-core.ts`). The shell realises the three
assets via the existing `guest-image:build` command, writes the layout,
runs the `:git-<sha12>` immutability guard (skopeo inspect; proceed /
skip-identical / abort-on-ambiguity, the `agent-image/publish.sh` posture),
pushes with `skopeo copy oci:<layout> docker://<repo>:git-<sha12>`, asserts
the pushed digest equals the local manifest digest, and prints
`repo@sha256:…` on stdout.

**Trigger and gating.** The publish job runs on push to main in
`.github/workflows/release.yml`, gated by a changed-path set
`GUEST_IMAGE_CLOSURE_PATHS` (the analog of the existing
`IMAGE_CLOSURE_PATHS` env in that workflow): `guest-image/**` and
`tools/guest-image/**`. Expected same-push skew, stated out loud so an
executor does not stop on it: a main push that publishes a NEW agent image
publishes a guest artifact built from the OLD lock. That is consistent with
the two-merge flow in §(a); the pin-bump merge that follows republishes the
artifact against the new agent digest.

Interfaces: CLI `bun tools/guest-image/publish.ts --repo
ghcr.io/rigelbuild/compass-guest-image --sha <sha12>`;
`tools/guest-image/publish-core.ts` exporting `layoutPlan(assets: {
kernel: string; rootfs: string; initrd: string }, provenance: { revision:
string; agentImageDigest: string }): LayoutPlan`, `manifestDigest(layout:
string): string`, `annotationViolations(annotations: Readonly<Record<string,
string>>): string[]`, and numbered `EXIT` codes; stdout contract: the digest
reference, nothing else.

Test cycle: unit tests over `publish-core.ts` (media-type/annotation mapping,
digest math against fixture blobs, immutability-guard state table, exit-code
edges); one dev-box dry run producing a local layout verified with `skopeo
inspect --raw oci:<layout>` (asserts artifactType + empty config — the
"nothing can run this" property); and a one-time scratch-tag push of that
layout to GHCR proving two things the local layout cannot: GHCR accepts the
empty-config artifact manifest form at push, and the pinned skopeo
round-trips `artifactType` without rewriting it. Live GHCR acceptance is
UNVERIFIED at design time — reasoned from the OCI 1.1 spec and registry
docs only, no `read:packages` scope was available — so this dry run is
where it gets proven, before the production publish job ever runs.

### T4 — `compass-stack` microVM materialisation

Net-new stack-lane work (the stack today sets no backend and no guest paths).
Extend `go/internal/stack`: `Config` gains `RuntimeBackend string`,
`GuestArtifact string` (a `repo@sha256:…` reference), and `GuestDir string`
(the air-gapped bypass; mutually exclusive with `GuestArtifact`). A new
`materialize.go` fetches the artifact manifest and three blobs from the OCI
distribution API by digest (anonymous GET; bounded, retried; no engine, no
cache beyond the content-addressed dir), verifies each blob sha256 against
its descriptor, writes `<stateDir>/guest-image/<digest>/{kernel,rootfs.erofs,
initrd,manifest.sha256}` via temp-file + atomic rename, and is a verified
no-op when the dir already exists. `runnerSpec` appends `--backend microvm`,
the three `--microvm-*` path flags, and `--microvm-image-manifest` when the
backend is microVM; the existing zero-value-omit guarantee holds (a
non-microVM config produces byte-identical Args to today). `compass-stack`
CLI grows the matching flags; `preflight.go`'s `MicroVMFloors` probe gates
selection as today.

Two skew warnings ride this task. (1) Agent-digest skew: after
materialising, compare the artifact's agent-digest provenance annotation
against the deployed agent image digest and warn on mismatch — §(a)'s
residual gap, made observable. (2) Cross-copy skew: when the Runner image
carries a baked guest-asset digest stamp and a different artifact digest is
materialised, warn that the baked and materialised copies diverge. The
stamp itself is a small `tools/runner-image/build.ts` + Dockerfile ENV
addition (a guest-asset digest beside the existing `GUEST_*` ENVs), carried
in this task so the writer and the reader land together.

Interfaces: `stack.Config{ RuntimeBackend, GuestArtifact, GuestDir string }`;
`func materializeGuestImage(ctx context.Context, ref string, stateDir string)
(GuestPaths, error)` with `type GuestPaths struct { Kernel, Rootfs, Initrd,
Manifest string }`; `runnerSpec(cfg Config, cert CertResult, token string)
ProcessSpec` (existing signature, new conditional args); CLI flags
`--runtime-backend`, `--guest-artifact`, `--guest-dir`.

Test cycle: unit tests for `materializeGuestImage` against an `httptest`
registry stub (happy path, blob digest mismatch fails closed, partial fetch
leaves no files at the final paths, second call is a verified no-op);
`runnerSpec` table tests (microvm arg set; non-microvm byte-identical;
`GuestDir` bypass produces the same paths with no fetch); one manual
`compass-stack up` smoke on a KVM dev box driving the Runner's own
`verifyImages` + boot canary against the materialised paths.

### T5 — Docs and deployment story

Document the two materialisation strategies and the bump flow: self-host
guide section (pull-by-digest default, `--guest-dir` air-gapped runbook
including how to extract the assets from the Runner image), and the
agent-bump runbook (publish agent image → Renovate pin PR → guest-image
gate + artifact republish → deployments advance the digest), replacing any
prose that describes the toolchain-import derivation. Update
`guest-image/default.nix`'s header comment and the runner-image comments that
describe the old coupling.
The runbook states two rules out loud: one deployment uses ONE
materialisation source (baked or artifact, never both — the cross-copy
divergence trap in §Approach), and a red guest gate on a fetch-hash
mismatch is recovered by rerunning the pin tool and committing the
refreshed lock.

Interfaces: `docs/**` prose only plus in-tree comments; no code. Exact pages:
the self-host deployment doc that owns `compass-stack` usage, and
`guest-image/default.nix` / `runner-image/moon.yml` header comments.

Test cycle: `rumdl check .` green; `moon run orion-ref-gate:check` green; a
reviewer follows the air-gapped runbook steps against the T4 `--guest-dir`
flag on a dev box.

## Tasks

- [ ] T1 — `guest-image/agent-oci.lock` + `tools/guest-image/pin-agent-image.ts`
  (pure core + provenance rejections + Renovate customManager PLUS
  postUpgradeTasks relock, the devenv-lock pair; sized as two concerns);
  pin + lockstep unit tests + live smoke producing the initial lock.
- [ ] T2 — `guest-image/default.nix` derives the rootfs from the pinned OCI
  image (FOD pull, unpack, userland contract check, boot-layer overlay,
  unchanged erofs determinism); moon inputs rewired; proven by the
  `ci / microvm` boot leg.
- [ ] T3 — `tools/guest-image/publish.ts` + `publish-core.ts` artifact lane
  (empty-config OCI artifact, immutability guard, digest assert, annotation
  scan; `GUEST_IMAGE_CLOSURE_PATHS`-gated main-push job); unit tests +
  local-layout `skopeo inspect` dry run + one scratch-tag GHCR push.
- [ ] T4 — `compass-stack` microVM materialisation (`Config` knobs,
  `materializeGuestImage`, `runnerSpec` extension, `--guest-dir` bypass,
  agent-digest + cross-copy skew warnings, Runner-image guest-asset digest
  stamp); httptest-stub unit tests + KVM dev-box smoke.
- [ ] T5 — Docs: strategies, air-gapped runbook, bump flow, one-source rule,
  pin-recovery step; stale comments updated; gates green.

## Open Questions

- **OQ1 (non-load-bearing): referrers linkage.** Should the guest artifact
  also set OCI `subject` to the agent image manifest, so registries with the
  referrers API can enumerate guest artifacts derived from an agent image?
  The provenance annotation already carries the digest; `subject` adds
  discoverability only. Default if unanswered: annotation only, no `subject`.
- **OQ2 (non-load-bearing): private-mirror credentials for the stack fetch.**
  T4 fetches anonymously from a public repo. A deployer mirroring the
  artifact into a private registry would need credential plumbing
  (`REGISTRY_AUTH_FILE`-shaped). Air-gapped deployers use `--guest-dir`, so
  nothing blocks on this. Default if unanswered: defer until a deployer asks.
- **OQ3 (non-load-bearing): retiring the baked assets from the Runner image.**
  Once artifact materialisation is proven in production deployments, the
  multi-GB baked copy could become optional to shrink the Runner image. That
  is a later record's call; this record keeps baking unconditionally
  (air-gapped Global Constraint).

## Resolved decisions

- **Recon Q1 — does `compass-stack` assume the Runner image carries the guest
  assets?** No. It sets no backend and no guest paths (`runnerSpec`,
  `go/internal/stack/spec.go`); its only microVM touchpoint is the
  `preflight.go` host-capability probe. Materialisation is net-new stack work
  (T4), sized as its own task.
- **Recon Q2 — where does the derived-erofs build run?** In the existing
  `guest-image` moon gate (`runInCI: true`): the OCI input arrives via a
  fixed-output derivation, the one network access nix builds permit, so the
  gate needs no new capability.
- **Recon Q3 — how is the derivation kept hermetic?** Per-layer fixed-output
  fetches keyed by the lock's descriptor digests (manifest digest pins the
  set).
- **The agent source: the published image, pinned by digest (ruled).** The
  rootfs derives from the published agent image through the pinned
  fixed-output fetch, not from the `agent-image` derivation in-eval. The
  in-eval alternative is hermetic and would let an agent change and its guest
  rootfs land atomically in one PR, but it drags the agent-image closure into
  every pre-merge guest gate run, and that closure is already the dominant CI
  cost. The pinned digest keeps the gate's cost flat and the pin reviewable as
  a one-line diff. The accepted cost is the two-merge flow in §(a): an agent
  change merges and publishes before the pin bump that picks it up.
- **Per-workload guest agent images** stay ruled out (see
  §Alternatives considered).
- **The Runner's three-file-path contract** stays frozen (see §Approach).
