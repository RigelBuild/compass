# The GTK4/WebKitGTK package set the Compass native app (Wails v3) links through
# cgo on Linux. ONE definition, imported by three consumers so they cannot drift:
# devenv.nix's PKG_CONFIG_PATH, tools/toolchain/gtk-e2e-env.nix (the CI e2e gate),
# and flake.nix's `compass-app` package. Kept as a bare name list so each consumer
# applies it against its own pinned `pkgs`, without this module taking a nixpkgs.
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
  # atk and gdk-pixbuf are intentionally omitted: GTK4 routes accessibility
  # through at-spi2, so atk is dead closure weight in the gtk4/webkitgtk-6.0
  # Requires-walk; gdk-pixbuf is pulled in transitively by gtk4.pc/librsvg, so
  # the explicit entry is redundant.
  harfbuzz
  librsvg
  gobject-introspection
]
