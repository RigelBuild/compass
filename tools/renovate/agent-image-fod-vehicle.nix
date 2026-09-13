# A VERIFICATION VEHICLE for the agent image's view of one shared FOD hash. It
# is NOT how the agent image is built; its only consumer is
# tools/renovate/refresh-fod-hashes.ts, whose FOD_ENTRIES table names this file
# and the `compass-agent` attribute below as one entry's build vehicle.
#
# Why it exists: `agent-image/entrypoint.nix` carries a SINGLE `outputHash` over
# the installed `node_modules`, imported by two consumers resolving two nixpkgs
# revs (guest-image with ROOT's pkgs, agent-image/devenv.nix with AGENT-IMAGE's).
# The one hash is correct for both only while their two buns produce a
# byte-identical install tree. The refresher's only vehicle was
# guest-image/default.nix (ROOT's pkgs); the agent-image consumer's builder was
# never exercised, so a bun drift stayed silently wrong until the heavy OCI build.
# This file closes that gap: a plain `nix build`-able expression with the SAME
# pkgs the image uses.
#
# It lives under tools/renovate/ beside its consumer deliberately: a file under
# agent-image/ would reschedule the image build (moon `inputs: ['**/*']`, the
# dominant CI cost) and read as part of the image definition. This one is test
# scaffolding for a Renovate task.
#
#   nix build -f tools/renovate/agent-image-fod-vehicle.nix compass-agent
#
# The refresher fakes the `outputHash` first, so the build fails AT the FOD with
# the `got: <real SRI>` line it parses, never reaching the bundle.
let
  # The AGENT-IMAGE devenv lock's nixpkgs, resolved as the repo's other plain-nix
  # vehicles resolve theirs (read the lock, fetch that rev, import it). Using the
  # ROOT lock instead would re-derive the value the authoritative vehicle already
  # produces and verify nothing.
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
  # The same import agent-image/devenv.nix performs, with the same `pkgs`.
  # entrypoint.nix returns the wrapper derivation; the FOD is an internal `let`
  # binding, pulled in as a dependency — so a faked `outputHash` errors there
  # before any bundle work. Its relative imports resolve against agent-image/,
  # so importing it from here is transparent.
  compass-agent = import ../../agent-image/entrypoint.nix { inherit pkgs lib; };
}
