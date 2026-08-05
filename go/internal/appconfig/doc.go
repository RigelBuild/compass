// Package appconfig is the native app's mode-selection config parser (design
// §A4). One file — $XDG_CONFIG_HOME/compass/app.toml (fallback
// ~/.config/compass/app.toml) — owns the choice between embedded mode (the
// zero-config default: first launch of the installed app just works) and
// native-client mode (a base URL plus optional private-anchor CA cert).
//
// The core is pure: Parse decodes and validates a TOML byte slice with no I/O,
// and Load layers path resolution and the --mode/COMPASS_APP_MODE override on
// top. Mirroring the stack package idiom, the caller resolves every path — Load
// takes configHome and home as parameters (the caller reads the env), so the
// resolution is fully unit-testable without touching real $HOME/$XDG_CONFIG_HOME.
//
// The config file carries neither the bearer token (entered once and stored in
// the OS keychain, DL-109) nor the caller account id (resolved by the WhoAmI
// RPC, DL-111); neither ever lives on disk here.
package appconfig
