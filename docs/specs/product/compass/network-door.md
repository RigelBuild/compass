# The network door

A distinct authenticated door for clients that reach the server over an
untrusted network rather than the local Unix socket. Unlike the socket (whose
`0600` mode is its whole credential) and the dev endpoint (loopback, ambient
identity, no bearer auth — so its admin-only RPCs are denied), the network door
is the internet-facing surface: it terminates TLS, authenticates every RPC except
health with a bearer token, and gates the admin-only RPCs. It is off by default and opened
only with `--listen`.

## Requirement: The network door requires TLS

The server SHALL open the network door only when `--listen` is given, and only
with a TLS certificate and key (`--tls-cert` / `--tls-key`). A bearer token over
cleartext is credential disclosure, so `--listen` without both SHALL be refused
up front — enforced by the server itself, not only the CLI, since the serving
entry point is an exported package function. A whitespace-only value for any
of the three flags counts as unset. The door SHALL require a minimum of TLS 1.3.

### Scenario: `--listen` without a keypair is rejected

- **Given** `compass-server --listen` with a TCP address but no `--tls-cert` /
  `--tls-key`
- **When** it starts
- **Then** it exits with an error naming the TLS requirement and binds nothing.

## Requirement: The network door authenticates a bearer token and gates admin RPCs

The server SHALL authenticate network-door RPCs with a bearer token: a
request with no token or an unresolvable token SHALL be rejected
`Unauthenticated`, on both unary and streaming RPCs, except the gRPC health
service's `Check` and `Watch` methods. `GET` and `HEAD /healthz` are also
anonymous. The admin-only RPCs (`IssueToken`, `RenameAccount`, and the
agent-session lifecycle RPCs) SHALL additionally require the caller to resolve
to the admin account, rejecting a non-admin `PermissionDenied`; the
authenticated-open RPCs (`GetServerInfo`, `SubscribeEvents`, `GetRefLinkRules`)
SHALL admit any valid bearer.

### Scenario: Health checks require no network-door bearer

- **Given** a running network door
- **When** a client calls health `Check` or `Watch`, or sends
  `GET`/`HEAD /healthz`, without a bearer token
- **Then** `Check` reports the current status, `Watch` returns `Unimplemented`,
  and `/healthz` returns 200 when serving or 503 when not serving.

### Scenario: A token-less call on the network door is rejected

- **Given** a running network door
- **When** a client calls any non-health RPC with no bearer token
- **Then** the server rejects it `Unauthenticated` before the handler runs.

### Scenario: A non-admin token calls an admin-only RPC

- **Given** a valid non-admin bearer token
- **When** the client calls `IssueToken` or `RenameAccount` over the network door
- **Then** the server rejects it `PermissionDenied`, while the same token on
  `GetServerInfo` succeeds.

## Requirement: The network door mints and persists a bootstrap admin token

Opening the network door SHALL ensure a bootstrap admin bearer token file at
mode `0600` under the state directory. The server SHALL reuse that file when it
is itself a regular file, not a symlink, with no group or other bits, and its
whitespace-trimmed token still authenticates as the bootstrap admin; otherwise
it SHALL mint a new one. A minted file SHALL be written atomically (temp file
`0600`, synced, renamed; removed on error), and the token SHALL NOT be logged,
only its path. A socket-only start SHALL write no token file. A file set by
`--admin-token-file` or `COMPASS_ADMIN_TOKEN_FILE` SHALL resolve, possibly
through symlinks, to a regular file with no setuid, setgid, sticky,
group-write, group-execute or others bit, and SHALL be group-readable only by
a group the process holds. It SHALL contain a whitespace-trimmed base64url
encoding of exactly 32 bytes. The server SHALL register that token for the
bootstrap admin and persist it under the state directory; a revoked token or
another subject's SHALL fail startup.

### Scenario: Opening the network door writes the admin token 0600

- **Given** `compass-server --listen` with a valid keypair and a state directory
- **When** the server reaches its serving state
- **Then** the admin-token file exists at mode `0600`, authenticates as the
  admin account, and the token value never appears in the server's logs.

### Scenario: A restart reuses a still-valid admin token

- **Given** a state directory whose admin-token file still authenticates as the
  bootstrap admin
- **When** the server restarts with `--listen`
- **Then** the file and its token are unchanged and no new token is minted;
  after the token is revoked, the next start mints and writes a new one.

### Scenario: A supplied admin token survives a database recreate

- **Given** an operator-supplied admin-token file
- **When** the server starts with a recreated database and the same file
- **Then** the token authenticates as the new bootstrap admin and is persisted
  under the state directory.

## Requirement: The network door defaults closed to browser origins

The network door SHALL apply no CORS policy unless `--cors-allowed-origin` names
a single explicit browser origin; when set, the door SHALL allow exactly that
one origin (never a wildcard), admit the `Authorization` request header, and
SHALL NOT enable credentialed (cookie) CORS. A preflight from any other origin
SHALL NOT be reflected.

### Scenario: A foreign origin preflight is not reflected

- **Given** a network door started with `--cors-allowed-origin` naming one origin
- **When** a CORS preflight arrives from a different origin
- **Then** the server does not reflect it, and the configured origin's preflight
  is answered with exactly that origin (never `*`) and without credentialed CORS.
