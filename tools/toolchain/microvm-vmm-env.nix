# The out-of-band microVM VMM stack for the CI KVM leg (the `gates` job's
# "microVM suites" step, design record microvm-ci-dev-enablement §E5). The tagged
# microVM suites drive cloud-hypervisor, pair it with virtiofsd, and give the
# guest userspace networking through passt; microvmtest.Require resolves all three
# from PATH.
#
# Realized here rather than through the shared gate-tools.nix / toolchain-parity
# machinery, mirroring chromium-e2e-env.nix EXACTLY:
#   - cloud-hypervisor / virtiofsd / passt are Linux-only in nixpkgs, so they
#     cannot live in devenv.nix's parsed `packages` literal — the parity gate
#     resolves every bare attr in that literal on macOS too, where they do not
#     exist, and an unverifiable attr fails the gate. They are guarded Linux-only
#     in the dev shell (devenv.nix, the E1 slice) and provisioned onto the CI
#     runner from here.
#   - they are a runtime VMM stack the microVM suites spawn, not dev-shell CLIs
#     whose ambient-vs-pinned PATH drift the parity gate exists to catch, so
#     keeping them out of the parsed attrs is conceptually right — they are a gate
#     input, not a toolchain.
#
# Pins nixpkgs to the SAME devenv.lock revision the dev shell and gate-tools.nix
# resolve, so CI drives the byte-for-byte VMM stack a Linux dev box does — the
# record's one-nix-pin constraint (§ Global Constraints).
#
# Three outputs the ci.yml step consumes, each realized with `nix build` (never
# `nix eval`, which strips the store context that would build the derivation); the
# step appends each out-path's bin/ to $GITHUB_PATH:
#
#   cloud-hypervisor  the VMM the microVM suites boot the guest with.
#   virtiofsd         the virtio-fs daemon paired with it for the session volume.
#   passt             the userspace networking backend for guest egress.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
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
