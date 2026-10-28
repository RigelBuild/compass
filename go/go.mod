// The Compass backend Go module.
// One module for the whole backend: the compass-server binary (served on
// server.sock), the comms packages, and the generated compass.v1 stubs (gen/).
//
// Module path is the repo's public GitHub path: github.com/RigelBuild/compass
// with the module rooted at go/. Imports resolve directly against the public
// repo (go-get meta for RigelBuild/compass), so the prefix must match it
// exactly or every import breaks.
//
// The `go` directive tracks the tools/toolchain/versions/go.nix pin minus at
// most one minor, so an upstream Go security patch never blocks on a mod edit
// (Global Constraint 1, floor policy).
module github.com/RigelBuild/compass/go

go 1.26.0

require (
	connectrpc.com/connect v1.21.0 // indirect
	connectrpc.com/cors v0.1.0
	connectrpc.com/otelconnect v0.12.0
	github.com/BurntSushi/toml v1.6.0
	github.com/go-jose/go-jose/v4 v4.1.5
	github.com/google/uuid v1.6.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/insomniacslk/dhcp v0.0.0-20260728151720-c308df0fdcef
	github.com/jackc/pgx/v5 v5.11.0
	github.com/mdlayher/vsock v1.3.0
	github.com/minio/minio-go/v7 v7.4.1
	github.com/nats-io/nats-server/v2 v2.15.1
	github.com/nats-io/nats.go v1.54.0
	github.com/rs/cors v1.11.1
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/spf13/viper v1.21.0
	github.com/vishvananda/netlink v1.3.1
	github.com/wailsapp/wails/v3 v3.0.0-beta.29
	github.com/zalando/go-keyring v0.2.8
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0
	golang.org/x/time v0.16.0
	google.golang.org/protobuf v1.36.12
)

require (
	connectrpc.com/connect/v2 v2.0.1
	connectrpc.com/grpchealth/v2 v2.0.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/antithesishq/antithesis-sdk-go v0.8.0-default-no-op // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mdlayher/packet v1.1.2 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/nats-io/jwt/v2 v2.8.2 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/u-root/uio v0.0.0-20230220225925-ffce2a382923 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.83.2 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
)

tool connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
