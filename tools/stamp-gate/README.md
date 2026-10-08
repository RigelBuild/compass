# stamp-gate

Checks that generated TypeScript files carry matching `protoc-gen-es` stamps
and that the generator on `PATH` has the same version. This names a toolchain
skew or partial regeneration before it appears as unexplained generated-file
drift.

## Run

```sh
moon run stamp-gate:check
moon run stamp-gate:ci
```

`check` reads generated files under `packages/*/src/gen` and runs
`protoc-gen-es --version`; make sure the generator is on `PATH`. `ci` also runs
typechecking and tests. The `.github/workflows/ci.yml` workflow runs the `ci`
task in its `moon (bun)` job when the concern matrix selects it.

## Credentials

None. The gate reads checked-in generated files and runs a local generator.

## Rotate

Nothing to rotate.
