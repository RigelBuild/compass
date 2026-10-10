# Guest-image agent pin: multi-arch index

Tracking: RIG-5044

> **Design record.** Paths cite `RigelBuild/compass` at `origin/main` as of
> 2026-10-10. Matt ruled on RIG-5044 (2026-10-10, option B): "We need to
> support both arches". This record designs how. It refines the lock schema of
> the [guest image artifact record](../compass-guest-image-artifact/design.md);
> DL-368 stays Active.

## Problem / Intent

PR #1775 (merged 2026-10-07) made every consumer-facing `compass-agent` tag
an OCI index of `linux/amd64` + `linux/arm64` (DL-366). The guest pin cannot
hold an index. `layerDigests` in `tools/guest-image/pin-core.ts` refuses one:
"an index would need a platform choice the lock cannot record". So the lock
stays on the last pre-index build, `git-7d22c69390cb`, and the bump path is
broken:

- Renovate PR #2042 rewrote the lock `digest` to `sha256:4166133e…`. The
  registry serves that digest as an `application/vnd.oci.image.manifest.v1+json`
  manifest whose config says `{"architecture":"amd64","os":"linux"}`. It is the
  amd64 *member* of `:latest`, not `:latest` itself.
- The relock then fails (PR #2074 fixes its auth step; the index refusal
  remains), and the gate fails with "agent-oci.lock layers do not match the
  manifest it pins".

Intent: the lock pins the index and one member per platform. The guest build
selects the member for the build host. The Renovate relock is green again.

## Approach

### Lock schema

The lock keeps its provenance fields and replaces the single `layers` list
with a per-platform map:

```json
{
	"repo": "ghcr.io/rigelbuild/compass-agent",
	"tag": "git-<sha12>",
	"digest": "sha256:<index digest>",
	"platforms": {
		"linux/amd64": {
			"manifest": "sha256:<member manifest digest>",
			"layers": ["sha256:<layer>", "…"]
		},
		"linux/arm64": {
			"manifest": "sha256:<member manifest digest>",
			"layers": ["sha256:<layer>", "…"]
		}
	}
}
```

`digest` becomes the index digest. Three readers depend on that:

- `tools/guest-image/publish.ts` copies `lock.digest` into the
  `org.compass.guest.agent-image-digest` annotation, unchanged. A bare-tag
  deployment (`docs/self-host-guest-image.md`) resolves the index digest.
- Renovate 44.46.2's `getDigest` picks a same-architecture member only when
  the current digest is an image manifest, hence PR #2042. With an index
  digest it returns the `:latest` index digest.
- The Renovate regex `"digest": "(?<currentDigest>sha256:…)"` must match
  once, so the member field is `manifest`, never `digest`.

Member `layers` stay in the lock: DL-368 keys fetches on "the lock's
descriptor digests", and a bump PR keeps a reviewable layer diff.

### Validation

Every check fails closed. pin-core `validatePin` guards the write path and
`checkedLock` the eval. Both check the lock: `repo` and `tag` as today
(DL-368 provenance), `digest` is `sha256:<64 hex>`, `platforms` has exactly
`linux/amd64` and `linux/arm64`, and each member has a `manifest` digest and
a non-empty, ordered `layers` list. A v1 lock has no map and is a bad pin.

The pin tool checks every member and its config against the registry:

- The top level is `application/vnd.oci.image.index.v1+json`.
  `publishImageIndex` in `tools/agent-image-index/index.ts` pushes with
  `"podman", "manifest", "push", "--format", "oci"`.
- The sorted `os/architecture` list equals the publish lane's
  `const EXPECTED_PLATFORMS = "linux/amd64,linux/arm64";` (same file), so a
  duplicate entry fails.
- Each member descriptor is an image manifest, never a nested index.
- Each member body hashes to its digest and passes `layerDigests`.
- Each member's config `os`/`architecture` equals its platform, repeating
  `verifyMemberConfig`, because `publishImageIndex` calls
  `verifyPublishedIndex` only after the push.

The nix eval checks the index platform set and the selected member only,
and fetches no config blob: the member manifest is fetched by digest, so its
config descriptor is the one the pin tool checked.

- The index's sorted platform list matches, compared as strings.
- The selected platform's index entry digest equals the lock's `manifest`.
- That manifest's layers equal the lock's `layers`, with the error text
  "layers do not match the manifest it pins" kept for the recovery doc.

Digests hash the exact `--raw` bytes, as `inspect` does today. On 2026-10-10
the live index and both member bodies each hashed to their digest exactly.

### Pin tool resolution

`--tag git-<sha12>`:

1. `inspect` the tag. The local hash of the raw body is the index digest.
2. `indexMembers` checks the index and returns the two platform descriptors.
3. For each member, `inspect` `repo@<member digest>` and run `checkMember` on
   its body hash and manifest.
4. For each member, run `skopeo inspect --no-creds --config` on
   `repo@<member digest>` and pass the result to `checkMemberConfig`.
5. `lockFromIndex` builds the lock. `locksEqual` self-gates and `renderLock`
   writes it.

`--relock` finds the tag as today, newest `git-<sha12>` first, then runs the
`--tag` steps. Discovery compares raw-body hashes and `resolvedDigest` is
deleted. Its `{{.Digest}}` already hashed the index, so results match; the
switch drops a full tag listing per probe and skopeo's host-member fetch,
which fails on a host with no member.

Two-tag coherence compares index digests (DL-366), so the "share a config
digest" comments in `tools/guest-image/pin-agent-image.ts` and
`tools/renovate/config.json5` are corrected. Reads go through the `skopeo()`
helper, which PR #2074 gives `--no-creds`.

### Nix member selection

The agent member's architecture must equal that of the nix boot layer over
it (guestd, kernel modules, kmod). So the member is chosen by
`pkgs.stdenv.hostPlatform.system` of the `pkgs` that builds the boot layer,
never `builtins.currentSystem`; a `guest-image/default.nix` comment says so.
The mapping follows `nativeBySystem` in `agent-image/entrypoint.nix`, which
keys `"x86_64-linux"` and `"aarch64-linux"` and ends in
`or (throw "compass-agent entrypoint: unsupported system ${system}")`:

```nix
ociPlatformBySystem = {
  "x86_64-linux" = "linux/amd64";
  "aarch64-linux" = "linux/arm64";
};
platformFor =
  system:
  ociPlatformBySystem.${system}
    or (throw "guest-image: no agent image member for ${system}; agent-oci.lock pins linux/amd64 and linux/arm64 only");
```

Any other system throws at eval, and only when the rootfs is forced.

The fetch chain becomes index, member, layers:

1. `agentIndex`: `fetchurl` of `manifests/<lock digest>` with
   `Accept: application/vnd.oci.image.index.v1+json` and `hash = digest`.
2. `memberDigest`: checks the index and returns the lock member's `manifest`.
3. `agentManifest`: the existing fetch, keyed on the member digest.
4. `lockedLayers`: the existing compare, against the member's `layers`.

Only the selected member is fetched. The pure checks move to a new file,
`guest-image/agent-pin.nix`, taking `{ lib }` and parsed JSON, so every
check is testable on any host.

Fixtures cannot prove the wiring, so T3 adds an aarch64 eval smoke. Its one
seam: `guest-image/default.nix` takes `{ crossSystem ? null }` for its
nixpkgs import, so boot layer and member move together; `nix build -f`
callers are unchanged. `--eval-system aarch64-linux` fails on an x86 host
("Required system: 'aarch64-linux'", 2026-10-10), because the devenv-nixpkgs
import builds its patched source for the eval system. Through `crossSystem`
it gave `hostPlatform.system` `aarch64-linux` and the arm64 member digest as
the fetch `outputHash`. Building and booting arm64 stay in T5.

### Renovate

The regex, `currentValueTemplate: "latest"`, the solo group and the relock
task do not change; their comments now say "index digest". A new lockstep
test runs the regex over the committed `guest-image/agent-oci.lock`, like
its `devenv.lock` and `go/internal/stack/postgres_image.go` neighbours, and
requires exactly one match, equal to the top-level `digest`. On a bump,
Renovate proposes the new index digest and `--relock` rewrites the rest.

### Migration

Both modes validate the existing lock first, so a v1 lock blocks them. The
implementation PR deletes it and runs `--tag` with the tag `:latest` points
at (`publish-image-manifest` prints ":latest matches :git-<sha12>").
`git-7d22c69390cb` has no index, so this also bumps the agent; the
guest-image build and `microvm` boot leg must pass. Close PR #2042 if
Renovate does not replace it.

### Scope

In scope: the lock, the pin tool, nix member selection, Renovate, tests and
docs. After this, `guest-image/default.nix` evaluates for `aarch64-linux`
and selects the arm64 member (T3 eval smoke).

Out of scope, named as T5: booting and publishing an aarch64 guest. Each
piece is x86-shaped today:

- The kernel asset: nixpkgs builds `bzImage` on x86 and `Image` on aarch64
  (upstream `pkgs/os-specific/linux/kernel/build.nix`). `ASSET_FILENAMES` in
  `tools/guest-image/publish-core.ts` (`kernel: "bzImage"`),
  `go/internal/stack/materialize.go` (`annotationName: "bzImage"`),
  `.github/workflows/ci.yml` (`COMPASS_TEST_GUEST_KERNEL="$kernel/bzImage"`)
  and `guest-image/default.nix` assume it.
- `go/internal/runtime/microvm/launch.go` appends `console=ttyS0`.
- `bootModuleConfigs` in `guest-image/default.nix` is checked against the
  x86 config only.
- Artifact addressing is a design fork: a per-arch tag, or an index of guest
  artifacts, which makes `validateGuestManifest` in
  `go/internal/stack/materialize.go` ("want exactly %d (kernel, rootfs,
  initrd)") and `--guest-artifact` platform-aware.
- `publish-guest-image` and the `microvm` job run on `ubuntu-latest`;
  `tools/toolchain/microvm-vmm-env.nix` is untried on aarch64. [INFERENCE]
  Hosted arm64 runners may lack `/dev/kvm`.
- `flake.nix` declares `systems = [ "x86_64-linux" ]`.

### Decision ledger

DL-445 records this and refines two Active decisions; neither file changes.

- DL-368 §(a), the lock schema. The rootfs is still "pinned by digest …
  keyed by the lock's descriptor digests"; provenance and boot rules hold.
  The single-manifest rule lived only in a `tools/guest-image/pin-core.ts`
  comment. The userland contract check covers `linux/amd64` only until an
  arm64 build leg exists.
- DL-366: "every puller relies on engine platform negotiation and no
  consumer changes". DL-445 scopes that to bare-tag engine pullers; the
  digest-pinned guest image needs this lock change.

## Alternatives considered

- **Pin one member with a `platform` field (option A).** An arm64 guest would
  get an amd64 userland or nothing. Matt ruled B.
- **Stop Renovate tracking the pin (option C).** The pin goes stale silently.
- **Pin the per-arch `:git-<sha12>-<arch>` tags.** DL-366 makes them
  internal, and Renovate would track two deps.
- **Lock only the index digest; derive the rest in nix.** It drops the
  descriptor digests DL-368 names as fetch keys, and the layer diff.
- **Annotate the artifact with the member digest.** A bare-tag deployment
  resolves the index digest, so a skew compare always mismatches. Adding it
  beside the index digest is deferred: nothing reads the annotation yet.

## Global Constraints

- **Fail closed.** Any shape, platform-set, media-type, hash or config
  disagreement is an error with a numbered exit code. Never fall back to one
  member.
- **The platform set is exactly `linux/amd64` + `linux/arm64`,** mirroring
  `EXPECTED_PLATFORMS`.
- **Anonymous reads.** skopeo carries `--no-creds` (PR #2074); nix fetches
  send GHCR's anonymous bearer `QQ==`, as today.
- **Provenance unchanged.** Repo and tag rules stay as DL-368 sets them.
- **Nix checks the selected member only,** never a config blob.
- **Only the pin tool writes the committed lock.** Tests never touch it.
- **Boot contract unchanged.** Existing attr names, the moon `build`
  command, and the kernel and initrd derivations stay the same.
- **Bun TypeScript only** (the no-bash-gate rule), pure core apart from I/O.
- **Public repo.** Follow `docs/designs/CONTRIBUTING.md`.
- **One PR for T1–T4, after PR #2074.** They share one lock format, so any
  split leaves main red or the new errors undocumented.

## Plan

### T1 — pin-core v2

Edit `tools/guest-image/pin-core.ts`. Replace `PinLock` and `lockFromInspect`.
Extend `validatePin`, `locksEqual` and `renderLock`. `layerDigests` keeps its
checks; its index error now names the member that failed.

Interfaces:

```ts
export const PLATFORMS = ["linux/amd64", "linux/arm64"] as const;
export type Platform = (typeof PLATFORMS)[number];
export type PinMember = { manifest: string; layers: string[] };
export type PinLock = {
	repo: string;
	tag: string;
	digest: string; // the index digest
	platforms: Record<Platform, PinMember>;
};
export type IndexMember = { platform: Platform; digest: string };

export function validatePin(value: unknown): PinLock;
export function indexMembers(index: unknown): IndexMember[]; // PLATFORMS order
export function checkMember(
	member: IndexMember,
	bodyDigest: string,
	manifest: unknown,
): PinMember;
export function checkMemberConfig(member: IndexMember, config: unknown): void;
export function lockFromIndex(
	repo: string,
	tag: string,
	indexDigest: string,
	members: Record<Platform, PinMember>,
): PinLock;
export function locksEqual(a: PinLock, b: PinLock): boolean;
// Key order: repo, tag, digest, platforms (PLATFORMS order), manifest, layers.
export function renderLock(lock: PinLock): string;
```

Exit codes: index, member and config faults are `EXIT.registryFailed`, as in
`layerDigests`, and the `EXIT` comment says code 4 covers wrong-shape
content too. A member hash mismatch is `EXIT.digestMismatch`.

Tests in `tools/guest-image/pin-core.test.ts`, written red first:

- `indexMembers` accepts the two-platform OCI index and returns both members
  in `PLATFORMS` order.
- It refuses a missing platform, an extra platform, a duplicate platform, a
  nested index member, a Docker manifest list, and a top-level image manifest.
  The last one replaces the existing "refuses a multi-platform index" test.
- `checkMember` refuses a body hash that differs from the descriptor
  (`EXIT.digestMismatch`) and keeps manifest layer order.
- `checkMemberConfig` refuses an arm64 member whose config says amd64.
- `validatePin` refuses a v1 lock, a missing or extra platform key, and a
  member with a malformed `manifest` or empty `layers`.
- `locksEqual` is false when only one member's layers change.
- `renderLock` matches a literal v2 fixture and round-trips through
  `validatePin`.

### T2 — pin CLI resolution

Edit `tools/guest-image/pin-agent-image.ts` on top of PR #2074. Add
`resolveIndex` (the `--tag` steps in Approach) and point `pin` at it. Make
`discoverBuildTag` compare raw-body hashes, delete `resolvedDigest`, and
update the header's discovery paragraph.

Interfaces:

```ts
async function resolveIndex(tag: string): Promise<PinLock>;
async function discoverBuildTag(): Promise<string>; // compares raw-body hashes
```

The CLI surface (`--tag git-<sha12>`, `--relock`), `LOCK_PATH` and the `EXIT`
set do not change.

Tests extend `tools/guest-image/pin-agent-image.test.ts` and never touch the
real lock. Each copies `pin-agent-image.ts` and `pin-core.ts` into
`<sandbox>/tools/guest-image/` beside a fixture
`<sandbox>/guest-image/agent-oci.lock`; `LOCK_PATH` follows
`import.meta.dir` into the sandbox, as in
`tools/renovate/refresh-agent-image-nixpkgs.test.ts`, so the CLI gets no
test-only knob. A stub `skopeo` on `PATH` serves fixture bodies.

- `--tag` writes the expected v2 lock bytes to the sandbox lock.
- A member body whose hash differs from its descriptor exits
  `EXIT.digestMismatch` and leaves the sandbox lock byte-identical.
- An index without `linux/arm64` exits `EXIT.registryFailed`.

### T3 — nix member selection and re-pin

- Add `guest-image/agent-pin.nix` (interface below). Every attribute read is
  guarded with `?` or `or` and throws explicitly, because `tryEval` does not
  catch a missing-attribute error.
- Rewire `checkedLock`, `agentManifest` and `lockedLayers` in
  `guest-image/default.nix` through it, add `agentIndex`, and add the
  same-`pkgs` comment. The `check-contract` label becomes
  `repo@<member digest>`.
- Add `guest-image/agent-pin-tests.nix` as the `compass-guest-agent-pin-tests`
  attr, using `lib.runTests`. Every case evaluates
  `builtins.tryEval (builtins.deepSeq x x)`: plain `tryEval` forces only the
  outermost value, so a throw inside a returned attrset or list would pass.
  The build fails when any test name is reported. Add the attr and both files
  to the `test` task in `guest-image/moon.yml`.
- Add the aarch64 eval smoke (interface below): the `crossSystem` argument,
  `passthru.agentPin` on `compass-guest-rootfs` from the same let-bound
  fetches its script uses, and `compass-guest-agent-pin-smoke`. A new
  `guest-image/moon.yml` task `eval-arm64` runs
  `nix eval -f default.nix compass-guest-agent-pin-smoke` (`cache: false`,
  `runInCI: true`; inputs `default.nix`, `agent-pin.nix`, `agent-oci.lock`,
  `/devenv.lock`) and joins `ci`'s `deps`.
- Re-pin as in Migration.

Interfaces:

```nix
# guest-image/agent-pin.nix
{ lib }:
{
  # "linux/amd64" | "linux/arm64"; throws on any other system.
  platformFor = system: ...;
  # The lock, unchanged; throws "guest-image: agent-oci.lock is not a valid pin: …".
  checkLock = lock: ...;
  # The member manifest digest; throws on a platform-set or member mismatch.
  memberDigest = { lock, index, platform }: ...;
  # The member's ordered layer digests; throws "… layers do not match the manifest it pins …".
  memberLayers = { lock, manifest, platform }: ...;
}

# guest-image/default.nix: was a bare `let … in { … }`.
{ crossSystem ? null }:
# pkgs = import nixpkgsSrc (if crossSystem == null then { } else { inherit crossSystem; });
# compass-guest-rootfs gains:
#   passthru.agentPin = { platform; index = agentIndex; manifest = agentManifest; layers = agentLayers; };
# New attr. Imports ./default.nix with crossSystem.system "aarch64-linux" and
# "x86_64-linux"; each agentPin.platform, "${outputHashAlgo}:${outputHash}" of
# manifest, and that list over layers must equal the lock member. tryEval of
# riscv64-linux's agentPin.platform must fail. Evaluates to true; otherwise
# throws "guest-image: eval smoke: <system> selected <field> <got>, lock pins <want>".
compass-guest-agent-pin-smoke = ...;
```

Test cases. Positive controls: a valid v2 lock, index and manifest give
`success = true` for each function on both platforms. Each negative case
changes one field of that valid fixture and must give `success = false`:

- `x86_64-darwin` in `platformFor`.
- A lock missing arm64, a lock with an extra platform, and a v1 lock.
- An index missing the build platform, and an index with a duplicate
  build-platform entry.
- An index entry digest that differs from the lock's `manifest`.
- A manifest layer list that differs from the lock's `layers`.

Eval smoke: fetches each index and member manifest, and builds no layer,
rootfs or boot asset. Proving mutation: passing `builtins.currentSystem` to
`platformFor` selects the amd64 member for the aarch64 case on an x86 host,
and the smoke must fail. Build smoke: `moon run guest-image:ci` builds the
amd64 rootfs.

### T4 — Renovate and docs

- `tools/renovate/config.json5`: correct the guest-pin manager comment.
- `tools/renovate/bot-config.json5`: in item 8, say the relock rewrites the
  tag, the index digest and each platform's manifest and layers.
- `tools/renovate/config.test.ts`: add the exactly-one-match test over the
  committed lock (Approach › Renovate). It does not import pin-core.
- `docs/self-host-guest-image.md`: update bump-flow step 2. In the recovery
  section, add the v1-lock case and the unsupported-system error.
- `tools/guest-image/README.md` and the `tools/guest-image/package.json`
  description: describe the index and its members.

Interfaces: none new.

### T5 — follow-up: aarch64 guest boot

A separate issue, filed by the driver, scoped to the Scope list in Approach.
Done when an `aarch64-linux` host builds the three attrs and boots a guest
in the microVM suite.

## Tasks

- [ ] T1 — pin-core v2: schema, index and member checks, render, unit tests
- [ ] T2 — pin CLI: `resolveIndex`, raw-hash discovery, sandboxed CLI tests
- [ ] T3 — `agent-pin.nix` and `deepSeq` eval tests with controls,
      `guest-image/default.nix` wiring, aarch64 eval smoke, moon `test` and
      `eval-arm64`, v2 lock re-pin
- [ ] T4 — Renovate comments and committed-lock regex test, docs and README
      (T1–T4 land as one PR)
- [ ] T5 — file the aarch64 guest boot follow-up issue

## Open Questions

- **When does an arm64 rootfs build join the gate?** Not load-bearing: no
  task, interface or schema depends on it, and adding the leg later is one
  more job.
  - (a) A build-only `ubuntu-24.04-arm` leg of `guest-image:build` now. It
    needs no KVM and catches an arm64 userland contract regression on the
    bump PR, for a second multi-GiB rootfs build on every guest-image PR.
  - (b) Add it with T5. Until then the T3 eval smoke proves arm64
    selection, and the arm64 userland is checked only by layer hash.

  **Recommendation:** (b), with T5. No arm64 guest runs before T5, so an
  arm64 userland regression has no consumer to break, and T5 needs an arm64
  build to boot from anyway.
