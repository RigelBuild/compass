#!/usr/bin/env bash
# build.sh — build the versioned Compass native-app release tarball
# (compass-app-<version>-linux-amd64.tar.gz), the A1/A2 bundle of the gtk3 shell
# + its four pure-Go sidecars + the pinned postgres tooling + the UI dist.
#
# Why bash: this is nix + go build orchestration glue — it realizes the pinned
# GTK cc/pkg-config closure and the postgres delta with `nix build`, links the
# five binaries against them, and stages a directory tree. It carries no runtime
# deps beyond nix/go/tar, and runs byte-identically in CI and on the dev box.
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
#        app-bundle/ so nix-collect-garbage cannot dangle the tarball's rpaths
#        or the postgres-tool bin/ symlinks (§435-444). A bare `nix build
#        --no-link` creates NO GC root. Reuse the already-pinned pkgConfig + cc
#        outputs directly off gtk-e2e-env.nix (no re-pinned copy); the
#        postgresql delta comes from bundle-env.nix.
log "Realizing pinned nix outputs (pkgConfig, cc, postgresql)"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" pkgConfig \
  -o "$SCRIPT_DIR/result-pkgconfig"
nix build -f "$REPO_ROOT/tools/toolchain/gtk-e2e-env.nix" cc.out \
  -o "$SCRIPT_DIR/result-cc"
nix build -f "$SCRIPT_DIR/bundle-env.nix" postgresql \
  -o "$SCRIPT_DIR/result-postgresql"

PC_ENV="$(readlink -f "$SCRIPT_DIR/result-pkgconfig")"
CC_ENV="$(readlink -f "$SCRIPT_DIR/result-cc")"
PG_ENV="$(readlink -f "$SCRIPT_DIR/result-postgresql")"

PKG_CONFIG_PATH="$PC_ENV/lib/pkgconfig:$PC_ENV/share/pkgconfig"
CC_BIN="$CC_ENV/bin/cc"

# --- 2. Version: one string, every binary (§196-205).
v="0.1.0+g$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
log "Bundle version: $v"

BUNDLE="compass-app-$v-linux-amd64"
TARBALL="$BUNDLE.tar.gz"

# --- 3/4. Build the binaries into a temp bin dir off the go module root.
GO_DIR="$REPO_ROOT/go"
STAGE="$SCRIPT_DIR/$BUNDLE"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/share/applications"

# The gtk3 shell: CGO, linked against the pinned cc + pkg-config closure so the
# ELF is store-rpathed and self-contained (§446-447).
log "Building gtk3 shell (compass-app)"
CGO_ENABLED=1 CC="$CC_BIN" PKG_CONFIG_PATH="$PKG_CONFIG_PATH" \
  go -C "$GO_DIR" build -trimpath -tags gtk3 \
  -ldflags "-X main.version=$v" \
  -o "$STAGE/bin/compass-app" ./cmd/compass-app

# The four pure-Go sidecars: CGO off (§448-449).
for cmd in compass-stack compass-server compass-runner compass-postgres; do
  log "Building sidecar ($cmd)"
  CGO_ENABLED=0 go -C "$GO_DIR" build -trimpath \
    -ldflags "-X main.version=$v" \
    -o "$STAGE/bin/$cmd" "./cmd/$cmd"
done

# --- 5. Stage the A2 layout (§156-168).
# postgres/initdb/createdb: store symlinks from the realized postgresql, so the
# bundle ships one pinned postgres 18.x (§164).
for tool in postgres initdb createdb; do
  ln -s "$PG_ENV/bin/$tool" "$STAGE/bin/$tool"
done

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
for b in compass-app compass-stack compass-server compass-runner compass-postgres; do
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
  got="$("$bin" --version)"
  if [[ "$got" != *"$v"* ]]; then
    err "sanity: bin/$b --version = '$got', expected to contain '$v'"
    exit 1
  fi
  log "  bin/$b --version = $got"
done

# postgres tools: present + executable, NOT stamp-matched (they carry
# PostgreSQL's own 18.x version).
for tool in postgres initdb createdb; do
  if [[ ! -x "$STAGE/bin/$tool" ]]; then
    err "sanity: missing/non-executable postgres tool: bin/$tool"
    exit 1
  fi
  log "  bin/$tool present + executable"
done

if [[ ! -f "$STAGE/bin/dist/index.html" ]]; then
  err "sanity: bin/dist/index.html missing"
  exit 1
fi
log "  bin/dist/index.html present"

# --- 7. Tar the bundle dir.
log "Creating tarball: $TARBALL"
rm -f "$SCRIPT_DIR/$TARBALL"
tar -czf "$SCRIPT_DIR/$TARBALL" -C "$SCRIPT_DIR" "$BUNDLE"

log "Done: $TARBALL"
