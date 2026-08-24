# tools/toolchain/versions/moon.nix — the moon toolchain pin.
#
# moon is pinned HERE rather than taken from nixpkgs (which lags on the
# 1.x line) so it tracks the 2.x series.
rec {
  version = "2.5.3";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-8Ji/w9ww7BcGv8wRircL8vropyTCR8iKtyHZ4Qv7Ra4=";
    };
    "aarch64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-unknown-linux-musl.tar.xz";
      hash = "sha256-04zH9V2Y2WqogwDGAyGIX7oVyzQKQ5dPg0sD0lKPECc=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-apple-darwin.tar.xz";
      hash = "sha256-j9ZjZnv3WmbmmQ2dElHdYo3KP7diGoaWJ3pZP2dwP48=";
    };
  };
}
