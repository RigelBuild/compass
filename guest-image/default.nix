# The Compass microVM guest image: the three nix attrs the cloud-hypervisor
# runtime boots per session guest. The rootfs userland IS the published agent
# OCI image unpacked, fetched fixed-output against `agent-oci.lock`, so
# guest/container drift is not expressible. Only the boot layer is added on top.
{ crossSystem ? null }:
let
  # The root devenv.lock-pinned nixpkgs, resolved as the other plain nix gates do
  # (read the lock, fetch that rev, import it). Supplies the BOOT layer only: the
  # agent userland comes from the OCI image, not from this pin.
  lock = builtins.fromJSON (builtins.readFile ../devenv.lock);
  node = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  # Choose the OCI member for the same target platform as the boot layer.
  pkgs = import nixpkgsSrc (if crossSystem == null then { } else { inherit crossSystem; });
  lib = pkgs.lib;
  agentPin = import ./agent-pin.nix { inherit lib; };
  agentLock = builtins.fromJSON (builtins.readFile ./agent-oci.lock);
  checkedLock = agentPin.checkLock agentLock;

  # The v2 registry API uses the lock's repository split into host and path.
  agentRegistry = "ghcr.io";
  agentPath = "rigelbuild/compass-agent";
  agentRepo = "${agentRegistry}/${agentPath}";
  agentIndex = pkgs.fetchurl {
    url = "https://${agentRegistry}/v2/${agentPath}/manifests/${checkedLock.digest}";
    curlOptsList = [
      "-H"
      "Authorization: Bearer QQ=="
      "-H"
      "Accept: application/vnd.oci.image.index.v1+json"
    ];
    hash = checkedLock.digest;
  };
  parsedIndex = builtins.fromJSON (builtins.readFile agentIndex);
  # Select from the pkgs that builds the boot layer, never the eval host.
  agentPlatform = agentPin.platformFor pkgs.stdenv.hostPlatform.system;
  memberDigest = agentPin.memberDigest {
    lock = checkedLock;
    index = parsedIndex;
    platform = agentPlatform;
  };
  agentManifest = pkgs.fetchurl {
    url = "https://${agentRegistry}/v2/${agentPath}/manifests/${memberDigest}";
    curlOptsList = [
      "-H"
      "Authorization: Bearer QQ=="
      "-H"
      "Accept: application/vnd.oci.image.manifest.v1+json"
    ];
    hash = memberDigest;
  };
  parsedManifest = builtins.fromJSON (builtins.readFile agentManifest);
  lockedLayers = agentPin.memberLayers {
    lock = checkedLock;
    manifest = parsedManifest;
    platform = agentPlatform;
  };
  agentLayers = map (
    digest:
    pkgs.fetchurl {
      url = "https://${agentRegistry}/v2/${agentPath}/blobs/${digest}";
      curlOptsList = [
        "-H"
        "Authorization: Bearer QQ=="
      ];
      hash = digest;
      # A nix-built image's layers embed /nix/store paths, which a fixed-output
      # fetch may not reference; the blob is opaque bytes here.
      derivationArgs.unsafeDiscardReferences.out = true;
    }
  ) lockedLayers;

  # The guest-side supervisor, running as guest PID 1: mounts the API
  # filesystems, brings networking up, serves the vsock Health handshake. Static
  # since a switch_root'd PID 1 cannot assume a libc; src is renamed off `go` so
  # buildGoModule's unpack cannot collide.
  guestd = pkgs.buildGoModule {
    pname = "compass-guestd";
    version = "0-v2a";
    src = builtins.path {
      path = ../go;
      name = "compass-go-src";
    };
    subPackages = [ "cmd/compass-guestd" ];
    proxyVendor = true;
    vendorHash = "sha256-ICzjNl8IK+K1WEvrnT5xmux9wUyQ2GWHVphwIk6THPo=";
    env.CGO_ENABLED = 0;
    ldflags = [
      "-s"
      "-w"
    ];
    # This derivation only packages the binary; guestd's logic is unit-tested
    # under the backend gate and the real boot is proved by the KVM-gated test.
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

  # The initramfs modprobes these before switch_root: nothing autoloads
  # afterwards (guestd IS init) and the loads persist across it. af_packet is
  # load-bearing -- guestd's DHCP raw socket fails EAFNOSUPPORT without it.
  # The check below breaks the build if a pin move flips one to `=y`.
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
        echo "  The initramfs assumes ${sym} is a loadable module." >&2
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
      # panics, so the cause is visible in the captured serial log.
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

    # No pre-switch_root check on /mnt/root/sbin/init: it is an ABSOLUTE store
    # symlink, so `test -x` resolves it against the CURRENT root (still the
    # initramfs, where guestd is absent) and fail-closes on every correct image.
    # switch_root chroots first, and is itself `|| fail`-closed.

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

  # The boot layer: everything the guest needs that the agent OCI image does not
  # carry. Kept as its own derivation so the erofs step can lay it over the
  # unpacked image and so its store closure is computed separately — the agent
  # image ships its own /nix/store, this does not.
  bootLayer = pkgs.runCommand "compass-guest-boot-layer" { } ''
    mkdir -p $out/sbin $out/bin $out/etc $out/lib

    # The real guest init, reachable both as /sbin/init and by name on PATH.
    ln -s ${guestd}/bin/compass-guestd $out/sbin/init
    ln -s ${guestd}/bin/compass-guestd $out/bin/compass-guestd

    # A writable /etc/resolv.conf — a real regular file, not a store symlink, so
    # the guest's net bringup can rewrite it through the tmpfs overlay at boot.
    install -Dm644 ${resolvConf} $out/etc/resolv.conf

    # The kernel's FULL /lib/modules tree, depmod metadata and all: guestd's
    # virtio_net/virtiofs/vsock transport and the in-guest netfilter arm resolve
    # their modules from here. Already in the closure (the kernel is
    # substituted), so this costs no extra build.
    ln -s ${kernel.modules}/lib/modules $out/lib/modules

    # The kernel's request_module() helper: without the binary named by
    # /proc/sys/kernel/modprobe it silently no-ops, making the module tree above
    # necessary but NOT sufficient -- nf_tables never registered and every
    # microVM Start failed to arm egress. kmod's: modules are .ko.xz.
    ln -s ${pkgs.kmod}/bin/modprobe $out/sbin/modprobe
  '';

  # The boot layer's store closure. The agent image carries its own store paths
  # inside its layers, so only these need materializing alongside.
  bootClosure = pkgs.closureInfo { rootPaths = [ bootLayer ]; };

  # The userland contract the Runner and guestd depend on, asserted at
  # derivation time against the ASSEMBLED tree: an agent-image change that
  # dropped one fails the build instead of booting a guest that cannot arm
  # egress (/bin/sh missing is a total backend outage) or run the agent.
  userlandContract = [
    "bin/sh"
    "bin/nft"
    "bin/getent"
    "bin/awk"
    "bin/compass-agent"
  ];

  # A fixed filesystem UUID for the erofs image. mkfs.erofs otherwise stamps a
  # random UUID, which would defeat bit-reproducibility; pinning it (with -T0 and
  # --all-root) makes the image a pure function of the closure, which is what
  # lets V5's preflight hash-verify the asset.
  rootfsUUID = "5da3f0a5-e0f5-4c0a-b0a1-c00000a55f5f";
  pinTests = import ./agent-pin-tests.nix { inherit lib; };
  pinSmoke =
    let
      armLock = checkedLock.platforms."linux/arm64";
      x86Lock = checkedLock.platforms."linux/amd64";
      pinFor = system:
        let image = import ./default.nix { crossSystem = { inherit system; }; };
        in image.compass-guest-rootfs.agentPin;
      arm = pinFor "aarch64-linux";
      x86 = pinFor "x86_64-linux";
      unsupported = builtins.tryEval (builtins.deepSeq (agentPin.platformFor "riscv64-linux") true);
      check = system: selected: platform: member:
        let
          manifestHash = "${selected.manifest.outputHashAlgo}:${selected.manifest.outputHash}";
          layerHashes = map (layer: "${layer.outputHashAlgo}:${layer.outputHash}") selected.layers;
        in
        if selected.platform != platform then throw "guest-image: eval smoke: ${system} selected platform ${selected.platform}, lock pins ${platform}"
        else if manifestHash != member.manifest then throw "guest-image: eval smoke: ${system} selected manifest ${manifestHash}, lock pins ${member.manifest}"
        else if layerHashes != member.layers then throw "guest-image: eval smoke: ${system} selected layers ${builtins.toJSON layerHashes}, lock pins ${builtins.toJSON member.layers}"
        else true;
    in
    if unsupported.success then throw "guest-image: eval smoke: riscv64-linux unexpectedly has an agent pin member"
    else check "aarch64-linux" arm "linux/arm64" armLock && check "x86_64-linux" x86 "linux/amd64" x86Lock;
in
  # Match the userland architecture to the kernel and boot layer being built.
{
  # Direct-boot kernel, substituted free from cache.nixos.org: its bzImage is
  # ${compass-guest-kernel}/bzImage.
  compass-guest-kernel = kernel;

  # The packed rootfs: a reproducible read-only erofs image, the boot disk
  # cloud-hypervisor attaches on virtio-blk. $out IS the image file, so the CI
  # leg's COMPASS_TEST_GUEST_ROOTFS points straight at it. The build packs twice
  # and `cmp`s the two, so nondeterminism fails here rather than at hash-verify.
  compass-guest-rootfs =
    pkgs.runCommand "compass-guest-rootfs.erofs"
      {
        nativeBuildInputs = [
          pkgs.erofs-utils
          pkgs.xz
        ];
        passthru.agentPin = {
          platform = agentPlatform;
          index = agentIndex;
          manifest = agentManifest;
          layers = agentLayers;
        };
      }
      ''
        root=$(mktemp -d)

        bash ${./assemble-layers.sh} unpack "$root" ${lib.concatStringsSep " " agentLayers}

        # Lay the boot layer over the unpacked image. It wins on any path
        # conflict: /sbin/init, /sbin/modprobe, /lib/modules and the writable
        # /etc/resolv.conf are the guest's, not the container's.
        cp -a --remove-destination ${bootLayer}/. "$root/"
        chmod -R u+w "$root"

        # The boot layer's own store closure. The agent layers already carry
        # their /nix/store paths, so only these are added.
        mkdir -p "$root/nix/store"
        while IFS= read -r p; do
          cp -a "$p" "$root/nix/store/"
        done < ${bootClosure}/store-paths

        # A setuid/setgid binary is a privilege path back to root, which the
        # guest's non-root agent must not have. Belt-and-braces: the unprivileged
        # builder cannot set those bits anyway (tar drops them), so this catches
        # a future privileged builder or a cp that preserves them.
        suid=$(find "$root" -type f -perm /6000 -printf '%M %P\n' || true)
        if [ -n "$suid" ]; then
          echo "guest-image: BUILD-BREAK — setuid/setgid files in the rootfs:" >&2
          printf '%s\n' "$suid" >&2
          exit 1
        fi

        bash ${./assemble-layers.sh} restore-dir-modes "$root" ${lib.concatStringsSep " " agentLayers}
        bash ${./assemble-layers.sh} check-contract "$root" \
          "${checkedLock.repo}@${memberDigest}" ${lib.concatStringsSep " " userlandContract}

        # Pack twice with identical flags and assert bit-equality; --workers=1
        # removes job-queue ordering as a variable. Intra-run smoke check only:
        # cross-machine drift is caught by nix's input-addressing plus the
        # stable flags, and by the preflight hash-verify.
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

  # Synthetic-layer fixtures for the unpack and contract logic the rootfs uses.
  compass-guest-assembly-tests = import ./assembly-tests.nix {
    inherit pkgs;
    script = ./assemble-layers.sh;
  };
  compass-guest-agent-pin-tests = assert pinTests; pkgs.runCommand "compass-guest-agent-pin-tests" { } ''
    touch $out
  '';
  compass-guest-agent-pin-smoke = pinSmoke;
}
