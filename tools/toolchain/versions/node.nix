# tools/toolchain/versions/node.nix — the node toolchain pin.
rec {
  version = "24.18.0";
  srcs = {
    "x86_64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-x64.tar.xz";
      hash = "sha256-VapxU/nYjyjXZfza1a5pRbXA+Yo2iBcDgX5MRQ+nZ0I=";
    };
    "aarch64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-arm64.tar.xz";
      hash = "sha256-WMlSBQH2ritS1bIQRE4kudDAKaWMUBG3l7wf5xBYhvY=";
    };
    "aarch64-darwin" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-darwin-arm64.tar.gz";
      hash = "sha256-4al+FMmcgD6WxzOUAyguoFpJnDL42D3v6e9exm+XntE=";
    };
  };
}
