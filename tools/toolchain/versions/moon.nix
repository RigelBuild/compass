# tools/toolchain/versions/moon.nix — the moon toolchain pin.
#
# moon is pinned HERE rather than taken from nixpkgs (which lags on the
# 1.x line) so it tracks the 2.x series.
rec {
  version = "2.6.1";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-IAMUYI7onR3uzHzQu9Uj1N/6ZP+7NlmJ8APzqNkckIA=";
    };
    "aarch64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-unknown-linux-musl.tar.xz";
      hash = "sha256-sftnkzIxycVl6kN1tU8PlkURLXbt52emBAxRQr98AxA=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-apple-darwin.tar.xz";
      hash = "sha256-KSLOcCd8xvN1AQb6nr0eJe2JDsjNukVwFI/4cBpb6lM=";
    };
  };
}
