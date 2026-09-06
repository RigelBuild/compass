//go:build unix

package stack

// DefaultNatsImage is the container image the bundled NATS component runs by
// default: the official `nats` image (which ships nats-server), the -alpine
// variant, pinned BY DIGEST. The alpine variant is chosen over the scratch-based
// default tag because the stack bind-mounts a config file and a JetStream data
// dir into it, and a shell-bearing base keeps a live `podman exec` diagnosis of
// a wedged store possible without swapping the image.
//
// The pin is by DIGEST, not a mutable tag, for the same reason
// DefaultCollectorImage and DefaultPostgresImage pin theirs: NATS is a
// supervised component of the installed stack holding the fabric's durable
// JetStream state, and both its config grammar and its on-disk store layout are
// version-sensitive, so a mutable tag would ship an unreviewed server — and an
// unreviewed store-format change — under the installed stack. This is a Go const
// Renovate cannot see, so it moves only via a reviewed manual PR.
//
// Bump procedure: advance the digest below, then re-run the NATS component
// bring-up against the new digest before landing (up -> GET /healthz on the
// monitor port answers 200 -> the JetStream block in /varz reports the rendered
// store_dir and a sync_interval of 100ms -> fresh-process down -> container
// gone), and re-validate the generated config against the new image
// (`nats-server -t -c <generated>`), since the config grammar and JetStream
// defaults can drift between server versions.
// The current image also boots with a 0600 config; 0644 is retained only for
// future non-root image variants, so the mode is not load-bearing today.
//
// Resolution provenance: this is the multi-arch image index digest for
// nats:2.14.6-alpine, resolved from the Docker Hub registry
// (docker-content-digest for the OCI image-index media type) and verified to
// pull and boot in-environment on 2026-09-04 — the booted server reported
// version 2.14.6 with JetStream enabled at sync_interval 100ms and answered
// /healthz 200.
const DefaultNatsImage = "docker.io/library/nats@sha256:ad7a43eb7e3337c3c38ce5d784d1461791f95f730f252d2b25eee699752a0ca3"
