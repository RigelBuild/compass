# The nixpkgs half of the toolchain, resolved at exactly the revision
# `devenv.lock` pins — so CI gets the identical derivations the dev shell does,
# and the parity gate has a concrete expected identity to compare against.
#
# `attrs` is passed in by tools/toolchain/parity.ts, read out of devenv.nix's
# `packages = with pkgs; [ … ]` list, never hand-listed here: adding a dev-shell
# tool extends CI and the gate with no edit to this file.
#
# Three outputs, one per consumer:
#   env      a symlink tree of every nixpkgs tool; CI prepends its bin/ to PATH.
#            The only way CI obtains buf/golangci-lint/biome/… at the pinned
#            version — no `setup-*` action could match a nixpkgs pin.
#   identity attr -> { version, store, bins }. `store` is the derivation path the
#            parity gate compares against; `bins` names the binaries to probe
#            (the attr name often differs from the command). The field is `store`,
#            not `outPath`: nix string-coerces an attrset with `outPath`, which
#            would collapse each entry to a bare path string.
#   langs    name -> identity for the language toolchains (bun/node/moon/go), the
#            closed set appended outside the parsed `packages` literal. Never
#            consumes `attrs` (the set is closed), which is why the head defaults it.
{ attrs ? [ ] }:
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };

  # go-overlay applied to the same devenv.lock-pinned nixpkgs, at the pinned
  # go-overlay rev — so CI and the dev shell build one go derivation. The overlay
  # exposes `go-bin`, keyed by the raw version string ("1.26.6"), NOT the
  # dots→underscores `go_1_26_6` flake attr the dev shell selects — mirroring the
  # flake key here would eval-fail on a nonexistent attribute.
  goOverlayNode = lock.nodes.go-overlay.locked;
  goOverlaySrc = builtins.fetchTarball {
    url = "https://github.com/${goOverlayNode.owner}/${goOverlayNode.repo}/archive/${goOverlayNode.rev}.tar.gz";
    sha256 = goOverlayNode.narHash;
  };
  goPin = import ./versions/go.nix;
  pkgsWithGo = import nixpkgsSrc { overlays = [ (import goOverlaySrc) ]; };
  goToolchain = pkgsWithGo.go-bin.versions.${goPin.version};

  toolchainTools = import ./toolchain-tools.nix { inherit pkgs; };

  # The Go analysis battery, each rebuilt with the go-overlay toolchain the dev
  # shell uses, passed the same goToolchain so CI and the dev shell resolve one
  # store path per tool. Covered by the `langs` verdict, not the parsed attrs.
  goAnalysis = import ./go-analysis.nix { inherit pkgs goToolchain; };

  # Command names a derivation exposes. Dot-prefixed entries are nix wrapper
  # internals, never on PATH as commands.
  binsOf = drv:
    let dir = "${drv}/bin";
    in
    if builtins.pathExists dir
    then builtins.filter (n: !(pkgs.lib.hasPrefix "." n)) (builtins.attrNames (builtins.readDir dir))
    else [ ];

  identityOf = drv: {
    version = drv.version or "";
    store = drv.outPath;
    bins = binsOf drv;
  };
in
{
  env = pkgs.buildEnv {
    name = "compass-gate-tools";
    paths = map (a: pkgs.${a}) attrs;
  };

  identity = builtins.listToAttrs (map
    (a: {
      name = a;
      value = identityOf pkgs.${a};
    })
    attrs);

  langs = {
    bun = identityOf toolchainTools.bun;
    node = identityOf toolchainTools.node;
    moon = identityOf toolchainTools.moon;
    go = identityOf goToolchain;
    golangci-lint = identityOf goAnalysis.golangci-lint;
    govulncheck = identityOf goAnalysis.govulncheck;
    go-licenses = identityOf goAnalysis.go-licenses;
    nilaway = identityOf goAnalysis.nilaway;
  };
}
