//go:build unix

package runner

// agentHost.RefreshConfig: the fleet-wide ConfigVersion-driven update pass. On a
// signal the host re-materializes the current config bundle into EACH live
// session's own per-container root — reading that container's own live MCS label
// — and Reloads the agent in place, but ONLY when the bundle version actually
// moved past what that container last materialized. Every case pins a contract a
// plausible bug would break: a version bump must Reload every live container
// exactly once, each into its OWN root with its OWN label (a shared-root or
// shared-label regression relabels one tree N times); an unchanged version must
// re-materialize idempotently but never Reload (never interrupt a live agent);
// and one container's failure must be swallowed so the rest of the fleet still
// updates.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// configFanoutRuntime is a ContainerRuntime for the RefreshConfig fan-out tests.
// Create returns the container NAME as its engine id (per-container-unique, so a
// per-container label and a per-container Reload count are distinguishable —
// stubStreamingRuntime's fixed "fake-id" would alias every container onto one),
// MountLabel serves a per-id label (or a per-id error), and ExecStreaming both
// counts launches per container id (Start = 1, each Reload = +1) and delegates to
// the embedded stub's real terminatable child so Reload's Stop+relaunch works.
type configFanoutRuntime struct {
	*stubStreamingRuntime
	labels    map[string]string // container id -> MCS label
	labelErrs map[string]error  // container id -> MountLabel error (best-effort case)
	execByID  map[string]int    // container id -> ExecStreaming launch count
}

func newConfigFanoutRuntime(t *testing.T) *configFanoutRuntime {
	t.Helper()
	return &configFanoutRuntime{
		stubStreamingRuntime: newStubStreamingRuntime(t),
		labels:               map[string]string{},
		labelErrs:            map[string]error{},
		execByID:             map[string]int{},
	}
}

func (r *configFanoutRuntime) Create(_ context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "create")
	r.created = append(r.created, spec)
	r.mu.Unlock()
	return runtime.ContainerID(spec.Name), nil
}

func (r *configFanoutRuntime) MountLabel(_ context.Context, id runtime.ContainerID) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.labelErrs[string(id)]; err != nil {
		return "", err
	}
	return r.labels[string(id)], nil
}

func (r *configFanoutRuntime) ExecStreaming(ctx context.Context, id runtime.ContainerID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	r.mu.Lock()
	r.execByID[string(id)]++
	r.mu.Unlock()
	return r.stubStreamingRuntime.ExecStreaming(ctx, id, spec)
}

// launchCount reports how many times an agent was launched into the container id
// (its Start plus each Reload), read under the lock.
func (r *configFanoutRuntime) launchCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.execByID[id]
}

// accountSpecBuilder names each container "cont-<accountID>", so a test can
// provision several distinct containers through one host.
type accountSpecBuilder struct{}

func (accountSpecBuilder) BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	spec := liveSpec()
	spec.Name = "cont-" + req.GetAgentAccountId()
	return spec, nil
}

// newConfigRefreshFixture builds an agentHost over the fan-out runtime with a
// live FetchAgentConfig wire (so a test drives the fleet config version) and a
// per-account spec builder. Returns the host, the engine, and the config server.
func newConfigRefreshFixture(t *testing.T) (*agentHost, *configFanoutRuntime, *capturePublish) {
	t.Helper()
	engine := newConfigFanoutRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	pub := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, pub))
	cfg := AgentHostConfig{RuntimeDir: shortRuntimeDir(t)}
	host := NewSessionHost(link, rt, registry, engine, accountSpecBuilder{}, cfg, discardLoggerRunner(), monotonicIDs()).(*agentHost)
	return host, engine, pub
}

// shortRuntimeDir returns a short-lived runtime dir under /tmp (not the deeply
// nested t.TempDir(), whose long test-function name blows the AF_UNIX socket
// path budget for the per-container agent socket). Cleaned up on test end.
func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	//nolint:usetesting // t.TempDir()'s path embeds the long test-function name, which overflows the 108-byte AF_UNIX sun_path budget for the per-container agent socket; a short /tmp dir is required here.
	dir, err := os.MkdirTemp("", "cfgrefresh-")
	if err != nil {
		t.Fatalf("mkdir temp runtime dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("removing temp runtime dir %q: %v", dir, err)
		}
	})
	return dir
}

// provisionAndStart provisions container "cont-<account>" and starts its agent,
// registering a cleanup that stops the live session (terminates the stub child).
// Returns the container name.
func provisionAndStart(t *testing.T, host *agentHost, account string) string {
	t.Helper()
	ctx := context.Background()
	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: account})
	if err != nil {
		t.Fatalf("Provision(%s) = %v", account, err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start(%s) = %v", name, err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })
	return name
}

func configBundleAt(t *testing.T, version string) AgentConfigBundle {
	t.Helper()
	return AgentConfigBundle{Version: version, Tarball: buildConfigTarball(t, validBundle())}
}

// configBundleFrom builds an AgentConfigBundle at version from arbitrary members,
// so a fan-out test can carry the five new SEA-1678 categories (fullBundle) or a
// partial/tolerant-empty subset and, on a version bump, a CHANGED member.
func configBundleFrom(t *testing.T, version string, entries map[string][]byte) AgentConfigBundle {
	t.Helper()
	return AgentConfigBundle{Version: version, Tarball: buildConfigTarball(t, entries)}
}

// A version bump Reloads every live container exactly once, each re-materializing
// into its OWN per-container root with its OWN live MCS label. The relabel calls
// prove per-root/per-label isolation; the launch counts prove exactly one Reload
// per container (2 launches: the Start plus the Reload). A shared-root regression
// would relabel one tree twice; a shared-label regression would use one label for
// both.
func TestRefreshConfigVersionChangedReloadsEachContainerOwnRootOwnLabel(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")

	// Move the fleet config forward and give each container its own live label.
	pub.setConfigBundle(configBundleAt(t, "v-2"))
	engine.labels[nameA] = "system_u:object_r:container_file_t:s0:c10,c20"
	engine.labels[nameB] = "system_u:object_r:container_file_t:s0:c30,c40"
	calls := stubRelabelAnyRoot(t)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil", err)
	}

	// One Reload per container: Start (1) + Reload (1) = 2 launches each.
	if got := engine.launchCount(nameA); got != 2 {
		t.Fatalf("container %s launched %d times, want 2 (Start + one Reload)", nameA, got)
	}
	if got := engine.launchCount(nameB); got != 2 {
		t.Fatalf("container %s launched %d times, want 2 (Start + one Reload)", nameB, got)
	}

	// Each container relabeled its OWN v-2 version dir with its OWN label.
	rootA := filepath.Join(host.runtimeDir, agentSocketDir, nameA, "config", "v-2")
	rootB := filepath.Join(host.runtimeDir, agentSocketDir, nameB, "config", "v-2")
	wantByRoot := map[string]string{
		rootA: engine.labels[nameA],
		rootB: engine.labels[nameB],
	}
	gotByRoot := map[string]string{}
	for _, c := range *calls {
		gotByRoot[c.destDir] = c.mcsLabel
	}
	for root, label := range wantByRoot {
		if gotByRoot[root] != label {
			t.Fatalf("relabel of %q used label %q, want %q (each container's own root + own label)", root, gotByRoot[root], label)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("relabel ran %d times, want exactly 2 (once per container's own root)", len(*calls))
	}
}

// An unchanged fleet version re-materializes idempotently but Reloads NOTHING —
// a live agent must never be interrupted mid-turn for a config that did not move.
// The launch count stays at 1 per container (the Start only, no Reload).
func TestRefreshConfigVersionUnchangedDoesNotReload(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")

	// Version does NOT move; give labels anyway so the update path runs.
	engine.labels[nameA] = "system_u:object_r:container_file_t:s0:c10,c20"
	engine.labels[nameB] = "system_u:object_r:container_file_t:s0:c30,c40"
	_ = stubRelabelAnyRoot(t)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil", err)
	}

	if got := engine.launchCount(nameA); got != 1 {
		t.Fatalf("container %s launched %d times, want 1 (Start only; unchanged version must not Reload)", nameA, got)
	}
	if got := engine.launchCount(nameB); got != 1 {
		t.Fatalf("container %s launched %d times, want 1 (Start only; unchanged version must not Reload)", nameB, got)
	}
}

// Best-effort: one container's MountLabel failure is swallowed and the OTHER
// container still updates and Reloads. One bad container never blocks the fleet.
func TestRefreshConfigBestEffortSwallowsPerContainerError(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")

	pub.setConfigBundle(configBundleAt(t, "v-2"))
	// cont-A's label read fails; cont-B is healthy.
	engine.labelErrs[nameA] = errors.New("inspect exploded")
	engine.labels[nameB] = "system_u:object_r:container_file_t:s0:c30,c40"
	_ = stubRelabelAnyRoot(t)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil (per-container errors are swallowed)", err)
	}

	// cont-A never Reloaded (its label read failed before materialize).
	if got := engine.launchCount(nameA); got != 1 {
		t.Fatalf("failed container %s launched %d times, want 1 (Start only; its error aborts its own update)", nameA, got)
	}
	// cont-B still updated despite cont-A's failure.
	if got := engine.launchCount(nameB); got != 2 {
		t.Fatalf("healthy container %s launched %d times, want 2 (Start + Reload); one bad container must not block the fleet", nameB, got)
	}
}

// A Reload that fails must be retried on the next signal: the tracked config
// version must NOT advance past the last SUCCESSFULLY reloaded version, or the
// container is silently stranded on old config until an unrelated version bump
// (there is no in-process reconnect until T9). Pass 1 bumps the fleet to v-2 but
// the container's Reload fails; pass 2 (same v-2) must therefore still see a
// version change and retry the Reload.
func TestRefreshConfigReloadFailureRetriesOnNextSignal(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	name := provisionAndStart(t, host, "a")

	pub.setConfigBundle(configBundleAt(t, "v-2"))
	engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
	_ = stubRelabelAnyRoot(t)

	// Force this pass's Reload to fail cleanly: deregister the container's handle
	// so Reload rejects at handle re-resolution, BEFORE it stops the live agent
	// stream — the container stays live so the next pass can retry.
	handle, ok := host.registry.Resolve(name)
	if !ok {
		t.Fatalf("container %s not registered after start", name)
	}
	host.registry.Deregister(name)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig (pass 1) = %v, want nil (Reload failure swallowed)", err)
	}
	// The failed Reload launched no new agent (it rejected before StartAgent).
	if got := engine.launchCount(name); got != 1 {
		t.Fatalf("after failed Reload, container %s launched %d times, want 1 (Start only)", name, got)
	}

	// Restore the handle so the retry's Reload can succeed, and drive the SAME
	// version again. If the failed pass had advanced the tracked version, this
	// pass would suppress the Reload and the launch count would stay at 1.
	host.registry.Register(handle)
	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig (pass 2) = %v, want nil", err)
	}
	if got := engine.launchCount(name); got != 2 {
		t.Fatalf("same-version retry after a failed Reload launched %d times, want 2 (Start + retried Reload); a failed Reload must not advance the tracked version", got)
	}
}

// Best-effort across the OTHER failure branch: a container whose Materialize
// fails (here a relabel error) is swallowed and a healthy sibling still updates
// and Reloads. The existing best-effort test fails at MountLabel, before
// Materialize; this one proves the Materialize `continue` is an independent path
// a `return err` regression would not survive.
func TestRefreshConfigBestEffortSwallowsMaterializeError(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")

	pub.setConfigBundle(configBundleAt(t, "v-2"))
	engine.labels[nameA] = "system_u:object_r:container_file_t:s0:c10,c20"
	engine.labels[nameB] = "system_u:object_r:container_file_t:s0:c30,c40"

	// Fail the relabel (hence Materialize) for cont-A only; cont-B relabels fine.
	old := relabel
	relabel = func(_ context.Context, _ string, destDir string) error {
		if strings.Contains(destDir, nameA) {
			return errors.New("chcon exploded")
		}
		return nil
	}
	t.Cleanup(func() { relabel = old })

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil (per-container Materialize error swallowed)", err)
	}
	// cont-A's Materialize failed → no Reload.
	if got := engine.launchCount(nameA); got != 1 {
		t.Fatalf("container %s with failed Materialize launched %d times, want 1 (Start only)", nameA, got)
	}
	// cont-B still updated despite cont-A's Materialize failure.
	if got := engine.launchCount(nameB); got != 2 {
		t.Fatalf("healthy container %s launched %d times, want 2 (Start + Reload); a sibling's Materialize failure must not block it", nameB, got)
	}
}

// stubRelabelAnyRoot swaps the package relabel var and records every call,
// resolving no shared root (the fan-out spans several container roots). It
// records the destDir and label so per-root/per-label isolation can be asserted.
func stubRelabelAnyRoot(t *testing.T) *[]relabelCall {
	t.Helper()
	var calls []relabelCall
	old := relabel
	relabel = func(_ context.Context, mcsLabel, destDir string) error {
		calls = append(calls, relabelCall{mcsLabel: mcsLabel, destDir: destDir})
		return nil
	}
	t.Cleanup(func() { relabel = old })
	return &calls
}

// A RefreshConfig pass with no live sessions is a clean no-op — no relabel, no
// error. A stray signal against an idle Runner must not fault.
func TestRefreshConfigNoLiveSessionsIsNoOp(t *testing.T) {
	host, _, pub := newConfigRefreshFixture(t)
	pub.setConfigBundle(configBundleAt(t, "v-1"))
	calls := stubRelabelAnyRoot(t)
	if err := host.RefreshConfig(context.Background()); err != nil {
		t.Fatalf("RefreshConfig with no sessions = %v, want nil", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("relabel ran %d times with no live sessions, want 0", len(*calls))
	}
}

// A relabel-target path check keeps the destDir substring assertion honest: the
// per-container root must literally contain the container name.
func TestRefreshConfigRootPathContainsContainerName(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()
	pub.setConfigBundle(configBundleAt(t, "v-1"))
	name := provisionAndStart(t, host, "a")
	pub.setConfigBundle(configBundleAt(t, "v-2"))
	engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
	calls := stubRelabelAnyRoot(t)
	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("relabel ran %d times, want 1", len(*calls))
	}
	if !strings.Contains((*calls)[0].destDir, name) {
		t.Fatalf("relabel destDir %q does not contain the container name %q (per-container root)", (*calls)[0].destDir, name)
	}
}

// SEA-1678 T6 — the fan-out update path carries all FIVE new config categories.
//
// At provision a fullBundle (settings/config.yml + top-level AGENTS.md +
// models.yml + a flat rules/ member + a flat agents/ member, atop the
// skills/extensions/mcp validBundle carries) must land under the container's own
// version dir with pinned modes; a version bump carrying a CHANGED member must
// re-materialize + Reload exactly once, advancing the version-moved gate only on
// Reload success; and an unchanged version must stay tolerant-empty — never a
// spurious Reload.

// containerVersionDir is a container's per-container config root for a version —
// mirrors host.configMaterializerFor's layout (runtimeDir/containers/<name>/config)
// plus the version subdir the materializer writes.
func containerVersionDir(host *agentHost, container, version string) string {
	return filepath.Join(host.runtimeDir, agentSocketDir, container, "config", version)
}

// Provision with a fullBundle lands every one of the five new categories under
// the container's own version dir with pinned modes (0644 files, 0755 dirs). RED
// if the runner drops or mis-modes a category on the materialize path a
// provision drives; the existing provision tests only carry the skills-only
// validBundle, so a category-dropping regression would slip past them.
func TestRefreshConfigProvisionCarriesAllFiveCategories(t *testing.T) {
	host, _, pub := newConfigRefreshFixture(t)
	pub.setConfigBundle(configBundleFrom(t, "v-1", fullBundle()))
	name := provisionAndStart(t, host, "a")

	versionDir := containerVersionDir(host, name, "v-1")
	files := map[string]string{
		filepath.Join("settings", "config.yml"): "compaction:\n  keepRecentTokens: 111\n",
		"AGENTS.md":                             "# fleet conventions\n",
		"models.yml":                            "providers:\n  x:\n    baseUrl: https://y\n",
		filepath.Join("rules", "red-green.md"):  "# red-green\n",
		filepath.Join("agents", "design.md"):    "# design agent\n",
	}
	for rel, want := range files {
		p := filepath.Join(versionDir, rel)
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q err=%v, want %q (provision must carry this category)", rel, got, err, want)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode = %v err=%v, want 0644", rel, fi.Mode().Perm(), err)
		}
	}
	for _, dir := range []string{"settings", "rules", "agents"} {
		di, err := os.Stat(filepath.Join(versionDir, dir))
		if err != nil || di.Mode().Perm() != 0o755 {
			t.Fatalf("%s dir mode = %v err=%v, want 0755", dir, di.Mode().Perm(), err)
		}
	}
}

// A version bump whose ONLY difference is a changed settings/config.yml member
// still mints a new version, so RefreshConfig re-materializes the new content
// and Reloads exactly once — and the container's on-disk current now serves the
// UPDATED settings value, the load-bearing "the agent reads the new config"
// contract at the runner tier. RED if the changed member did not carry through:
// the version dir would still hold the old value, or (were the gate keyed on the
// unchanged skills subset) no Reload would fire.
func TestRefreshConfigChangedSettingsMemberRematerializesAndReloads(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleFrom(t, "v-1", fullBundle()))
	name := provisionAndStart(t, host, "a")

	// v-2 changes ONLY settings/config.yml; every other member is identical.
	changed := fullBundle()
	changed["settings/config.yml"] = []byte("compaction:\n  keepRecentTokens: 999\n")
	pub.setConfigBundle(configBundleFrom(t, "v-2", changed))
	engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
	_ = stubRelabelAnyRoot(t)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil", err)
	}
	// Exactly one Reload: Start (1) + Reload (1) = 2 launches.
	if got := engine.launchCount(name); got != 2 {
		t.Fatalf("container %s launched %d times, want 2 (Start + one Reload for the changed member)", name, got)
	}
	// The re-materialized v-2 version dir serves the UPDATED settings value.
	got, err := os.ReadFile(filepath.Join(containerVersionDir(host, name, "v-2"), "settings", "config.yml"))
	if err != nil || string(got) != "compaction:\n  keepRecentTokens: 999\n" {
		t.Fatalf("v-2 settings/config.yml = %q err=%v, want the updated 999 value", got, err)
	}
	// The version-moved gate advanced only after a successful Reload: a second
	// same-version pass must now Reload NOTHING.
	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig (second pass, same version) = %v, want nil", err)
	}
	if got := engine.launchCount(name); got != 2 {
		t.Fatalf("after a same-version second pass, container %s launched %d times, want 2 (no further Reload)", name, got)
	}
}

// A version bump whose only change is a rules/ member mints a new version and
// Reloads exactly once, carrying the updated rule content — the same update-path
// contract for the rules category. RED if a changed rules member did not move
// the version or did not carry through to the re-materialized version dir.
func TestRefreshConfigChangedRuleMemberRematerializesAndReloads(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleFrom(t, "v-1", fullBundle()))
	name := provisionAndStart(t, host, "a")

	changed := fullBundle()
	changed["rules/red-green.md"] = []byte("# red-green (updated)\n")
	pub.setConfigBundle(configBundleFrom(t, "v-2", changed))
	engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
	_ = stubRelabelAnyRoot(t)

	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want nil", err)
	}
	if got := engine.launchCount(name); got != 2 {
		t.Fatalf("container %s launched %d times, want 2 (Start + one Reload for the changed rule)", name, got)
	}
	got, err := os.ReadFile(filepath.Join(containerVersionDir(host, name, "v-2"), "rules", "red-green.md"))
	if err != nil || string(got) != "# red-green (updated)\n" {
		t.Fatalf("v-2 rules/red-green.md = %q err=%v, want the updated rule body", got, err)
	}
}

// Partially-configured bundles keep the tolerant-empty contract on the update
// path: a settings-only, a rules-only, and a skills-only bundle each
// materialize and Reload once on a version bump without crashing, and an
// UNCHANGED partial bundle Reloads nothing (never interrupts a live agent for
// config that did not move). RED if a missing category faulted the materialize,
// or the unchanged pass spuriously Reloaded.
func TestRefreshConfigPartialBundlesAreTolerant(t *testing.T) {
	cases := map[string]map[string][]byte{
		"settings only": {"settings/config.yml": []byte("compaction:\n  keepRecentTokens: 1\n")},
		"rules only":    {"rules/only.md": []byte("# only rule\n")},
		"skills only":   {"skills/greet": []byte("hi\n")},
	}
	for label, members := range cases {
		t.Run(label, func(t *testing.T) {
			host, engine, pub := newConfigRefreshFixture(t)
			ctx := context.Background()

			pub.setConfigBundle(configBundleFrom(t, "v-1", members))
			name := provisionAndStart(t, host, "a")

			// Unchanged version: no Reload.
			engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
			_ = stubRelabelAnyRoot(t)
			if err := host.RefreshConfig(ctx); err != nil {
				t.Fatalf("RefreshConfig (unchanged %s) = %v, want nil", label, err)
			}
			if got := engine.launchCount(name); got != 1 {
				t.Fatalf("%s: unchanged partial bundle launched %d times, want 1 (no spurious Reload)", label, got)
			}

			// Version bump of the same partial shape: one Reload, no crash.
			bumped := map[string][]byte{}
			for k, v := range members {
				bumped[k] = append([]byte{}, v...)
			}
			for k := range bumped {
				bumped[k] = append(bumped[k], []byte("# bumped\n")...)
				break
			}
			pub.setConfigBundle(configBundleFrom(t, "v-2", bumped))
			if err := host.RefreshConfig(ctx); err != nil {
				t.Fatalf("RefreshConfig (bumped %s) = %v, want nil", label, err)
			}
			if got := engine.launchCount(name); got != 2 {
				t.Fatalf("%s: version bump launched %d times, want 2 (Start + one Reload)", label, got)
			}
		})
	}
}
