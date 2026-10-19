# Release and images

What `.github/workflows/release.yml` publishes, and how a release is cut. The
CI gate that runs before any of this is in [Build & CI](build-and-ci.md). The
frozen records are
`RigelBuild/docs:docs/designs/compass/infra/release/compass-unified-release-lane/design.md`
(the release lane) and
`RigelBuild/docs:docs/designs/compass/infra/release/compass-distribution/design.md`
(the distribution surface).

## Publishing the agent image

The `compass-agent` runtime image is the base every agent workstream runs in.
It is `ghcr.io/rigelbuild/compass-agent`, and it has three tags:

- `:git-<sha12>` is the 12-hex short commit sha. It is **immutable**. The native
  app bakes it in and hands it to the runner via `--image` /
  `$COMPASS_AGENT_IMAGE`, so it is the real consumption path. The publish
  refuses to overwrite an existing `:git-<sha>` with different content, and
  re-inspects after each push to assert the digest landed.
- `:latest` moves. It is a documented **first-run fallback only**, never the
  default.
- `:vX.Y.Z` is the **immutable** semver tag. The `release-image` job adds it
  when a release is cut, by re-tagging the published `:git-<sha>` digest. There
  is no rebuild, so the released image is byte-identical to the per-push one.

Every closure-affecting push to main publishes `:git-<sha12>`, then `:latest`,
so the immutable pin exists before the moving tag moves. The image is
multi-arch: `publish-image-amd64` and `publish-image-arm64` build natively in
parallel, and `publish-image-manifest` pushes one `linux/amd64` +
`linux/arm64` index. The package is **public**: the payload is public source
and carries no runtime secrets, since the runner supplies those per exec. So the
first-run pull needs no credential.

The published image and the local `dogfood:agent-image` load come from the
*same* nix derivation, through the image's `container build agent`. Only the
skopeo destination differs (a registry ref versus `containers-storage:`), so
what CI publishes is byte-for-byte what a developer loads locally.

**The release lane.** Publishing is not a job in `ci.yml` and not a required
check.

- The agent-image publish jobs run only on main, on a push or a
  `workflow_dispatch`. They check the image closure's changed paths in the job,
  not with a trigger `paths:` filter, because release-pr needs every commit.
- `publish-image-manifest` is the only writer of `:git-<sha12>` and `:latest`.
  Its concurrency group, shared with `release-image`, sets
  `cancel-in-progress: false` and `queue: max`, so up to 100 pending runs wait
  in order and a burst of pushes never drops a pending release tag.
- The publish jobs get a `packages: write` token that the `ci.yml` gate jobs
  never get.
- `release.yml` lists no moon projects, like `ci.yml`, because a list goes stale
  silently. `agent-image/` is a standalone devenv, not a moon project.

A published tag is the source of truth. A missing per-push tag is **not** a
failure: the paths check found no closure change. A `workflow_dispatch` on main
republishes main's HEAD on demand.

The same workflow runs `publish-runner-image`, `publish-server-image`,
`publish-guest-image` and `publish-gateway-image`, plus the
`release-runner-image` and `release-server-image` re-tags. The gateway job
builds and smoke-tests one local OCI layout, publishes its write-once
`:git-<sha12>` tag, and records the `repo@sha256:…` identity to pin in the step
summary.

The weekly `agent-image-retention` workflow prunes old `git-*` versions outside
the window. It keeps release tags, off-main images, lock-pinned digests, index
children and recent untagged publishes. Dispatch defaults to a dry-run plan, and
GitHub keeps deleted versions restorable for 30 days.

**Smoke.** On a runner host, pull the immutable tag and drive the consumer seam:

```sh
podman pull ghcr.io/rigelbuild/compass-agent:git-<sha12>
compass-runner --image ghcr.io/rigelbuild/compass-agent:git-<sha12>
# then drive one provision
```

**One-time setup.** The first `GITHUB_TOKEN` push creates the package as
private, and only if the org policy allows packages created by `GITHUB_TOKEN`.
Otherwise the push returns 403 and an owner must pre-create the package. After
the first push, an owner sets the package **public** once; the repo link gives
the workflow write access from then on. Pre-creating the empty package also
settles the first publish: the immutability guard frees a tag only on an
authoritative `manifest unknown`. A package that already exists returns that
answer cleanly, not a missing repository's error.

### Pre-merge build check

`agent-image/` is the `compass-agent-image` moon project, so CI builds the image
on any PR that affects its closure. A build break blocks merge, not a later
publish.

The `build` task builds the image with the same pinned devenv derivation the
publish lane ships: `nix run "$src" -- container build agent`. `$src` is the
devenv flakeref that `tools/toolchain/devenv-cli` resolves from
`agent-image/devenv.lock`. So a green build catches both an eval-time break
(bun pin drift against the `agent-image/toolchain.nix` assert) and a build-time
break (an `agent-image/entrypoint.nix` FOD-hash change or a broken bundle).

The Compass agent's compile script derives the SDK's lazy legacy-module
registry from installed package exports and supplies it through a local Bun
plugin. If the SDK adds a supported root option, Compass can use that option and
remove the plugin.

The image closure is the main CI cost, but it does not set the `moon` job's 90m
timeout. A PR pays it only when it touches the task's `inputs`: the
`agent-image/` tree, `packages/compass-agent/`, the root `package.json` and
`bun.lock`, and `tools/toolchain/versions/bun.nix`, since the image builds bun
from that pin. The `publish-image` path check in `release.yml` uses the same set
plus `release.yml` itself. Every push to main builds it. As a `ci-group.nix`
project it runs in the `moon (nix)` leg behind `rollup`.

## Cutting a release

`release.yml` runs release-please. It keeps a standing Release PR that batches
conventional commits since the last release. Merging that PR is the release: it
cuts the `vX.Y.Z` tag, updates the CHANGELOG, and stamps the root `version.txt`
to `X.Y.Z`.

The tag gates the release jobs. The binaries are built stamped from
`version.txt`, so every released binary reports the same `X.Y.Z`, and
`release-image` mints the `:vX.Y.Z` image tag. Dev and bundle builds from
`app-bundle/build.sh` read the same `version.txt` and append `+g<shortsha>`, so a
local bundle reports the release it descends from.

## Release assets and install channels

`release-pr` cuts the tag and creates the GitHub Release as a **draft**.
`release-assets` builds the Linux assets, collects the macOS dmg from
`release-assets-macos`, attaches the set, and then publishes the draft as its
last step. A published release is immutable, so a release published before its
assets exist could never get them.

| Asset | Built by | Notes |
| --- | --- | --- |
| `compass`, `compass-server`, `compass-runner`, `compass-stack` (`_linux-amd64`) | `release-assets` | static (`CGO_ENABLED=0`) |
| `compass_<tag>_darwin-arm64` | `release-assets` | the CLI, cross-built |
| `compass-app_<tag>_linux-amd64.tar.gz` | `release-assets` | gtk4 shell, the embedded sidecars, UI dist |
| `compass-app_<tag>_darwin-arm64.dmg` | `release-assets-macos` (`macos-14`) | ad-hoc signed `.app`; handed over as an artifact |
| `SHA256SUMS` | `release-assets` | checksums of the seven downloads above |
| `nix-outputs.json` | `release-assets` | nix output manifest; not in `SHA256SUMS` |

Every asset in `SHA256SUMS` gets a build-provenance attestation from
`actions/attest-build-provenance`. The tag ruleset blocks a forged tag; the
attestation lets a user detect an asset the release workflow did not build.

The macOS app links the system WebKit framework, so it builds only on a mac
runner. Developer ID signing, notarization and the app's homebrew cask wait on
the Apple Developer Program enrollment.

**Homebrew.** After a semver release publishes, `bump-homebrew-tap` renders
`Formula/compass-cli.rb` with `tools/homebrew-tap` from the tag and the
published `SHA256SUMS`. It commits the formula to `RigelBuild/homebrew-tap`
with an App token scoped to that repo. `brew install rigelbuild/tap/compass-cli`
installs the CLI on Apple-silicon macOS and x86_64 Linux.

- Prerelease tags never reach the job, and the renderer rejects any tag that is
  not `vX.Y.Z`.
- The job skips the write when the tap pins the same or a newer version, so an
  out-of-order release cannot downgrade it.
- If it fails, re-run that job alone. It reads only the published release;
  re-running `release-assets` would try to upload to a published release.

**Nix flake.** `flake.nix` exposes `compass`, `compass-server`,
`compass-runner`, `compass-stack`, `compass-app`, `compass-ui` and
`compass-stack-env`, for `x86_64-linux` only. `compass-stack-env` bundles the
microVM userspace trio and `secretspec`. The `flake-gate` moon project runs
`nix flake check`, so a package that stops building from a bare checkout fails
CI.

**Postgres image.** The stack's default database is the stock `postgres:18`
image pinned by digest (`DefaultPostgresImage` in
`go/internal/stack/postgres_image.go`). There is no postgres build lane;
bumping it means changing the digest.
