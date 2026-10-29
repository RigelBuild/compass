# tools/toolchain/versions/node.nix — the node toolchain pin.
rec {
  version = "24.22.2";
  srcs = {
    "x86_64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-x64.tar.xz";
      hash = "sha256-SMwOIeBIeQ5UwM9XScJyUAUxPWUmUcQLeiH+uWl6r3U=";
    };
    "aarch64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-arm64.tar.xz";
      hash = "sha256-6xh4N6IjDUBHFhrAvgCFhOFrRvcm6clJDuNdxrF9m6g=";
    };
    "aarch64-darwin" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-darwin-arm64.tar.gz";
      hash = "sha256-qpVJNJ5H3/qkLc+V1uBq3oxL2vyYCXmIO4h2dnDfOFo=";
    };
  };
}
