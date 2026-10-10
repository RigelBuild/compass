// Package appconfig is the native app's config parser (design §A1). One file —
// $XDG_CONFIG_HOME/compass/app.toml (fallback ~/.config/compass/app.toml) —
// selects the app's operating mode and, in client mode, its connection.
//
// The app is dual-mode:
//   - embedded: a present file with absent, empty, or embedded mode selects
//     local-stack supervision. server_url and ca_cert must be absent.
//   - client: the app dials a remote compass-server. It requires a server_url
//     that is an absolute https origin and may carry an optional ca_cert trust
//     anchor.
//
// An explicit --mode/$COMPASS_APP_MODE override, resolved by the caller and
// passed to Load, wins over the file. With neither override nor file, Load
// returns ErrNoConfig and the native app opens the first-run chooser. The app
// writes app.toml once, after setup succeeds, and never replaces an existing file.
//
// The core is pure: Parse decodes and validates a TOML byte slice with no I/O,
// Load takes configHome and home parameters instead of reading environment state,
// which keeps path resolution testable without changing the process environment.
//
// The config file carries neither the bearer token (entered once and stored in
// the OS keychain, DL-109) nor the caller account id (resolved by the WhoAmI
// RPC, DL-111); neither ever lives on disk here.
package appconfig
