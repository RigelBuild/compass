#!/usr/bin/env bash
# build.sh — build the versioned Compass native-app release tarball
# (compass-app-<version>-linux-amd64.tar.gz): the gtk4 shell (compass-app) + the
# three embedded sidecars (compass-stack, compass-server, compass-runner) + the
# UI dist + the desktop file + LICENSE, every binary stamped with the ONE
# version. No postgres tooling and no compass-postgres sidecar — the embedded
# stack's postgres is a stock postgres:18 container via rootless podman (§A4).
#
# Why bash: this is nix + go build orchestration glue — it realizes the pinned
# GTK cc/pkg-config closure with `nix build`, links the one gtk4 binary against
# it, and stages a directory tree. It carries no runtime deps beyond nix/go/tar,
# and runs byte-identically in CI and on the dev box.
#
# Run from app-bundle/ (moon's default project cwd). Paths are anchored to this
# script's own directory so it is cwd-independent.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$SCRIPT_DIR"

log() { printf '>> %s\n' "$*" >&2; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

# --- 1. Realize the pinned nix outputs, keeping GC-root symlinks under
#        app-bundle/ so nix-collect-garbage cannot dangle the tarball's rpaths.
#        A bare `nix build --no-link` creates NO GC root. Reuse the
#        already-pinned pkgConfig + cc outputs directly off gtk-e2e-env.nix (no
#        re-pinned copy) — the one gtk4 binary is rpathed against them.
log "Realizing pinned nix outputs (pkgConfig, cc)"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" pkgConfig \
  -o "$SCRIPT_DIR/result-pkgconfig"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" cc.out \
  -o "$SCRIPT_DIR/result-cc"

PC_ENV="$(readlink -f "$SCRIPT_DIR/result-pkgconfig")"
CC_ENV="$(readlink -f "$SCRIPT_DIR/result-cc")"

PKG_CONFIG_PATH="$PC_ENV/lib/pkgconfig:$PC_ENV/share/pkgconfig"
CC_BIN="$CC_ENV/bin/cc"

# --- 2. Version: one string, every binary (§196-205). Base from version.txt
#        (release-please's source of truth), +g<shortsha> dev/bundle suffix.
#        Read version.txt on its own line: in a composite assignment `set -e`
#        only inspects the LAST substitution, so folding the `cat` into the same
#        line as the `git` substitution would silently swallow a missing file
#        and leave an empty base.
if [[ -n "${COMPASS_BUNDLE_VERSION:-}" ]]; then
  # Release path (release.yml release-assets): the semver tag IS the identity
  # (Global Constraint 1), stamped clean like the daemons — no +g<sha> suffix,
  # no git rev-parse. Set by the release-assets job to the release version.
  v="$COMPASS_BUNDLE_VERSION"
else
  version_base="$(cat "$REPO_ROOT/version.txt")"
  [[ -n "$version_base" ]] || { err "version.txt missing or empty"; exit 1; }
  v="${version_base}+g$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
fi
log "Bundle version: $v"

BUNDLE="compass-app-$v-linux-amd64"
TARBALL="$BUNDLE.tar.gz"

# --- 3/4. Build the binaries into a temp bin dir off the go module root.
GO_DIR="$REPO_ROOT/go"
STAGE="$SCRIPT_DIR/$BUNDLE"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/share/applications"

# The gtk4 shell: CGO, linked against the pinned cc + pkg-config closure so the
# ELF is store-rpathed and self-contained (§446-447).
log "Building gtk4 shell (compass-app)"
CGO_ENABLED=1 CC="$CC_BIN" PKG_CONFIG_PATH="$PKG_CONFIG_PATH" \
  go -C "$GO_DIR" build -trimpath -tags gtk4 \
  -ldflags "-X main.version=$v" \
  -o "$STAGE/bin/compass-app" ./cmd/compass-app

# The three embedded sidecars (§A4/DL-321): pure-Go daemons the supervised stack
# resolves in-bundle via prependExecDirToPath, so they build WITHOUT the gtk4
# tag and without the CC/PKG_CONFIG closure the shell needs — same $v stamp.
# NO compass-postgres: embedded's postgres is a DL-260 container, not a sidecar.
for b in compass-stack compass-server compass-runner; do
  log "Building sidecar ($b)"
  CGO_ENABLED=0 go -C "$GO_DIR" build -trimpath \
    -ldflags "-X main.version=$v" \
    -o "$STAGE/bin/$b" "./cmd/$b"
done

# --- 5. Stage the rest of the layout — dist + desktop + LICENSE (§156-168).

# dist: the compass-ui:build output (apps/ui/dist), staged beside the shell.
UI_DIST="$REPO_ROOT/apps/ui/dist"
if [[ ! -f "$UI_DIST/index.html" ]]; then
  err "apps/ui/dist/index.html missing — run 'moon run compass-ui:build' first (the deps edge)"
  exit 1
fi
cp -R "$UI_DIST" "$STAGE/bin/dist"

# desktop template (inert as shipped, §466-469) + LICENSE (AGPL-3.0-only).
cp "$SCRIPT_DIR/compass.desktop" "$STAGE/share/applications/compass.desktop"
cp "$REPO_ROOT/LICENSE" "$STAGE/LICENSE"

# --- 6. Sanity assertions (§256-261). A green build means a COMPLETE bundle.
log "Sanity: verifying staged bundle"
for b in compass-app compass-stack compass-server compass-runner; do
  bin="$STAGE/bin/$b"
  if [[ ! -x "$bin" ]]; then
    err "sanity: missing/non-executable binary: bin/$b"
    exit 1
  fi
  # Each command's --version output must CONTAIN the stamp. The stamp value is
  # what the gate asserts (§257); the surrounding format is per-command and
  # already heterogeneous in the as-built tree (compass-server prints
  # "compass-server <v>", the others the bare value — A0 §77-84), so a
  # substring check is the faithful "prints the stamped value" test, not an
  # exact-string match that a correct bundle would fail.
  # Capture stdout+stderr and guard the exit explicitly: a binary that crashes
  # on --version would otherwise abort at this assignment under `set -e` before
  # the diagnostic below runs, losing the operator's clue to which binary broke.
  if ! got="$("$bin" --version 2>&1)"; then
    err "sanity: bin/$b --version exited non-zero: $got"
    exit 1
  fi
  if [[ "$got" != *"$v"* ]]; then
    err "sanity: bin/$b --version = '$got', expected to contain '$v'"
    exit 1
  fi
  log "  bin/$b --version = $got"
done

if [[ ! -f "$STAGE/bin/dist/index.html" ]]; then
  err "sanity: bin/dist/index.html missing"
  exit 1
fi
log "  bin/dist/index.html present"

# --- 7. Tar the bundle dir. Clear ALL prior release tarballs first, not just
# this version's: build.sh stamps the name with the git sha, so a dev box that
# builds across commits (or a reused moon cache dir) accumulates stale
# compass-app-*.tar.gz beside the fresh one. The manual SMOKE.md runbook globs
# the one tarball in this dir and expects exactly this build's artifact, so
# "one tarball after a build" is an invariant this step owns. CI's workspace is
# clean so this is a no-op there; it is the dev-box path that needs it.
log "Creating tarball: $TARBALL"
rm -f "$SCRIPT_DIR"/compass-app-*-linux-amd64.tar.gz
tar -czf "$SCRIPT_DIR/$TARBALL" -C "$SCRIPT_DIR" "$BUNDLE"

log "Done: $TARBALL"
