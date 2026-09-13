# The Compass microVM guest image: the three nix attrs V2a's cloud-hypervisor
# runtime consumes to boot a session guest — a direct-boot kernel, a packed erofs
# root filesystem, and a module initramfs. It reuses agent-image/'s toolchain
# closure, so the guest ships the same agent runtime as the container path — one
# closure, two artifact shapes (OCI layers there, a bootable erofs image here).
#
# Pin divergence (a parity note V2a honors): `agent-image/toolchain.nix` is
# called here with ROOT's `pkgs` (the root devenv.lock), NOT agent-image's own
# pin, so the whole rootfs closure resolves from the root pin. Deliberate: the
# guest-image moon gate's `inputs` track the root devenv.lock, so the gate
# reschedules on a root-pin move. agent-image's OCI build keeps its own pin; the
# two closures are the same shape through two nixpkgs revisions.
let
  # The root devenv.lock-pinned nixpkgs, resolved as the other plain nix gates do
  # (read the lock, fetch that rev, import it). This is the "root's pkgs" the pin
  # divergence turns on.
  lock = builtins.fromJSON (builtins.readFile ../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  lib = pkgs.lib;

  # The SAME bundled agent entrypoint and toolchain closure the agent image ships,
  # imported unchanged and fed root's `pkgs`. Their own relative imports resolve
  # against agent-image/, not this file, so importing them here is transparent.
  compassAgent = import ../agent-image/entrypoint.nix { inherit pkgs lib; };
  toolchain = import ../agent-image/toolchain.nix { inherit pkgs compassAgent; };

  # The real guest init (T2, go/cmd/compass-guestd): the guest-side supervisor —
  # mounts the API filesystems, brings networking up (in-process DHCP), mounts the
  # virtio-fs workspace, serves the vsock Health handshake as guest PID 1.
  # buildGoModule of the backend module; static (CGO_ENABLED=0) so it needs no
  # in-guest libc a switch_root'd PID 1 cannot assume.
  #   * src is renamed off `go` so buildGoModule's $GOPATH unpack does not collide
  #     ("go.mod file not found").
  #   * proxyVendor is required: wails/secretspec //go:embed patterns reference
  #     darwin/windows-only files a vendor-tree build would fail on; proxyVendor
  #     touches only the packages actually compiled for linux/amd64.
  #   * vendorHash pins the fetched module set; recompute with `lib.fakeHash` on a
  #     go.mod/go.sum move.
  guestd = pkgs.buildGoModule {
    pname = "compass-guestd";
    version = "0-v2a";
    src = builtins.path {
      path = ../go;
      name = "compass-go-src";
    };
    subPackages = [ "cmd/compass-guestd" ];
    proxyVendor = true;
    vendorHash = "sha256-hxjuJ8jRbNNnk4ZhXDIaFpSwno1hb6P7aeH0G9OWd8o=";
    env.CGO_ENABLED = 0;
    ldflags = [
      "-s"
      "-w"
    ];
    # This slice only packages the binary; guestd's logic is unit-tested under the
    # backend gate and the real boot is T4's KVM-gated proof.
    doCheck = false;
  };

  # A writable /etc/resolv.conf. The guest's net bringup provisions it at boot;
  # the egress arm then READS it to build the DNS allowlist. It MUST be a writable
  # REGULAR FILE (never a store symlink) so the guest can rewrite it through the
  # tmpfs overlay. The placeholder nameserver is inert: bringup overwrites it.
  resolvConf = pkgs.writeText "compass-guest-resolv.conf" ''
    # Provisioned by the guest net bringup at boot; read by the egress arm.
    nameserver 127.0.0.1
  '';

  # The pinned direct-boot kernel. cloud-hypervisor boots its uncompressed bzImage
  # directly; as an unmodified nixpkgs kernel at the root-pinned rev it is
  # substituted free from cache.nixos.org, never built in CI. Its modules live in
  # the separate `modules` output, consumed by both the rootfs and the initrd.
  kernel = pkgs.linuxPackages.kernel;

  # The module set the initramfs loads before switch_root, via kmod modprobe from
  # the shrunk closure, because the guest has no udev/systemd-modules-load to
  # autoload post-switch_root (guestd IS init). These loads persist across
  # switch_root, so every driver the guest needs is bound by the time guestd
  # starts: the boot-critical set mounts the root overlay (virtio transport +
  # block, erofs, overlayfs); the runtime set covers guestd's net/workspace/vsock
  # and af_packet (its in-process DHCP client's raw socket — without it the lease
  # fails EAFNOSUPPORT). Every one is `=m` in the pinned kernel; the check below
  # fails the build on a pin move that flips one to `=y` or drops it, rather than
  # shipping a guest that boots but cannot reach network, workspace, or host.
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

  # The .config symbol each named module is gated on, checked `=m` at build time.
  # Spelled out because the module→Kconfig mapping is not a mechanical
  # upper-casing. Only these are checked; transitive deps (vsock, fuse) ride the
  # closure.
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

  # Derivation-time assertion: the pinned kernel's .config still carries the exact
  # `=m` set the initramfs assumes. A shell snippet reused by the initrd build, so
  # a pin move that changes any of these fails the moon gate at the initrd
  # derivation instead of shipping a kernel the initrd cannot modprobe.
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

  # The boot-critical modules + their dependency closure, shrunk from the kernel's
  # `modules` output with depmod-generated modules.dep; modprobe in the init
  # resolves deps from this tree via `-d`. `kernel.modules` (not `kernel`): the
  # pinned kernel splits modules into a separate output.
  bootModulesClosure = pkgs.makeModulesClosure {
    kernel = kernel.modules;
    firmware = kernel.modules;
    rootModules = bootModules;
  };

  # The initramfs /init: a tiny module-load + mount-overlay + switch_root shim.
  # cloud-hypervisor loads it before the kernel, which execs /init as PID 1. It
  # carries no userland beyond the store paths its absolute references pull in
  # (busybox, kmod, the module closure), packed by makeInitrd's closure walk.
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

  # The initramfs image, built with nixpkgs' makeInitrd: a zstd cpio of the init's
  # store closure. makeInitrd walks the closure of every `object`, so the init's
  # absolute references resolve inside the unpacked cpio at boot; the init lands
  # at /init. The cpio is reproducible (sorted, +0:+0 owners, epoch mtimes).
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

  # The rootfs contents tree: a store-path symlink farm + a real writable
  # resolv.conf + the kernel's full /lib/modules tree; the erofs step below packs
  # its store closure into the bootable image. Assembled by hand (not `buildEnv`)
  # so resolv.conf lands as a real file and the closure references stay explicit.
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

    # The egress prerequisites (microvm-runner.md:446-449) plus /bin/sh. Already
    # present via the toolchain closure above (agent-image/toolchain.nix:144-147),
    # linked again here explicitly so the guest's contract does not depend on the
    # toolchain's internal package list. `ln -sf` because the toolchain loop may
    # already have created these names. /bin/sh is load-bearing under always-arm
    # (record §(e)): every microVM Start spawns `/bin/sh -c <script>` to arm
    # egress, so a missing /bin/sh is a total-backend outage, not egress-only.
    # bashInteractive is already in the rootfs closure via the toolchain, so the
    # link adds zero closure.
    ln -sf ${pkgs.nftables}/bin/nft $out/bin/nft
    ln -sf ${pkgs.getent}/bin/getent $out/bin/getent
    ln -sf ${pkgs.gawk}/bin/awk $out/bin/awk
    ln -sf ${pkgs.bashInteractive}/bin/sh $out/bin/sh

    # The real guest init, reachable both as /sbin/init and by name on PATH.
    ln -s ${guestd}/bin/compass-guestd $out/sbin/init
    ln -s ${guestd}/bin/compass-guestd $out/bin/compass-guestd

    # A writable /etc/resolv.conf — a real regular file, not a store symlink, so
    # the guest's net bringup can rewrite it through the tmpfs overlay at boot.
    install -Dm644 ${resolvConf} $out/etc/resolv.conf

    # The kernel's FULL /lib/modules tree (record §(a)), depmod metadata and
    # all: guestd's virtio_net/virtiofs/vsock transport and V3's in-guest
    # netfilter arm resolve their modules from here. Already in the closure (the
    # kernel is substituted) — zero extra build. A symlink into the modules
    # output; the erofs packing below dereferences it into the image.
    ln -s ${kernel.modules}/lib/modules $out/lib/modules

    # The kernel module-autoload usermode helper. The guest has no
    # udev/systemd-modules-load (guestd is PID 1, §(d)), so post-switch_root the
    # ONLY on-demand module loader is the kernel's request_module() path: when
    # in-kernel code needs an unloaded module it execs the binary named by
    # /proc/sys/kernel/modprobe (CONFIG_MODPROBE_PATH is unset in the pinned
    # kernel, so this defaults to /sbin/modprobe). Nothing staged that binary,
    # so request_module was a silent no-op and the /lib/modules tree above was
    # necessary but NOT sufficient (the false OQ-3 assumption, RIG-3028): the
    # first `nft` of the egress arm opened a NETLINK_NETFILTER socket, the
    # kernel fired request_module("net-pf-16-proto-12") -> nfnetlink, found no
    # helper, and nf_tables never registered -> EPROTONOSUPPORT (mnl.c:66), so
    # §(e) always-arm failed EVERY microVM Start. Staging kmod's modprobe here
    # (NOT busybox's: modules are .ko.xz and CONFIG_MODULE_DECOMPRESS is unset,
    # so the helper must decompress in userspace — the same reason the initrd
    # uses kmod at line 244) closes that: any module the guest asks for
    # autoloads on demand from the shipped tree via its depmod alias/dep
    # metadata. This is the general mechanism (not a fixed preload), so it also
    # covers egress rulesets beyond the base one — a future user-defined rule
    # pulling a new nft expression module autoloads with no guest-image change.
    # The ${pkgs.kmod} reference pulls kmod into the rootfs closure; the erofs
    # packing below materializes it into the image. Plain `ln -s` (no -f): no
    # prior modprobe name exists to overwrite, matching /sbin/init above.
    ln -s ${pkgs.kmod}/bin/modprobe $out/sbin/modprobe
  '';

  # The store closure the rootfs symlink farm points into, materialized into the
  # erofs image so it is self-contained and bootable (/nix/store lives in the
  # image, made writable by the overlay upper).
  rootfsClosure = pkgs.closureInfo { rootPaths = [ rootfsTree ]; };

  # A fixed filesystem UUID for the erofs image. mkfs.erofs otherwise stamps a
  # random UUID, which would defeat bit-reproducibility; pinning it (with -T0 and
  # --all-root) makes the image a pure function of the closure, which is what
  # lets V5's preflight hash-verify the asset.
  rootfsUUID = "5da3f0a5-e0f5-4c0a-b0a1-c00000a55f5f";
in
{
  # Direct-boot kernel, substituted free from cache.nixos.org: its bzImage is
  # ${compass-guest-kernel}/bzImage.
  compass-guest-kernel = kernel;

  # The packed rootfs: a reproducible read-only erofs image (the boot disk
  # cloud-hypervisor attaches on virtio-blk). $out IS the image file, so the CI
  # leg's COMPASS_TEST_GUEST_ROOTFS export points straight at it. Deterministic
  # flags (-T0, --all-root, -U <fixed>); the build packs twice and `cmp`s the two
  # at build time so any nondeterminism fails the gate, not V5 hash-verify.
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

  # The initramfs: a zstd cpio carrying the boot-critical module set + an init
  # that loads them, mounts the erofs root + tmpfs overlay, and switch_roots to
  # /sbin/init. $out IS the initrd file. The module-set check gates the build:
  # a kernel-pin move that drops a `=m` module fails here, not at boot.
  compass-guest-initrd =
    pkgs.runCommand "compass-guest-initrd"
      { }
      ''
        ${moduleConfigCheck}
        cp ${initrdImage}/initrd $out
      '';
}
