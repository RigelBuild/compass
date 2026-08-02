//go:build pgtest

package store

// Config-bundle POOL round-trips (SEA-1624 T1), pgtest gate — these exercise
// PutAgentConfig/CurrentAgentConfig against a real Postgres via the shared
// harness (newTestStore/mustUser only exist under this tag). The door
// validation + canonical-hash logic is proven purely in the default-gate
// sibling; here we prove the singleton persistence contract: a Put→Current
// round trip, ErrNotFound on an empty store, version-stable idempotent re-put,
// and current-only retention (a second Put replaces, leaving exactly one row).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"
	"time"
)

// mkBundle gzip-tars a set of (name, content) files for a pool test.
func mkBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(content)),
			ModTime:  time.Unix(1000, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestPutAgentConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	b := mkBundle(t, map[string]string{
		"skills/review/SKILL.md": "# review",
		"mcp/linear.json":        `{"url":"https://x"}`,
	})
	version, err := s.PutAgentConfig(ctx, actor.ID, b)
	if err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	if version == "" {
		t.Fatal("PutAgentConfig returned empty version")
	}

	gotVersion, gotBundle, err := s.CurrentAgentConfig(ctx)
	if err != nil {
		t.Fatalf("CurrentAgentConfig: %v", err)
	}
	if gotVersion != version {
		t.Errorf("version mismatch: put %s, got %s", version, gotVersion)
	}
	if !bytes.Equal(gotBundle, b) {
		t.Error("round-tripped bundle bytes differ from what was put")
	}
}

func TestCurrentAgentConfigEmptyNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, _, err := s.CurrentAgentConfig(ctx)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on empty store, got %v", err)
	}
}

func TestPutAgentConfigEmptyActorInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	b := mkBundle(t, map[string]string{"skills/review/SKILL.md": "# review"})
	_, err := s.PutAgentConfig(ctx, "", b)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for empty actor, got %v", err)
	}
}

func TestPutAgentConfigIdempotentVersionStable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	files := map[string]string{"skills/review/SKILL.md": "# review"}
	// Two independently-built tarballs of identical CONTENT (fresh gzip framing
	// each time) must yield the same version — the content-hash contract.
	v1, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, files))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	v2, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, files))
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("idempotent re-put changed version: %s vs %s", v1, v2)
	}
	if n := countConfigRows(t, s); n != 1 {
		t.Fatalf("re-put left %d rows, want 1", n)
	}
}

func TestPutAgentConfigCurrentOnlyRetention(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	v1, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{"skills/a/f": "v1"}))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	v2, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{"skills/a/f": "v2"}))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if v1 == v2 {
		t.Fatal("distinct content produced the same version")
	}
	if n := countConfigRows(t, s); n != 1 {
		t.Fatalf("current-only retention violated: %d rows, want 1", n)
	}
	gotVersion, _, err := s.CurrentAgentConfig(ctx)
	if err != nil {
		t.Fatalf("CurrentAgentConfig: %v", err)
	}
	if gotVersion != v2 {
		t.Errorf("current bundle is %s, want the superseding %s", gotVersion, v2)
	}
}

func countConfigRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM agent_config_bundle").Scan(&n); err != nil {
		t.Fatalf("count agent_config_bundle: %v", err)
	}
	return n
}
