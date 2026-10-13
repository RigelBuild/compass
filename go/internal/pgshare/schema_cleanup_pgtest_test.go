//go:build (pgtest || podman) && unix

package pgshare

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestIsolatedSchemaDSNCleanupDropsSchema(t *testing.T) {
	// AcquireDSN may already carry a search_path `options`; IsolatedSchemaDSN rejects a second one.
	root, err := url.Parse(AcquireDSN(t))
	if err != nil {
		t.Fatalf("parse acquired DSN: %v", err)
	}
	q := root.Query()
	q.Del("options")
	root.RawQuery = q.Encode()
	rootDSN := root.String()
	var isolatedDSN string
	t.Run("isolated schema", func(t *testing.T) {
		isolatedDSN = IsolatedSchemaDSN(t, rootDSN)
	})

	u, err := url.Parse(isolatedDSN)
	if err != nil {
		t.Fatalf("parse isolated DSN: %v", err)
	}
	options, err := url.QueryUnescape(u.Query().Get("options"))
	if err != nil {
		t.Fatalf("decode search_path options: %v", err)
	}
	const prefix = "-c search_path="
	if !strings.HasPrefix(options, prefix) {
		t.Fatalf("search_path options %q do not start with %q", options, prefix)
	}
	schema := strings.TrimPrefix(options, prefix)
	conn, err := pgx.Connect(t.Context(), rootDSN)
	if err != nil {
		t.Fatalf("connect to inspect schema cleanup: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	var count int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM information_schema.schemata WHERE schema_name = $1", schema).Scan(&count); err != nil {
		t.Fatalf("query schema cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("schema %q remains after subtest cleanup; found %d rows", schema, count)
	}
}
