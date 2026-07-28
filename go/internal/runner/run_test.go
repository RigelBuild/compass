//go:build unix

package runner

// Startup validation of the Runner's runtime dir against the AF_UNIX sun_path
// budget (SEA-1443): a misconfigured deployment must refuse to boot with a
// legible message instead of failing at the first provision with a bare EINVAL.

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
)

// socketTailWidth measures the fixed tail the Runner appends to the runtime dir
// — /containers/ + compass-agent- + a 32-char account id + /agent.sock — by
// differencing the joined path against a known-length dir, using the same
// filepath.Join the production path construction uses (host.go serveSocket) so
// the test cannot drift from it. A runtime dir of exactly (sunPathMax - tail)
// bytes is the last acceptable one.
func socketTailWidth() int {
	const probe = "/x"
	widest := filepath.Join(probe, agentSocketDir,
		AgentContainerNamePrefix+strings.Repeat("0", agentAccountIDWidth), agentSocketFile)
	return len(widest) - len(probe)
}

// dirOfLen builds an absolute runtime dir of exactly n bytes.
func dirOfLen(t *testing.T, n int) string {
	t.Helper()
	if n < 1 {
		t.Fatalf("dirOfLen(%d): need at least 1 byte for the leading slash", n)
	}
	return "/" + strings.Repeat("d", n-1)
}

func TestValidateRuntimeDir(t *testing.T) {
	tail := socketTailWidth()
	atBudget := sunPathMax - tail

	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{name: "production default boots", dir: "/run/compass"},
		{name: "exactly at budget is accepted", dir: dirOfLen(t, atBudget)},
		{name: "one byte over budget is refused", dir: dirOfLen(t, atBudget+1), wantErr: true},
		{name: "relative dir is refused", dir: "relative/runtime", wantErr: true},
		{name: "empty dir is refused", dir: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimeDir(tc.dir)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateRuntimeDir(%d-byte dir) = %v, want nil", len(tc.dir), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateRuntimeDir(%d-byte dir) = nil, want an over-budget error", len(tc.dir))
			}
			// The message must let an operator turn the right knob. The budget
			// error names the runtime dir's own length and the per-platform
			// cap; the non-absolute error is a different (earlier) rejection,
			// so only assert the numbers on the over-budget cases.
			msg := err.Error()
			if !filepath.IsAbs(tc.dir) {
				if !strings.Contains(msg, "absolute") {
					t.Errorf("non-absolute dir %q rejected without saying why: %s", tc.dir, msg)
				}
				return
			}
			if !strings.Contains(msg, strconv.Itoa(len(tc.dir))) {
				t.Errorf("error does not name the runtime dir length %d: %s", len(tc.dir), msg)
			}
			if !strings.Contains(msg, strconv.Itoa(sunPathMax)) {
				t.Errorf("error does not name the AF_UNIX budget %d: %s", sunPathMax, msg)
			}
		})
	}
}

// The budget must come from the platform's own sockaddr_un, never a literal:
// 107 on Linux, 103 on darwin and the BSDs (104-byte array). Asserted per-GOOS
// rather than as a two-value allow-set, so an off-by-N that happens to land on
// the other platform's value is still a failure here.
func TestSunPathMaxIsPlatformDerived(t *testing.T) {
	var want int
	switch goruntime.GOOS {
	case "linux":
		want = 107
	case "darwin", "freebsd", "openbsd", "netbsd":
		want = 103
	default:
		t.Skipf("no known len(sun_path) for GOOS %q", goruntime.GOOS)
	}
	if sunPathMax != want {
		t.Fatalf("sunPathMax = %d, want %d on %s (len(sun_path)-1)", sunPathMax, want, goruntime.GOOS)
	}
}

// agentAccountIDWidth is the one budget operand with no check anywhere: the
// Runner never verifies an incoming id is that wide (spec.go validAccountID
// constrains the shape only), so if the minting site ever changes width, the
// budget silently clears a path the kernel will refuse. The comment below and
// TestSunPathMaxMatchesTheKernel bind sunPathMax to reality; nothing binds
// this. Pin it against the actual construction rather than against the literal
// 32 — store.newID is hex.EncodeToString of a 16-byte array, and asserting
// `32 == 32` would restate the constant instead of testing it.
func TestAgentAccountIDWidthMatchesTheMintingSite(t *testing.T) {
	var minted [16]byte
	if got := len(hex.EncodeToString(minted[:])); got != agentAccountIDWidth {
		t.Fatalf("agentAccountIDWidth = %d, but store.newID mints %d chars (hex of %d bytes); the socket-path budget in validateRuntimeDir is derived from this width and is now wrong",
			agentAccountIDWidth, got, len(minted))
	}
}

// The budget model is only worth anything if the kernel agrees with it. The
// tests above derive their expectations from the same constants as the code
// under test, so shrinking the model (id width, name prefix, socket dir) leaves
// them green while the real budget is wrong. This one binds: a real worst-case
// shaped path of exactly sunPathMax bytes must bind, and one byte more must not.
func TestSunPathMaxMatchesTheKernel(t *testing.T) {
	//nolint:usetesting // t.TempDir embeds the test name, which is exactly what puts a path over the sun_path cap — the bug this test exists to measure. A short fixed root is required.
	root, err := os.MkdirTemp("", "rb")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) }) // cleanup-only discard: nothing actionable at test teardown.

	tail := socketTailWidth()
	// The runtime dir that puts the worst-case socket path exactly at the cap.
	atBudget := sunPathMax - tail
	if atBudget <= len(root)+1 {
		t.Skipf("TMPDIR root %q (%d bytes) is too deep to build a %d-byte worst-case path",
			root, len(root), sunPathMax)
	}

	// bindAt builds the production path shape under a runtime dir padded to
	// dirLen bytes, and reports whether the kernel accepted the bind.
	bindAt := func(t *testing.T, dirLen int) (string, error) {
		t.Helper()
		dir := root + "/" + strings.Repeat("p", dirLen-len(root)-1)
		path := filepath.Join(dir, agentSocketDir,
			AgentContainerNamePrefix+strings.Repeat("0", agentAccountIDWidth), agentSocketFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return path, err
		}
		_ = ln.Close() // cleanup-only discard: the bind already succeeded, which is the assertion.
		return path, nil
	}

	path, err := bindAt(t, atBudget)
	if err != nil {
		t.Fatalf("binding a %d-byte path (the modelled cap) failed: %v", len(path), err)
	}
	t.Logf("bound %d-byte path (sunPathMax = %d)", len(path), sunPathMax)

	over, err := bindAt(t, atBudget+1)
	if err == nil {
		t.Fatalf("binding a %d-byte path succeeded, want failure past the %d-byte cap", len(over), sunPathMax)
	}
	t.Logf("refused %d-byte path: %v", len(over), err)
}

// Run must actually consult the budget: without this, deleting the
// validateRuntimeDir call from Run leaves the suite green, because every other
// test calls the validator directly. An over-budget RuntimeDir has to be
// refused before Run reaches the network, so the error is the runtime-dir one
// rather than a dial or context failure.
func TestRunRejectsOverBudgetRuntimeDirBeforeDialing(t *testing.T) {
	tail := socketTailWidth()
	dir := dirOfLen(t, sunPathMax-tail+1)

	// Cancelled up front: if Run ever got as far as Dial, the error would be a
	// context/dial failure, which is exactly what this test distinguishes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: "http://127.0.0.1:1", // never reached
		Token:      "t",
		Engine:     newPipeRuntime(),
		RuntimeDir: dir,
	}
	err := Run(ctx, cfg, nil, discardLoggerRunner())
	if err == nil {
		t.Fatalf("Run with a %d-byte runtime dir = nil, want the over-budget error", len(dir))
	}
	if !strings.Contains(err.Error(), "runtime dir") {
		t.Fatalf("Run error = %v, want the runtime-dir budget error (not a dial/context failure)", err)
	}
}
