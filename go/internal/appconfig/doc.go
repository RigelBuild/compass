// Package appconfig is the native app's client config parser (design §A3). One
// file — $XDG_CONFIG_HOME/compass/app.toml (fallback ~/.config/compass/app.toml)
// — configures the native-client connection: a base URL plus an optional
// private-anchor CA cert. The native app is a client.
//
// The core is pure: Parse decodes and validates a TOML byte slice with no I/O,
// and Load layers path resolution on top. Mirroring the stack package idiom, the
// caller resolves every path — Load takes configHome and home as parameters (the
// caller reads the env), so the resolution is fully unit-testable without
// touching real $HOME/$XDG_CONFIG_HOME.
//
// The config file carries neither the bearer token (entered once and stored in
// the OS keychain, DL-109) nor the caller account id (resolved by the WhoAmI
// RPC, DL-111); neither ever lives on disk here.
package appconfig
