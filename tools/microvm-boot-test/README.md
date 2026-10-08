# microvm-boot-test

Runs the local KVM-gated microVM boot suite. It builds the guest image and VMM
binaries with Nix, sets the environment used by the tests, and runs the tagged
Go suite. This is the dev-box entry point; CI runs its own microVM step.

## Run

```sh
moon run compass-go:test-microvm
moon run microvm-boot-test:ci
```

The first command runs the boot suite and needs KVM and Nix. The second runs
this project's typecheck and unit tests. `.github/workflows/ci.yml` job
`microvm` runs the tagged suite directly; the workflow does not invoke this
local runner.

## Credentials

None. The runner builds local Nix outputs and starts Go tests; it reads no
credential or authentication file.

## Rotate

Nothing to rotate.
