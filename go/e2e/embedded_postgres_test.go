//go:build podman

package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestEmbeddedPostgresSmoke is the one e2e leg that runs the product-default
// postgres: the stack spawns and owns it, so this is the only dedicated e2e
// coverage of the supervisor-owns-postgres lifecycle and its pgid-record
// teardown. Every other leg attaches to the suite postgres instead.
func TestEmbeddedPostgresSmoke(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	ctx := context.Background()
	f := NewFixture(ctx, t, WithEmbeddedPostgres())

	// The stack recorded the postgres it spawned, and that group is alive.
	pgid := recordedPostgresPgid(t, f.StateDir())
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("recorded postgres group %d is not alive after Up: %v", pgid, err)
	}

	health, err := f.Stack().Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.State != stack.StatusReady {
		t.Fatalf("stack state = %v (%s), want Ready", health.State, health.Detail)
	}

	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().GetServerInfo(rctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("authed GetServerInfo over the embedded postgres: %v", err)
	}
	if resp.Msg.GetVersion() == "" {
		t.Fatal("GetServerInfo returned an empty version")
	}

	if err := f.Stack().Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	post, err := f.Stack().Health(ctx)
	if err != nil {
		t.Fatalf("Health after Down: %v", err)
	}
	if post.State == stack.StatusReady {
		t.Fatalf("stack still Ready after Down (%s)", post.Detail)
	}
	// A clean Down reaps the postgres group and drops the teardown record.
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("postgres group %d after Down: kill(0) = %v, want ESRCH", pgid, err)
	}
	if _, err := os.Stat(filepath.Join(f.StateDir(), "stack.pgids")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pgid record after a clean Down: stat = %v, want not-exist", err)
	}
}

// fatalRecorder is a testing.TB whose Fatalf records the call and stops the
// goroutine, so a test can observe a helper's fatal without failing itself.
type fatalRecorder struct {
	testing.TB
	fataled bool
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(string, ...any) {
	r.fataled = true
	runtime.Goexit()
}

// TestWithEmbeddedPostgresRejectsExternalDSN has no podmanUsable skip, so it runs
// even where the stack cannot: it checks only the option guard, which must
// refuse a fixture asked for both postgres arms.
func TestWithEmbeddedPostgresRejectsExternalDSN(t *testing.T) {
	for name, opts := range map[string][]fixtureOption{
		"conflict":      {WithEmbeddedPostgres(), withExternalDatabase("host=/tmp dbname=x")},
		"embedded only": {WithEmbeddedPostgres()},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &fatalRecorder{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				buildFixtureConfig(rec, opts)
			}()
			<-done
			if want := name == "conflict"; rec.fataled != want {
				t.Fatalf("buildFixtureConfig fataled = %v, want %v", rec.fataled, want)
			}
		})
	}
}

// recordedPostgresPgid reads the postgres process-group id from the stack's
// pgid record ("proc postgres <pgid> <start>" in the v2 format).
func recordedPostgresPgid(t *testing.T, stateDir string) int {
	t.Helper()
	path := filepath.Join(stateDir, "stack.pgids")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pgid record: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) == 4 && f[0] == "proc" && f[1] == "postgres" {
			pgid, err := strconv.Atoi(f[2])
			if err != nil || pgid <= 1 {
				t.Fatalf("pgid record postgres entry %q has a bad pgid", line)
			}
			return pgid
		}
	}
	t.Fatalf("pgid record %q has no proc postgres entry; the stack did not spawn its own postgres:\n%s", path, raw)
	return 0
}
