# syntax=docker/dockerfile:1.7-labs
# The compass-runner container image (R1 of the Compass Runner containerization
# record). Carries the Runner binary, the KVM userland it exec's
# (cloud-hypervisor, virtiofsd, passt) and the three guest assets the microVM
# backend direct-boots, so a Runner pod needs no host toolchain — only /dev/kvm
# and the session-volume mount.
#
# WHY A DOCKERFILE AND NOT nix2container. The fleet's first-party image-builds
# spec splits the two mechanisms by what the image's RUNTIME is, not by what
# built the artifact: an image that IS a Nix environment (a CI step image, a
# dev/agent shell) earns nix2container; a PREBUILT APPLICATION on a minimal base
# is a Dockerfile built by rootless BuildKit. It forecloses the tempting
# inference in terms — an image does not earn the nix path "merely because Nix
# built it or it ships a compiled binary" — and rejects nix2container for app
# images on cost. The Runner is the prebuilt-application row: it runs no
# toolchain, it exec's four binaries. The sibling agent-image/ sits on the other
# row BY NAME, because that container's job is to be a toolchain; sharing the
# `*-image/` directory shape is not sharing the mechanism.
#
# The prescribed shape for a nix-built artifact is therefore `nix build` it and
# COPY the result onto the base — which is exactly what build.sh stages into
# `store/` and what the single COPY below installs.

# Distroless STATIC, not :base and not Alpine. The carried binaries are
# dynamically linked but nix-closed: each names an ABSOLUTE /nix/store
# interpreter and resolves every NEEDED library through its own RPATH, so the
# closure supplies its own ld.so and glibc and the base is never consulted for a
# library. That makes the base's own libc dead weight, and `static` is the
# smallest base that still provides what IS needed from it: /etc/passwd +
# /etc/group (the nonroot uid), CA certificates (the Runner dials the Server
# over TLS), and /tmp. Verified: with the closure present all four binaries
# execute; with ONLY cloud-hypervisor's own store path copied — its glibc
# omitted — the same exec fails "missing dynamic library", which is the control
# proving the loader comes from the closure rather than the base.
#
# :nonroot selects uid/gid 65532. The Runner does not need root: R4 grants
# /dev/kvm access by supplementary group, not by uid. Pinned by digest, never by
# tag, for the same reason R2 pins this image's own digest downstream.
FROM gcr.io/distroless/static-debian12@sha256:c0f429e16b13e583da7e5a6ec20dd656d325d88e6819cafe0adb0828976529dc

# The realised nix closure: the Runner binary, the KVM userland and the guest
# assets, each at its own absolute /nix/store path. ONE COPY of a
# build.ts-staged directory rather than a COPY per component, because the store
# paths reference each other by absolute path — splitting them across layers by
# component would interleave a binary and its glibc into different layers for no
# cache benefit, since a Go rebuild moves the Runner path and nothing else.
#
# Ownership is left at the default (root-owned, world-readable) and the image
# runs as nonroot: the closure is read-only at runtime, so a non-root process
# needs no write access to it. This also keeps the layer bit-identical to the
# staged tree, which is what makes the digest reproducible across rebuilds.
COPY store /nix/store

# The stable entrypoint name is /nix/store/.compass-runner, a RELATIVE symlink
# build.ts stages inside store/ (so it arrives with the COPY above — there is no
# second COPY). ENTRYPOINT is exec-form and cannot expand a build ARG, while the
# Runner's own store path carries a hash that moves on every Go rebuild; the
# symlink gives the image a fixed entrypoint whose target moves with the build.
#
# It must be relative and inside the staged tree. An absolute symlink at the
# context root dangles on the BUILD HOST, and BuildKit checksums context entries
# before any COPY runs, so such a link fails the build outright with "not found"
# rather than resolving later inside the image.

# Absolute store paths, resolved at build time from the staged closure by
# build.ts and passed in as build args. They are baked into ENV rather than
# hardcoded because every path carries a content hash that moves on any rebuild
# of its input.
#
# The Runner reads each of these as the documented env fallback for the matching
# --microvm-* flag (main.go registerBackendFlags), so an operator can still
# override any one on the command line without rebuilding the image. Setting
# them here is what makes the image self-describing: the microVM backend's
# preflight is fail-closed (it requires both a static support probe and a real
# boot canary), so a missing or wrong path fails at startup, loudly, rather than
# at first session.
ARG RUNNER_BIN
ARG VMM_BIN
ARG VIRTIOFSD_BIN
ARG GUEST_KERNEL
ARG GUEST_ROOTFS
ARG GUEST_INITRD

ENV COMPASS_RUNTIME_BACKEND=microvm \
	COMPASS_MICROVM_VMM=${VMM_BIN} \
	COMPASS_MICROVM_VIRTIOFSD=${VIRTIOFSD_BIN} \
	COMPASS_MICROVM_KERNEL=${GUEST_KERNEL} \
	COMPASS_MICROVM_ROOTFS=${GUEST_ROOTFS} \
	COMPASS_MICROVM_INITRD=${GUEST_INITRD}

# passt is exec'd by name, not by configured path (unlike the VMM and virtiofsd,
# which have --microvm-* flags), so its bin dir must be on PATH. Prepended to
# the base PATH rather than replacing it.
ARG STACK_BIN_DIR
ENV PATH=${STACK_BIN_DIR}:/usr/local/bin:/usr/bin:/bin

# ENTRYPOINT, not CMD: the image runs exactly one program, and an ENTRYPOINT
# lets a pod spec's `args` add flags without re-stating the binary path. Exec
# form, so the Runner is pid 1 and receives SIGTERM directly at pod shutdown —
# a shell wrapper would swallow it and leave the graceful drain to the kill
# timeout.
ENTRYPOINT ["/nix/store/.compass-runner"]
