//go:build unix

// Command compass-mint-runner-token is the Runner-token provisioning CLI: it
// mints a per-Runner bearer token against the store and emits the plaintext
// exactly once — to stdout by default, or to a 0600 file with --token-out — for
// out-of-band delivery to the Runner host.
//
// This is the operator provisioning step OQ7 specifies — a dedicated
// Runner-subject mint path, NOT the account door's IssueToken, and deliberately
// not an automated RPC (compass-0.6/design.md:1308-1312). The store keeps only
// the SHA-256 hash; the plaintext is unrecoverable after this command exits, so
// capture it now: `compass-mint-runner-token --runner-id r1 > runner.token`
// (then chmod 0600), or `--token-out runner.token` to write+chmod it directly.
// With --token-out the mint is skip-if-present (idempotent across restarts);
// stdout always mints. All logs go to stderr, so stdout carries the token and
// nothing else. The mint logic lives in internal/runnerhub; this binary is a
// thin wrapper that assembles config from flags/env, mirroring cmd/compass-server
// and cmd/compass-runner.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// version is the build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		slog.Error("compass-mint-runner-token exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	runnerID := flag.String("runner-id", "",
		"The Runner id to mint a token for (the token's subject). Required.")
	databaseFlag := flag.String("database", "",
		"Postgres DSN for the store of record (e.g. postgres://user:pass@host/compass). "+
			"Defaults to $COMPASS_DATABASE_DSN.")
	tokenOut := flag.String("token-out", "",
		"Write the token to this file (0600, atomically) instead of stdout. With "+
			"it set, the mint is idempotent: if the file exists and its token is "+
			"already registered in the store, it no-ops; if the file exists but "+
			"the store no longer knows the token (e.g. the database was replaced), "+
			"it re-registers that same token without rotating it. Pass --force to "+
			"mint a fresh token and overwrite. Without it, the token goes to "+
			"stdout exactly once (capture it: `mint --runner-id r1 > runner.token`).")
	force := flag.Bool("force", false,
		"With --token-out, mint a fresh token and overwrite the file even when it "+
			"already exists. Ignored without --token-out (stdout always mints).")
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		// Version is the command's own output, so it goes to stdout. A caller
		// capturing a token never passes --version, so there is no ambiguity
		// with the token on stdout.
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	// Logs to stderr so stdout carries only the minted token, keeping the
	// `mint > runner.token` capture clean.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *runnerID == "" {
		return errors.New("a runner id is required: pass --runner-id")
	}

	dsn, err := resolveDSN(*databaseFlag)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	if *tokenOut != "" {
		return mintToFile(ctx, st, *runnerID, *tokenOut, *force)
	}
	return mintAndPrint(ctx, st, *runnerID, os.Stdout)
}

// resolveDSN mirrors compass-server's precedence exactly (main.go: flag wins,
// else $COMPASS_DATABASE_DSN, else error), so the two binaries read the same
// store from one DSN source with no drift.
func resolveDSN(flagVal string) (string, error) {
	dsn := flagVal
	if dsn == "" {
		dsn = os.Getenv("COMPASS_DATABASE_DSN")
	}
	if dsn == "" {
		return "", errors.New("a Postgres DSN is required: pass --database or set $COMPASS_DATABASE_DSN")
	}
	return dsn, nil
}

// mintAndPrint mints a Runner-subject token via runnerhub.MintRunnerToken and
// writes the plaintext exactly once to out with a trailing newline — nothing
// else on stdout — so a redirect captures a clean single-line credential. The
// store persists only the SHA-256 hash; this printed plaintext is the sole copy.
func mintAndPrint(ctx context.Context, st runnerhub.TokenPutter, runnerID string, out io.Writer) error {
	token, err := runnerhub.MintRunnerToken(ctx, st, runnerID)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, token); err != nil {
		return fmt.Errorf("writing minted token: %w", err)
	}
	return nil
}

// tokenStore is the store surface the file sink needs: write a hash and resolve
// one. store.Store satisfies it; a fake satisfies it in tests.
type tokenStore interface {
	runnerhub.TokenPutter
	runnerhub.TokenHashResolver
}

// mintToFile makes the --token-out file idempotent against the store, not just
// against file presence. It writes the raw token (no trailing newline, matching
// the admin-token file convention in server/network_door.go) at 0600 atomically.
//
//   - force, or no existing file: mint a fresh token, write the file FIRST, then
//     commit its hash. Ordering the file before the store commit means a failed
//     file write never leaves an orphaned hash whose plaintext is gone; if the
//     hash commit then fails, the just-written file is removed so no file is left
//     holding a token the store never accepted.
//   - existing file, token already registered: no-op (the common restart).
//   - existing file, token NOT registered (the store was replaced): re-register
//     that exact token so the Runner's on-disk credential keeps working — heal
//     without rotating, which a blind skip (stale hash) or a blind re-mint
//     (rotated credential) would both get wrong.
func mintToFile(ctx context.Context, st tokenStore, runnerID, path string, force bool) error {
	if !force && fileExists(path) {
		existing, err := os.ReadFile(path) //nolint:gosec // path is the operator-provided --token-out flag, the file this binary owns and just checked exists
		if err != nil {
			return fmt.Errorf("reading existing token file %q: %w", path, err)
		}
		token := string(existing)
		registered, err := runnerhub.RunnerTokenRegistered(ctx, st, token)
		if err != nil {
			return err
		}
		if registered {
			slog.Info("runner token file already present and registered; skipping (pass --force to re-mint)",
				"token_out", path, "runner_id", runnerID)
			return nil
		}
		if err := runnerhub.StoreRunnerTokenHash(ctx, st, token, runnerID); err != nil {
			return fmt.Errorf("re-registering existing runner token: %w", err)
		}
		slog.Info("existing runner token re-registered in store (store had no record); file left unchanged",
			"token_out", path, "runner_id", runnerID)
		return nil
	}

	token, err := runnerhub.GenerateRunnerToken()
	if err != nil {
		return err
	}
	// File before store: a failed file write must not orphan a committed hash.
	if err := writeTokenFile(path, token); err != nil {
		return err
	}
	if err := runnerhub.StoreRunnerTokenHash(ctx, st, token, runnerID); err != nil {
		// The hash never committed, so the file holds a token the store would
		// reject. Remove it rather than leave a dead credential on disk.
		_ = os.Remove(path)
		return err
	}
	slog.Info("minted runner token", "token_out", path, "runner_id", runnerID)
	return nil
}

// writeTokenFile writes token to path at mode 0600, atomically: a temp file in
// the same directory (born 0600, chmod-pinned against umask) is written, synced,
// and renamed over path, so a reader never sees a partial credential and a crash
// mid-write leaves either the old file or the new one — never a truncated token.
// The token is written raw (no trailing newline), matching the admin-token file.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating temp token file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod 0600 token temp file: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing runner token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing runner token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing token temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming token file into place at %q: %w", path, err)
	}
	return nil
}

// fileExists reports whether path names an existing file. A stat error other
// than not-exist counts as "not present", so the mint path runs and surfaces
// the real error rather than silently skipping on an ambiguous stat.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
