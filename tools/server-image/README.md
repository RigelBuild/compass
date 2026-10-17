# server-image

Builds the `compass-server` OCI image from its realised Nix closure. The image
uses the pinned distroless static base and runs the wrapper as uid 65532.

## Build

Start a reachable BuildKit daemon and set `BUILDKIT_HOST`, then run:

```sh
moon run compass-server-image:build
sha12="$(git rev-parse HEAD | cut -c1-12)"
DOCKER_CONFIG="/path/to/docker-config" \
  bun tools/runner-image/publish.ts \
    --lane server --repo ghcr.io/rigelbuild/compass-server --sha "$sha12"
REGISTRY_AUTH_FILE="/path/to/ghcr-auth.json" \
SERVER_IMAGE_CLOSURE_PATHS="<newline-separated paths>" \
  bun tools/runner-image/retag.ts \
    --lane server --repo ghcr.io/rigelbuild/compass-server --tag v1.2.3 \
    --release-sha "$(git rev-parse HEAD)"
```

`publish-server-image` builds the local OCI layout, scans it, then calls the
shared publish CLI with `--lane server`. `release-server-image` calls the shared
retag CLI with `--lane server` to copy the verified digest to the release tag.

## Credentials

The publish job reads GHCR credentials through `DOCKER_CONFIG/config.json` and
uses `skopeo` with that same file. The release job logs in through
`REGISTRY_AUTH_FILE`. Both files use a per-run `GITHUB_TOKEN`.
