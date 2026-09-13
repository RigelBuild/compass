# The out-of-band environment for the multi-window gtk4 e2e gate — the ONE CI
# lane that compiles + runs the Compass native app (Wails v3). The per-PR moon
# battery is deliberately GTK-free, so this dedicated ci.yml step realizes the
# closure itself rather than teaching gate-tools.nix about the heavy WebKitGTK
# set. Pins nixpkgs to the SAME devenv.lock rev the dev shell resolves; the
# GTK4/WebKitGTK set comes from gtk-closure.nix (the one definition devenv.nix's
# PKG_CONFIG_PATH is also built from) so the two cannot drift.
#
# Three outputs, ALL realized with `nix build` (never `nix eval`):
#   bin        a buildEnv with xvfb-run (+ bundled Xvfb) + pkg-config + dbus (for
#              dbus-run-session, the session bus the GTK4 app aborts without),
#              prepended to PATH.
#   pkgConfig  a buildEnv over the closure; the step sets PKG_CONFIG_PATH to its
#              lib/pkgconfig + share/pkgconfig subdirs so the cgo link resolves
#              the gtk4 / webkitgtk-6.0 `.pc` files.
#   cc         the nixpkgs cc-wrapper; the step sets CC/CXX to it so the cgo link
#              uses the glibc WebKitGTK was built against, not the runner's gcc.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
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

  # A REALIZABLE pkg-config tree over the closure — ci.yml `nix build`s this so
  # the WebKitGTK closure is actually built into the runner's store. A bare
  # PKG_CONFIG_PATH string works in the dev shell (string-context realizes it)
  # but `nix eval --raw` STRIPS that context, so the link would name `.pc` files
  # nix never built. buildEnv makes the closure a first-class output. Same
  # pcClosure the dev shell uses, same two-subdir shape.
  pkgConfig = pkgs.buildEnv {
    name = "compass-gtk-e2e-pkgconfig";
    paths = pcClosure;
    extraOutputsToInstall = [ "dev" ];
    # Curated, uniquely-named `.pc` set; keep a stray duplicate from aborting the
    # merge (a search-path string would just pick the first anyway).
    ignoreCollisions = true;
  };

  # The nixpkgs C toolchain the step points CC/CXX at — NOT the runner's
  # /usr/bin/gcc. WebKitGTK here is built against glibc 2.42; a stock runner's
  # gcc links against its older glibc and `ld` fails on a GLIBC_2.42 symbol. The
  # cc-wrapper bundles the matching glibc and rpaths the store lib dirs, so the
  # test binary is self-contained regardless of the runner's libc. Referenced by
  # its own store path, never merged into a buildEnv (a cc-wrapper resolves its
  # siblings through nix-support/ files a symlink-merge would break).
  cc = pkgs.stdenv.cc;
}
