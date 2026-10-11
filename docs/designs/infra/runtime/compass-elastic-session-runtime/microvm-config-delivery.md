# MicroVM fleet-config delivery

Tracking: RIG-3093

> **Design record.** Paths cite `RigelBuild/compass` at `origin/main` as of
> 2026-10-11. Decision: [DL-478](../../../decisions/infra/DL-478.md) (pending
> Matt). On the microVM tier it amends
> [DL-080](../../../decisions/agent/DL-080.md) (read-only bind mount) and
> [DL-081](../../../decisions/agent/DL-081.md) (re-materialize + Reload). It
> closes [microvm-v4-gateway-over-vsock.md](microvm-v4-gateway-over-vsock.md)
> §(f) and OQ-3.

## Problem / Intent

A microVM agent runs with empty fleet config. `provisionVsockGateway`
(`go/internal/runner/host.go`) launches with no config mount and no
`configVersions` seed, because the backend accepts exactly one read-write
mount. `workspaceShare` (`go/internal/runtime/microvm_lifecycle.go`) rejects
both `if mounts[0].ReadOnly {` and
`if mounts[0].ContainerPath != workspaceMountPath {` with
`return Mount{}, &UnsupportedMountError{Mount: mounts[0]}`. `refreshOneContainer` then skips
the session with `if _, ok := h.engine.(vsockGatewayEngine); ok { return nil }`.
Intent: config parity with podman, including live refresh on a ConfigVersion
publish, with that gate and the Provision skip both removed.

## Approach

### (a) Mechanism: push the bundle over GuestControl

The Runner fetches the bundle and checks its schema, then streams it to
`compass-guestd` over a new `GuestControl` RPC. guestd is PID 1 and guest
root. It enforces content-agnostic safety and installs the tree at the agent's
fixed default path, `/run/compass/agent-config`, in the layout
`ConfigMaterializer` writes on the host: `<version>/` with a `version` file,
plus a relative `current` symlink flipped by rename. The agent is unchanged:
it reads `AGENT_CONFIG_MOUNT_PATH` through `current/` (`resolveConfigMountPath`,
`packages/compass-agent/src/config-reader.ts`). Exec cannot do this job:
`WriteAgentFiles` (`go/internal/runtime/agent.go`) writes as the agent uid,
and `resolveUID` (`go/internal/guestd/supervisor.go`) refuses uid 0: "exec uid
0 is refused: the guest supervisor never runs an exec as root".

### (b) Wire shape (additive contract change)

```proto
// InstallAgentConfig installs the fleet agent-config bundle at the agent's fixed
// config path as guest root: first frame version, then gzip-tar chunks. Refused
// until Provision succeeds; idempotent per version; "" installs no tree.
rpc InstallAgentConfig(stream InstallAgentConfigRequest) returns (InstallAgentConfigResponse);
message InstallAgentConfigRequest {
  oneof frame {
    string version = 1;  // first frame only; the bundle content hash
    bytes chunk = 2;     // gzip-tar bytes, in order
  }
}
message InstallAgentConfigResponse {}
```

It streams because the compressed tarball's wire cap (see (c)) exceeds the
16 MiB per-message cap that `serveHandshake` sets (`connect.WithReadMaxBytes`,
`go/internal/guestd/vsock.go`). The frame schema mirrors
`FetchAgentConfigResponse` (`proto/compass/v1/runner.proto`); the direction is
new, and client streaming follows `PublishEvents`. The host sends 512 KiB
chunks under a new Runner-side `installChunkBytes`. It matches the Server's
`configChunkBytes` (`go/internal/runnerhub/handler.go`) by choice, not by
reuse. Shipping with it: the
doc comments above, one file-header line, and the regenerated internal Go
lane. The gen-fence already covers the `GuestControl` stem, so it needs no
edit. `runner-tiers.md` does not cover config delivery.

### (c) Guest install and the security boundary

guestd accepts a non-empty version matching `^[A-Za-z0-9_-]+$` other than the
reserved `current`, or `""` with no chunks. Wire bytes are capped at
`agentconfig.MaxWireBytes = 2 * MaxBytes`, because the decompressed cap is the
wrong bound for compressed bytes. That is a coarse ceiling, not a tight tar
bound: PAX headers and gzip expansion make a tight bound fragile. The Runner
checks `len(tarball) <= MaxWireBytes` before the push, so an oversized bundle
fails on the host with a named error rather than mid-stream. The Server's own
caps sit far below it (`go/internal/store/agent_config.go`:
`maxFileCount = 4096`, regular files only; `maxDecompressedBytes int64 = 64 << 20`).
`agentconfig.Tree.Install`
then runs with a zero `Check`: caps,
traversal and containment, dir/regular types only, pinned 0755/0644 modes,
and the writer. A `configMu` lock, separate from `s.mu`, serializes installs.
The boundary has three legs:

- **Ownership.** The tree and `current` are root-owned under the 0711
  `/run/compass` that `startGatewayProxy`
  (`go/internal/guestd/gateway_proxy.go`) creates:
  `os.MkdirAll(filepath.Dir(socketPath), 0o711)`.
- **No DAC override.** Exec children are non-root with an empty capability
  set (`linuxCredential`), so the agent cannot write, chmod, or rename there.
- **No RPC access.** `peerCIDListener` (`go/internal/guestd/vsock.go`) refuses
  every non-host CID, including in-guest loopback, before any HTTP byte.

The tree sits on the tmpfs overlay upper. It dies with the VM, and every
Provision installs it again. **Known failure mode:** the agent shares that
tmpfs and can fill it. Installs then fail with `CodeInternal`, and refresh
retries on every signal without reloading. This is self-DoS only, and it is
accepted.

### (d) Provision wiring: the skip is removed

`vsockGatewayEngine` gains `InstallAgentConfig`, so a vsock engine without
config delivery cannot compile, and there is no runtime missing-installer
branch. `provisionVsockGateway` fetches and schema-checks before `Launch`, as
podman materializes before `Launch`, so a bad bundle or a Server fault costs
no VM boot. It then launches, resolves the endpoint, serves, and writes
`h.sockets[name]`. It pushes only after that, so on failure `closeSocket`
finds the listener. Then it seeds `configVersions[name]`. A failed push runs
`closeSocket` + `teardownContainer` and returns `installing agent config: %w`,
the `provisionHostGateway` precedent. The spec still carries only the
workspace mount, so OQ-C's refuse-don't-drop is untouched.

### (e) Refresh: the gate is removed

`refreshOneContainer` swaps the gate and its `MountLabel` + `Materialize` pair
for `deliverConfig`. Non-vsock engines run today's two calls unchanged. The
microVM path fetches with `ifVersion = lastVersion`. On a match the Server
sends only the version frame, so there is no push and no reload. Otherwise
it schema-checks and pushes. The version compare, the ERRORED branch, and
`reloadLocked` stay shared and unedited. A failed reload leaves `lastVersion`
old, so the next signal re-fetches, and the install is idempotent per
version. `MicroVMRuntime` is not a `runtime.SessionSweeper`, so the sweep is a
no-op. The agent loads config once, at start (`loadMountedConfig`,
`packages/compass-agent/src/cli.ts`), so a new version reaches a live session
only through the reload, as on podman.

### (f) Split validation: schema on the host, safety in the guest (OQ-1b)

`validateAndUnpack` (`go/internal/runner/config_materialize.go`) mixes two
things:

- a **safety kernel**: caps, traversal, types, and modes;
- a **bundle schema**: the `topDir*` allow-list, the member grammar, and the
  JSON/YAML shape, which grew in RIG-1678, RIG-2968, and RIG-3075.

The new `go/internal/agentconfig` holds the kernel and the `Tree` writer
(guestd cannot import `runner`, which pulls in `runner/gateway`). The schema
stays in the Runner as an `agentconfig.Check`, run on the host before every
push. That gives fast failure with host-side logs, and the guest image never
needs a lockstep release when the schema grows. The allow-list is a contract,
not a boundary: only the host reaches the RPC. `relabel` stays in the Runner
as a `beforeFlip` hook, so the podman sequence (unpack, chcon, flip, prune) is
unchanged.

## Alternatives considered

- **A second, read-only virtio-fs share.** virtiofsd `--readonly` is enforced
  on the host, a *stronger* boundary than in-guest DAC that holds even against
  guest root. It is rejected on cost: a fourth process per session, a wider
  `workspaceShare` that reopens OQ-C, and a host-side `current` flip that must
  cross the virtio-fs cache. Push loses only against guest root, which V2b
  puts out of scope.
- **Stage the tree in `/workspace`.** The share is read-write as the agent
  uid, so the agent could rewrite its config, and stale trees would persist
  with the session volume.
- **A generic `InstallTree(path, tar)` RPC.** It widens host-to-guest-root
  writes to arbitrary paths.
- **A read-only bind remount in the guest.** DAC already stops an agent with
  no capabilities, and a remount adds mount-namespace surface.
- **The full schema in the guest.** See OQ-1b.

## Global Constraints

- **Podman, host, and apple-container are byte-identical.**
  `config_materialize_test.go`, `config_refresh_test.go`, `host_test.go`,
  `host_gateway_test.go`, and `config_delivery_e2e_test.go` keep every
  assertion and error string. The only edits rename moved constants
  (`go/internal/runner/config_materialize.go`: `maxConfigFiles = 10_000`,
  every member, → `agentconfig.MaxFiles`; `maxConfigBytes = 64 << 20` →
  `agentconfig.MaxBytes`).
- **The guest image ships with or before the Runner.** An older guest answers
  `CodeUnimplemented`. That maps to `microvm.ErrGuestImageTooOld` and fails
  Provision closed, as podman aborts when config cannot materialize.
- **`WorkloadRuntime` is untouched**; capabilities stay as unexported probes
  on `agentHost`.
- **Invariants.** The proto change is additive and internal-go only. OQ-C
  stays. The agent is byte-identical, with no
  `COMPASS_AGENT_CONFIG_MOUNT_PATH` on microVM. The tree is root-owned
  0755/0644 with a `version` file and a relative `current`.
- **Build.** guestd stays `CGO_ENABLED=0` and go.mod is unchanged, so the
  `vendorHash` in `guest-image/default.nix` should hold. The guest-image
  build confirms it. T1 adds `/go/internal/agentconfig/**` to the `build`
  inputs in `guest-image/moon.yml`, which list `/go/internal/guestd/**` and
  `/go/internal/gen/**` today. Without it, a kernel change would not
  reschedule the guest-image build.

## Plan

T1 → T2 → T3 are hermetic. T4 is KVM-gated. T2's handler is reachable over
`serveHandshake`, and the host client lands with its caller in T3, so no step
ships inert code.

### T1 — Extract the `go/internal/agentconfig` safety kernel

- **Interfaces:** produces the following:

  ```go
  const MaxBytes = 64 << 20; const MaxFiles = 10_000 // the Runner's caps today; MaxFiles counts every member
  const MaxWireBytes = 2 * MaxBytes                  // coarse compressed-bytes ceiling
  var ErrUnsafeBundle = errors.New("agentconfig: unsafe bundle")
  type Check struct {
      Member  func(clean string, typeflag byte) error  // before the member's bytes are read
      Content func(clean string, content []byte) error // regular files, after the read
  }
  func Walk(tarball []byte, check Check) error // safety + check, no writes
  func Unpack(tarball []byte, destDir string, check Check) error
  type Tree struct { Root string; Log *slog.Logger }
  func (t Tree) EnsureRoot() error
  func (t Tree) Install(version string, tarball []byte, check Check, beforeFlip func(versionDir string) error) error
  ```

  `Install("")` is `EnsureRoot` only. Otherwise it runs staging, `Unpack`, the
  `version` file, the mode pin, and the rename (skipped if the dir exists),
  then `beforeFlip`, the flip, and the prune. Safety failures wrap
  `ErrUnsafeBundle`, and `Check` errors pass through. The Runner keeps `var
  bundleSchema agentconfig.Check` (the layout grammar and JSON/YAML shape) and
  gains `func (m *ConfigMaterializer) Fetch(ctx context.Context, ifVersion
  string) (AgentConfigBundle, error)`, which keeps today's
  `CodeFailedPrecondition` → empty mapping. `Materialize` becomes `Fetch(ctx,
  "")`, then `EnsureRoot` (keeping the `gateway.ErrOperatorConfig` wrap), then
  `Install(…, bundleSchema, relabel hook iff mcsLabel != "")`.
- **Test cycle:** in `agentconfig`, the kernel with a zero `Check`
  (traversal, absolute, symlink, hardlink; `MaxFiles` and `MaxBytes` each at
  the limit and one over), a `Check` error
  stopping the unpack, an idempotent `Install`, a `beforeFlip` error leaving
  `current` unflipped, and a prune that keeps dot-prefixed entries. The runner
  suites pass under the parity constraint.

### T2 — `InstallAgentConfig` proto + guestd handler

- **Interfaces:** consumes T1. Produces the §(b) messages and the regenerated
  client and handler, plus:

  ```go
  func (s *supervisor) InstallAgentConfig(ctx context.Context,
      stream *connect.ClientStream[compassv1internal.InstallAgentConfigRequest],
  ) (*connect.Response[compassv1internal.InstallAgentConfigResponse], error)
  ```

  It adds `supervisor.agentConfigRoot` (empty means
  `/run/compass/agent-config`; a hermetic override, like `gatewaySocketPath`)
  and `supervisor.configMu`. Codes: not provisioned →
  `CodeFailedPrecondition`; frame-order, version, or `ErrUnsafeBundle` →
  `CodeInvalidArgument`; wire overrun → `CodeResourceExhausted`; anything else
  → `CodeInternal`. The fakes `canaryFakeClient`
  (`go/internal/runtime/boot_canary_test.go`) and `fakeGuestClient`
  (`go/internal/runtime/microvm_start_test.go`) gain the method.
- **Test cycle:** `moon run proto:ci` is green. Over `serveHandshake` with a
  tempdir root:
  - The tree installs with 0755/0644 modes, and `current/version` holds the
    version.
  - The call is refused before Provision.
  - A chunk before the version, a second version, `current`, `../x`, or empty
    with chunks returns `CodeInvalidArgument`.
  - Exactly `MaxWireBytes` is accepted past the wire check; one byte more
    returns `CodeResourceExhausted`.
  - A traversal member returns `CodeInvalidArgument`, with no flip.
  - An unknown top dir installs (the guest is schema-agnostic).
  - `""` leaves no `current`.
  - A filesystem failure (the test root made read-only after Provision)
    returns `CodeInternal`, with no flip.

### T3 — Host client, Runner wiring, gate removal

- **Interfaces:** consumes T1 and T2. In `go/internal/runtime/microvm`:
  `func InstallAgentConfig(ctx context.Context, client
  compassv1internalconnect.GuestControlClient, version string, tarball []byte)
  error` and `var ErrGuestImageTooOld`, which wraps `CodeUnimplemented`.
  `func (m *MicroVMRuntime) InstallAgentConfig(ctx context.Context, name,
  version string, tarball []byte) error` resolves `spec.Name` like
  `AgentGatewayEndpoint` and errors for an unknown or unstarted session. It
  uses a new `microvmSession.client`, set in `Start`. In the Runner:

  ```go
  type vsockGatewayEngine interface {
      AgentGatewayEndpoint(name string) (endpoint string, ok bool)
      InstallAgentConfig(ctx context.Context, name, version string, tarball []byte) error
  }
  func (h *agentHost) fetchCheckedConfig(ctx context.Context, containerName, ifVersion string) (AgentConfigBundle, error) // Fetch + Walk(bundleSchema)
  func (h *agentHost) deliverConfig(ctx context.Context, containerName string, containerID runtime.WorkloadID, lastVersion string) (string, error)
  ```

  It edits `provisionVsockGateway` per §(d) and `refreshOneContainer` per
  §(e), deletes the gate, and rewrites the doc comments on
  `vsockGatewayEngine` and `RefreshConfig` (whose "MountLabel, Materialize"
  wording becomes backend-dependent). `vsockGatewayFakeRuntime`
  (`go/internal/runner/host_vsock_gateway_test.go`) records installs, with an
  injectable error.
- **Test cycle:**
  - **Client** (`fakeGuest`, `go/internal/runtime/microvm/exec_test.go`): the
    version frame comes first, then 1.5 MiB arrives as three chunks;
    `CodeUnimplemented` yields `ErrGuestImageTooOld`.
  - **Runner.** These replace `TestVsockRefreshConfigSkipsProbedSession` and
    the "no config version was seeded" block in
    `TestVsockProvisionServesAtSuffixedPathWithNoRefusedMounts`:
    - Provision installs (name, v-1), seeds v-1, and has no config mount.
    - A bad-schema bundle, or one over `MaxWireBytes`, fails Provision with no
      `create` call.
    - An install failure closes the socket, records stop and remove, and
      errs.
    - Refresh from v-1 to v-2 installs v-2 and reaches `exec_streaming` == 2.
    - Refresh with lastVersion v-1 against a version-only v-1 fetch makes no
      install call, no reload, and keeps `exec_streaming` at 1.
    - An install failure on refresh neither reloads nor advances the version.
  - The parity suites are green.

### T4 — KVM acceptance

- **Interfaces:** consumes T1–T3. `w3Relay`
  (`go/internal/runner/e2e_vsock_gateway_microvm_test.go`) serves a settable
  bundle and honours `if_version`.
- **Test cycle** (`microvmtest.Require`), as the agent uid:
  - after Provision, `current/version` reads v-1;
  - `touch` inside `current/` and `ln -sfn` over `current` both fail;
  - after v-2 is published and `RefreshConfig` runs, it reads v-2 and the
    session stream has been replaced.

## Tasks

- [ ] T1 — `agentconfig` safety kernel + `Tree`; Runner keeps `bundleSchema`; `Fetch`
- [ ] T2 — `InstallAgentConfig` proto + gen + guestd handler + client fakes
- [ ] T3 — host client + `ErrGuestImageTooOld`, probe extended, Provision + refresh wired, gate removed
- [ ] T4 — KVM acceptance: install, agent cannot write, refresh + reload

## Open Questions

The body is designed against each recommendation. The former OQ-3 moved to
Alternatives, and OQ-4 is resolved by `if_version`.

- **OQ-1 (load-bearing): push, or a read-only virtio-fs share?**
  **Recommendation:** push (DL-478, pending Matt). See Alternatives.
- **OQ-1b (load-bearing): where does the bundle schema live?**
  - (i) The full schema runs in the guest. This gives one validator, but every
    schema change needs a lockstep guest-image release, or microVM Provision
    fails.
  - (ii) Split: the Runner checks the schema before the push, and guestd runs
    only the safety kernel.

  **Recommendation:** (ii). The allow-list is a contract, not a boundary, and
  only the host reaches the RPC.
- **OQ-2 (non-load-bearing): clear-to-empty.** On podman, an empty-version `Materialize` keeps
  the old `current`, and guestd mirrors that. **Recommendation:** keep parity
  here; fix both tiers in one follow-up.
- **OQ-5 (non-load-bearing): version path safety.** guestd checks the version, but podman's
  `Materialize` joins the Server version into a path unchecked.
  **Recommendation:** harden it in a separate issue (podman is frozen in this
  slice).
- **OQ-6 (non-load-bearing): guest RAM.** `--microvm-memory-mb` defaults to 0 ("VMM default"),
  but `vmmArgs` (`go/internal/runtime/microvm/launch.go`) always passes
  `"--memory", "size=" + strconv.Itoa(cfg.MemoryMB) + "M,shared=on"`, so 0
  reaches cloud-hypervisor as `size=0M`. The worst-case
  install holds the wire tarball plus two version dirs on tmpfs before the
  prune. **Recommendation:** a floor (0 resolves to 1024 MiB in `bootConfig`,
  and non-zero values below 512 are rejected), filed as its own issue. This
  slice does not depend on it: an install that exhausts RAM fails Provision
  closed and visibly.
