//go:build unix

package runner

// NewConfigSpecBuilder + BuildSpec: the constructor rejects incomplete defaults
// (no image, missing checkout/home dir), and BuildSpec fills the workspace
// layout from the defaults and derives the container name as
// prefix+agent_account_id. Every test names the contract a plausible bug would
// break: a misconfigured Runner must fail at startup, not first provision.

import (
	"strings"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// goodDefaults is a complete, valid SpecDefaults.
func goodDefaults() SpecDefaults {
	return SpecDefaults{
		Image:       "compass-agent:latest",
		Egress:      runtime.MustAllowEgress("github.com"),
		CheckoutDir: "/work/repo",
		HomeDir:     "/home/agent",
		UID:         1000,
		NamePrefix:  "compass-agent-",
	}
}

// The constructor rejects incomplete defaults so a misconfigured Runner fails at
// startup, not at the first provision. Table-driven over the required-field
// gaps and the non-root uid guard.
func TestNewConfigSpecBuilderRejectsIncompleteDefaults(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SpecDefaults)
	}{
		{"no image", func(d *SpecDefaults) { d.Image = "" }},
		{"no checkout dir", func(d *SpecDefaults) { d.CheckoutDir = "" }},
		{"no home dir", func(d *SpecDefaults) { d.HomeDir = "" }},
		// NamePrefix is the operator-derived half of the container name, which
		// becomes a path segment of the agent socket. A separator here escapes
		// RuntimeDir through the same filepath.Join clean that validAccountID
		// guards on the request-derived half, so both operands are checked.
		{"a name prefix containing a path separator", func(d *SpecDefaults) { d.NamePrefix = "a/../../" }},
		{"zero uid", func(d *SpecDefaults) { d.UID = 0 }},
		// An over-long prefix is not a missing field but the same class of
		// startup misconfiguration: it widens every container name past what
		// the Runner's socket-path budget reserved, so the runtime dir clears
		// the budget at boot and the socket then fails EINVAL at bind.
		{"name prefix wider than the socket budget reserves", func(d *SpecDefaults) {
			d.NamePrefix = AgentContainerNamePrefix + "x"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := goodDefaults()
			tc.mutate(&d)
			if _, err := NewConfigSpecBuilder(d); err == nil {
				t.Fatalf("NewConfigSpecBuilder(%s) = nil error, want a startup rejection", tc.name)
			}
		})
	}
}

// A complete SpecDefaults yields a builder.
func TestNewConfigSpecBuilderAcceptsCompleteDefaults(t *testing.T) {
	if _, err := NewConfigSpecBuilder(goodDefaults()); err != nil {
		t.Fatalf("NewConfigSpecBuilder(complete) = %v, want a builder", err)
	}
}

// BuildSpec fills the workspace layout from the defaults — checkout dir, home
// dir, and agent uid — with no clone source or branch: the runner no longer
// clones, the agent self-clones post-launch.
func TestBuildSpecFillsWorkspaceFromDefaults(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatalf("BuildSpec = %v", err)
	}
	want := runtime.Workspace{
		CheckoutDir: "/work/repo",
		HomeDir:     "/home/agent",
		UID:         1000,
	}
	if spec.Workspace != want {
		t.Fatalf("workspace = %+v, want %+v", spec.Workspace, want)
	}
}

// BuildSpec maps the request's server-authoritative persona onto the AgentSpec
// so it rides through to the agent's system prompt. A bug that dropped it would
// boot every agent with no identity overlay.
func TestBuildSpecMapsPersona(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: strings.Repeat("a", 32), Persona: "You are Ada."})
	if err != nil {
		t.Fatalf("BuildSpec = %v", err)
	}
	if spec.Persona != "You are Ada." {
		t.Fatalf("spec.Persona = %q, want %q (req.Persona must reach the AgentSpec)", spec.Persona, "You are Ada.")
	}
}

// BuildSpec maps the request's server-authoritative role onto the AgentSpec so
// it rides through to the agent's block-0 customSystemPrompt. A bug that dropped
// it would boot every agent with no role and the default block-0 prompt.
func TestBuildSpecMapsRole(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: strings.Repeat("a", 32), Role: "manager"})
	if err != nil {
		t.Fatalf("BuildSpec = %v", err)
	}
	if spec.Role != "manager" {
		t.Fatalf("spec.Role = %q, want %q (req.Role must reach the AgentSpec)", spec.Role, "manager")
	}
}

// The container name is prefix+agent_account_id. A bug in the name derivation
// would collide containers or misroute the per-agent workspace.
func TestBuildSpecDerivesName(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("BuildSpec = %v", err)
	}
	if spec.Name != "compass-agent-0123456789abcdef0123456789abcdef" {
		t.Fatalf("spec name = %q, want prefix+agent_account_id (compass-agent-0123456789abcdef0123456789abcdef)", spec.Name)
	}
	// Defaults thread through onto the spec.
	if spec.Image != "compass-agent:latest" {
		t.Fatalf("spec image = %q, want the default image", spec.Image)
	}
	if spec.Workspace.CheckoutDir != "/work/repo" || spec.Workspace.HomeDir != "/home/agent" {
		t.Fatalf("workspace layout = %+v, want the defaults", spec.Workspace)
	}
}

// An empty agent_account_id must be rejected, not silently prefixed. Production
// wires a non-empty NamePrefix ("compass-agent-"), so before the fix
// prefix+"" == "compass-agent-" passed the old `if name == ""` guard and
// collapsed every empty-account provision onto one shared container name. Uses
// goodDefaults() (non-empty NamePrefix) with an otherwise valid request so the
// only failure is the account-id validation.
func TestBuildSpecRejectsEmptyAgentAccountID(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: ""})
	if err == nil {
		t.Fatal("BuildSpec with empty agent_account_id = nil error, want an account-id rejection")
	}
	if !strings.Contains(err.Error(), "agent account id") {
		t.Fatalf("BuildSpec error = %v, want the agent-account-id validation error (not a repo failure)", err)
	}
	if spec.Name != "" {
		t.Fatalf("BuildSpec on rejection returned spec with name %q, want the zero AgentSpec", spec.Name)
	}
}

// The agent account id becomes a path segment of the agent socket
// (RuntimeDir/containers/<prefix><id>/agent.sock), and filepath.Join cleans, so
// a "../" in the id escapes RuntimeDir: "../../../../tmp/pwned" resolves to
// /run/tmp/pwned/agent.sock, where the Runner would MkdirAll a 0700 directory
// and bind. The socket's length guard cannot catch this, because traversal
// SHORTENS the path — every row here is well under the AF_UNIX cap. Each reject
// must surface a non-nil error AND the zero AgentSpec, so no half-built spec
// carries an escaping name to Launch.
func TestBuildSpecRejectsAgentAccountIDThatEscapesItsPathElement(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	rejected := []struct {
		name      string
		accountID string
	}{
		{"parent traversal escaping the runtime dir", "../../../../tmp/pwned"},
		{"shallow parent traversal", "../../etc"},
		{"a single parent segment", ".."},
		{"the current directory", "."},
		{"an embedded separator", "abc/def"},
		{"a leading separator (absolute)", "/etc/passwd"},
		{"a trailing separator", "abc/"},
		// Control and format characters clear every check above and, measured,
		// pass MkdirAll and bind too (a NUL is the exception, failing at
		// MkdirAll) — so without this guard they reach the container name and
		// the logs that quote it. The C1 and bidi rows are the ones a predicate
		// written as `r < 0x20 || r == 0x7f` would silently admit.
		{"an embedded NUL", "abc\x00def"},
		{"an embedded newline", "abc\ndef"},
		{"an embedded DEL", "abc\x7fdef"},
		{"an embedded C1 control", "abc\u0085def"},
		{"an embedded bidi override", "abc\u202edef"},
		// The fixed-width lowercase-hex guard adds three dimensions the shape
		// checks above never covered: wrong width (either side) and a
		// path-safe, right-width string that is not [0-9a-f].
		{"over-width (33 hex)", strings.Repeat("a", 33)},
		{"under-width (short hex)", "abc"},
		{"right width, non-hex letter", strings.Repeat("g", 32)},
		{"right width, uppercase hex", "A" + strings.Repeat("a", 31)},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: tc.accountID})
			if err == nil {
				t.Fatalf("BuildSpec with agent_account_id %q = nil error, want a path-element rejection", tc.accountID)
			}
			if !strings.Contains(err.Error(), "agent account id") {
				t.Fatalf("BuildSpec error = %v, want the agent-account-id validation error (not a repo failure)", err)
			}
			if spec.Name != "" {
				t.Fatalf("BuildSpec on rejection returned spec with name %q, want the zero AgentSpec", spec.Name)
			}
		})
	}

	// The ordinary minted shape still builds, so the guard refuses traversal
	// rather than every id.
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: strings.Repeat("f", 32)})
	if err != nil {
		t.Fatalf("BuildSpec with a 32-hex account id: %v", err)
	}
	if want := "compass-agent-" + strings.Repeat("f", 32); spec.Name != want {
		t.Fatalf("BuildSpec name = %q, want %q", spec.Name, want)
	}
}

// imageIrrelevantFake stands in for a backend declaring whether it reads the
// agent image, mirroring egressMarkerFakeRuntime in the runtime package: the
// real microVM runtime needs a host VMM closure this unit lane has no business
// provisioning.
type imageIrrelevantFake struct {
	runtime.WorkloadRuntime
	irrelevant bool
}

func (f *imageIrrelevantFake) AgentImageIrrelevant() bool { return f.irrelevant }

// A backend that never reads the agent image starts with none configured: the
// microVM boot path runs the agent from the guest rootfs, so requiring an OCI
// ref there would make the shipped Runner image unstartable (its Dockerfile
// sets the microVM env but no COMPASS_AGENT_IMAGE).
func TestResolveAgentImageNotRequiredWhenBackendNeverReadsIt(t *testing.T) {
	img, irrelevant, err := ResolveAgentImage(&imageIrrelevantFake{irrelevant: true}, "")
	if err != nil {
		t.Fatalf("ResolveAgentImage(irrelevant backend, no image) = %v, want a startup", err)
	}
	if img != "" {
		t.Fatalf("ResolveAgentImage returned image %q, want empty", img)
	}
	if !irrelevant {
		t.Fatal("ResolveAgentImage reported the image as read, want irrelevant")
	}
}

// A configured image on such a backend is refused rather than ignored: the
// operator believes it pins the agent, and nothing on that path reads it. The
// message must name where the pin actually lives, or the refusal is a dead end.
func TestResolveAgentImageRefusesAConfiguredImageTheBackendCannotApply(t *testing.T) {
	_, _, err := ResolveAgentImage(&imageIrrelevantFake{irrelevant: true}, "compass-agent:latest")
	if err == nil {
		t.Fatal("ResolveAgentImage(irrelevant backend, image set) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "microvm-rootfs") {
		t.Fatalf("ResolveAgentImage error = %v, want it to name the rootfs as the real pin", err)
	}
}

// The container backends are untouched: an image is still required, and a
// configured one still reaches the specs. A backend that does not implement the
// capability at all takes the same path, so the relaxation cannot leak by
// omission.
func TestResolveAgentImageStillRequiredOnBackendsThatReadIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine runtime.WorkloadRuntime
	}{
		{"a backend declaring it reads the image", &imageIrrelevantFake{irrelevant: false}},
		{"a backend without the capability at all", &imageObliviousFake{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ResolveAgentImage(tc.engine, ""); err == nil {
				t.Fatal("ResolveAgentImage(no image) = nil error, want the required-image rejection")
			}
			img, irrelevant, err := ResolveAgentImage(tc.engine, "compass-agent:latest")
			if err != nil {
				t.Fatalf("ResolveAgentImage(image set) = %v, want the image", err)
			}
			if img != "compass-agent:latest" {
				t.Fatalf("ResolveAgentImage returned %q, want the configured image", img)
			}
			if irrelevant {
				t.Fatal("ResolveAgentImage reported the image as unread on a backend that reads it")
			}
		})
	}
}

// imageObliviousFake implements no image capability, so it exercises the
// type-assertion miss.
type imageObliviousFake struct{ runtime.WorkloadRuntime }

// The builder accepts an empty image only when the backend declared it unread,
// so the second guard cannot fail a microVM Runner the resolver just cleared —
// and cannot be relaxed for a backend that does read it.
func TestNewConfigSpecBuilderAcceptsAnEmptyImageOnlyWhenDeclaredIrrelevant(t *testing.T) {
	d := goodDefaults()
	d.Image = ""
	d.ImageIrrelevant = true
	if _, err := NewConfigSpecBuilder(d); err != nil {
		t.Fatalf("NewConfigSpecBuilder(empty image, declared irrelevant) = %v, want a builder", err)
	}
	d.ImageIrrelevant = false
	if _, err := NewConfigSpecBuilder(d); err == nil {
		t.Fatal("NewConfigSpecBuilder(empty image, not declared) = nil error, want a rejection")
	}
}

// Image and ImageIrrelevant encode one fact, so the contradiction is refused at
// startup: a caller that sets both would otherwise carry a stale image into
// every spec on a backend that declared the field unread.
func TestNewConfigSpecBuilderRejectsAnImageDeclaredIrrelevant(t *testing.T) {
	d := goodDefaults()
	d.ImageIrrelevant = true
	if _, err := NewConfigSpecBuilder(d); err == nil {
		t.Fatal("NewConfigSpecBuilder(image set, declared irrelevant) = nil error, want a rejection")
	}
}
