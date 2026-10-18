 # The FOD fixture uses the image's locked nixpkgs to reproduce its node_modules hash.
 # Keep it outside agent-image/ so edits do not trigger an image rebuild.
let
  # Use the image lock; the root lock may select a different Bun version.
  lock = builtins.fromJSON (builtins.readFile ../../agent-image/devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  lib = pkgs.lib;
in
{
  # Import the image entrypoint with the same pkgs; its FOD is pulled in as a
  # dependency and relative imports still resolve from agent-image/.
  compass-agent = import ../../agent-image/entrypoint.nix { inherit pkgs lib; };
}
