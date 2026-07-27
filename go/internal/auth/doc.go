// Package auth is the Go re-realization of the Rust compass-daemon auth layer
// (oss/compass/crates/compass-daemon/src/auth.rs): the F2 token-auth layer for
// the network door. It holds the credential store plus the interceptors that
// establish a request's caller identity, and the method-level admin gate over
// the network door's privileged CompassService session RPCs.
//
// Two doors reach the same compass.v1 service, and each authenticates
// differently:
//
//   - The network door (the authenticated TCP/TLS endpoint) is open to anyone
//     who can reach the port, so every request must carry a bearer token. The
//     BearerInterceptor reads "authorization: Bearer <token>", resolves it
//     against the TokenStore, and attaches the caller's account to the request
//     context — or rejects the request as CodeUnauthenticated.
//   - The Unix-socket door is already gated by the socket's 0600 mode: only the
//     owning user can connect, so reaching it is the credential. The
//     AmbientIdentity interceptor attaches a fixed account without a token, so
//     handlers read the caller identity the same way on both doors — via
//     CallerFrom.
//
// Authentication (who you are) and authorization (what you may call) are
// deliberately separate: BearerInterceptor only authenticates and injects the
// account, while the admin gate is a distinct method-level guard that rejects a
// non-admin account on the network door's privileged session RPCs with
// CodePermissionDenied.
//
// Each door's caller injection is split across a unary and a streaming
// interceptor that must BOTH be installed: BearerInterceptor with
// BearerStreamInterceptor on the network door, AmbientIdentity with
// AmbientStreamInterceptor on the socket door. Installing only the unary form
// leaves streaming RPCs with no caller in context — CallerFrom returns false in
// the handler and the admin gate fail-closes — a silent authorization gap rather
// than a compile error, so the server stack must wire each pair together.
//
// Per docs/designs/platform/go-idioms-and-libraries.md the Rust is the spec, not
// a template: the wire contract, invariants, and error semantics carry over
// exactly while the implementation is written as native Go. The store keeps only
// the SHA-256 hash of each issued token, never the plaintext, so reading it
// yields no usable credential; a minted token is shown to its holder exactly
// once (at TokenStore.Issue) and never recoverable after.
package auth
