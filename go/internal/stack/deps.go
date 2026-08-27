//go:build unix

package stack

import (
	"context"
	"time"
)

// Deps is the set of external effects the supervisor core is inverted over. Each
// field is one genuine external effect; the real adapters (which import the
// server/runner/certgen/runtime packages) are supplied by the CLI slice, and
// unit tests supply stubs. The core imports none of those packages itself.
type Deps struct {
	// Supervisor starts, signals, and waits child processes (postgres,
	// compass-server, compass-runner). These are the record's "stubbed process
	// execs".
	Supervisor ProcessSupervisor
	// Certs ensures the TLS anchor under the state dir exists and is not near
	// expiry.
	Certs CertEnsurer
	// Tokens ensures the runner enrollment token exists (idempotent, 0600).
	Tokens TokenEnsurer
	// Images ensures the agent image is present in the local store.
	Images ImageEnsurer
	// Prober probes GetServerInfo over the server socket for readiness and the
	// attach version check.
	Prober HealthProber
	// DBProber probes Postgres reachability between starting postgres and
	// compass-server, so the store opens on the first try (devenv.nix:224-242).
	DBProber DBProber
	// GroupSignaller signals and liveness-checks a persisted child process
	// group by pgid for the cross-process teardown (DownDetached). It is the
	// only seam that touches groups this process did not spawn (and thus holds
	// no Process handle for); the real adapter targets the negative pgid, the
	// same primitive the in-process escalation uses.
	GroupSignaller GroupSignaller
	// Containers tears down container children by their stable name for the
	// cross-process teardown (DownDetached), the container analogue of
	// GroupSignaller. Nil until the container-backed postgres adapter (T8) wires
	// the real podman-exec adapter; a record with no container entries never
	// dereferences it.
	Containers ContainerController
	// PostgresContainer starts the container-backed postgres child (S4): the
	// installed-stack default when Config.PostgresImage is set. Start runs
	// `podman run` per the S4 contract and returns a Process handle whose Pid
	// the caller does NOT persist as a pgid (a rootless container runs beneath
	// conmon, outside the client's group) — the container's teardown identity
	// is its stable name, recorded as a v2 container entry and torn down via
	// Containers on a fresh down. Nil on the dev/process path (empty
	// PostgresImage) and on the external-database path, where no container
	// starts; the core dereferences it only when it dispatches to the container
	// path.
	PostgresContainer PostgresContainer
	// CollectorContainer starts the container-backed Plane-B fan-in OTel
	// Collector child (T4 / D3): the installed-stack default when
	// Config.ExternalOTLPEndpoint is empty. Start runs `podman run` (detached)
	// per the T4 contract and returns a Process handle whose Pid the caller does
	// NOT persist as a pgid (a rootless container runs beneath conmon, outside
	// the client's group) — the container's teardown identity is its stable
	// name, recorded as a v2 container entry and torn down via Containers on a
	// fresh down, exactly like PostgresContainer. Nil on the --otel-external
	// path, where no collector container starts; the core dereferences it only
	// when it dispatches to the collector start.
	CollectorContainer CollectorContainer
	// CollectorProber probes the collector's health_check extension for
	// readiness between launching the collector and the components that emit to
	// it — the collector analogue of DBProber, since CollectorContainer.Start
	// returns at launch, not at readiness. Nil on the --otel-external path,
	// where no collector component starts; the core dereferences it only on the
	// collector-readiness gate.
	CollectorProber CollectorProber
	// Now is the clock the cert-expiry math reads. Nil defaults to time.Now.
	Now func() time.Time

	// ExpectedVersion is the version this build of the stack drives. On
	// attach-if-live it is compared against the probed server version; a mismatch
	// surfaces ErrVersionMismatch (an upgraded app must not silently drive a
	// lingering older stack). It lives here rather than being pulled from the
	// server package so the core imports nothing red.
	ExpectedVersion string
}

// ProcessSpec describes a child process to start: which component it is, its
// argument vector (excluding argv[0]), and additional environment entries in
// "KEY=VALUE" form. The concrete binary path is resolved by the supervisor
// adapter from Component — binary location is a deployment concern, not the
// core's. Secrets (the runner token) travel in Env, never Args, so they never
// reach the process table.
type ProcessSpec struct {
	Component Component
	Args      []string
	Env       []string
}

// Component identifies a supervised child of the stack. It doubles as the log
// label and the key the supervisor adapter resolves to a binary path.
type Component int

const (
	// ComponentPostgres is the private store-of-record child.
	ComponentPostgres Component = iota
	// ComponentServer is compass-server (serves the socket + TLS door).
	ComponentServer
	// ComponentRunner is compass-runner (enrolls over the TLS door).
	ComponentRunner
	// ComponentCollector is the bundled Plane-B fan-in OTel Collector child (T4
	// / D3). Like ComponentPostgres on the container path it is a container
	// child, so it uses no componentBinary; the enum + String case are its log
	// label and the component key its v2 container pgid entry records.
	ComponentCollector
)

// String renders the component for logs and errors.
func (c Component) String() string {
	switch c {
	case ComponentPostgres:
		return "postgres"
	case ComponentServer:
		return "compass-server"
	case ComponentRunner:
		return "compass-runner"
	case ComponentCollector:
		return "otel-collector"
	default:
		return "unknown-component"
	}
}

// Process is a handle to a started child. Signal requests a graceful stop; Wait
// blocks until the child exits (or ctx is done) and returns its exit error, if
// any. Pid reports the child's PID, which doubles as its process-group ID (the
// adapter sets Setpgid at Start), so the supervisor can persist the pgid for a
// cross-process teardown that no longer holds this in-memory handle.
type Process interface {
	Signal(sig ProcessSignal) error
	Wait(ctx context.Context) error
	Pid() int
}

// ProcessSignal is the stop disposition a signal targets. The in-process
// Process.Signal seam only ever asks for a graceful termination (SignalTerm);
// the GroupSignaller seam (cross-process teardown) also carries SignalKill for
// the bounded escalation after the per-child drain budget.
type ProcessSignal int

const (
	// SignalTerm requests a graceful termination (SIGTERM semantics).
	SignalTerm ProcessSignal = iota
	// SignalKill requests an unconditional hard kill (SIGKILL semantics). It is
	// the terminal step of DownDetached's bounded escalation, never a first
	// resort, and is not a valid disposition for the in-process Process.Signal.
	SignalKill
)

// ProcessSupervisor starts child processes. Start returns a live handle or an
// error if the child could not be launched.
type ProcessSupervisor interface {
	Start(ctx context.Context, spec ProcessSpec) (Process, error)
}

// GroupSignaller signals and identity-checks a persisted child process group by
// its process-group id. It is the cross-process teardown primitive: DownDetached
// reads pgids from the state-dir record and drives them here, since the tearing
// process holds no Process handle for a stack a prior up spawned.
//
// Signal delivers sig to the whole group (the real adapter targets the negative
// pgid, matching the in-process escalation's syscall.Kill(-pid, ...)). Alive
// reports whether a group with this pgid exists AND its leader's current start
// time matches startTime — the identity gate that turns "a group with this pgid
// exists" (which a recycled pid passes falsely) into "the ORIGINAL group is
// still alive". A gone group (ESRCH) or a start-time mismatch reports not-alive.
type GroupSignaller interface {
	Signal(pgid int, sig ProcessSignal) error
	Alive(pgid int, startTime uint64) bool
}

// ContainerController tears down a container child by its stable name for the
// cross-process teardown (DownDetached), the container analogue of
// GroupSignaller: DownDetached reads container names from the state-dir record
// and drives them here, since the tearing process holds no handle for a
// container a prior up ran. It is the only seam that touches containers this
// process did not start.
//
// Exists reports whether a container with this name is present (the real adapter
// runs `podman container exists <name>`) — the liveness channel, the container
// analogue of GroupSignaller.Alive; a container needs no start-time identity
// token because its name is unique per state dir (S4). Stop requests a graceful
// stop bounded by timeout (`podman stop -t <seconds> <name>`); Remove is the
// SIGKILL-tier escalation that force-removes it (`podman rm -f <name>`).
type ContainerController interface {
	Exists(name string) bool
	Stop(name string, timeout time.Duration) error
	Remove(name string) error
}

// PostgresContainer starts the container-backed postgres child (S4): the
// installed-stack store-of-record, run as a supervised stack component so the
// one-command up/down lifecycle holds (the container is a child, not an
// out-of-tree quadlet). It is the START analogue of ContainerController (which
// tears down): this seam brings the container up at spawn; ContainerController
// tears it down on a fresh cross-process down by the persisted name.
//
// Start runs the S4 `podman run` (detached) from spec and returns a Process
// handle for the in-process lifecycle: Signal(SignalTerm) maps to `podman stop`
// and Wait blocks until the container exits, so an in-process Down drains it the
// same way it drains a process child. The handle's Pid is NOT a process-group id
// (a rootless container runs beneath conmon) and is never persisted as a pgid;
// the container's durable teardown identity is spec.Name, recorded as a v2
// container entry. A non-nil error means the container could not be launched.
type PostgresContainer interface {
	Start(ctx context.Context, spec PostgresContainerSpec) (Process, error)
}

// CollectorContainer starts the container-backed Plane-B fan-in OTel Collector
// child (T4 / D3), the collector analogue of PostgresContainer. It is the START
// seam; ContainerController tears the collector down on a fresh cross-process
// down by the persisted name (the same reusable teardown contract postgres
// uses).
//
// Start writes the generated collector config to disk, runs `podman run`
// (detached) from spec, and returns a Process handle for the in-process
// lifecycle: Signal(SignalTerm) maps to `podman stop` and Wait blocks until the
// container exits, so an in-process Down drains it the same way it drains a
// process child. The handle's Pid is NOT a process-group id (a rootless
// container runs beneath conmon) and is never persisted as a pgid; the
// container's durable teardown identity is spec.Name, recorded as a v2 container
// entry. A non-nil error means the container could not be launched.
type CollectorContainer interface {
	Start(ctx context.Context, spec CollectorContainerSpec) (Process, error)
}

// CertEnsurer ensures the TLS anchor (one PEM that is both the server's
// --tls-cert and the runner's --ca) exists under stateDir and is valid well past
// now. It is expiry-aware, not skip-if-present: when the existing anchor's
// NotAfter falls within the rotation window it regenerates and reports
// rotated=true, so a live stack is never driven over an expired door. The
// returned paths are the cert and key files to hand to the server and runner.
type CertEnsurer interface {
	EnsureCert(ctx context.Context, stateDir string, now time.Time) (CertResult, error)
}

// CertResult is the outcome of EnsureCert: the resolved cert/key paths and
// whether this call regenerated the anchor.
type CertResult struct {
	CertPath string
	KeyPath  string
	Rotated  bool
}

// TokenEnsurer ensures the runner enrollment token exists (idempotent: it does
// not rotate an existing token), writes it 0600 under the state dir, and returns
// the token value. runnerID is the subject the minted token is issued for; the
// runner cross-checks its --runner-id against it at enroll, so mint and spawn
// must be threaded the same id. The value is handed to the runner via env only.
type TokenEnsurer interface {
	EnsureToken(ctx context.Context, stateDir, runnerID string) (string, error)
}

// ImageEnsurer ensures the agent image ref is present in the local container
// store (the runner refuses to boot without it).
type ImageEnsurer interface {
	EnsureImage(ctx context.Context, image string) error
}

// HealthProber probes GetServerInfo over the server socket. A nil error means
// the server answered — the real readiness signal, since the socket binds before
// migrations complete. The returned ServerInfo carries the version used for the
// attach mismatch check.
type HealthProber interface {
	Probe(ctx context.Context, socketPath string) (ServerInfo, error)
}

// DBProber probes that Postgres is accepting connections on the DSN — the real
// "postgres up and reachable" signal spawnChain needs before starting
// compass-server, whose store.Open pings once with no retry. A nil error means
// reachable; a non-nil error means not yet (postgres still starting, or the
// compass database not yet created by the postgres wrapper's ensureDatabase).
type DBProber interface {
	ProbeDB(ctx context.Context, dsn string) error
}

// CollectorProber probes the bundled collector's health_check extension for
// readiness — the collector analogue of DBProber. CollectorContainer.Start
// returns at launch, not at readiness, so spawnChain polls this between
// launching the collector and the components that emit to it. A nil error means
// the collector answered healthy on healthEndpoint; a non-nil error means not
// yet (the collector still starting).
type CollectorProber interface {
	ProbeCollector(ctx context.Context, healthEndpoint string) error
}

// ServerInfo is the subset of GetServerInfo the core consumes.
type ServerInfo struct {
	Version string
}

// now returns the configured clock or time.Now when unset, so callers need not
// always supply one.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
