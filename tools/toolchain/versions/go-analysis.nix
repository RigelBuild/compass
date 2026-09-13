# The Go analysis battery pins. golangci-lint/govulncheck/go-licenses/nilaway
# link go/types + x/tools, so each must be BUILT with the same Go toolchain the
# code compiles with — a go1.26-built analyzer fails on a go1.27 stdlib. They are
# rebuilt with the go-overlay toolchain (see ../go-analysis.nix).
#
# Rebuilding is necessary but not sufficient: an analyzer also needs a RELEASE new
# enough for the newer language/IR, so two carry a source override past the
# nixpkgs pin — nilaway (nixpkgs' x/tools v0.31.0 can't parse go1.27; pinned to
# v0.45.0) and golangci-lint (nixpkgs' bundled staticcheck panics on go1.27 IR;
# 2.13.0 added support, pinned to 2.13.2). govulncheck/go-licenses need none.
#
# MANUALLY MAINTAINED — NOT yet Renovate-managed (tracked as a follow-up). Bump:
# move a tool's rev/tag + hash + vendorHash together and re-prefetch both. The FOD
# fails loudly on a wrong hash but not a stale-but-consistent pin, so check it on
# every Go bump (nilaway tracks an untagged main rev, most likely to rot).
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
