# Publish the compass-agent image for linux/arm64

Status: Draft
Tracking: RIG-3625

> **Design record.** This designs the multi-arch (linux/amd64 + linux/arm64)
> publish lane for the `compass-agent` runtime image. Every path cited below is
> a path in `RigelBuild/compass` at `origin/main` as of 2026-09-12; line numbers
> drift as the code evolves, so resolve them against that ref. It extends, and
> partially supersedes, the platform contract in
> [`compass-agent-image-publish`](../compass-agent-image-publish/design.md)
> ("Decision: platform contract — `linux/amd64` single-arch"), whose other
> decisions (tag names, immutability posture, auth pin, build-once mechanism)
> all stay live and are carried forward here.

## Problem / Intent

The Apple-container runtime tier on macOS runs agent containers in a real
Linux arm64 guest, so the `compass-agent` image must exist for linux/arm64.
Today the publish lane builds and ships linux/amd64 only, and CI actively
asserts that: `.github/workflows/release.yml:333-335` fails the publish run
when the pushed image is not `linux/amd64`. This record designs the arm64
build, the multi-arch tag layout, and the multi-arch form of every existing
integrity guard. Matt has authorized building the arm image (RIG-3625).

## Context: how the image is built and guarded today

### The build is native, single-arch, and independent of the root flake

`agent-image/publish.sh:65-68` builds the image spec through the shared devenv
fork, resolved from `agent-image/devenv.lock` — never from the root
`flake.nix`:

```bash
DEVENV_SRC="$(bun "$SCRIPT_DIR/../tools/toolchain/devenv-cli/index.ts" --lock "$SCRIPT_DIR/devenv.lock" --mode flakeref)"
log "Building image spec: nix run $DEVENV_SRC -- container build agent"
BUILD_OUT="$(nix run "$DEVENV_SRC" -- container build agent)"
SPEC="$(printf '%s\n' "$BUILD_OUT" | tail -n 1)"
```

`nix run` evaluates the devenv fork's flake for the *builder's* system and
nix2container emits a spec for that system — so the build is native: an arm64
image needs an arm64 builder (or emulation). The root `flake.nix:33`
(`systems = [ "x86_64-linux" ];`) is **not in this build's dependency graph**:
the inputs are `agent-image/devenv.yaml`'s own `nixpkgs`, `nix2container`,
`mk-shell-bin`, and `devenv` fork inputs, pinned in `agent-image/devenv.lock`.
The moon gate build (`agent-image/moon.yml`, `build` task) runs the identical
two-step resolve-then-run:

```text
script: 'src=$(bun tools/toolchain/devenv-cli/index.ts --lock agent-image/devenv.lock --mode flakeref) && cd agent-image && nix run "$src" -- container build agent'
```

So the blast radius of arm64 support is `agent-image/` plus the publish jobs in
`release.yml` — the root flake's `systems` list does not change. The
`flake.nix:138-141` TODO ("TODO(aarch64-darwin follow-up): the darwin app links
system WebKit via frameworks … add a darwin branch when the systems list
grows") is about aarch64-**darwin** and the gtk/WebKit app; it is unrelated to
the agent image's aarch64-**linux** need and stays untouched.

### The single-digest assumptions a manifest list breaks

Three guards assume one image config per tag. All three are deliberate
security/correctness properties, and each needs a multi-arch equivalent (never
deletion — `rule://no-inert-gating`).

1. **`guard_immutable` + `LOCAL_DIGEST`** (`agent-image/publish.sh:81`,
   `:92-127`): the local identity is one config digest,

   ```bash
   LOCAL_DIGEST="$("${SKOPEO[@]}" inspect --raw "nix:$SPEC" | jq -r .config.digest)"
   ```

   and the guard compares it to the remote tag's
   (`remote_digest="$(printf '%s' "$remote_raw" | jq -r .config.digest)"`,
   `publish.sh:102`), aborting on any ambiguous registry error. The post-copy
   assert (`publish.sh:153`) re-inspects the pushed tag against the same
   digest. On a manifest list, `skopeo inspect --raw` returns an OCI image
   index — it has **no** `.config.digest`, so `jq -r .config.digest` yields
   `null` and every compare is meaningless.

2. **The amd64 tripwire** (`.github/workflows/release.yml:330-336`):

   ```bash
   # Cheapest platform-contract-regression tripwire.
   arch="$(jq -r .Architecture "$inspect_json")"
   os="$(jq -r .Os "$inspect_json")"
   if [ "$arch" != "amd64" ] || [ "$os" != "linux" ]; then
     echo "platform contract violated: got $os/$arch, want linux/amd64" >&2
     exit 1
   fi
   ```

   `skopeo inspect` on a manifest list resolves one member (or errors,
   depending on flags); `.Architecture` is not a property of the list. The
   tripwire must become an assertion about the new contract — the index
   contains exactly the expected platform set — not disappear.

3. **The `:latest` / `:git-<sha>` coherence check**
   (`.github/workflows/release.yml:338-345`):

   ```bash
   git_digest="$(skopeo inspect --raw --authfile "$REGISTRY_AUTH_FILE" "$ref:git-$sha12" | jq -r .config.digest)"
   latest_digest="$(skopeo inspect --raw --authfile "$REGISTRY_AUTH_FILE" "$ref:latest" | jq -r .config.digest)"
   if [ "$git_digest" != "$latest_digest" ]; then
   ```

   Same breakage: no `.config.digest` on an index. The property itself
   (the two tags are the same artifact) survives — compared as manifest-list
   digests instead.

   The semver re-tag job carries the same shape: `release.yml:874-885` copies
   `:git-<sha12>` to `:vX.Y.Z` with `skopeo copy` and verifies
   `.config.digest` coherence. Two multi-arch breaks there: a bare
   `skopeo copy` of a list copies a **single resolved image**, not the list
   (it needs `--multi-arch all`), and the digest compare needs the same
   list-digest form.

### Where publish runs in CI

The `publish-image` job (`release.yml:94-129`) runs on `runs-on: ubuntu-latest`
(`release.yml:96`) with `permissions: contents: read / packages: write`
(`release.yml:98-100`), serialized under
`concurrency: group: publish-agent-image, cancel-in-progress: false, queue: max`
(`release.yml:110-113`), gated to `github.ref == 'refs/heads/main'`
(`release.yml:117`), `working-directory: agent-image` (`release.yml:127-129`).
Its bootstrap, per step:

- an in-job changed-path gate over `IMAGE_CLOSURE_PATHS` (`release.yml:49`,
  consumed at `:204`) decides `should_publish`;
- `cachix/install-nix-action` with reviewed substituters (`release.yml:209-222`);
- pinned bun via `nix eval -f tools/toolchain/gate-tools.nix langs.bun`
  (`release.yml:224-244`) — publish needs the devenv-CLI resolver under bun;
- the fork's patched skopeo via
  `nix build -f tools/toolchain/skopeo-nix2container-env.nix skopeo`
  (`release.yml:246-278`), prepended to `PATH` — it understands the `nix:`
  transport stock skopeo lacks;
- `REGISTRY_AUTH_FILE=$RUNNER_TEMP/ghcr-auth.json` pinned to `GITHUB_ENV`
  (`release.yml:287`) so login and copy resolve the same creds file;
- `skopeo login ghcr.io` with `GITHUB_TOKEN` via `--password-stdin`
  (`release.yml:302-305`);
- `run: ./publish.sh` with no args (`release.yml:312`) — the default two-tag
  set, `publish.sh:54-59`: `TAGS=("git-${SHA}" "latest")`, immutable pin
  first;
- the verify step (`release.yml:314-346`) quoted above.

## Approach

Fan the build out to one native job per arch, keep every per-arch artifact
under an immutable per-arch tag with the existing single-digest guards intact,
then compose and push an OCI image index under the existing consumer-facing
tags (`:git-<sha12>`, `:latest`, and at release time `:vX.Y.Z`), with each
guard translated to its list-level equivalent.

### Decision A — consumer-facing tags are a manifest list; per-arch tags are internal

**Options.**

1. **Manifest list under the existing tags** (recommended). `:git-<sha12>` and
   `:latest` become OCI image indexes listing exactly
   `{linux/amd64, linux/arm64}`. Per-arch tags `:git-<sha12>-amd64` /
   `:git-<sha12>-arm64` exist as immutable internal building blocks the index
   references by digest.
2. **Per-arch tags only** (`:git-<sha12>-arm64`, `:latest-arm64`). Keeps every
   existing guard byte-for-byte, but pushes arch selection onto every consumer
   forever.
3. **Separate arm64 repository** (`compass-agent-arm64`). Same consumer burden
   as 2 plus a second package's visibility/auth surface. No advantage.

**Consumers found** (search: `compass-agent` refs across the repo):

- `go/cmd/compass-app/embedded.go:43` —
  `const defaultAgentImage = "ghcr.io/rigelbuild/compass-agent:latest"`, the
  embedded stack's default when no `--image`/`$COMPASS_AGENT_IMAGE` is given.
- `go/internal/stack/stack.go:316` —
  `s.deps.Images.EnsureImage(ctx, s.cfg.AgentImage)`; the ensurer pulls by
  tag: `go/internal/stack/adapters/image.go` `imageCLI` is
  `ImageExists(ctx, image)` + `Pull(ctx, image)`, backed by
  `go/internal/runtime/podman.go:651-652` — `p.run(ctx, "podman pull",
  []string{"pull", image})`. A plain tag pull, no digest, no platform flag.
- `go/cmd/compass-stack/main.go:273` — `AgentImage: f.image` plumbs the
  `--image` flag into that config.
- The Apple-container tier (the motivating consumer):
  `docs/designs/platform/apple-container-macos-runner/design.md` OQ-8 states
  "the compass-agent image is built for the host arch" — i.e. its pull path
  also consumes the plain ref and expects the registry to serve the host arch.

Every consumer pulls a bare tag and relies on the container engine's default
platform negotiation. With a manifest list, **zero consumer changes**: podman
(and Apple `container`) resolve the index to the host-native member. With
per-arch tags, `defaultAgentImage`, the baked `--image` pin, the stack config,
and every doc naming the ref would all need arch-switching logic — permanent
complexity in many places to avoid one-time complexity in the publish lane.

**Recommendation: option 1.** The publish lane is the single place that knows
the platform set; keep the arch knowledge there.

### Decision B — native `ubuntu-24.04-arm` runner, not QEMU

**Options.**

1. **GitHub-hosted `ubuntu-24.04-arm` runner** (recommended). Free-tier Linux
   arm64 runners are GA for public repos (labels `ubuntu-24.04-arm` /
   `ubuntu-22.04-arm`); `RigelBuild/compass` is public. The arm64 job is a
   near-clone of the amd64 job: same nix install, same bootstrap steps, same
   `publish.sh` invocation. The build is native, so nix2container is expected
   to emit an aarch64-linux spec with no cross machinery. That is
   designed-to-be-true, not yet observed (UNVERIFIED until T2; OQ-1).
2. **binfmt/QEMU emulation on `ubuntu-latest`.** One runner, but the image
   closure is the dominant CI cost already (the 90-minute `timeout-minutes`
   at `release.yml:120` is sized by it); emulating a full nix build of that
   closure multiplies it several-fold and adds a binfmt setup step as a new
   trust surface. Rejected.
3. **Self-hosted arm64 (the mattmini).** The mac mini is committed to darwin
   spike/contract work (apple-container record, OQ-9 ruling); a Linux arm64
   build lane on it would need a Linux VM and a standing runner registration.
   Unnecessary while hosted arm64 runners are free.

**Recommendation: option 1**, with these grounded facts and stated unknowns:

- **bun pin: verified present for arm64.**
  `tools/toolchain/versions/bun.nix:9-11` already carries an
  `"aarch64-linux"` entry (`bun-linux-aarch64.zip` + hash), so the
  `gate-tools.nix langs.bun` bootstrap resolves on the arm runner.
- **FOD hash: verified single-platform — a real change.**
  `agent-image/entrypoint.nix` pins the bundled entrypoint's `node_modules`
  tree as one fixed-output hash
  (`outputHash = "sha256-JbgM44AwH7/b3Y/2T44+eBXwyvMi8owXToGVspEeCk4="`), and
  its own comments state the hash covers "the INSTALLED TREE … as it lands on
  THIS build platform" including "platform-specific optional dependencies".
  An aarch64 install produces a different tree, so the single hash must
  become a per-system attrset keyed like `versions/bun.nix` (T1).
- **UNVERIFIED: the devenv fork + nix2container fork + patched skopeo
  toolchain has never been run on aarch64-linux.** Nobody has executed
  `nix run <fork> -- container build agent` on an arm64 builder. Upstream
  devenv, nix2container, and nixpkgs all support aarch64-linux, and
  `tools/toolchain/skopeo-nix2container-env.nix` imports nixpkgs
  system-implicitly ("the system is implied by the nixpkgs it is imported
  with"), so nothing *pins* x86_64 — but this record does not claim it works.
  T2 is a spike that proves or disproves it before any workflow change.
- **UNVERIFIED: binary-cache coverage on aarch64-linux.** The reviewed
  substituters (`devenv.cachix.org`, `cachix.cachix.org`,
  `release.yml:219-222`) may hold few aarch64 artifacts. The risk is
  build-from-source time, not correctness: a cache miss falls back to source
  builds within the 90-minute ceiling or fails it visibly. If T2 shows the
  wall-clock is unacceptable, populating a cache is a follow-up, not a design
  change.
- **The `@oh-my-pi` native-addon copy block is x64-hardcoded and must be
  edited, independent of whether an aarch64 prebuilt exists.**
  `agent-image/entrypoint.nix:218-220` names the arch three times:
  `natives=node_modules/.bun/node_modules/@oh-my-pi/pi-natives-linux-x64`,
  then `cp $natives/pi_natives.linux-x64-modern.node` and
  `pi_natives.linux-x64-baseline.node`. The package name, both filenames, and
  the variant scheme itself all change on arm64: the surrounding comment
  (`:214-217`) states the loader picks `modern` when the host has AVX2 else
  `baseline`, and AVX2 is an x86 feature with no arm64 analogue, so the
  two-variant copy is not portable as written. T1 owns this edit.
- **UNVERIFIED: whether that aarch64 prebuilt exists at all, and under which
  variant names.** If absent, the entrypoint bundle fails at build time on
  arm64 — a loud, pre-push failure. T2 surfaces both the existence and the
  real filenames the copy block must use.

### Decision C — the guards' multi-arch forms

Every guard survives; none is deleted (`rule://no-inert-gating`).

1. **Per-arch immutability: unchanged code, new tag names.** `publish.sh` runs
   once per arch job and pushes only that arch's tag
   (`git-<sha12>-<arch>`). Single-arch manifests still have exactly one
   `.config.digest`, so `LOCAL_DIGEST` (`publish.sh:81`), `guard_immutable`
   (`publish.sh:92-127`) — including its "ambiguous registry error → abort,
   never overwrite" posture — and the post-copy assert (`publish.sh:153`)
   work verbatim. The script grows a tag-suffix/skip-latest mode (T3); its
   guard logic does not change.

2. **Index immutability: the same guard shape one level up.** The compose
   step's local identity is the index's **manifest-list digest** (the sha256
   of the raw index bytes, `skopeo inspect --raw docker://…:git-<sha12> |
   sha256sum`, or `skopeo inspect --format '{{.Digest}}'`). Before pushing
   `:git-<sha12>`: if the remote tag exists and its list digest equals the
   locally composed one → idempotent skip; if it exists and differs → hard
   fail; if inspect fails with anything but a definitive manifest-unknown →
   abort. Identical decision table to `guard_immutable`, with
   `.config.digest` replaced by the list digest.

   **This guard's skip arm depends on byte-deterministic index composition,
   which is UNVERIFIED (OQ-5).** An OCI image index is not canonicalized by
   the spec, so a recomposed index could differ in member order or carry an
   injected annotation and hash differently while describing the same two
   images. If that happens, the digest-equality skip never fires and a
   re-run after a mid-compose failure hits the "exists and differs → hard
   fail" arm against a tag that is immutable by design, wedging that sha's
   publish with no clean recovery. T2 closes this by composing, pushing to a
   scratch tag, recomposing from the same member digests, and comparing the
   two list digests. If composition proves non-deterministic, the guard's
   identity becomes the **member digest set** (assert the remote index's
   members are exactly the two per-arch digests) rather than the list digest
   — same immutability property, no dependence on byte-stable serialization.

3. **The platform tripwire becomes a platform-set assertion.** Replacement
   for `release.yml:330-336`: fetch the raw index for `:git-<sha12>`, assert
   `mediaType` is an image index, and assert the platform set is **exactly**
   `{linux/amd64, linux/arm64}` — no members missing, none extra:

   ```bash
   platforms="$(jq -r '[.manifests[].platform | "\(.os)/\(.architecture)"] | sort | join(",")' "$index_json")"
   [ "$platforms" = "linux/amd64,linux/arm64" ] || fail
   ```

   Then, for each member, resolve its digest-addressed manifest and assert
   its config's `architecture`/`os` match the entry's declared platform —
   the direct descendant of the old tripwire, now per member. This is a
   stronger contract than today's, not a weaker one: it also fails when the
   arm64 half silently vanishes.

4. **Two-tag coherence compares list digests.** Replacement for
   `release.yml:338-345`: `:latest`'s manifest-list digest must equal
   `:git-<sha12>`'s. Same property ("the moving tag is the pinned artifact"),
   same hard-fail, one level up.

5. **The semver re-tag copies the whole list.** `release.yml:874-875`'s
   `skopeo copy "$ref:git-$resolved_sha12" "$ref:$tag"` gains
   `--multi-arch all`, and the coherence verify at `:880-885` switches from
   `.config.digest` to the manifest-list digest. The §A4 ancestor-walk
   resolver (`release.yml:830-870`) is digest-agnostic (it only probes tag
   existence) and needs no change.

### Decision D — ordering and partial-failure posture

**Order (three phases, strictly sequenced):**

1. **Per-arch build+push, in parallel.** `publish-image-amd64`
   (`ubuntu-latest`) and `publish-image-arm64` (`ubuntu-24.04-arm`) each run
   the full bootstrap and `publish.sh` in per-arch mode, pushing only
   `:git-<sha12>-<arch>`. They share no tag, so they need no mutual
   serialization.
2. **Compose+push the index.** A third job, `needs:` both, composes the index
   from the two per-arch tags **by digest** (re-inspect each per-arch tag,
   pin the member digests into the index — never by tag, so a race cannot
   swap a member) and pushes `:git-<sha12>` first, then `:latest`, preserving
   `publish.sh:54-59`'s pin-before-moving-tag order.
3. **Verify.** The platform-set assertion and list-digest coherence check
   (Decision C.3/C.4), in the compose job.

Only the compose job carries the `publish-agent-image` concurrency group
(`release.yml:110-113` semantics: `cancel-in-progress: false`, `queue: max`) —
it is the only writer of shared tags, and the release-time `release-image` job
already shares that group (`release.yml:723-726`).

**Partial-failure analysis, preserving the "immutable `:git-*`, abort on
ambiguity" posture:**

- One arch job fails → the compose job never runs; `:git-<sha12>` and
  `:latest` do not move. Consumers see the previous coherent state. A
  stranded `:git-<sha12>-<arch>` tag is harmless: it is immutable,
  digest-pinned, and consumer-invisible (nothing pulls `-<arch>` tags).
  Re-running the workflow is idempotent — the stranded tag hits
  `guard_immutable`'s matching-digest skip.
- Compose pushes `:git-<sha12>` but fails before `:latest` → exactly today's
  failure mode between the two `skopeo copy` iterations of
  `publish.sh:128-159`; the re-run's index guard (C.2) skips the pin and
  moves `:latest`. No new window is introduced. **This recovery is only as
  good as C.2's skip arm, which is UNVERIFIED pending OQ-5**: if index
  composition is not byte-deterministic the re-run hard-fails instead of
  skipping, and the fallback identity in C.2 (assert the member digest set
  rather than the list digest) is what restores the clean re-run.
- A registry blip during any guard probe → abort without pushing, verbatim
  `guard_immutable` posture.

**Index-composition tool.** `skopeo` cannot assemble an index. Options:
`podman manifest create/add/push` (preinstalled on GitHub runners, honors
`REGISTRY_AUTH_FILE`), `buildah manifest`, or writing the OCI index JSON and
pushing it raw. **Recommendation: `podman manifest`**, digest-pinned members,
with the pushed bytes re-inspected for the guard digest. UNVERIFIED: that the
GitHub-runner podman version pushes an OCI-mediaType index GHCR serves
correctly to both podman and Apple `container`; T2's spike includes this
end-to-end pull check.

### What does not change

- The root `flake.nix` (`systems = [ "x86_64-linux" ]`, `flake.nix:33`) — the
  agent image does not build through it (see Context). The
  `flake.nix:138-141` aarch64-darwin TODO is out of scope.
- `agent-image/devenv.yaml` / `devenv.lock` inputs — same fork revs, evaluated
  for a second system.
- `guard_immutable`'s logic and the auth/`REGISTRY_AUTH_FILE` pin.
- The moon gate (`agent-image/moon.yml`) stays amd64-only as the PR-time
  build-health signal; an arm64 PR gate would double the dominant CI cost for
  drift classes T2 shows are rare. Revisit only if arm64-only breakage
  recurs (Open Questions).

## Plan

### Global Constraints

- Tag contract: `:git-<sha12>` (immutable) and `:latest` (moving) remain the
  only consumer-facing per-push tags; `:vX.Y.Z` remains release-time. Per-arch
  tags `:git-<sha12>-{amd64,arm64}` are internal and immutable; no `-latest`
  per-arch moving tags.
- Platform set: exactly `{linux/amd64, linux/arm64}`, asserted, not implied.
- No guard is deleted or weakened; every existing property gets its
  list-level equivalent (`rule://no-inert-gating`).
- All registry writes stay `GITHUB_TOKEN` + `REGISTRY_AUTH_FILE`-pinned,
  least-privilege `packages: write`, main-ref-guarded, inside the
  `publish-agent-image` concurrency group for shared-tag writers.
- Runners: GitHub-hosted only (`ubuntu-latest`, `ubuntu-24.04-arm`); no
  self-hosted, no QEMU.

### T1 — per-system `entrypoint.nix` (FOD hash and native-addon copy)

Two edits in `agent-image/entrypoint.nix`, both required before an arm64 build
can succeed.

First, make `nodeModules.outputHash` a per-system attrset keyed by
`pkgs.stdenv.hostPlatform.system`, following the shape of
`tools/toolchain/versions/bun.nix` (`"x86_64-linux"` / `"aarch64-linux"`
entries). The aarch64 hash is obtained the way the file's own comment
prescribes (set `lib.fakeSha256`, take the reported value) — on the T2 spike
runner, since the hash is what the arm64 install tree produces.

Second, parameterize the native-addon copy block at `:218-220` per system. It
hardcodes the arch three times: the `pi-natives-linux-x64` package path and
both `pi_natives.linux-x64-{modern,baseline}.node` filenames. The variant
scheme is also not portable: per the block's own comment (`:214-217`) the
loader picks `modern` on an AVX2 host else `baseline`, and AVX2 is x86-only.
So arm64 needs its real variant names rather than a renamed pair, and the
"copy both" rule holds only if arm64 ships two. T2 reports the actual package
contents; this task consumes that answer.

**T1 does not merge on its own.** Both edits need a real aarch64 value that
only the T2 runner can produce, so landing the seam alone would put a
`lib.fakeSha256` placeholder and an unresolved copy block on `main` where
nothing selects them until the arm64 lane exists — config that provably does
nothing, which `rule://no-inert-gating` forbids. T1 is authored against the
T2 spike and lands with T2's measured hash and variant names in the same
change, or it waits for the T4 cutover. T2 itself is dispatch-only and writes
no tags, so it is not a merge gate for anything else.

Interfaces: consumes `pkgs` (already in scope); produces the same `outputHash`
string and the same two `.node` files in `$out` per system. The amd64 hash and
copied filenames stay byte-identical to today's.

### T2 — arm64 build spike (workflow_dispatch, no tag writes)

A temporary `workflow_dispatch`-only job on `ubuntu-24.04-arm`: full bootstrap
(nix, pinned bun, patched skopeo), then
`nix run <fork> -- container build agent` in `agent-image/`, then
`skopeo inspect --raw nix:$SPEC` asserting `architecture == arm64`. No
registry writes. Also: compose a throwaway index in a scratch tag under the
actor's namespace and pull it with podman to close the UNVERIFIED
index-serving question. Success criteria: spec builds, config architecture is
arm64, wall-clock recorded against the 90-minute ceiling, native addon
present in the bundle.

Interfaces: consumes T1; produces a go/no-go plus the aarch64 FOD hash and a
wall-clock number that sizes the arm64 job's timeout.

### T3 — `publish.sh` per-arch mode

Add flags (e.g. `--arch-suffix <arch>` implying suffix-tagged pushes and no
`:latest`): the default tag computation (`publish.sh:54-59`) becomes
`git-<sha12>-<arch>` only. Guard logic untouched. Bare invocation keeps
today's behavior until T4 cuts over, then bare invocation is removed with the
cutover (no dead mode left behind).

Interfaces: consumes `REGISTRY_AUTH_FILE`, arch flag; produces the immutable
per-arch tag, guard-verified.

### T4 — `release.yml` fan-out + compose job

Split `publish-image` into `publish-image-amd64` / `publish-image-arm64`
(identical steps, `runs-on` differs, both gated by the same
`IMAGE_CLOSURE_PATHS` in-job gate) and add `publish-image-manifest`
(`needs:` both, `ubuntu-latest`, `publish-agent-image` concurrency group):
digest-pinned `podman manifest create/add`, push `:git-<sha12>` then
`:latest`, with the C.2 index-immutability guard before the pin push and the
C.3/C.4 verify replacing `release.yml:314-346`.

Interfaces: consumes T3's script mode; produces the two index tags plus the
verify assertions. The old single `publish-image` job is removed in this same
change.

### T5 — `release-image` multi-arch re-tag

`skopeo copy --multi-arch all` at `release.yml:874-875`; coherence verify at
`:880-885` switches to manifest-list digests. Ancestor-walk resolver
unchanged.

Interfaces: consumes T4's published index; produces `:vX.Y.Z` as the same
index.

### T6 — docs + record cross-reference sweep

Update `docs/architecture`/record references to the `linux/amd64 single-arch`
contract where they describe the *current* lane (not frozen decisions), and
note in the Apple-container record's OQ-8 successor context that the arm64
image now exists. Frozen records are not rewritten; this record is the new
authority for the platform contract.

## Tasks

- [ ] T1 — per-system `entrypoint.nix`: FOD hash + native-addon copy block
- [ ] T2 — arm64 build spike on `ubuntu-24.04-arm` (dispatch-only, no pushes)
- [ ] T3 — `publish.sh` per-arch mode
- [ ] T4 — `release.yml` fan-out + index compose/verify jobs
- [ ] T5 — `release-image` `--multi-arch all` re-tag + list-digest verify
- [ ] T6 — docs sweep

Ordering: T1 → T2 (spike needs the per-system hash seam to obtain the arm64
hash) → T3 → T4 → T5; T6 with T4/T5. T2 is the gate: if the toolchain does not
bootstrap on aarch64-linux, findings come back to this record's Open Questions
before any workflow change lands.

## Open Questions

- **OQ-1 [load-bearing until T2] — does the pinned toolchain run on
  aarch64-linux?** Nobody has run the devenv fork + nix2container fork +
  patched skopeo on an arm64 Linux host. Nothing found pins x86_64
  (`skopeo-nix2container-env.nix` is system-implicit; `versions/bun.nix`
  carries an aarch64-linux entry), but this is designed-to-be-true, not
  observed-true. T2 resolves it empirically; the recommendation stands only
  if T2 is green.
- **OQ-2 [load-bearing until T2] — GHCR index serving to both consumers.**
  That a podman-composed OCI index at GHCR resolves correctly for podman on
  linux/amd64 hosts and Apple `container` on arm64 macOS guests. T2's scratch
  pull closes it.
- **OQ-3 [non-load-bearing] — arm64 in the PR-time moon gate.** Deferred:
  the moon `build` gate stays amd64. If T2/production show arm64-only
  breakage classes (FOD drift, addon prebuilt gaps), an affected-scoped arm64
  gate is a later add; the publish lane's own arm64 job reds post-merge
  either way.
- **OQ-4 [non-load-bearing] — aarch64 binary-cache population.** If T2's
  wall-clock is painful, publishing the arm64 closure to a Rigel cachix cache
  is a follow-up optimization; correctness does not depend on it.
- **OQ-5 [load-bearing until T2] — is index composition byte-deterministic?**
  C.2's immutability guard and Decision D's clean re-run both assume that
  recomposing the index from the same two member digests produces identical
  bytes, and so an identical list digest. The OCI spec does not canonicalize
  an index, so member order or an injected annotation could break it. T2
  composes, pushes to a scratch tag, recomposes, and compares list digests.
  If it is not deterministic, C.2's identity becomes the member digest set
  instead of the list digest — the immutability property is preserved either
  way, so this changes the guard's mechanism, not the design.
