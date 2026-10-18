# Provide nix2container's patched skopeo for the image publish lane.
# This dotted input cannot be a parsed nixpkgs attr; keep it out of the agent
# image to avoid adding an unused ~168 MB closure. Pin nixpkgs and nix2container
# from the root devenv.lock and realize with nix build.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);

  nixpkgsNode = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${nixpkgsNode.owner}/${nixpkgsNode.repo}/archive/${nixpkgsNode.rev}.tar.gz";
    sha256 = nixpkgsNode.narHash;
  };
  pkgs = import nixpkgsSrc { };

  n2cNode = lock.nodes.nix2container.locked;
  n2cSrc = builtins.fetchTarball {
    url = "https://github.com/${n2cNode.owner}/${n2cNode.repo}/archive/${n2cNode.rev}.tar.gz";
    sha256 = n2cNode.narHash;
  };
  # nix2container's default.nix takes `{ pkgs }` and returns the package set
  # directly (skopeo-nix2container at top level); the system is implied by the
  # nixpkgs it is imported with.
  n2c = import n2cSrc { inherit pkgs; };
in
{
  skopeo = n2c.skopeo-nix2container;
}
