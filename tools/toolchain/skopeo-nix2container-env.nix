# The out-of-band skopeo for the agent-image publish lane. The nix2container
# fork's patched skopeo understands the `nix:` transport (reads a nix2container
# image spec directly), which stock skopeo does not. The publish workflow and
# the agent-image env-gate drive it to inspect and copy the built image.
#
# Realized here rather than through gate-tools.nix (mirroring chromium-e2e-env):
# it is a dotted input reference, not a bare nixpkgs attr, so it cannot live in
# devenv.nix's parsed `packages` literal, and it is a publish-lane tool, not a
# dev-shell CLI whose PATH drift the parity gate catches.
#
# Deliberately NOT in agent-image/devenv.nix `packages`, whose packages get baked
# into every published image — this would add skopeo's ~168 MB closure the agent
# never uses. It IS in the root devenv.nix (nothing bakes it); this file is how
# CI resolves the identical derivation without entering the banner-emitting shell.
#
# Pins BOTH the nix2container fork rev AND nixpkgs to the SAME root devenv.lock
# revs the dev shell resolves, so CI builds byte-for-byte the same skopeo — the
# single source of truth is the root devenv.lock (OQ2 Decision 2). agent-image's
# own lock pins nix2container separately by design (a different consumer), so the
# two are not lockstep. Realized with `nix build`, never `nix eval` (which strips
# the store context).
#
# One output: `skopeo`, the fork's patched skopeo derivation.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);

  nixpkgsNode = lock.nodes.nixpkgs.locked;
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
