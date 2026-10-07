# The out-of-band secretspec CLI for the Go secrets paths. The server's secret
# read path (internal/secrets SpecResolver: `export`, `check`) spawns the CLI
# BY NAME, so it is unreachable unless the binary is on PATH; the armed
# forge-secret pgtest fails closed without it. This file is how CI gets it.
#
# Realized here rather than through gate-tools.nix (mirroring
# skopeo-nix2container-env): secretspec is a dotted input reference, not a bare
# nixpkgs attr, so it cannot live in devenv.nix's parsed `packages` literal, and
# it is a runtime dependency of one package's test/read path, not a dev-shell
# CLI whose PATH drift the parity gate catches.
#
# Pins from the SAME devenv.lock `secretspec-nixpkgs` rev the dev shell resolves.
# That input is a SECOND nixpkgs, deliberately not `follows: nixpkgs`: the shell's
# own rev still carries secretspec 0.14.0, which has no `age` provider (the
# encrypted-at-rest default). The pin (0.20.0) is what hostcheck.SecretSpecFloor
# tracks.
#
# One output, realized with `nix build` (never `nix eval`):
#   secretspec     the CLI derivation; ci.yml reads `bin/secretspec`.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);

  # A github-type node (owner/repo/rev/narHash), here NixOS/nixpkgs. The
  # `nixpkgs` node is cachix/devenv-nixpkgs, whose rev carries 0.14.0 (no `age`
  # provider), which is why this second input exists.
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
