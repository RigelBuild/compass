# A VERIFICATION VEHICLE for the agent image's view of one shared FOD hash.
# It is NOT how the agent image is built, and nothing in the image, the publish
# lane, or the moon gates evaluates it. Its only consumer is
# tools/renovate/refresh-fod-hashes.ts, whose FOD_ENTRIES table names this file
# and the `compass-agent` attribute below as one entry's build vehicle.
#
# ── WHY IT EXISTS ──
# `agent-image/entrypoint.nix` carries a SINGLE `outputHash` literal over the
# installed `node_modules` tree, and it is imported by TWO consumers resolving
# TWO different nixpkgs revisions:
#
#   guest-image/default.nix:66   with ROOT's `pkgs`        (../devenv.lock)
#   agent-image/devenv.nix:34    with AGENT-IMAGE's `pkgs` (../agent-image/devenv.lock)
#
# That FOD's builder takes `nativeBuildInputs = [ pkgs.bun ]`, so each consumer
# realises it with its own bun derivation, and the one committed hash is correct
# for both only while those two buns produce a byte-identical install tree.
#
# The refresher realises a vehicle to recompute the hash, and the only vehicle in
# the repo was `guest-image/default.nix` — which takes ROOT's pkgs. Nothing built
# `agent-image/devenv.nix` (it is a devenv container definition, realised by
# `devenv container build`, not by `nix build`). So the agent-image consumer's
# builder was never exercised by the refresh: if the two channel revs drifted
# onto bun versions with different install trees, the pin stayed right for
# guest-image and was silently wrong for the agent image, surfacing only when the
# heavy OCI build ran. This file closes that gap by giving that consumer a plain
# `nix build`-able expression with the SAME pkgs the image uses.
#
# ── WHY IT IS A SEPARATE FILE, NOT A CHANGE TO agent-image/ ──
# It lives under tools/renovate/ beside its sole consumer, deliberately:
#
#   * agent-image/moon.yml's build task declares `inputs: ['**/*']`, so a file
#     added under agent-image/ would reschedule the image build (the dominant CI
#     cost, the reason ci.yml's timeout is 90m) on every edit to a file that
#     cannot affect the image.
#   * a nix file inside agent-image/ reads as part of the image definition. This
#     one is test scaffolding for a Renovate task; putting it beside
#     refresh-fod-hashes.ts is what tells the next reader that.
#
# ── HOW IT IS USED ──
#   nix build -f tools/renovate/agent-image-fod-vehicle.nix compass-agent
#
# The refresher first fakes the `outputHash` in entrypoint.nix, so the build
# fails AT the fixed-output derivation with the `got: <real SRI>` line it parses,
# and never proceeds to the `bun build --compile` bundle. That fail-fast is the
# same property `guest-image/default.nix` gives the authoritative entry.
let
  # The AGENT-IMAGE devenv lock's nixpkgs, resolved exactly as the repo's other
  # plain-nix vehicles resolve theirs (guest-image/default.nix:51-57,
  # tools/toolchain/gate-tools.nix:36-42) — read the lock, fetch that rev, import
  # it. The lock path is the whole point of this file: `../../devenv.lock` (the
  # root lock) is what the authoritative vehicle already uses, and reading it
  # here would make this vehicle re-derive the identical value and verify
  # nothing.
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
  # entrypoint.nix is `{ pkgs, lib }:` and returns the wrapper derivation, not
  # the FOD — the FOD is an internal `let` binding. Realising the wrapper pulls
  # the FOD in as a dependency, which is all the refresh needs: nix builds
  # dependencies first, so a faked `outputHash` errors there before any of the
  # bundle work starts. entrypoint.nix's own relative imports
  # (../packages/compass-agent, ../package.json, …) resolve against agent-image/,
  # not against this file, so importing it from here is transparent.
  compass-agent = import ../../agent-image/entrypoint.nix { inherit pkgs lib; };
}
