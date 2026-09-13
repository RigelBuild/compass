# syntax=docker/dockerfile:1.7-labs
# The compass-runner container image (R1 of the Compass Runner containerization
# record). Carries the Runner binary, the KVM userland it exec's
# (cloud-hypervisor, virtiofsd, passt) and the three guest assets the microVM
# backend direct-boots, so a Runner pod needs no host toolchain — only /dev/kvm
# and the session-volume mount.
#
# A Dockerfile on a minimal base, not a nix2container image: the mechanism
# follows the image's RUNTIME, and the Runner is a prebuilt application that
# runs no toolchain. Full rationale in the design record's R1 task; nix still
# builds every carried artifact, and build.ts COPYs the result.

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
# The pinned digest is the nonroot variant (uid/gid 65532). The Runner needs no
# root: /dev/kvm access is granted by supplementary group at the pod layer.
# Pinned by digest, never by tag — GHCR-style tag mutability gives no
# immutability guarantee.
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
# Consumed by the dockerfile frontend (paired with the export's
# rewrite-timestamp) to fix the config `created` field and the layer mtimes, so
# two builds of the same inputs yield the same digest.
ARG SOURCE_DATE_EPOCH

ARG STACK_BIN_DIR
ENV PATH=${STACK_BIN_DIR}:/usr/local/bin:/usr/bin:/bin

# ENTRYPOINT, not CMD: the image runs exactly one program, and an ENTRYPOINT
# lets a pod spec's `args` add flags without re-stating the binary path. Exec
# form, so the Runner is pid 1 and receives SIGTERM directly at pod shutdown —
# a shell wrapper would swallow it and leave the graceful drain to the kill
# timeout.
# Explicit, rather than inherited from the base's own config: a future digest
# bump that landed on the root `static` variant would otherwise silently produce
# a root-running container, and nothing here would catch it.
USER 65532:65532

ENTRYPOINT ["/nix/store/.compass-runner"]
