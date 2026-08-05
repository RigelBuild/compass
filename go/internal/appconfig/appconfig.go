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

// Mode is the native app's operating mode, selected by app.toml (design §A4).
type Mode int

const (
	// ModeEmbedded runs the private embedded stack in-process — the zero-config
	// default: an absent file or mode="embedded" resolves here, so first launch
	// of the installed app just works.
	ModeEmbedded Mode = iota
	// ModeClient connects to a remote compass-server over its authenticated
	// loopback/network door; it requires a ServerURL and may carry a CACert.
	ModeClient
)

// The canonical mode strings as written in app.toml and the --mode/
// COMPASS_APP_MODE override — the single source of truth shared by String,
// Parse, and applyOverride.
const (
	modeStrEmbedded = "embedded"
	modeStrClient   = "client"
)

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
	// Empty means use the system roots.
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
// no I/O. The rules (design §A4):
//   - absent/empty mode or mode="embedded" → ModeEmbedded (the zero-config
//     default);
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
		return Config{Mode: ModeEmbedded}, nil
	case modeStrClient:
		return parseClient(fc)
	default:
		return Config{}, fmt.Errorf(
			"appconfig: unknown mode %q in app.toml: valid modes are %q and %q",
			fc.Mode, ModeEmbedded, ModeClient)
	}
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
// default (first launch just works). A present file is read and Parsed.
//
// override is the resolved --mode/COMPASS_APP_MODE value (empty = none). It is
// applied AFTER the file parse and wins: precedence is override > file >
// embedded-default. An override with no file present still works.
func Load(configHome, home string, override string) (Config, error) {
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
		// Absent file → embedded zero-config default; not an error.
	default:
		return Config{}, fmt.Errorf("appconfig: reading %s: %w", path, readErr)
	}

	return applyOverride(cfg, override)
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

// applyOverride applies the resolved --mode/COMPASS_APP_MODE override on top of
// the file-derived config. An empty override is a no-op. An override to embedded
// clears client-only fields; an override to client keeps whatever server_url and
// ca_cert the file supplied, then re-validates them so an override into client
// mode without a usable server_url fails legibly.
func applyOverride(cfg Config, override string) (Config, error) {
	switch strings.TrimSpace(override) {
	case "":
		return cfg, nil
	case modeStrEmbedded:
		return Config{Mode: ModeEmbedded}, nil
	case modeStrClient:
		client := fileConfig{ServerURL: cfg.ServerURL, CACert: cfg.CACert}
		out, err := parseClient(client)
		if err != nil {
			return Config{}, err
		}
		return out, nil
	default:
		return Config{}, fmt.Errorf(
			"appconfig: unknown mode override %q: valid modes are %q and %q",
			override, ModeEmbedded, ModeClient)
	}
}
