//go:build unix

// The config-driven SpecBuilder: it assembles a launchable runtime.AgentSpec
// from operator-supplied defaults (the image, the default-deny egress allowlist,
// the workspace layout) plus the per-request repo + ref. It is the production
// SpecBuilder the Runner binary wires; the per-agent-account credential and
// egress derivation that later tiers add plugs into the same SpecBuilder seam
// without changing Provision.
package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"unicode"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// SpecDefaults are the operator-provisioned, request-independent parts of an
// AgentSpec: the image every agent container runs, the default-deny egress
// allowlist, the in-container checkout/home layout + agent uid, and any read-only
// host mounts (e.g. a bare-repo mirror cache). The per-workstream repo and ref
// come from the request; everything else is policy set once at Runner startup.
type SpecDefaults struct {
	Image       string
	Egress      runtime.EgressPolicy
	CheckoutDir string
	HomeDir     string
	UID         uint32
	Mounts      []runtime.Mount
	// NamePrefix prefixes the derived container name so containers are
	// identifiable per Runner/agent; the agent account id is appended.
	NamePrefix string
}

// configSpecBuilder builds specs from fixed defaults + the request.
type configSpecBuilder struct {
	defaults SpecDefaults
}

// NewConfigSpecBuilder returns a SpecBuilder that assembles each AgentSpec from
// defaults plus the provision request. Returns an error if the defaults are
// incomplete (no image or no checkout dir), so a misconfigured Runner fails at
// startup rather than at the first provision.
func NewConfigSpecBuilder(defaults SpecDefaults) (SpecBuilder, error) {
	if defaults.Image == "" {
		return nil, errors.New("spec defaults require an image")
	}
	if defaults.CheckoutDir == "" || defaults.HomeDir == "" {
		return nil, errors.New("spec defaults require checkout and home dirs")
	}
	// The other operand of the container name. validAccountID constrains the
	// request-derived half; this constrains the operator-derived half, so both
	// inputs to a path segment are checked and a separator here cannot escape
	// RuntimeDir through the same filepath.Join clean.
	if strings.Contains(defaults.NamePrefix, "/") {
		return nil, errors.New("spec defaults name prefix must not contain a path separator")
	}
	if defaults.UID == 0 {
		return nil, errors.New("spec defaults require a non-root uid")
	}
	// Length is a separate property from shape, and the budget depends on it.
	// The Runner's startup socket-path budget (validateRuntimeDir) models the
	// container name as AgentContainerNamePrefix + a 32-char account id. A
	// longer prefix would build a path wider than the budget cleared, so the
	// runtime dir would pass at boot and the socket would then fail EINVAL at
	// bind — the exact failure the budget check exists to prevent. Reject the
	// prefix here instead, at the same startup edge, so the model stays true.
	if len(defaults.NamePrefix) > len(AgentContainerNamePrefix) {
		return nil, fmt.Errorf(
			"spec defaults name prefix %q (%d bytes) exceeds the %d bytes the agent socket path budget reserves for it",
			defaults.NamePrefix, len(defaults.NamePrefix), len(AgentContainerNamePrefix))
	}
	return &configSpecBuilder{defaults: defaults}, nil
}

// BuildSpec maps the request's agent account + repo + ref onto a full AgentSpec,
// filling image/egress/workspace-layout from the defaults. Exactly one of the
// request's repo variants (remote_url / local_path) must be set, mirroring the
// built RepoSource.
func (b *configSpecBuilder) BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	source, err := repoSourceFromRequest(req)
	if err != nil {
		return runtime.AgentSpec{}, err
	}
	branch := req.GetRef()

	d := b.defaults
	accountID := req.GetAgentAccountId()
	if err := validAccountID(accountID); err != nil {
		return runtime.AgentSpec{}, err
	}
	name := d.NamePrefix + accountID
	return runtime.AgentSpec{
		Name:  name,
		Image: d.Image,
		Workspace: runtime.Workspace{
			Source:      source,
			Branch:      branch,
			CheckoutDir: d.CheckoutDir,
			HomeDir:     d.HomeDir,
			UID:         d.UID,
		},
		Egress: d.Egress,
		Mounts: d.Mounts,
	}, nil
}

// validAccountID refuses an agent account id that is not a single safe path
// element. The id is not merely a label: it is concatenated into the container
// name (below) and that name becomes a path segment of the agent socket,
// RuntimeDir/containers/<container>/agent.sock (host.go). filepath.Join CLEANS,
// so a "../" in the id escapes RuntimeDir entirely — "../../../../tmp/pwned"
// resolves to /run/tmp/pwned/agent.sock — and the socket's own length guard
// cannot catch it, because traversal SHORTENS the path.
//
// Nothing upstream makes this check redundant. The id is minted 32-hex, but the
// width is a property of the minting site and is never re-checked here; the
// foreign key that ties the id to a real account is enforced by
// RecordAgentContainer, which runs after hub.Provision has already created the
// 0700 directory and bound the socket; and an admin-only RPC narrows who calls
// it, not what they may pass. So this is the hop that has to reject the shape.
//
// The WIDTH is deliberately not asserted here, and that is an open question
// rather than an oversight. run.go's validateRuntimeDir derives the startup
// budget from agentAccountIDWidth, so a path-safe id wider than 32 clears the
// budget at boot and then fails EINVAL at bind. Shape and length are
// independent properties and only the shape is checked. Asserting the width
// here would change the Runner's contract with every caller — four test files
// provision ids like "acct-1" and "a" — so it is a design call, not a fixture
// fix. Tracked as SEA-1443; see this PR's Open Questions.
func validAccountID(id string) error {
	if id == "" {
		return errors.New("provision request requires an agent account id")
	}
	// fs.ValidPath rejects "..", any empty segment, a leading or trailing "/",
	// and invalid UTF-8, but returns true for "." (documented) and for a legal
	// multi-element path like "abc/def" — hence the other two conjuncts. (The
	// empty id returned above, with its own message.)
	if !fs.ValidPath(id) || id == "." || strings.Contains(id, "/") {
		return fmt.Errorf("agent account id %q is not a valid path element", id)
	}
	// A control or format character clears every check above — it is valid UTF-8
	// and carries no separator — and nothing downstream stops it either.
	// Measured on Linux against listenAgentSocket's own ordering: a newline, a
	// DEL, a C1 control and a bidi override all pass MkdirAll AND bind, so the
	// id reaches the container name and every log line that quotes it intact.
	// (A NUL is the lone exception, failing at MkdirAll — one hop before the
	// listener — as a bare EINVAL naming nothing.) The name is what an operator
	// reads back out of `podman ps` and the Runner's logs to identify an agent,
	// so a character that can forge a line break or reorder the display makes
	// that identification unreliable. unicode.IsControl covers C0, DEL and C1;
	// the format class (Cf) is checked with it because a bidi override is not a
	// control character but spoofs a rendered name just as effectively.
	if strings.ContainsFunc(id, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return fmt.Errorf("agent account id %q contains a control or format character", id)
	}
	return nil
}

// repoSourceFromRequest maps the request's repo oneof to a runtime.RepoSource,
// enforcing that exactly one variant is set.
func repoSourceFromRequest(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.RepoSource, error) {
	switch r := req.GetRepo().(type) {
	case *compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl:
		if r.RemoteUrl == "" {
			return runtime.RepoSource{}, errors.New("remote_url is empty")
		}
		if err := validateRemoteURL(r.RemoteUrl); err != nil {
			return runtime.RepoSource{}, err
		}
		return runtime.RemoteSource(r.RemoteUrl), nil
	case *compassv1.ProvisionAgentWorkspaceRequest_LocalPath:
		if r.LocalPath == "" {
			return runtime.RepoSource{}, errors.New("local_path is empty")
		}
		if err := validateLocalPath(r.LocalPath); err != nil {
			return runtime.RepoSource{}, err
		}
		return runtime.LocalPathSource(r.LocalPath), nil
	default:
		return runtime.RepoSource{}, errors.New("provision request requires a repo (remote_url or local_path)")
	}
}

// validateRemoteURL constrains a caller-supplied remote_url to the git transports
// we intend before it reaches `git clone` inside the container. The URL is
// forwarded verbatim as the clone target (workspace.go CloneCommand), so an
// unconstrained value is a clone-time code-execution / read-outside-boundary
// vector: git's `ext::<cmd>` transport runs an arbitrary command, and `file://`
// (or a bare local path) reads outside the intended network-clone boundary.
// Allow only explicit https:// and ssh:// — the two network transports a
// workstream repo is cloned over; a container-local mirror uses local_path, not
// a remote_url. A leading '-' is rejected so the value can never be mistaken for
// a git option.
func validateRemoteURL(raw string) error {
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("remote_url %q must not begin with '-'", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("remote_url %q is not a valid URL: %w", raw, err)
	}
	switch u.Scheme {
	case "https", "ssh":
		if u.Host == "" {
			return fmt.Errorf("remote_url %q has no host", raw)
		}
		return nil
	default:
		return fmt.Errorf("remote_url %q must use the https:// or ssh:// transport (got scheme %q)", raw, u.Scheme)
	}
}

// validateLocalPath constrains a caller-supplied local_path: it becomes the
// clone target as file://<path> (workspace.go cloneArg), so it must be a plain
// absolute in-container path — not itself a URL (no scheme, no `://`, no `ext::`)
// and not an option-injecting leading '-'. It names a container-local bare mirror
// bind-mounted in; the container boundary + read-only mount are the containment,
// this guards the value's shape.
func validateLocalPath(raw string) error {
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("local_path %q must not begin with '-'", raw)
	}
	if !strings.HasPrefix(raw, "/") {
		return fmt.Errorf("local_path %q must be an absolute path", raw)
	}
	if strings.Contains(raw, "://") || strings.Contains(raw, "::") {
		return fmt.Errorf("local_path %q must be a plain path, not a URL", raw)
	}
	return nil
}
