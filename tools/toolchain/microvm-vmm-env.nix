# Provide Linux-only cloud-hypervisor, virtiofsd, and passt for the CI microVM
# suites. Pin nixpkgs to devenv.lock so CI matches Linux dev environments.
# Keep these gate inputs outside devenv.nix's cross-platform parsed packages.
# Realize each output with nix build and append its bin/ directory to GITHUB_PATH.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
in
{
  cloud-hypervisor = pkgs.cloud-hypervisor;
  virtiofsd = pkgs.virtiofsd;
  passt = pkgs.passt;
}
