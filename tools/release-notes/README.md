# release-notes

Generates the release body appendix and `nix-outputs.json` manifest. It records
the release assets, the public GHCR image digest, and the pinned Nix toolchain
output identities.

## Run

```sh
moon run release-notes:ci
bun run tools/release-notes/index.ts \
  --sha 0123456789ab \
  --version 1.2.3 \
  --tag v1.2.3 \
  --asset compass_1.2.3_linux-amd64 \
  --dry-run
```

The `ci` task runs typecheck and unit tests. `.github/workflows/release.yml`
job `release-assets` runs the generator and writes the release body and
manifest. `.github/workflows/ci.yml` job `moon (bun)` runs the project checks.

## Credentials

The generator reads no token or auth file. Without `--image-digest` it queries
the public GHCR image anonymously with `skopeo`; CI passes the digest and skips
that query. In `release.yml`, the later asset-upload step sets
`GH_TOKEN` from `secrets.GITHUB_TOKEN`; the generator does not receive it.

## Rotate

Nothing to rotate for this tool. The separate workflow `GITHUB_TOKEN` is
per-run and auto-rotated by GitHub.
