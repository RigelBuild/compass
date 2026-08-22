#!/usr/bin/env bash
# publish.sh — build the compass-agent image spec once and push it to GHCR
# under each requested tag, enforcing :git-<sha> immutability.
#
# Why bash: this is nix-orchestration glue. It builds the compass-agent image
# through the vendored forks/devenv `devenv container build` (that half is not
# reversed yet) and drives the RigelBuild/nix2container fork's patched skopeo,
# which it invokes by name off PATH (see the SKOPEO= note below — skopeo is
# deliberately NOT in agent-image/devenv.nix; it comes from the root dev shell
# locally or the publish workflow's pinned-helper bootstrap in CI).
# agent-image/ is a standalone nix devenv with zero bun/TS infrastructure, and
# the publish must run byte-identically locally and in CI, which this thin bash
# glue over `nix`/`skopeo` does directly. Per AGENTS.md, a script that genuinely
# must be bash carries its rationale inline; this is it.

set -euo pipefail

# Run cwd-independent: the nix invocations use path:../forks/* relative to
# agent-image/, so anchor to this script's own directory rather than $PWD.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

IMAGE=ghcr.io/rigelbuild/compass-agent

# skopeo login (in CI) and the copies below run as SEPARATE `nix run`
# processes, so they must resolve the SAME credentials file. The default
# location ($XDG_RUNTIME_DIR/containers/auth.json) is environment-dependent —
# a mismatch greens login but 401s copy. Pin one explicit path both honor; CI
# exports this, and this default keeps local runs working.
: "${REGISTRY_AUTH_FILE:=${RUNNER_TEMP:-/tmp}/ghcr-auth.json}"
export REGISTRY_AUTH_FILE

# The fork's patched skopeo understands the `nix:` transport (reads a
# nix2container image spec directly); stock skopeo does not. The publish
# workflow puts it on PATH (resolved from the shared pinned helper
# tools/toolchain/skopeo-nix2container-env.nix, from the lockfile-pinned
# nix2container + nixpkgs revs — one source of truth, no raw flake ref), so it
# is a plain command here. It is NOT in agent-image/devenv.nix: a package there
# would bake skopeo's closure into the published image via the container
# entrypoint. Locally, `direnv`/`devenv shell` puts it on PATH from the root
# devenv.nix `packages` the same way.
SKOPEO=(skopeo)

log() { printf '>> %s\n' "$*" >&2; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

# Default tag set: immutable pin FIRST, then the moving tag, so :git-<sha>
# exists before :latest moves onto it. A GA add is one more positional arg
# (./publish.sh git-<sha> v<semver> latest).
if [[ $# -gt 0 ]]; then
  TAGS=("$@")
else
  SHA="$(git rev-parse --short=12 HEAD)"
  TAGS=("git-${SHA}" "latest")
fi

# Build the image spec exactly as dogfood:agent-image does — same fork-pinned
# derivation, so the published tag and the local load are copies of ONE build.
# devenv tracing goes to stderr; the spec store path is the last stdout line.
log "Building image spec: nix run path:../forks/devenv#devenv -- container build agent"
BUILD_OUT="$(nix run path:../forks/devenv#devenv -- container build agent)"
SPEC="$(printf '%s\n' "$BUILD_OUT" | tail -n 1)"

# A future fork bump that adds a trailing stdout line must not silently feed
# skopeo a non-path — assert the shape before trusting it.
if [[ "$SPEC" != /nix/store/* ]]; then
  err "expected a /nix/store spec path as the last stdout line, got: ${SPEC}"
  exit 1
fi
log "Image spec: ${SPEC}"

# Read the local spec's config digest ONCE. This is --raw + .config.digest
# (the rootfs-pinning identity that survives a copy verbatim), NOT plain
# inspect's .Digest (the re-serializable manifest digest).
LOCAL_DIGEST="$("${SKOPEO[@]}" inspect --raw "nix:$SPEC" | jq -r .config.digest)"
if [[ -z "$LOCAL_DIGEST" || "$LOCAL_DIGEST" == null ]]; then
  err "could not read local spec config digest from nix:${SPEC}"
  exit 1
fi
log "Local config digest: ${LOCAL_DIGEST}"

# Immutability guard for a :git-* tag. Returns 0 to proceed with the copy, 10
# to skip (idempotent re-run: remote already IS this artifact). Aborts non-zero
# on any ambiguous state — a transient registry error must NEVER be read as
# "tag absent, safe to overwrite".
guard_immutable() {
  local tag="$1" remote_raw remote_err remote_digest
  local dest="docker://${IMAGE}:${tag}"

  log "Checking immutable tag ${tag} on GHCR"
  # Capture stdout and stderr separately: stdout is the raw manifest, stderr
  # carries the failure classification we need.
  remote_err="$(mktemp)"
  if remote_raw="$("${SKOPEO[@]}" inspect --raw --authfile "$REGISTRY_AUTH_FILE" "$dest" 2>"$remote_err")"; then
    rm -f "$remote_err"
    remote_digest="$(printf '%s' "$remote_raw" | jq -r .config.digest)"
    if [[ "$remote_digest" == "$LOCAL_DIGEST" ]]; then
      log "Tag ${tag} already present with matching digest — idempotent, skipping copy"
      return 10
    fi
    err "tag ${tag} exists and is immutable: remote ${remote_digest} != local ${LOCAL_DIGEST}"
    return 1
  fi

  # inspect failed — classify. Only GHCR's authoritative "manifest unknown"
  # (the absent-tag response to an authenticated, write-scoped request) frees
  # the tag. A bare gateway/CDN 404 with no manifest token, and every auth /
  # 5xx / rate-limit error, is ambiguous and must abort — a transient must
  # NEVER be read as "tag absent, safe to overwrite".
  local msg
  msg="$(cat "$remote_err")"
  rm -f "$remote_err"
  if printf '%s' "$msg" | grep -qiE 'manifest unknown|manifest.*not.*found'; then
    log "Tag ${tag} absent on GHCR — free to publish"
    return 0
  fi
  err "inspect of ${dest} failed ambiguously (NOT a clean manifest-unknown); refusing to overwrite. Registry said:"
  printf '%s\n' "$msg" >&2
  return 1
}

for tag in "${TAGS[@]}"; do
  dest="docker://${IMAGE}:${tag}"

  if [[ "$tag" == git-* ]]; then
    set +e
    guard_immutable "$tag"
    rc=$?
    set -e
    case "$rc" in
      0) ;;               # tag free, proceed
      10) continue ;;     # idempotent no-op success
      *) exit "$rc" ;;    # ambiguous or immutability violation
    esac
  else
    log "Tag ${tag} is a moving tag — skipping immutability guard"
  fi

  log "Copying nix:${SPEC} -> ${dest}"
  # --insecure-policy matches the fork module's own copy invocation.
  "${SKOPEO[@]}" --insecure-policy copy "nix:$SPEC" "$dest" --authfile "$REGISTRY_AUTH_FILE"

  # Post-copy assert: re-inspect the PUSHED tag and confirm its config digest
  # equals the local spec's. Upgrades the guarantee from "skopeo exited 0" to
  # "the artifact I built is the artifact now at that tag", and detects a
  # racing overwrite in the check-then-copy window.
  pushed_digest="$("${SKOPEO[@]}" inspect --raw --authfile "$REGISTRY_AUTH_FILE" "$dest" | jq -r .config.digest)"
  if [[ "$pushed_digest" != "$LOCAL_DIGEST" ]]; then
    err "post-copy verification failed for ${tag}: pushed ${pushed_digest} != local ${LOCAL_DIGEST}"
    exit 1
  fi
  log "Published and verified ${dest} (${pushed_digest})"
done

log "Done."
