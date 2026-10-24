# tools/toolchain/versions/moon.nix — the moon toolchain pin.
#
# moon is pinned HERE rather than taken from nixpkgs (which lags on the
# 1.x line) so it tracks the 2.x series.
rec {
  version = "2.6.4";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-aQpWEYDeg0vvx9zYVeZ1jbBKoUi46KODj3Dn7pVf3tY=";
    };
    "aarch64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-unknown-linux-musl.tar.xz";
      hash = "sha256-gLTLcT1IyBouH4bA4wia2nGHdSOhkJu8GzKhUFAE2OA=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-apple-darwin.tar.xz";
      hash = "sha256-PyKDJHpyPib8hGIyOtSL+sAawxufi0jPIQQYvB2P7HM=";
    };
  };
}
