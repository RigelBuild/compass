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

// Mode is the native app's operating mode. Client is the only mode — embedded
// mode was retired in RIG-2554 (the app no longer supervises a stack). Mode is
// kept as a validation-only concept: Load/Parse only ever yield ModeClient or a
// legible error, and the shell's launch dispatch keeps a default rejection arm.
type Mode int

const (
	// ModeClient connects to a remote compass-server over its authenticated
	// loopback/network door; it requires a ServerURL and may carry a CACert.
	ModeClient Mode = iota
)

// modeStrClient is the canonical client mode string as written in app.toml — the
// single source of truth shared by String and Parse.
const modeStrClient = "client"

// modeStrEmbedded is the retired mode value. It is NOT a supported mode: it
// parses to a legible rejection (see Parse) naming the retirement, not a
// compatibility arm.
const modeStrEmbedded = "embedded"

// String renders the mode as it is written in app.toml (the TOML mode value),
// for logs and round-tripping.
func (m Mode) String() string {
	switch m {
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
	// Mode is the resolved operating mode. Client is the only mode.
	Mode Mode
	// ServerURL is the native-client base URL (an absolute https URL). It is
	// always required (client is the only mode).
	ServerURL string
	// CACert is an optional path to a private trust anchor (PEM) for a
	// native-client connection whose server presents a private-CA certificate.
	// Empty means use the system roots.
	CACert string
}

// fileConfig is the on-disk TOML shape. It is decoded and then validated into a
// Config.
type fileConfig struct {
	Mode      string `toml:"mode"`
	ServerURL string `toml:"server_url"`
	CACert    string `toml:"ca_cert"`
}

// Parse decodes and validates an app.toml byte slice into a Config. It performs
// no I/O. The rules (design §A3, client-only):
//   - absent/empty mode or mode="client" → client mode, which REQUIRES a
//     non-empty server_url that parses as an absolute https URL (ca_cert is
//     optional);
//   - mode="embedded" → a legible rejection naming the retirement (embedded mode
//     was retired; run a headless stack with `compass-stack up` and point the
//     app at it in client mode). This is an error string, NOT a compatibility
//     arm (Global Constraint 5's sanctioned residue).
//   - any other mode value is an error naming the one valid mode.
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
	case "", modeStrClient:
		return parseClient(fc)
	case modeStrEmbedded:
		return Config{}, errors.New(
			`appconfig: mode="embedded" is no longer supported: embedded mode was retired (RIG-2554). ` +
				"Run a headless Compass stack with `compass-stack up` on a dedicated machine and " +
				`point the app at it in client mode (mode="client" with server_url = "https://host:8443")`)
	default:
		return Config{}, fmt.Errorf(
			"appconfig: unknown mode %q in app.toml: the only valid mode is %q",
			fc.Mode, modeStrClient)
	}
}

// parseClient validates the client-mode fields.
func parseClient(fc fileConfig) (Config, error) {
	if strings.TrimSpace(fc.ServerURL) == "" {
		return Config{}, errors.New(
			`appconfig: server_url is required in app.toml (e.g. server_url = "https://host:8443")`)
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

// Load resolves the app configuration from disk.
//
// The config path is computed from the caller-provided paths (design §A4):
// configHome/compass/app.toml when configHome is non-empty (the resolved
// $XDG_CONFIG_HOME), else home/.config/compass/app.toml. The caller reads the
// env; Load performs the resolution so it stays testable.
//
// An absent file is a legible first-run error: client mode requires a server_url
// and there is no zero-config default any more (embedded mode was retired,
// RIG-2554). A present file is read and Parsed.
func Load(configHome, home string) (Config, error) {
	path, err := configPath(configHome, home)
	if err != nil {
		return Config{}, err
	}

	// The path is the app's own config file, resolved from the caller's config
	// home — not attacker-controlled input.
	data, readErr := os.ReadFile(path) //nolint:gosec // G304: caller-resolved app config path, not user input
	switch {
	case readErr == nil:
		return Parse(data)
	case errors.Is(readErr, os.ErrNotExist):
		return Config{}, fmt.Errorf(
			"appconfig: no app config found at %s: the Compass app is a client and needs a "+
				`server_url to connect to. Create it with mode="client" and `+
				`server_url = "https://host:8443" (the address of a headless stack started with `+
				"`compass-stack up`)", path)
	default:
		return Config{}, fmt.Errorf("appconfig: reading %s: %w", path, readErr)
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
