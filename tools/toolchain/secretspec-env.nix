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
# The READ path is different in kind from the write path: the resolver's
# `SpecResolver.Resolve`/`Statuses` do NOT shell the CLI — they call the
# `secretspec-go` SDK, a purego FFI binding that dlopens a `libsecretspec`
# cdylib located via `SECRETSPEC_FFI_LIB` (binding_purego.go). nixpkgs packages
# ONLY the `secretspec` crate (via fetchCrate of the crate tarball), whose
# derivation emits a single `bin/` output and no shared library, so it cannot
# produce the cdylib. The cdylib is a SEPARATE workspace member
# (`libsecretspec`, crate-type = ["cdylib", "staticlib"], lib name "secretspec"
# so the artifact is `libsecretspec.so`) that lives only in the upstream repo,
# not the published crate. So the read half is realized here by building that
# workspace member from the upstream repo at tag v${version}, with `version`
# read from the SAME pinned nixpkgs `secretspec` package the CLI comes from —
# one source of truth for the number, so the CLI, the SDK go.mod pin, and this
# cdylib cannot drift. Only the source content-hash and cargo vendor-hash are
# literals here (they are not versions); the tag is derived from `version`.
#
# Two outputs, each realized with `nix build` (never `nix eval`, which strips
# the store context that would build the derivation):
#
#   secretspec     the CLI derivation (write path); ci.yml reads `bin/secretspec`.
#   libsecretspec  the FFI cdylib (read path); ci.yml/devenv point
#                  SECRETSPEC_FFI_LIB at `lib/libsecretspec.so`.
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

  # The single source of truth for the version number: the pinned nixpkgs
  # `secretspec` package the CLI output below is. The cdylib is fetched at the
  # matching release, so the read half and write half share one version.
  version = pkgs.secretspec.version;

  # The libsecretspec cdylib the read-path SDK dlopens, taken from the upstream
  # RELEASE rather than built from source: the crate has no binary cache, so
  # compiling it cost CI ~13 minutes of Rust per job. The asset is pinned by the
  # sha256 upstream publishes beside it.
  #
  # Linux x86_64 and aarch64 plus Darwin arm64 are the hosts that run this; any
  # other host throws rather than silently yielding no library.
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

  # The SDK's findLibrary looks for `libsecretspec.<ext>`, so the asset is
  # renamed from its triple-qualified release name to that flat one.
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
