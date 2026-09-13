# The out-of-band microVM VMM stack for the CI KVM leg (the `gates` job's
# "microVM suites" step). The tagged suites drive cloud-hypervisor, pair it with
# virtiofsd, and give the guest userspace networking through passt;
# microvmtest.Require resolves all three from PATH.
#
# Realized here rather than through gate-tools.nix (mirroring chromium-e2e-env):
# cloud-hypervisor / virtiofsd / passt are Linux-only in nixpkgs, so they cannot
# live in devenv.nix's parsed `packages` literal (the parity gate resolves every
# bare attr on macOS too). They are Linux-guarded in the dev shell and
# provisioned onto the CI runner from here — a gate input, not a toolchain.
#
# Pins nixpkgs to the SAME devenv.lock rev the dev shell resolves, so CI drives
# the byte-for-byte VMM stack a Linux dev box does.
#
# Three outputs, each realized with `nix build` (never `nix eval`); the step
# appends each out-path's bin/ to $GITHUB_PATH:
#   cloud-hypervisor  the VMM the suites boot the guest with.
#   virtiofsd         the virtio-fs daemon for the session volume.
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
