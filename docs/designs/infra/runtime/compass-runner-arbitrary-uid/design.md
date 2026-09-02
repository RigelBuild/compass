# Design: compass-runner arbitrary host uid support

Status: Draft

## Problem / Intent

The embedded compass-runner refuses to start unless its real uid is 1000, which
blocks hosted/GA deployments where the host uid is arbitrary
(`docs/designs/ui/compass-native-app/design.md` §OQ5 froze the split:
preflight-and-refuse is the interim, arbitrary-uid is the GA-blocking
follow-up — this record). The **launch mechanism is already decided and
frozen** in the Active record
`docs/designs/agent/compass-agent-container-runtime.md` — T1's uid-mapping
invariant (`:630-640`), T2's flag switch (`:668-670`: "``Create``
(`podman.go:347-357`) switches `--userns=keep-id` to
`--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>`" — `Create` is now at
`podman.go:355`), and OQ1 (`:1052-1061`).
This record does not re-decide it. It decides **only the residuals T1/T2 left
open**: the disposition of the `verifyRunnerUID` refusal, the podman version
floor, the bind-mount ownership and init-user analyses under the remap, and the
regression test targets.

## Global Constraints

- Rootless podman only — no daemon, no root, no rootful fallback
  (`go/internal/runtime/podman.go:23-24`: "Rootless is a hard requirement
  (compass.md §5.3, §7.1): no daemon, no root, no rootful fallback").
- No behavior change to how agent work runs unprivileged inside the container:
  every agent exec keeps its explicit `--user <Workspace.UID>`
  (`go/internal/runtime/agent.go:187,233,306,322` all call
  `AsUser(strconv.FormatUint(uint64(...UID), 10))`).
- Podman **≥ 4.3** is the version floor: `--userns=keep-id:uid=UID,gid=GID` is
  a 4.3+ option (see OQ-B). Verified available: dev box 5.8.4 (`podman
  --version` this session), CI `ubuntu-latest` 4.9.3.
- **Scope boundary — the uid slice only.** T2 of
  `compass-agent-container-runtime.md` bundles the flag switch with a much
  larger rework (delete `cloneRepo` + the `Workspace` clone surface,
  `AllowAllEgress`, the `direnv exec` wrapper). All of that is a **non-goal**
  here; this record ships only the userns flag change, the guard disposition,
  and the tests.
- AGENTS.md: no issue-id / planning-metadata in code or comments; name the
  defect directly. Code comments may cite this record by path.
- Tests are red-first (`rule://red-green-testing`): the guard relaxation needs
  a regression test proving a uid≠1000-shaped launch produces a container
  whose agent is uid 1000 and owns `/nix` (red before the flag change, green
  after).

## Approach

Implement the frozen T1/T2 launch mechanism as a thin slice —
`--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>` in `PodmanCLI.Create` —
and remove/replace the now-wrong startup refusal. Five residual decisions,
each grounded below: (a) and (b) were load-bearing forks Matt ruled on (both
as the record's chosen option — see Open Questions); (c), (d), (e) are decided
here.

### (a) `verifyRunnerUID` disposition — replace with a podman-floor preflight (OQ-A)

Today `go/cmd/compass-runner/main.go:93` runs
`if err := verifyRunnerUID(os.Getuid()); err != nil { return err }`, and
`:178-188` refuses any uid ≠ `defaultAgentUID` (`:163`, `const defaultAgentUID
uint32 = 1000`):

> "the runner must run as uid %d, but it is running as uid %d: the agent image
> bakes the agent user, /nix and $HOME as uid %d, and podman's
> --userns=keep-id maps the host uid into the container unchanged, so an agent
> launched by this runner would not own /nix"

Once `Create` remaps arbitrary→1000, the premise of that message is false and
the refusal must go. Options:

1. **Delete outright.** Simplest, but forfeits the property the guard was
   placed for (`main.go:89-92`: "this validates the process's own identity
   ... an operator on the wrong uid is told to set a token, fixes that,
   re-runs, and only then learns the process can never work as this user") —
   fail at startup with a legible cause, not deep inside the first create.
2. **Replace with a podman version-floor preflight** (chosen): a startup
   check that the engine supports `keep-id:uid=` (podman ≥ 4.3), e.g. parse
   `podman version --format {{.Client.Version}}` (the CLI version is the
   engine version for local rootless podman; remote client/server skew is
   out of scope). Keeps the fail-at-startup legibility; the new failure mode
   this change introduces is exactly "podman too old", so the preflight
   guards the new invariant the same way `verifyRunnerUID` guarded the old
   one.

**Decision: option 2 — Matt ruled REPLACE** (see Open Questions). Option 1
(delete outright) is rejected: it forfeits the fail-at-startup legibility for
the one new failure mode the remap introduces. P2 implements option 2 —
swap the uid refusal for the version preflight, not delete it.

### (b) Podman version floor ≥ 4.3, no `--uidmap` fallback (OQ-B)

`--userns=keep-id:uid=UID,gid=GID` requires podman ≥ 4.3 (podman docs,
verified this session). Grounded availability:

- Dev box: `podman version 5.8.4` (run this session).
- CI: GitHub `ubuntu-latest` ships podman 4.9.3 — though CI never runs the
  podman-gated tests (`.github/workflows/publish-agent-image.yml:30-31`:
  "not executed here — it needs a rootless-podman host, which GitHub-hosted
  runners are not").
- GA runtime hosts: **unpinned**. Podman is the external host's, not shipped
  by the repo — `devenv.nix:415-418` is explicit that even the dogfood loop
  "Uses the host's rootless podman", and `agent-image/` bakes no podman.

**Decision: hard floor ≥ 4.3, no `--uidmap`/`--gidmap` fallback — Matt ruled**
(see Open Questions), enforced by the OQ-A startup preflight and documented in
the runner's operator-facing docs. Rationale: podman 4.3 shipped in 2022, so
the below-floor population is small but not empty — Ubuntu 22.04 LTS (supported
into 2027) ships podman 3.4.x, and GA runtime hosts are unpinned
(`devenv.nix:415-418`), so a 22.04 operator is a plausible GA host that this
floor **will** refuse. That is exactly why the OQ-A preflight's legible startup
refusal (not a deep-in-create failure) is the right handling, and a `--uidmap`
fallback would double the launch-path test matrix to serve it. The error copy
must name the podman floor and the found version.

### (c) Bind-mount ownership round-trip — holds; decided

The rationale for bare `keep-id` is `podman.go:25-26`: "Containers run with
--userns=keep-id so files the agent writes in a bind-mount map back to the
invoking user on the host" (restated at the flag itself, `:363-364`). The
runner's bind-mounts are two kinds: the read-only config/cache mounts
(`ContainerSpec.Mounts`, `podman.go:82-84` — whose "read-only" field comment
is now stale, see P1; the materialized config tree,
`go/internal/runner/config_materialize.go:199-201`), and one **read-write**
mount — the per-container agent gateway socket
(`go/internal/runner/host.go:152`: `spec.Mounts = append(spec.Mounts,
listener.Mount(agentSocketMountPath))`; `gateway/socket.go:278-280`:
`ReadOnly: false`, "the agent must connect()"). The workspace itself is an
in-container clone, not a mount (`podman.go:7-8`: "The agent clones its own
repos ... from inside the container").

`keep-id:uid=N` preserves the round-trip: it maps the **invoking host user**
to container uid N (instead of to the same value), so container-N writes
still land on the host as the invoking user. Verified empirically this
session on podman 5.8.4 (host uid 1000, gid 100):

```console
$ podman run --rm -v $d:/mnt:Z --userns=keep-id:uid=2000,gid=2000 alpine \
    sh -c 'id -u; touch /mnt/probe'   # → 2000
$ stat -c '%u:%g' $d/probe            # → 1000:100  (the invoking host user)
```

For read-only mounts the direction that matters is host→container
readability: the config tree pins 0755/0644
(`config_materialize.go:249-253`), which reads as world for any mapped uid —
unchanged. For the read-write socket mount the invariant is in-container
ownership: the socket "must be OWNED by the mapped agent uid in-container ...
and connect()-able from inside" (`gateway/socket_podman_test.go:5-10`). It
holds by the same mapping — the invoking host user owns the host socket file,
and `keep-id:uid=1000` maps that user to container uid 1000 (the agent), so
the in-container socket is agent-owned and connect()-able. **Decision: the
remap is ownership-correct for both mount kinds; no mount changes needed.**

### (d) Init-user caveat (podman #24934) — no behavioral delta; decided

Podman issue #24934: `keep-id:uid=` also changes the container init user. Here
that is a no-op delta, because **bare `keep-id` already does the equivalent**:
it runs the init process as the invoking user's mapped uid. Grounding:

- The image bakes `Config.User = agent`
  (`forks/devenv/src/modules/containers.nix:265`: `User = "${cfgUser cfg}";`
  with `uid = "1000"` at `:55`; confirmed live: `podman image inspect
  compass-agent:latest --format '{{.Config.User}}'` → `agent`). Init
  (`sleep infinity`, `agent.go:253`) runs as uid 1000 today and still does
  under the remap — the mapped-to uid is 1000 either way.
- The egress-arming exec keeps its identity under the remap. `armEgress` runs
  its nft script via `NewExecSpec` with **no** `AsUser` (`agent.go:284-285`),
  so it runs as the image's default user — `Config.User=agent`, uid 1000 —
  **not** root. It can still arm the firewall because it inherits the
  container's added CAP_NET_ADMIN. Verified this session on a
  `keep-id:uid=1000,gid=1000` container with `--cap-add NET_ADMIN`: the
  default-user exec runs as uid 1000 with `CapEff 0x0000000000001000`
  (CAP_NET_ADMIN only) and `nft add` succeeds. (`podman exec --user 0` is a
  separate datum — uid 0, `CapEff 0x00000000800415fb`, real root-in-userns;
  nothing in the launch path uses it.) All three identities are
  mapping-independent — byte-identical under bare `keep-id` and the remap,
  because `Config.User` resolution and cap assignment do not depend on the uid
  mapping. The `agent.go:282-283` "as root" comment and the `podman.go:95-96`
  `ExecSpec.User` "Nil runs as the image's default user (root)" comment are
  both stale — the image default is uid 1000, not root — and are folded into
  P1's comment sweep.
- Agent execs stay unprivileged: `--user 1000` under the remap yields
  `CapEff 0x0000000000000000` and `nft add` fails "Operation not permitted"
  (both probed this session), preserving the egress integrity model
  (`go/internal/runtime/egress.go:6-10`: the agent "cannot flush or edit the
  ruleset even though the container nominally holds the capability").

Security note (F4-shape): the remap grants no extra host access — it is a
narrowing of the same rootless user-namespace podman already builds from the
invoker's `/etc/subuid`/`/etc/subgid` ranges; no new subuid requirements
beyond what rootless podman already needs (`devenv.nix:202-203` documents the
existing "uid-1000 subuid/subgid ranges" prereq, which generalizes to "the
invoking uid's ranges"). **Decision: no fork; fold as analysis.**

### (e) Regression test targets — decided

- `go/cmd/compass-runner/main_test.go` — `TestVerifyRunnerUID` (`:18-32`)
  currently asserts the refusal; it is **replaced** by the preflight's test
  (see P2).
- `go/internal/runtime/podman_test.go` — a new `createArgs` unit test pinning
  the exact userns token, in the style of
  `TestExecStreamingArgsAssemblesInteractiveExec` (`:72`).
- `go/internal/runtime/userns_remap_test.go` (new, podman-gated via the
  existing `podmanUsable()` skip, `lifecycle_test.go:54-59`) — the red/green
  integration proof: launch with a remap target distinct from the host uid
  and assert in-container identity and `/nix` ownership (see P3).

### Coordination with the container-runtime epic (same lane)

The parent epic that owns T1/T2 is **the same compass-runner lane** as this
record — no cross-agent handoff exists. T2's flag switch is not yet
implemented (`podman.go:365` is still bare `--userns=keep-id` at this
writing), and the epic's in-review PRs cover other tasks. This record
implements **only the uid slice** of T2's flag switch plus the residuals
above; whichever lands first — this thin slice or T2's fuller launch-path
rework — the other rebases the one-line flag change. The T1/T2 invariant
(baked-uid == `Workspace.UID` == mapped in-container uid) is shared and
unchanged by ordering.

### Cross-links (not solved here)

- **Uid SoT consolidation** (should 1000 be one shared const both lanes
  import) is orthogonal — tracked separately; this record keeps
  `defaultAgentUID` (`main.go:163`) and threads it through, no consolidation.
- **App-side preflight mirror**: `go/internal/preflight/preflight.go:98`
  asserts `CurrentUID == ExpectedAgentUID` with the injected value documented
  as "compass-runner's defaultAgentUID, 1000" (`:22-26`). It is a pure
  downstream mirror owned by compass-native; relaxing/parameterizing it to
  match is compass-native's follow-on once this record lands the shape.

## Plan

### P1 — Remap the userns flag in `Create`

Switch `PodmanCLI.Create` from the bare token to the remap, threading the
agent uid into `ContainerSpec` so the flag derives from the same value every
exec already uses (the T1/T2 invariant).

- Changes:
  - `go/internal/runtime/podman.go`: add `UID uint32` to `ContainerSpec`
    (`:70-90`; note `ContainerSpec` has no `User` field — `:92-97` is
    `ExecSpec`), documented as the container uid the invoking
    host user is mapped to. `Create` (`:355-366`) emits
    `fmt.Sprintf("--userns=keep-id:uid=%d,gid=%d", spec.UID, spec.UID)` in
    place of `"--userns=keep-id"` — gid is collapsed to uid because the image
    bakes gid==uid==1000 (`containers.nix:55-56`); a distinct GID field is not
    threaded until an image diverges. Extract the argv assembly into
    `createArgs(spec ContainerSpec) []string` (mirroring
    `execStreamingArgs`, `:635`, "split out so the argv assembly is
    unit-testable without spawning podman").
  - `go/internal/runtime/agent.go` `createAndStart` (`:245-254`): set
    `UID: spec.Workspace.UID` on the `ContainerSpec`.
  - Update the stale comments: `podman.go:25-26` and `:363-364` (the
    "maps back to the invoking user" rationale now reads "the invoking host
    user is mapped to the baked agent uid; files the agent writes in a
    bind-mount still map back to the invoking user"); `podman.go:82-84`
    (`ContainerSpec.Mounts` "read-only host bind mounts" — false, the agent
    socket is read-write, see §(c)); `podman.go:95-96` (`ExecSpec.User` "Nil
    runs as the image's default user (root)" — the image default is uid 1000,
    not root); `agent.go:282-283` (`armEgress` "as root" — it runs as the
    image default user, uid 1000, with CAP_NET_ADMIN); and the
    `agent-image/devenv.nix:69-82` identity comment (it cites bare keep-id
    and the verifyRunnerUID guard).
- Interfaces:
  - `type ContainerSpec struct { ...; UID uint32 }` (new field).
  - `func createArgs(spec ContainerSpec) []string` (new, package-private).
  - `func (p *PodmanCLI) Create(ctx context.Context, spec ContainerSpec) (ContainerID, error)` — unchanged signature.
- Test cycle: new `TestCreateArgsRemapsUserns` in
  `go/internal/runtime/podman_test.go` pinning the exact token
  `--userns=keep-id:uid=1000,gid=1000` for `ContainerSpec{UID: 1000}`.
  Order within P1: extract `createArgs` first (still emitting the bare token),
  commit the test red against the bare token, then flip the flag to green.
  Run `go test ./go/internal/runtime/ -run TestCreateArgs`.

### P2 — Replace `verifyRunnerUID` with the podman-floor preflight

Matt ruled **replace** (OQ-A): swap the refusal for a podman-version preflight,
not delete it.

- Changes:
  - `go/cmd/compass-runner/main.go`: delete `verifyRunnerUID` (`:178-188`)
    and its call (`:93`); in its place call a version preflight at the same
    "ahead of every operator-input check" position (`:89-92` — keep that
    comment's rationale, rewritten for the new invariant).
  - `go/internal/runtime/podman.go`: add the probe on `PodmanCLI` so the
    engine seam owns engine facts.
- Interfaces:
  - `func (p *PodmanCLI) VerifyUsernsRemapSupport(ctx context.Context) error`
    — runs `podman version --format {{.Client.Version}}`, parses
    major/minor, errors below 4.3 with a message naming the required floor
    and the found version (no issue ids).
  - `func parsePodmanVersion(s string) (major, minor int, err error)`
    (package-private, unit-testable without podman).
- Test cycle: replace `TestVerifyRunnerUID`
  (`go/cmd/compass-runner/main_test.go:18-32`) with
  `TestParsePodmanVersion` table cases (4.2 → error shape, 4.3/5.8.4 → ok,
  garbage → error) in `go/internal/runtime/podman_test.go`; assert
  `main.go` no longer refuses on uid (the old test's refusal case goes red
  when deleted first — the red half of the cycle). Run
  `go test ./go/cmd/compass-runner/ ./go/internal/runtime/`.

### P3 — Red-first integration regression: arbitrary-uid launch owns `/nix`

The GA contract in one test: a launch whose remap target differs from the
invoking host uid still yields an agent that is uid 1000-equivalent in-container
and owns `/nix`-equivalent paths. The dev box runs as uid 1000, so the test
inverts the probe: remap to a **non-host** uid and assert the mapping, which
exercises the identical mechanism an arbitrary-host-uid deployment relies on.

- Changes: new `go/internal/runtime/userns_remap_test.go`, gated on
  `podmanUsable()` (the existing skip helper, `lifecycle_test.go:54-59`),
  alpine-based like `config_mount_test.go` (no compass-agent image
  dependency):
  1. `Create`/`Start` a container with `ContainerSpec{UID: <target ≠
     host-uid>}` shape — i.e. drive the real `PodmanCLI.Create` with a `UID`
     distinct from `os.Getuid()` — and assert `id -u` inside equals the
     spec'd UID (the remap maps host→spec'd uid).
  2. Bind-mount a tempdir, write a file as the container user, assert the
     host file is owned by the invoking host uid (the round-trip of §(c)).
  3. Against the real `compass-agent:latest` when present (skip otherwise,
     mirroring `agentImageExists()` in
     `go/internal/runner/config_delivery_e2e_test.go`), assert
     `stat -c %u /nix` inside == 1000.
- Interfaces: test functions only —
  `TestKeepIDRemapMapsHostUIDToSpecUID`,
  `TestKeepIDRemapBindMountRoundTrip`,
  `TestKeepIDRemapAgentOwnsNix`.
- Test cycle: red against bare keep-id (case 1 fails: in-container uid is the
  host uid, not the spec'd one), green after P1. Run
  `go test ./go/internal/runtime/ -run TestKeepIDRemap`.

Ordering: P1 → P2 (the preflight replaces the guard only once the launch no
longer needs it) → P3 lands with P1 in the same PR (red-first: commit the
test red, then the flag). OQ-A is resolved (REPLACE), so nothing gates P2;
it implements the preflight per that ruling.

## Tasks

- [ ] P1 — `ContainerSpec.UID` + `createArgs` extraction +
      `--userns=keep-id:uid=,gid=` in `Create`; comment sweep
      (`podman.go:25,363`, `agent-image/devenv.nix:69-82`);
      `TestCreateArgsRemapsUserns` green.
- [ ] P2 — delete `verifyRunnerUID` + call site; add
      `PodmanCLI.VerifyUsernsRemapSupport` / `parsePodmanVersion` +
      startup wiring; `TestParsePodmanVersion` green, `TestVerifyRunnerUID`
      removed.
- [ ] P3 — `userns_remap_test.go`: remap-mapping, bind-mount round-trip,
      agent-owns-`/nix` integration tests, red-first with P1.

## Open Questions (resolved)

Both were load-bearing forks; **Matt ruled on both** — each as the record's
chosen option, so the Approach and Plan above already reflect the
decisions.

1. **OQ-A — `verifyRunnerUID`: delete vs replace? → REPLACE (Matt).** The
   uid==1000 refusal (`main.go:93,178-188`) is wrong once launch remaps
   arbitrary→1000. **Decision: replace** it with a startup podman-version
   preflight (≥ 4.3, the new invariant the launch depends on), per P2 — it
   preserves the guard's stated purpose (`main.go:89-92`, fail at startup with
   a legible cause rather than deep in the first create) against the one new
   way this change can fail.

2. **OQ-B — podman floor: hard ≥ 4.3, or a `--uidmap` fallback for < 4.3? →
   HARD FLOOR ≥ 4.3, NO FALLBACK (Matt).** `keep-id:uid=` needs podman ≥ 4.3.
   Dev box (5.8.4) and CI (4.9.3) clear it; GA runtime hosts are unpinned
   (podman is the host's, not shipped — `devenv.nix:415-418`). **Decision:
   hard floor ≥ 4.3**, enforced by the OQ-A preflight and documented for
   operators; no `--uidmap` fallback. The known below-floor case is Ubuntu
   22.04 LTS (podman 3.4.x, supported into 2027), a plausible unpinned GA host
   the floor will refuse; a legible startup refusal is the right handling, and
   a fallback would double the launch-path test matrix.
