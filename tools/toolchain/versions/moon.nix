# tools/toolchain/versions/moon.nix — the moon toolchain pin.
#
# moon is pinned HERE rather than taken from nixpkgs (which lags on the
# 1.x line) so it tracks the 2.x series.
rec {
  version = "2.4.2";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-aYkMHUXhv7SDKC95zUXcKMPoeKpM7CwGz0X9y2W/Yi8=";
    };
    "aarch64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-unknown-linux-musl.tar.xz";
      hash = "sha256-jsw2H3k20/D5DE0uRo7eShNsjAyz0WpGn9rr5mMr08g=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-apple-darwin.tar.xz";
      hash = "sha256-3O/lMlbKZckSWbbmvFcOGCMQIGU9QhzHBoD3hoJQwb8=";
    };
  };
}
