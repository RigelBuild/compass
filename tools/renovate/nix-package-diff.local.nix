{ lib, pkgs, config, ... }:
let
  containers = builtins.attrValues config.containers;
  containerRoots = lib.concatMap
    (container: lib.concatMap
      (layer: (pkgs.buildEnv { paths = layer.copyToRoot or [ ]; }).paths)
      (container.layers or [ ]))
    containers;
in
{
  options.packageDiff = {
    direct = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      readOnly = true;
    };
    closure = lib.mkOption {
      type = lib.types.package;
      readOnly = true;
    };
  };

  config.packageDiff = {
    direct = map (package: package.name) config.packages
      ++ map (package: package.name) containerRoots;
    closure = pkgs.writeText "nix-package-diff.json" (builtins.toJSON {
      profile = config.devenv.profile;
      env = config.env;
      processes = lib.mapAttrs (_: process: process.exec) config.processes;
    });
  };
}
