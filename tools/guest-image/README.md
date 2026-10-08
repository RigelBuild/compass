# guest-image

Pins the published `compass-agent` image by immutable tag, manifest digest, and
layer descriptors in `guest-image/agent-oci.lock`. It also assembles and
publishes the guest kernel, rootfs, and initrd as one OCI artifact for the
microVM guest.

## Run

From the repository root:

```sh
moon run compass-guest-image:build
moon run compass-guest-image:ci
moon run guest-image:ci
bun tools/guest-image/pin-agent-image.ts --relock
sha12="$(git rev-parse HEAD | cut -c1-12)"
REGISTRY_AUTH_FILE="/path/to/ghcr-auth.json" \
  bun tools/guest-image/publish.ts \
    --repo ghcr.io/rigelbuild/compass-guest-image --sha "$sha12"
```

`.github/workflows/ci.yml` runs `compass-guest-image:ci` in its `moon (nix)` job
and `guest-image:ci` in its `moon (bun)` job. The `publish-guest-image` job in
`.github/workflows/release.yml` publishes the artifact. The `renovate` job in
`.github/workflows/renovate.yml` runs `--relock` after an agent-image pin update.

## Credentials

`pin-agent-image.ts` reads the public agent image anonymously. The publisher
uses `REGISTRY_AUTH_FILE` for GHCR access. CI creates it with `skopeo login`
using `secrets.GITHUB_TOKEN` in the `publish-guest-image` job, which grants
`packages: write`.

## Rotate

GitHub issues `GITHUB_TOKEN` per run and rotates it automatically. The workflow
recreates the temporary registry auth file on each run; no long-lived token
needs rotation.
