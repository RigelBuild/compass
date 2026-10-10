# Refresh vehicle for apps/ui/dist.nix's pinned node_modules FOD.
# flake.lock supplies pkgs, matching the compass-ui production derivation.
{ ... }:
let
  lock = builtins.fromJSON (builtins.readFile ../../flake.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
in
{
  compass-ui = import ../../apps/ui/dist.nix {
    inherit (pkgs) lib;
    inherit pkgs;
    version = "fod-refresh";
  };
}
