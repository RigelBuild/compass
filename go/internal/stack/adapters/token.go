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
// on subsequent ones. It keeps a token valid for runnerID and rotates one that
// is live under another subject.
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

// ensure reuses a registered token, re-registers one missing from the store, and mints when absent or bound elsewhere.
// It commits the token hash only after atomically writing the 0600 file; the returned plaintext is never logged.
func ensure(ctx context.Context, st tokenStore, stateDir, runnerID string) (string, error) {
	path := filepath.Join(stateDir, tokenFileName)

	if existing, ok, err := readTokenFile(path); err != nil {
		return "", err
	} else if ok {
		state, _, err := runnerhub.RunnerTokenStatus(ctx, st, existing, runnerID)
		if err != nil {
			return "", err
		}
		switch state {
		case runnerhub.TokenRegistered, runnerhub.TokenRevoked:
			return existing, nil
		case runnerhub.TokenUnknown:
			// Store forgot this token (e.g. the database was replaced): re-register
			// the same token so the runner's credential keeps working; do not rotate.
			if err := runnerhub.StoreRunnerTokenHash(ctx, st, existing, runnerID); err != nil {
				return "", fmt.Errorf("re-registering existing runner token: %w", err)
			}
			return existing, nil
		case runnerhub.TokenOtherSubject:
			// Falls through to a fresh mint below.
		}
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

// writeTokenFile atomically replaces path with a synced 0600 file, so readers never see a partial credential.
// Store the raw token without a newline to match the runner's file convention.
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
