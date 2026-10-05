// Package appconfig is the native app's config parser (design §A1). One file —
// $XDG_CONFIG_HOME/compass/app.toml (fallback ~/.config/compass/app.toml) —
// selects the app's operating mode and, in client mode, its connection.
//
// The app is dual-mode:
//   - embedded: the app supervises a private local stack in-process. A present
//     file with absent, empty, or embedded mode selects it; server_url and
//     ca_cert are client-only fields and must be absent in embedded mode.
//   - client: the app dials a remote compass-server. It requires a server_url
//     that is an absolute https origin and may carry an optional ca_cert trust
//     anchor.
//
// A --mode/$COMPASS_APP_MODE override, resolved by the caller and passed to
// Load, wins over the file. With neither, Load returns ErrNoConfig; the native
// app falls back to embedded until the first-run chooser is implemented.
//
// The core is pure: Parse decodes and validates a TOML byte slice with no I/O,
// Load takes configHome and home parameters instead of reading environment state,
// which keeps path resolution testable without changing the process environment.
//
// The config file carries neither the bearer token (entered once and stored in
// the OS keychain, DL-109) nor the caller account id (resolved by the WhoAmI
// RPC, DL-111); neither ever lives on disk here.
package appconfig
