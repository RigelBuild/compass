# tools/toolchain/versions/go.nix — the go toolchain pin.
#
# Version only: the derivation comes from the go-overlay flake input
# (devenv.yaml / devenv.nix), which carries its own per-platform hashes in
# its manifest. Both the dev shell and the parity gate select go-overlay at
# this version so they resolve the same derivation.
#
# Floor policy: the `go` directive in go/go.mod tracks this pin minus at most
# one minor, so an upstream Go security patch never blocks on a mod edit.
{ version = "1.27.1"; }
