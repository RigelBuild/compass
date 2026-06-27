#!/usr/bin/env bash
# Build + publish the compass CI step image — the OCI image the Woodpecker
# agents run `moon run :ci` in. It is the dev shell (repo-root devenv.nix, the
# `ci` container) built as an image, so CI and local share one toolchain.
#
# The `ci` container sets copyToRoot = [ ]: it carries the toolchain but NOT the
# repo (Woodpecker checks the repo out per job), so no sources leak into a
# pullable image.
#
# MUST run on an x86_64-linux host with nix. `devenv container build` maps the
# host to its own linux arch, so an aarch64-darwin box can't produce the
# x86_64-linux image.
#
# This is an inherent bootstrap: the image *is* the CI toolchain, so a job
# running inside it can't build it. Re-run on devenv.{nix,yaml,lock} /
# .prototools / ci/ci-toolchain.nix changes (by hand, or a timer on the always-on
# linux agent).
#
# Auth: a GitHub token with packages:write on the ghcr.io namespace below. The
# token is written to a 0600 authfile and passed to skopeo via --dest-authfile,
# never on the command line (argv shows in the process list).
#   GHCR_TOKEN   required   the token
#   GHCR_USER    optional   token owner (default: sealedsecurity)
#
# Usage (on a linux agent):
#   GHCR_TOKEN=<token> bash ci/publish-ci-image.sh

set -euo pipefail

SYSTEM="x86_64-linux"
# devenv appends `<name>:<version>` (containers.ci -> compass-ci:latest) to this
# prefix, so it MUST end in a slash and carry no image/tag of its own.
REGISTRY="docker://ghcr.io/sealedsecurity/"
GHCR_USER="${GHCR_USER:-sealedsecurity}"

if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
	echo "Run on an x86_64-linux agent: devenv can't cross-build the linux image" >&2
	echo "from this host." >&2
	exit 1
fi
: "${GHCR_TOKEN:?set GHCR_TOKEN (a GitHub token with packages:write on ghcr.io/sealedsecurity)}"

# Repo root (devenv.nix lives there).
cd "$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"

echo "Building ghcr.io/sealedsecurity/compass-ci:latest (${SYSTEM}) from devenv.nix ..."
# Strip the push token from the build env so it can't be baked into the image
# config in the (persistent) nix store before the scan below runs. The scan is
# the backstop.
image_json="$(env -u GHCR_TOKEN devenv container build ci --system "$SYSTEM")"
# Fail closed if the build did not yield a single readable manifest path (e.g.
# extra stdout lines): otherwise the scan's grep would error and `|| true` would
# wave an uninspected image through to the push.
[ -f "$image_json" ] || {
	echo "error: container build did not yield a readable image manifest: '$image_json'" >&2
	exit 2
}

# Guard: `devenv container build` bakes devenv.nix's `env` into the image's OCI
# config (image-config.Env), so any secret reaching `env` would ship in this
# pullable image. Fail closed if the built image embeds a secret-shaped value.
echo "Scanning the built image config for baked secrets ..."
secret_shapes='cr-[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
leaked_keys="$(grep -oE "\"[A-Za-z_][A-Za-z0-9_]*=($secret_shapes)" "$image_json" | sed 's/=.*//; s/^"//' | sort -u || true)"
if [ -n "$leaked_keys" ]; then
	echo "error: the built image embeds secret-shaped values in env var(s):" >&2
	echo "  - ${leaked_keys//$'\n'/$'\n'  - }" >&2
	echo "  devenv container build bakes devenv.nix's \`env\` into the image —" >&2
	echo "  never set a secret there. Refusing to push." >&2
	exit 3
fi

# Credentials go in a private authfile, not argv. skopeo (which devenv's copy
# shells out to) reads --dest-authfile; the file is 0600 and removed on exit.
authfile="$(mktemp)"
trap 'rm -f "$authfile"' EXIT
chmod 600 "$authfile"
printf '{"auths":{"ghcr.io":{"auth":"%s"}}}' \
	"$(printf '%s:%s' "$GHCR_USER" "$GHCR_TOKEN" | base64 | tr -d '\n')" >"$authfile"

echo "Pushing ghcr.io/sealedsecurity/compass-ci:latest ..."
# devenv container copy pushes via skopeo (daemonless — no docker needed).
# --copy-args forwards skopeo flags; use --copy-args=<val> so the flag's leading
# dashes aren't parsed as a devenv option.
devenv container copy ci \
	--system "$SYSTEM" \
	--registry "$REGISTRY" \
	--copy-args="--dest-authfile=$authfile"

echo "Published ghcr.io/sealedsecurity/compass-ci:latest"
