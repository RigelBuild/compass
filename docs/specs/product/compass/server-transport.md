# Server transport and lifecycle

## Local transport

Native clients — any native gRPC client, and the designed desktop shell's
webview — reach `compass-server` through the `compass.v1` gRPC service over a
**Unix domain socket**. The socket is the shipped door: it serves native gRPC
(HTTP/2) and gRPC-Web (HTTP/1.1) off one listener, so no localhost TCP port is
exposed in production on macOS or native Linux. A browser cannot dial a Unix
socket, so browser-based development instead uses the opt-in, off-by-default
loopback TCP endpoint ([Development endpoint](#development-endpoint)); the
designed desktop shell ([Not yet specified](README.md#not-yet-specified)) will bridge its
webview to the socket over the shell's own IPC.

The socket path defaults to `$XDG_RUNTIME_DIR/compass/server.sock`, falling
back to `$HOME/.compass/server.sock`, and is overridable with `--socket`.

### Requirement: The Unix socket serves native gRPC and gRPC-Web on one listener

The server SHALL serve the `compass.v1` service over the Unix domain socket to
both native gRPC (HTTP/2) clients and gRPC-Web (HTTP/1.1) clients, from a single
listener, without an external proxy. Native gRPC requests SHALL be served
unchanged; only gRPC-Web requests are translated.

#### Scenario: A native gRPC client calls over the socket

- **Given** a running server bound to its Unix socket
- **When** a native gRPC (HTTP/2) client calls `GetServerInfo` over the socket
- **Then** the server returns its version and the `compass.v1` api-version.

#### Scenario: A gRPC-Web client calls over the same socket

- **Given** a running server bound to its Unix socket
- **When** a gRPC-Web (HTTP/1.1) client issues the same call over the socket
- **Then** the server answers it over gRPC-Web without a separate port or proxy.

### Requirement: The shipped path exposes no localhost TCP port

On macOS and native Linux the server SHALL NOT open any TCP listener as part of
its normal (shipped) operation; the Unix socket is the only transport. A TCP
listener is opened only when a dev endpoint is explicitly requested (below).

#### Scenario: Default startup opens no TCP port

- **Given** `compass-server` started without `--dev-http`
- **When** it reaches its serving state
- **Then** it listens only on the Unix domain socket and binds no TCP port.

### Requirement: The socket is owner-only

The server SHALL restrict the socket so only its owner can connect: any parent
directory the server creates for the socket SHALL be created mode `0700`, and the
socket file SHALL be set to mode `0600`. A pre-existing parent directory's mode
is left unchanged.

#### Scenario: The server creates the socket's parent directory

- **Given** a socket path whose parent directory does not yet exist
- **When** the server creates that directory and binds the socket
- **Then** the created directory is mode `0700` and the socket file is mode `0600`.

## Single-instance startup

### Requirement: The server refuses to displace a live server

On startup the server SHALL probe the socket path. It SHALL refuse to start
(returning an error) if a live server already answers there, and SHALL refuse a
path that exists but is not a socket. It SHALL clear only a genuinely stale
socket (one whose owner is gone) before binding.

#### Scenario: A second server starts on a live socket

- **Given** a server already serving on a socket path
- **When** a second `compass-server` is started on the same path
- **Then** the second refuses to start, names the already-running server, and
  leaves the live socket intact.

#### Scenario: A stale socket file remains from a dead server

- **Given** a socket file on disk whose server is no longer running
- **When** a new server starts on that path
- **Then** it clears the stale socket and binds successfully.

## Serving lifecycle

### Requirement: Clean shutdown drains streams and removes the socket

On its shutdown signal the server SHALL stop accepting connections, wake every
open `SubscribeEvents` stream so in-flight streams end and graceful drain
completes, and then remove the socket file — but only if the on-disk socket is
still the inode it bound (so it never deletes a successor server's socket).

#### Scenario: Shutdown with a subscriber stream held open

- **Given** a client holding a `SubscribeEvents` stream open
- **When** the server receives its shutdown signal
- **Then** `Serve` returns without error and removes the socket, rather than
  hanging on the held-open stream.

### Requirement: Startup failures surface promptly

The server SHALL bind its listeners before entering its serving state, so a bind
failure fails startup up front rather than after the server reports itself
serving. If a server task exits on its own (an error or panic) while serving, the
server SHALL tear down its peer server and propagate the failure rather than
leaving it unobserved until an external shutdown.

#### Scenario: The requested dev port is already in use

- **Given** `--dev-http` names a TCP address already bound by another listener
- **When** the server starts
- **Then** startup fails promptly with an error naming the dev-endpoint bind
  failure, and the server does not report itself serving.

## Development endpoint

A browser cannot dial a Unix socket, so local UI development against a real
browser uses an opt-in loopback TCP endpoint. It is a development affordance, not
part of the shipped surface.

### Requirement: The dev endpoint is opt-in and loopback-only

The server SHALL serve a gRPC-Web endpoint over TCP only when `--dev-http` is
given. The address MUST be a loopback address (`127.0.0.1` or `::1`); a
non-loopback address SHALL be rejected. The rejection SHALL be enforced by the
server itself, not only by the CLI, since the serving entry point is an exported
package function and the endpoint is unauthenticated with permissive CORS.

#### Scenario: A non-loopback dev address is rejected

- **Given** `compass-server --dev-http` with a routable (non-loopback) address
- **When** it starts
- **Then** it exits with an error naming the loopback requirement and binds
  nothing.

### Requirement: The dev endpoint applies gRPC-Web CORS

When serving the dev endpoint the server SHALL apply a CORS layer that exposes
the gRPC-Web status trailers (`grpc-status`, `grpc-message`,
`grpc-status-details-bin`) so a browser gRPC-Web client can read call status. The
Unix socket SHALL NOT apply CORS — it is a same-origin surface (the shell's
webview or a native client).

### Requirement: The dev endpoint denies admin-only RPCs

The dev endpoint mounts no bearer interceptor, so it never carries an
authenticated caller. Its admin-only RPCs (`IssueToken`, `RenameAccount`, and the
agent-session lifecycle RPCs) SHALL therefore fail closed `PermissionDenied`, while the
authenticated-open RPCs (`GetServerInfo`, `SubscribeEvents`, `GetRefLinkRules`)
and the ambient-admin comms RPCs remain reachable. This prevents a browser page loaded
against a configured dev endpoint from minting a bootstrap-admin token via
`IssueToken` and replaying it against the network door.

#### Scenario: `IssueToken` on the dev endpoint is denied

- **Given** a server started with `--dev-http`
- **When** a client calls `IssueToken` over the dev endpoint
- **Then** the server rejects it `PermissionDenied` and mints no token, while
  `GetServerInfo` and a comms read over the same endpoint succeed.
