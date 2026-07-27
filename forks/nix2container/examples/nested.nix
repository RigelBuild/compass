{ pkgs, nix2container }:

nix2container.buildImage {
  name = "nested";
  config = {
    entrypoint = ["/bin/bash" "-c" "${pkgs.hello}/bin/hello"];
  };
  layers = [(nix2container.buildLayer {
    deps = [pkgs.bashInteractive];
    copyToRoot = pkgs.buildEnv {
      name = "root";
      paths = [pkgs.bashInteractive];
      pathsToLink = ["/bin"];
    };
    layers = [
      (nix2container.buildLayer {
        deps = [pkgs.readline];
      })
      (nix2container.buildLayer {
        deps = [pkgs.readline];
      })
    ];
  })];
}
