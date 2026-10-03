# Compass gateway image publish lane

Tracking: RIG-4209 (decision: option B, a compass publish lane). Consumer:
RIG-2862 (the stack's supervised gateway child, compass PR #1382).

## Problem / Intent

The installed stack runs the LLM gateway as a podman child, but
`DefaultGatewayImage` in `go/internal/stack/gateway_image.go` is `""`, so
`compass-stack up` needs `--gateway-image` or `--gateway-external`. No
published gateway image exists. The gateway is the `auth-gateway serve`
command of the RigelBuild/oh-my-pi fork, booted by
`packages/coding-agent/src/cli/gateway-boot.ts` and packaged by the fork's
`Dockerfile.gateway`. This record makes compass build that image from a pinned
fork commit, publish it to GHCR by digest, and hand the digest to a reviewed
pin of `DefaultGatewayImage`. This is the same pin rule as
`DefaultCollectorImage`.

## Approach

Add a new lane, `tools/gateway-image/`, that follows the shape of the runner and
guest lanes: a pure core with tests and a thin I/O shell, both in Bun
TypeScript. A new `publish-gateway-image` job in `release.yml` drives it. The
lane makes six decisions.

1. **Fork pin.** `tools/gateway-image/fork-pin.json` holds
   `{ "repo": "https://github.com/RigelBuild/oh-my-pi.git", "commit": "<40-hex>" }`.
   A bump is a reviewed compass PR that edits this file. The fork is public, so
   CI fetches it with no credential:
   - `git init`, then `git fetch --depth=1 origin <commit>`, then check out
     `FETCH_HEAD`, into a temp directory.
   - Then the lane fetches fork `main` with `--filter=tree:0` and
     `git merge-base --is-ancestor`. Together they assert that the commit is on
     fork `main`.

   The ancestry check refuses a pin on an unmerged PR branch. That branch could
   be force-pushed away, and its code has no fork review. The fork has no LFS
   files and no submodules, so a shallow checkout is the complete build
   context. Build from source, not npm: fork npm releases wait on RIG-3149
   (RIG-4209).
2. **Two-stage BuildKit build.** The fork ships two Dockerfiles, and
   `Dockerfile.gateway` starts `FROM ${PI_BASE}` (default `oh-my-pi/pi:dev`).
   The lane runs two rootless `buildctl` solves on one buildkitd:
   - **Stage 1:** the fork `Dockerfile` with target `pi-runtime`, exported to a
     local OCI layout. This stage compiles the Rust natives addon.
   - **Stage 2:** `Dockerfile.gateway`, with the base supplied as a named
     context:
     `--oci-layout pibase=<layout> --opt context:oh-my-pi/pi:dev=oci-layout://pibase@<digest>`.

   The base never leaves the runner. Nothing is published except the gateway
   image. Both solves use the runner lane's reproducibility settings:
   `SOURCE_DATE_EPOCH=1`, `rewrite-timestamp=true`, `oci-mediatypes=true`, and
   platform `linux/amd64`. A local probe on 2026-10-03 at fork commit
   `91ad19fbe19c` (PR #108 head) tested the mapping with buildkit 0.32.2. Two
   stage-2 solves gave the same digest, and that digest equals
   `containerimage.digest` in the metadata file.
3. **Push the bytes that were smoked.** The guest lane pushes with
   `skopeo copy` from a local layout. The gateway lane copies that shape, not
   the runner lane's re-solve with the push exporter. The order is:
   1. Build once to an OCI layout.
   2. Secret-scan the image config.
   3. Smoke-boot the image (item 4).
   4. Run `skopeo copy --preserve-digests oci:<layout> docker://<repo>:git-<sha12>`.
   5. Re-read the manifest from the registry and assert that its digest equals
      the local digest.

   No second build exists, so a reproducibility drift cannot publish a digest
   that was never smoked. A tag that already holds the same digest is a no-op.
   A tag that holds a different digest aborts the run. That is the guest
   lane's `tagDisposition` rule.
4. **Smoke before push.** The lane loads the image into podman with
   `skopeo copy oci:<layout> docker-archive:<tar>` and then `podman load -i`.
   The bare `ubuntu-latest` runner denies the nested unshare that a direct
   copy into containers-storage needs. The `e2e` job in `ci.yml` documents
   this and loads its seed image the same way. Then the lane boots the image
   the way the stack runtime contract does (RIG-2862):
   - a 0600 token file mounted at `/run/compass/gateway.token`;
   - port `4000` published to loopback;
   - the image's own entrypoint and CMD.

   The checks are:
   - `/healthz` answers 200 with `{"ok":true,…}` within 30 s. This is the
     budget of `waitGateway` in the stack.
   - `/v1/models` without a token answers 401.
   - `podman stop -t 25` ends with exit code 143.

   `auth-gateway serve` exits 1 when `OMP_AUTH_BROKER_URL` is not set. (A
   local run at the same commit confirmed this, and the T1 comment in
   `gateway-boot.ts` says so.) The smoke therefore starts a broker from the
   same image first:
   - The broker runs `omp auth-broker serve --bind=0.0.0.0:8765` on a private
     podman network.
   - The lane waits for its `/v1/healthz`.
   - It reads the broker's minted token with `podman exec` from
     `/tmp/.omp/auth-broker.token`. The image sets `HOME=/tmp`, so the config
     root is `/tmp/.omp`.
   - It passes `OMP_AUTH_BROKER_URL` to the gateway, and `OMP_AUTH_BROKER_TOKEN`
     as a name-only `-e` whose value comes from the environment, which keeps
     it off argv.

   A local run of this broker + gateway pair gave `/healthz` 200 after 2.5 s,
   401 without a token, and exit 143 on SIGTERM.
5. **No reuse of runner-image publish-core.** `secretConfigViolations` in that
   file rejects two benign names in the gateway config:
   - `GPG_KEY`, a public-key fingerprint set by the `python` base image;
   - `COMPASS_GATEWAY_TOKEN_FILE`, a path.

   Adding an allowlist to it would change a gated lane that gains nothing from
   the change. There are no cross-tool imports today: the guest lane copied
   the shape, and only `../toolchain/` is shared. So the gateway core carries
   its own copy of the scan, with an exact-name allowlist of those two names,
   and copies the few helpers it needs (`buildTag`, `digestRef`, the `EXIT`
   numbering).
6. **Tags and pin.** The lane writes only `:git-<sha12>`, where `sha12` is the
   compass commit. It writes no `:latest` and runs no `:vX.Y.Z` retag. The
   stack consumes a digest compiled into the release binary, so a version tag
   on the image would add nothing. The digest reaches `DefaultGatewayImage`
   through a reviewed manual pin PR, the same as `DefaultCollectorImage`.
   Renovate cannot order `git-<sha12>` tags, and no CI identity here may open
   PRs. The job writes `repo@sha256:…` to the step summary, and the pin PR
   copies it from there. A compass commit maps to its fork commit through the
   `fork-pin.json` at that compass commit, so the image needs no extra labels.

Change detection uses a new `GATEWAY_IMAGE_CLOSURE_PATHS` set
(`tools/gateway-image/**` and `.github/workflows/release.yml`) and the same
push-diff gate step as `publish-runner-image`. Every fallback of that gate errs
toward publishing. The pin is a file inside `tools/gateway-image/`, so a fork
bump is a closure change by construction.

Alternative considered: run the build in the fork's own CI and only pin the
result in compass (RIG-4209 option A). Matt ruled for option B.

## Plan

### Global Constraints

- Bun TypeScript only (the no-bash-gate rule). Keep the pure core (`core.ts` +
  `core.test.ts`) apart from the I/O shells. Import nothing from another
  `tools/*` lane; `../toolchain/` is the only shared import.
- Build with rootless BuildKit through `buildctl`. Never use `docker build`
  and never mount a host docker socket. Build `linux/amd64` only. Use
  `SOURCE_DATE_EPOCH=1` and `rewrite-timestamp=true` on every output.
- The deployed reference is always `repo@sha256:<64-hex>`. A tag only
  addresses a build. The lane writes `:git-<sha12>` and no other tag. `sha12`
  is the first 12 characters of the compass commit (`cut -c1-12`, never
  `--short`).
- Repo: `ghcr.io/rigelbuild/compass-gateway` (an assumption, OQ-2).
- `fork-pin.json` `commit` is 40 lowercase hex characters and an ancestor of
  RigelBuild/oh-my-pi `main`.
- Exit codes, numbered as in the runner and guest lanes: usage 2,
  secretFound 3, pushFailed 4, digestMismatch 5, badLayout 6, plus smokeFailed
  7 and badPin 8.
- No retries anywhere. A fault fails the step, and the remedy is a re-run.
- Prerequisite: fork PR #108 and its base PR are merged to fork `main`.
  Until then no commit satisfies the ancestry rule.
- compass is public. Cite only compass, the public fork, and Linear IDs.

### T1 — Lane scaffold and pure core

Create `tools/gateway-image/` with `package.json` (`@compass/gateway-image`,
private), `tsconfig.json`, `biome.json`, and `moon.yml`. Copy the guest lane's
`typecheck` / `test` / `ci` tasks. Its `build`, `smoke`, and `publish` tasks
use `runInCI: false`. Also add:
- `fork-pin.json`;
- `core.ts` and `core.test.ts`;
- the `gateway-image: 'tools/gateway-image'` entry in `.moon/workspace.yml`;
- `/tools/gateway-image/out/` in `.gitignore`.

Interfaces (`core.ts`):
- `EXIT` — the constant described in Global Constraints.
- `type ForkPin = { repo: string; commit: string }`;
  `parseForkPin(text: string): ForkPin`. It throws on unknown keys, on a
  `repo` other than `https://github.com/RigelBuild/oh-my-pi.git`, and on a
  `commit` that is not 40 lowercase hex characters.
- `BASE_CONTEXT = "oh-my-pi/pi:dev"`, which must equal the default of
  `ARG PI_BASE` in `Dockerfile.gateway`. `PLATFORM = "linux/amd64"`.
  `SOURCE_DATE_EPOCH = 1`.
- `baseBuildArgs(forkDir: string, ociDir: string): string[]` — the stage-1
  `buildctl` argv: frontend `dockerfile.v0`, `filename=Dockerfile`,
  `target=pi-runtime`, platform, `build-arg:SOURCE_DATE_EPOCH`, output
  `type=oci,dest=<ociDir>,tar=false,rewrite-timestamp=true`.
- `gatewayBuildArgs(forkDir: string, baseOciDir: string, baseDigest: string, ociDir: string, metadataFile: string): string[]`
  — the stage-2 argv with `filename=Dockerfile.gateway`,
  `--oci-layout pibase=<baseOciDir>`, and
  `context:oh-my-pi/pi:dev=oci-layout://pibase@<baseDigest>`. The output is
  the same OCI spec with `oci-mediatypes=true`, plus `--metadata-file`.
- `layoutDigest(indexJson: string): string` — the single sha256 manifest an
  OCI index names. Any other shape throws.
- `SECRET_NAME_ALLOWLIST = ["GPG_KEY", "COMPASS_GATEWAY_TOKEN_FILE"]`;
  `secretConfigViolations(config: { env: readonly string[]; labels: Readonly<Record<string, string>> }): string[]`
  — the runner lane's pattern, with names in the allowlist skipped only on an
  exact match.
- `buildTag(repo: string, sha12: string): string`,
  `digestRef(repo: string, digest: string): string`,
  `tagDisposition(probe: { exitCode: number; stdout: string; stderr: string }, localDigest: string): { action: "publish" | "skip" | "abort"; reason?: string }`
  — the guest lane's semantics, copied.
- `healthzOk(status: number, body: string): boolean` — true only when the
  status is 200 and the JSON body has `ok === true`.

Tests: for each rejection in `parseForkPin`, a test shows it fails. A real
secret name such as `OMP_AUTH_BROKER_TOKEN` is still flagged. A near-miss of
an allowlisted name (`GPG_KEY_X`) is still flagged. An existing tag with a
different digest aborts. An ambiguous probe aborts.

### T2 — Build and smoke shells

Interfaces:
- `bun tools/gateway-image/build.ts [--out <dir>]` (default
  `tools/gateway-image/out`). It needs `BUILDKIT_HOST`. It reads
  `fork-pin.json` and fetches the commit shallowly into `<out>/src`. It
  asserts ancestry against fork `main` and exits 8 if the commit is not an
  ancestor. Then it runs stage 1 into `<out>/base-oci` and stage 2 into
  `<out>/oci`. It asserts that `containerimage.digest` in the metadata equals
  `layoutDigest(<out>/oci/index.json)` and exits 6 if they differ. It prints
  the digest on stdout.
- `bun tools/gateway-image/smoke.ts [--out <dir>]`. It needs `skopeo` and
  `podman`. It loads `<out>/oci` through a `docker-archive` tar with
  `podman load`, then runs the broker + gateway smoke from Approach item 4.
  It names containers and the network with a per-run suffix and always tears
  them down. It exits 0 on pass and 7 on any failed check.

Test cycle: on a host with rootless buildkitd and podman, run `build.ts`
twice; the two digests are the same. `smoke.ts` passes. To show the smoke can
fail, run it once with the broker env withheld; it must exit 7.

### T3 — Publish shell and release job

Interfaces:
- `bun tools/gateway-image/publish.ts --repo <repo> --sha <sha12> [--out <dir>]`.
  It honours `REGISTRY_AUTH_FILE`. In order, it:
  1. Scans the `<out>/oci` image config `Env` and `Labels`, and exits 3 if any
     name is flagged.
  2. Probes the tag and applies `tagDisposition`.
  3. Runs `skopeo copy --preserve-digests oci:<out>/oci docker://<tag>`.
  4. Re-reads the tag with `skopeo inspect --raw`, and exits 5 if the digest
     differs from the local one.
  5. Prints `digestRef` on stdout.
- `.github/workflows/release.yml` changes:
  - A new env block, `GATEWAY_IMAGE_CLOSURE_PATHS`, with
    `tools/gateway-image/**` and `.github/workflows/release.yml`.
  - A new job, `publish-gateway-image`, that copies `publish-runner-image`:
    - permissions `contents: read`, `packages: write`;
    - concurrency group `publish-gateway-image`, `cancel-in-progress: false`,
      `queue: max`;
    - `if: github.ref == 'refs/heads/main'` and `timeout-minutes: 90`.
  - Its steps are:
    1. the gate step over the new set;
    2. pinned bun and skopeo from `tools/toolchain/`;
    3. rootless buildkitd;
    4. `REGISTRY_AUTH_FILE` with `skopeo login` (the guest job's login);
    5. `build.ts`, `smoke.ts`, then `publish.ts --repo
       ghcr.io/rigelbuild/compass-gateway --sha <sha12>`;
    6. the ref appended to `GITHUB_STEP_SUMMARY`.

Test cycle: `core.test.ts` covers every branch of the publish decision. The
first main push after merge publishes the image. Then `skopeo inspect --raw`
on `:git-<sha12>` must give the digest the summary shows.

### Out of scope — the first pin

Pinning `DefaultGatewayImage` is the stack lane's change, not this lane's.
It waits on PR #1382 merging and on RIG-4251: how the stack gives the
gateway its broker credentials. `auth-gateway serve` exits 1 without
`OMP_AUTH_BROKER_URL`, and PR #1382 passes only the three `COMPASS_GATEWAY_*`
vars. So a pinned default cannot boot until RIG-4251 is decided. This lane
builds and publishes the same image under every RIG-4251 option.

What this lane hands that pin:

- the `repo@sha256:…` ref in the `publish-gateway-image` step summary;
- a bump procedure for the doc comment, in the style of `collector_image.go`:
  edit `fork-pin.json`, merge, copy the ref from the summary, open a pin PR;
- provenance: the compass commit, and through its `fork-pin.json`, the fork
  commit.

Before the first pin, Matt sets the GHCR package to public once, as for the
compass-agent image, so the stack pulls with no registry login.

## Tasks

- [ ] T1 — `tools/gateway-image/` scaffold, `fork-pin.json`, `core.ts` and
      tests, moon and `.gitignore` registration
- [ ] T2 — `build.ts` (fork fetch, ancestry check, two-stage solve) and
      `smoke.ts` (broker + gateway boot, `/healthz`, 401, exit 143)
- [ ] T3 — `publish.ts` and the `publish-gateway-image` job with
      `GATEWAY_IMAGE_CLOSURE_PATHS`
- [ ] DECISIONS.md rows DL-386 and DL-387 in the same PR as this record

## Open Questions

- **OQ-2 — Package name and visibility.** Not load-bearing.
  `ghcr.io/rigelbuild/compass-gateway` is the RIG-4209 proposal, not yet
  confirmed. Public visibility follows the compass-agent image ruling.
  Renaming before T3 lands costs one string.
