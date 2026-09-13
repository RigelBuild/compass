# The Go analysis battery (golangci-lint, govulncheck, go-licenses, nilaway),
# each rebuilt with the go-overlay toolchain the code compiles against so the
# analyzer and the compiler stay on one Go version. The dev shell and the CI
# gate both import this module with the same `goToolchain`, so the two resolve
# one store path per tool.
#
# Why not the bare nixpkgs attrs: they are go1.26-built and fail under go1.27
# with `file requires newer Go version`. Two also need a source past the nixpkgs
# pin (nilaway's x/tools, golangci-lint's staticcheck) — a `src`/`vendorHash`
# override in versions/go-analysis.nix. go-licenses additionally needs the
# toolchain in its own `go` arg (it wraps GOROOT).
{ pkgs, goToolchain }:
let
  pins = import ./versions/go-analysis.nix;

  # buildGoModule with the go toolchain swapped. buildGoModule reads `go` from
  # its own scope, so it is swapped by overriding the builder — a `go` arg on the
  # package itself is silently ignored.
  buildGoModule' = pkgs.buildGoModule.override { go = goToolchain; };

  # Rebuild a nixpkgs Go package with the go1.27 builder, optionally pinning a
  # newer upstream source. `builderArg` names the buildGo*Module argument the
  # package takes (they differ). `passGo` also threads the toolchain through the
  # package's OWN `go` arg, needed by a package that wraps its binary with
  # `--set GOROOT` (go-licenses) — else the wrapper points at go1.26 GOROOT while
  # the binary is go1.27. `pin` fully replaces `src`, so a package deriving attrs
  # from `version` needs checking when pinned.
  rebuild =
    { pkg, builderArg, pin ? null, passGo ? false }:
    assert pin == null || (pin ? hash && (pin ? tag || pin ? rev));
    let
      base = pkg.override (
        { ${builderArg} = buildGoModule'; }
        // pkgs.lib.optionalAttrs passGo { go = goToolchain; }
      );
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
    # go-licenses wraps its binary with `--set GOROOT '${go}/share/go'`, so the
    # toolchain must reach its own `go` arg too — else it classifies a go1.27
    # build's stdlib against a go1.26 GOROOT.
    passGo = true;
  };
  nilaway = rebuild {
    pkg = pkgs.nilaway;
    builderArg = "buildGoModule";
    pin = pins.nilaway;
  };
}
