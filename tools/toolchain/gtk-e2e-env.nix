# The out-of-band environment for the multi-window gtk3 e2e gate (design record
# compass-multi-window §M4) — the ONE CI lane that compiles + runs the Compass
# native app (Wails v3, go/cmd/compass-app). The per-PR moon battery is
# deliberately GTK-free (devenv.nix's `env` block owns that rationale), so the
# dedicated ci.yml e2e step realizes this closure itself rather than teaching
# the shared gate-tools.nix / toolchain-parity machinery about the heavy
# WebKitGTK set.
#
# Pins nixpkgs to the SAME devenv.lock revision the dev shell and gate-tools.nix
# resolve, so the e2e runner links byte-for-byte the libraries a dev box does.
# The GTK3/WebKitGTK package set is imported from tools/toolchain/gtk-closure.nix
# — the one definition devenv.nix's PKG_CONFIG_PATH is also built from — so the
# two cannot drift.
#
# Three outputs the ci.yml step consumes, ALL realized with `nix build` (never
# `nix eval`, which strips the store context that would build the closure):
#
#   bin        a buildEnv whose bin/ holds xvfb-run (and its bundled Xvfb) +
#              pkg-config + dbus (for dbus-run-session, which gives the GTK4
#              app the session bus it aborts without); the step prepends it to
#              PATH so `xvfb-run` wraps `go test` and pkg-config is on hand for
#              the cgo link.
#   pkgConfig  a buildEnv over the closure; the step sets PKG_CONFIG_PATH to its
#              lib/pkgconfig + share/pkgconfig subdirs so the cgo link resolves
#              the gtk+-3.0 / webkit2gtk-4.1 `.pc` files (and the libs they
#              reference) — the same two-subdir shape devenv.nix uses.
#   cc         the nixpkgs cc-wrapper; the step sets CC/CXX to its bin/cc,bin/c++
#              so the cgo link uses the glibc WebKitGTK was built against (not
#              the runner's system gcc) and the test binary is self-contained.
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
    # xvfb-run (with its bundled Xvfb) + pkg-config + dbus (dbus-run-session),
    # from the same pinned nixpkgs, so the step needs nothing off the ambient
    # CI PATH to link + run.
    paths = [
      pkgs.xvfb-run
      pkgs.pkg-config
      pkgs.dbus
    ];
  };

  # A REALIZABLE pkg-config tree over the closure — ci.yml `nix build`s this so
  # the WebKitGTK closure is actually built into the runner's store. The dev
  # shell can set a bare PKG_CONFIG_PATH *string* because nix's string-context
  # pulls those derivations into the shell's input closure and realizes them
  # when direnv builds the environment; `nix eval --raw` STRIPS that context, so
  # a plain search-path string would name `.pc` files nix never built and the
  # cgo link fails "No package 'glib-2.0' found". buildEnv makes the closure a
  # first-class build output whose `.pc` files (dev output) and the libraries
  # they reference (out output) are realized as its dependencies. Same pcClosure
  # the dev shell's PKG_CONFIG_PATH is built from, so the two cannot drift; same
  # lib/pkgconfig + share/pkgconfig two-subdir shape (searched by the step).
  pkgConfig = pkgs.buildEnv {
    name = "compass-gtk-e2e-pkgconfig";
    paths = pcClosure;
    extraOutputsToInstall = [ "dev" ];
    # Curated, uniquely-named `.pc` set; keep a stray duplicate from aborting
    # the merge (a search-path string would just pick the first anyway).
    ignoreCollisions = true;
  };

  # The nixpkgs C toolchain (cc-wrapper) the step points CC/CXX at for the cgo
  # link — NOT the runner's /usr/bin/gcc. WebKitGTK from this pinned nixpkgs is
  # built against glibc 2.42; a stock GitHub runner's system gcc links against
  # its older system glibc, so `ld` fails "undefined reference to
  # `__inet_pton_chk@GLIBC_2.42'" against libwebkit2gtk-4.1.so. The cc-wrapper
  # bundles the matching glibc and, via its binutils ld-wrapper, stamps the
  # binary's ELF interpreter to that glibc's ld-linux and rpaths the store lib
  # dirs — so the test binary is self-contained (right libc + every GTK/WebKit
  # .so at runtime) regardless of the runner's system libc. A dev box links this
  # same toolchain implicitly (its `cc` IS the nix wrapper), so CI now matches.
  # Referenced by its own store path, never merged into a buildEnv: a cc-wrapper
  # resolves its siblings through nix-support/ files + relative symlinks a
  # symlink-merge would break.
  cc = pkgs.stdenv.cc;
}
