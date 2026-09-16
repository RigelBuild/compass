# The Compass microVM guest image: the three nix attrs V2a's cloud-hypervisor
# runtime consumes to boot a session guest. The rootfs userland IS the published
# agent OCI image unpacked, fetched fixed-output against `agent-oci.lock`, so
# guest/container drift is not expressible. Only the boot layer is added on top.
let
  # The root devenv.lock-pinned nixpkgs, resolved as the other plain nix gates do
  # (read the lock, fetch that rev, import it). Supplies the BOOT layer only: the
  # agent userland comes from the OCI image, not from this pin.
  lock = builtins.fromJSON (builtins.readFile ../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  lib = pkgs.lib;

  # The pinned agent image, written only by tools/guest-image/pin-agent-image.ts.
  # The shape is re-checked HERE as well as in the pin tool: the tool guards the
  # write path, this guards the eval, and a hand-edited lock has to defeat both.
  agentLock = builtins.fromJSON (builtins.readFile ./agent-oci.lock);

  # Split registry host from repository path: the lock stores the full
  # reference, the v2 API needs the two separately.
  agentRegistry = "ghcr.io";
  agentPath = "rigelbuild/compass-agent";
  agentRepo = "${agentRegistry}/${agentPath}";

  # A digest is only a pin if it is a real sha256. `match` returns null on any
  # deviation, so a truncated or hex-invalid digest fails eval instead of
  # reaching fetchurl as an unenforceable hash.
  isSha256 = s: builtins.isString s && builtins.match "sha256:[0-9a-f]{64}" s != null;

  # Fail at eval, naming the field, rather than letting a malformed lock surface
  # as an opaque fetch or hash error deep in the build.
  checkedLock =
    let
      bad =
        if !builtins.isAttrs agentLock then
          "lock must be a JSON object"
        else if !builtins.isString (agentLock.repo or null) || agentLock.repo != agentRepo then
          "repo must be ${agentRepo}, got ${toString (agentLock.repo or "<missing>")}"
        else if
          !builtins.isString (agentLock.tag or null) || builtins.match "git-[0-9a-f]{12}" agentLock.tag == null
        then
          "tag must match git-<sha12>, got ${toString (agentLock.tag or "<missing>")}"
        else if !isSha256 (agentLock.digest or "") then
          "digest must be sha256:<64 hex>, got ${toString (agentLock.digest or "<missing>")}"
        else if !builtins.isList (agentLock.layers or null) || agentLock.layers == [ ] then
          "layers must be a non-empty list"
        else if !builtins.all isSha256 agentLock.layers then
          "every layer must be sha256:<64 hex>"
        else
          null;
    in
    if bad == null then
      agentLock
    else
      throw "guest-image: agent-oci.lock is not a valid pin: ${bad}. Rewrite it with `bun tools/guest-image/pin-agent-image.ts --relock`, never by hand.";

  # The pinned manifest, fetched fixed-output against the lock's digest: a
  # manifest digest IS the sha256 of its body, so nix's hash check authenticates
  # it outright. Without this the digest would be decoration -- the layer
  # descriptors alone decide what gets unpacked.
  agentManifest = pkgs.fetchurl {
    url = "https://${agentRegistry}/v2/${agentPath}/manifests/${checkedLock.digest}";
    curlOptsList = [
      "-H"
      "Authorization: Bearer QQ=="
      "-H"
      "Accept: application/vnd.oci.image.manifest.v1+json"
    ];
    hash = checkedLock.digest;
  };

  # The manifest's own ordered layer list. Reading it here rather than trusting
  # the lock's copy binds the unpacked bytes to the pinned image: a hand-edited
  # lock pairing this digest with another image's valid layers no longer builds.
  manifestLayers = map (l: l.digest) (builtins.fromJSON (builtins.readFile agentManifest)).layers;

  lockedLayers =
    if manifestLayers == checkedLock.layers then
      checkedLock.layers
    else
      throw "guest-image: agent-oci.lock layers do not match the manifest it pins (${checkedLock.digest}). Rewrite it with `bun tools/guest-image/pin-agent-image.ts --relock`, never by hand.";

  # Each layer blob, fetched fixed-output against its descriptor digest -- the
  # registry's own content address, where one archive's narHash would track
  # skopeo's byte layout. The bearer is GHCR's literal anonymous public-read
  # token, not a credential: an unauthenticated blob GET 401s demanding one.
  agentLayers = map (
    digest:
    pkgs.fetchurl {
      url = "https://${agentRegistry}/v2/${agentPath}/blobs/${digest}";
      curlOptsList = [
        "-H"
        "Authorization: Bearer QQ=="
      ];
      hash = digest;
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
    vendorHash = "sha256-hxjuJ8jRbNNnk4ZhXDIaFpSwno1hb6P7aeH0G9OWd8o=";
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
in
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
      }
      ''
        root=$(mktemp -d)

        # Whiteouts in a layer hide only the layers BELOW it, so each layer's
        # markers are applied to the accreting tree before its own content is
        # extracted -- an opaque marker applied afterwards would delete the
        # sibling files that same layer adds.
        for layer in ${lib.concatStringsSep " " agentLayers}; do
          members=$(tar -tzf "$layer" | sed 's|^\./||')

          # Archive names drive the rm -rf and tar writes below, so a `..`
          # component fails the build rather than being sanitized: tar would
          # follow it out of the staged tree.
          if printf '%s\n' "$members" | grep -qE '(^|/)\.\.(/|$)'; then
            echo "guest-image: BUILD-BREAK — layer $layer has a '..' member path." >&2
            exit 1
          fi

          # A layer replacing a lower layer's symlink with a real directory is
          # legitimate OCI, but tar would write THROUGH the stale symlink --
          # outside $root when its target is absolute. Drop such parents so tar
          # materializes a directory instead.
          for d in $(printf '%s\n' "$members" | sed -n 's|/[^/]*$||p' | sort -u); do
            p=""
            IFS=/
            for c in $d; do
              [ -n "$c" ] || continue
              p="$p/$c"
              if [ -L "$root$p" ]; then rm -f "$root$p"; fi
            done
            unset IFS
          done

          # A marker is a `.wh.`-prefixed final COMPONENT. Matching the
          # substring anywhere would read an ordinary path that merely contains
          # `.wh.` as a delete order against a file the layer legitimately
          # ships.
          printf '%s\n' "$members" | { grep -E '(^|/)\.wh\.' || true; } | while read -r marker; do
            dir=$(dirname "$marker")
            base=$(basename "$marker")
            case "$base" in
              .wh.*) ;;
              *) continue ;;
            esac
            if [ "$base" = ".wh..wh..opq" ]; then
              # A marker naming a path the lower layers never created means the
              # layer stack is not what this build thinks it is; fail loudly
              # rather than swallowing the status.
              if [ ! -d "$root/$dir" ]; then
                echo "guest-image: BUILD-BREAK — opaque marker names a missing directory $dir." >&2
                exit 1
              fi
              find "$root/$dir" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
            else
              rm -rf "$root/$dir/''${base#.wh.}"
            fi
          done

          # -p restores each member's recorded mode; without it every member
          # takes the builder's umask instead (measured: /bin packed 0755 where
          # the image ships 0555). Staging write access is re-opened on
          # DIRECTORIES only, leaving file modes as published.
          tar -xzpf "$layer" -C "$root" --overwrite \
            --exclude='.wh.*' --exclude='*/.wh.*'
          find "$root" -type d -exec chmod u+w {} +
        done

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

        # Re-extract the directory members to restore their published modes,
        # undoing the staging u+w. / is then set outright: `cp -a ${bootLayer}/.`
        # stamps the store's 0555 onto it, so leaving it to a chmod's residue
        # makes / untraversable or accidentally right depending on ordering.
        for layer in ${lib.concatStringsSep " " agentLayers}; do
          # Select directory members by tar's type flag: these layers list
          # directories with no trailing slash, so matching on one restores
          # nothing.
          if tar -tvzf "$layer" | awk '$1 ~ /^d/ {print $NF}' > dirs.txt; then
            tar -xzpf "$layer" -C "$root" --overwrite --no-recursion -T dirs.txt
          fi
        done
        chmod 0755 "$root"

        # The userland contract, checked on the ASSEMBLED tree. EVERY component
        # resolves inside $root, as the kernel will after switch_root: following
        # only the final symlink resolves an intermediate /bin -> /nix/store/...
        # against the BUILDER's store, passing an image with no /bin/sh at all.
        for p in ${lib.concatStringsSep " " userlandContract}; do
          resolved=""
          rest="/$p"
          hops=0
          while [ -n "$rest" ]; do
            comp=''${rest#/}
            comp=''${comp%%/*}
            tail=''${rest#/"$comp"}
            case "$comp" in
              . | "")
                rest="$tail"
                continue
                ;;
              ..)
                resolved=$(dirname "$resolved")
                [ "$resolved" = "/" ] || [ "$resolved" = "." ] && resolved=""
                rest="$tail"
                continue
                ;;
            esac
            resolved="$resolved/$comp"
            if [ -L "$root$resolved" ]; then
              # Count symlink traversals, not path components: a deep
              # symlink-free path is not a loop, and Linux's own ELOOP is 40.
              hops=$((hops + 1))
              if [ "$hops" -gt 40 ]; then
                echo "guest-image: BUILD-BREAK — /$p is a symlink loop in the image." >&2
                exit 1
              fi
              link=$(readlink "$root$resolved")
              case "$link" in
                /*)
                  resolved=""
                  rest="$link$tail"
                  ;;
                *)
                  resolved=$(dirname "$resolved")
                  [ "$resolved" = "/" ] || [ "$resolved" = "." ] && resolved=""
                  rest="/$link$tail"
                  ;;
              esac
            else
              rest="$tail"
            fi
          done
          target="$resolved"
          if [ ! -f "$root$target" ] || [ ! -x "$root$target" ]; then
            echo "guest-image: BUILD-BREAK — /$p is not an executable in the rootfs." >&2
            echo "  It resolves to $target, which the image does not carry as one." >&2
            echo "  The guest userland comes from the pinned agent image" >&2
            echo "  (${checkedLock.repo}@${checkedLock.digest})." >&2
            echo "  Either that image stopped shipping it, or a layer failed to" >&2
            echo "  unpack. /bin/sh missing is a TOTAL backend outage: every" >&2
            echo "  microVM Start shells out to arm egress." >&2
            exit 1
          fi
        done

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
}
