// The Compass backend Go module (docs/designs/platform/go-toolchain-default.md).
// One module for the whole backend: the compass-server binary (served on
// server.sock), the comms packages, and the generated compass.v1 stubs (gen/).
//
// Module path is the PUBLIC Copybara-destination path — oss/ is stripped when
// oss/compass/ mirrors out to github.com/sealedsecurity/compass, so the import
// prefix must be the destination or every import breaks on export
// (oss/README.md; the oss/seal -> github.com/sealedsecurity/seal precedent).
//
// The `go` directive tracks the .prototools pin (1.26.5) minus at most one
// minor, so an upstream Go security patch never blocks on a mod edit (Global
// Constraint 1, floor policy).
module github.com/sealedsecurity/compass/go

go 1.25.0

require (
	connectrpc.com/connect v1.20.0
	connectrpc.com/cors v0.1.0
	github.com/cachix/secretspec/secretspec-go v0.15.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.10.0
	github.com/rs/cors v1.11.1
	golang.org/x/sync v0.22.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ebitengine/purego v0.8.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/text v0.39.0 // indirect
)
