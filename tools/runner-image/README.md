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
    --repo ghcr.io/rigelbuild/compass-runner --sha "$sha12"
REGISTRY_AUTH_FILE="/path/to/ghcr-auth.json" \
RUNNER_IMAGE_CLOSURE_PATHS="<newline-separated paths>" \
  bun tools/runner-image/retag.ts \
    --repo ghcr.io/rigelbuild/compass-runner --tag v1.2.3 \
    --release-sha "$(git rev-parse HEAD)"
```

The `publish-runner-image` job in `.github/workflows/release.yml` builds the
local OCI layout and runs the publish script. The `release-runner-image` job
runs `retag.ts` to copy a verified image to the release tag. `retag.ts` needs
full git history and `RUNNER_IMAGE_CLOSURE_PATHS`, the closure path list set in
`release.yml`'s workflow-level `env`.

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
