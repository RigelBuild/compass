# bundle-env.nix — the nix inputs the compass-app bundle build realizes, over
# and above the shell's cgo link environment.
#
# The gtk3 shell link needs the SAME already-pinned `pkgConfig` + `cc` outputs
# the M4 e2e gate realizes (tools/toolchain/gtk-e2e-env.nix), so this file does
# NOT re-pin a third copy of the devenv.lock boilerplate for them — build.sh
# realizes those two directly off gtk-e2e-env.nix. This file carries ONLY the
# bundle's delta: the postgres tooling the private Postgres sidecar shells out
# to by bare name (go/cmd/compass-postgres/main.go: exec.LookPath of
# postgres/initdb/createdb), which the bundle stages as store symlinks in bin/.
#
# The `postgresql` attr is the devenv.lock-pinned BARE `postgresql` — the same
# derivation devenv.nix:99-113 puts on the dev/CI PATH ("Bare `postgresql`, not
# a version-suffixed attr, for strict parity", postgresql-18.4 at this pin), so
# the bundle ships one postgres, byte-identical to the one the e2e suite runs.
#
# nixpkgs is re-pinned to the devenv.lock revision exactly as
# tools/toolchain/gtk-e2e-env.nix:28-38 does, so this delta cannot drift from
# the shell env or the dev shell.
let
  lock = builtins.fromJSON (builtins.readFile ../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
in
{
  # The bare, devenv.lock-pinned postgresql (18.4 at this pin) — provides
  # postgres/initdb/createdb in its bin/, staged into the bundle as store
  # symlinks.
  postgresql = pkgs.postgresql;
}
