package appconfig

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Mode is the native app's operating mode, selected by app.toml and an optional
// --mode/$COMPASS_APP_MODE override (design §A1). The app is dual-mode: it
// either supervises a local stack (embedded) or dials a remote one (client).
type Mode int

const (
	// ModeClient connects to a remote compass-server over its authenticated
	// loopback/network door; it requires a ServerURL and may carry a CACert.
	// It KEEPS the zero value so a client Config need not be spelled out.
	ModeClient Mode = iota
	// ModeEmbedded is the local-supervisor onboarding mode: the app brings up
	// and supervises a private stack in-process. It is the zero-config default
	// an absent app.toml (and an empty/absent mode) resolves to, so a first
	// launch of the installed app just works without any server_url. It is
	// declared AFTER ModeClient so ModeClient retains the zero value.
	ModeEmbedded
)

// modeStrClient is the canonical client mode string as written in app.toml and
// the --mode/$COMPASS_APP_MODE override — the single source of truth shared by
// String, Parse, and applyOverride.
const modeStrClient = "client"

// modeStrEmbedded is the canonical embedded mode string, shared by String,
// Parse, and applyOverride.
const modeStrEmbedded = "embedded"

// String renders the mode as it is written in app.toml (the TOML mode value),
// for logs and round-tripping.
func (m Mode) String() string {
	switch m {
	case ModeEmbedded:
		return modeStrEmbedded
	case ModeClient:
		return modeStrClient
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Config is the resolved app configuration. It carries neither the bearer token
// (OS keychain, DL-109) nor the caller account id (WhoAmI RPC, DL-111); neither
// lives in the config file.
type Config struct {
	// Mode is the resolved operating mode (embedded or client).
	Mode Mode
	// ServerURL is the native-client base URL (an absolute https URL). It is
	// required in client mode and empty in embedded mode.
	ServerURL string
	// CACert is an optional path to a private trust anchor (PEM) for a
	// native-client connection whose server presents a private-CA certificate.
	// Empty means use the system roots. Client-only; empty in embedded mode.
	CACert string
}

// fileConfig is the on-disk TOML shape. It is decoded and then validated into a
// Config; keeping it separate lets Parse distinguish an absent mode key from an
// explicit empty string only where that matters (both resolve to embedded).
type fileConfig struct {
	Mode      string `toml:"mode"`
	ServerURL string `toml:"server_url"`
	CACert    string `toml:"ca_cert"`
}

// Parse decodes and validates an app.toml byte slice into a Config. It performs
// no I/O. The rules (design §A1):
//   - absent/empty mode or mode="embedded" → ModeEmbedded (the zero-config
//     onboarding default). server_url and ca_cert are client-only fields, so a
//     non-empty value under embedded mode is a legible error;
//   - mode="client" requires a non-empty server_url that parses as an absolute
//     https URL (ca_cert is optional);
//   - any other mode value is an error naming the two valid modes.
func Parse(data []byte) (Config, error) {
	var fc fileConfig
	md, err := toml.Decode(string(data), &fc)
	if err != nil {
		return Config{}, fmt.Errorf("appconfig: parsing app.toml: %w", err)
	}
	// Reject unknown/typo'd keys rather than silently dropping them: a
	// mistyped ca_cert would otherwise vanish and the client would fall back
	// to system roots, surfacing later as an opaque TLS failure instead of a
	// legible config error.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("appconfig: unknown key(s) in app.toml: %v", undecoded)
	}

	switch strings.TrimSpace(fc.Mode) {
	case "", modeStrEmbedded:
		return parseEmbedded(fc)
	case modeStrClient:
		return parseClient(fc)
	default:
		return Config{}, fmt.Errorf(
			"appconfig: unknown mode %q in app.toml: valid modes are %q and %q",
			fc.Mode, modeStrEmbedded, modeStrClient)
	}
}

// parseEmbedded validates the embedded-mode fields. Embedded supervises a local
// stack, so server_url and ca_cert are client-only and must be absent: a value
// under embedded mode is almost certainly a misfiled client config, so it is
// rejected legibly rather than silently ignored.
func parseEmbedded(fc fileConfig) (Config, error) {
	if strings.TrimSpace(fc.ServerURL) != "" {
		return Config{}, errors.New(
			`appconfig: server_url is a client-only field and must not be set in embedded mode ` +
				`(embedded supervises a local stack); use mode="client" to dial a remote server_url`)
	}
	if strings.TrimSpace(fc.CACert) != "" {
		return Config{}, errors.New(
			`appconfig: ca_cert is a client-only field and must not be set in embedded mode ` +
				`(embedded supervises a local stack); use mode="client" to dial a remote server with a private CA`)
	}
	return Config{Mode: ModeEmbedded}, nil
}

// parseClient validates the client-mode fields.
func parseClient(fc fileConfig) (Config, error) {
	if strings.TrimSpace(fc.ServerURL) == "" {
		return Config{}, errors.New(
			`appconfig: mode="client" requires server_url in app.toml (e.g. server_url = "https://host:8443")`)
	}
	if err := validateServerURL(fc.ServerURL); err != nil {
		return Config{}, err
	}
	return Config{
		Mode:      ModeClient,
		ServerURL: fc.ServerURL,
		CACert:    fc.CACert,
	}, nil
}

// validateServerURL enforces that a client server_url is an absolute https URL.
// http and relative URLs are rejected: the Global Constraint forbids a cleartext
// network path.
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("appconfig: server_url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf(
			"appconfig: server_url %q must use https (got scheme %q): cleartext connections are not allowed",
			raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("appconfig: server_url %q must be absolute with a host (e.g. https://host:8443)", raw)
	}
	if u.User != nil {
		return fmt.Errorf(
			"appconfig: server_url %q must not embed credentials; the bearer token is entered in the connect screen and stored in the OS keychain (DL-109)",
			raw)
	}
	return nil
}

// Load resolves the app configuration from disk and applies an override.
//
// The config path is computed from the caller-provided paths (design §A4):
// configHome/compass/app.toml when configHome is non-empty (the resolved
// $XDG_CONFIG_HOME), else home/.config/compass/app.toml. The caller reads the
// env; Load performs the resolution so it stays testable.
//
// An absent file is not an error — it resolves to the ModeEmbedded zero-config
// onboarding default (first launch just works). A present file is read and
// Parsed.
//
// override is the resolved --mode/$COMPASS_APP_MODE value (empty = none). It is
// applied AFTER the file parse and wins: precedence is override (flag > env,
// resolved by the caller) > file > embedded-default (OQ-3). An override with no
// file present still works.
func Load(configHome, home, override string) (Config, error) {
	path, err := configPath(configHome, home)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{Mode: ModeEmbedded}
	// The path is the app's own config file, resolved from the caller's config
	// home — not attacker-controlled input.
	data, readErr := os.ReadFile(path) //nolint:gosec // G304: caller-resolved app config path, not user input
	switch {
	case readErr == nil:
		cfg, err = Parse(data)
		if err != nil {
			return Config{}, err
		}
	case errors.Is(readErr, os.ErrNotExist):
		// Absent file → embedded zero-config onboarding default; not an error.
	default:
		return Config{}, fmt.Errorf("appconfig: reading %s: %w", path, readErr)
	}

	return applyOverride(cfg, override)
}

// applyOverride applies the resolved --mode/$COMPASS_APP_MODE override on top of
// the file-derived config. An empty override is a no-op. An override to embedded
// clears the client-only fields; an override to client keeps whatever server_url
// and ca_cert the file supplied, then re-validates them so an override into
// client mode without a usable server_url fails legibly.
func applyOverride(cfg Config, override string) (Config, error) {
	switch strings.TrimSpace(override) {
	case "":
		return cfg, nil
	case modeStrEmbedded:
		return Config{Mode: ModeEmbedded}, nil
	case modeStrClient:
		return parseClient(fileConfig{ServerURL: cfg.ServerURL, CACert: cfg.CACert})
	default:
		return Config{}, fmt.Errorf(
			"appconfig: unknown mode override %q: valid modes are %q and %q",
			override, modeStrEmbedded, modeStrClient)
	}
}

// configPath computes the app.toml path from the caller-resolved config home and
// home directory: configHome/compass/app.toml, else home/.config/compass/app.toml.
func configPath(configHome, home string) (string, error) {
	if configHome != "" {
		return filepath.Join(configHome, "compass", "app.toml"), nil
	}
	if home != "" {
		return filepath.Join(home, ".config", "compass", "app.toml"), nil
	}
	return "", errors.New("appconfig: cannot resolve config path: both configHome and home are empty")
}
