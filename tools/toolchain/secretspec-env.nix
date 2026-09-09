# The out-of-band secretspec CLI for the Go secrets WRITE path. The resolver
# spawns it BY NAME (go/internal/secrets/resolver.go's `defaultCLI = "secretspec"`,
# exec.CommandContext) for `set`, so the write path is unreachable
# unless the binary is on PATH — and TestSecretSpecCLIVersionFloor
# (go/internal/secrets/resolver_test.go) now FAILS CLOSED rather than skipping
# when exec.LookPath finds nothing, because secrets not working is a failure
# either way. This file is how CI gets that binary.
#
# Realized here rather than through the shared gate-tools.nix / toolchain-parity
# machinery, mirroring skopeo-nix2container-env.nix, for two reasons:
#   - secretspec is a dotted input reference
#     (inputs.secretspec-nixpkgs.legacyPackages.<system>.secretspec), not a bare
#     nixpkgs attr, so it cannot live in devenv.nix's parsed `packages` literal —
#     the toolchain-parity gate resolves every bare attr in that literal,
#     including on macOS, and throws on any non-bare token. That is exactly why
#     devenv.nix:251-253 appends it OUTSIDE the parsed `with pkgs` literal, and
#     why `parity.ts --print-nix-attrs` does not carry it for CI's nixpkgs-attrs
#     step to install.
#   - it is a runtime dependency of one package's test + write path, not a
#     dev-shell CLI whose ambient-vs-pinned PATH drift the parity gate exists to
#     catch, so keeping it out of the parsed attrs is conceptually right.
#
# Pins from the SAME devenv.lock `secretspec-nixpkgs` revision the dev shell
# resolves, so CI runs byte-for-byte the binary a local dev box does, and the
# lock stays the single source of truth (no raw flake-ref literal). That input
# is a SECOND nixpkgs, deliberately not `follows: nixpkgs` (devenv.yaml): the
# shell's own nixpkgs rev still carries secretspec 0.14.0, which has no `age`
# provider compiled in — the encrypted-at-rest default the server-secret
# resolver writes through.
#
# The version at this pin (0.20.0) is what hostcheck.SecretSpecFloor and the
# `secretspec-go` SDK pin in go/go.mod both track, so the read half (SDK +
# native lib) and the write half (this CLI) cannot drift.
#
# One output the ci.yml step reads `bin/secretspec` off, realized with
# `nix build` (never `nix eval`, which strips the store context that would build
# the derivation):
#
#   secretspec  the CLI derivation.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);

  # A github-type node: owner/repo/rev/narHash — the same node SHAPE
  # chromium-e2e-env.nix reads, though a different repo. This one is
  # NixOS/nixpkgs; the `nixpkgs` node is cachix/devenv-nixpkgs, whose rev
  # carries 0.14.0 (no `age` provider), which is why a second input exists.
  node = lock.nodes."secretspec-nixpkgs".locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
in
{
  secretspec = pkgs.secretspec;
}
