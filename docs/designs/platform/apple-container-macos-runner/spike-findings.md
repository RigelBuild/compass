# T-1 spike findings — apple `container` on real hardware

Deliverable of T-1 in [design.md](design.md). Run 2026-09-05 on the committed
mac mini (`mattmini`), the hardware design.md:368-369 names as committed. Verdict
per open question, with measured numbers and the transcript-level evidence each
verdict rests on.

**Headline: the apple-container direction is GREEN. The vsock transport is
not — and that returns to Matt.** Three of six probes ran to a full verdict; OQ-1,
OQ-5 and OQ-12 are partial (the `/nix` + `$HOME` leg, OQ-7's
memory-growth-over-session measurement, and the darwin preflight leg are unexercised
and carried forward). None challenges the ruled direction (apple-container as the
macOS engine). But the load-bearing transport probe found the ruled *transport*
unreachable: the CLI exposes no host-side vsock attach point, so the guestd-style
unix→vsock forwarder cannot be ported onto this backend through the documented CLI
surface. design.md:612-613 states the disposition for exactly this outcome — "If the
vsock leg is NOT reachable through the CLI, the transport question (not the
apple-container direction) returns to Matt with the finding" — and OQ-11
(design.md:712-714) is a Matt ruling ("RULED: yes, vsock, mirroring the microVM").
**That trigger has fired.**

A working substitute exists and is proven end to end (the CLI's own
`--publish-socket`, running **guest-binds / host-dials** — the inverse of
today's gateway), but adopting it changes a ruled item, so it is a
**recommendation awaiting Matt's ruling**, not a decision this record makes.
T-2 should not build the transport against it until Matt rules.

**Not closed by this spike:**

- OQ-1's `/nix` + `$HOME` usability leg, pending an arm64 compass-agent image.
- OQ-12's darwin preflight leg, carried to T-4.
- OQ-7's memory-growth-over-session measurement.

Two further adjustments are genuinely T-2 shape changes that reopen nothing
already settled and in fact *simplify* the plan: virtiofs makes the
`--userns=keep-id` port unnecessary for the host-side ownership round-trip (the
`/nix` + `$HOME` leg is unverified, see item 5), and the egress arming identity
moves to the root-arms-then-agent-drops model
`go/internal/runtime/egress.go:6-10` already specifies. The spike also turned up
**four open prerequisites** that are not shape changes: T-2's stack port needs
work on both halves — postgres's bind-mount socket shape is RED here, and the
collector's TCP-publishing path went unprobed — no arm64 `compass-agent` image
exists, which blocks the live legs of T-2/T-3, and every probe ran on `container`
1.1.0 rather than the current 1.3.1. All seven are detailed under
[Consequences for T-2..T-5](#consequences-for-t-2t-5).

## Host + toolchain

| Fact | Value |
| --- | --- |
| Host | `mattmini`, `Mac14,12` (M2 Pro mini), arm64 |
| macOS | 26.5.1 (build 25F80) |
| `kern.hv_support` | `1` |
| `container` | **1.1.0** (build: release, commit `5973b9c`) |
| Guest kernel | 6.18.15 aarch64 (kata-static 3.28.0) |
| Provisioning | `nix shell github:nixos/nixpkgs/nixos-unstable#container` |

The record's passing installer remark (design.md:327-329, an aside inside the
substrate-invariant bullet noting that "the installer requiring admin once to
place files under `/usr/local` is an install-time cost") turns out to be
unnecessary — no `.pkg` is involved. nixpkgs-unstable packages apple/container
for `aarch64-darwin` at 1.1.0, clearing the ≥ 1.0.0 floor design.md:313-319
sets. The spike ran entirely from a `nix shell` — no sudo, no host mutation,
cached-binary fetch in ~12 s. The declarative follow-up is filed as RIG-3352
(see [Provisioning](#provisioning-the-clis-real-path)).

One-time host state the CLI does create on first `system start`: it registers a
`container-apiserver` launchd job and wants a default guest kernel, installed
non-interactively with `container system kernel set --recommended` (a bare
`container system start` **prompts** and dies on a non-TTY stdin with
`Error: failed to read user input` — relevant to any future CI use).

## Verdicts

| OQ | Probe | Verdict | One-line result |
| --- | --- | --- | --- |
| OQ-1 | (a) uid mapping | **GREEN (partial)** | ownership round-trip better than podman; the `/nix` + `$HOME` leg is UNPROBED (no arm64 image manifest) |
| OQ-2/OQ-11 | (b) transport | **direction GREEN; ruled vsock transport RED → returns to Matt** | no host-side vsock attach point in the CLI; `--publish-socket` is a proven substitute, guest-binds/host-dials |
| OQ-3 | (c) egress arming | **GREEN (already the documented model)** | caps silently dropped at uid≠0, but `egress.go:6-10`'s root-arms-then-agent-drops model is exactly what works |
| OQ-4 | (d) streaming exec | **GREEN** | streaming, stdin, exit-code and signal semantics all match the `ChildHandle` contract |
| OQ-5 | (e) timings | **GREEN (partial)** | 721-952 ms warm start; 2.6-2.9 MiB idle per container VM; OQ-7's memory-growth-over-session measurement (design.md:681 assigns it to T-1(e)) was not taken |
| OQ-12 | (f) runner-on-darwin | **GREEN (partial)** | cross-builds + runs natively on macOS 26; darwin `sun_path` budget measured at 34, matching `socket.go:138-139`; the darwin preflight leg is unexercised and carried to T-4 |

### OQ-1 — uid mapping: GREEN (partial); ownership simpler than podman

virtiofs performs identity translation at the boundary, so the podman
`--userns=keep-id:uid=,gid=` machinery (`podman.go:25-27`) has **no analogue
and needs none**:

- Guest running as uid 1000 wrote `/w/mapped.txt`; the host saw it owned by
  `501:0 <invoking-user>:wheel` — the invoking macOS user, which is the property
  the record wanted.
- A file created host-side appeared in-guest as `1000:1000` and was readable.
- Same translation with **no** `--uid` flag (guest default is uid 0): host file
  still landed as the invoking user.

So the "fixed/root uid" failure branch the record hedged against does not
occur. `--uid/--gid/--user` exist and set the in-guest process identity; they
are not needed for host-side ownership correctness.

**Not probed: the `/nix` + `$HOME` leg.** T-1(a) (design.md:375-381) asks two
questions, and only the ownership round-trip above is answered. Every OQ-1
probe ran against `docker.io/library/alpine:3.20`, not the compass-agent image,
so it cannot speak to whether the baked-at-uid-1000 `/nix` store and `$HOME`
are usable. Attempting the real image surfaced a **separate T-2 blocker**:

```text
image=ghcr.io/rigelbuild/compass-agent:latest
Error: image sha256:0b1f9901… does not support required platforms
```

The published compass-agent image has no `linux/arm64` manifest, so it cannot
run on this backend at all. That is an image-supply-chain prerequisite for
T-2/T-3 (a multi-arch build), independent of the runtime work, and it means the
`/nix` + `$HOME` leg stays open until an arm64 agent image exists.

### OQ-2/OQ-11 — transport: direction GREEN, ruled vsock transport RED

Three candidates were probed end-to-end with a purpose-built static Go
dialer/listener pair (Alpine's busybox `nc` has no `-U`, so shell probes cannot
settle this).

**1. Raw AF_UNIX over a virtiofs bind-mount — RED.** The host-side listener was
created before container start (the `socket.go:11-12` ordering), and in-guest:

```text
ls: /w/gw.sock: Not supported
socket_visible_in_guest=no
```

This **confirms the `compass-local-dev/design.md:194-199` limitation HOLDS** on
this backend — the hazard obtains rather than dissolving, and the record was
right not to assume otherwise (T-1(b)'s "explicitly-secondary datapoint").
design.md:240-245 argued the hazard dissolves *because* the ruled vsock
transport takes the socket off the virtiofs path; that argument dies with vsock
(candidate 2 below), so it is re-derived for `--publish-socket` in candidate 3.
The consequence for T-2's **postgres** port — which relies on exactly this
bind-mounted-socket shape — is spelled out in
[Consequences](#consequences-for-t-2t-5) item 2. The collector does not use a
unix socket and is unaffected by this result; its own open question is item 3.

**2. Raw AF_VSOCK from the guest — family present, no usable listener path.**
The guest kernel has full vsock support (`/dev/vsock` present as
`crw------- 10, 256`; `CONFIG_VSOCKETS=y`, `CONFIG_VIRTIO_VSOCKETS=y`), and
`socket(AF_VSOCK, SOCK_STREAM)` **succeeds** in-guest. But connecting to the
host CID fails:

```text
PROBE_INFO vsock socket(AF_VSOCK) created
PROBE_ERR vsock connect cid=2 port=1024: connection reset by peer
```

A reset rather than `ENODEV` or a timeout indicates the host vsock stack is live
and answering. The CLI exposes no flag to bind a host-side vsock listener
(`container run --help` has no `vsock` surface; no top-level vsock subcommand).
So the record's literal T-1(b) plan — port the guestd unix→vsock forwarder
(`gateway_proxy.go:29-33`) onto this backend — has **no host-side attach point
through the CLI**. Reaching VZVirtioSocketDevice directly would mean bypassing
the CLI for the Virtualization.framework API, which is out of scope for the
ruled "drive the `container` CLI" approach.

**3. `--publish-socket` — GREEN, full bidirectional round-trip.** design.md:609
already cited this flag, as proof that host↔guest forwarding exists as a
first-class mechanism at all. The spike's finding is stronger and narrower: it
is not merely evidence for vsock's plausibility, it is the **only channel probed
to a full round-trip**:

```text
--publish-socket <spec>   Publish a socket from container to host
                          (format: host_path:container_path)
```

Proven end-to-end, as uid 1000, twice in one container:

```text
LISTENER_OK bound /tmp/gw.sock            # in-guest Go listener
HOST_ROUND_TRIP_1=b'PONG-FROM-GUEST:PING-1'
HOST_ROUND_TRIP_2=b'PONG-FROM-GUEST:PING-2'
LISTENER_RECV "PING-1"                    # guest-side confirmation
LISTENER_RECV "PING-2"
```

Semantics, pinned precisely (each cost a failed attempt, so they are recorded
for the T-2 executor):

- **Direction is inverted from today's gateway.** "From container to host" means
  the **guest binds** `container_path` and the **host dials** `host_path`. Today
  the host listens at Provision and the guest dials
  (`go/internal/runner/gateway/socket.go:4-13`). This is the one real contract
  change in the whole spike.
- **An identical host and container path works** — the invariant postgres needs.
  Publishing `/tmp/.s.PGSQL.5432:/tmp/.s.PGSQL.5432` round-tripped:
  `LISTENER_OK bound /tmp/.s.PGSQL.5432` then
  `SAME_PATH_ROUND_TRIP=b'PONG-FROM-GUEST:PG-SHAPED'`. The parent directory must
  already exist guest-side: an earlier attempt with a host `mktemp -d` path
  failed `bind: no such file or directory`, because publishing creates the
  socket, not its parent dirs.
- **The CLI creates `host_path`; it must not pre-exist.** Pre-creating it fails
  with `Error: host socket <path> already exists and may be in use`. The host
  socket appears immediately at container start (`srwxr-xr-x`, `S_ISSOCK` true)
  and a host dial `connect()`s even before the guest has bound — so a
  successful host-side connect is **not** evidence the guest is listening;
  readiness needs an application-level handshake, not a connect check.
- **`container_path` is lazily materialised** — absent in-guest at startup
  (`guest_path_exists=no`), so the guest must bind it itself.
- **The guest path must be in a guest-writable dir.** Binding `/run/gw.sock` as
  uid 1000 fails (`bind: permission denied`, `/run` is root-owned); `/tmp` works.

**Disposition: this returns to Matt.** The real option set is:

1. **`--publish-socket` — proven end to end.** It costs the guest-binds/host-
   dials inversion of `gateway/socket.go:4-13` plus an application-level
   readiness handshake, because host connect alone does not prove the guest is
   listening.
2. **`-p/--publish` TCP loopback — surface confirmed present but UNPROBED.** It
   keeps the host-listens direction, but puts the gateway on an in-namespace IP
   hop and therefore re-couples OQ-2/OQ-3; it would need an nft allowlist
   carve-out, unlike the socket path.
3. **`--ssh` — named at design.md:609, NOT probed.** It would keep the
   CLI-driven approach, but layers an SSH server and key material into the
   guest, which the socket path avoids; settling it needs a further probe.
4. **Bypass the CLI for Virtualization.framework — out of scope** under the
   ruled "drive the `container` CLI" approach.

Recommendation: adopt `--publish-socket`, since it is the only option proven
end to end without bypassing the CLI or reopening the nft coupling. It remains
a *proven substitute*, not an adopted decision: it changes ruled behaviour.
T-2 should hold the transport leg until Matt rules; nothing else in T-2 is
blocked by it.

### OQ-3 — egress arming: GREEN, and it is already the documented model

Capabilities are silently dropped for any non-zero uid on this backend, so the
*podman* arming identity — the image's default user (uid 1000) **with**
CAP_NET_ADMIN and no `--user` (`agent.go:319-328`, whose doc comment states it
verbatim) — cannot arm here. Full matrix:

| Flags | `CapEff` | uid |
| --- | --- | --- |
| root + `--cap-add CAP_NET_ADMIN` | `a80435fb` | 0 |
| root, no `--cap-add` | `a80425fb` | 0 |
| `--uid 1000 --gid 1000 --cap-add CAP_NET_ADMIN` | **`0000000000000000`** | 1000 |
| `-u 1000 --cap-add CAP_NET_ADMIN` | **`0000000000000000`** | 1000 |
| `--uid 0 --gid 0 --cap-add CAP_NET_ADMIN` | `a80435fb` | 0 |
| `--uid 1000 --gid 1000 --cap-add ALL` | **`0000000000000000`** | 1000 |
| `--cap-add ALL` (root) | `000001ffffffffff` | 0 |

`CapAmb` is `0` in every case, so there is no ambient-capability path to carry
NET_ADMIN across the uid drop, and `--cap-add ALL` does not help. Symptom as the
production identity: `nft: not found` then `arm_as_uid1000=DENIED`.

**This costs nothing, because arm-as-root-then-drop is what the codebase
already specifies.** `go/internal/runtime/egress.go:6-10` states the integrity
model verbatim: the container "is granted NET_ADMIN only so a **root
entrypoint** can arm nft; the agent then runs as a non-root user whose
capability set is empty, so it cannot flush or edit the ruleset", and "never run
the agent as container-root". Podman's uid-1000 arming is the deviation from
that written model; this backend simply cannot deviate. Probed to completion —
armed as root (default-deny output chain + DNS-only allowlist), then dropped to
uid 1000:

```text
armed=yes (default-deny + DNS-only allowlist)
dns53=allowed                      # allowlisted destination passes
https443=blocked_good              # non-allowlisted TCP/443 blocked
http80=blocked_good                # non-allowlisted TCP/80 blocked
https443_after_allow=allowed_good  # runtime allowlist addition takes effect
unpriv_flush=no_good               # dropped user cannot flush the ruleset
unpriv_read=no                     # nor read it
```

So every property `go/internal/runtime/egress.go:6-10` cares about holds —
default-deny, allowlist carve-outs, and ruleset integrity against the workload
user. Only *who runs the arming step* changes.

**OQ-2/OQ-3 coupling: dissolved, as the record predicted, for a different
reason.** With default-deny armed, the published gateway socket still carried a
full round-trip:

```text
GATEWAY_UNDER_DENY=b'PONG-FROM-GUEST:PING-UNDER-DENY'
```

The record's reasoning was that vsock is a virtio device rather than an
in-namespace IP hop, so netfilter cannot see it. The conclusion holds for
`--publish-socket` too — it is out-of-band of the guest's netfilter and needs
**no allowlist carve-out**. The dissolution survives the mechanism change **for
`--publish-socket`** (a socket forwarded out-of-band of the guest's netfilter),
but would **not** survive a TCP-loopback transport (`-p/--publish`), which is an
in-namespace IP hop needing an nft allowlist carve-out; if the transport ruling
lands there, OQ-3's coupling re-opens.

### OQ-4 — streaming exec: GREEN

Against a detached container, every leg of the `ChildHandle` kill/wait contract
(`podman.go:217-233`, `:253-273`) behaved:

```text
tick-1 / tick-2 / tick-3      # incremental stdout, 1s apart
guest_read=STDIN_PAYLOAD      # stdin delivered via exec -i
exit42_observed=42            # exit code passthrough
sigterm_exit=143              # 128+SIGTERM, distinguishable from a normal exit
```

`container exec` carries `-i`, `-t`, `--uid/--gid/--user`, `-w/--workdir`,
`--env/--env-file`, `--ulimit`, `-d/--detach` — a superset of what the interface
needs. The OQ-4 kill/exit-code semantics are pinned to `container` 1.1.0; they
remain unverified on 1.2.x/1.3.1, and T-2 must re-verify them on 1.3.1.

### OQ-5 — timings + footprint: GREEN (partial)

| Measure | Value |
| --- | --- |
| Cold first run (image pull + kernel + init fetch) | 5.11 s |
| Warm `run --rm` create→exit, 3 consecutive | **952 / 721 / 729 ms** |
| Idle memory, one running container VM | **2.60-2.94 MiB** of a 1.00 GiB cap |
| Idle CPU | 0.13-0.14 % |
| Default resources per container | 4 vCPU, 1 GiB (`memoryInBytes=1073741824`) |

Sub-second warm start substantiates the record's "sub-second boot" claim. On
footprint, note what the idle number is and is not: 2.6 MiB is a resident
sample for an *idle* guest, a lower bound that will not hold with an agent
workload resident. Concurrent-capacity planning should use the **1 GiB per-VM
cap** plus OQ-7's caveat that Virtualization.framework's partial memory
ballooning does not return freed guest pages to the host
(design.md:673-682) — not the idle figure. These are the numbers T-5's flip
brief carries, with that scoping.

CLI output-format notes for T-2's parsers: progress renders as repeated
`[N/6] <phase> [Ns]` step lines on stderr (needs filtering in captured output);
`container stats` re-prints its header per sample; `container inspect` returns a
JSON **array**, with resources under `configuration.resources`.

### OQ-12 — runner-on-darwin: GREEN (partial)

`compass-runner` is already `//go:build unix` (`compass-runner/main.go:1`), and
the run verdict the record asked for is positive:

- Cross-build `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/compass-runner`
  succeeds (28.6 MB binary).
- Copied to `mattmini` and executed natively on macOS 26: `--help` renders the
  full flag set and exits **0**.
- **Darwin `sun_path` budget measured on the host**, the first hazard T-1(f)
  names (design.md:410-413). A darwin/arm64 probe binary reports
  `sunPathMax=103` and a runtime-dir budget of **34** — matching
  `socket.go:138-139`'s documented 34-on-darwin exactly (linux measures 107/38
  on the same probe) — and the default `/run/compass` (12 bytes) fits.

**Scope of this verdict.** `--help` returns at `flag.Parse()`
(`compass-runner/main.go:84`), while backend selection and preflight run later
at `:106-112` (`selectEngine` then `verifyBackendPreflight`), so the second
hazard T-1(f) names — that no podman host-capability preflight fires on darwin
(`main.go:89-100`) — is **not** exercised by a `--help` run and stays open for
T-4, which owns the darwin preflight probe. The `sun_path` leg above is closed
with measured evidence.

This confirms the ruled host-side-runner topology (OQ-12) on real hardware. Note
`-backend` currently documents only `'podman' (default, transitional)` and
`'microvm'` — the `SelectBackend` case T-2 adds is what makes this binary
useful on a Mac.

## Provisioning: the CLI's real path

No `.pkg` is needed. `container` is in nixpkgs-unstable for `aarch64-darwin`,
and `mattmini` is nix-darwin managed declaratively from the **private
infrastructure monorepo's fleet flake** (its `darwinConfigurations."mattmini"`
entry), whose mac-runner overlay list already has the exact
pull-a-package-from-unstable pattern this needs — the same mechanism that
already sources `jujutsu` and the CI agent from unstable. Those definitions
live outside this repository, so they are named by role rather than path; this
repo's only flake is `./flake.nix` at root.

Probes ran on `container` **1.1.0** (what nixpkgs-unstable currently packages
for `aarch64-darwin`); upstream is **1.3.1**. design.md:645-648 requires the
current release, not docs. The kill/exit-code semantics and CLI output-format
notes for T-2's parsers (OQ-5) are therefore pinned to 1.1.0 and unverified
on 1.2.x/1.3.1.

Version note for whoever lands it: the host's pinned `nixpkgs-darwin` has
`container` at **0.12.3**, *below* the ≥ 1.0.0 floor design.md:313-319 sets, so
it must come through the unstable overlay (1.1.0), not the default pin. T-2
must re-verify on 1.3.1, alongside the other prerequisites.

That flake is repo-infra rather than this lane, so the declarative change is
filed as **RIG-3352** for the owning lane rather than made here. Nothing in
T-2..T-5 blocks on it — `nix shell` covers spike and dev use.

## Consequences for T-2..T-5

The apple-container **direction** is unchanged. One **ruled** item does change
and is escalated rather than decided here (item 1). The rest are T-2 shape
changes.

1. **Transport: the ruled vsock leg is unreachable — awaiting Matt's ruling.**
   The guestd unix→vsock forwarder has no host-side attach point through the
   CLI, which fires design.md:612-613's escalation trigger against OQ-11's
   ruling (design.md:712-714). The recommended substitute is `--publish-socket`,
   publishing a per-session socket so the **agent binds in-guest** while the
   **host-side runner dials** — inverting `gateway/socket.go`'s current
   host-listens/guest-dials ordering for this backend. Design consequences if
   adopted: the host socket path must not pre-exist; a host connect succeeding
   does not imply guest readiness, so the gateway needs an application-level
   handshake; and the guest-side path must sit in a guest-writable directory
   whose parent already exists. **T-2 holds this leg until Matt rules.**
2. **T-2's postgres port needs a new socket plan** (design.md:444-450, Matt's
   OQ-13 ruling). Postgres's contract is a host unix-socket directory
   bind-mounted into the container *at the same path*, with the host opening the
   byte-identical `host=<SocketDir>` DSN (`go/internal/stack/postgres_container.go:45-48`;
   the mount itself at `adapters/postgres_container.go:183`). The spike proved
   that bind-mount shape **RED** on this backend, so the port cannot ride
   virtiofs. `--publish-socket` is the candidate and its direction fits
   (postgres binds in-container, the host dials), and an identical
   host/container path was verified working — but this was probed with a
   stand-in listener, not real postgres, so treat it as a strong lead needing
   T-2 confirmation rather than a settled design.
3. **The collector is a different problem, and this spike did not probe it.**
   Unlike postgres it uses no unix socket at all: it publishes three TCP
   loopback ports (`-p …:4317`, `:4318`, `:13133`) and bind-mounts only a
   read-only config file (`adapters/collector_container.go:154-165`), with
   readiness an HTTP GET against the health endpoint (`:120-122`). Its real
   requirement, **host-side TCP port publishing on apple-container, was never
   exercised here**; the CLI does carry
   `-p/--publish [host-ip:]host-port:container-port[/protocol]`, so the surface
   exists, but behavior remains a T-2 prerequisite.
4. **Egress arming runs as root, then drops to the agent user** (T-2). The
   capability matrix shows `CapEff` **`0000000000000000`** for every uid-1000
   invocation, including `--cap-add ALL`, and records `nft: not found` /
   `arm_as_uid1000=DENIED`. This falsifies design.md:195-197's premise that
   `AgentRuntime.armEgress`'s nft exec path "runs unchanged", because
   capabilities are dropped for any non-zero uid. `AgentRuntime.armEgress`
   (`go/internal/runtime/agent.go:319-328`) is shared across backends and execs
   `NewExecSpec("sh", "-c", egress.NftScript())` with no user parameter. T-2
   must choose a per-backend arming identity or the `inGuestEgressArmer` marker
   (`agent.go:298-300`) that design.md:454-455 explicitly declines for this
   backend. That is a real T-2 design choice. Either branch is contained:
   `NftScript()` and the `egress.go:6-10` integrity model stay byte-for-byte
   on both. Note the asymmetry in blast radius: widening the shared seam also
   runs on the podman path (`agent.go:319-328` is shared and `PodmanCLI`
   carries no `EgressArmedInGuest` marker, `agent.go:294-300`), so that
   branch needs podman regression cover; the marker branch skips `armEgress`
   entirely (`agent.go:308-312`) and touches podman not at all.
5. **Scope the `--userns=keep-id` conclusion** (T-2). The flag has no analogue
   on this CLI; virtiofs supplies the host-side ownership round-trip it was
   needed for (verified on `alpine:3.20`). The other property — a `/nix` store
   and `$HOME` baked at uid 1000 being usable by the in-guest process — is
   unverified and rides item 6. If it fails on the arm64 agent image, T-2 may
   still need an ownership-fixup or named-volume workspace model
   (design.md:595-598).
6. **An arm64 compass-agent image is a prerequisite** for T-2/T-3's live legs.
   `ghcr.io/rigelbuild/compass-agent:latest` has no `linux/arm64` manifest,
   which also leaves T-1(a)'s `/nix` + `$HOME` leg open.
7. **Re-verify the CLI contract on 1.3.1** (T-2). All probes ran on `container`
   1.1.0 (the nixpkgs-unstable package for `aarch64-darwin`); upstream is
   1.3.1, and design.md:645-648 requires testing against the current release,
   not docs. The kill/exit-code semantics (OQ-4) and the CLI output-format
   notes for T-2's parsers (OQ-5) are pinned to 1.1.0 and unverified above it.
   The discrete `create`/`start`/`stop`/`rm` argv and stop-timeout semantics
   were not separately transcripted and are re-verified here too.

Unaffected: the exec/kill contract, the host-side-runner topology, and T-5's
flip criteria — now backed by real numbers. On the
`SelectBackend`/`ContainerRuntime` seam, all nine verbs are accounted for: six
were exercised incidentally through the `run`/`exec` probes (Create, Start,
Exec, ExecStreaming, Stop, and Remove), and `Exists` was driven directly
(`container inspect <name>` exits 0 in any state and 1 with a distinguishable
`container not found` when absent — exactly the `Exists(ctx, name) (bool,
error)` contract at `podman.go:378-379`). The remaining two, `MountLabel` and
`Resize`, were not exercised; `Resize` returns `ErrResizeNotImplemented`, and
at 1.1.0 `container --help` plus `container <verb> --help` exposes no live
resource-update verb (no `update` or `resize` subcommand), so the stub posture
design.md:451-454 remains correct.
