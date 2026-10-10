//go:build unix

package runner

// Startup validation of the Runner's runtime dir against the AF_UNIX sun_path
// budget (RIG-1443): a misconfigured deployment must refuse to boot with a
// legible message instead of failing at the first provision with a bare EINVAL.
import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
)

func TestAgentHostConfigCarriesRunnerSettings(t *testing.T) {
	cfg := RunnerConfig{
		RuntimeDir:    "/run/compass",
		AgentModel:    "claude-opus-4",
		AgentBatching: "on",
	}
	got := agentHostConfig(cfg, "runner-1")
	if got.AgentBatching != "on" {
		t.Fatalf("AgentBatching = %q, want on", got.AgentBatching)
	}
	if got.AgentModel != cfg.AgentModel || got.RuntimeDir != cfg.RuntimeDir || got.RunnerID != "runner-1" {
		t.Fatalf("agentHostConfig = %+v, lost existing Runner config", got)
	}

	got = agentHostConfig(RunnerConfig{}, "runner-1")
	if got.AgentBatching != "" {
		t.Fatalf("unset AgentBatching = %q, want empty", got.AgentBatching)
	}
}

func TestRunDialWithRetryTransientThenSuccess(t *testing.T) {
	cfg := RunnerConfig{RunnerID: "runner-1", RuntimeDir: t.TempDir(), Engine: newPipeRuntime()}
	want := &ServerLink{}
	var attempts int
	var delays []time.Duration
	got, err := runDialWithRetry(context.Background(), cfg, discardLoggerRunner(), func(context.Context, RunnerConfig) (*ServerLink, error) {
		attempts++
		if attempts < 3 {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("server restarting"))
		}
		return want, nil
	}, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return true
	})
	if err != nil || got != want {
		t.Fatalf("runDialWithRetry = (%p, %v), want successful link %p", got, err, want)
	}
	if attempts != 3 || !slices.Equal(delays, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("attempts/delays = %d/%v, want 3/[1s 2s]", attempts, delays)
	}
}

func TestRunDialWithRetryExhaustsBoundedAttempts(t *testing.T) {
	cfg := RunnerConfig{RunnerID: "runner-1", RuntimeDir: t.TempDir(), Engine: newPipeRuntime()}
	logs := newCaptureLog()
	var attempts int
	var delays []time.Duration
	_, err := runDialWithRetry(context.Background(), cfg, logs.logger(), func(context.Context, RunnerConfig) (*ServerLink, error) {
		attempts++
		return nil, errors.New("last dial failure")
	}, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "5 attempts") || !strings.Contains(err.Error(), "last dial failure") {
		t.Fatalf("runDialWithRetry error = %v, want five attempts and last error", err)
	}
	if attempts != 5 || !slices.Equal(delays, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}) {
		t.Fatalf("attempts/delays = %d/%v, want 5/[1s 2s 4s 8s]", attempts, delays)
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		line := logs.recvLine(t)
		if line.level != slog.LevelWarn || line.attrs["attempt"] != strconv.Itoa(attempt) || line.attrs["max_attempts"] != "5" || !strings.Contains(line.attrs["error"], "last dial failure") {
			t.Fatalf("warning %d = %+v, want Warn with attempt/max/error", attempt, line)
		}
	}
}

func TestRunDialWithRetryFailsFastOnNonRetryableErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "unauthenticated", err: connect.NewError(connect.CodeUnauthenticated, errors.New("bad token"))},
		{name: "permission denied", err: connect.NewError(connect.CodePermissionDenied, errors.New("denied"))},
		{name: "invalid argument", err: connect.NewError(connect.CodeInvalidArgument, errors.New("bad request"))},
		{name: "failed precondition", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("not ready"))},
		{name: "dial configuration", err: &dialConfigurationError{err: errors.New("bad client config")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := RunnerConfig{RunnerID: "runner-1", RuntimeDir: t.TempDir(), Engine: newPipeRuntime()}
			attempts := 0
			delays := 0
			wantErr := fmt.Errorf("enrolling: %w", tc.err)
			_, err := runDialWithRetry(context.Background(), cfg, discardLoggerRunner(), func(context.Context, RunnerConfig) (*ServerLink, error) {
				attempts++
				return nil, wantErr
			}, func(context.Context, time.Duration) bool {
				delays++
				return true
			})
			if !strings.Contains(err.Error(), "not retryable") {
				t.Fatalf("runDialWithRetry error = %v, want explicit not-retryable wording", err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("runDialWithRetry error = %v, want original error %v", err, tc.err)
			}
			if attempts != 1 || delays != 0 {
				t.Fatalf("attempts/delays = %d/%d, want 1/0", attempts, delays)
			}
		})
	}
}

func TestRunDialWithRetryTimeoutsStalledAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
		defer cancel()
		cfg := RunnerConfig{RunnerID: "runner-1", RuntimeDir: t.TempDir(), Engine: newPipeRuntime()}
		var attempts int
		var delays []time.Duration
		got, err := runDialWithRetry(ctx, cfg, discardLoggerRunner(), func(ctx context.Context, _ RunnerConfig) (*ServerLink, error) {
			attempts++
			<-ctx.Done()
			return nil, ctx.Err()
		}, func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			return true
		})
		if got != nil || err == nil {
			t.Fatalf("runDialWithRetry = (%p, %v), want a timeout error", got, err)
		}
		if attempts != 5 || !slices.Equal(delays, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}) {
			t.Fatalf("attempts/delays = %d/%v, want 5/[1s 2s 4s 8s]", attempts, delays)
		}
	})
}

func TestRunDialWithRetryCancelDuringBackoffReturnsCtxErr(t *testing.T) {
	cfg := RunnerConfig{RunnerID: "runner-1", RuntimeDir: t.TempDir(), Engine: newPipeRuntime()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runDialWithRetry(ctx, cfg, discardLoggerRunner(), func(context.Context, RunnerConfig) (*ServerLink, error) {
			return nil, errors.New("temporary failure")
		}, func(ctx context.Context, _ time.Duration) bool {
			close(entered)
			<-ctx.Done()
			return false
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-timeAfter():
		t.Fatal("retry backoff did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runDialWithRetry after cancellation = %v, want context.Canceled", err)
		}
	case <-timeAfter():
		t.Fatal("runDialWithRetry did not return after backoff cancellation")
	}
}

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

// This test binds agentAccountIDWidth to the minting site. validAccountID now
// rejects any incoming id that is not exactly this wide, so a request can never
// widen the socket path past what the budget cleared; but nothing binds the
// constant to store.newID's actual output, so if the minting site ever changed
// width the two would drift silently. TestSunPathMaxMatchesTheKernel binds
// sunPathMax to reality; this binds the width. Pin it against the actual
// construction rather than against the literal 32 — store.newID is
// hex.EncodeToString of a 16-byte array, and asserting `32 == 32` would restate
// the constant instead of testing it.
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
		Token:      StaticToken("t"),
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

func TestRunRejectsEmptyRunnerIDWithoutTokenFile(t *testing.T) {
	err := Run(context.Background(), RunnerConfig{
		ServerAddr: "http://127.0.0.1:1",
		Token:      StaticToken("t"),
		Engine:     newPipeRuntime(),
		RuntimeDir: t.TempDir(),
	}, nil, discardLoggerRunner())
	if err == nil || !strings.Contains(err.Error(), "runner id") {
		t.Fatalf("Run with empty RunnerID and static token = %v, want runner-id configuration error", err)
	}
}

func TestRunAcceptsEmptyRunnerIDWithTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(tokenWithExpiry(t, time.Now().Add(time.Minute))), 0o600); err != nil {
		t.Fatalf("write projected token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, RunnerConfig{
		ServerAddr: "http://127.0.0.1:1",
		Token:      NewFileToken(path, discardLoggerRunner(), time.Now),
		Engine:     newPipeRuntime(),
		RuntimeDir: shortRuntimeDir(t),
	}, nil, discardLoggerRunner())
	if err != nil {
		t.Fatalf("Run with empty RunnerID and projected token = %v, want clean cancellation", err)
	}
}
