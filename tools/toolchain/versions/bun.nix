# tools/toolchain/versions/bun.nix — the bun toolchain pin.
rec {
  version = "1.4.3";
  srcs = {
    "x86_64-linux" = {
      url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-linux-x64-baseline.zip";
      hash = "sha256-H8LtrIQxApCeOhvh2NmALMYHHPB05n6IIx9P/g+LOXs=";
    };
    "aarch64-linux" = {
      url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-linux-aarch64.zip";
      hash = "sha256-76mBPaXtckI7+Ef5FujSxHwNd2rdlyNUAmp14Q2pqiE=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/bun-darwin-aarch64.zip";
      hash = "sha256-gK/UwGm0am+o8+xSC+vHUKeEPkqNW5WD0prUNsAKNAM=";
    };
  };
}
