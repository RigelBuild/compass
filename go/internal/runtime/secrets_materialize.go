// The T5 Runner-side secret materializer: turn a fetched secret set into
// container state, copying the git-credential channel's posture exactly
// (workspace.go CredentialSetupScript + agent.go installCredentials). Each setup
// script is fed to `sh -s` over stdin — never `sh -c`, never argv — run as the
// unprivileged agent uid in the agent's scoped $HOME, writing 0600 files.
//
// The load-bearing script-injection-safety invariant (design record §808-818):
// a secret VALUE is never interpolated into shell source. It is base64-encoded
// on the Go side, embedded as a single-quoted literal, and decoded in-container
// (`printf %s '<b64>' | base64 -d`). The base64 alphabet ([A-Za-z0-9+/=]) holds
// no single quote and no newline, so a value whose own bytes form a delimiter
// line, a `$(...)`, or a `;` is inert — the transport is delimiter-independent.
//
// Every value-bearing type here redacts under %s/%v/%#v, like Credentials
// (workspace.go:35-40) and secrets.ResolvedSecret.
package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sealedsecurity/compass/go/internal/secrets"
)

// providerCredentialAPIKey is the SDK ApiKeyCredential discriminant
// (pi-ai auth-storage.d.ts:16-20 — {type:"api_key", key}). The api-key MVP
// emits only this type; an OAuth extension adds a sibling.
const providerCredentialAPIKey = "api_key"

// compassDirName is the per-agent config subdir under $HOME, and envFileName is
// the aggregate env-secret file within it. Env-delivery secrets are written here
// as KEY=VALUE lines (SEA-1327 T5) and sourced by the agent from its own
// namespace at startup — never `-e KEY=VALUE` (host-process-list visible) nor
// `podman exec --env-file` (podman resolves that path host-side, where this
// container-internal file does not exist).
const (
	compassDirName = ".compass"
	envFileName    = "env"
)

// AgentEnvFilePath is the in-container path of the aggregate env-secret file for
// an agent whose scoped $HOME is homeDir. The materializer writes it and the
// agent sources it at startup, so both derive the path from here and cannot
// drift.
func AgentEnvFilePath(homeDir string) string {
	return filepath.Join(homeDir, compassDirName, envFileName)
}

// ProviderSeedEntry is one provider's credential in the AuthStorage seed. The
// field set mirrors the SDK's ApiKeyCredential ({type:"api_key", key} —
// pi-ai auth-storage.d.ts:16-20), so an OAuth extension is additive. Key is the
// secret and redacts under every fmt verb.
type ProviderSeedEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// String redacts Key so a formatted entry never lands in a log line.
func (e ProviderSeedEntry) String() string {
	return fmt.Sprintf("ProviderSeedEntry{type: %q, key: <redacted>}", e.Type)
}

// GoString redacts Key under %#v as well.
func (e ProviderSeedEntry) GoString() string { return e.String() }

// ProviderSeed is the provider-id → credential seed the Runner materializes to a
// 0600 $HOME/.compass/auth-seed.json, which the container entrypoint's per-call
// getApiKey resolver re-reads (design Decision 3, T5). Every entry redacts.
type ProviderSeed struct {
	Entries map[string]ProviderSeedEntry `json:"entries"`
}

// String redacts every entry's key: it lists the provider ids present, never a
// credential.
func (s ProviderSeed) String() string {
	ids := make([]string, 0, len(s.Entries))
	for id := range s.Entries {
		ids = append(ids, id)
	}
	return fmt.Sprintf("ProviderSeed{providers: %v, keys: <redacted>}", ids)
}

// GoString redacts under %#v as well.
func (s ProviderSeed) GoString() string { return s.String() }

// GHCredentials is a gh forge credential routed to ~/.config/gh/hosts.yml
// (design §414-419). Token is the secret and redacts.
type GHCredentials struct {
	Host  string
	Token string
}

// String redacts the token, showing only the host.
func (g GHCredentials) String() string {
	return fmt.Sprintf("GHCredentials{host: %q, token: <redacted>}", g.Host)
}

// GoString redacts the token under %#v as well.
func (g GHCredentials) GoString() string { return g.String() }

// SecretFile is one generic file-delivery secret: a validated name, its target
// path under $HOME/.compass/secrets/, and its value. Value is the secret and
// redacts.
type SecretFile struct {
	Name  string
	Path  string
	Value string
}

// String redacts the value, showing only the name and path.
func (f SecretFile) String() string {
	return fmt.Sprintf("SecretFile{name: %q, path: %q, value: <redacted>}", f.Name, f.Path)
}

// GoString redacts the value under %#v as well.
func (f SecretFile) GoString() string { return f.String() }

// SecretEnv is one env-delivery secret: a validated name (an env-var key) and
// its value. Value is the secret and redacts. Env secrets aggregate into the
// 0600 $HOME/.compass/env file the agent sources at startup.
type SecretEnv struct {
	Name  string
	Value string
}

// String redacts the value, showing only the name.
func (e SecretEnv) String() string {
	return fmt.Sprintf("SecretEnv{name: %q, value: <redacted>}", e.Name)
}

// GoString redacts the value under %#v as well.
func (e SecretEnv) GoString() string { return e.String() }

// SecretMaterializer installs a fetched secret set into a container over the
// stdin-exec channel, routing each secret by Kind. It is the Runner-side half of
// SEA-1327 T5, driven from the SecretsVersion dispatch hook (initial materialize
// and rotation ride the same signal path).
type SecretMaterializer struct {
	runtime ContainerRuntime
	log     *slog.Logger
}

// NewSecretMaterializer builds a materializer over the container engine. A nil
// log falls back to slog.Default.
func NewSecretMaterializer(runtime ContainerRuntime, log *slog.Logger) *SecretMaterializer {
	if log == nil {
		log = slog.Default()
	}
	return &SecretMaterializer{runtime: runtime, log: log}
}

// ProviderSeedScript is the setup script that writes the provider seed to a 0600
// $HOME/.compass/auth-seed.json. The serialized seed JSON is base64-embedded and
// decoded in-container — never shell-interpolated — and written write-temp +
// atomic mv so a concurrent reader never sees a half-written seed. Meant to be
// fed to `sh -s` over stdin, never `sh -c`.
func ProviderSeedScript(homeDir string, seed ProviderSeed) (string, error) {
	payload, err := json.Marshal(seed)
	if err != nil {
		return "", fmt.Errorf("serializing provider seed: %w", err)
	}
	home := shellSingleQuote(homeDir)
	b64 := base64.StdEncoding.EncodeToString(payload)

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	b.WriteString("h=" + home + "\n")
	b.WriteString(`mkdir -p "$h/.compass"` + "\n")
	b.WriteString(`f="$h/.compass/auth-seed.json"` + "\n")
	b.WriteString(`t="$f.tmp.$$"` + "\n")
	writeBase64ToFile(&b, b64)
	b.WriteString(`chmod 600 "$t"` + "\n")
	b.WriteString(`mv "$t" "$f"` + "\n")
	return b.String(), nil
}

// GHHostsScript builds the setup script that writes ALL gh forge credentials
// into a single ~/.config/gh/hosts.yml (0600) under the agent's scoped $HOME.
// gh keys hosts.yml by host, so github.com and any custom forge coexist as
// sibling host blocks in one file; writing every host in ONE atomic replace
// (rather than one truncate-replace per host) is what lets a multi-forge
// session authenticate against every forge, not only the last one written.
// Each host is validated (isValidHost — written into the yml, never escaped).
// Each token is emitted as a yaml double-quoted, escaped scalar, and the whole
// document rides as base64, decoded in-container — so a token byte can neither
// break the yaml framing (double-quote escaping) nor reach shell source (base64
// transport). Byte-exact for the document, the same posture as ProviderSeedScript.
// Meant to be fed to `sh -s` over stdin, never `sh -c`. Empty creds → ("", nil).
func GHHostsScript(homeDir string, creds []GHCredentials) (string, error) {
	if len(creds) == 0 {
		return "", nil
	}
	// gh keys hosts.yml by host and yaml.v3 rejects a duplicate mapping key, so
	// two credentials naming the same host must collapse to a single block
	// (last wins) — emitting the host twice would make the WHOLE file
	// unparseable and break auth for every forge, not just the colliding one.
	order := make([]string, 0, len(creds))
	tokenByHost := make(map[string]string, len(creds))
	for _, c := range creds {
		if !isValidHost(c.Host) {
			return "", &InvalidHostError{Host: c.Host}
		}
		if _, seen := tokenByHost[c.Host]; !seen {
			order = append(order, c.Host)
		}
		tokenByHost[c.Host] = c.Token
	}
	var doc strings.Builder
	for _, host := range order {
		// oauth_token is the field gh reads; the host key coexists with any
		// other forge's block in the same file.
		doc.WriteString(host + ":\n")
		doc.WriteString("    oauth_token: " + yamlDoubleQuote(tokenByHost[host]) + "\n")
	}
	home := shellSingleQuote(homeDir)
	b64 := base64.StdEncoding.EncodeToString([]byte(doc.String()))

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	b.WriteString("h=" + home + "\n")
	b.WriteString(`mkdir -p "$h/.config/gh"` + "\n")
	b.WriteString(`f="$h/.config/gh/hosts.yml"` + "\n")
	b.WriteString(`t="$f.tmp.$$"` + "\n")
	writeBase64ToFile(&b, b64)
	b.WriteString(`chmod 600 "$t"` + "\n")
	b.WriteString(`mv "$t" "$f"` + "\n")
	return b.String(), nil
}

// yamlDoubleQuote renders s as a yaml double-quoted scalar, so an arbitrary
// token byte cannot restructure the document or terminate the value line. The
// two framing-dangerous bytes (" and newline) are escaped, and every other C0
// control and DEL is emitted as a \xNN escape so the scalar is always a VALID
// yaml document (yaml.v3 rejects a literal control byte in a quoted scalar).
// An ASCII token (every real gh token) stays byte-exact.
func yamlDoubleQuote(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 || c == 0x7f {
				b.WriteString(`\x`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0x0f])
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SecretSetupScript is the setup script that writes each generic file-delivery
// secret to its 0600 path under $HOME/.compass/secrets/ (write-temp + atomic
// mv, umask 077). Defense in depth: each name is re-validated against
// secrets.ValidateName (validated at declaration in T3) and each cleaned path is
// checked to lie under $HOME/.compass/secrets/, so neither a name nor a path can
// escape the secrets dir or become a script token. Values ride as base64,
// decoded in-container — never interpolated into shell source. Fed to `sh -s`.
func SecretSetupScript(homeDir string, files []SecretFile) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	secretsDir := filepath.Join(homeDir, ".compass", "secrets")
	home := shellSingleQuote(homeDir)

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	b.WriteString("h=" + home + "\n")
	b.WriteString(`mkdir -p "$h/.compass/secrets"` + "\n")
	for _, f := range files {
		if err := secrets.ValidateName(f.Name); err != nil {
			return "", fmt.Errorf("secret name %q: %w", f.Name, err)
		}
		cleaned := filepath.Clean(f.Path)
		if err := checkUnderDir(cleaned, secretsDir); err != nil {
			return "", fmt.Errorf("secret %q path: %w", f.Name, err)
		}
		// The path is $HOME/.compass/secrets/<validated-name>; the name matched
		// secrets.ValidateName (no separator, no metachar), so the relative
		// segment is safe to interpolate as a single-quoted literal.
		rel := shellSingleQuote(filepath.Base(cleaned))
		valB64 := base64.StdEncoding.EncodeToString([]byte(f.Value))
		b.WriteString(`p="$h/.compass/secrets/"` + rel + "\n")
		b.WriteString(`t="$p.tmp.$$"` + "\n")
		writeBase64ToFile(&b, valB64)
		b.WriteString(`chmod 600 "$t"` + "\n")
		b.WriteString(`mv "$t" "$p"` + "\n")
	}
	return b.String(), nil
}

// EnvFileScript is the setup script that writes the aggregate 0600 env-secret
// file at $HOME/.compass/env (write-temp + atomic mv, umask 077). Each secret
// becomes one `KEY=VALUE` line; the whole file is base64-embedded and decoded
// in-container, so no value is ever interpolated into shell source (the same
// delimiter-independent transport as the seed and file secrets). Defense in
// depth: each name is re-validated against secrets.ValidateName (the env-var-key
// grammar), and a value carrying a newline or NUL is rejected — the env-file
// line grammar is one KEY=VALUE per line, so such a value cannot be represented
// and must fail loudly rather than silently truncate. Fed to `sh -s`. Always
// writes the file — an empty set yields a 0600 empty file — so the agent can
// unconditionally source it at startup without a missing-file special case.
func EnvFileScript(homeDir string, envs []SecretEnv) (string, error) {
	var payload strings.Builder
	for _, e := range envs {
		if err := secrets.ValidateName(e.Name); err != nil {
			return "", fmt.Errorf("env secret name %q: %w", e.Name, err)
		}
		if strings.ContainsAny(e.Value, "\n\x00") {
			return "", fmt.Errorf("env secret %q value contains a newline or NUL, which the env-file line grammar cannot carry", e.Name)
		}
		payload.WriteString(e.Name + "=" + e.Value + "\n")
	}
	home := shellSingleQuote(homeDir)
	b64 := base64.StdEncoding.EncodeToString([]byte(payload.String()))

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	b.WriteString("h=" + home + "\n")
	b.WriteString(`mkdir -p "$h/.compass"` + "\n")
	b.WriteString(`f="$h/.compass/env"` + "\n")
	b.WriteString(`t="$f.tmp.$$"` + "\n")
	writeBase64ToFile(&b, b64)
	b.WriteString(`chmod 600 "$t"` + "\n")
	b.WriteString(`mv "$t" "$f"` + "\n")
	return b.String(), nil
}

// Install materializes the resolved secret set into the container, routing each
// secret by delivery then Kind: DeliveryEnv secrets aggregate into the 0600
// $HOME/.compass/env file (sourced by the agent at startup); otherwise
// SecretProvider entries aggregate into one provider seed, SecretGH secrets
// install per host, and generic file-delivery secrets write to
// $HOME/.compass/secrets/<NAME>. Each setup script is fed to `sh -s` over stdin
// as the agent uid in the agent's $HOME, the git-credential posture.
func (m *SecretMaterializer) Install(ctx context.Context, id ContainerID, homeDir string, uid uint32, resolved []secrets.ResolvedSecret) error {
	seed := ProviderSeed{Entries: map[string]ProviderSeedEntry{}}
	var files []SecretFile
	var ghCreds []GHCredentials
	var envs []SecretEnv

	for _, s := range resolved {
		if s.Delivery == secrets.DeliveryEnv {
			// Env delivery: aggregate into the 0600 $HOME/.compass/env file the
			// agent sources at startup. The name is re-validated (env-var-key
			// grammar) in EnvFileScript; a newline/NUL value is rejected there.
			envs = append(envs, SecretEnv{Name: s.Name, Value: s.Value})
			continue
		}
		switch s.Kind {
		case secrets.SecretProvider:
			seed.Entries[s.Provider] = ProviderSeedEntry{Type: providerCredentialAPIKey, Key: s.Value}
		case secrets.SecretGH:
			ghCreds = append(ghCreds, GHCredentials{Host: s.Host, Token: s.Value})
		default:
			if err := secrets.ValidateName(s.Name); err != nil {
				return fmt.Errorf("generic secret name %q: %w", s.Name, err)
			}
			files = append(files, SecretFile{
				Name:  s.Name,
				Path:  filepath.Join(homeDir, ".compass", "secrets", s.Name),
				Value: s.Value,
			})
		}
	}

	if len(seed.Entries) > 0 {
		script, err := ProviderSeedScript(homeDir, seed)
		if err != nil {
			return fmt.Errorf("building provider seed script: %w", err)
		}
		if err := m.runScript(ctx, id, homeDir, uid, "install provider seed", script); err != nil {
			return err
		}
	}
	if len(ghCreds) > 0 {
		script, err := GHHostsScript(homeDir, ghCreds)
		if err != nil {
			return fmt.Errorf("building gh credential script: %w", err)
		}
		if err := m.runScript(ctx, id, homeDir, uid, "install gh credentials", script); err != nil {
			return err
		}
	}
	if len(files) > 0 {
		script, err := SecretSetupScript(homeDir, files)
		if err != nil {
			return fmt.Errorf("building secret file script: %w", err)
		}
		if err := m.runScript(ctx, id, homeDir, uid, "install file secrets", script); err != nil {
			return err
		}
	}
	// Always install the env file — even with no env secrets — so the agent can
	// unconditionally source $HOME/.compass/env without a missing-file case.
	envScript, err := EnvFileScript(homeDir, envs)
	if err != nil {
		return fmt.Errorf("building env-file script: %w", err)
	}
	if err := m.runScript(ctx, id, homeDir, uid, "install env secrets", envScript); err != nil {
		return err
	}
	return nil
}

// runScript feeds one setup script to `sh -s` over stdin as the agent uid in the
// agent's $HOME — never `sh -c`, never argv (the secret is in the script body,
// and argv is visible in the container's process list while stdin is not).
func (m *SecretMaterializer) runScript(ctx context.Context, id ContainerID, homeDir string, uid uint32, stage, script string) error {
	spec := NewExecSpec("sh", "-s").
		AsUser(strconv.FormatUint(uint64(uid), 10)).
		InDir(homeDir).
		WithStdin(script)
	out, err := m.runtime.Exec(ctx, id, spec)
	if err != nil {
		return atStage(stage, err)
	}
	return requireSuccess(stage, out)
}

// writeBase64ToFile appends the decode-and-write line to the temp file "$t": the
// base64 literal is single-quoted (its alphabet holds no quote/newline, so it
// cannot break out), decoded, and redirected. Every caller stages through "$t"
// then chmod+mv into place.
func writeBase64ToFile(b *strings.Builder, b64 string) {
	b.WriteString("printf %s '" + b64 + "' | base64 -d > \"$t\"\n")
}

// checkUnderDir reports an error unless cleaned lies within dir (a defense-in-
// depth prefix check against a path that would escape the secrets dir). Both
// arguments must already be cleaned/absolute.
func checkUnderDir(cleaned, dir string) error {
	rel, err := filepath.Rel(dir, cleaned)
	if err != nil {
		return fmt.Errorf("resolving %q against %q: %w", cleaned, dir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes the secrets dir %q", cleaned, dir)
	}
	return nil
}
