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

## Credentials

None. The fork is public, so the build fetches it without a credential. The smoke mints a throwaway gateway token per run and deletes it on exit.

## Rotate

Nothing to rotate.
