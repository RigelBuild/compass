# tools/toolchain/versions/moon.nix — the moon toolchain pin.
#
# moon is pinned HERE rather than taken from nixpkgs (which lags on the
# 1.x line) so it tracks the 2.x series.
rec {
  version = "2.6.5";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-8U/sYJbtYspZdiiNCT759VXygryOnxqbv76CricRkjQ=";
    };
    "aarch64-linux" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-unknown-linux-musl.tar.xz";
      hash = "sha256-GHDK2sZ+gRXpduT8d4L7syfUB7Ltm2H8ar9fs5SmhMM=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/moonrepo/moon/releases/download/v${version}/moon_cli-aarch64-apple-darwin.tar.xz";
      hash = "sha256-caLOeW+ESvTNcFrxnjqDbKe5fI449wK3wfCpW6iwgpA=";
    };
  };
}
