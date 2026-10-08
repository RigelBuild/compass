# agent-image-index

Composes the amd64 and arm64 `compass-agent` images into an OCI index, publishes
it to GHCR, and verifies the immutable build tag and `:latest`. It exists to
publish one verified multi-architecture reference from the per-architecture
images.

## Run

From the repository root, with `podman`, `skopeo`, and a populated GHCR auth
file ready:

```sh
export GH_SHA="$(git rev-parse HEAD)"
export RUNNER_TEMP="$(mktemp -d)"
export REGISTRY_AUTH_FILE="/path/to/ghcr-auth.json"
bun tools/agent-image-index/index.ts
```

The `publish-image-manifest` job in `.github/workflows/release.yml` runs this
command after `skopeo login`.

## Credentials

`REGISTRY_AUTH_FILE` names the auth file used by `skopeo` and `podman`. The
`publish-image-manifest` job creates the file with `skopeo login` using
`secrets.GITHUB_TOKEN`; it grants `packages: write`.

## Rotate

GitHub issues `GITHUB_TOKEN` per run and rotates it automatically. CI recreates
the temporary auth file on each run, so there is no long-lived registry token
to rotate.
