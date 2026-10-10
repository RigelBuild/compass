# toolchain

Keeps the dev shell and CI on the same pinned tool closures. The version pins
and `toolchain-tools.nix` define Bun, Node, and Moon; `gate-tools.nix` exposes
the language toolchains, pinned nixpkgs tools, and Meissa's linters (biome,
rumdl, and the rumdl base policy, from the `meissa` input in `devenv.lock`) as
CI build and identity sets. The directory also contains reusable Nix
environments for GTK, Chromium, microVM, secretspec, and patched skopeo, plus
TypeScript parity and version checks.

## Run

```sh
moon run toolchain-parity:ci
bun tools/toolchain/parity.ts
```

The moon `ci` task runs typecheck, tests, and the PATH parity check. The
`.github/workflows/ci.yml` workflow runs this gate in its `moon (bun)` job and
also checks parity before the moon battery. Other workflow jobs use the Nix
helpers to realize their pinned tool closures.

## Credentials

None. The parity check compares commands on `PATH` with Nix store paths and
reads the checkout's configuration and lock files.

## Rotate

Nothing to rotate.
