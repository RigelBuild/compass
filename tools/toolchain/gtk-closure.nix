# The GTK3/WebKitGTK package set the Compass native app (Wails v3,
# go/cmd/compass-app) links through cgo on Linux — the frozen SEA-1172 closure
# (docs/designs/platform/ci-toolchain-shared-defs.md). ONE definition, imported
# by two consumers so they cannot drift:
#
#   - devenv.nix's `env` block builds PKG_CONFIG_PATH over `lib.closePropagation`
#     of this set for the dev shell (and a local gtk3 build/test);
#   - tools/toolchain/gtk-e2e-env.nix realizes the same closure on a CI runner
#     for the multi-window gtk3 e2e gate (design record compass-multi-window
#     §M4), the ONE CI lane that compiles + runs the native app.
#
# Kept as a bare name list (not the resolved derivations) so a consumer applies
# it against whichever pinned `pkgs` it already resolves — the dev shell's, or
# the e2e helper's devenv.lock-pinned nixpkgs — without this module taking a
# nixpkgs of its own.
pkgs:
with pkgs;
[
  dbus
  openssl
  glib
  gtk3
  webkitgtk_4_1
  libsoup_3
  cairo
  pango
  gdk-pixbuf
  atk
  harfbuzz
  librsvg
  gobject-introspection
]
