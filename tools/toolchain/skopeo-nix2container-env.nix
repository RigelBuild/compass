# The out-of-band skopeo for the agent-image publish lane. The
# RigelBuild/nix2container fork's patched skopeo understands the `nix:` transport
# — it reads a nix2container image spec directly — which stock skopeo does not.
# The publish workflow (login / publish.sh / verify) and the agent-image env-gate
# drive it to inspect and copy the built image.
#
# Realized here rather than through the shared gate-tools.nix / toolchain-parity
# machinery, mirroring chromium-e2e-env.nix, for two reasons:
#   - skopeo-nix2container is a dotted input reference
#     (inputs.nix2container.packages.<system>.skopeo-nix2container), not a bare
#     nixpkgs attr, so it cannot live in devenv.nix's parsed `packages` literal —
#     the toolchain-parity gate resolves every bare attr in that literal and
#     throws on any non-bare token.
#   - it is a publish-lane tool, not a dev-shell CLI whose ambient-vs-pinned PATH
#     drift the parity gate exists to catch, so keeping it out of the parsed
#     attrs is conceptually right — it is a gate/publish input, not a toolchain.
#
# It is deliberately NOT in agent-image/devenv.nix `packages`: that devenv is the
# one the container module bakes into the published image via the entrypoint's
# `source ${shell.envScript}`, so a package there lands skopeo's ~168 MB closure
# in every agent image — a publish-only tool the running agent never invokes. It
# IS in the root devenv.nix `packages` (a plain dev shell, nothing bakes it), so
# a local `direnv`/`devenv shell` puts it on PATH; this file is how CI resolves
# the identical derivation without entering that (banner-emitting) shell.
#
# Pins BOTH the nix2container fork rev AND nixpkgs to the SAME devenv.lock
# revisions the root dev shell resolves (devenv.yaml's `nix2container` input
# follows the shell's nixpkgs), so CI builds byte-for-byte the skopeo a local
# dev box does — the single source of truth for both revs is devenv.lock (OQ2
# Decision 2: no raw nix2container flake-ref literal). Realized with `nix build`
# (never `nix eval`, which strips the store context that would build the
# derivation).
#
# One output the consumers read `bin/skopeo` off:
#
#   skopeo  the fork's patched skopeo derivation.
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
  # directly (skopeo-nix2container at top level), not under packages.<system> —
  # the system is implied by the nixpkgs it is imported with.
  n2c = import n2cSrc { inherit pkgs; };
in
{
  skopeo = n2c.skopeo-nix2container;
}
