# The nixpkgs half of the toolchain, resolved at exactly the revision
# `devenv.lock` pins — so CI gets the identical derivations the dev shell does,
# and the parity gate has a concrete expected identity to compare against.
#
# `attrs` is passed in by tools/toolchain/parity.ts, which reads it out of
# devenv.nix's `packages = with pkgs; [ … ]` list. It is never hand-listed here:
# adding a tool to the dev shell must extend CI and the gate with no edit to
# this file or to the workflow.
#
# Two outputs, one per consumer:
#
#   env      a single symlink tree of every tool; CI prepends its bin/ to PATH.
#            This is how CI obtains buf/golangci-lint/biome/… — there is no
#            `setup-*` action for them that could match a nixpkgs pin, and
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
{ attrs }:
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  pkgs = import
    (builtins.fetchTarball {
      url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
      sha256 = node.narHash;
    })
    { };

  # Command names a derivation exposes. Dot-prefixed entries are nix wrapper
  # internals (.go-licenses-wrapped), never on PATH as commands.
  binsOf = drv:
    let dir = "${drv}/bin";
    in
    if builtins.pathExists dir
    then builtins.filter (n: !(pkgs.lib.hasPrefix "." n)) (builtins.attrNames (builtins.readDir dir))
    else [ ];
in
{
  env = pkgs.buildEnv {
    name = "compass-gate-tools";
    paths = map (a: pkgs.${a}) attrs;
  };

  identity = builtins.listToAttrs (map
    (a: {
      name = a;
      value = {
        version = pkgs.${a}.version or "";
        store = pkgs.${a}.outPath;
        bins = binsOf pkgs.${a};
      };
    })
    attrs);
}
