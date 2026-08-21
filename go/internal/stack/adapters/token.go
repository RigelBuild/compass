//go:build unix

package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/stack"
	"github.com/RigelBuild/compass/go/internal/store"
)

// tokenFileName is the runner enrollment token's on-disk name under the state
// dir. It matches the credential the mint CLI's --token-out writes and the
// runner reads at enroll, so the supervisor-provisioned token and an
// operator-provisioned one are interchangeable on the same host.
const tokenFileName = "runner.token"

// tokenStore is the store surface the ensure logic needs: write a hash and
// resolve one. *store.Store satisfies it; a fake satisfies it in tests, so the
// mint/idempotence logic is unit-testable without a real database.
type tokenStore interface {
	runnerhub.TokenPutter
	runnerhub.TokenHashResolver
}

// TokenEnsurer is the real stack.TokenEnsurer: it ensures the runner enrollment
// token exists under the state dir and is registered in the store, minting it
// via internal/runnerhub on first bring-up and healing a store-forgotten token
// on subsequent ones — without ever rotating a token that is still valid.
//
// EnsureToken's signature carries no DSN, so the adapter holds it: the store is
// opened at the edge (EnsureToken) and the mint/idempotence logic runs against
// the store interface (ensure), keeping the DB-open thin and the logic testable.
type TokenEnsurer struct {
	databaseDSN string
}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.TokenEnsurer = (*TokenEnsurer)(nil)

// NewTokenEnsurer builds a TokenEnsurer over the store reachable at databaseDSN.
// The DSN is opened per EnsureToken call (provisioning is infrequent, so a
// long-lived pool is not worth the lifecycle), keeping the adapter a plain value.
func NewTokenEnsurer(databaseDSN string) *TokenEnsurer {
	return &TokenEnsurer{databaseDSN: databaseDSN}
}

// EnsureToken opens the real store over databaseDSN and delegates to ensure. It
// is the thin DB-open edge; all the file+mint logic lives in ensure so it can be
// unit-tested against a fake store.
func (e *TokenEnsurer) EnsureToken(ctx context.Context, stateDir, runnerID string) (string, error) {
	st, err := store.Open(ctx, e.databaseDSN)
	if err != nil {
		return "", fmt.Errorf("opening store for runner token: %w", err)
	}
	defer st.Close()
	return ensure(ctx, st, stateDir, runnerID)
}

// ensure makes the runner token file idempotent against the store, not just
// against file presence — mirroring the mint CLI's --token-out semantics
// (cmd/compass-mint-runner-token/main.go mintToFile):
//
//   - file present, token already registered: return it unchanged (the common
//     restart) — no mint, no rotation.
//   - file present, token NOT registered (the store was replaced): re-register
//     that exact token so the runner's on-disk credential keeps working — heal
//     without rotating, which a blind skip (stale hash) or a blind re-mint
//     (rotated credential) would both get wrong.
//   - file absent: mint a fresh token, write the file FIRST at 0600 atomically,
//     then commit its hash. File-before-store means a failed file write never
//     orphans a committed hash whose plaintext is gone; if the hash commit then
//     fails, the just-written file is removed so no dead credential is left.
//
// The plaintext is returned but never logged.
func ensure(ctx context.Context, st tokenStore, stateDir, runnerID string) (string, error) {
	path := filepath.Join(stateDir, tokenFileName)

	if existing, ok, err := readTokenFile(path); err != nil {
		return "", err
	} else if ok {
		registered, err := runnerhub.RunnerTokenRegistered(ctx, st, existing)
		if err != nil {
			return "", err
		}
		if registered {
			return existing, nil
		}
		// Store forgot this token (e.g. the database was replaced): re-register
		// the same token so the runner's credential keeps working; do not rotate.
		if err := runnerhub.StoreRunnerTokenHash(ctx, st, existing, runnerID); err != nil {
			return "", fmt.Errorf("re-registering existing runner token: %w", err)
		}
		return existing, nil
	}

	token, err := runnerhub.GenerateRunnerToken()
	if err != nil {
		return "", err
	}
	// File before store: a failed file write must not orphan a committed hash.
	if err := writeTokenFile(path, token); err != nil {
		return "", err
	}
	if err := runnerhub.StoreRunnerTokenHash(ctx, st, token, runnerID); err != nil {
		// The hash never committed, so the file holds a token the store would
		// reject. Remove it rather than leave a dead credential on disk.
		if rmErr := os.Remove(path); rmErr != nil {
			return "", errors.Join(
				fmt.Errorf("committing runner token hash: %w", err),
				fmt.Errorf("removing orphaned token file: %w", rmErr),
			)
		}
		return "", fmt.Errorf("committing runner token hash: %w", err)
	}
	return token, nil
}

// readTokenFile reads the token at path. ok is false when the file is absent, so
// the caller runs the mint path. A stat/read error other than not-exist
// surfaces, so the caller never silently mints over an unreadable existing file.
func readTokenFile(path string) (token string, ok bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is stateDir/runner.token, the file this adapter owns
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading existing runner token file %q: %w", path, err)
	}
	return string(b), true, nil
}

// writeTokenFile writes token to path at mode 0600, atomically: a temp file in
// the same directory (born 0600, chmod-pinned against umask) is written, synced,
// and renamed over path, so a reader never sees a partial credential and a crash
// mid-write leaves either the old file or the new one — never a truncated token.
// The token is written raw (no trailing newline), matching the mint CLI and the
// admin-token file convention the runner reads.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating temp runner token file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure below; a successful rename makes this a
	// no-op (the temp name no longer exists), so its error is not actionable.
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() // already failing; the chmod error is the real one
		cleanup()
		return fmt.Errorf("chmod 0600 runner token temp file: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close() // already failing; the write error is the real one
		cleanup()
		return fmt.Errorf("writing runner token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // already failing; the sync error is the real one
		cleanup()
		return fmt.Errorf("syncing runner token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing runner token temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("renaming runner token file into place at %q: %w", path, err)
	}
	return nil
}
