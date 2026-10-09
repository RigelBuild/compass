# The Go analysis battery pins. golangci-lint/govulncheck/go-licenses/nilaway
# link go/types + x/tools, so each must be BUILT with the same Go toolchain the
# code compiles with — a go1.26-built analyzer fails on a go1.27 stdlib. They are
# rebuilt with the go-overlay toolchain (see ../go-analysis.nix).
#
# Rebuilding is necessary but not sufficient: an analyzer also needs a RELEASE new
# enough for the newer language/IR, so two carry a source override past the
# nixpkgs pin — nilaway (go1.27.2 export data v5 needs x/tools v0.50.0; pinned to
# that) and golangci-lint (nixpkgs' bundled staticcheck panics on go1.27 IR;
# 2.14.0 reads go1.27.2 export data). govulncheck/go-licenses need none.
#
# Renovate manages nilaway and golangci-lint. refresh-go-analysis-hashes.ts updates
# their derived version/tag fields and both source and vendor hashes after a bump.
{
  nilaway = {
    version = "0-unstable-2026-09-18";
    owner = "uber-go";
    repo = "nilaway";
    rev = "acb8859b9031bb9496be97e027df5573f9fb5340";
    hash = "sha256-GvDZ5tlvOrTI93tYcIcLd45ZHdqwFopVtoBffD/kbuM=";
    vendorHash = "sha256-qVmvDneq6V/q5UHZ/Cjjqd5/XPPNfvVGoxwg9nz4/Ds=";
  };
  golangci-lint = {
    version = "2.14.0";
    owner = "golangci";
    repo = "golangci-lint";
    tag = "v2.14.0";
    hash = "sha256-HATA7JKHwEouM+8jYZbQrkX7p4gut4IpyTvcBexu/4o=";
    vendorHash = "sha256-ekP/zDhYMpMG+tYAyYHNfLOCt/JkxrXUw1jHUEfsM8k=";
  };
}
