# Release assets and install channels

The frozen record
`RigelBuild/docs:docs/designs/compass/infra/release/compass-distribution/design.md` defines the
distribution surface. This section lists what is live.

**The Release.** `release-pr` cuts the `vX.Y.Z` tag and creates the GitHub
Release as a **draft**. `release-assets` builds the Linux assets, collects the
macOS dmg from `release-assets-macos`, attaches the set, then publishes the
draft as its last step. Releases are immutable once published, so a release
published before its assets exist could never get them.

| Asset | Built by | Notes |
| --- | --- | --- |
| `compass`, `compass-server`, `compass-runner`, `compass-stack` (`_linux-amd64`) | `release-assets` | static (`CGO_ENABLED=0`) |
| `compass_<tag>_darwin-arm64` | `release-assets` | the CLI, cross-built |
| `compass-app_<tag>_linux-amd64.tar.gz` | `release-assets` | gtk4 shell, the embedded sidecars, UI dist |
| `compass-app_<tag>_darwin-arm64.dmg` | `release-assets-macos` (`macos-14`) | ad-hoc signed `.app`; handed over as an artifact |
| `SHA256SUMS` | `release-assets` | checksums of the seven downloads above |
| `nix-outputs.json` | `release-assets` | nix output manifest; not in `SHA256SUMS` |

Every asset listed in `SHA256SUMS` gets a build-provenance attestation from
`actions/attest-build-provenance` in `release-assets`. The tag ruleset blocks a
forged tag; the attestation lets a user detect an asset the release workflow
did not build.

The macOS app links the system WebKit framework, so it builds only on a mac
runner and is never cross-compiled. Developer ID signing, notarization and the
app's homebrew cask are not live yet. They wait on the Apple Developer Program
enrollment.

**Homebrew.** After `release-assets` publishes a semver release, the
`bump-homebrew-tap` job renders `Formula/compass-cli.rb` with
`tools/homebrew-tap` from the tag and the published `SHA256SUMS`, then commits
it to `RigelBuild/homebrew-tap` with an App token scoped to that repo.
`brew install rigelbuild/tap/compass-cli` installs the `compass` CLI on
Apple-silicon macOS and on x86_64 Linux. Prerelease tags never reach the job,
and the renderer rejects any tag that is not `vX.Y.Z`. The job skips the write
when the tap already pins the same or a newer version, so an out-of-order
release cannot downgrade it. If the job fails, re-run that job alone: it reads
only the published release, while re-running `release-assets` would try to
re-upload to the published release.

**Nix flake.** `flake.nix` exposes `compass`, `compass-server`,
`compass-runner`, `compass-stack`, `compass-app`, `compass-ui` and
`compass-stack-env`, for `x86_64-linux` only. `compass-stack-env` bundles the
microVM userspace trio and `secretspec`. The `flake-gate` moon project runs
`nix flake check`, so a package that stops building from a bare checkout fails
CI.

**darwin CI.** The `ci.yml` `darwin` job compiles the macOS shell and builds
the ad-hoc signed `.app`/`.dmg` with `tools/macos-bundle` on `macos-14`. It
runs on every push to main and nightly. On a PR it runs only when the setup
job's `darwin_affected` output is true (computed by `tools/ci-matrix` from the
changed paths).

**arm64 agent FOD pin.** `agent-image/entrypoint.nix` pins one node_modules
`outputHash` per system. The `renovate:agent-fod` task checks the host's pin,
so the `ci.yml` `agent-fod-arm64` job reruns the same script on
`ubuntu-24.04-arm` for the `aarch64-linux` pin. On a PR it runs only when the
setup job's `agent_fod_affected` output is true (moon reports
`renovate:agent-fod` affected). `refresh-fod-hashes.ts` rewrites both pins
from any host, because each FOD selects its arch with `bun install --cpu`.

**Postgres image.** The stack's default database is the stock `postgres:18`
image pinned by digest (`DefaultPostgresImage` in
`go/internal/stack/postgres_image.go`). There is no postgres build lane;
bumping it means changing the digest.
