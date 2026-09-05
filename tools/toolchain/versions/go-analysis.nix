# tools/toolchain/versions/go-analysis.nix — the Go analysis battery pins.
#
# golangci-lint, govulncheck, go-licenses and nilaway are Go programs that link
# the go/types + golang.org/x/tools source-processing packages, so each must be
# BUILT with the same Go toolchain the code is COMPILED with (go.nix). A skew —
# a go1.26-built analyzer parsing a go1.27 stdlib — fails every run with
# `file requires newer Go version go1.27 (application built with go1.26)`. So
# these four are rebuilt with the go-overlay toolchain (tools/toolchain/
# go-analysis.nix) rather than taken as the bare go1.26-built nixpkgs attrs.
#
# Rebuilding with the newer compiler is necessary but not always sufficient: an
# analyzer also needs a RELEASE new enough to understand the newer language/IR.
# So two of the four carry a source override past the nixpkgs pin:
#
#   - nilaway: nixpkgs ships 0-unstable-2025-03-07 (x/tools v0.31.0), which
#     cannot parse go1.27 even when rebuilt. Pinned to an upstream rev carrying
#     x/tools v0.45.0.
#   - golangci-lint: nixpkgs ships 2.12.2, whose bundled staticcheck
#     (honnef.co/go/tools) panics on the go1.27 IR. 2.13.0 added go1.27 support
#     (#6642); pinned to 2.13.2.
#
# govulncheck and go-licenses need no source override — their nixpkgs pins carry
# an x/tools new enough for go1.27, so they omit a pin here and build the nixpkgs
# source with the overridden toolchain (go-licenses also gets the toolchain in
# its own `go` arg — see go-analysis.nix). Only the two below carry a pin.
#
# MANUALLY MAINTAINED — these pins are NOT yet Renovate-managed (the other
# versions/*.nix files each have a customManager in tools/renovate/config.json5;
# this file has none — tracked as a follow-up). Bump policy until then: move a
# tool's rev/tag + hash + vendorHash together and re-prefetch both hashes. The
# FOD derivations fail loudly on a WRONG hash, but never on a stale-but-consistent
# pin — this file is the pin most likely to rot (nilaway tracks an untagged main
# rev) and the least likely to be noticed, so check it on every Go bump.
{
  nilaway = {
    version = "0-unstable-2026-08-08";
    owner = "uber-go";
    repo = "nilaway";
    rev = "8649a03c818a94ba1e27c405843dad4753d85149";
    hash = "sha256-YCMQIxrfOtdV3UvtIVm9LsFJ8pV9pw7MHvTJpKRtl2Y=";
    vendorHash = "sha256-O6suySxR53G5agmbdqZ7z8QoBemLbSDLPTpyKTjo2WE=";
  };
  golangci-lint = {
    version = "2.13.2";
    owner = "golangci";
    repo = "golangci-lint";
    tag = "v2.13.2";
    hash = "sha256-RbWKPIG+UK82S9W9tp/CciZ669vudh95VOfHfdQWx3M=";
    vendorHash = "sha256-R83GeyfuZ+w30jZqFGYi0yua8E1Ey2q7/OlVmw8zDCg=";
  };
}
