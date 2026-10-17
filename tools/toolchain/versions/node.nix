# tools/toolchain/versions/node.nix — the node toolchain pin.
rec {
  version = "24.22.0";
  srcs = {
    "x86_64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-x64.tar.xz";
      hash = "sha256-zC9gHO6C2aWiMzAv4werxgBycDX9fSw6NXF1WYnpUAg=";
    };
    "aarch64-linux" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-linux-arm64.tar.xz";
      hash = "sha256-/vALtNNuHFAQSpd1oQNZvu6Y8WK0EE3RJHLHUGXGoUM=";
    };
    "aarch64-darwin" = {
      url = "https://nodejs.org/dist/v${version}/node-v${version}-darwin-arm64.tar.gz";
      hash = "sha256-apck2bUKXyK0gWze1EG1qNndrlRhkaXlt++lpeLuJtU=";
    };
  };
}
