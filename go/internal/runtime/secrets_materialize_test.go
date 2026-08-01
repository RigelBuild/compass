package runtime

// The T5 secret materializer: script-injection safety (a value whose bytes form
// shell metacharacters is written VERBATIM via base64 transport, never
// executed), name rejection (a name with a path separator / newline is refused
// before it becomes a path or a script token), redaction (no secret-bearing type
// leaks its value under %v/%#v/%s), kind routing (provider→seed, gh→hosts.yml,
// generic-file→secrets/<NAME>, each 0600), and the DeliveryEnv skip-with-warn.
//
// The scripts are executed through a real /bin/sh (scriptRunner below) against a
// temp $HOME, so "the value landed verbatim in a 0600 file and no command ran"
// is an observed fact, not an assertion about script text.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/internal/secrets"
)

// scriptRunner is a ContainerRuntime whose Exec actually runs the setup script
// through /bin/sh over stdin (as the real container would run `sh -s`), against
// the host filesystem — so the files a script writes are real and inspectable.
// AsUser is ignored (a test can't setuid); every other effect is genuine. The
// specs are recorded so a test can assert what identity each exec carried.
type scriptRunner struct {
	mu    sync.Mutex
	specs []ExecSpec
}

func (r *scriptRunner) Create(context.Context, ContainerSpec) (ContainerID, error) {
	return ContainerID("fake"), nil
}
func (r *scriptRunner) Start(context.Context, ContainerID) error { return nil }

func (r *scriptRunner) Exec(ctx context.Context, _ ContainerID, spec ExecSpec) (ExecOutput, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	if spec.Stdin == nil {
		return ExecOutput{}, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Stdin = strings.NewReader(*spec.Stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero script exit is a successful runtime call returning a failed
		// command (the ContainerRuntime contract), never a spawn error.
		return ExecOutput{Stderr: stderr.String(), ExitCode: exitErr.ExitCode()}, nil
	}
	if err != nil {
		return ExecOutput{}, err
	}
	return ExecOutput{}, nil
}

func (r *scriptRunner) ExecStreaming(context.Context, ContainerID, StreamingExecSpec) (*StreamingExec, error) {
	return nil, errors.New("scriptRunner does not support streaming exec")
}
func (r *scriptRunner) Stop(context.Context, ContainerID, time.Duration) error { return nil }
func (r *scriptRunner) Remove(context.Context, ContainerID) error              { return nil }
func (r *scriptRunner) Exists(context.Context, string) (bool, error)           { return false, nil }

func (r *scriptRunner) specsSnapshot() []ExecSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ExecSpec, len(r.specs))
	copy(out, r.specs)
	return out
}

// runScript runs a setup script through /bin/sh over stdin, exactly as the
// container runs `sh -s`, failing the test if the script itself errors.
func runScript(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup script failed: %v\noutput: %s\nscript:\n%s", err, out, script)
	}
}

// assert0600 asserts a file exists with exactly 0600 permission bits — the
// invariant every materialized secret file must hold.
func assert0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", path, got)
	}
}

// evilValue is a secret value whose bytes form every shell metacharacter that
// could break framing if it were interpolated into script source: a newline, a
// command substitution, a statement separator, and a single quote. Written
// verbatim, none of it executes.
func evilValue(markerPath string) string {
	return "first\n$(touch " + markerPath + ")\n;echo pwned\n'quote\"dquote`backtick"
}

// --- script-injection safety -------------------------------------------------

func TestSecretFileValueWrittenVerbatimNoExecution(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	value := evilValue(marker)
	path := filepath.Join(home, ".compass", "secrets", "DB_URL")

	script, err := SecretSetupScript(home, []SecretFile{{Name: "DB_URL", Path: path, Value: value}})
	if err != nil {
		t.Fatalf("SecretSetupScript = %v, want nil", err)
	}
	runScript(t, script)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading secret file: %v", err)
	}
	if string(got) != value {
		t.Fatalf("secret file content = %q, want the value verbatim %q", got, value)
	}
	assert0600(t, path)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists: an embedded $(...) executed — the value was interpolated, not base64-transported")
	}
	// The base64-transported script must not carry the raw dangerous bytes.
	if strings.Contains(script, "$(touch") || strings.Contains(script, ";echo pwned") {
		t.Fatalf("script contains the raw value bytes; the value must ride as base64, not shell source\nscript:\n%s", script)
	}
}

func TestProviderSeedValueWrittenVerbatimNoExecution(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	key := evilValue(marker)
	seed := ProviderSeed{Entries: map[string]ProviderSeedEntry{
		"anthropic": {Type: "api_key", Key: key},
	}}

	script, err := ProviderSeedScript(home, seed)
	if err != nil {
		t.Fatalf("ProviderSeedScript = %v, want nil", err)
	}
	runScript(t, script)

	path := filepath.Join(home, ".compass", "auth-seed.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading seed file: %v", err)
	}
	var decoded ProviderSeed
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("seed file is not valid JSON: %v\ngot: %s", err, got)
	}
	entry, ok := decoded.Entries["anthropic"]
	if !ok {
		t.Fatalf("seed JSON not shaped {entries:{<provider>:...}}; got %q", got)
	}
	if entry.Key != key {
		t.Fatalf("decoded seed key = %q, want the value verbatim %q", entry.Key, key)
	}
	if entry.Type != "api_key" {
		t.Fatalf("decoded seed entry type = %q, want api_key", entry.Type)
	}
	assert0600(t, path)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists: an embedded $(...) executed in the seed script")
	}
}

// --- name rejection ----------------------------------------------------------

func TestSecretSetupScriptRejectsBadNames(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"../escape", "a/b", "with space", "new\nline", "", "-lead"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(home, ".compass", "secrets", name)
			_, err := SecretSetupScript(home, []SecretFile{{Name: name, Path: path, Value: "v"}})
			if err == nil {
				t.Fatalf("SecretSetupScript accepted a bad name %q, want an error", name)
			}
		})
	}
}

func TestSecretSetupScriptRejectsPathOutsideSecretsDir(t *testing.T) {
	home := t.TempDir()
	// A well-formed name but a Path pointing outside $HOME/.compass/secrets/ —
	// the defense-in-depth prefix check must refuse it.
	_, err := SecretSetupScript(home, []SecretFile{{
		Name:  "OK",
		Path:  filepath.Join(home, ".ssh", "id_rsa"),
		Value: "v",
	}})
	if err == nil {
		t.Fatal("SecretSetupScript accepted a path outside the secrets dir, want an error")
	}
}

func TestInstallRejectsBadGenericName(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())
	err := m.Install(context.Background(), ContainerID("c"), home, 1000, []secrets.ResolvedSecret{
		{Name: "../evil", Value: "v", Kind: secrets.SecretGeneric, Delivery: secrets.DeliveryFile},
	})
	if err == nil {
		t.Fatal("Install accepted a generic secret with a bad name, want an error")
	}
}

// --- redaction ---------------------------------------------------------------

func TestSecretBearingTypesRedact(t *testing.T) {
	secret := "sk-super-secret-value"
	cases := []struct {
		name string
		v    any
	}{
		{"ProviderSeedEntry", ProviderSeedEntry{Type: "api_key", Key: secret}},
		{"ProviderSeed", ProviderSeed{Entries: map[string]ProviderSeedEntry{"openai": {Type: "api_key", Key: secret}}}},
		{"GHCredentials", GHCredentials{Host: "github.com", Token: secret}},
		{"SecretFile", SecretFile{Name: "DB_URL", Path: "/home/agent/.compass/secrets/DB_URL", Value: secret}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%s", "%#v", "%+v"} {
				got := fmtVerb(verb, tc.v)
				if strings.Contains(got, secret) {
					t.Fatalf("%s under %s leaked the secret: %q", tc.name, verb, got)
				}
				if !strings.Contains(got, "redacted") {
					t.Fatalf("%s under %s = %q, want a <redacted> marker", tc.name, verb, got)
				}
			}
		})
	}
}

// --- kind routing ------------------------------------------------------------

func TestInstallRoutesByKind(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())

	resolved := []secrets.ResolvedSecret{
		{Name: "OPENAI", Value: "sk-openai", Kind: secrets.SecretProvider, Provider: "openai", Delivery: secrets.DeliveryFile},
		{Name: "GH", Value: "gho_token", Kind: secrets.SecretGH, Host: "github.com", Delivery: secrets.DeliveryFile},
		{Name: "DB_URL", Value: "postgres://db", Kind: secrets.SecretGeneric, Delivery: secrets.DeliveryFile},
	}
	if err := m.Install(context.Background(), ContainerID("c"), home, 1000, resolved); err != nil {
		t.Fatalf("Install = %v, want nil", err)
	}

	seedPath := filepath.Join(home, ".compass", "auth-seed.json")
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("provider seed not written: %v", err)
	}
	if !strings.Contains(string(seed), "sk-openai") || !strings.Contains(string(seed), "openai") {
		t.Fatalf("provider seed missing the routed provider credential: %q", seed)
	}
	assert0600(t, seedPath)

	ghPath := filepath.Join(home, ".config", "gh", "hosts.yml")
	gh, err := os.ReadFile(ghPath)
	if err != nil {
		t.Fatalf("gh hosts.yml not written: %v", err)
	}
	if !strings.Contains(string(gh), "github.com") || !strings.Contains(string(gh), "gho_token") {
		t.Fatalf("gh hosts.yml missing host/token: %q", gh)
	}
	assert0600(t, ghPath)

	filePath := filepath.Join(home, ".compass", "secrets", "DB_URL")
	generic, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("generic secret file not written: %v", err)
	}
	if string(generic) != "postgres://db" {
		t.Fatalf("generic secret file = %q, want the value", generic)
	}
	assert0600(t, filePath)

	// Every exec ran as the agent uid, in the home dir — the git-credential
	// posture, copied.
	for _, spec := range rt.specsSnapshot() {
		if spec.User == nil || *spec.User != "1000" {
			t.Fatalf("materialize exec User = %v, want the agent uid 1000", spec.User)
		}
		if spec.Workdir == nil || *spec.Workdir != home {
			t.Fatalf("materialize exec Workdir = %v, want the home dir", spec.Workdir)
		}
		if len(spec.Command) < 2 || spec.Command[0] != "sh" || spec.Command[1] != "-s" {
			t.Fatalf("materialize exec command = %v, want sh -s (stdin channel, never argv)", spec.Command)
		}
		if spec.Stdin == nil {
			t.Fatal("materialize exec carried no stdin script")
		}
	}
}

// --- env-file delivery -------------------------------------------------------

// TestInstallWritesEnvDeliveryToEnvFile: a DeliveryEnv secret lands in the 0600
// aggregate $HOME/.compass/env as a KEY=VALUE line, and a file secret in the
// same set still lands in the secrets dir — the two channels are independent.
func TestInstallWritesEnvDeliveryToEnvFile(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())

	resolved := []secrets.ResolvedSecret{
		{Name: "ENV_ONLY", Value: "env-secret", Kind: secrets.SecretGeneric, Delivery: secrets.DeliveryEnv},
		{Name: "FILE_ONE", Value: "file-secret", Kind: secrets.SecretGeneric, Delivery: secrets.DeliveryFile},
	}
	if err := m.Install(context.Background(), ContainerID("c"), home, 1000, resolved); err != nil {
		t.Fatalf("Install with an env secret = %v, want nil", err)
	}

	envPath := filepath.Join(home, ".compass", "env")
	env, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file not materialized: %v", err)
	}
	if got := string(env); got != "ENV_ONLY=env-secret\n" {
		t.Fatalf("env file = %q, want the KEY=VALUE line", got)
	}
	assert0600(t, envPath)

	// The env secret was NOT also written to the secrets dir (env is its own
	// channel).
	if _, err := os.Stat(filepath.Join(home, ".compass", "secrets", "ENV_ONLY")); !os.IsNotExist(err) {
		t.Fatal("env-delivery secret was written to the secrets dir; it belongs only in the env file")
	}
	// The file secret in the same set still landed.
	if _, err := os.Stat(filepath.Join(home, ".compass", "secrets", "FILE_ONE")); err != nil {
		t.Fatalf("file secret not materialized alongside an env secret: %v", err)
	}
}

// TestEnvFileScriptRejectsNewlineValue: the env-file line grammar is one
// KEY=VALUE per line, so a value carrying a newline cannot be represented and
// must fail loudly rather than silently corrupt the file.
func TestEnvFileScriptRejectsNewlineValue(t *testing.T) {
	_, err := EnvFileScript("/home/agent", []SecretEnv{{Name: "BAD", Value: "line1\nline2"}})
	if err == nil {
		t.Fatal("EnvFileScript with a newline value = nil, want an error (env-file line grammar cannot carry it)")
	}
	if strings.Contains(err.Error(), "line1") {
		t.Fatalf("EnvFileScript error leaked the value: %v", err)
	}
}

// TestEnvFileScriptEmptySetWritesEmptyFile: an empty set still produces a script
// (the file must exist because the agent exec attaches --env-file always).
func TestEnvFileScriptEmptySetWritesEmptyFile(t *testing.T) {
	script, err := EnvFileScript("/home/agent", nil)
	if err != nil {
		t.Fatalf("EnvFileScript(empty) = %v, want a valid script", err)
	}
	if script == "" {
		t.Fatal("EnvFileScript(empty) returned no script; the env file must still be written")
	}
}

// --- multi-host / multi-provider aggregation ---------------------------------

// TestInstallMultipleGHHostsAllLand: a session declaring two gh forge
// credentials (github.com + a custom GHE host) must authenticate against BOTH —
// gh keys hosts.yml by host, so the second credential must not clobber the
// first. Materializing each into its own truncate-replace of the shared file
// would leave only the last host; this asserts both host blocks and both tokens
// survive in one hosts.yml.
func TestInstallMultipleGHHostsAllLand(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())

	resolved := []secrets.ResolvedSecret{
		{Name: "GH_DOTCOM", Value: "gho_dotcom", Kind: secrets.SecretGH, Host: "github.com", Delivery: secrets.DeliveryFile},
		{Name: "GH_ENTERPRISE", Value: "gho_ghe", Kind: secrets.SecretGH, Host: "ghe.example.com", Delivery: secrets.DeliveryFile},
	}
	if err := m.Install(context.Background(), ContainerID("c"), home, 1000, resolved); err != nil {
		t.Fatalf("Install = %v, want nil", err)
	}

	gh, err := os.ReadFile(filepath.Join(home, ".config", "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("gh hosts.yml not written: %v", err)
	}
	got := string(gh)
	for _, want := range []string{"github.com", "gho_dotcom", "ghe.example.com", "gho_ghe"} {
		if !strings.Contains(got, want) {
			t.Fatalf("gh hosts.yml missing %q; only the last host survived clobbering?\nfile:\n%s", want, got)
		}
	}
	assert0600(t, filepath.Join(home, ".config", "gh", "hosts.yml"))
}

// TestGHHostsScriptSameHostCollapsesToOneBlock: two gh credentials naming the
// SAME host must collapse to a single host block (last wins). gh loads
// hosts.yml with yaml.v3, which rejects a duplicate mapping key — so emitting
// the host twice would make the WHOLE file unparseable and break auth for every
// forge. Asserts exactly one `github.com:` key and that the last token wins.
func TestGHHostsScriptSameHostCollapsesToOneBlock(t *testing.T) {
	home := t.TempDir()
	script, err := GHHostsScript(home, []GHCredentials{
		{Host: "github.com", Token: "gho_first"},
		{Host: "github.com", Token: "gho_second"},
	})
	if err != nil {
		t.Fatalf("GHHostsScript = %v, want nil", err)
	}
	runScript(t, script)

	gh, err := os.ReadFile(filepath.Join(home, ".config", "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("gh hosts.yml not written: %v", err)
	}
	got := string(gh)
	if n := strings.Count(got, "github.com:"); n != 1 {
		t.Fatalf("want exactly one github.com: block (duplicate key breaks yaml.v3), got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "gho_second") {
		t.Fatalf("last token did not win:\n%s", got)
	}
	if strings.Contains(got, "gho_first") {
		t.Fatalf("first token should be superseded by the same-host collision:\n%s", got)
	}
}

// TestYamlDoubleQuoteEscapesControlBytes: a C0 control byte or DEL in a token
// must become a \xNN escape, not a literal byte — yaml.v3 rejects a literal
// control char in a quoted scalar, so an unescaped byte would make the whole
// hosts.yml unparseable. The named escapes stay named; a plain ASCII byte is
// verbatim.
func TestYamlDoubleQuoteEscapesControlBytes(t *testing.T) {
	got := yamlDoubleQuote("a\x00b\x1bc\x7fd\te")
	want := `"a\x00b\x1bc\x7fd\te"`
	if got != want {
		t.Fatalf("yamlDoubleQuote control-byte escaping = %q, want %q", got, want)
	}
}

// TestGHHostsScriptTokenWrittenVerbatim: a gh token whose bytes include yaml
// metacharacters (a quote, a newline, a backslash) lands byte-exact in the
// oauth_token value — the yaml double-quote escaping must round-trip it without
// restructuring the document, and the base64 transport must keep it off the
// shell source.
func TestGHHostsScriptTokenWrittenVerbatim(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "pwned")
	// A token carrying a command substitution, a quote, and a backslash: written
	// verbatim, none of it executes and none of it breaks the yaml value.
	token := `gho_$(touch ` + marker + `)"end\back`
	script, err := GHHostsScript(home, []GHCredentials{{Host: "github.com", Token: token}})
	if err != nil {
		t.Fatalf("GHHostsScript = %v, want nil", err)
	}
	runScript(t, script)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("gh token command substitution executed; the value reached shell source")
	}
	gh, err := os.ReadFile(filepath.Join(home, ".config", "gh", "hosts.yml"))
	if err != nil {
		t.Fatalf("gh hosts.yml not written: %v", err)
	}
	if !strings.Contains(string(gh), `oauth_token: "`+`gho_$(touch `+marker+`)`+`\"end\\back"`) {
		t.Fatalf("gh token not written as a verbatim yaml-escaped scalar:\n%s", gh)
	}
}

// TestInstallMultipleProvidersAllLand: two provider credentials aggregate into
// one auth-seed.json — neither entry drops.
func TestInstallMultipleProvidersAllLand(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())

	resolved := []secrets.ResolvedSecret{
		{Name: "OPENAI", Value: "sk-openai", Kind: secrets.SecretProvider, Provider: "openai", Delivery: secrets.DeliveryFile},
		{Name: "ANTHROPIC", Value: "sk-anthropic", Kind: secrets.SecretProvider, Provider: "anthropic", Delivery: secrets.DeliveryFile},
	}
	if err := m.Install(context.Background(), ContainerID("c"), home, 1000, resolved); err != nil {
		t.Fatalf("Install = %v, want nil", err)
	}

	seed, err := os.ReadFile(filepath.Join(home, ".compass", "auth-seed.json"))
	if err != nil {
		t.Fatalf("provider seed not written: %v", err)
	}
	for _, want := range []string{"openai", "sk-openai", "anthropic", "sk-anthropic"} {
		if !strings.Contains(string(seed), want) {
			t.Fatalf("auth-seed.json missing %q; a provider entry dropped:\n%s", want, seed)
		}
	}
}

// TestInstallEmptySetWritesOnlyTheEmptyEnvFile: an empty resolved set writes no
// seed/gh/file secrets, but still materializes the aggregate env file (empty) —
// the agent exec attaches --env-file unconditionally, so the path must exist.
func TestInstallEmptySetWritesOnlyTheEmptyEnvFile(t *testing.T) {
	home := t.TempDir()
	rt := &scriptRunner{}
	m := NewSecretMaterializer(rt, discardLog())

	if err := m.Install(context.Background(), ContainerID("c"), home, 1000, nil); err != nil {
		t.Fatalf("Install(empty) = %v, want nil", err)
	}
	// Exactly one exec: the env-file write.
	if specs := rt.specsSnapshot(); len(specs) != 1 {
		t.Fatalf("Install(empty) ran %d execs, want 1 (the env-file only)", len(specs))
	}
	envPath := filepath.Join(home, ".compass", "env")
	env, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("empty-set Install did not write the env file: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("empty-set env file = %q, want empty", env)
	}
	assert0600(t, envPath)
	// No seed / gh / secrets-dir content.
	if _, err := os.Stat(filepath.Join(home, ".compass", "auth-seed.json")); !os.IsNotExist(err) {
		t.Fatal("empty-set Install wrote a provider seed")
	}
}

// fmtVerb formats v with the given fmt verb — a one-liner so the redaction table
// stays readable.
func fmtVerb(verb string, v any) string { return fmt.Sprintf(verb, v) }

// discardLog builds a slog.Logger that drops output, for the materializer paths
// whose log lines a test does not assert on.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
