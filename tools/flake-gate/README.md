# flake-gate

Checks that the repository flake evaluates and builds its declared outputs.
It also checks the flake and devenv nixpkgs pins for parity and keeps their
version guards aligned.

## Run

```sh
moon run flake-gate:flake-check
moon run flake-gate:ci
```

`ci` runs the flake check, pin-parity check, and version guard. The
`.github/workflows/ci.yml` workflow runs the selected task in its `moon (nix)`
job for affected pull requests and its full main-branch sweeps.

## Credentials

None. These checks build from the checkout and do not require live service
credentials.

## Rotate

Nothing to rotate.
