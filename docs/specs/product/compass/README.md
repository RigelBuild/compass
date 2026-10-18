# Compass

Living source of truth for **Compass** behavior and build. Historical rationale
— the ADE vision (the Dispatcher, the Bridge, per-agent containers, the
three-tier Client→Server→Runner architecture) — lives in the design corpus,
indexed by its decision files
([`decisions/`](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/README.md)) and
[architecture lineage](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/meta/compass-architecture-lineage/design.md);
this spec describes only what the code exposes today.

Interface- and security-critical behavior is stated as `Requirement:` headings
with nested `Scenario:` headings (RFC 2119 SHALL/MUST, Given/When/Then); prose
frames the model.

## Overview

Compass is an Agentic Development Environment (ADE) — a persistent local
**server** (`compass-server`, Go) fronting a web **UI**. The server owns the
backend; the UI is a client. The two meet at a single, owned contract:
**`compass.v1`**, a gRPC service the server serves over its local transport.

What is built today is the contract seam and the server's transport:
`compass-server` serves the `compass.v1` service over a Unix domain socket, and
a generated client is the only sanctioned way to reach it. The agent runtime
(a first-party in-container agent built on the Oh My Pi SDK, emitting
`compass.v1` natively, hosted on a per-workstream **Runner**) and the Bridge UI
are designed but not yet fully built;
the server
currently reports its liveness, provisions agent workstreams, and pushes a
server-status event stream, and the web UI is a walking skeleton. The sections
below describe the serving surface as it stands.

## Spec files

- [The `compass.v1` contract](contract.md)
- [Server transport and lifecycle](server-transport.md)
- [The event stream](event-stream.md)
- [Agent sessions](agent-sessions.md)
- [Session relay and observation](session-relay.md)
- [The communication layer](comms.md)
- [Comms stream, threads, and asks](comms-stream.md)
- [Ask mirroring and channel subscriptions](comms-mirroring.md)
- [The agent tree](agent-tree.md)
- [The network door](network-door.md)
- [Runner enrollment CLI](runner-enrollment.md)

## Not yet specified

These remaining surfaces are designed (see the design record) but not yet
implemented, and are out of scope for this spec until they land:

- **Dispatcher and audit events** — the agent session layer above plugs a
  Dispatcher MCP endpoint into a named seam, but its logic is a separate issue.
  The Bridge UI that consumes the agent event stream is out of scope.
- **Desktop shell (Wails v3)** — a thin shell bridging the UI webview to the
  server's Unix socket over its own IPC (a browser cannot dial a Unix socket),
  streaming gRPC-Web responses back as ordered byte frames with no localhost TCP
  port. Designed in the records; the shell is not built in the current backend.
- **Windows / WSL2** — a token-guarded loopback (or named-pipe) transport, since
  AF_UNIX sockets do not cross the WSL2 boundary.
- **Concurrent-start locking** — an exclusive start-lock closing the
  probe→bind race between two servers starting at once.
