//go:build unix

package runner

// The AgentEnv producer seam: the Runner's only chance to tell the agent process
// who it is, where it runs, and which model to use. Everything here is read by
// the agent at boot (packages/compass-agent/src/cli.ts) or enforced by podman at
// exec time, so a dropped or misspelled key is a silently broken session rather
// than a build error. Each test names the contract a plausible bug would break.
//
// The argv these specs assemble into is not re-tested here — execStreamingArgs
// is covered in internal/runtime/podman_test.go. This file pins the spec.

import (
	"testing"
)

// The workdir must reach the agent by BOTH mechanisms, from the one Workdir
// value: podman's --workdir (the process's real cwd) and COMPASS_WORKDIR (what
// the agent reads to locate the checkout). They are independent paths that must
// agree; a bug that set only one leaves the agent's idea of the checkout
// disagreeing with its actual cwd, which is how the agent ended up running in
// $HOME instead of the repo.
func TestExecSpecCarriesWorkdirAsBothCwdAndEnv(t *testing.T) {
	const workdir = "/srv/checkout"
	spec := AgentEnv{UID: 1000, HomeDir: "/home/coder", Workdir: workdir}.execSpec()

	if spec.Workdir == nil {
		t.Fatal("spec.Workdir is nil; the exec would run in the image's default dir, not the checkout")
	}
	if *spec.Workdir != workdir {
		t.Fatalf("spec.Workdir = %q, want %q (podman --workdir is the agent's cwd)", *spec.Workdir, workdir)
	}
	if got := spec.Env["COMPASS_WORKDIR"]; got != workdir {
		t.Fatalf("COMPASS_WORKDIR = %q, want %q (the agent reads this to locate the checkout)", got, workdir)
	}
}

// HOME is the scoped home dir: without it the agent cannot find its provider
// seed and fails at boot. A bug that dropped the key, or sourced it from the
// workdir instead, would redden here.
func TestExecSpecSetsScopedHome(t *testing.T) {
	spec := AgentEnv{UID: 1000, HomeDir: "/home/coder", Workdir: "/srv/checkout"}.execSpec()

	if got := spec.Env["HOME"]; got != "/home/coder" {
		t.Fatalf("HOME = %q, want /home/coder (the agent's provider seed lives under it)", got)
	}
}

// COMPASS_MODEL is exported only when a selector is configured. An empty Model
// must leave the key ABSENT, not mapped to "": the agent's resolveModelSelector
// treats an absent var as "use the SDK default", so exporting a blank value
// would force it to special-case an empty string. A non-empty Model is
// exported verbatim — the selector was permanently inert before this seam
// existed, so this is the assertion that proves it is wired at all.
func TestExecSpecExportsModelOnlyWhenConfigured(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		want    string
		present bool
	}{
		{name: "empty model omits the key entirely", model: "", present: false},
		{name: "configured model is exported verbatim", model: "claude-opus-4", want: "claude-opus-4", present: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := AgentEnv{UID: 1000, HomeDir: "/home/coder", Workdir: "/srv/checkout", Model: tc.model}.execSpec()

			got, ok := spec.Env["COMPASS_MODEL"]
			if ok != tc.present {
				t.Fatalf("COMPASS_MODEL present = %v (value %q), want present = %v", ok, got, tc.present)
			}
			if ok && got != tc.want {
				t.Fatalf("COMPASS_MODEL = %q, want %q", got, tc.want)
			}
		})
	}
}

// SECURITY-LOAD-BEARING. The container is created with --cap-add NET_ADMIN
// (runtime/agent.go:212) so its root entrypoint can arm the nft egress
// firewall. Podman strips a container's ambient capabilities from an exec ONLY
// when --user is passed explicitly: an exec with no --user inherits NET_ADMIN,
// and an agent holding NET_ADMIN can `nft flush ruleset` and disarm its own
// egress firewall — precisely the integrity invariant runtime/egress.go:6-10
// states ("Never run the agent as container-root"). spec.User being set to the
// workspace uid is what keeps the agent unprivileged.
//
// The two uids rule out a hardcoded value passing by coincidence, and the
// decimal rendering is the contract podman's --user parses.
func TestExecSpecRunsAsWorkspaceUIDNotContainerRoot(t *testing.T) {
	tests := []struct {
		name string
		uid  uint32
		want string
	}{
		{name: "conventional agent uid", uid: 1000, want: "1000"},
		{name: "high uid renders decimal", uid: 65534, want: "65534"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := AgentEnv{UID: tc.uid, HomeDir: "/home/coder", Workdir: "/srv/checkout"}.execSpec()

			if spec.User == nil {
				t.Fatal("spec.User is nil: the exec omits --user, so it runs as container-root and inherits NET_ADMIN — it could flush its own egress ruleset")
			}
			if *spec.User != tc.want {
				t.Fatalf("spec.User = %q, want %q (the unprivileged workspace uid)", *spec.User, tc.want)
			}
		})
	}
}

// The S3 + session-identity vars are the session-log-persistence carriage: each
// COMPASS_S3_* / COMPASS_SESSION_ID key is exported exactly when its AgentEnv
// field is set, and an AgentEnv with none of them set exports none of the keys —
// so the dev path (empty S3Endpoint) leaves the agent's persistence off rather
// than handing it blank credentials it would have to special-case. A bug that
// always exported a field, or misspelled a key, is a silently mis-persisted (or
// unreachable) session log rather than a build error.
func TestExecSpecExportsS3AndSessionVarsOnlyWhenSet(t *testing.T) {
	// Every S3/session field populated: each maps to its contracted key verbatim.
	full := AgentEnv{
		UID: 1000, HomeDir: "/home/coder", Workdir: "/srv/checkout",
		S3Endpoint:        "https://s3.example:9000",
		S3Bucket:          "compass-sessions",
		S3AccessKeyID:     "AKIA",
		S3SecretAccessKey: "secret",
		SessionID:         "sess-7",
		SessionEpoch:      "3",
	}.execSpec()
	for _, want := range []struct{ key, value string }{
		{"COMPASS_S3_ENDPOINT", "https://s3.example:9000"},
		{"COMPASS_S3_BUCKET", "compass-sessions"},
		{"COMPASS_S3_ACCESS_KEY_ID", "AKIA"},
		{"COMPASS_S3_SECRET_ACCESS_KEY", "secret"},
		{"COMPASS_SESSION_ID", "sess-7"},
		{"COMPASS_SESSION_EPOCH", "3"},
	} {
		if got, ok := full.Env[want.key]; !ok || got != want.value {
			t.Fatalf("%s = %q (present %v), want %q", want.key, got, ok, want.value)
		}
	}

	// No S3/session fields set: none of the keys appear. Model still absent too,
	// but these are the keys under test.
	bare := AgentEnv{UID: 1000, HomeDir: "/home/coder", Workdir: "/srv/checkout"}.execSpec()
	for _, key := range []string{
		"COMPASS_S3_ENDPOINT", "COMPASS_S3_BUCKET", "COMPASS_S3_ACCESS_KEY_ID",
		"COMPASS_S3_SECRET_ACCESS_KEY", "COMPASS_SESSION_ID", "COMPASS_SESSION_EPOCH",
		"COMPASS_SESSION_RESUME",
	} {
		if got, ok := bare.Env[key]; ok {
			t.Fatalf("%s exported as %q with no field set; want the key absent", key, got)
		}
	}
}

// COMPASS_SESSION_RESUME is present exactly when Resume is true, carrying "1";
// false must leave the key absent so the agent takes its fresh-start path rather
// than special-casing a blank value. (Var name is a T7 stated assumption — the
// design record leaves it unspecified; T8 is its only setter.)
func TestExecSpecExportsResumeOnlyWhenTrue(t *testing.T) {
	if got, ok := (AgentEnv{Resume: true}).execSpec().Env["COMPASS_SESSION_RESUME"]; !ok || got != "1" {
		t.Fatalf("COMPASS_SESSION_RESUME = %q (present %v), want \"1\" when Resume is true", got, ok)
	}
	if got, ok := (AgentEnv{Resume: false}).execSpec().Env["COMPASS_SESSION_RESUME"]; ok {
		t.Fatalf("COMPASS_SESSION_RESUME exported as %q with Resume false; want the key absent", got)
	}
}

// COMPASS_SESSION_EPOCH is present exactly when SessionEpoch is set. The fresh
// path (T7) leaves it empty for the agent to derive; only a resume sets it, so
// an empty epoch must not export a blank key.
func TestExecSpecExportsEpochOnlyWhenSet(t *testing.T) {
	if got, ok := (AgentEnv{SessionEpoch: "5"}).execSpec().Env["COMPASS_SESSION_EPOCH"]; !ok || got != "5" {
		t.Fatalf("COMPASS_SESSION_EPOCH = %q (present %v), want \"5\"", got, ok)
	}
	if got, ok := (AgentEnv{SessionEpoch: ""}).execSpec().Env["COMPASS_SESSION_EPOCH"]; ok {
		t.Fatalf("COMPASS_SESSION_EPOCH exported as %q with empty SessionEpoch; want the key absent", got)
	}
}
