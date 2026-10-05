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

// ErrNoConfig means app.toml is absent and no mode override selected a mode.
var ErrNoConfig = errors.New("appconfig: no app.toml and no mode override")

// ErrConfigExists means a config was already present when a save was attempted.
var ErrConfigExists = errors.New("appconfig: app.toml already exists")

// URLError carries the raw input and the sentence shown by the setup UI.
type URLError struct {
	URL    string
	Reason string
	msg    string // the config-file diagnostic Error returns
}

func (e *URLError) Error() string { return e.msg }

// NormalizeServerURL trims and validates a server origin used by client RPCs.
func NormalizeServerURL(raw string) (string, error) {
	fail := func(reason, msg string) (string, error) {
		return "", &URLError{URL: raw, Reason: reason, msg: msg}
	}
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil {
		return fail("The server URL is not valid.",
			fmt.Sprintf("appconfig: server_url %q is not a valid URL: %v", raw, err))
	}
	if u.Scheme != "https" {
		return fail("The server URL must use https.", fmt.Sprintf(
			"appconfig: server_url %q must use https (got scheme %q): cleartext connections are not allowed", raw, u.Scheme))
	}
	if u.Hostname() == "" {
		return fail("The server URL must include a host.",
			fmt.Sprintf("appconfig: server_url %q must be absolute with a host (e.g. https://host:8443)", raw))
	}
	if u.User != nil {
		return fail("The server URL must not include credentials.", fmt.Sprintf(
			"appconfig: server_url %q must not embed credentials; the bearer token is entered in the connect screen and stored in the OS keychain (DL-109)", raw))
	}
	// url.Parse drops an empty trailing "#", so the raw text is checked too.
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(trimmed, "#") {
		return fail("The server URL must not include a path, query, or fragment.",
			fmt.Sprintf("appconfig: server_url %q must not include a path, query, or fragment (e.g. https://host:8443)", raw))
	}
	return "https://" + u.Host, nil
}

// Mode is the native app's operating mode, selected by app.toml and an optional
// --mode/$COMPASS_APP_MODE override (design §A1). The app is dual-mode: it
// either supervises a local stack (embedded) or dials a remote one (client).
type Mode int

const (
	// ModeClient connects to a remote compass-server over its authenticated
	// loopback/network door; it requires a ServerURL and may carry a CACert.
	// It KEEPS the zero value so a client Config need not be spelled out.
	ModeClient Mode = iota
	// ModeEmbedded is the local-supervisor mode: the app brings up and
	// supervises a private stack in-process. An app.toml with an empty or
	// absent mode selects it. It is declared AFTER ModeClient so ModeClient
	// retains the zero value.
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
//   - absent/empty mode or mode="embedded" → ModeEmbedded. server_url and
//     ca_cert are client-only fields, so a
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
	serverURL, err := NormalizeServerURL(fc.ServerURL)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Mode:      ModeClient,
		ServerURL: serverURL,
		CACert:    fc.CACert,
	}, nil
}

// Load resolves app config and applies a mode override. ErrNoConfig signals an
// absent file with no override; explicit overrides still resolve independently.
func Load(configHome, home, override string) (Config, error) {
	path, err := ConfigPath(configHome, home)
	if err != nil {
		return Config{}, err
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // G304: caller-resolved app config path, not user input
	if errors.Is(readErr, os.ErrNotExist) && strings.TrimSpace(override) == "" {
		// A dangling symlink reads as absent but blocks the exclusive save, so it
		// must surface as a read error, not as a first run that can never finish.
		if _, lerr := os.Lstat(path); lerr == nil {
			return Config{}, fmt.Errorf("appconfig: reading %s: %w", path, readErr)
		}
		return Config{}, ErrNoConfig
	}

	cfg := Config{Mode: ModeEmbedded}
	switch {
	case readErr == nil:
		cfg, err = Parse(data)
		if err != nil {
			return Config{}, err
		}
	case errors.Is(readErr, os.ErrNotExist):
		// A resolved override can select a mode without a config file.
	default:
		return Config{}, fmt.Errorf("appconfig: reading %s: %w", path, readErr)
	}

	return applyOverride(cfg, override)
}

// applyOverride applies a resolved mode override after the file is validated.
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

// ConfigPath resolves app.toml from the caller-provided config and home dirs.
func ConfigPath(configHome, home string) (string, error) {
	if configHome != "" {
		return filepath.Join(configHome, "compass", "app.toml"), nil
	}
	if home != "" {
		return filepath.Join(home, ".config", "compass", "app.toml"), nil
	}
	return "", errors.New("appconfig: cannot resolve config path: both configHome and home are empty")
}
