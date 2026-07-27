{ pkgs, nix2container }:

nix2container.buildImage {
  name = "duplicated";
  config = {
    entrypoint = ["${pkgs.hello}/bin/hello"];
  };
  layers = [(nix2container.buildLayer {
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
