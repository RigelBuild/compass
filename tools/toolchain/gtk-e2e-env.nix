# Build the Linux GTK4/WebKitGTK e2e environment from the locked nixpkgs.
# `gtk-closure.nix` keeps it aligned with devenv.nix; this remains outside the
# PR gate battery because it compiles and runs the native app.
# Outputs provide Xvfb/dbus, a realized pkg-config closure, and matching cc.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  lib = pkgs.lib;

  pcClosure = lib.closePropagation (import ./gtk-closure.nix pkgs);
in
{
  bin = pkgs.buildEnv {
    name = "compass-gtk-e2e-bin";
    # xvfb-run (with bundled Xvfb) + pkg-config + dbus, from the same pinned
    # nixpkgs, so the step needs nothing off the ambient CI PATH.
    paths = [
      pkgs.xvfb-run
      pkgs.pkg-config
      pkgs.dbus
    ];
  };

  # buildEnv realizes the closure; nix eval --raw would strip store context.
  pkgConfig = pkgs.buildEnv {
    name = "compass-gtk-e2e-pkgconfig";
    paths = pcClosure;
    extraOutputsToInstall = [ "dev" ];
    # Curated, uniquely-named `.pc` set; keep a stray duplicate from aborting the
    # merge (a search-path string would just pick the first anyway).
    ignoreCollisions = true;
  };

  # Use the pinned cc-wrapper: WebKitGTK needs its matching glibc and rpaths.
  cc = pkgs.stdenv.cc;
}
