# operator-cli-check

Checks the Compass CLI built by the dogfood task. It verifies that the binary
exists, is executable and newer than its Go source inputs, then runs `--help`.
This catches a missing or stale build before the dogfood server starts.

## Run

```sh
devenv tasks run dogfood:check-cli
moon run operator-cli-check:ci
```

The devenv task builds the CLI first and supplies `DEVENV_STATE`. The moon `ci`
task runs this project's typecheck and unit tests, not the live artifact check.
`.github/workflows/ci.yml` job `moon (bun)` runs those project checks; no
workflow invokes the dogfood task.

## Credentials

None. The checker reads the local devenv state directory and source files.

## Rotate

Nothing to rotate.
