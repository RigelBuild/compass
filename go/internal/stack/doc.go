// Package stack is the embedded-stack supervisor: the single-user Compass ADE
// backbone (DL-108). It productizes the devenv dogfood chain — private postgres,
// TLS anchor, compass-server, readiness, runner token, agent image,
// compass-runner — behind a Up/Down/Health API over an app state dir.
//
// The core is written against injected seams (see deps.go): every external
// effect — process spawns, cert generation, token minting, image pulls, the
// GetServerInfo probe, the clock — is an interface the caller supplies. The core
// therefore imports nothing from the server, comms, runnerhub, delivery, or
// generated-client packages; the real adapters that DO import those live in a
// sibling slice and are wired in at the CLI boundary. This keeps the state
// machine fully unit-testable with stubs and green in isolation.
package stack
