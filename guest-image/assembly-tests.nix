# Fixture tests for assemble-layers.sh: tiny synthetic OCI layers built in the
# sandbox, so the packer logic is checked without a registry or a KVM boot.
{ pkgs, script }:
pkgs.runCommand "compass-guest-assembly-tests" { } ''
  set -euo pipefail
  assemble() { bash ${script} "$@"; }

  # A layer is a deterministic gzip tar of an explicit member list, so a test
  # can ship a file without its parent directory entry, as real layers do.
  mklayer() {
    local out=$1 dir=$2
    shift 2
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
      --no-recursion -C "$dir" -cf - "$@" | gzip -n > "$out"
  }

  fail() { echo "FAIL: $*" >&2; exit 1; }

  # expect_break MESSAGE CMD... — the command must fail and name MESSAGE.
  expect_break() {
    local want=$1
    shift
    if "$@" 2> err.txt; then fail "expected BUILD-BREAK ($want), command succeeded"; fi
    grep -qF "$want" err.txt || { cat err.txt >&2; fail "stderr lacks: $want"; }
  }

  # (a) whiteout-then-re-add across layers.
  mkdir -p a1/etc a2/etc a3/etc
  echo one > a1/etc/a; echo keep > a1/etc/b
  touch a2/etc/.wh.a
  echo three > a3/etc/a
  mklayer a1.tgz a1 etc etc/a etc/b
  mklayer a2.tgz a2 etc/.wh.a
  mklayer a3.tgz a3 etc/a
  r=$(mktemp -d); assemble unpack "$r" a1.tgz a2.tgz
  [ ! -e "$r/etc/a" ] || fail "(a) whiteout left etc/a in place"
  [ -f "$r/etc/b" ] || fail "(a) whiteout removed a sibling"
  r=$(mktemp -d); assemble unpack "$r" a1.tgz a2.tgz a3.tgz
  [ "$(cat "$r/etc/a")" = three ] || fail "(a) re-added etc/a is not the upper layer's"

  # (b) an opaque directory whose own layer adds siblings.
  mkdir -p b1/opt b2/opt
  echo old > b1/opt/old
  touch b2/opt/.wh..wh..opq; echo new > b2/opt/new
  mklayer b1.tgz b1 opt opt/old
  mklayer b2.tgz b2 opt/.wh..wh..opq opt/new
  r=$(mktemp -d); assemble unpack "$r" b1.tgz b2.tgz
  [ ! -e "$r/opt/old" ] || fail "(b) opaque marker kept a lower-layer entry"
  [ -f "$r/opt/new" ] || fail "(b) opaque marker deleted its own layer's sibling"

  # (c) a layer replacing a lower-layer symlink parent with a real directory.
  mkdir -p c1/usr/lib c2/lib
  ln -s usr/lib c1/lib
  echo real > c2/lib/real
  mklayer c1.tgz c1 usr usr/lib lib
  mklayer c2.tgz c2 lib/real
  r=$(mktemp -d); assemble unpack "$r" c1.tgz c2.tgz
  [ -d "$r/lib" ] && [ ! -L "$r/lib" ] || fail "(c) lib is still a symlink"
  [ -f "$r/lib/real" ] || fail "(c) lib/real missing"
  [ ! -e "$r/usr/lib/real" ] || fail "(c) wrote through the stale symlink"

  # (d) the userland contract: a resolvable chain passes, a missing sh fails.
  mkdir -p d1/usr/bin
  printf '#!/bin/sh\n' > d1/usr/bin/sh; chmod 0755 d1/usr/bin/sh
  ln -s usr/bin d1/bin
  mklayer d1.tgz d1 usr usr/bin usr/bin/sh bin
  r=$(mktemp -d); assemble unpack "$r" d1.tgz
  assemble check-contract "$r" fixture@sha256:0 bin/sh || fail "(d) bin/sh via bin -> usr/bin did not resolve"
  expect_break "/bin/nft is not an executable" assemble check-contract "$r" fixture@sha256:0 bin/sh bin/nft

  # An absolute intermediate link must resolve inside the image, as the real
  # /bin -> /nix/store/... does; resolving it on the builder would pass wrongly.
  store=/nix/store/00000000000000000000000000000000-fixture
  mkdir -p "d2$store/bin"
  printf '#!/bin/sh\n' > "d2$store/bin/sh"; chmod 0755 "d2$store/bin/sh"
  ln -s "$store/bin" d2/bin
  mklayer d2.tgz d2 nix nix/store "''${store#/}" "''${store#/}/bin" "''${store#/}/bin/sh" bin
  r=$(mktemp -d); assemble unpack "$r" d2.tgz
  assemble check-contract "$r" fixture@sha256:0 bin/sh || fail "(d) bin/sh via an in-image store link did not resolve"
  rm "$r$store/bin/sh"
  expect_break "/bin/sh is not an executable" assemble check-contract "$r" fixture@sha256:0 bin/sh

  # (e) the header allowlist.
  mkdir e1; mkfifo e1/pipe
  mklayer e1.tgz e1 pipe
  expect_break "unsupported entry type" assemble unpack "$(mktemp -d)" e1.tgz

  mkdir -p e2/sub; ln -s ../../etc/passwd e2/sub/esc
  mklayer e2.tgz e2 sub sub/esc
  expect_break "symlink climbs out of the archive root" assemble unpack "$(mktemp -d)" e2.tgz

  mkdir e3; ln -s /etc/passwd e3/host
  mklayer e3.tgz e3 host
  expect_break "symlink to a host path" assemble unpack "$(mktemp -d)" e3.tgz

  # -P keeps the absolute target tar would otherwise strip, so the archive
  # really carries a hardlink pointing outside itself.
  mkdir e4; echo x > e4/a; ln e4/a e4/hl
  tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --no-recursion \
    -P --transform='s|^a$|/etc/passwd|' -C e4 -cf - a hl | gzip -n > e4.tgz

  # A store symlink is what the real image ships, so it must pass.
  mkdir e5; ln -s /nix/store/00000000000000000000000000000000-x/bin/sh e5/sh
  mklayer e5.tgz e5 sh
  assemble unpack "$(mktemp -d)" e5.tgz || fail "(e) a /nix/store symlink was rejected"

  touch $out
''
