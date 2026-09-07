# Apple `container` macOS runner — amendment: transport slow-rolled, macOS ships podman

> **Design amendment.** Amends the frozen apple-container macOS runner record
> (`docs/designs/platform/apple-container-macos-runner/design.md`, RIG-3238,
> ruled by Matt 2026-09-05). That record is frozen and is **not** rewritten in
> place (a later change adds a record, never rewrites the frozen one); this
> amendment records the transport ruling Matt made on the T-1 spike's RED vsock
> finding (2026-09-07, DL-338) and is the authority where it and the parent
> record disagree. Every citation is a path in the **`RigelBuild/compass`**
> monorepo.

Status: Active — ruled by Matt (2026-09-07)
Tracking: RIG-3490 (transport ruling)
Amends: RIG-3238 apple-container macOS runner (`design.md`)
Refs: DL-338; DL-330 (sequencing narrowed, direction intact); T-1 spike findings (`spike-findings.md`)

## Problem / Intent

The parent record made apple-container adoption conditional on one hardware
unknown: whether the guestd unix→vsock forwarder has a host-side attach point
through the `container` CLI. It named the disposition for a red result at
`design.md:619-620` — "If the vsock leg is NOT reachable through the CLI, the
transport question (not the apple-container direction) returns to Matt with the
finding" — against OQ-11's ruling at `:719-721`.

The T-1 spike (`spike-findings.md`, PR #923) ruled that leg **RED**: there is no
host-side attach point. That fired the escalation trigger, so the transport
question went back to Matt.

## The ruling

Matt ruled (RIG-3490): *"Ok yeah we can slow roll the apple/container adoption
then. we'll ship macOS with podman."*

So **T-2 is held**, and `--publish-socket` — which the spike proved as a working
substitute — is **deliberately NOT adopted**. The gateway keeps its current
host-listens/guest-dials ordering
(`go/internal/runner/gateway/socket.go:4-13`). `--publish-socket` would invert
that: the guest binds and the host dials. It also needs an application-level
readiness handshake, because a host connect succeeds before the guest has
bound. Neither is built.

macOS embedded ships on **podman-machine**, consistent with the parent's
sequencing ruling at `design.md:95-100` and its interim framing at `:162-164`.

This narrows DL-330's **sequencing** only. DL-330's direction — apple-container
as the macOS embedded runtime, when adopted — is unchanged, which is the
disposition DL-330 itself pre-authorized by returning the transport question
rather than the direction.

## Task state this sets

The parent's Tasks list is frozen and reads pre-ruling. Its live state is:

- **T-1** — **DONE.** `spike-findings.md` landed in PR #923 (`01bbd3a0e`).
- **T-2** — **HELD** by this ruling.
- **T-3, T-4, T-5** — unchanged, still gated behind T-2.

## Parent text this supersedes

A reader who lands on these lines in the frozen parent gets the pre-ruling
world. This amendment is the authority where they disagree:

- `design.md:69-70` — "only the flip timing and the **vsock hardware leg**
  remain gated." The vsock leg is no longer gated; it is resolved RED and the
  direction is on hold.
- `design.md:232-233` — "The spike **is the only task that runs today** ... T-2
  onward are gated only on the spike proving the load-bearing vsock/uid/exec
  unknowns." After this ruling no task runs today: T-1 is done and T-2 is held.
- `design.md:168-169` and `:262-266` — describe the vsock unknown as unverified and
  the flip as gated on the spike proving that leg. The spike disproved it.

## Held, not settled

This is a **hold, not a settled transport**: if apple-container is picked back
up, the fork is still open and RIG-3490 is the starting point. Two spike
findings must be re-read rather than re-assumed at that point, because both
contradict text in the parent record:

- Raw AF_UNIX over a virtiofs bind-mount is **RED** on this backend, which
  confirms the `compass-local-dev/design.md:194-199` limitation holds. The
  argument at `design.md:247-252` — that the hazard dissolves *because* vsock
  takes the socket off the virtiofs path — dies with the vsock leg.
- `CapEff` is **all-zero for every non-root uid** (measured at uid 1000,
  including `--cap-add ALL`), falsifying `design.md:203-204`'s premise that
  `AgentRuntime.armEgress`'s nft path runs unchanged. Its fix at
  `go/internal/runtime/agent.go:319-328` is **shared with the live podman
  path**. That seam has no regression cover reaching it today: CI's only
  `-tags podman` run is scoped to `./e2e/...`
  (`.github/workflows/ci.yml:2184`), so 16 of the 32 podman-tagged files fall
  outside that lane. T-2 must widen that lane before any cover counts. The
  hold makes this less urgent but does not remove it.
