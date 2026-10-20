#!/usr/bin/env bash
# build.sh — builds the versioned Compass native-app release tarball (GTK shell,
# sidecars, pinned tools, UI); embedded postgres is the stock postgres:18 image.
# Bash: Nix/Go build-and-staging glue that runs the same in CI and locally.
# Paths are script-relative.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$SCRIPT_DIR"

log() { printf '>> %s\n' "$*" >&2; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

# Keep GC-root symlinks under app-bundle/ so GC cannot dangle the tarball rpaths.
# Reuse gtk-e2e-env.nix outputs. cgo execs pkg-config off PATH, not just
# PKG_CONFIG_PATH, so the pinned binary must be on PATH or a bare CI runner fails.
log "Realizing pinned nix outputs (bin, pkgConfig, cc)"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" bin \
  -o "$SCRIPT_DIR/result-bin"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" pkgConfig \
  -o "$SCRIPT_DIR/result-pkgconfig"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" cc.out \
  -o "$SCRIPT_DIR/result-cc"

BIN_ENV="$(readlink -f "$SCRIPT_DIR/result-bin")"
PC_ENV="$(readlink -f "$SCRIPT_DIR/result-pkgconfig")"
CC_ENV="$(readlink -f "$SCRIPT_DIR/result-cc")"

# Prepend, so the pinned pkg-config wins over any ambient one.
export PATH="$BIN_ENV/bin:$PATH"
PKG_CONFIG_PATH="$PC_ENV/lib/pkgconfig:$PC_ENV/share/pkgconfig"
CC_BIN="$CC_ENV/bin/cc"

# One version for every binary: dev builds use version.txt plus the git SHA;
# releases use COMPASS_BUNDLE_VERSION below.
# Keep cat separate: `set -e` checks only the last substitution in a composite
# assignment and can hide a missing version file.
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

# The four embedded sidecars (§A4/DL-321): pure-Go daemons the supervised stack
# resolves in-bundle via prependExecDirToPath, so they build WITHOUT the gtk4
# tag and without the CC/PKG_CONFIG closure the shell needs — same $v stamp.
# NO compass-postgres: embedded's postgres is a DL-260 container, not a sidecar.
for b in compass-stack compass-server compass-runner compass-clear-token; do
  log "Building sidecar ($b)"
  CGO_ENABLED=0 go -C "$GO_DIR" build -trimpath \
    -ldflags "-X main.version=$v" \
    -o "$STAGE/bin/$b" "./cmd/$b"
done

# secretspec: compass-server spawns it by name at boot, and the bundle bin/ is
# on the stack's PATH. The upstream release binary, which runs off-store.
log "Staging secretspec"
nix build -f "$REPO_ROOT/tools/toolchain/secretspec-env.nix" secretspecRelease \
  -o "$SCRIPT_DIR/result-secretspec"
install -m 755 "$SCRIPT_DIR/result-secretspec/bin/secretspec" "$STAGE/bin/secretspec"

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
# Icon=compass in the desktop entry resolves through the hicolor theme.
install -D -m 644 "$REPO_ROOT/apps/ui/src/assets/compass-needle.svg" \
  "$STAGE/share/icons/hicolor/scalable/apps/compass.svg"
cp "$REPO_ROOT/LICENSE" "$STAGE/LICENSE"

# --- 6. Sanity assertions (§256-261). A green build means a COMPLETE bundle.
log "Sanity: verifying staged bundle"
for b in compass-app compass-stack compass-server compass-runner compass-clear-token; do
  bin="$STAGE/bin/$b"
  if [[ ! -x "$bin" ]]; then
    err "sanity: missing/non-executable binary: bin/$b"
    exit 1
  fi
  # Version output formats differ, so assert the stamp is present rather than
  # matching the whole string.
  # Capture stderr and check status explicitly so --version failures get the
  # binary-specific diagnostic below despite `set -e`.
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

if ! got="$("$STAGE/bin/secretspec" --version 2>&1)"; then
  err "sanity: bin/secretspec --version exited non-zero: $got"
  exit 1
fi
log "  bin/secretspec --version = $got"

# Clear old tarballs: git-SHA names leave stale ones across dev builds, which can
# be mistaken for the artifact under test.
log "Creating tarball: $TARBALL"
rm -f "$SCRIPT_DIR"/compass-app-*-linux-amd64.tar.gz
tar -czf "$SCRIPT_DIR/$TARBALL" -C "$SCRIPT_DIR" "$BUNDLE"

log "Done: $TARBALL"
