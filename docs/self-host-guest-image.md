# Guest image: materialisation and bumps

Every agent session runs in a microVM. The runner direct-boots three guest
assets: a kernel, an erofs rootfs, and an initrd. The rootfs userland is the
published `compass-agent` image, unpacked. This page covers how those three
files reach a self-hosted runner, how to run fully air-gapped, and how a new
agent image becomes a new guest.

## How the runner consumes the guest

The runner takes three file paths, and nothing else:
`--microvm-kernel`, `--microvm-rootfs`, `--microvm-initrd`. When
`--microvm-image-manifest` is also set, runner preflight hashes each file
against that `sha256sum`-format manifest and refuses to start on a mismatch.
The runner has no registry client. Any mechanism that puts verified files on
the host before the runner starts satisfies it.

On the microVM backend the agent runs from the guest rootfs, so the runner
refuses `--image`. `compass-stack` still requires `--image` on the command
line, but does not forward it when `--runtime-backend microvm` is set.

## Choosing a materialisation source

`compass-stack up` takes one of three sources. All three require
`--runtime-backend microvm` (or `$COMPASS_RUNTIME_BACKEND=microvm`).

| Source | Flag (env) | Network at `up` | Use when |
| --- | --- | --- | --- |
| Pulled artifact | `--guest-artifact <ref>@sha256:<hex>` (`$COMPASS_GUEST_ARTIFACT`) | GHCR, anonymous | A host with registry access. The default for self-host. |
| Pre-materialised dir | `--guest-dir <abs path>` (`$COMPASS_GUEST_DIR`) | None | Air-gapped hosts, or any host where you stage files yourself. |
| Baked into the Runner image | neither flag | None | The Runner container image. The assets are already in it. |

`--guest-artifact` and `--guest-dir` are mutually exclusive. For each, the flag
wins over the environment variable.

### Pulled artifact

The guest assets publish to `ghcr.io/rigelbuild/compass-guest-image` as a
non-runnable OCI artifact. It has three layers (kernel, rootfs, initrd) and its
own sha256 annotation per layer. Each closure-affecting push to `main`
publishes a `:git-<sha12>` tag. The `publish-guest-image` job in
`.github/workflows/release.yml` writes the digest reference to the job's step
summary.

The flag takes a digest reference, never a tag. Resolve a tag to its digest
once:

```console
$ skopeo inspect --raw docker://ghcr.io/rigelbuild/compass-guest-image:git-<sha12> \
    | sha256sum
e094962721798bd57e7b8da892dddac9799f8f07fc62fb5c5217297275f5851a  -
```

`skopeo inspect --format '{{.Digest}}'` does not work here: skopeo refuses
image-specific operations on this artifact type. Hash the raw manifest instead.

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --runtime-backend microvm \
    --guest-artifact ghcr.io/rigelbuild/compass-guest-image@sha256:<hex>
```

`up` fetches the manifest and the three blobs and checks each blob's sha256
against its descriptor. It then renames them into
`<state-dir>/guest-image/<hex>/`, next to `manifest.sha256` and the raw
`manifest.oci.json`. A later `up` with the same digest reuses that directory
after re-verifying it. A fetch or verification failure stops `up` before the
runner starts.

### Pre-materialised directory (air-gapped)

`--guest-dir` points at a directory that already holds:

| File | Contents |
| --- | --- |
| `kernel` | The direct-boot kernel (`bzImage`) |
| `rootfs.erofs` | The guest root filesystem |
| `initrd` | The guest initramfs |
| `manifest.sha256` | `sha256sum` output for the three files above, by these names |

`up` fetches nothing. It checks that each file exists, is a regular file, and is
non-empty. Then it passes the four paths to the runner. The hash check happens
in runner preflight, against `manifest.sha256`. A tampered file or a stale
manifest therefore fails at preflight with a `digest mismatch` error. The path
must be absolute.

This is a supported deployment path, not a fallback. The runbook below builds
the directory.

### Baked into the Runner image

The `ghcr.io/rigelbuild/compass-runner` container image carries the three
assets in its nix closure. Its environment points the runner at them through
`COMPASS_MICROVM_KERNEL`, `COMPASS_MICROVM_ROOTFS`, and
`COMPASS_MICROVM_INITRD`. A Runner container started from that image needs no
guest flags and no network for the guest. With neither guest flag set,
`compass-stack` passes no guest paths, and the runner falls back to those
environment variables.

## Rule: one deployment, one source

Use exactly one materialisation source per deployment: baked, or a pulled or
staged artifact. Never mix them.

Each copy carries its own manifest, so each copy verifies clean on its own.
Nothing compares a baked copy against an artifact copy. A Runner image built at
commit A and an artifact from commit B both pass preflight, while you believe
they match. Pick one source and change versions only through that source.

## Air-gapped runbook

Build the guest directory on a connected machine, carry it across, and point
`--guest-dir` at it. There are two ways to get the files.

### Option A: extract from the Runner image

Run this on a machine with podman and registry access. The baked environment
variables name each asset's path inside the image.

```bash
set -euo pipefail
image=ghcr.io/rigelbuild/compass-runner:git-<sha12>
out=./compass-guest
mkdir -p "$out"
podman pull "$image"
ctr=$(podman create "$image")
for pair in KERNEL:kernel ROOTFS:rootfs.erofs INITRD:initrd; do
  var="COMPASS_MICROVM_${pair%%:*}"
  src=$(podman image inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' "$image" \
    | sed -n "s|^${var}=||p")
  podman cp "${ctr}:${src}" "${out}/${pair##*:}"
done
podman rm "$ctr"
(cd "$out" && sha256sum kernel rootfs.erofs initrd > manifest.sha256)
```

The manifest generated here records the hashes of the extracted files. Check it
against the published artifact for the same commit. The
`org.compass.guest.layer.*` annotations on the artifact manifest carry the
expected sha256 of each asset:

```console
skopeo inspect --raw docker://ghcr.io/rigelbuild/compass-guest-image:git-<sha12>
```

### Option B: copy a materialised artifact

On a connected host, run `compass-stack up --guest-artifact …` once, as above.
Then copy `<state-dir>/guest-image/<hex>/` as it is. It already holds the four
files `--guest-dir` needs, and its `manifest.sha256` was written from the
verified descriptors.

### Bring up the air-gapped host

Transfer the directory, for example to `/var/lib/compass-guest/<sha12>/`, and
start the stack:

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --runtime-backend microvm \
    --guest-dir /var/lib/compass-guest/<sha12>
```

Keep one directory per version. To roll forward, stage the new directory and
change `--guest-dir`. To roll back, point it back at the old one.

The runner also needs its session run root, `--microvm-runroot` or
`$COMPASS_MICROVM_RUNROOT`, in its environment. Without it, preflight fails with
`run-root is not configured`. `compass-stack` does not set it. Keep it short,
because session socket paths must fit the AF_UNIX limit.

## Agent-image bump flow

A new agent image reaches deployments in four steps:

1. **Publish the agent image.** A closure-affecting push to `main` publishes
   `ghcr.io/rigelbuild/compass-agent:git-<sha12>` and moves `:latest`. See
   [Publishing the agent image](architecture/build-and-ci.md#publishing-the-agent-image).
2. **Renovate opens the pin PR.** The `compass-agent-guest` manager tracks the
   `:latest` digest. Its post-upgrade task runs
   `bun tools/guest-image/pin-agent-image.ts --relock`. That rewrites
   `guest-image/agent-oci.lock` with the immutable tag, the manifest digest, and
   the per-layer digests the nix fetches key on.
3. **The guest-image gate builds, then the artifact republishes.** The pin PR
   affects `guest-image/`, so the CI gate builds the guest from the new lock.
   After merge, `publish-guest-image` publishes a new
   `compass-guest-image:git-<sha12>`. The Runner image is rebuilt from the same
   tree, with the same assets baked in.
4. **Deployments advance.** Each deployment moves its own source: a new
   `--guest-artifact` digest, a new `--guest-dir` directory, or a new Runner
   image tag. Nothing advances a deployment automatically.

To pin a specific build by hand instead of waiting for Renovate:

```console
bun tools/guest-image/pin-agent-image.ts --tag git-<sha12>
```

Never edit `agent-oci.lock` by hand. Both the pin tool and the nix evaluation
validate it.

## Rule: a red guest gate on a fetch-hash mismatch

When the guest-image build fails on a fixed-output hash mismatch for an agent
layer, the lock no longer matches the registry. Do not edit hashes. Rerun the
pin tool and commit the refreshed lock on the same branch.

On a Renovate pin PR, rerun the same relock its post-upgrade task ran. It
re-resolves the tag `:latest` points at and rewrites every field from the
registry:

```console
bun tools/guest-image/pin-agent-image.ts --relock
```

To keep the pinned build and only refresh its digests, re-pin the lock's own
tag (the `tag` field in `guest-image/agent-oci.lock`):

```console
bun tools/guest-image/pin-agent-image.ts --tag git-<sha12>
```

Either way, commit the rewritten `guest-image/agent-oci.lock`. The gate then
rebuilds against layer digests read from the registry.
