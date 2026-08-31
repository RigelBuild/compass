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
// Bump procedure: a DIGEST bump within postgres:18 (an upstream security rebuild,
// same major) is automated — Renovate surfaces this const as a docker dep via a
// customManager in tools/renovate/config.json5 (depName `postgres-stack`) and
// opens a reviewable PR to advance the digest. A MAJOR bump (18 -> 19) is frozen
// by DL-260 for on-disk-format stability and stays manual: the customManager's
// packageRule pins allowedVersions to /^18$/, so 18 -> 19 is never auto-proposed.
// When you do move the major deliberately, re-run the T8 container integration
// test (up -> probe DSN -> fresh-process down -> container gone) against the new
// digest before landing, and keep the major aligned with pgtest.go's pin
// discipline so a dev-box stack and an installed stack never skew on-disk format.
const DefaultPostgresImage = "docker.io/library/postgres:18@sha256:1957b2ff3137e4ef7f3bc813e74fff50b1e1ffddc85c8b9d6f14ade972be8687"
