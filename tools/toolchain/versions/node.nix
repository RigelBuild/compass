# tools/toolchain/versions/node.nix — the node toolchain pin.
rec {
  version = "24.20.0";
  srcs = {
    "x86_64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-x64.tar.xz";
      hash = "sha256-LywNoWIxjw3kdmVBDHyMLtPTbI8xBd5LvGEXbHCny/I=";
    };
    "aarch64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-arm64.tar.xz";
      hash = "sha256-X03athDBqyAWs8Inzr2/bZSVFhSH5HOce5AJBZX0Zfc=";
    };
    "aarch64-darwin" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-darwin-arm64.tar.gz";
      hash = "sha256-QOVgfl7LPbkZJyN3baLXXZZiYPx0p6nnMcG9Z92pa8g=";
    };
  };
}
