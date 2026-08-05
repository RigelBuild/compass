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
	github.com/BurntSushi/toml v1.6.0
	github.com/cachix/secretspec/secretspec-go v0.17.1
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.10.0
	github.com/minio/minio-go/v7 v7.2.1
	github.com/rs/cors v1.11.1
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/spf13/viper v1.21.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/sync v0.22.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/ebitengine/purego v0.8.2 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/pelletier/go-toml/v2 v2.3.1 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	gopkg.in/ini.v1 v1.67.2 // indirect
)
