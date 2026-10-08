# devenv-cli

Resolves the devenv CLI source from a named `devenv.lock`, so workflows never
carry a hand-pinned devenv rev. `flakeref` mode prints the
`github:<owner>/<repo>/<rev>#devenv` ref; `bin-dir` mode builds it and prints a
directory holding a single `devenv` binary.

## Run

```sh
bun tools/toolchain/devenv-cli/index.ts --lock agent-image/devenv.lock --mode flakeref
bun tools/toolchain/devenv-cli/index.ts --lock devenv.lock --mode bin-dir
moon run devenv-cli:ci
```

`.github/workflows/ci.yml` uses `flakeref` mode in the e2e job to seed the agent
image. `.github/workflows/renovate.yml` uses `bin-dir` mode to put the root
lock's devenv on `PATH`.
`ci` runs typecheck and tests in the `moon (bun)` job.

## Credentials

None. It reads a lock file and builds from a public flake ref.

## Rotate

Nothing to rotate.
