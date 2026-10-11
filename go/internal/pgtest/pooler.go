//go:build pgtest && unix

package pgtest

import (
	"net/url"
	"os"
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgshare"
)

// PoolerDSNEnvVar points the harness at a transaction-mode connection pooler
// (PgBouncer in CI) that fronts the COMPASS_TEST_DATABASE_DSN server and routes
// any database name to it.
const PoolerDSNEnvVar = "COMPASS_TEST_POOLER_DSN"

// RequirePoolerDSN returns a direct and a pooled DSN for one fresh database.
// It isolates by database because a pooler rejects the libpq options parameter
// that schema isolation uses. Both DSNs must be URL-form. It SKIPs when either
// variable is unset, or FAILs when COMPASS_REQUIRE_LIVE is set.
func RequirePoolerDSN(t *testing.T) (direct, pooled string) {
	t.Helper()
	base, pooler := os.Getenv(DSNEnvVar), os.Getenv(PoolerDSNEnvVar)
	if base == "" || pooler == "" {
		if os.Getenv(RequireLiveEnvVar) != "" {
			t.Fatalf(
				"%s is set but %s or %s is unset: the pooler leg must run",
				RequireLiveEnvVar,
				DSNEnvVar,
				PoolerDSNEnvVar,
			)
		}
		t.Skipf("no %s/%s; skipping transaction-mode pooler test", DSNEnvVar, PoolerDSNEnvVar)
	}
	name := pgshare.IsolatedDatabase(t, base)
	return withDatabase(t, base, name), withDatabase(t, pooler, name)
}

// withDatabase returns the URL-form dsn with its database replaced by name.
func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("pgtest: parse DSN for the pooler leg: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("pgtest: the pooler leg needs URL-form DSNs (postgres://...), not keyword/value form")
	}
	u.Path = "/" + name
	return u.String()
}
