package runtime

// The apple-container hermetic suite: the pure argv assembly, version parsing,
// and inspect classification that decide what the Runner shells out to Apple's
// `container` CLI with, and how it reads the answers back. No subprocess is
// spawned — these pin the serialized command shapes and the CLI-output
// contracts a real bug (a dropped flag, a leaked podman-ism, a misread
// "not found") would silently corrupt.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The full create the Runner assembles: caps, mounts, env, image and command.
// Env is emitted in sorted key order, so the expectation is exact rather than
// order-tolerant. The absence of a userns token is the load-bearing assertion —
// virtiofs translates identity at the boundary, and a ported
// --userns=keep-id:uid=,gid= would be rejected by a CLI that has no such flag.
func TestAppleCreateArgsAssemblesCreateWithoutUserns(t *testing.T) {
	args := appleCreateArgs(WorkloadSpec{
		Name:    "agent-1",
		Image:   "compass-agent:latest",
		UID:     1000,
		CapAdd:  []string{"NET_ADMIN"},
		Mounts:  []Mount{{HostPath: "/tmp/work", ContainerPath: "/work"}, {HostPath: "/tmp/cache", ContainerPath: "/src", ReadOnly: true}},
		Env:     map[string]string{"HOME": "/home/agent", "COMPASS_WORKDIR": "/work"},
		Command: []string{"sleep", "infinity"},
	})

	want := []string{
		"create", "--name", "agent-1",
		"--cap-add", "NET_ADMIN",
		"--volume", "/tmp/work:/work",
		"--volume", "/tmp/cache:/src:ro",
		"--env", "COMPASS_WORKDIR=/work",
		"--env", "HOME=/home/agent",
		"compass-agent:latest", "sleep", "infinity",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("appleCreateArgs = %q, want %q", args, want)
	}
	for _, arg := range args {
		if strings.Contains(arg, "userns") {
			t.Fatalf("appleCreateArgs = %q, want no userns token (virtiofs translates identity at the boundary)", args)
		}
	}
}

// A mount must not carry podman's :Z SELinux relabel: macOS has no SELinux, and
// the CLI would read the suffix as part of the container path.
func TestAppleMountArgOmitsSELinuxRelabel(t *testing.T) {
	tests := []struct {
		name  string
		mount Mount
		want  string
	}{
		{"read-write mount", Mount{HostPath: "/tmp/work", ContainerPath: "/work"}, "/tmp/work:/work"},
		{"read-only mount", Mount{HostPath: "/tmp/cache", ContainerPath: "/src", ReadOnly: true}, "/tmp/cache:/src:ro"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleMountArg(tc.mount); got != tc.want {
				t.Fatalf("appleMountArg(%+v) = %q, want %q", tc.mount, got, tc.want)
			}
		})
	}
}

// A one-shot exec with a stdin script: --interactive appears only when there is
// input to feed (so `sh -s` reads the script off the pipe, never the argv), and
// env rides --env rather than podman's -e.
func TestAppleExecArgsAssemblesOneShotExec(t *testing.T) {
	script := "nft -f -"
	spec := NewExecSpec("sh", "-s").AsUser("0").InDir("/work").WithStdin(script)
	spec.Env["LANG"] = "C"

	args := appleExecArgs(WorkloadID("ctr123"), spec)

	want := []string{
		"exec", "--interactive",
		"--user", "0",
		"--workdir", "/work",
		"--env", "LANG=C",
		"ctr123", "sh", "-s",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("appleExecArgs = %q, want %q", args, want)
	}
}

// Without stdin there is no --interactive: a one-shot probe must not hold a
// pipe open waiting for input that never arrives.
func TestAppleExecArgsOmitsInteractiveWithoutStdin(t *testing.T) {
	args := appleExecArgs(WorkloadID("ctr123"), NewExecSpec("true"))

	want := []string{"exec", "ctr123", "true"}
	if !slices.Equal(args, want) {
		t.Fatalf("appleExecArgs = %q, want %q", args, want)
	}
}

// The agent's streaming exec: always --interactive (stdin stays open for the
// process's life), never --tty.
func TestAppleExecStreamingArgsAssemblesInteractiveExec(t *testing.T) {
	spec := NewStreamingExecSpec("compass-agent").AsUser("1000").InDir("/work")
	spec.Env["HOME"] = "/home/agent"
	spec.Env["COMPASS_MODEL"] = "test-model"

	args := appleExecStreamingArgs(WorkloadID("ctr123"), spec)

	want := []string{
		"exec", "--interactive",
		"--user", "1000",
		"--workdir", "/work",
		"--env", "COMPASS_MODEL=test-model",
		"--env", "HOME=/home/agent",
		"ctr123", "compass-agent",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("appleExecStreamingArgs = %q, want %q", args, want)
	}
	if slices.Contains(args, "--tty") {
		t.Fatalf("appleExecStreamingArgs = %q, want no --tty (the agent is headless)", args)
	}
}

// The graceful stop grace must survive the Duration→whole-seconds conversion: a
// sub-second grace rounding to 0 would be an immediate kill with no grace.
func TestAppleStopArgsCarriesGrace(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    []string
	}{
		{"whole seconds pass through", 10 * time.Second, []string{"stop", "--time", "10", "ctr123"}},
		{"sub-second grace rounds up", 100 * time.Millisecond, []string{"stop", "--time", "1", "ctr123"}},
		{"negative grace clamps to zero", -5 * time.Second, []string{"stop", "--time", "0", "ctr123"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appleStopArgs(WorkloadID("ctr123"), tc.timeout)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("appleStopArgs(%v) = %q, want %q", tc.timeout, got, tc.want)
			}
		})
	}
}

// Remove must force: a still-running container is torn down, not refused.
func TestAppleRemoveArgsForces(t *testing.T) {
	want := []string{"rm", "--force", "ctr123"}
	if got := appleRemoveArgs(WorkloadID("ctr123")); !slices.Equal(got, want) {
		t.Fatalf("appleRemoveArgs = %q, want %q", got, want)
	}
}

// parseAppleContainerVersion + the floor comparison together decide the startup
// gate. The CLI prints prose ("container CLI version 1.1.0 (build: …)"), not a
// bare version, so a parser that assumed a leading number would reject every
// real host.
func TestParseAppleContainerVersion(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantMajor    int
		wantMinor    int
		wantParseErr bool
		wantRefused  bool
	}{
		{"real CLI prose is parsed", "container CLI version 1.1.0 (build: release, commit: 5973b9c)", 1, 1, false, false},
		{"bare version is parsed", "1.4.1", 1, 4, false, false},
		{"below floor 0.12 is refused", "container CLI version 0.12.3 (build: release)", 0, 12, false, true},
		{"at floor 1.0 is admitted", "container CLI version 1.0.0 (build: release)", 1, 0, false, false},
		{"garbage is a parse error", "not-a-version", 0, 0, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, err := parseAppleContainerVersion(tc.in)
			if tc.wantParseErr {
				if err == nil {
					t.Fatalf("parseAppleContainerVersion(%q) = (%d, %d, nil), want a parse error", tc.in, major, minor)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAppleContainerVersion(%q) = unexpected error %v", tc.in, err)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("parseAppleContainerVersion(%q) = (%d, %d), want (%d, %d)", tc.in, major, minor, tc.wantMajor, tc.wantMinor)
			}
			verdict := appleVersionFloorVerdict(tc.in)
			if (verdict != nil) != tc.wantRefused {
				t.Fatalf("appleVersionFloorVerdict(%q) = %v, want refused:%v", tc.in, verdict, tc.wantRefused)
			}
			if !tc.wantRefused {
				return
			}
			// A refusal an operator cannot act on is a bad refusal: it must
			// name the required floor and the version actually found.
			for _, want := range []string{"1.0", tc.in} {
				if !strings.Contains(verdict.Error(), want) {
					t.Fatalf("appleVersionFloorVerdict(%q) = %q, want it to name %q", tc.in, verdict, want)
				}
			}
		})
	}
}

// Exists reads absence off `container inspect`, which has no exists-style exit
// code contract. Only the CLI's own "not found" wording means absent; folding a
// generic failure into absence would let a caller recreate a container that
// already exists.
func TestClassifyInspectErr(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		stderr   string
		want     bool
		wantErr  bool
	}{
		{"exit 0 with empty stderr is present", 0, "", true, false},
		{"exit 1 with not-found is absent", 1, "Error: container not found: zznotexist\n", false, false},
		{"apiserver down is a failure, not absence", 1, "Error: XPC connection error: Connection invalid. Ensure container system service has been started with `container system start`.\n", false, true},
		{"a generic non-zero exit is a failure", 125, "Error: container system is not running\n", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyInspectErr(tc.exitCode, tc.stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classifyInspectErr(%d, %q) err = %v, wantErr %v", tc.exitCode, tc.stderr, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("classifyInspectErr(%d, %q) = %v, want %v", tc.exitCode, tc.stderr, got, tc.want)
			}
		})
	}
}

// SelectBackend routes the new name, and its refusal copy must name every
// accepted value — an operator who typo'd the backend learns the valid set from
// the error, not from the source.
func TestSelectBackendAppleContainer(t *testing.T) {
	rt, err := SelectBackend(BackendConfig{Backend: "apple-container"})
	if err != nil {
		t.Fatalf("SelectBackend(apple-container) err = %v, want nil", err)
	}
	if _, ok := rt.(*AppleContainerCLI); !ok {
		t.Fatalf("SelectBackend(apple-container) = %T, want *AppleContainerCLI", rt)
	}

	_, err = SelectBackend(BackendConfig{Backend: "bogus"})
	if err == nil {
		t.Fatal("SelectBackend(bogus) err = nil, want non-nil")
	}
	for _, accepted := range []string{"podman", "microvm", "apple-container"} {
		if !strings.Contains(err.Error(), accepted) {
			t.Fatalf("SelectBackend(bogus) err = %q, want it to name %q", err, accepted)
		}
	}
}

// MountLabel reports no label rather than failing: macOS has no SELinux, so the
// config-update relabel has nothing to target and must not be handed a bogus
// category.
func TestAppleContainerMountLabelIsEmpty(t *testing.T) {
	label, err := NewAppleContainerCLI(AppleContainerConfig{}).MountLabel(t.Context(), WorkloadID("ctr123"))
	if err != nil {
		t.Fatalf("MountLabel err = %v, want nil", err)
	}
	if label != "" {
		t.Fatalf("MountLabel = %q, want an empty label", label)
	}
}

// Resize must refuse loudly: a silent success would report a limit change that
// never happened, and this CLI exposes no resource-update verb at the floor.
func TestAppleContainerResizeIsReserved(t *testing.T) {
	err := NewAppleContainerCLI(AppleContainerConfig{}).Resize(t.Context(), WorkloadID("ctr123"), ResourceLimits{CPUShares: 512})
	if !errors.Is(err, ErrResizeNotImplemented) {
		t.Fatalf("Resize err = %v, want ErrResizeNotImplemented", err)
	}
}

// appleMissingContainer is the single place absence is read off a failed
// command, and it must hold for BOTH measured wordings: `inspect` says
// "container not found: <id>" while `rm`/`stop` say
// `notFound: "container with ID <id> not found"`. The apiserver-down case is
// the hazard the exit-code half of the guard exists for — it is also exit 1,
// so a code-only guard would call every container absent while the engine is
// down, and teardown would silently "succeed" against a live container.
func TestAppleMissingContainer(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		stderr   string
		want     bool
	}{
		{"inspect not-found", 1, "Error: container not found: zznotexist", true},
		{"rm not-found", 1, `Error: internalError: "failed to delete container" (cause: "notFound: "container with ID zznotexist not found"")`, true},
		{"stop not-found", 1, `Error: internalError: "failed to stop container" (cause: "notFound: "container with ID zznotexist not found"")`, true},
		{
			"apiserver down is not absence", 1,
			"Error: XPC connection error: Connection invalid. Ensure container system service has been started with `container system start`.",
			false,
		},
		{"unrelated exit 1 is not absence", 1, "Error: invalid argument --nope", false},
		{"exit 0 is not absence", 0, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleMissingContainer(tc.exitCode, tc.stderr); got != tc.want {
				t.Fatalf("appleMissingContainer(%d, %q) = %v, want %v", tc.exitCode, tc.stderr, got, tc.want)
			}
		})
	}
}

// appleStubCLI builds a CLI whose engine binary is a shell stub printing stderr
// and exiting 1, so the teardown verbs are exercised end to end through the
// real spawn seam without a `container` binary. Exit 1 is fixed: it is the code
// this CLI returns for both a missing container and an apiserver-down refusal,
// which is exactly what the tolerate-missing guard has to tell apart.
func appleStubCLI(t *testing.T, stderr string) *AppleContainerCLI {
	t.Helper()
	prog := filepath.Join(t.TempDir(), "container-stub.sh")
	script := "#!/bin/sh\ncat <<'EOF' >&2\n" + stderr + "\nEOF\nexit 1\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	return NewAppleContainerCLI(AppleContainerConfig{Program: prog, Timeout: 10 * time.Second})
}

// Teardown must be idempotent: AgentRuntime.Teardown propagates these errors,
// so a second teardown — or one after an operator removed the container by
// hand — would be a hard failure instead of a no-op. Both verbs exit 1 on a
// missing id on real hardware, unlike podman's `rm --force`.
func TestAppleStopRemoveTolerateMissingContainer(t *testing.T) {
	const rmMissing = `Error: internalError: "failed to delete container" (cause: "notFound: "container with ID gone not found"")`
	const stopMissing = `Error: internalError: "failed to stop container" (cause: "notFound: "container with ID gone not found"")`

	t.Run("remove of a missing container is nil", func(t *testing.T) {
		if err := appleStubCLI(t, rmMissing).Remove(t.Context(), WorkloadID("gone")); err != nil {
			t.Fatalf("Remove err = %v, want nil", err)
		}
	})

	t.Run("stop of a missing container is nil", func(t *testing.T) {
		if err := appleStubCLI(t, stopMissing).Stop(t.Context(), WorkloadID("gone"), 5*time.Second); err != nil {
			t.Fatalf("Stop err = %v, want nil", err)
		}
	})

	// The guard must stay narrow: an apiserver-down teardown is also exit 1,
	// and swallowing it would report a live container as torn down.
	t.Run("apiserver down still fails the teardown", func(t *testing.T) {
		const down = "Error: XPC connection error: Connection invalid. Ensure container system service has been started with `container system start`."
		if err := appleStubCLI(t, down).Remove(t.Context(), WorkloadID("live")); err == nil {
			t.Fatal("Remove err = nil, want the apiserver-down failure surfaced")
		}
		if err := appleStubCLI(t, down).Stop(t.Context(), WorkloadID("live"), 5*time.Second); err == nil {
			t.Fatal("Stop err = nil, want the apiserver-down failure surfaced")
		}
	})
}
