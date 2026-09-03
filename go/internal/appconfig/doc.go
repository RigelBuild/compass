// Package appconfig is the native app's config parser (design §A1). One file —
// $XDG_CONFIG_HOME/compass/app.toml (fallback ~/.config/compass/app.toml) —
// selects the app's operating mode and, in client mode, its connection.
//
// The app is dual-mode:
//   - embedded (the zero-config onboarding default): the app supervises a
//     private local stack in-process. An absent file, an empty mode, or
//     mode="embedded" resolves here, so a first launch just works. server_url
//     and ca_cert are client-only fields and must be absent in embedded mode.
//   - client: the app dials a remote compass-server. It requires a server_url
//     that is an absolute https URL (cleartext is refused) and may carry an
//     optional ca_cert private trust anchor.
//
// A --mode/$COMPASS_APP_MODE override, resolved by the caller and passed to
// Load, wins over the file: precedence is override > file > embedded-default.
//
// The core is pure: Parse decodes and validates a TOML byte slice with no I/O,
// and Load layers path resolution and the override on top. Mirroring the stack
// package idiom, the caller resolves every path — Load takes configHome and home
// as parameters (the caller reads the env), so the resolution is fully
// unit-testable without touching real $HOME/$XDG_CONFIG_HOME.
//
// The config file carries neither the bearer token (entered once and stored in
// the OS keychain, DL-109) nor the caller account id (resolved by the WhoAmI
// RPC, DL-111); neither ever lives on disk here.
package appconfig
