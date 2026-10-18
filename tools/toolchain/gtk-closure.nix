# GTK4/WebKitGTK closure shared by devenv.nix, the CI e2e gate, and flake.nix.
# Keep a bare attribute list so each consumer applies its own pinned pkgs.
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
  # Omit atk: GTK4 uses at-spi2, and gdk-pixbuf is already transitively included.
  harfbuzz
  librsvg
  gobject-introspection
]
