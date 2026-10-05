package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // secrets read seam: spawns the secretspec CLI by name (G204 site justified below)
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/RigelBuild/compass/go/internal/store"
)

// manifestProject is the single SecretSpec project name Compass resolves under.
// The Server owns one project, one profile; the registry, not a repo manifest,
// is the source of truth (Decision 2, no repo-committed secretspec.toml).
const manifestProject = "compass"

// defaultProfile is the SecretSpec profile Compass resolves under when none is
// configured — the Server owns one project, one profile.
const defaultProfile = "default"

// defaultCLI is the SecretSpec binary both read paths spawn, resolved off PATH.
// hostcheck.SecretSpecFloor names the same binary and guards the stack preflight.
const defaultCLI = "secretspec"

// cliWaitDelay matches the other CLI spawns (runtime/clispawn.go).
const cliWaitDelay = 10 * time.Second

// reportStatusResolved is the SecretSpec report status meaning the provider
// holds a value for a declared secret. The report's other statuses
// ("missing_required", "missing_optional") both mean no value is present, so
// the status path tests for this one rather than enumerating the misses — a new
// miss status upstream then reads as unset, which is the safe direction.
const reportStatusResolved = "resolved"

// declarations is the read surface the Resolver needs from the store: the whole
// declared set. store.Store satisfies it. An interface (not the concrete
// *store.Store) so the pure resolve logic is unit-testable with a fake, without
// a Postgres harness.
type declarations interface {
	DeclaredSecrets(ctx context.Context) ([]store.SecretDeclaration, error)
}

// Resolver resolves the declared secret set to values through SecretSpec.
// Resolve reads the whole registry (inject-all: no per-agent filter in the MVP
// — a names filter is the future grants seam).
type Resolver interface {
	// Resolve resolves every declared secret to its value via SecretSpec,
	// returning a ResolvedSecret per declaration with a content-hash Version.
	// reason is recorded in the SecretSpec audit log. A required secret missing
	// from the provider is an error (the store declared it, so the provider must
	// hold it).
	Resolve(ctx context.Context, reason string) ([]ResolvedSecret, error)
	// Statuses reports, per declared secret, whether the provider currently
	// holds a value for it — names + set/unset, NEVER a value. It is the
	// value-free counterpart to Resolve, for the caller that needs to
	// distinguish "declared" from "populated" without reading any value.
	//
	// Unlike Resolve it does NOT fail when a declared secret is unpopulated: an
	// absent required value is reported as IsSet=false, not an error. That is
	// the whole point — a server secret's row is self-declared at boot while its
	// value is populated separately, so declared-but-unset is a normal state
	// that must be observable rather than a resolve fault.
	Statuses(ctx context.Context, reason string) ([]SecretStatus, error)
}

// SpecResolver is the SecretSpec-backed Resolver. It reads the names registry
// from the store, generates a SecretSpec manifest under its own state dir, and
// resolves values from the configured provider. The resolver process (the
// Server) is the only place SecretSpec runs — containers receive resolved
// values, never provider access.
type SpecResolver struct {
	store    declarations
	provider string // SecretSpec provider URI (e.g. "keyring://"); "" = the CLI's default chain
	profile  string // SecretSpec profile (e.g. "default")
	// stateDir is where the generated manifest is written — the Server's own
	// state directory, never repo state. The CLI reads it through --file.
	stateDir string
	cli      string // the secretspec binary, by name or path
}

// SpecOption configures a SpecResolver.
type SpecOption func(*SpecResolver)

// WithProvider pins the SecretSpec provider URI (e.g. "keyring://",
// "onepassword://Production"). Empty uses the CLI's default provider chain.
func WithProvider(uri string) SpecOption { return func(r *SpecResolver) { r.provider = uri } }

// WithProfile pins the SecretSpec profile. Empty resolves to defaultProfile
// (see resolvedProfile), never the CLI built-in default.
func WithProfile(profile string) SpecOption { return func(r *SpecResolver) { r.profile = profile } }

// WithCLI pins the secretspec CLI binary the read paths spawn (default: "secretspec" on PATH).
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
// without a store or the CLI.
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
		// a missing one fails the export, surfaced loudly.
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
	// the CLI resolves the same <profile>.
	profile := r.resolvedProfile()
	manifestPath, err := r.writeManifest(profile, decls)
	if err != nil {
		return nil, err
	}
	// The manifest is a transient input to the CLI — a per-resolve temp file, so
	// concurrent resolves never share one path (each gets its own). Remove it
	// once resolved; the registry, not this file, is the durable source.
	defer func() { _ = os.Remove(manifestPath) }()

	stdout, err := r.run(ctx, r.cliArgs("export", manifestPath, profile, reason, "--format=json"))
	if err != nil {
		// stdout and stderr both stay out of the error: a provider parse error
		// quotes the offending source line, which can hold a value.
		return nil, fmt.Errorf("secrets: resolve via %s: %w", r.cli, err)
	}
	// A nil entry is a JSON null: the name is present with no usable value.
	var resolved map[string]*string
	if err := json.Unmarshal(stdout, &resolved); err != nil {
		return nil, fmt.Errorf("secrets: resolve via %s: %w", r.cli, redactDecodeError(err))
	}
	if resolved == nil {
		return nil, fmt.Errorf("secrets: resolve via %s: output is not a JSON object", r.cli)
	}

	out := make([]ResolvedSecret, 0, len(decls))
	for _, d := range decls {
		value, ok := resolved[d.Name]
		if !ok {
			// The manifest declared it required, so the export would have failed
			// on a genuine miss; a declared name absent here means the CLI dropped
			// it — surface it rather than emit an empty value.
			return nil, fmt.Errorf("secrets: declared secret %q not in resolver output", d.Name)
		}
		if value == nil {
			return nil, fmt.Errorf("secrets: declared secret %q resolved with no value", d.Name)
		}
		out = append(out, ResolvedSecret{
			Name:     d.Name,
			Value:    *value,
			Version:  Version(*value),
			Delivery: deliveryFromStore(d.Delivery),
			Kind:     kindFromStore(d.Kind),
			Host:     d.Host,
			Provider: d.Provider,
		})
	}
	return out, nil
}

// Statuses reports each declared secret's value-free set/unset state, reading
// the SecretSpec RESOLUTION REPORT (`secretspec check --json`) rather than
// resolving values. reason is recorded in the SecretSpec audit log exactly as on
// the resolve path. An empty registry reports an empty set with no provider call.
//
// The report, not an export, is the primitive this needs, for two reasons:
//
//   - buildManifest declares every name required = true, so an export fails
//     WHOLESALE the moment ONE declared secret is unpopulated. That is the
//     common state for a server secret — the row is self-declared at boot, the
//     value populated separately — so an export-based status path would error
//     out in precisely the case it exists to describe. The report lists a
//     missing required secret as a per-secret status instead.
//   - The report never carries a value. An export would pull every deployment
//     secret's VALUE into this process just to answer a names-and-flags question.
//
// `check --json` exits 1 when a required secret is missing but still prints
// the full report, so a parseable report is success whatever the exit code.
// A provider fault (provider unreachable, bad manifest, reason policy refused)
// prints no report and is returned as an error, never flattened into an
// all-unset report: a broken provider must not read as an unprovisioned one.
// The report is a SecretSpec 0.20+ surface; hostcheck.SecretSpecFloor keeps it.
func (r *SpecResolver) Statuses(ctx context.Context, reason string) ([]SecretStatus, error) {
	decls, err := r.store.DeclaredSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("secrets: read registry: %w", err)
	}
	if len(decls) == 0 {
		return nil, nil
	}
	profile := r.resolvedProfile()
	manifestPath, err := r.writeManifest(profile, decls)
	if err != nil {
		return nil, err
	}
	// Same transient-input discipline as Resolve: a per-call temp manifest, so
	// concurrent callers never share one path. The remove error is discarded
	// deliberately — the registry, not this file, is the durable source, so a
	// failed unlink of a temp file is not actionable.
	defer func() { _ = os.Remove(manifestPath) }()

	stdout, runErr := r.run(ctx, r.cliArgs("check", manifestPath, profile, reason, "--json"))
	var report struct {
		Secrets []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil || report.Secrets == nil {
		// No report means the CLI failed before resolving (a provider fault):
		// return that, never an all-unset set. The report is value-free, so
		// its decode error is safe to wrap.
		if runErr != nil {
			return nil, fmt.Errorf("secrets: report via %s: %w", r.cli, runErr)
		}
		if err != nil {
			return nil, fmt.Errorf("secrets: report via %s: %w", r.cli, err)
		}
		return nil, fmt.Errorf("secrets: report via %s: no secrets list in the report", r.cli)
	}

	// Index the report by name: it is a slice, and the declared set is the
	// authority on WHICH names to answer for, so this reports one status per
	// declaration rather than whatever order the provider enumerated.
	set := make(map[string]bool, len(report.Secrets))
	for _, s := range report.Secrets {
		set[s.Name] = s.Status == reportStatusResolved
	}
	out := make([]SecretStatus, 0, len(decls))
	for _, d := range decls {
		// A declared name absent from the report is unset, not an error: the
		// report enumerates the manifest we just wrote from these same
		// declarations, so an absence means the provider holds nothing for it —
		// which is exactly what IsSet=false says.
		out = append(out, SecretStatus{Name: d.Name, IsSet: set[d.Name]})
	}
	return out, nil
}

// resolvedProfile is the SecretSpec profile every invocation runs under: the
// pinned profile, or defaultProfile when none is configured (an explicit
// WithProfile("")). One accessor for both paths so the generated manifest
// header and the profile the CLI acts under can never diverge.
func (r *SpecResolver) resolvedProfile() string {
	if r.profile == "" {
		return defaultProfile
	}
	return r.profile
}

// cliArgs builds the argv for one secretspec subcommand. Every flag uses the
// joined --flag=value form: the two-token form parses a value that starts with
// "-" (a reason, say) as the next flag and exits 2. No secret value is in argv.
func (r *SpecResolver) cliArgs(sub, manifestPath, profile, reason string, extra ...string) []string {
	args := []string{sub, "--file=" + manifestPath}
	if r.provider != "" {
		args = append(args, "--provider="+r.provider)
	}
	args = append(args, "--profile="+profile, "--reason="+reason)
	return append(args, extra...)
}

// run spawns the CLI and returns its stdout, which may hold secret values, so
// callers must never log it or put it in an error. stderr is discarded for the
// same reason: diagnostics quote provider source lines.
func (r *SpecResolver) run(ctx context.Context, args []string) ([]byte, error) {
	//nolint:gosec // G204: r.cli is the operator-pinned secretspec binary and args is an argv slice handed straight to exec (no shell); each variable is one joined --flag=value token, so none can add an argv element.
	cmd := exec.CommandContext(ctx, r.cli, args...)
	// An ambient scope would silently retarget the export; flags cover the rest.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "SECRETSPEC_SCOPE=")
	})
	// Provider helpers (op, gpg) inherit the stdout pipe; bound the wait after cancel.
	cmd.WaitDelay = cliWaitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.Bytes(), err
}

// redactDecodeError rewrites a JSON decode error of the export without its
// text: a syntax error quotes the offending byte and a type error the literal,
// either of which can be part of a secret value. Offsets and kinds are kept.
func redactDecodeError(err error) error {
	if syn, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("export output is not valid JSON (offset %d)", syn.Offset)
	}
	if typ, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return fmt.Errorf("export output has a non-string value (offset %d)", typ.Offset)
	}
	return errors.New("export output is not a JSON object of strings")
}

// writeManifest renders the manifest for the current declared set and writes it
// to a unique 0600 temp file in the resolver's state dir, returning its path.
// A per-call file (not one shared path) so concurrent resolves never race on
// the write-to-read interval — each gets its own manifest and reads exactly the
// snapshot it wrote. The state dir is created 0700 if absent. Server state,
// never repo state; the caller removes the file after the CLI returns.
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
