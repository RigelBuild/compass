# Agent sessions

An agent runs as a **first-party program built on the Oh My Pi SDK** inside its
own container (the per-agent isolation substrate), emitting `compass.v1` frames
natively — there is no [Agent Client Protocol](https://agentclientprotocol.com/)
anywhere in the system, and no translator. The agent is spawned on the container
host — the **Runner** — over a long-lived streaming exec, and its
newline-delimited `compass.v1` stream is relayed up to the server, which fans it
out to clients. One container holds one agent, driven by one session.

The topology is **Client → Server → Runner**: the server holds no
container-engine code; the Runner owns every container operation and dials out to
the server (a NAT-friendly outbound connection, like a CI runner). Container
lifecycle commands flow Client → Server → RunnerHub → Runner over the internal
Runner seam; agent frames flow Runner → Server → Client.

## Requirement: Provisioning a workstream creates its isolated container

The server SHALL expose `ProvisionAgentWorkspace` — agent ref + repo/workstream
spec in, a stable `container_name` out. It routes the request Client → Server →
RunnerHub → Runner, where the Runner drives the built lifecycle façade to create
the isolated per-agent container; the server itself runs no container-engine
code. Provisioning and starting are **separate** operations: a container can
exist idle before a session runs in it, so `ProvisionAgentWorkspace` and
`StartAgentSession` stay distinct RPCs. When the request carries a
`client_request_id`, it is an idempotency key: a timeout-retry with the same id
SHALL return the same `container_name` rather than creating a second container.

### Scenario: A retried provision creates no duplicate container

- **Given** a provision request carrying a `client_request_id`
- **When** the client's first call times out and it retries with the same id
- **Then** the server returns the same `container_name` and the Runner has
  created exactly one container.

## Requirement: The lifecycle RPCs manage one agent session per container

The server SHALL expose `StartAgentSession`, `StopAgentSession`,
`ReloadAgentSession`, and `GetAgentStatus`, each routing Client → Server →
RunnerHub → Runner. `StartAgentSession` SHALL bring the first-party agent in a
provisioned container online — spawning it over the streaming-exec bridge — and
return a server-assigned session id. `StopAgentSession` SHALL deliberately
terminate the in-container agent and release the session, and SHALL succeed for
an unknown or already-stopped session (idempotent teardown). `GetAgentStatus`
SHALL return one session's status, or every retained session's when no id is
given, reconciled to the owning Runner's authoritative session set; retained
sessions include `ERRORED` ones awaiting `ReloadAgentSession`.

### Scenario: Starting an agent session streams its status

- **Given** a provisioned agent container with no live session
- **When** a client calls `StartAgentSession` for it
- **Then** the server brings the first-party agent online and publishes an
  `AgentSessionStatus` transition to `STARTING` then `READY` on the event
  stream, and returns the session id.

## Requirement: One live session per container; replacing goes through reload

A container holds at most one live session. `StartAgentSession` for a container
that already has a live session SHALL be rejected (the existing session is
unaffected); replacing a live session SHALL go through `ReloadAgentSession`.
Reload is defined as *teardown-then-fresh-start against the same container*: the
server stops the current agent and session, then starts a new one under the
**same** session id (so the id stays a stable handle) and the agent reloads from
its workspace state.

## Requirement: One container per agent account

The server SHALL bind at most one live container to a given agent account. A
`SpawnAgent` for an agent account that already holds a live session SHALL be
rejected with `ALREADY_EXISTS` before any container is provisioned (the
existing session is unaffected). A `SpawnAgent` for an account whose only
session is `ERRORED` SHALL be rejected with `FAILED_PRECONDITION` ("agent
errored; reload or stop it"), also before any provisioning. This binds the
reject-on-live rule to the
*agent account*, not merely to the container name: the existing
one-session-per-container requirement above coincides with it today only
because the container name is derived from the account, an incidental property
a future Runner naming containers differently would break.

## Requirement: A deliberate stop is distinct from an unexpected exit or a lost Runner

The server SHALL distinguish three session-ending conditions. A deliberate
stop/reload SHALL NOT be reported as `ERRORED`. An unexpected agent exit (a
crash, out-of-memory, or engine restart) SHALL transition the session to
`ERRORED` on the event stream and SHALL NOT be auto-reconnected; recovery is an
explicit `ReloadAgentSession`. A lost **Runner link** is distinct from both: the
session SHALL transition to `DISCONNECTED` — live session truth is temporarily
unreachable but not lost — and a bounded reattach window SHALL govern recovery.
A reattach within the window resumes the session; window expiry falls to
`ERRORED`, after which the no-auto-reconnect policy applies.
> **Implementation status (RIG-4075):** The Runner observes an unexpected agent
> process exit and publishes `ERRORED`, including when the agent cannot send its
> own terminal frame. The Runner retains the session for `ReloadAgentSession`.
> Only a Runner-link loss before T9 has no process-exit frame; its bounded
> reattach-window behavior remains governed by the T9 implementation status below.

### Scenario: A Runner disconnect moves its sessions to DISCONNECTED, not ERRORED

- **Given** a Runner with a live agent session
- **When** the Runner's link to the server drops
- **Then** the session transitions to `DISCONNECTED` on the event stream, and a
  reattach within the bounded window resumes it rather than ending it as
  `ERRORED`.

> **Implementation status (RIG-1243 T4):** T4 ships the `DISCONNECTED` state on
> the contract and the disconnect *signal* — a lost Runner link fails the
> session's in-flight commands. The server-side **reattach-window enforcement**
> — the per-session registry that publishes `DISCONNECTED` on link loss, the
> bounded timer, the expiry→`ERRORED` transition, and `GetAgentStatus`
> reconciliation to the Runner's set on reattach — is **T9**
> (RIG-1328). Until T9 lands, the
> window/expiry state machine in this Requirement is not yet enforced.

## Requirement: Recover a session after its container is removed outside Compass

When an agent's container was removed outside Compass while the Runner still
holds its handle, `ProvisionAgentWorkspace` for the same agent SHALL replace it:
the Runner retires the `ERRORED` session, drops the stale handle, and launches a
new container under the same name. A following `StartAgentSession` with the
original `resume_session_id` SHALL then recover the session. Until then,
`StartAgentSession` and `ReloadAgentSession` for that container SHALL fail with
`NOT_FOUND` before any exec, and `RemoveAgentWorkspace` SHALL succeed and drop
the handle and placement. A message wake that receives `NOT_FOUND` SHALL
re-provision the agent and retry the start.

### Scenario: Removing a stale container releases its placement

- **Given** the container was removed outside Compass and the Runner still
  holds the handle
- **When** `StartAgentSession` or `ReloadAgentSession` is called
- **Then** it fails with `NOT_FOUND` and no agent exec runs
- **And** `RemoveAgentWorkspace` succeeds and releases the placement.

### Scenario: Re-provisioning recovers a container removed outside Compass

- **Given** a session is `ERRORED` and its container was removed outside
  Compass, while the Runner still has the container handle
- **When** a client calls `ProvisionAgentWorkspace` for the same agent with a
  new or empty `client_request_id`, then `StartAgentSession` with the original
  `resume_session_id`
- **Then** the Runner launches a replacement container instead of rejecting
  the provision as already provisioned, and the resumed session starts on it.
