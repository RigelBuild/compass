//go:build unix

package stack

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// containerDSN is a well-formed DSN for the container path: it names the socket
// dir (the bind-mount target) and port the spec parses. dbname/sslmode ride
// along unused by the spec but present so the shape matches production.
const containerDSN = "host=/state/pgsock port=5432 dbname=compass sslmode=disable"

// TestExternalDatabaseSkipsPostgres pins the S4 external-DB opt-out: with
// ExternalDatabase set, Up starts NO postgres component (neither process nor
// container) and probes the given DSN as-is, so the cold sequence begins at the
// TLS anchor and nothing postgres-shaped is recorded for teardown.
func TestExternalDatabaseSkipsPostgres(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.ExternalDatabase = true
	cfg.DatabaseDSN = containerDSN
	// A container seam is wired but MUST NOT be touched on the external path.
	pc := newFakePostgresContainer(h.rec)
	h.deps.PostgresContainer = pc

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if s.attached {
		t.Fatal("cold Up should not be attached")
	}

	// No postgres start of either kind; the sequence is the cold chain minus the
	// postgres step.
	want := []string{
		"ensure-cert",
		"start compass-server",
		"ensure-token",
		"ensure-image",
		"start compass-runner",
	}
	got := filterEvents(h.rec.snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("external-db cold sequence:\n got  %v\n want %v", got, want)
	}
	if pc.started != 0 {
		t.Fatalf("PostgresContainer.Start called %d times on the external path, want 0", pc.started)
	}

	// The DBProber probe target is the caller's DSN, unchanged — the readiness
	// gate still runs, it just runs against the external database.
	if got := h.dbProber.lastProbedDSN(); got != containerDSN {
		t.Fatalf("DBProber probed %q, want the external DSN %q unchanged", got, containerDSN)
	}

	// Nothing postgres-shaped is recorded, so a down tears down only
	// server+runner. The record's postgres entry must be absent.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	for _, e := range rec.Entries {
		if e.Component == ComponentPostgres {
			t.Fatalf("external path recorded a postgres entry %+v, want none", e)
		}
	}
}

// TestContainerPathBuildsSpecAndRecordsContainerEntry pins the S4 container
// start path: with PostgresImage set, Up starts the container (not the wrapper
// process) via the PostgresContainer seam, and records a v2 container-kind pgid
// entry keyed by the stable per-state-dir name — the teardown identity a fresh
// down reconstructs.
func TestContainerPathBuildsSpecAndRecordsContainerEntry(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.PostgresImage = "docker.io/library/postgres:18@sha256:abc"
	cfg.DatabaseDSN = containerDSN
	pc := newFakePostgresContainer(h.rec)
	h.deps.PostgresContainer = pc

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}

	// The container path was taken, not the process path.
	want := []string{
		"start postgres-container",
		"ensure-cert",
		"start compass-server",
		"ensure-token",
		"ensure-image",
		"start compass-runner",
	}
	got := filterEvents(h.rec.snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("container cold sequence:\n got  %v\n want %v", got, want)
	}
	if pc.started != 1 {
		t.Fatalf("PostgresContainer.Start called %d times, want 1", pc.started)
	}

	// The spec the core built from Config: image passthrough, data dir under the
	// state dir, socket dir + port from the DSN, stop timeout pinned.
	spec := pc.spec()
	if spec.Image != cfg.PostgresImage {
		t.Errorf("spec.Image = %q, want %q", spec.Image, cfg.PostgresImage)
	}
	wantData := cfg.StateDir + "/postgres"
	if spec.DataDir != wantData {
		t.Errorf("spec.DataDir = %q, want %q", spec.DataDir, wantData)
	}
	if spec.SocketDir != "/state/pgsock" {
		t.Errorf("spec.SocketDir = %q, want /state/pgsock", spec.SocketDir)
	}
	if spec.Port != "5432" {
		t.Errorf("spec.Port = %q, want 5432", spec.Port)
	}
	if spec.StopTimeout != containerStopTimeout {
		t.Errorf("spec.StopTimeout = %v, want %v", spec.StopTimeout, containerStopTimeout)
	}
	if spec.Name != containerName(cfg.StateDir) {
		t.Errorf("spec.Name = %q, want the derived name %q", spec.Name, containerName(cfg.StateDir))
	}

	// The persisted teardown record carries the postgres child as a v2
	// container entry keyed by the same stable name — never a pgid.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	var pg *pgidEntry
	for i := range rec.Entries {
		if rec.Entries[i].Component == ComponentPostgres {
			pg = &rec.Entries[i]
		}
	}
	if pg == nil {
		t.Fatal("no postgres entry recorded on the container path")
	}
	if pg.Kind != entryContainer {
		t.Errorf("postgres entry kind = %v, want entryContainer", pg.Kind)
	}
	if pg.ContainerName != spec.Name {
		t.Errorf("recorded container name = %q, want %q", pg.ContainerName, spec.Name)
	}
	if pg.Pgid != 0 || pg.StartTime != 0 {
		t.Errorf("container entry carries pgid/starttime %d/%d, want zero (torn down by name)", pg.Pgid, pg.StartTime)
	}

	// In-process Down drains the container child in reverse order like any other,
	// via its Process handle (Signal → stop, Wait), and clears the record.
	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	assertPgidFileGone(t, cfg.StateDir)
	wantStops := []string{
		"signal compass-runner", "wait compass-runner",
		"signal compass-server", "wait compass-server",
		"signal postgres", "wait postgres",
	}
	if gotStops := stopEvents(h.rec.snapshot()); !reflect.DeepEqual(gotStops, wantStops) {
		t.Fatalf("container down stop order:\n got  %v\n want %v", gotStops, wantStops)
	}
}

// TestContainerStartFailureDrains pins the failure surface on the container
// path: a container that fails to launch surfaces the error and leaves no
// half-started stack (nothing after it was started).
func TestContainerStartFailureDrains(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.PostgresImage = "img:pinned"
	cfg.DatabaseDSN = containerDSN
	pc := newFakePostgresContainer(h.rec)
	pc.startErr = context.DeadlineExceeded
	h.deps.PostgresContainer = pc

	if _, err := Up(context.Background(), cfg, h.deps); err == nil {
		t.Fatal("Up() = nil, want the container start error")
	}
	// Nothing past postgres started.
	for _, e := range h.rec.snapshot() {
		if e == "start compass-server" {
			t.Fatal("compass-server started after a failed container postgres start")
		}
	}
}

// TestPostgresContainerSpecRejectsBadDSN pins that a DSN missing the fields the
// container bind-mount + socket contract need is a hard error, not a run against
// a half-formed spec.
func TestPostgresContainerSpecRejectsBadDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"missing host", "port=5432 dbname=compass", "socket directory"},
		{"missing port", "host=/state/pgsock dbname=compass", "port"},
		{"malformed", "not-a-dsn", "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := postgresContainerSpec(Config{StateDir: "/state", DatabaseDSN: tt.dsn})
			if err == nil {
				t.Fatalf("postgresContainerSpec(%q) = nil error, want a rejection", tt.dsn)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tt.want)
			}
		})
	}
}

// TestContainerNameDeterministicPerStateDir pins the S4 stable-name contract:
// the name is a pure function of the state dir (so a fresh down reconstructs it)
// and distinct across state dirs (so concurrent stacks never collide).
func TestContainerNameDeterministicPerStateDir(t *testing.T) {
	a1 := containerName("/state/a")
	a2 := containerName("/state/a")
	b := containerName("/state/b")
	if a1 != a2 {
		t.Fatalf("containerName not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("containerName collides across state dirs: both %q", a1)
	}
	if !strings.HasPrefix(a1, "compass-postgres-") {
		t.Fatalf("containerName %q missing the compass-postgres- prefix", a1)
	}
	// A trailing slash is the same cluster (filepath.Clean), so the name is
	// stable across the cosmetic difference a caller might pass.
	if containerName("/state/a/") != a1 {
		t.Fatalf("containerName not clean-normalized: %q vs %q", containerName("/state/a/"), a1)
	}
}
