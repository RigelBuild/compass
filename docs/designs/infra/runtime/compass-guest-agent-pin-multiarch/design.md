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

`digest` becomes the index digest. Three readers depend on that choice:

- `tools/guest-image/publish.ts` reads `lock.digest` for the
  `org.compass.guest.agent-image-digest` annotation, unchanged. A bare-tag
  deployment (`--image ghcr.io/rigelbuild/compass-agent:latest` in
  `docs/self-host-guest-image.md`) resolves the index digest first.
- Renovate 44.46.2's `getDigest` picks a same-architecture member only when
  the current digest is an image manifest. The v1 lock held one, hence PR
  #2042. With an index digest it returns the `:latest` index digest.
- The Renovate regex `"digest": "(?<currentDigest>sha256:…)"` must match
  exactly once. So the member field is `manifest`, never `digest`.

The member `layers` lists stay in the lock. DL-368 keys the per-layer fetches
on "the lock's descriptor digests", and a bump PR keeps a reviewable layer
diff.

### Validation

Every check fails closed. pin-core `validatePin` guards the write path and
`checkedLock` guards the eval, as today.

On the lock: `repo` and `tag` as today (DL-368 provenance); `digest` is
`sha256:<64 hex>`; `platforms` has exactly the keys `linux/amd64` and
`linux/arm64`; each member has a `manifest` digest and a non-empty, ordered
`layers` list. A v1 lock has no map and is a bad pin.

On the registry, in the pin tool:

- The top-level media type is `application/vnd.oci.image.index.v1+json`;
  `publishImageIndex` in `tools/agent-image-index/index.ts` pushes OCI only.
- The sorted `os/architecture` list equals `linux/amd64,linux/arm64`, the
  publish lane's `EXPECTED_PLATFORMS`, so a duplicate entry fails.
- Each member descriptor is an image manifest, never a nested index.
- Each member body hashes to its digest and passes `layerDigests`.
- Each member's config `os`/`architecture` equals its platform, repeating
  `verifyMemberConfig`, because `publishImageIndex` verifies only after it
  pushes.

In the nix eval:

- The fetched index has the same sorted platform list, compared as strings,
  never as attrset keys, so a duplicate entry fails here too.
- The build platform's entry digest equals the lock member's `manifest`.
- The member manifest's layers equal the lock member's `layers`. The error
  keeps the text "layers do not match the manifest it pins", so the recovery
  doc still applies.

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

`--relock` finds the tag as today, matching `:latest` against `git-<sha12>`
tags newest first, then runs the `--tag` steps. Discovery switches to
`inspect`, and `resolvedDigest` is deleted. Its `{{.Digest}}` already hashed
the top-level index, so the comparison is unchanged. The switch removes cost
and a failure mode: without `--no-tags` each probe listed every tag, and on
an index skopeo also fetches the host-platform member and config, failing on
a host with no member.

The publish lane's two-tag coherence check compares index digests (DL-366),
so the "share a config digest" comments in `pin-agent-image.ts` and
`tools/renovate/config.json5` are corrected. All reads go through the
`skopeo()` helper, which PR #2074 gives `--no-creds`; this lands after #2074.

### Nix member selection

The invariant: the agent member's architecture must equal that of the nix
boot layer over it (guestd, kernel modules, kmod). So the member is chosen
from the same `pkgs` that builds the boot layer, never from an argument,
`builtins.currentSystem` or a cross `pkgs`; a `default.nix` comment says so.
That is `pkgs.stdenv.hostPlatform.system`, mapped like `nativeBySystem` in
`agent-image/entrypoint.nix`:

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

The fetch chain becomes index, then member, then layers:

1. `agentIndex`: `fetchurl` of `manifests/<lock digest>` with
   `Accept: application/vnd.oci.image.index.v1+json` and `hash = digest`.
2. `memberDigest`: reads the index, checks it, and returns the lock member's
   `manifest`.
3. `agentManifest`: the existing fetch, now keyed on the member digest.
4. `lockedLayers`: the existing compare, now against the member's `layers`.

Only the build platform's member and layers are fetched.

The pure checks move to a new file, `guest-image/agent-pin.nix`, taking
`{ lib }` and parsed JSON values. This is the one new abstraction: inside
`default.nix` the checks sit between fetches with `system` fixed to the
host, so no test could reach the arm64 branch. As functions over fixtures,
every check is testable on any host. The arm64 fetch wiring first runs on an
aarch64 builder (Open Questions).

### Renovate

The regex, `currentValueTemplate: "latest"`, the solo group and the
branch-mode relock task do not change; their comments now say "index
digest". A new lockstep test runs the regex over the committed
`guest-image/agent-oci.lock`, as its neighbours do with `devenv.lock` and
`postgres_image.go`. It requires exactly one match, equal to the lock's
top-level `digest`. A member field renamed to `digest` fails it. On a bump,
Renovate proposes the new index digest, `--relock` rewrites the tag, index
digest and both members, and the gate builds the amd64 member.

### Migration

Both modes validate the existing lock first, so a v1 lock blocks them. The
implementation PR deletes it and runs `--tag` with the tag `:latest` points
at, which `publish-image-manifest` prints (":latest matches :git-<sha12>").

`git-7d22c69390cb` has no index, so the migration PR also bumps the agent.
Both the guest-image build and the `microvm` boot leg must pass on it. Close
PR #2042 if Renovate does not replace it.

### Scope

In scope: the lock, the pin tool, nix member selection, Renovate, tests and
docs. After this, the rootfs evaluates and fetches its layers on
`aarch64-linux`.

Out of scope, named as T5: booting an aarch64 guest and publishing its
artifact. Each piece is x86-shaped today:

- The kernel asset: nixpkgs builds `bzImage` on x86 and `Image` on aarch64
  (upstream `pkgs/os-specific/linux/kernel/build.nix`), but `ASSET_FILENAMES`
  in `tools/guest-image/publish-core.ts`, `materialize.go`, `ci.yml` and
  `default.nix` assume `bzImage`.
- `go/internal/runtime/microvm/launch.go` appends `console=ttyS0`.
- `bootModuleConfigs` in `default.nix` is checked against the x86 config only.
- Artifact addressing is a design fork: a per-arch tag, or an index of guest
  artifacts, which makes `materialize.go` (exactly three layers per manifest)
  and `--guest-artifact` platform-aware.
- `publish-guest-image` and the `microvm` job run on `ubuntu-latest`, and
  `tools/toolchain/microvm-vmm-env.nix` is untried on aarch64. [INFERENCE]
  Hosted arm64 runners may lack `/dev/kvm`.
- `flake.nix` declares `systems = [ "x86_64-linux" ]`.

### Decision ledger

DL-445 records this and refines DL-368 §(a)'s lock schema. DL-368 stays
Active: the rootfs is still "pinned by digest … keyed by the lock's
descriptor digests", and its provenance and boot rules are unchanged. The
single-manifest rule lived only in a `pin-core.ts` comment. One guarantee
narrows: the userland contract check runs for `linux/amd64` only until an
arm64 build leg exists. DL-366 stays Active; its "zero consumer changes"
claim in the [arm64 image record](../../ci/compass-agent-arm64-image/design.md)
missed this consumer.

## Alternatives considered

- **Pin one member with a `platform` field (option A).** An arm64 guest would
  get an amd64 userland or nothing. Matt ruled B.
- **Stop Renovate tracking the pin (option C).** The pin goes stale silently.
- **Pin the per-arch `:git-<sha12>-<arch>` tags.** DL-366 makes these
  internal. Renovate would track two deps, and the lock would not name what a
  consumer pulls.
- **Lock only the index digest; derive the rest in nix.** It drops the
  descriptor digests DL-368 names as fetch keys, and the layer diff.
- **Annotate the artifact with the member digest instead.** A bare-tag
  deployment resolves the index digest, so a skew compare always mismatches.
- **Annotate with both digests.** The member digest is the exact rootfs
  provenance. Deferred: nothing reads the annotation yet (`materialize.go`
  only shape-checks it). The planned skew warning adds it if needed.

## Global Constraints

- **Fail closed.** Any shape, platform-set, media-type, hash or config
  disagreement is an error with a numbered exit code. Never fall back to one
  member.
- **The platform set is exactly `linux/amd64` + `linux/arm64`,** mirroring
  `EXPECTED_PLATFORMS` in `tools/agent-image-index/index.ts`.
- **Anonymous reads.** skopeo carries `--no-creds` (PR #2074); nix fetches
  send GHCR's anonymous bearer `QQ==`, as today.
- **Provenance unchanged.** Repo and tag rules stay as DL-368 sets them.
- **Only the pin tool writes the committed lock.** Tests never touch it.
- **Boot contract and gate shape unchanged.** Output attr names, the moon
  `build` command, and the kernel and initrd derivations stay the same.
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
`layerDigests`; the `EXIT` comment notes that code 4 also covers wrong-shape
registry content, which a retry will not fix. A member hash mismatch is
`EXIT.digestMismatch`.

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
`resolveIndex`, which runs the `--tag` steps in Approach. Point `pin` at it.
Make `discoverBuildTag` compare `inspect` digests. Delete `resolvedDigest`.
Rewrite the header's discovery paragraph for index digests.

Interfaces:

```ts
async function resolveIndex(tag: string): Promise<PinLock>;
async function discoverBuildTag(): Promise<string>; // compares raw-body hashes
```

The CLI surface (`--tag git-<sha12>`, `--relock`), `LOCK_PATH` and the `EXIT`
set do not change.

Tests: extend `tools/guest-image/pin-agent-image.test.ts`, never touching
the real lock. Each test copies `pin-agent-image.ts` and `pin-core.ts` into
`<sandbox>/tools/guest-image/` with a fixture
`<sandbox>/guest-image/agent-oci.lock`. `LOCK_PATH` is relative to
`import.meta.dir`, so it resolves inside the sandbox, as in
`tools/renovate/refresh-agent-image-nixpkgs.test.ts`; the CLI gets no
test-only knob. A stub `skopeo` on `PATH` serves fixture bodies.

- `--tag` writes the expected v2 lock bytes to the sandbox lock.
- A member body whose hash differs from its descriptor exits
  `EXIT.digestMismatch` and leaves the sandbox lock byte-identical.
- An index without `linux/arm64` exits `EXIT.registryFailed`.

### T3 — nix member selection and re-pin

- Add `guest-image/agent-pin.nix` (interface below). Every attribute read is
  guarded with `?` or `or` and throws explicitly, because `tryEval` does not
  catch a missing-attribute error.
- Rewire `checkedLock`, `agentManifest` and `lockedLayers` in `default.nix`
  through it, add `agentIndex`, and add the same-`pkgs` comment. The
  `check-contract` label becomes `repo@<member digest>`.
- Add `guest-image/agent-pin-tests.nix` as the `compass-guest-agent-pin-tests`
  attr, using `lib.runTests`. Every case evaluates
  `builtins.tryEval (builtins.deepSeq x x)`: plain `tryEval` forces only the
  outermost value, so a throw inside a returned attrset or list would pass.
  The build fails when any test name is reported. Add the attr and both files
  to the `test` task in `guest-image/moon.yml`.
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

Smoke: `moon run guest-image:ci` builds the rootfs from the amd64 member.

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

A separate issue, filed by the driver. Its scope is the Scope list in
Approach, including the artifact-addressing fork and the arm64 KVM host. It
is done when an `aarch64-linux` host builds the three attrs and boots a guest
in the microVM suite.

## Tasks

- [ ] T1 — pin-core v2: schema, index and member checks, render, unit tests
- [ ] T2 — pin CLI: `resolveIndex`, raw-hash discovery, sandboxed CLI tests
- [ ] T3 — `agent-pin.nix` and `deepSeq` eval tests with controls,
      `default.nix` wiring, moon `test`, v2 lock re-pin
- [ ] T4 — Renovate comments and committed-lock regex test, docs and README
      (T1–T4 land as one PR)
- [ ] T5 — file the aarch64 guest boot follow-up issue

## Open Questions

- **When does an arm64 rootfs build join the gate?** This question is not
  load-bearing: no task, interface or schema here depends on the answer, and
  adding the leg later is one more job, not a redesign.
  - (a) A build-only `ubuntu-24.04-arm` leg of `guest-image:build` now. It
    needs no KVM, and it catches an arm64 userland regression (unpack and the
    `bin/sh`, `nft`, `getent`, `awk`, `compass-agent` contract) on the bump
    PR. The cost is a second multi-GiB rootfs build on every guest-image PR.
  - (b) Add it with T5. There is no CI cost now. arm64 layers are checked
    only by hash until the boot work lands.

  **Recommendation:** (b), with T5. No arm64 guest runs anywhere until T5,
  so an arm64 userland regression has no consumer to break before then. T5
  needs an arm64 build to boot from anyway.
