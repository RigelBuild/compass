# The GTK4/WebKitGTK package set the Compass native app (Wails v3,
# go/cmd/compass-app) links through cgo on Linux. ONE definition, imported
# by three consumers so they cannot drift:
#
#   - devenv.nix's `env` block builds PKG_CONFIG_PATH over `lib.closePropagation`
#     of this set for the dev shell (and a local gtk4 build/test);
#   - tools/toolchain/gtk-e2e-env.nix realizes the same closure on a CI runner
#     for the multi-window gtk4 e2e gate (design record compass-multi-window
#     §M4), the ONE CI lane that compiles + runs the native app.
#   - flake.nix's `compass-app` package realizes the same closure as cgo
#     buildInputs for the `nix build .#compass-app` / bundle build.
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
  gtk4
  webkitgtk_6_0
  libsoup_3
  cairo
  pango
  # atk (legacy GTK accessibility) and gdk-pixbuf are intentionally omitted:
  # GTK4 routes accessibility through at-spi2 (gtk4.pc pulls atspi-2 via
  # gtk4-atspi.pc), so atk is unreachable in the gtk4/webkitgtk-6.0 .pc
  # Requires-walk and would be dead closure weight; gdk-pixbuf is still needed
  # at link but gtk4.pc (and librsvg) already `Requires: gdk-pixbuf-2.0`, so
  # closePropagation pulls it in transitively — the explicit entry is redundant.
  harfbuzz
  librsvg
  gobject-introspection
]
