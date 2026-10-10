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
# Outputs, realized with `nix build` (never `nix eval`):
#   secretspec         the CLI derivation; ci.yml reads `bin/secretspec`, and the
#                      flake wraps compass-server with it.
#   secretspecRelease  the upstream release binary at the same version, staged
#                      into the app bundles: a store-linked build is not
#                      relocatable onto a user's machine.
{
  system ? builtins.currentSystem,
}:
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);

  # A github-type node (owner/repo/rev/narHash), here NixOS/nixpkgs. Root's
  # nixpkgs input is cachix/devenv-nixpkgs, whose rev carries 0.14.0 (no `age`
  # provider), which is why this second input exists.
  node = lock.nodes."secretspec-nixpkgs".locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { inherit system; };
  inherit (pkgs.secretspec) version;

  # Linux takes the static musl build so the bundle needs no host libc match.
  asset =
    {
      x86_64-linux = {
        triple = "x86_64-unknown-linux-musl";
        hash = "sha256-v2y1Wvw2tD4z0ewagYxY/j2f3FnDOtRQLzyrwriITvI=";
      };
      aarch64-darwin = {
        triple = "aarch64-apple-darwin";
        hash = "sha256-wX+kl4JaOnI3V0z+p6VGADd7NOqnliUYu6PRx+1ZSow=";
      };
    }
    .${system} or (throw "secretspecRelease: no published asset for ${system}");
in
{
  secretspec = pkgs.secretspec;

  secretspecRelease = pkgs.stdenvNoCC.mkDerivation {
    pname = "secretspec-release";
    inherit version;
    src = pkgs.fetchurl {
      url = "https://github.com/cachix/secretspec/releases/download/v${version}/secretspec-${asset.triple}.tar.xz";
      inherit (asset) hash;
    };
    # Upstream's own build: patching its interpreter or rpath would break it.
    dontFixup = true;
    installPhase = ''
      runHook preInstall
      install -Dm555 secretspec "$out/bin/secretspec"
      runHook postInstall
    '';
  };
}
