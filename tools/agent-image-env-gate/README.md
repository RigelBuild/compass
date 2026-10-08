# agent-image-env-gate

Builds and inspects the `compass-agent` image. It catches leaked development
environment values, build-host paths, and platform mismatches that a successful
image build alone does not detect.

## Run

```sh
moon run agent-image-env-gate:check
moon run agent-image-env-gate:ci
```

`check` builds the image and checks its OCI configuration. `ci` also runs the
typecheck and fixture tests. CI runs the selected task in the `moon (bun)` job
of `.github/workflows/ci.yml`; the check depends on
`compass-agent-image:build`.

## Credentials

None. The gate reads the checkout and local Nix build output; it does not read
a secret or token.

## Rotate

Nothing to rotate.
