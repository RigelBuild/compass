package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // secrets write seam: spawns the operator-pinned secretspec CLI (G204 site justified below)
	"sort"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store"
	secretspec "github.com/cachix/secretspec/secretspec-go"
)

// manifestProject is the single SecretSpec project name Compass resolves under.
// The Server owns one project, one profile; the registry, not a repo manifest,
// is the source of truth (Decision 2, no repo-committed secretspec.toml).
const manifestProject = "compass"

// defaultProfile is the SecretSpec profile Compass resolves under when none is
// configured — the Server owns one project, one profile.
const defaultProfile = "default"

// defaultCLI is the SecretSpec binary the write path spawns by name, resolved
// off PATH (the dev shell and the deployed image both stage it). Named so the
// drift guard asserting the staged binary's version floor and the resolver
// agree on which binary that is.
const defaultCLI = "secretspec"

// declarations is the read surface the Resolver needs from the store: the whole
// declared set. store.Store satisfies it. An interface (not the concrete
// *store.Store) so the pure resolve logic is unit-testable with a fake, without
// a Postgres harness.
type declarations interface {
	DeclaredSecrets(ctx context.Context) ([]store.SecretDeclaration, error)
}

// Resolver resolves the declared secret set to values through SecretSpec, and
// provides the provider write path the T7 entry RPCs require. Resolve reads the
// whole registry (inject-all: no per-agent filter in the MVP — a names filter
// is the future grants seam). Set/Delete are the provider write path.
type Resolver interface {
	// Resolve resolves every declared secret to its value via SecretSpec,
	// returning a ResolvedSecret per declaration with a content-hash Version.
	// reason is recorded in the SecretSpec audit log. A required secret missing
	// from the provider is an error (the store declared it, so the provider must
	// hold it).
	Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error)
	// Set writes a value into the provider for an already-declared name. The
	// value is fed to the pinned CLI over stdin, never argv. reason is recorded
	// in the SecretSpec audit log and is required: an empty reason is rejected
	// before the CLI is spawned, so the audit reason travels with every write
	// exactly as it does on the read path.
	Set(ctx context.Context, name, value, reason string) error
	// Delete removes a value from the provider for a name.
	Delete(ctx context.Context, name string) error
}

// SpecResolver is the SecretSpec-backed Resolver. It reads the names registry
// from the store, generates a SecretSpec manifest under its own state dir, and
// resolves values from the configured provider. The resolver process (the
// Server) is the only place SecretSpec runs — containers receive resolved
// values, never provider access.
type SpecResolver struct {
	store    declarations
	provider string // SecretSpec provider URI (e.g. "keyring://"); "" = SDK default chain
	profile  string // SecretSpec profile (e.g. "default")
	// stateDir is where the generated manifest is written — the Server's own
	// state directory, never repo state. The SDK builder takes provider/profile
	// plus a manifest path, so the resolver points WithPath at this manifest.
	stateDir string
	// cli is the pinned secretspec binary for the write path (Set/Delete). The
	// SDK is read-shaped; upstream writes are CLI-only. Defaults to "secretspec"
	// resolved on PATH; set explicitly to pin the Server's closure binary.
	cli string
}

// SpecOption configures a SpecResolver.
type SpecOption func(*SpecResolver)

// WithProvider pins the SecretSpec provider URI (e.g. "keyring://",
// "onepassword://Production"). Empty uses the SDK's default provider chain.
func WithProvider(uri string) SpecOption { return func(r *SpecResolver) { r.provider = uri } }

// WithProfile pins the SecretSpec profile. Empty uses the SDK default.
func WithProfile(profile string) SpecOption { return func(r *SpecResolver) { r.profile = profile } }

// WithCLI pins the secretspec CLI binary used for the write path.
func WithCLI(path string) SpecOption { return func(r *SpecResolver) { r.cli = path } }

// NewSpecResolver constructs a SecretSpec-backed Resolver over the store's
// names registry. stateDir is the Server-owned directory the generated manifest
// is written under (created if absent).
func NewSpecResolver(st declarations, stateDir string, opts ...SpecOption) *SpecResolver {
	r := &SpecResolver{
		store:    st,
		profile:  defaultProfile,
		stateDir: stateDir,
		cli:      defaultCLI,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// buildManifest renders the SecretSpec manifest TOML for a declared set: one
// [project] block and one [profiles.<profile>] block with every declared name
// as a required key. Value-free — the manifest declares names, never values.
// Names are re-validated here (defense in depth) so a malformed name can never
// reach the emitted TOML. A pure function of its inputs, so it is unit-testable
// without a store or the FFI resolver.
func buildManifest(profile string, decls []store.SecretDeclaration) (string, error) {
	if profile == "" {
		profile = defaultProfile
	}
	if err := ValidateProfile(profile); err != nil {
		return "", err
	}
	// Sort by name for a deterministic manifest (stable across resolves).
	sorted := append([]store.SecretDeclaration(nil), decls...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	fmt.Fprintf(&b, "[project]\nname = %q\nrevision = \"1.0\"\n\n", manifestProject)
	fmt.Fprintf(&b, "[profiles.%s]\n", profile)
	for _, d := range sorted {
		if err := ValidateName(d.Name); err != nil {
			return "", err
		}
		// required = true: the store declared it, so the provider must hold it;
		// a missing one is a MissingRequiredError at resolve, surfaced loudly.
		fmt.Fprintf(&b, "%s = { description = %q, required = true }\n", d.Name, "compass declared secret")
	}
	return b.String(), nil
}

// Resolve reads the whole names registry, generates the manifest, resolves it
// through SecretSpec against the configured provider, and maps each declaration
// to a ResolvedSecret (value from the provider, content-hash Version). An empty
// registry resolves to an empty set with no provider call. inject-all: the
// whole store, no per-agent filter (the future grants seam).
func (r *SpecResolver) Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error) {
	decls, err := r.store.DeclaredSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets: read registry: %w", err)
	}
	if len(decls) == 0 {
		return nil, nil
	}
	// One accessor for the profile so the manifest header and the resolving
	// profile can never diverge: buildManifest emits [profiles.<profile>] and
	// the SDK resolves the same <profile>.
	profile := r.resolvedProfile()
	manifestPath, err := r.writeManifest(profile, decls)
	if err != nil {
		return nil, err
	}
	// The manifest is a transient input to Load — a per-resolve temp file, so
	// concurrent resolves never share one path (each gets its own). Remove it
	// once resolved; the registry, not this file, is the durable source.
	defer func() { _ = os.Remove(manifestPath) }()

	b := secretspec.New().WithPath(manifestPath).WithReason(reason)
	if r.provider != "" {
		b = b.WithProvider(r.provider)
	}
	b = b.WithProfile(profile)
	resolved, err := b.Load()
	if err != nil {
		// MissingRequiredError and *secretspec.Error both carry a value-free
		// message (names + kind), so wrapping cannot leak a value.
		return nil, fmt.Errorf("secrets: resolve: %w", err)
	}

	// resolved.Close removes the 0400 temp files SecretSpec creates for as_path
	// secrets. The manifest declares none today (every key is {description,
	// required=true}), so this is a no-op that keeps the SDK's documented idiom.
	// If as_path is ever declared, out[].Value would hold a file PATH the caller
	// reads after Resolve returns — move this Close to after materialization, or
	// it removes the file before the caller reads it.
	defer func() { _ = resolved.Close() }()

	out := make([]ResolvedSecret, 0, len(decls))
	for _, d := range decls {
		rs, ok := resolved.Secrets[d.Name]
		if !ok {
			// The manifest declared it required, so Load would have errored on a
			// genuine miss; a name present in decls but absent here means the
			// resolver dropped it — surface it rather than emit an empty value.
			return nil, fmt.Errorf("secrets: declared secret %q not in resolver output", d.Name)
		}
		value, present := rs.Usable()
		if !present {
			return nil, fmt.Errorf("secrets: declared secret %q resolved with no value", d.Name)
		}
		out = append(out, ResolvedSecret{
			Name:     d.Name,
			Value:    value,
			Version:  Version(value),
			Delivery: deliveryFromStore(d.Delivery),
			Kind:     kindFromStore(d.Kind),
			Host:     d.Host,
			Provider: d.Provider,
		})
	}
	return out, nil
}

// Set writes value into the provider for name via the pinned CLI, feeding the
// value on stdin (never argv, so it is not visible in the host process list).
// The SDK is read-shaped, so the write path shells the CLI. name must be a
// valid secret name; an empty value and an empty reason are both rejected up
// front. reason is recorded in the SecretSpec audit log and can be required by
// the provider policy, so it travels with every write exactly as it does on
// the read path.
//
// The write is pointed at a generated manifest through the global --file flag,
// the same explicit-manifest treatment Resolve gives the read path: the
// registry is the source of truth and no secretspec.toml is committed, so a
// CLI left to discover one walks up from the process cwd and finds nothing.
// The generated manifest declares exactly the name being written.
//
// Verified against secretspec v0.20.0 source (secrets.rs:4423-4427 for the
// piped-stdin branch and trim, :4430-4433 for empty-value rejection): `set
// <NAME>` with the value omitted from argv and stdin not a tty takes the
// piped-stdin branch — a first-class io::stdin().read_to_string() with no
// interactive prompt constructed — then trims the value and rejects an empty
// one. So `secretspec --file=<m> --reason=<r> set <NAME> --provider <p>
// --profile <P>` with the value on stdin is the write path, no positional
// VALUE. The joined --flag=value form is required, not stylistic: the
// two-token form parses a leading-dash reason as the next flag and exits 2.
func (r *SpecResolver) Set(ctx context.Context, name, value, reason string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	// The CLI trims the piped value and rejects an empty one; reject it here so
	// the failure is a deterministic caller error, not a shelled-out exit.
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secrets: set %q: value is empty", name)
	}
	// The reason is the audit record, and the CLI's own require_reason policy is
	// an environment heuristic (it gates on agent-env detection), so an omitted
	// reason makes the same write succeed on one host and be refused on another.
	// Screen it here for a deterministic caller error instead.
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("secrets: set %q: reason is empty", name)
	}
	// The CLI loads the profile's declared set from a manifest; generate one
	// declaring just this name rather than letting it search the process cwd.
	// One resolved profile feeds both the manifest header and the argv below, so
	// the two cannot describe different profiles.
	profile := r.resolvedProfile()
	manifestPath, err := r.writeManifest(profile, []store.SecretDeclaration{{Name: name}})
	if err != nil {
		return err
	}
	// A transient input to the CLI, exactly as on the read path — remove it once
	// the write returns; the registry, not this file, is the durable source.
	defer func() { _ = os.Remove(manifestPath) }()
	args := r.setArgs(name, reason, manifestPath, profile)
	//nolint:gosec // G204: the SecretSpec write seam — spawns the operator-pinned
	// secretspec CLI (r.cli) with an argv slice passed straight to exec, so no
	// shell interprets any of it. Three variables ride it: name, validated
	// against the env-var-name grammar (ValidateName) above; and reason plus
	// manifestPath, each a single joined --flag=value token, so neither can
	// introduce a new argv element or be re-parsed as a flag. The value rides
	// stdin, never argv.
	cmd := exec.CommandContext(ctx, r.cli, args...)
	cmd.Stdin = strings.NewReader(value + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("secrets: set %q via %s: %w (%s)", name, r.cli, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Delete removes name's value from the provider. See the package/record note:
// with a manifest-driven resolver only declared names ever resolve, so removing
// the store declaration (store.DeleteSecretDeclaration) is the effective MVP
// delete; a provider-value hard-delete has no CLI verb upstream. This method is
// the seam for that write once a verb exists; today it validates the name and
// is a no-op success so the T7 handler can call one uniform surface.
func (r *SpecResolver) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return nil
}

// resolvedProfile is the SecretSpec profile every invocation runs under: the
// pinned profile, or defaultProfile when none is configured (an explicit
// WithProfile("")). One accessor for both paths so the generated manifest
// header and the profile the CLI/SDK acts under can never diverge.
func (r *SpecResolver) resolvedProfile() string {
	if r.profile == "" {
		return defaultProfile
	}
	return r.profile
}

// setArgs builds the argv for the write path (pure, so it is unit-testable
// without executing the binary). The value never appears here — it rides stdin.
// --file and --reason are global flags, accepted on either side of the `set`
// subcommand; both are emitted before it as the canonical, unambiguous
// position. The joined form binds each value to its flag, so a leading-dash
// reason is recorded as the reason rather than parsed as a flag.
//
// profile is the caller's resolvedProfile(), hence non-empty by construction,
// so --profile is emitted unconditionally: the CLI acts under exactly the
// profile the generated manifest declares instead of falling back to its own
// built-in default and agreeing only by coincidence.
func (r *SpecResolver) setArgs(name, reason, manifestPath, profile string) []string {
	args := []string{"--file=" + manifestPath, "--reason=" + reason, "set", name}
	if r.provider != "" {
		args = append(args, "--provider", r.provider)
	}
	return append(args, "--profile", profile)
}

// writeManifest renders the manifest for the current declared set and writes it
// to a unique 0600 temp file in the resolver's state dir, returning its path.
// A per-resolve file (not one shared path) so concurrent resolves never race on
// the write-to-Load interval — each gets its own manifest and reads exactly the
// snapshot it wrote. The state dir is created 0700 if absent. Server state,
// never repo state; the caller removes the file after Load.
func (r *SpecResolver) writeManifest(profile string, decls []store.SecretDeclaration) (string, error) {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return "", fmt.Errorf("secrets: create state dir: %w", err)
	}
	body, err := buildManifest(profile, decls)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(r.stateDir, "secretspec-*.toml")
	if err != nil {
		return "", fmt.Errorf("secrets: create manifest: %w", err)
	}
	// CreateTemp makes the file 0600 already; write the body and close.
	// Cleanup discards below are deliberate: on these paths the write/close
	// error is what the caller needs, and a failed remove of a temp file we
	// are already abandoning is not actionable.
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("secrets: write manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("secrets: close manifest: %w", err)
	}
	return f.Name(), nil
}
