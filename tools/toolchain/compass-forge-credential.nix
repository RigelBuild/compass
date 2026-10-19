{ pkgs, src }:
pkgs.buildGoModule {
  pname = "compass-forge-credential";
  version = "0.1.0";
  inherit src;
  subPackages = [ "cmd/compass-forge-credential" ];
  proxyVendor = true;
  vendorHash = "sha256-nBIJqT2rTagffAS7t/SrU1OzmUP6N89lvJU2tyJseOg=";
  env.CGO_ENABLED = 0;
  ldflags = [ "-X main.ghBinary=${pkgs.gh}/bin/gh" ];
  doCheck = false;
}
