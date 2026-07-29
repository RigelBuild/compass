//go:build unix

package runner

// NewConfigSpecBuilder + BuildSpec: the constructor rejects incomplete defaults
// (no image, missing checkout/home dir), and BuildSpec maps the request's repo
// oneof to the right RepoSource (remote_url→RemoteSource, local_path→
// LocalPathSource, neither→error) and derives the container name as
// prefix+agent_account_id. Every test names the contract a plausible bug would
// break: a misconfigured Runner must fail at startup, not first provision; a
// request with the wrong repo variant must not silently launch.

import (
	"strings"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
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

// BuildSpec maps each repo oneof variant to the matching RepoSource, and neither
// set is an error. Table-driven, including the empty-URL / empty-path sub-cases
// the mapper rejects.
func TestBuildSpecMapsRepoSource(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}

	t.Run("remote url", func(t *testing.T) {
		spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: strings.Repeat("a", 32),
			Repo:           &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: "https://example.com/r.git"},
		})
		if err != nil {
			t.Fatalf("BuildSpec(remote) = %v", err)
		}
		if spec.Workspace.Source != runtime.RemoteSource("https://example.com/r.git") {
			t.Fatalf("remote source = %+v, want RemoteSource of the url", spec.Workspace.Source)
		}
	})

	t.Run("local path", func(t *testing.T) {
		spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: strings.Repeat("a", 32),
			Repo:           &compassv1.ProvisionAgentWorkspaceRequest_LocalPath{LocalPath: "/mirror/r.git"},
		})
		if err != nil {
			t.Fatalf("BuildSpec(local) = %v", err)
		}
		if spec.Workspace.Source != runtime.LocalPathSource("/mirror/r.git") {
			t.Fatalf("local source = %+v, want LocalPathSource of the path", spec.Workspace.Source)
		}
	})

	t.Run("neither set is an error", func(t *testing.T) {
		_, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: strings.Repeat("a", 32)})
		if err == nil {
			t.Fatal("BuildSpec with no repo variant = nil error, want a required-repo error")
		}
	})

	t.Run("empty remote url is an error", func(t *testing.T) {
		_, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: strings.Repeat("a", 32),
			Repo:           &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: ""},
		})
		if err == nil {
			t.Fatal("BuildSpec with empty remote_url = nil error, want a rejection")
		}
	})
}

// The container name is prefix+agent_account_id, and the request's ref becomes
// the workspace branch. A bug in the name derivation would collide containers or
// misroute the per-agent workspace.
func TestBuildSpecDerivesNameAndBranch(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId: "0123456789abcdef0123456789abcdef",
		Ref:            "feature-branch",
		Repo:           &compassv1.ProvisionAgentWorkspaceRequest_LocalPath{LocalPath: "/mirror/r.git"},
	})
	if err != nil {
		t.Fatalf("BuildSpec = %v", err)
	}
	if spec.Name != "compass-agent-0123456789abcdef0123456789abcdef" {
		t.Fatalf("spec name = %q, want prefix+agent_account_id (compass-agent-0123456789abcdef0123456789abcdef)", spec.Name)
	}
	if spec.Workspace.Branch != "feature-branch" {
		t.Fatalf("workspace branch = %q, want the request ref", spec.Workspace.Branch)
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
// only failure is the account-id validation, not a repo error.
func TestBuildSpecRejectsEmptyAgentAccountID(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId: "",
		Repo:           &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: "https://example.com/r.git"},
	})
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
// carries an escaping name to Launch. Uses a valid repo so the id is the only
// failing dimension.
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
			spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
				AgentAccountId: tc.accountID,
				Repo:           &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: "https://example.com/r.git"},
			})
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
	spec, err := builder.BuildSpec(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId: strings.Repeat("f", 32),
		Repo:           &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: "https://example.com/r.git"},
	})
	if err != nil {
		t.Fatalf("BuildSpec with a 32-hex account id: %v", err)
	}
	if want := "compass-agent-" + strings.Repeat("f", 32); spec.Name != want {
		t.Fatalf("BuildSpec name = %q, want %q", spec.Name, want)
	}
}

// BuildSpec must constrain the caller-supplied repo value before it is forwarded
// verbatim as the `git clone` target: an unconstrained remote_url reaches git's
// `ext::<cmd>` (arbitrary command) / `file://` (read-outside-boundary) transports,
// and a non-absolute or URL-shaped local_path escapes the intended container-local
// mirror. Table-driven over the allowed transports and each rejected shape; every
// reject row must surface a non-nil error AND return the zero AgentSpec (no
// half-built spec leaks past a rejected clone target). Uses goodDefaults() + a
// valid agent_account_id so the repo value is the only failing dimension.
func TestBuildSpecValidatesRepoScheme(t *testing.T) {
	builder, err := NewConfigSpecBuilder(goodDefaults())
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	cases := []struct {
		name    string
		repo    isRepoVariant
		wantErr bool
	}{
		// Accept: the two network transports plus an absolute container-local mirror.
		{"https remote", remote("https://example.com/r.git"), false},
		{"ssh remote", remote("ssh://git@github.com/org/repo.git"), false},
		{"absolute local path", local("/mirror/r.git"), false},

		// Reject remote_url: non-git-network transports and option injection.
		{"ext transport remote", remote("ext::sh -c 'touch /tmp/pwned'"), true},
		{"file transport remote", remote("file:///etc/passwd"), true},
		{"git transport remote", remote("git://example.com/r.git"), true},
		{"plaintext http remote", remote("http://example.com/r.git"), true},
		{"scp shorthand remote", remote("git@github.com:org/repo.git"), true},
		{"option-injecting remote", remote("-oProxyCommand=evil"), true},

		// Reject local_path: non-absolute, URL-shaped, and option injection.
		{"relative local path", local("relative/path"), true},
		{"ext scheme local path", local("ext::evil"), true},
		{"file url local path", local("file:///x"), true},
		{"option-injecting local path", local("-x"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: strings.Repeat("a", 32)}
			tc.repo(req)
			spec, err := builder.BuildSpec(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BuildSpec(%s) = nil error, want an unsafe-repo rejection", tc.name)
				}
				if spec.Name != "" {
					t.Fatalf("BuildSpec(%s) on rejection returned spec with name %q, want the zero AgentSpec", tc.name, spec.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildSpec(%s) = %v, want the safe repo value accepted", tc.name, err)
			}
		})
	}
}

// isRepoVariant sets the repo oneof on a request. The oneof interface is
// unexported in the compassv1 package, so the table stores a setter instead of
// the variant value directly.
type isRepoVariant func(*compassv1.ProvisionAgentWorkspaceRequest)

func remote(url string) isRepoVariant {
	return func(req *compassv1.ProvisionAgentWorkspaceRequest) {
		req.Repo = &compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl{RemoteUrl: url}
	}
}

func local(path string) isRepoVariant {
	return func(req *compassv1.ProvisionAgentWorkspaceRequest) {
		req.Repo = &compassv1.ProvisionAgentWorkspaceRequest_LocalPath{LocalPath: path}
	}
}
