# Provide the Go server's secretspec runtime CLI and its relocatable release
# binary. The CLI is a dotted locked input, not a dev-shell tool, and CI needs it
# on PATH. Pin the dedicated secretspec-nixpkgs input: root nixpkgs lacks the
# required age provider at its locked version.
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
