# The Compass microVM guest image: the three nix attrs V2a's cloud-hypervisor
# runtime consumes to boot a session guest — a direct-boot kernel, a packed
# root filesystem image, and a module initramfs. It is a sibling of agent-image/
# and reuses that image's toolchain closure, so the guest ships the same agent
# runtime the rootless-container path does — one closure, two artifact shapes
# (OCI layers there, a bootable erofs image here).
#
# WHAT THIS FILE PRODUCES:
#
#   * `compass-guest-kernel` — the root devenv.lock-pinned nixpkgs kernel
#     (`pkgs.linuxPackages.kernel`). cloud-hypervisor boots an uncompressed
#     bzImage kernel DIRECTLY (no bootloader), so the plain nixpkgs kernel
#     derivation is the artifact: its bzImage is `${out}/bzImage`. Because it is
#     an unmodified nixpkgs kernel at the pinned rev, it is SUBSTITUTED free from
#     cache.nixos.org and never built in CI. A size / boot-time-optimized custom
#     kernel config is a deliberate deferral (record OQ5) — the generic pinned
#     kernel is correct for this slice. Its loadable modules live in the separate
#     `modules` output, consumed by both the rootfs and the initrd.
#
#   * `compass-guest-rootfs` — the guest root filesystem, packed into a
#     reproducible read-only **erofs image file** (record §(a)): the agent-image
#     toolchain closure, the real `compass-guestd` init (T2), the egress
#     prerequisites (nft / getent / awk), a writable `/etc/resolv.conf`, and the
#     kernel's full `/lib/modules` tree (so post-boot virtio/netfilter modules
#     autoload on demand). cloud-hypervisor attaches it on virtio-blk as the boot
#     disk; the guest's writable view is a whole-root tmpfs overlay assembled by
#     the initrd before switch_root (record §(a)/(b)). The E3 format-agnostic
#     tree is now an internal `rootfsTree` let-binding this attr packs.
#
#   * `compass-guest-initrd` — a zstd-compressed cpio initramfs carrying the
#     boot-critical module set (virtio_pci, virtio_blk, erofs, overlay) from the
#     SAME kernel derivation, plus an init that loads them, mounts the erofs root
#     + tmpfs overlay, and switch_roots to /sbin/init. Required because the
#     pinned generic kernel ships virtio/erofs/overlay as modules, not built-ins
#     (record §(a)); a derivation-time check fails the build if a pin move drops
#     one from the `=m` set the initrd assumes.
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
  # gates in this repo do (tools/toolchain/gate-tools.nix:37-42) — read the lock,
  # fetch that rev, import it. This is the "root's pkgs" the record's pin
  # divergence turns on.
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

  # The real guest init. E3 shipped a `writeCBin` placeholder; V2a (T2,
  # go/cmd/compass-guestd) grows the actual guest-side supervisor — it mounts the
  # API filesystems, brings networking up (in-process DHCP), mounts the
  # virtio-fs workspace, and serves the vsock Health handshake as guest PID 1.
  # T1 packages that binary as the rootfs `/sbin/init` (and on PATH), replacing
  # the stub. A `buildGoModule` of the backend module rooted at `go/`
  # (github.com/RigelBuild/compass/go); static (CGO_ENABLED=0) so it needs no
  # in-guest libc, which a switch_root'd PID 1 cannot assume.
  #
  #   * src is renamed off `go` via `builtins.path { name = …; }`: buildGoModule
  #     unpacks the source into $GOPATH (=/build/go), and a source root literally
  #     named `go` collides with it ("go.mod file not found"). A neutral store
  #     name sidesteps the collision without touching the module.
  #   * proxyVendor is required: the backend module pulls in wails/secretspec,
  #     whose //go:embed patterns reference darwin/windows-only asset files. A
  #     vendor-tree build resolves every package's embeds and fails on those
  #     absent cross-platform files; proxyVendor populates the module cache
  #     instead, so only the packages actually compiled for linux/amd64 are
  #     touched.
  #   * vendorHash pins the fetched module set (no vendor/ dir in-repo). Recompute
  #     with `vendorHash = lib.fakeHash;` on a go.mod/go.sum move.
  guestd = pkgs.buildGoModule {
    pname = "compass-guestd";
    version = "0-v2a";
    src = builtins.path {
      path = ../go;
      name = "compass-go-src";
    };
    subPackages = [ "cmd/compass-guestd" ];
    proxyVendor = true;
    vendorHash = "sha256-LmCUIADU45KTiLTuAvq57ZdFzdUqrcGctopPtT/u/yk=";
    env.CGO_ENABLED = 0;
    ldflags = [
      "-s"
      "-w"
    ];
    # This slice packages the binary; guestd's own logic is unit-tested in its
    # package (go/internal/guestd) under the backend gate, and the real boot is
    # T4's KVM-gated proof — running the suite again here would only re-pay it.
    doCheck = false;
  };

  # A writable /etc/resolv.conf. The guest's net bringup provisions it at boot
  # (compass-guestd + the D6 userspace net backend give the guest its IP and
  # resolv.conf, microvm-runner.md:451); the egress arm then READS it to build
  # the DNS allowlist (go/internal/runtime/egress.go:112,122). Either way it is a
  # hard requirement that resolv.conf be a writable REGULAR FILE the guest can
  # rewrite (microvm-runner.md:446-449), never a symlink into the immutable store.
  # The whole-root tmpfs overlay ((b)) makes every rootfs path rewritable, so
  # this real file in the erofs lower is writable through the upper at boot. The
  # placeholder nameserver is inert: the net bringup overwrites the whole file.
  resolvConf = pkgs.writeText "compass-guest-resolv.conf" ''
    # Provisioned by the guest net bringup at boot; read by the egress arm.
    nameserver 127.0.0.1
  '';

  # The pinned direct-boot kernel. cloud-hypervisor boots its uncompressed
  # bzImage directly (no bootloader); because it is an unmodified nixpkgs kernel
  # at the root-pinned rev it is substituted free from cache.nixos.org and never
  # built in CI. Its loadable modules live in the separate `modules` output
  # (`kernel.modules`, `$out/lib/modules/<version>`), consumed by both the rootfs
  # (full tree, on-demand autoload) and the initrd (boot-critical subset).
  kernel = pkgs.linuxPackages.kernel;

  # The module set the initramfs loads before switch_root. Two groups, one
  # mechanism (kmod modprobe from the shrunk closure), because the guest has no
  # udev/systemd-modules-load to autoload anything post-switch_root — guestd IS
  # init (§(d)). Modules loaded here are kernel state that persists across
  # switch_root, so every driver the guest needs is bound by the time guestd
  # starts:
  #   - boot-critical (mount the root overlay before switch_root): the virtio
  #     transport + block device, erofs (the lower), overlayfs (the writable
  #     view);
  #   - runtime (the devices + socket families guestd binds right after
  #     switch_root): virtio-net (guestd's eth0), virtio-fs (the /workspace
  #     mount), the virtio vsock transport (the host↔guest GuestControl
  #     channel), and af_packet (the AF_PACKET raw socket guestd's in-process
  #     DHCP client opens for its broadcast DORA exchange — without it the
  #     lease step fails with EAFNOSUPPORT). Their AF_VSOCK/fuse dependencies
  #     are pulled into the closure automatically by modprobe.
  # Every one is `=m` in the pinned generic kernel (record §(a)); the
  # derivation-time check below fails the build if a pin move flips one to `=y`
  # or drops it, rather than silently producing a guest that boots but cannot
  # reach the network, its workspace, or the host.
  bootModules = [
    "virtio_pci"
    "virtio_blk"
    "erofs"
    "overlay"
    "virtio_net"
    "virtiofs"
    "vmw_vsock_virtio_transport"
    "af_packet"
  ];

  # The .config symbol each named module is gated on, checked `=m` at build
  # time. The mapping is module→Kconfig (virtio_blk⇒CONFIG_VIRTIO_BLK,
  # erofs⇒CONFIG_EROFS_FS, overlay⇒CONFIG_OVERLAY_FS, virtiofs⇒CONFIG_VIRTIO_FS,
  # vmw_vsock_virtio_transport⇒CONFIG_VIRTIO_VSOCKETS, af_packet⇒CONFIG_PACKET)
  # — not a mechanical upper-casing, so it is spelled out. Only the explicitly-
  # named modules are checked; their transitive deps (vsock, fuse) ride along
  # via the closure.
  bootModuleConfigs = [
    "CONFIG_VIRTIO_PCI"
    "CONFIG_VIRTIO_BLK"
    "CONFIG_EROFS_FS"
    "CONFIG_OVERLAY_FS"
    "CONFIG_VIRTIO_NET"
    "CONFIG_VIRTIO_FS"
    "CONFIG_VIRTIO_VSOCKETS"
    "CONFIG_PACKET"
  ];

  # Derivation-time assertion (record §(a) test cycle): the pinned kernel's
  # .config still carries the exact `=m` set the initramfs assumes. Emitted as a
  # shell snippet reused by the initrd build, so a nixpkgs-pin move that changes
  # any of these fails the moon gate at the initrd derivation instead of shipping
  # a kernel whose virtio/erofs/overlay drivers the initrd cannot modprobe.
  moduleConfigCheck = ''
    echo "guest-image: verifying boot-critical kernel modules are =m in ${kernel.configfile}"
    ${lib.concatMapStringsSep "\n" (sym: ''
      if ! grep -qx '${sym}=m' ${kernel.configfile}; then
        echo "guest-image: BUILD-BREAK — kernel .config lacks '${sym}=m'." >&2
        echo "  The initramfs assumes ${sym} is a loadable module (record §(a))." >&2
        echo "  A kernel-pin move flipped it to =y or dropped it; the initrd would" >&2
        echo "  not boot. Re-audit guest-image/default.nix bootModules against the" >&2
        echo "  new kernel before proceeding." >&2
        exit 1
      fi
    '') bootModuleConfigs}
    echo "guest-image: boot-critical module set OK"
  '';

  # The boot-critical modules + their dependency closure, shrunk from the
  # kernel's `modules` output (which holds $out/lib/modules/<version>) with
  # depmod-generated modules.dep. modprobe in the init resolves deps from this
  # tree via `-d`. `kernel.modules` (not `kernel`) is the arg: the pinned kernel
  # splits its modules into a separate output, so $out/lib/modules only exists on
  # `.modules`.
  bootModulesClosure = pkgs.makeModulesClosure {
    kernel = kernel.modules;
    firmware = kernel.modules;
    rootModules = bootModules;
  };

  # The initramfs /init: a tiny module-load + mount-overlay + switch_root shim.
  # cloud-hypervisor loads this before the kernel; the kernel execs /init as PID
  # 1 in the unpacked cpio. It carries no busybox userland of its own beyond the
  # store paths its absolute references pull in (busybox for the mount/switch_root
  # applets, kmod for modprobe's xz-module handling, the module closure) — those
  # land in the cpio automatically via makeInitrd's closure walk.
  initScript = pkgs.writeScript "compass-guest-initrd-init" ''
    #!${pkgs.busybox}/bin/sh
    # Fail-closed: any unhandled error aborts /init, PID 1 dies, the guest
    # panics, and the host's boot deadline fires — the same posture guestd takes
    # post-switch_root.
    set -e
    export PATH=${pkgs.busybox}/bin

    fail() {
      echo "compass-guest-initrd: $1" >&2
      # Give the console a moment to flush before PID 1 exits and the kernel
      # panics, so the cause is visible in T4's captured serial log.
      exec sh -c 'echo "compass-guest-initrd: boot aborted"; exit 1'
    }

    # The API filesystems the shim itself needs: /dev for the virtio-blk node,
    # /proc + /sys for module loading and device discovery. guestd re-mounts
    # these post-switch_root (EBUSY-tolerant), so a bare handover is fine.
    mount -t devtmpfs devtmpfs /dev || fail "mount /dev failed"
    mount -t proc     proc     /proc || fail "mount /proc failed"
    mount -t sysfs    sysfs    /sys  || fail "mount /sys failed"

    # Load the boot-critical modules (deps resolved from the shrunk closure).
    # kmod's modprobe (not busybox's) handles the kernel's xz-compressed .ko.xz.
    ${kmodModprobe} -d ${bootModulesClosure} -a ${lib.concatStringsSep " " bootModules} \
      || fail "loading boot modules (${lib.concatStringsSep " " bootModules}) failed"

    # Assemble the whole-root overlay ((a)/(b)): erofs lower (the immutable
    # image on virtio-blk /dev/vda) + a tmpfs upper/work, so every path —
    # including /nix/store and /etc — is copy-on-write in-guest.
    mkdir -p /mnt/lower /mnt/rw /mnt/root
    mount -t erofs -o ro /dev/vda /mnt/lower || fail "mount erofs root (/dev/vda) failed"
    mount -t tmpfs tmpfs /mnt/rw || fail "mount overlay tmpfs failed"
    mkdir -p /mnt/rw/upper /mnt/rw/work
    mount -t overlay overlay \
      -o lowerdir=/mnt/lower,upperdir=/mnt/rw/upper,workdir=/mnt/rw/work \
      /mnt/root || fail "mount whole-root overlay failed"

    # No pre-switch_root existence check on /mnt/root/sbin/init: it is an
    # ABSOLUTE store symlink (-> /nix/store/…-compass-guestd/bin/compass-guestd),
    # so `test -x` would follow the symlink and resolve its absolute target
    # against the CURRENT process root — still the initramfs, where guestd is
    # absent — and fail-close on every correct image. switch_root below is the
    # gate: it chroots into /mnt/root first, so /sbin/init resolves in the
    # overlay where guestd exists, and it is itself `|| fail`-closed.

    # Hand off to the real guest init. switch_root tears down the initramfs and
    # execs /sbin/init as PID 1 in the overlay root.
    exec switch_root /mnt/root /sbin/init || fail "switch_root failed"
  '';

  # kmod's modprobe, referenced by absolute path from the init (so kmod lands in
  # the initrd closure). Split into its own binding to keep the init readable.
  kmodModprobe = "${pkgs.kmod}/bin/modprobe";

  # The initramfs image, built with nixpkgs' makeInitrd: a cpio of the init's
  # store closure (busybox, kmod, the shrunk boot-module tree, the init script)
  # compressed with zstd (CONFIG_RD_ZSTD=y in the pinned kernel). makeInitrd
  # walks the closure of every `object` and packs it, so the init's absolute
  # ${pkgs.busybox}/${pkgs.kmod}/${bootModulesClosure} references resolve inside
  # the unpacked cpio at boot. The init lands at /init — where the kernel execs
  # PID 1 from an initramfs. makeInitrd's cpio is itself reproducible (sorted,
  # fixed +0:+0 owners, epoch mtimes).
  initrdImage = pkgs.makeInitrd {
    name = "compass-guest-initrd-image";
    compressor = "zstd";
    contents = [
      {
        object = initScript;
        symlink = "/init";
      }
    ];
  };

  # The E3 rootfs contents tree, folded into a `let`-binding (record §T1). It is
  # a store-path symlink farm + a real writable resolv.conf + the kernel's full
  # /lib/modules tree; the erofs packing step below materializes its store
  # closure and packs it into the boot-consumable image. Assembled by hand
  # (rather than a bare `buildEnv`) so the resolv.conf lands as a real file and
  # the closure references stay explicit and reviewable.
  rootfsTree = pkgs.runCommand "compass-guest-rootfs-tree" { } ''
    mkdir -p $out/bin $out/sbin $out/etc $out/lib

    # The agent-image toolchain closure: its /bin and /etc, symlinked in. These
    # point into the store closure the packed image ships — the same
    # relocated-/etc + store-closure shape nix2container gives the OCI artifact.
    for f in ${toolchain}/bin/*; do
      ln -s "$f" "$out/bin/$(basename "$f")"
    done
    if [ -d ${toolchain}/etc ]; then
      cp -a ${toolchain}/etc/. $out/etc/
      # cp -a preserves the store's read-only dir/file modes; make the staged
      # /etc writable so the resolv.conf install below lands cleanly.
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

    # The real guest init, reachable both as /sbin/init and by name on PATH.
    ln -s ${guestd}/bin/compass-guestd $out/sbin/init
    ln -s ${guestd}/bin/compass-guestd $out/bin/compass-guestd

    # A writable /etc/resolv.conf — a real regular file, not a store symlink, so
    # the guest's net bringup can rewrite it through the tmpfs overlay at boot.
    install -Dm644 ${resolvConf} $out/etc/resolv.conf

    # The kernel's FULL /lib/modules tree (record §(a)): so guestd's
    # virtio_net/virtiofs/vsock transport and V3's in-guest netfilter arm
    # autoload on demand post-switch_root. Already in the closure (the kernel is
    # substituted) — zero extra build. A symlink into the modules output; the
    # erofs packing below dereferences it into the image.
    ln -s ${kernel.modules}/lib/modules $out/lib/modules
  '';

  # The store closure the rootfs symlink farm points into. Materialized into the
  # erofs image so the packed image is self-contained and bootable (record §(a):
  # /nix/store lives in the image, made writable by the overlay upper).
  rootfsClosure = pkgs.closureInfo { rootPaths = [ rootfsTree ]; };

  # A fixed filesystem UUID for the erofs image. mkfs.erofs otherwise stamps a
  # random UUID, which alone would defeat bit-reproducibility; pinning it (with
  # -T0 fixed timestamps and --all-root ownership) makes the image a pure
  # function of the closure — what lets V5's preflight hash-verify the asset
  # (record §(a), microvm-runner.md:221-224).
  rootfsUUID = "5da3f0a5-e0f5-4c0a-b0a1-c00000a55f5f";
in
{
  # Direct-boot kernel, substituted free from cache.nixos.org (never built): its
  # bzImage is ${compass-guest-kernel}/bzImage.
  compass-guest-kernel = kernel;

  # The packed rootfs: a reproducible read-only erofs image file (not the E3
  # tree) — the boot disk cloud-hypervisor attaches on virtio-blk (record §(a)).
  # $out IS the image file, so the CI leg's existing `COMPASS_TEST_GUEST_ROOTFS`
  # export (the attr's out-path) points straight at the bootable image with no
  # env-var change. Deterministic flags: -T0 (fixed build + file timestamps),
  # --all-root (uid/gid 0), -U <fixed uuid>. The build packs the tree twice and
  # `cmp`s the two images — the record's determinism check, run at build time so
  # any nondeterminism fails the gate rather than surfacing at V5 hash-verify.
  compass-guest-rootfs =
    pkgs.runCommand "compass-guest-rootfs.erofs"
      {
        nativeBuildInputs = [
          pkgs.erofs-utils
          pkgs.xz
        ];
      }
      ''
        root=$(mktemp -d)

        # Stage the rootfs tree (its store-path symlinks preserved) and the store
        # closure they resolve into, so the image is self-contained.
        cp -a ${rootfsTree}/. "$root/"
        # cp -a preserves the store's read-only dir modes; make the staged tree
        # writable so the /nix/store population and mkfs.erofs's own metadata
        # walk can proceed (--all-root normalizes ownership; -T0 normalizes
        # timestamps, so these staging perms never reach the packed image).
        chmod -R u+w "$root"
        mkdir -p "$root/nix/store"
        for p in $(cat ${rootfsClosure}/store-paths); do
          cp -a "$p" "$root/nix/store/"
        done

        # Pack twice with identical deterministic flags and assert bit-equality.
        # --workers=1 removes multi-threaded job-queue ordering as a determinism
        # variable; the tree is small, so the cost is negligible. Scope: this is
        # an intra-run smoke check only — it cannot catch cross-machine/-time/-tool
        # drift. The real cross-build reproducibility guarantee is nix's
        # input-addressing plus the stable -U/-T0/--all-root flags (verified with
        # `nix build --rebuild`); V5's preflight hash-verify is the load-bearing gate.
        flags="-T0 --all-root -U ${rootfsUUID} --workers=1"
        mkfs.erofs $flags img1.erofs "$root"
        mkfs.erofs $flags img2.erofs "$root"
        if ! cmp -s img1.erofs img2.erofs; then
          echo "guest-image: BUILD-BREAK — erofs image is not reproducible" >&2
          echo "  Two packs of the same tree differ; the deterministic flags" >&2
          echo "  ($flags) no longer guarantee a stable image. V5's preflight" >&2
          echo "  hash-verify depends on this invariant." >&2
          exit 1
        fi
        mv img1.erofs $out
      '';

  # The initramfs (record §(a)): a zstd-compressed cpio carrying only the
  # boot-critical module set + an init that loads them, mounts the erofs root +
  # tmpfs overlay, and switch_roots to /sbin/init. $out IS the initrd file
  # (mirroring the kernel's bzImage shape) — the CI leg exports it directly. The
  # derivation-time module-set check gates the build: a kernel-pin move that
  # drops a `=m` module fails here, not at boot.
  compass-guest-initrd =
    pkgs.runCommand "compass-guest-initrd"
      { }
      ''
        ${moduleConfigCheck}
        cp ${initrdImage}/initrd $out
      '';
}
