# gateway-image

Builds and smokes the LLM gateway image from the commit pinned in
`fork-pin.json`. The build exports a local OCI layout and blocks images whose
config contains credential-shaped names.

## Run

From the repository root:

```sh
BUILDKIT_HOST=<rootless-buildkitd-address> bun tools/gateway-image/build.ts
bun tools/gateway-image/smoke.ts
```

Both commands accept `--out <dir>`; the default is `tools/gateway-image/out`.
The build needs a reachable rootless BuildKit daemon and `BUILDKIT_HOST`. The
smoke needs `skopeo` and `podman`.

`moon run gateway-image:ci` runs the lane's typecheck and tests. The build and
smoke tasks are not part of CI.

## Publish

After building and smoke-testing the same output directory, publish its OCI
layout with `bun tools/gateway-image/publish.ts --repo ghcr.io/rigelbuild/compass-gateway --sha <sha12> [--out <dir>]`.
The default output is `tools/gateway-image/out`. The command requires `skopeo`
and honors `REGISTRY_AUTH_FILE`; it prints the verified `repo@sha256:…` ref.
It refuses to overwrite an existing tag with different content.

## Credentials

Build and smoke need no registry credentials: the fork is public, and the smoke
mints a throwaway gateway token per run and deletes it on exit. Publish needs
GHCR write access through `REGISTRY_AUTH_FILE`.

## Rotate

Nothing to rotate.
