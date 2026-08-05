package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const testAgentUID = 1000

// okDeps returns a Deps whose every injected effect passes, for the given host
// GOOS and uid. Tests override individual fields to drive one failure at a time.
func okDeps(goos string, uid int) Deps {
	return Deps{
		GOOS:             goos,
		CurrentUID:       uid,
		ExpectedAgentUID: testAgentUID,
		PodmanRootless:   func(context.Context) error { return nil },
		ImagePresent:     func(context.Context, string) error { return nil },
		DBReachable:      func(context.Context, string) error { return nil },
	}
}

var testParams = Params{
	AgentImage:  "ghcr.io/sealedsecurity/compass-agent:latest",
	DatabaseDSN: "postgres://localhost:5432/compass",
}

// resultByName finds a check result by its Name.
func resultByName(t *testing.T, rs []Result, name string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %v", name, rs)
	return Result{}
}

func TestRunAllPass(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("linux", testAgentUID).Run(ctx, testParams)

	if len(rs) != 5 {
		t.Fatalf("want 5 results, got %d: %v", len(rs), rs)
	}
	for _, r := range rs {
		if !r.OK {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
	if err := FirstFailure(rs); err != nil {
		t.Errorf("want nil error when all pass, got %v", err)
	}
}

func TestRunWrongGOOS(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("darwin", testAgentUID).Run(ctx, testParams)

	got := resultByName(t, rs, checkOS)
	if got.OK {
		t.Fatal("os check should fail on darwin")
	}
	for _, tok := range []string{"Linux", "darwin"} {
		if !strings.Contains(got.Detail, tok) {
			t.Errorf("os detail %q missing token %q", got.Detail, tok)
		}
	}
	assertErrContains(t, FirstFailure(rs), "darwin")
}

func TestRunWrongUID(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("linux", 501).Run(ctx, testParams)

	got := resultByName(t, rs, checkUID)
	if got.OK {
		t.Fatal("uid check should fail when uid != expected")
	}
	// The copy must name BOTH uids: the required and the actual.
	for _, tok := range []string{"1000", "501"} {
		if !strings.Contains(got.Detail, tok) {
			t.Errorf("uid detail %q missing uid token %q", got.Detail, tok)
		}
	}
	assertErrContains(t, FirstFailure(rs), "501")
}

func TestRunPodmanProbeFails(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux", testAgentUID)
	d.PodmanRootless = func(context.Context) error {
		return errors.New("podman socket not found")
	}
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkPodman)
	if got.OK {
		t.Fatal("podman check should fail when probe errors")
	}
	if !strings.Contains(got.Detail, "podman socket not found") {
		t.Errorf("podman detail %q missing probe error", got.Detail)
	}
	assertErrContains(t, FirstFailure(rs), "rootless podman")
}

func TestRunImageAbsent(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux", testAgentUID)
	d.ImagePresent = func(context.Context, string) error {
		return errors.New("no such image")
	}
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkImage)
	if got.OK {
		t.Fatal("image check should fail when image absent")
	}
	if !strings.Contains(got.Detail, testParams.AgentImage) {
		t.Errorf("image detail %q missing image ref %q", got.Detail, testParams.AgentImage)
	}
	assertErrContains(t, FirstFailure(rs), testParams.AgentImage)
}

func TestRunDBUnreachable(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux", testAgentUID)
	d.DBReachable = func(context.Context, string) error {
		return errors.New("connection refused")
	}
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkDB)
	if got.OK {
		t.Fatal("db check should fail when unreachable")
	}
	if !strings.Contains(got.Detail, "connection refused") {
		t.Errorf("db detail %q missing probe error", got.Detail)
	}
	assertErrContains(t, FirstFailure(rs), "connection refused")
}

// TestRunMultipleFailures asserts Run does not short-circuit: every failing
// precondition is reported and appears in the aggregated error.
func TestRunMultipleFailures(t *testing.T) {
	ctx := context.Background()
	d := Deps{
		GOOS:             "windows",
		CurrentUID:       0,
		ExpectedAgentUID: testAgentUID,
		PodmanRootless:   func(context.Context) error { return errors.New("no podman") },
		ImagePresent:     func(context.Context, string) error { return errors.New("no image") },
		DBReachable:      func(context.Context, string) error { return errors.New("no db") },
	}
	rs := d.Run(ctx, testParams)

	for _, name := range []string{checkOS, checkUID, checkPodman, checkImage, checkDB} {
		if resultByName(t, rs, name).OK {
			t.Errorf("check %q should have failed", name)
		}
	}

	err := FirstFailure(rs)
	if err == nil {
		t.Fatal("want aggregated error for multiple failures, got nil")
	}
	// Every failing check's load-bearing token must appear in one error.
	for _, tok := range []string{
		"windows", "1000", "no podman", testParams.AgentImage, "no db",
	} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("aggregated error %q missing token %q", err.Error(), tok)
		}
	}
}

// TestResultsErrMethod exercises the named-slice Err method directly.
func TestResultsErrMethod(t *testing.T) {
	if err := (Results{{Name: checkOS, OK: true}}).Err(); err != nil {
		t.Errorf("want nil for all-OK, got %v", err)
	}
	err := (Results{{Name: checkDB, OK: false, Detail: "boom"}}).Err()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("want error containing detail, got %v", err)
	}
}

func assertErrContains(t *testing.T, err error, tok string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", tok)
	}
	if !strings.Contains(err.Error(), tok) {
		t.Errorf("error %q missing token %q", err.Error(), tok)
	}
}
