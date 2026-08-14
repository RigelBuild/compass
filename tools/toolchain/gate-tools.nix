# The nixpkgs half of the toolchain, resolved at exactly the revision
# `devenv.lock` pins — so CI gets the identical derivations the dev shell does,
# and the parity gate has a concrete expected identity to compare against.
#
# `attrs` is passed in by tools/toolchain/parity.ts, which reads it out of
# devenv.nix's `packages = with pkgs; [ … ]` list. It is never hand-listed here:
# adding a tool to the dev shell must extend CI and the gate with no edit to
# this file or to the workflow.
#
# Three outputs, one per consumer:
#
#   env      a single symlink tree of every nixpkgs tool; CI prepends its bin/
#            to PATH. This is how CI obtains buf/golangci-lint/biome/… — there
#            is no `setup-*` action for them that could match a nixpkgs pin, and
#            guessing a close-enough version is exactly the drift the gate
#            exists to reject.
#   identity attr -> { version, store, bins }. `store` is the derivation path
#            the parity gate compares a resolved binary against; `bins` tells it
#            which binaries to probe, since the attribute name is often not the
#            command name (protobuf ships protoc, buf ships the protoc-gen-buf-*
#            plugins). Deriving that from the built tree keeps a name mapping
#            from having to be maintained by hand. The field is `store`, not
#            `outPath`: nix string-coerces any attrset carrying an `outPath`, so
#            naming it that collapses each entry to a bare path string.
#   langs    name -> { version, store, bins } for the language toolchains
#            (bun/node/moon/go) — the closed set the dev shell appends outside
#            its parsed `packages` literal. Same identity shape as `identity`,
#            so the parity gate checks it with the identical store-path method.
#            bun/node/moon come from toolchain-tools.nix (the module the dev
#            shell also imports); go comes from the go-overlay overlay applied
#            to this same nixpkgs. `langs` never consumes `attrs` — the language
#            set is closed, so it builds with no `--arg attrs`, which is why the
#            head below gives `attrs` a default.
{ attrs ? [ ] }:
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };

  # go-overlay applied to the same devenv.lock-pinned nixpkgs the dev shell
  # resolves go against, at the devenv.lock-pinned go-overlay rev — so CI and
  # the dev shell build one go derivation. The overlay's root default.nix
  # exposes `go-bin`, whose `versions` set is keyed by the raw version string
  # (e.g. "1.26.6") — NOT the dots→underscores `go_1_26_6` flake attribute the
  # dev shell selects. The two selectors are asymmetric on purpose: the overlay
  # sets no top-level `go_1_26_6`, so mirroring the flake key here would be an
  # eval failure on a nonexistent attribute. Both resolve one derivation
  # because both build over this nixpkgs (devenv.yaml pins go-overlay's nixpkgs
  # to follow the shell's) at one go-overlay rev.
  goOverlayNode = lock.nodes.go-overlay.locked;
  goOverlaySrc = builtins.fetchTarball {
    url = "https://github.com/${goOverlayNode.owner}/${goOverlayNode.repo}/archive/${goOverlayNode.rev}.tar.gz";
    sha256 = goOverlayNode.narHash;
  };
  goPin = import ./versions/go.nix;
  pkgsWithGo = import nixpkgsSrc { overlays = [ (import goOverlaySrc) ]; };
  goToolchain = pkgsWithGo.go-bin.versions.${goPin.version};

  toolchainTools = import ./toolchain-tools.nix { inherit pkgs; };

  # Command names a derivation exposes. Dot-prefixed entries are nix wrapper
  # internals (.go-licenses-wrapped), never on PATH as commands.
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
  };
}
