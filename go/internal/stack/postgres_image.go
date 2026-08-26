//go:build unix

package stack

// DefaultPostgresImage is the container image the S4 container-backed postgres
// component (the T8 adapter) runs by default: the stock upstream postgres:18,
// pinned BY DIGEST. This is Matt's OQ-5 ruling (DL-260) — the bundled stock
// service, NOT a nix2container build of nixpkgs postgresql (that mechanism is
// for the NixOS+devenv agent shell, not a third-party service) and NOT a custom
// wrapper-entrypoint image. The official postgres entrypoint already does
// initdb-if-needed + createdb + a clean SIGTERM drain, so no wrapper is built.
//
// The pin is by DIGEST, not a mutable tag, for the same reason
// go/internal/pgtest/pgtest.go pins its CI image: postgres behavior the stack
// depends on (the store-of-record's on-disk format, and the same
// generated-tsvector / websearch_to_tsquery behavior the suites assert) is
// major/minor-version-sensitive, and a mutable tag would ship an unreviewed
// database under the installed stack.
//
// Bump procedure: advance the digest below when the postgres minor/major moves,
// then re-run the T8 container integration test (up -> probe DSN -> fresh-process
// down -> container gone) against the new digest before landing. Keep the major
// aligned with pgtest.go's pin discipline so a dev-box stack and an installed
// stack never skew on-disk format. This is a Go const Renovate cannot see (like
// pgtest.go's pgImage), so it moves only via a reviewed manual PR.
const DefaultPostgresImage = "docker.io/library/postgres:18@sha256:1957b2ff3137e4ef7f3bc813e74fff50b1e1ffddc85c8b9d6f14ade972be8687"
