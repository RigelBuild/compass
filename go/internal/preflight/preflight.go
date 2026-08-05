package preflight

import (
	"context"
	"fmt"
	"strings"
)

// Deps is the set of external effects the preflight core is inverted over. Each
// func field is one genuine external effect; the real adapters (which shell out
// to podman/inspect the local image store/dial Postgres) are supplied at the T4
// wiring boundary, and unit tests supply stubs. The core imports none of those
// subsystems itself.
type Deps struct {
	// GOOS is the host operating system (runtime.GOOS at the wiring boundary).
	// Injected rather than read directly so the OS check is unit-testable.
	GOOS string
	// CurrentUID is the real uid of the running process (os.Getuid at the
	// wiring boundary). Injected so the uid check is unit-testable.
	CurrentUID int
	// ExpectedAgentUID is the uid the embedded runner requires (compass-runner's
	// defaultAgentUID, 1000). Injected by the caller so compass-runner's constant
	// stays the single source of truth and this package does not drift onto its
	// own literal.
	ExpectedAgentUID int
	// PodmanRootless probes that rootless podman is present and usable. A nil
	// error means it is available; a non-nil error explains why not.
	PodmanRootless func(ctx context.Context) error
	// ImagePresent probes that the given agent image ref is present in the local
	// container store. A nil error means present; a non-nil error means it is not
	// available locally (it is pulled from GHCR at first run).
	ImagePresent func(ctx context.Context, image string) error
	// DBReachable probes that Postgres is accepting connections on the DSN. A nil
	// error means reachable; a non-nil error means not yet.
	DBReachable func(ctx context.Context, dsn string) error
}

// Params is what the checks need from the caller: the resolved agent image ref
// and the Postgres DSN the embedded stack will open.
type Params struct {
	// AgentImage is the container image ref the embedded runner runs; its
	// presence in the local store is checked.
	AgentImage string
	// DatabaseDSN is the Postgres DSN the embedded stack's store of record opens;
	// its reachability is checked.
	DatabaseDSN string
}

// Result is the outcome of one preflight check. Name identifies the check; OK
// reports whether it passed; Detail carries the actionable failure copy when
// !OK (and may be empty when OK).
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Check names, stable for logs and error copy.
const (
	checkOS     = "os"
	checkUID    = "uid"
	checkPodman = "podman"
	checkImage  = "image"
	checkDB     = "database"
)

// Run executes every host precondition in order and returns one Result per
// check. It does NOT short-circuit: an operator should see every failing
// precondition at once, so all checks run even after an earlier failure.
func (d Deps) Run(ctx context.Context, p Params) []Result {
	results := make([]Result, 0, 5)

	// (1) OS is Linux (Global Constraint; devenv.nix:157-158).
	osRes := Result{Name: checkOS, OK: d.GOOS == "linux"}
	if !osRes.OK {
		osRes.Detail = "embedded mode is Linux-only, this host is " + d.GOOS
	}
	results = append(results, osRes)

	// (2) Running uid == the runner's required uid (mirrors
	// compass-runner/main.go:178-188; arbitrary-uid support is a GA follow-up,
	// §Decisions/OQ5).
	uidRes := Result{Name: checkUID, OK: d.CurrentUID == d.ExpectedAgentUID}
	if !uidRes.OK {
		uidRes.Detail = fmt.Sprintf(
			"the embedded runner requires uid %d, this process is uid %d: the "+
				"agent image bakes the agent user at that uid",
			d.ExpectedAgentUID, d.CurrentUID)
	}
	results = append(results, uidRes)

	// (3) Rootless podman present.
	podmanRes := Result{Name: checkPodman, OK: true}
	if err := d.PodmanRootless(ctx); err != nil {
		podmanRes.OK = false
		podmanRes.Detail = fmt.Sprintf("rootless podman is required: %v", err)
	}
	results = append(results, podmanRes)

	// (4) Agent image present in the local store. Reporting "not available
	// locally" until the GHCR publish lane lands is the correct behavior, not a
	// stub: the image is pulled from GHCR at first run.
	imageRes := Result{Name: checkImage, OK: true}
	if err := d.ImagePresent(ctx, p.AgentImage); err != nil {
		imageRes.OK = false
		imageRes.Detail = fmt.Sprintf(
			"agent image %s is not available locally; it is pulled from GHCR at "+
				"first run: %v", p.AgentImage, err)
	}
	results = append(results, imageRes)

	// (5) Postgres reachable.
	dbRes := Result{Name: checkDB, OK: true}
	if err := d.DBReachable(ctx, p.DatabaseDSN); err != nil {
		dbRes.OK = false
		dbRes.Detail = fmt.Sprintf(
			"the embedded database is not reachable at %s: %v",
			p.DatabaseDSN, err)
	}
	results = append(results, dbRes)

	return results
}

// Results is a preflight run's set of check outcomes.
type Results []Result

// Err aggregates the failed checks into one legible multi-line error, or returns
// nil when every check passed. Returning all failures at once lets the operator
// fix every unmet precondition in a single pass.
func (rs Results) Err() error {
	var failed []Result
	for _, r := range rs {
		if !r.OK {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("embedded-mode preflight failed:")
	for _, r := range failed {
		b.WriteString("\n  - ")
		b.WriteString(r.Detail)
	}
	return fmt.Errorf("%s", b.String())
}

// FirstFailure aggregates the failed checks of a plain []Result into one error,
// or nil if all passed. It is a convenience wrapper over Results.Err for callers
// holding the []Result returned by Run.
func FirstFailure(rs []Result) error {
	return Results(rs).Err()
}
