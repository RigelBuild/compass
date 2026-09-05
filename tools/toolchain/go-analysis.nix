# The Go analysis battery (golangci-lint, govulncheck, go-licenses, nilaway),
# each rebuilt with the go-overlay toolchain the code compiles against so the
# analyzer and the compiler stay on one Go version. Both the dev shell
# (devenv.nix) and the CI toolchain gate (tools/toolchain/gate-tools.nix) import
# this one module and pass the same `goToolchain`, so the two sides resolve one
# store path per tool; the store-path parity `langs` verdict checks CI against
# the dev shell.
#
# Why not the bare nixpkgs attrs: they are built with nixpkgs' go (go1.26 at
# this pin) and fail every run under go1.27 with `file requires newer Go version
# go1.27 (application built with go1.26)`. Two also need a release past the
# nixpkgs pin to understand go1.27 at all (nilaway's x/tools, golangci-lint's
# bundled staticcheck) — those carry a `src`/`vendorHash` override in
# versions/go-analysis.nix; govulncheck and go-licenses need only the rebuild.
{ pkgs, goToolchain }:
let
  pins = import ./versions/go-analysis.nix;

  # buildGoModule with the go toolchain swapped. buildGoModule reads `go` from
  # its own scope, so the toolchain is swapped by overriding the builder — a `go`
  # arg on the package itself is silently ignored.
  buildGoModule' = pkgs.buildGoModule.override { go = goToolchain; };

  # Rebuild a nixpkgs Go package with the go1.27 builder, optionally pinning a
  # newer upstream source. `builderArg` names the buildGo*Module argument the
  # package's package.nix takes (they differ: plain buildGoModule vs the
  # go-version-pinned buildGo126Module / buildGoLatestModule).
  rebuild =
    { pkg, builderArg, pin ? null }:
    let
      base = pkg.override { ${builderArg} = buildGoModule'; };
    in
    if pin == null then
      base
    else
      base.overrideAttrs (_old: {
        inherit (pin) version vendorHash;
        src = pkgs.fetchFromGitHub ({
          inherit (pin) owner repo hash;
        } // (if pin ? tag then { inherit (pin) tag; } else { inherit (pin) rev; }));
      });
in
{
  golangci-lint = rebuild {
    pkg = pkgs.golangci-lint;
    builderArg = "buildGo126Module";
    pin = pins.golangci-lint;
  };
  govulncheck = rebuild {
    pkg = pkgs.govulncheck;
    builderArg = "buildGoLatestModule";
  };
  go-licenses = rebuild {
    pkg = pkgs.go-licenses;
    builderArg = "buildGoModule";
  };
  nilaway = rebuild {
    pkg = pkgs.nilaway;
    builderArg = "buildGoModule";
    pin = pins.nilaway;
  };
}
