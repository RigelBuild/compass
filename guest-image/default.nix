# The Compass microVM guest image: the two nix attrs V2a's cloud-hypervisor
# runtime consumes to boot a session guest — a direct-boot kernel and a root
# filesystem CONTENTS closure. It is a sibling of agent-image/ and reuses that
# image's toolchain closure, so the guest ships the same agent runtime the
# rootless-container path does — one closure, two artifact shapes (OCI layers
# there, a block/filesystem image here).
#
# WHAT THIS FILE PRODUCES, AND WHAT IT DELIBERATELY DOES NOT:
#
#   * `compass-guest-kernel` — the root devenv.lock-pinned nixpkgs kernel
#     (`pkgs.linuxPackages.kernel`). cloud-hypervisor boots an uncompressed /
#     bzImage kernel DIRECTLY (no bootloader), so the plain nixpkgs kernel
#     derivation is the artifact: its bzImage is `${out}/bzImage`. Because it is
#     an unmodified nixpkgs kernel at the pinned rev, it is SUBSTITUTED free from
#     cache.nixos.org and never built in CI. A size / boot-time-optimized custom
#     kernel config is a deliberate deferral (record OQ5) — the generic pinned
#     kernel is correct for this slice.
#
#   * `compass-guest-rootfs` — the guest root filesystem CONTENTS CLOSURE: the
#     agent-image toolchain closure, plus a stub `compass-guestd` init, the
#     egress prerequisites (nft / getent / awk), a writable `/etc/resolv.conf`,
#     and a virtio-fs mount unit for the stable session path. It builds to an
#     assembled store-path tree — a format-AGNOSTIC contents root. The exact
#     image FORMAT (erofs vs ext4) and the writable-`/nix` mechanism the
#     toolchain's in-guest `nix` needs are V2a's to decide, NOT this record's
#     (§ E-D4). So there is no image-packing step here: V2a packs this tree into
#     its chosen format.
#
# THE PIN DIVERGENCE (a parity note V2a must honor). `agent-image/toolchain.nix`
# is a `{ pkgs, compassAgent }:` function. This file calls it with ROOT's `pkgs`
# — the nixpkgs the root `devenv.lock` pins below — NOT agent-image's own
# devenv.lock pin. So the whole rootfs closure (and the kernel) resolve from the
# ROOT pin. This is deliberate: the guest-image moon gate's `inputs` track the
# root `devenv.lock`, so the gate reschedules on a root-pin move. agent-image's
# OCI build keeps its own pin; the two closures are the same shape, resolved
# through two nixpkgs revisions.
let
  # The root devenv.lock-pinned nixpkgs, resolved exactly as the other plain nix
  # gates in this repo do (tools/toolchain/gate-tools.nix:37-42,
  # app-bundle/bundle-env.nix:21-27) — read the lock, fetch that rev, import it.
  # This is the "root's pkgs" the record's pin divergence turns on.
  lock = builtins.fromJSON (builtins.readFile ../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  lib = pkgs.lib;

  # The SAME bundled agent entrypoint and toolchain closure the agent image
  # ships (agent-image/devenv.nix:34-36), imported unchanged and fed root's
  # `pkgs`. entrypoint.nix is `{ pkgs, lib }:`; toolchain.nix is
  # `{ pkgs, compassAgent }:`. Their own relative imports
  # (../tools/toolchain/toolchain-tools.nix, ../package.json, …) resolve against
  # agent-image/, not this file, so importing them here is transparent.
  compassAgent = import ../agent-image/entrypoint.nix { inherit pkgs lib; };
  toolchain = import ../agent-image/toolchain.nix { inherit pkgs compassAgent; };

  # The stub init. V2a grows the real `compass-guestd` (the guest-side supervisor
  # that mounts the session volume, arms egress, and runs the agent); this slice
  # ships a minimal placeholder so the rootfs has a named init to boot to and the
  # derivation shape is settled. A real compiled binary (not a shell script) so
  # it is a genuine `/sbin/init` candidate; it does nothing but announce itself
  # and exit — this image never actually boots in E3 (boot verification is
  # V2a's job).
  guestd = pkgs.writeCBin "compass-guestd" ''
    #include <stdio.h>
    int main(void) {
      fputs("compass-guestd: E3 stub init — V2a grows the real guest supervisor\n", stderr);
      return 0;
    }
  '';

  # A writable /etc/resolv.conf. The guest's net bringup provisions it at boot
  # (compass-guestd + the D6 userspace net backend give the guest its IP and
  # resolv.conf, microvm-runner.md:451); the egress arm then READS it to build
  # the DNS allowlist (go/internal/runtime/egress.go:112,122). Either way it is a
  # hard requirement that resolv.conf be a writable REGULAR FILE the guest can
  # rewrite (microvm-runner.md:446-449), never a symlink into the immutable store
  # — V2a's image packing then yields a file the guest can write. Staged as a
  # real file in the installPhase below (a store `writeText` would land as a
  # read-only symlink). The placeholder nameserver is inert: the net bringup
  # overwrites the whole file at boot.
  resolvConf = pkgs.writeText "compass-guest-resolv.conf" ''
    # Provisioned by the guest net bringup at boot; read by the egress arm.
    nameserver 127.0.0.1
  '';

  # The virtio-fs mount unit for the stable session path. cloud-hypervisor
  # exposes the session volume as a virtio-fs device with a tag; the guest mounts
  # that tag at the stable checkout path (/workspace — the Runner's default
  # -checkout-dir, cmd/compass-runner/main.go:46), preserving the no-copy
  # invariant (microvm-runner.md:179-182). A systemd `.mount` unit named for its
  # mount point (`workspace.mount` ⇒ /workspace, systemd's naming rule). The stub
  # init does not run it in E3; it is the declarative artifact V2a's real init
  # wires up. The `workspace` fs tag is the V2a/host contract for the device.
  mountUnit = pkgs.writeText "workspace.mount" ''
    [Unit]
    Description=Compass session volume (virtio-fs) at the stable workspace path
    DefaultDependencies=no
    After=local-fs-pre.target
    Before=local-fs.target

    [Mount]
    What=workspace
    Where=/workspace
    Type=virtiofs
    Options=rw,relatime

    [Install]
    WantedBy=local-fs.target
  '';
in
{
  # Direct-boot kernel, substituted free from cache.nixos.org (never built): its
  # bzImage is ${compass-guest-kernel}/bzImage.
  compass-guest-kernel = pkgs.linuxPackages.kernel;

  # The rootfs contents closure: a staged directory tree V2a packs into its
  # chosen image format. Assembled by hand (rather than a bare `buildEnv`) so the
  # writable resolv.conf lands as a real file and the closure references stay
  # explicit and reviewable.
  compass-guest-rootfs = pkgs.runCommand "compass-guest-rootfs" { } ''
    mkdir -p $out/bin $out/sbin $out/etc/systemd/system

    # The agent-image toolchain closure: its /bin and /etc, symlinked in. These
    # point into the store closure the packed image ships — the same
    # relocated-/etc + store-closure shape nix2container gives the OCI artifact.
    for f in ${toolchain}/bin/*; do
      ln -s "$f" "$out/bin/$(basename "$f")"
    done
    if [ -d ${toolchain}/etc ]; then
      cp -a ${toolchain}/etc/. $out/etc/
      # cp -a preserves the store's read-only dir/file modes; make the staged
      # /etc writable so the resolv.conf install below (and V2a's packing) can
      # add to it.
      chmod -R u+w $out/etc
    fi

    # The egress prerequisites (microvm-runner.md:446-449). Already present via
    # the toolchain closure above (agent-image/toolchain.nix:144-147), linked
    # again here explicitly so the guest's contract does not depend on the
    # toolchain's internal package list. `ln -sf` because the toolchain loop may
    # already have created these names.
    ln -sf ${pkgs.nftables}/bin/nft $out/bin/nft
    ln -sf ${pkgs.getent}/bin/getent $out/bin/getent
    ln -sf ${pkgs.gawk}/bin/awk $out/bin/awk

    # The stub init, reachable both as /sbin/init and by name on PATH.
    ln -s ${guestd}/bin/compass-guestd $out/sbin/init
    ln -s ${guestd}/bin/compass-guestd $out/bin/compass-guestd

    # A writable /etc/resolv.conf — a real regular file, not a store symlink, so
    # V2a's packing yields a file the guest's net bringup can rewrite at boot.
    install -Dm644 ${resolvConf} $out/etc/resolv.conf

    # The virtio-fs mount unit for the stable session path.
    install -Dm644 ${mountUnit} $out/etc/systemd/system/workspace.mount
  '';
}
