# tools/toolchain/versions/node.nix — the node toolchain pin.
rec {
  version = "24.22.1";
  srcs = {
    "x86_64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-x64.tar.xz";
      hash = "sha256-bBcqmrZxj3uAH7WnvUhq8u5eMnWAhqBLhPzOKqdwleY=";
    };
    "aarch64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-arm64.tar.xz";
      hash = "sha256-26v3ocv5R+YFn/rSwnuABB25feoeVTtir97n8T/KM24=";
    };
    "aarch64-darwin" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-darwin-arm64.tar.gz";
      hash = "sha256-icaCWT12zUQjP6U7FWMmTn3py7GJytFpE1xbT637wKc=";
    };
  };
}
