# The out-of-band secretspec CLI + FFI cdylib for the Go secrets paths. The
# resolver spawns the CLI BY NAME for `set`, so the write path is unreachable
# unless it is on PATH; TestSecretSpecCLIVersionFloor fails closed when
# exec.LookPath finds nothing. This file is how CI gets both artifacts.
#
# Realized here rather than through gate-tools.nix (mirroring
# skopeo-nix2container-env): secretspec is a dotted input reference, not a bare
# nixpkgs attr, so it cannot live in devenv.nix's parsed `packages` literal, and
# it is a runtime dependency of one package's test/write path, not a dev-shell
# CLI whose PATH drift the parity gate catches.
#
# Pins from the SAME devenv.lock `secretspec-nixpkgs` rev the dev shell resolves.
# That input is a SECOND nixpkgs, deliberately not `follows: nixpkgs`: the shell's
# own rev still carries secretspec 0.14.0, which has no `age` provider (the
# encrypted-at-rest default). The pin (0.20.0) is what hostcheck.SecretSpecFloor
# and the `secretspec-go` SDK pin in go/go.mod both track, so read and write
# halves cannot drift.
#
# The READ path differs in kind: the resolver calls the `secretspec-go` SDK, a
# purego FFI binding that dlopens a `libsecretspec` cdylib via SECRETSPEC_FFI_LIB.
# nixpkgs packages ONLY the `secretspec` crate (bin output, no shared library),
# so the cdylib — a separate workspace member living only in the upstream repo —
# is built here from the upstream repo at tag v${version}, with `version` read
# from the SAME pinned nixpkgs `secretspec` package. One source of truth for the
# number, so CLI, SDK go.mod pin, and cdylib cannot drift.
#
# Two outputs, each realized with `nix build` (never `nix eval`):
#   secretspec     the CLI derivation (write path); ci.yml reads `bin/secretspec`.
#   libsecretspec  the FFI cdylib (read path); SECRETSPEC_FFI_LIB points at
#                  `lib/libsecretspec.so`.
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

  # The single source of truth for the version: the pinned nixpkgs `secretspec`
  # package the CLI output below is. The cdylib is fetched at the matching
  # release, so read and write halves share one version.
  version = pkgs.secretspec.version;

  # The libsecretspec cdylib the read-path SDK dlopens, taken from the upstream
  # RELEASE not built from source (no binary cache, ~13 min of Rust per CI job).
  # Pinned by the published sha256. Linux x86_64/aarch64 + Darwin arm64 are the
  # hosts that run this; any other throws rather than silently yielding nothing.
  ffiAsset =
    {
      x86_64-linux = {
        name = "libsecretspec-x86_64-unknown-linux-gnu.so";
        hash = "sha256-9YHvFdtHga5b4Z2Nv4U3C9Kwt0jQRs+pcBMoE6ZS3TQ=";
      };
      aarch64-linux = {
        name = "libsecretspec-aarch64-unknown-linux-gnu.so";
        hash = "sha256-UumvFC0pKEgiEHtCyKHXbODpa4+9weFHoSOAUbkTHYE=";
      };
      aarch64-darwin = {
        name = "libsecretspec-aarch64-apple-darwin.dylib";
        hash = "sha256-WoSv2V+/lAr1kVF+L1naNAmyoCIgqh0S4XYeuzhfjOY=";
      };
    }
    .${pkgs.stdenv.hostPlatform.system}
      or (throw "libsecretspec: no published asset for ${pkgs.stdenv.hostPlatform.system}");

  # The SDK's findLibrary looks for `libsecretspec.<ext>`, so the asset is renamed
  # from its triple-qualified release name to that flat one.
  libsecretspec = pkgs.stdenvNoCC.mkDerivation {
    pname = "libsecretspec";
    inherit version;

    src = pkgs.fetchurl {
      url = "https://github.com/cachix/secretspec/releases/download/v${version}/${ffiAsset.name}";
      hash = ffiAsset.hash;
    };

    dontUnpack = true;

    installPhase = ''
      runHook preInstall
      install -Dm555 "$src" \
        "$out/lib/libsecretspec${pkgs.stdenv.hostPlatform.extensions.sharedLibrary}"
      runHook postInstall
    '';
  };
in
{
  secretspec = pkgs.secretspec;
  inherit libsecretspec;
}
