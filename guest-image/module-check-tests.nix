{ pkgs, moduleConfigCheck }:
pkgs.runCommand "compass-guest-module-check-tests" { } ''
  set -euo pipefail
  fail() { echo "FAIL: $*" >&2; exit 1; }

  cat > all-m.config <<'CONFIG'
CONFIG_VIRTIO_PCI=m
CONFIG_VIRTIO_BLK=m
CONFIG_EROFS_FS=m
CONFIG_OVERLAY_FS=m
CONFIG_VIRTIO_NET=m
CONFIG_VIRTIO_FS=m
CONFIG_VIRTIO_VSOCKETS=m
CONFIG_PACKET=m
CONFIG
  cat > all-y.config <<'CONFIG'
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_BLK=y
CONFIG_EROFS_FS=y
CONFIG_OVERLAY_FS=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_FS=y
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_PACKET=y
CONFIG

  cat > check.sh <<'CHECK'
  ${moduleConfigCheck "\"$1\""}
CHECK

  sh check.sh all-m.config || fail "all eight =m symbols should pass"
  echo "PASS: all eight symbols set to =m"
  sh check.sh all-y.config || fail "all eight =y symbols should pass"
  echo "PASS: all eight symbols set to =y"

  grep -v '^CONFIG_PACKET=' all-m.config > absent.config
  sed 's/^CONFIG_PACKET=m/# CONFIG_PACKET is not set/' all-m.config > unset.config
  expect_break() {
    config=$1
    if sh check.sh "$config" >out.txt 2>err.txt; then
      fail "expected BUILD-BREAK for $config"
    fi
    grep -qF "BUILD-BREAK" err.txt || { cat err.txt >&2; fail "stderr lacks BUILD-BREAK for $config"; }
    echo "PASS: $config fails with BUILD-BREAK"
  }
  expect_break absent.config
  expect_break unset.config

  touch $out
''
