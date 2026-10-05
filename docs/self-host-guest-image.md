# Guest image: materialisation and bumps

This page applies to microVM-backed agent sessions. On that backend the runner
direct-boots three guest assets: a kernel, an erofs rootfs, and an initrd. The
rootfs userland is the published `compass-agent` image, unpacked. This page
covers how those files reach the runner, how to stage them on an air-gapped
host, and how a new agent image becomes a new guest.

## How the runner consumes the guest

The runner takes three file paths, and nothing else:
`--microvm-kernel`, `--microvm-rootfs`, `--microvm-initrd` (or
`$COMPASS_MICROVM_KERNEL`, `$COMPASS_MICROVM_ROOTFS`, `$COMPASS_MICROVM_INITRD`).
When `--microvm-image-manifest` is also set, runner preflight hashes each file
against that `sha256sum`-format manifest and refuses to start on a mismatch.
Without a manifest, preflight checks only that the files exist. The runner has
no registry client.

On the microVM backend the agent runs from the guest rootfs, so the runner
refuses `--image`. `compass-stack` still requires `--image` on the command
line, but does not forward it when `--runtime-backend microvm` is set.

### Run root

Every microVM runner needs a session run root, or preflight fails with
`run-root is not configured`. `compass-stack` does not set one. Export
`COMPASS_MICROVM_RUNROOT` in the environment `compass-stack up` runs in, and the
runner inherits it. A runner you start yourself can take `--microvm-runroot`
instead. Keep the path short, because session socket paths must fit the
AF_UNIX limit.

## Choosing a materialisation source

There are three sources. Two are `compass-stack up` flags. The third is the
Runner container image, which carries its own copy.

| Source | Where it is set | Guest fetch | Hash-verified at preflight |
| --- | --- | --- | --- |
| Pulled artifact | `compass-stack up --guest-artifact <ref>@sha256:<hex>` (`$COMPASS_GUEST_ARTIFACT`) | From GHCR, anonymous | Yes |
| Staged directory | `compass-stack up --guest-dir <abs path>` (`$COMPASS_GUEST_DIR`) | None | Yes, against the directory's own manifest |
| Baked into the Runner image | `ghcr.io/rigelbuild/compass-runner` container environment | None | No, presence only |

Both flags require `--runtime-backend microvm` (or
`$COMPASS_RUNTIME_BACKEND=microvm`). They are mutually exclusive. For each, the
flag wins over the environment variable.

"Guest fetch" covers the guest only. The bundled database, NATS, and collector
are container images that `up` also pulls. An air-gapped host must preload them
or use the matching `--*-external` options.

### Pulled artifact

The guest assets publish to `ghcr.io/rigelbuild/compass-guest-image` as a
non-runnable OCI artifact. It has three layers (kernel, rootfs, initrd) and a
sha256 annotation per layer. Each push to `main` that changes the guest closure
publishes a `:git-<sha12>` tag.

Deploy by digest, never by tag. The trusted source for the digest is the
`publish-guest-image` job in `.github/workflows/release.yml`: its step summary
prints `guest image: ghcr.io/rigelbuild/compass-guest-image@sha256:<hex>`. Use
that reference.

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --runtime-backend microvm \
    --guest-artifact ghcr.io/rigelbuild/compass-guest-image@sha256:<hex>
```

To check that a tag still points at that digest, hash the raw manifest.
`skopeo inspect --format '{{.Digest}}'` does not work here, because skopeo
refuses image operations on this artifact type.

```console
$ skopeo inspect --raw docker://ghcr.io/rigelbuild/compass-guest-image:git-<sha12> \
    | sha256sum
e094962721798bd57e7b8da892dddac9799f8f07fc62fb5c5217297275f5851a  -
```

`up` fetches the manifest and the three blobs and checks each blob's sha256
against its descriptor. It then renames them into
`<state-dir>/guest-image/<hex>/`, next to `manifest.sha256` and the raw
`manifest.oci.json`. A later `up` with the same digest reuses that directory
after re-verifying it. A fetch or verification failure stops `up` before the
runner starts.

### Staged directory (air-gapped)

`--guest-dir` points at a directory that already holds:

| File | Contents |
| --- | --- |
| `kernel` | The direct-boot kernel (`bzImage`) |
| `rootfs.erofs` | The guest root filesystem |
| `initrd` | The guest initramfs |
| `manifest.sha256` | `sha256sum` output for the three files above, by these names |

`up` fetches nothing. It checks that each file exists, is a regular file, and is
non-empty. Then it passes the four paths to the runner. Runner preflight hashes
the three assets against `manifest.sha256`, so a changed file fails with a
`digest mismatch` error. The path must be absolute.

The manifest travels with the files, so it catches corruption and partial
copies. It does not catch someone who replaces an asset and the manifest
together. Before you accept a transferred directory, compare its
`manifest.sha256` against hashes from a trusted source. The runbook below says
how.

This is a supported deployment path, not a fallback.

### Baked into the Runner image

The `ghcr.io/rigelbuild/compass-runner` container image carries the three
assets in its nix closure. Its environment sets `COMPASS_MICROVM_KERNEL`,
`COMPASS_MICROVM_ROOTFS`, and `COMPASS_MICROVM_INITRD` to them, and sets no
image manifest. A runner started from that container image boots them with no
guest flags, after a presence check only.

This source does not apply to `compass-stack up`, which starts the
`compass-runner` binary from the host `PATH`, not the container image. With
neither guest flag set, `compass-stack` passes no guest paths. The host runner
then needs the three `COMPASS_MICROVM_*` paths in the environment, or preflight
fails.

## Rule: one deployment, one source

Use exactly one materialisation source per deployment: baked, or a pulled or
staged artifact. Never mix them.

Nothing compares a baked copy against an artifact copy. The artifact and
staged paths verify against their own manifest, and the baked path is not
hash-verified at all. A Runner image built at commit A and an artifact from
commit B both pass preflight, while you believe they match. Pick one source and
change versions only through that source.

## Air-gapped runbook

Build the guest directory on a connected machine, verify it, carry it across,
and point `--guest-dir` at it.

### Option A: copy a materialised artifact

On a connected host, run `compass-stack up --guest-artifact …` once with the
digest from the publish job summary, as above. Then copy
`<state-dir>/guest-image/<hex>/` as it is. It holds the four files `--guest-dir`
needs, and `up` wrote its `manifest.sha256` only after the blobs matched the
pinned digest.

### Option B: extract from the Runner image

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

This manifest only records what you extracted. Verify it against a published
guest artifact before you trust it. The Runner and guest images publish under
separate path gates, so a Runner tag may have no guest tag at the same commit.
Compare against the newest guest artifact published at or before the Runner
image's commit.

The artifact manifest carries one annotation per asset, holding bare hex:

| Annotation | File |
| --- | --- |
| `org.compass.guest.layer.bzImage` | `kernel` |
| `org.compass.guest.layer.compass-guest-rootfs.erofs` | `rootfs.erofs` |
| `org.compass.guest.layer.compass-guest-initrd` | `initrd` |

```console
skopeo inspect --raw docker://ghcr.io/rigelbuild/compass-guest-image@sha256:<hex>
```

Each value must equal the matching line of your `manifest.sha256`.

### Bring up the air-gapped host

Transfer the directory, for example to `/var/lib/compass-guest/<sha12>/`. Set
the run root (see [Run root](#run-root)), then start the stack:

```console
$ COMPASS_MICROVM_RUNROOT=/run/compass-vm compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --runtime-backend microvm \
    --guest-dir /var/lib/compass-guest/<sha12>
```

Keep one directory per version. To roll forward, stage the new directory and
change `--guest-dir`. To roll back, point it back at the old one.

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
   changes `guest-image/`, so the CI gate builds the guest from the new lock.
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

## Rule: recovering a red guest gate

When the guest-image build fails on the agent lock, do not edit hashes. Rerun
the pin tool and commit the refreshed lock on the same branch.

The nix evaluation fails closed with one of two errors, and each names the fix:

- `agent-oci.lock is not a valid pin`: the lock is malformed.
- `agent-oci.lock layers do not match the manifest it pins`: the layer list is
  stale. The usual cause is a partial bump.

Both are recovered with the relock that Renovate's post-upgrade task runs:

```console
bun tools/guest-image/pin-agent-image.ts --relock
```

`--relock` re-resolves the immutable tag that `:latest` points at, and rewrites
every field from the registry. To keep the currently pinned build instead,
re-pin its tag (the `tag` field in the lock) with `--tag git-<sha12>`. That
prints `no change` when the lock already matches.

Commit the rewritten `guest-image/agent-oci.lock`. The gate then rebuilds
against digests read from the registry.
