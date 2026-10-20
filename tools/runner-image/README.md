# runner-image

Builds the `compass-runner` container image from the Nix closure, stages it,
and uses rootless BuildKit with a hardened runtime base. The image carries the
runner, its VMM userland, and the guest assets.

## Run

From the repository root, start a reachable BuildKit daemon and set
`BUILDKIT_HOST`. Create a Docker config with GHCR credentials before publishing:

```sh
moon run compass-runner-image:build
sha12="$(git rev-parse HEAD | cut -c1-12)"
DOCKER_CONFIG="/path/to/docker-config" \
  bun tools/runner-image/publish.ts \
    --lane runner --repo ghcr.io/rigelbuild/compass-runner --sha "$sha12"
REGISTRY_AUTH_FILE="/path/to/ghcr-auth.json" \
RUNNER_IMAGE_CLOSURE_PATHS="<newline-separated paths>" \
  bun tools/runner-image/retag.ts \
    --lane runner --repo ghcr.io/rigelbuild/compass-runner --tag v1.2.3 \
    --release-sha "$(git rev-parse HEAD)"
```

`publish-runner-image` builds the local OCI layout and calls the shared publish
CLI with `--lane runner`. `release-runner-image` calls the shared retag CLI with
`--lane runner`. The latter needs full git history and
`RUNNER_IMAGE_CLOSURE_PATHS` from the workflow environment.

On every main push, `tag-main-images` waits for the runner and server publish
jobs, resolves each lane's newest published ancestor, and copies both images by
digest to `main-<n>-<sha12>`. Here `n` is the head commit's first-parent count.
The job needs full git history and both closure path sets; it uses
`REGISTRY_AUTH_FILE` for destination writes.

Each lane's publish gate (`publish-gate.ts`) compares the head with the newest
published `:git-<sha12>` ancestor, not with the push's `before` sha. So when
merges land back to back, every head whose closure differs from a published
image publishes its own, and no head tags an image older than its closure.

## Credentials

`publish.ts` reads `DOCKER_CONFIG/config.json` for BuildKit and `skopeo` GHCR
auth. The `publish-runner-image` job writes that file using
`secrets.GITHUB_TOKEN`. `retag.ts` uses the auth file named by
`REGISTRY_AUTH_FILE`; the `release-runner-image` job creates it with
`skopeo login` and `secrets.GITHUB_TOKEN`. Both jobs grant `packages: write`.

## Rotate

GitHub issues `GITHUB_TOKEN` per run and rotates it automatically. Both jobs
create temporary registry auth files, so there is no long-lived registry token
to rotate.
